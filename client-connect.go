package rsyncfs

import (
	"fmt"
	"io"
	"io/fs"

	"github.com/values-conflict/go-rsyncfs/protocol"
	"github.com/values-conflict/go-rsyncfs/protocol/mux"
)

// clientInfo is the subprotocol feature string the client advertises in the -e option for proto >= 30 (upstream maybe_add_e_option in options.c).  It mirrors the upstream client minus the features this client does not implement: 'i' (incremental file list), 'L' (symlink time-setting), 's' (symlink iconv), 'I' (inplace partial dir), and 'u' (id-0 name map).
const clientInfo = ".fxCv"

// transferArgstr builds the combined short-option argument the client sends (upstream server_options in options.c for a recursive, metadata-preserving pull): -l (symlinks), -o (owner), -g (group), -D (devices), -t (times), -p (perms), -r (recursive).  -v is deliberately omitted so the daemon stays quiet on the multiplexed message channel, and proto >= 30 appends the client_info -e option.
func transferArgstr(version int) string {
	argstr := "-logDtpr"
	if version >= 30 {
		argstr += "e" + clientInfo
	}
	return argstr
}

// Session holds an active connection to an rsync daemon, ready for FS operations.  In root mode (Module == ""), the Session is a config holder -- connectFunc creates fresh connections for each operation.
//
// Session is not safe for concurrent use.  The rsync protocol is sequential: selectors are sent one-at-a-time through a single-phase loop, and the compressed NDX encoder maintains shared delta state.
type Session struct {
	client *Client

	rw  io.ReadWriter // live connection (nil in root mode)
	out io.Writer     // client -> daemon: the raw rw for proto < 30, the mux.Writer for proto >= 30
	in  io.Reader     // daemon -> client: always muxed in module mode (nil in root mode)
	mr  *mux.Reader   // raw daemon -> client mux reader (nil in root mode)
	mw  *mux.Writer   // client -> daemon mux writer (nil for proto < 30 and in root mode)

	version     int
	subProtocol byte
	digest      string // negotiated auth digest
	checksum    string // negotiated strong checksum
	seed        int32
	compatFlags int

	varintFlist bool // CF_VARINT_FLIST_FLAGS negotiated
	seedFix     bool // CF_CHKSUM_SEED_FIX negotiated
	id0Names    bool // CF_ID0_NAMES negotiated

	moduleName  string
	ndx         *protocol.NdxState                  // compressed NDX delta state for sending selectors (generator role)
	recvNdx     *protocol.NdxState                  // compressed NDX delta state for reading echoed selectors (receiver role); upstream keeps the read and write NDX delta trackers separate
	connectFunc func(string) (io.ReadWriter, error) // root mode: fresh connections per operation

	consumed bool // true once the current connection ran its phase exchange and was closed
}

var _ fs.FS = (*Session)(nil)

// Connect runs the full pre-transfer handshake (greeting exchange, version negotiation, module selection, authentication, argument transmission, compat flags, algorithm negotiation, checksum seed, filter list) and returns an active session ready for filesystem operations.  If rw is nil and [Client.ConnectFunc] is set, ConnectFunc is called with the configured module name to create the connection.  For root mode (Module == ""), use [Client.OpenRoot] instead.
//
// The session's input is always multiplexed (the daemon's output is muxed on every supported protocol version); its output is multiplexed for proto >= 30 and raw below.  The file list itself is read by the first Open.
func (c Client) Connect(rw io.ReadWriter) (*Session, error) {
	if c.Module == "" {
		return nil, fmt.Errorf("Module is empty; use OpenRoot for root mode")
	}
	if rw == nil {
		if c.ConnectFunc == nil {
			return nil, fmt.Errorf("Connect called with nil io.ReadWriter but ConnectFunc is not set")
		}
		var err error
		if rw, err = c.ConnectFunc(c.Module); err != nil {
			return nil, fmt.Errorf("ConnectFunc: %w", err)
		}
	}

	s := &Session{}
	if err := c.runHandshake(rw, s); err != nil {
		return nil, err
	}
	return s, nil
}

