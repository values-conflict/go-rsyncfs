package rsyncfs

import (
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/values-conflict/go-rsyncfs/protocol"
	"github.com/values-conflict/go-rsyncfs/protocol/mux"
)

// HandleConnection runs the full rsync daemon protocol on a single connection, sequentially: the pre-transfer handshake (greeting, module selection, auth, arguments, compat flags, algorithm negotiation, checksum seed), then the transfer phase in [(*Server).doServerSender].
//
// The rw is the underlying transport (TCP socket, pipe, etc).  The call blocks until the connection is complete; it returns nil for clean disconnects and #list requests, or an error that aborted the protocol.
func (s *Server) HandleConnection(rw io.ReadWriter) error {
	result, err := s.doHandshake(rw)
	if err != nil {
		return err
	}
	if result == nil {
		return nil
	}
	return s.doServerSender(rw, result)
}

// handshakeResult carries the negotiated state out of doHandshake and into
// the transfer phase.
type handshakeResult struct {
	module *ServerModule
	ver    int

	// varint is upstream xfer_flags_as_varint (CF_VARINT_FLIST_FLAGS
	// negotiated).
	varint bool
	// seedFix is upstream proper_seed_order (CF_CHKSUM_SEED_FIX
	// negotiated).
	seedFix bool
	// id0Names is upstream xmit_id0_names (CF_ID0_NAMES negotiated).
	id0Names bool
	// preserve carries the client's uid/gid preservation settings (-o /
	// -g flags), which the daemon applies to its file list.
	preserve [2]bool
	// preserveHlinks is the client's -H flag.
	preserveHlinks bool

	checksum string // negotiated strong-hash name
	seed     int32

	// outMw is the mux writer started in doHandshake for proto < 23,
	// where upstream muxes the daemon's output in rsync_module before
	// setup_protocol writes the seed.  Nil from proto 23 on, where the
	// mux starts in doServerSender (upstream start_server) after the
	// seed.
	outMw *mux.Writer
}