// runHandshake runs the full pre-transfer handshake on rw and fills in the
// session's live-connection state in place.  It is the body of [Client.Connect]
// split out so [Session.Open] can re-establish a connection after the
// previous one was consumed by a transfer.
func (c Client) runHandshake(rw io.ReadWriter, s *Session) error {
	s.client = &c
	s.rw = rw
	s.out = rw
	s.in = nil
	s.mr = nil
	s.mw = nil
	s.version = 0
	s.subProtocol = 0
	s.digest = ""
	s.checksum = ""
	s.seed = 0
	s.compatFlags = 0
	s.varintFlist = false
	s.seedFix = false
	s.id0Names = false
	s.moduleName = c.Module
	s.ndx = protocol.NewNdxState()
	s.recvNdx = protocol.NewNdxState()
	s.consumed = false

	greet := c.Greeting
	greet.ApplyDefaults()

	// Greeting exchange.  Both sides write their greeting before reading the
	// peer's (upstream exchange_protocols: output_daemon_greeting then
	// read_line_old); the lines are small enough that a real rsync pair never
	// deadlocks on the simultaneous write.
	if err := protocol.WriteGreeting(rw, greet); err != nil {
		return fmt.Errorf("write greeting: %w", err)
	}
	serverGreet, err := protocol.ReadGreeting(rw)
	if err != nil {
		return fmt.Errorf("read server greeting: %w", err)
	}
	version, subProtocol, digest, err := protocol.Negotiate(greet, *serverGreet)
	if err != nil {
		return fmt.Errorf("negotiate protocol version: %w", err)
	}
	s.version = version
	s.subProtocol = subProtocol
	s.digest = digest

	// Module selection + authentication.  The server answers the module line
	// with a challenge (AUTHREQD), OK, or @ERROR; the upstream client loops on
	// the response (clientserver.c rsync_module), so we do the same.
	if err := protocol.WriteModuleRequest(rw, c.Module); err != nil {
		return fmt.Errorf("send module request: %w", err)
	}
	for {
		challenge, err := protocol.ReadAuthChallenge(rw)
		if err != nil {
			return fmt.Errorf("read server response: %w", err)
		}
		if challenge == nil { // @RSYNCD: OK
			break
		}
		if c.AuthUser == "" || c.AuthResponse == nil {
			return fmt.Errorf("server requires authentication but AuthUser/AuthResponse are not set")
		}
		digestBytes, err := c.AuthResponse(digest, challenge)
		if err != nil {
			return fmt.Errorf("compute auth response: %w", err)
		}
		if err := protocol.WriteAuthResponse(rw, c.AuthUser, digestBytes); err != nil {
			return fmt.Errorf("send auth response: %w", err)
		}
	}

	// Argument transmission.  The file argument is "<module>/": the daemon
	// strips the module-name prefix (glob_expand_module in io.c) and resolves
	// the remainder against the module directory, so the module root comes
	// out as "/".
	args := []string{"--server", "--sender", transferArgstr(version), ".", c.Module + "/"}
	if err := protocol.WriteArgs(rw, args, version); err != nil {
		return fmt.Errorf("send arguments: %w", err)
	}

	// Compat flags + algorithm negotiation (proto >= 30 only).  The daemon
	// sends its resolved compat flags as a varint; when
	// CF_VARINT_FLIST_FLAGS is set both sides exchange checksum lists as
	// vstrings, otherwise both fall back to the per-version default (md5
	// for proto >= 30, md4 below).  Compression negotiation is out of scope
	// (no -z), so only the checksum list is sent.
	if version >= 30 {
		compatFlags, err := protocol.ReadCompatFlags(rw, version)
		if err != nil {
			return fmt.Errorf("read compat flags: %w", err)
		}
		s.compatFlags = compatFlags
		s.varintFlist = compatFlags&protocol.CompatVarintFlistFlags != 0
		s.seedFix = compatFlags&protocol.CompatChksumSeedFix != 0
		s.id0Names = compatFlags&protocol.CompatId0Names != 0

		if s.varintFlist {
			algos, err := protocol.NegotiateAlgorithms(rw, protocol.SupportedDigests(), nil)
			if err != nil {
				return fmt.Errorf("negotiate algorithms: %w", err)
			}
			s.checksum = algos.Checksum
		} else {
			s.checksum = protocol.DefaultAlgorithms(version).Checksum
		}
	} else {
		s.checksum = protocol.DefaultAlgorithms(version).Checksum
	}

	// Switch to multiplexed I/O before reading the checksum seed.  The
	// daemon's output is muxed on every supported protocol version, but
	// where the switch happens differs: proto < 23 starts muxing in
	// rsync_module before setup_protocol, so the seed arrives inside an
	// MSG_DATA frame and the client's input must already be a mux reader;
	// 23 and up start muxing in start_server after the seed, which stays
	// plain bytes.  The client's output only becomes framed from proto 30
	// on (client_run), raw below.
	s.mr = mux.NewReader(rw)
	if version >= 30 {
		s.mw = mux.NewWriter(rw)
		s.out = s.mw
	}

	// Checksum seed (sent by the daemon on every protocol version).
	seedReader := io.Reader(rw)
	if version < 23 {
		seedReader = s.mr
	}
	seed, err := protocol.ReadChecksumSeed(seedReader)
	if err != nil {
		return fmt.Errorf("read checksum seed: %w", err)
	}
	s.seed = seed

	flush := func() error { return nil }
	if version >= 30 {
		flush = s.mw.Flush
	}
	// Push pending mux output to the wire before each read (the
	// flushBeforeRead wrapper from the server side), so a selector or the
	// filter list can never sit in the send buffer while the client blocks
	// on a read the daemon is gating on it.
	s.in = &flushBeforeRead{inner: s.mr, flush: flush}

	// Filter list.  Always sent (even when empty: the terminating int32 0),
	// after the mux switch so the daemon's mux input is ready for it.
	if err := protocol.WriteInt32(s.out, 0); err != nil {
		return fmt.Errorf("send filter list: %w", err)
	}

	return nil
}

// OpenRoot returns a Session for root mode (modules as top-level directories).  Unlike [Client.Connect], it does not establish a live connection -- each FS operation gets its own connection via [Client.ConnectFunc] (the server closes the connection after #list, so a single persistent connection is not possible).
func (c Client) OpenRoot() (*Session, error) {
	if c.Module != "" {
		return nil, fmt.Errorf("OpenRoot requires Module to be empty, got %q -- use Connect instead", c.Module)
	}
	if c.ConnectFunc == nil {
		return nil, fmt.Errorf("root mode requires ConnectFunc")
	}

	greet := c.Greeting
	greet.ApplyDefaults()

	// No connection is opened here: every FS operation (#list, module
	// Open) runs its own full handshake, so probing now would only
	// re-learn what the next operation re-learns.
	return &Session{
		client:      &c,
		version:     greet.Version,
		digest:      greet.Digests[0],
		connectFunc: c.ConnectFunc,
	}, nil
}