// doHandshake runs the pre-transfer handshake in raw (unmultiplexed) I/O:
// greeting exchange, module selection, authentication, argument
// transmission, compat flags, algorithm negotiation, and the checksum seed
// (upstream start_server + clientserver.c).  It returns a nil result for
// clean disconnects and #list requests.
func (s *Server) doHandshake(rw io.ReadWriter) (*handshakeResult, error) {
	// greeting exchange -- copy s.Greeting so the (shared, reusable) Server
	// is not mutated by the defaults fill
	greeting := s.Greeting
	greeting.ApplyDefaults()
	if err := protocol.WriteGreeting(rw, greeting); err != nil {
		return nil, fmt.Errorf("write greeting: %w", err)
	}
	clientGreeting, err := protocol.ReadGreeting(rw)
	if err != nil {
		_ = protocol.WriteError(rw, "invalid greeting")
		return nil, fmt.Errorf("read client greeting: %w", err)
	}
	version, _, _, err := protocol.Negotiate(*clientGreeting, greeting)
	if err != nil {
		_ = protocol.WriteError(rw, "protocol version negotiation failed")
		return nil, fmt.Errorf("negotiate version: %w", err)
	}

	// module selection
	moduleName, err := protocol.ReadModuleRequest(rw)
	if err != nil {
		if errors.Is(err, io.EOF) {
			return nil, nil // clean disconnect
		}
		return nil, fmt.Errorf("read module request: %w", err)
	}
	if moduleName == "#list" || moduleName == "" {
		modules := make([]protocol.ModuleInfo, 0, len(s.modules))
		for _, m := range s.modules {
			modules = append(modules, protocol.ModuleInfo{Name: m.Name, Comment: m.Comment})
		}
		if err := protocol.WriteModuleList(rw, modules, version); err != nil {
			return nil, fmt.Errorf("write module list: %w", err)
		}
		return nil, nil
	}
	selected, ok := s.modules[moduleName]
	if !ok {
		_ = protocol.WriteError(rw, "Unknown module")
		return nil, fmt.Errorf("unknown module %q", moduleName)
	}
	// upstream chdirs into the module path before the binary phase; a
	// missing root is an @ERROR at this point
	if _, err := selected.FS.Open("."); err != nil {
		_ = protocol.WriteError(rw, "chdir failed")
		return nil, fmt.Errorf("module %q root inaccessible: %w", moduleName, err)
	}

	// authentication
	if selected.AuthCallback != nil {
		challenge := make([]byte, 16)
		if _, err := rand.Read(challenge); err != nil {
			return nil, fmt.Errorf("generate challenge: %w", err)
		}
		if err := protocol.WriteAuthChallenge(rw, challenge); err != nil {
			return nil, fmt.Errorf("write auth challenge: %w", err)
		}
		username, responseHash, err := protocol.ReadAuthResponse(rw)
		if err != nil {
			_ = protocol.WriteError(rw, "Authentication failed")
			return nil, fmt.Errorf("read auth response: %w", err)
		}
		expectedHash, err := selected.AuthCallback(username, challenge)
		if err != nil || !hashEqual(responseHash, expectedHash) {
			_ = protocol.WriteError(rw, "Authentication failed")
			return nil, fmt.Errorf("authentication failed for user %q", username)
		}
		if err := protocol.WriteAuthOK(rw); err != nil {
			return nil, fmt.Errorf("write auth ok: %w", err)
		}
	} else {
		if err := protocol.WriteAuthOK(rw); err != nil {
			return nil, fmt.Errorf("write auth ok: %w", err)
		}
	}

	// argument transmission
	args, err := protocol.ReadArgs(rw, version)
	if err != nil {
		return nil, fmt.Errorf("read arguments: %w", err)
	}

	// compat flags (proto >= 30)
	var compatFlags int
	if version >= 30 {
		clientInfo := protocol.ExtractClientInfo(args)
		compatFlags = protocol.ResolveCompatFlags(serverCapabilities(), clientInfo)
		if err := protocol.WriteCompatFlags(rw, compatFlags, version); err != nil {
			return nil, fmt.Errorf("write compat flags: %w", err)
		}
	}

	// algorithm negotiation -- happens only when CF_VARINT_FLIST_FLAGS is
	// negotiated (upstream do_negotiated_strings); otherwise both sides
	// default to md5 (proto >= 30) or md4 before
	var checksum string
	if compatFlags&protocol.CompatVarintFlistFlags != 0 {
		algos, err := protocol.NegotiateAlgorithms(rw, protocol.SupportedDigests(), nil)
		if err != nil {
			return nil, fmt.Errorf("negotiate algorithms: %w", err)
		}
		checksum = algos.Checksum
	} else {
		checksum = protocol.DefaultAlgorithms(version).Checksum
	}

	// checksum seed -- the daemon generates it and sends it on every
	// protocol version (upstream setup_protocol writes it unconditionally).
	// A zero seed changes the legacy block checksums (old rsync only
	// mixes the seed in when it is non-zero), so keep it non-zero.
	seed := int32(time.Now().UnixNano())
	var seedBuf [4]byte
	if _, err := rand.Read(seedBuf[:]); err == nil {
		seed = int32(binary.LittleEndian.Uint32(seedBuf[:]))
	}
	if seed == 0 {
		seed = 32761
	}
	// The daemon's output multiplexing starts right after argument
	// parsing (upstream io_start_multiplex_out in rsync_module), so for
	// proto < 23 the seed is already framed; for proto 23 and up the
	// switch happens in start_server after setup_protocol, so the seed
	// stays plain bytes there.
	seedWriter := io.Writer(rw)
	var outMw *mux.Writer
	if version < 23 {
		outMw = mux.NewWriter(rw)
		seedWriter = outMw
	}
	if err := protocol.WriteChecksumSeed(seedWriter, seed); err != nil {
		return nil, fmt.Errorf("write checksum seed: %w", err)
	}

	preserveUID, preserveGID, preserveHlinks := protocol.ExtractPreserveFlags(args)
	return &handshakeResult{
		module:         selected,
		ver:            version,
		varint:         compatFlags&protocol.CompatVarintFlistFlags != 0,
		seedFix:        compatFlags&protocol.CompatChksumSeedFix != 0,
		id0Names:       compatFlags&protocol.CompatId0Names != 0,
		preserve:       [2]bool{preserveUID, preserveGID},
		preserveHlinks: preserveHlinks,
		checksum:       checksum,
		seed:           seed,
		outMw:          outMw,
	}, nil
}

// serverCapabilities returns the compat flag bits this server supports,
// mirroring the daemon's setup in compat.c.  CF_INC_RECURSE is omitted
// (this server never walks incrementally), CF_ID0_NAMES is omitted (id 0's
// name is not resolvable here), CF_SYMLINK_ICONV is omitted (no iconv
// support), and CF_INPLACE_PARTIAL_DIR is omitted (no partial-dir
// support).
func serverCapabilities() int {
	return protocol.CompatSymlinkTimes |
		protocol.CompatSafeFlist |
		protocol.CompatAvoidXattrOptim |
		protocol.CompatChksumSeedFix |
		protocol.CompatVarintFlistFlags
}

// hashEqual compares two byte slices in constant time to avoid timing
// attacks.
func hashEqual(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	result := 0
	for i := range a {
		if a[i] != b[i] {
			result = 1
		}
	}
	return result == 0
}
