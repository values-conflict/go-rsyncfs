package rsyncfs

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"io"
	"io/fs"
	"strings"
	"time"

	"github.com/values-conflict/go-rsyncfs/protocol"
	"github.com/values-conflict/go-rsyncfs/protocol/mux"
)

// Client configures a connection to an rsync daemon module.
// Construct directly with &Client{...}; zero-value fields use sensible defaults applied lazily during [Client.Connect] or [Client.OpenRoot].
type Client struct {
	// Module is the rsync module name to connect to.
	// Empty string enables root mode (all modules as top-level directories).
	Module string

	// Greeting is the greeting sent to the server.
	// Zero value defaults to protocol version 32 with md5/md4 digests.
	Greeting protocol.Greeting

	// AuthUser is the username for module authentication.
	// Empty string means anonymous access.
	AuthUser string

	// AuthResponse computes the auth response hash given the server's challenge
	// and the negotiated digest algorithm. The returned raw hash bytes are
	// base64-encoded by the library before sending.
	// Nil means anonymous access (AuthUser must also be empty).
	// Use [PasswordAuth] for the standard password+challenge digest flow.
	AuthResponse func(challenge []byte, digest string) ([]byte, error)

	// ConnectFunc creates a new connection to the rsync server.
	// Required for root mode (Module == ""). The moduleName argument is
	// the target module name, or empty string for a #list request.
	// The caller is responsible for closing the returned ReadWriter.
	//
	// In root mode, each FS operation (listing modules, opening a module)
	// gets its own connection -- the server closes the connection after #list,
	// so a single persistent connection is not possible.
	//
	// When used with [Client.Connect] and a nil io.ReadWriter, ConnectFunc
	// is called with the configured Module name to create the connection.
	ConnectFunc func(moduleName string) (io.ReadWriter, error)
}

// applyDefaults sets default greeting values if not already configured.
func (c *Client) applyDefaults() {
	if c.Greeting.Version == 0 {
		c.Greeting.Version = 32
		c.Greeting.SubProtocol = 0
	}
	if len(c.Greeting.Digests) == 0 {
		c.Greeting.Digests = []string{"md5", "md4"}
	}
}

// PasswordAuth returns an AuthResponse function that computes the standard rsync auth hash: digest(password + challenge) using the negotiated algorithm.
func PasswordAuth(password string) func(challenge []byte, digest string) ([]byte, error) {
	return func(challenge []byte, digest string) ([]byte, error) {
		return computeAuthHash(password, challenge, digest)
	}
}

// computeAuthHash computes the rsync auth response hash: digest(password + challenge).
func computeAuthHash(password string, challenge []byte, digest string) ([]byte, error) {
	// TODO implement md4, md5, and any other digests rsync supports
	return nil, fmt.Errorf("auth digest %q not yet implemented", digest)
}

// Session holds an active connection to an rsync daemon, ready for FS operations.
// In root mode (Module == ""), the Session is a config holder -- connectFunc creates fresh connections for each operation (the server closes after #list).
type Session struct {
	client      *Client
	rw          io.ReadWriter // live connection (nil in root mode)
	muxReader   *mux.Reader   // nil in root mode
	muxWriter   *mux.Writer   // nil in root mode
	version     int
	digest      string
	moduleName  string
	connectFunc func(string) (io.ReadWriter, error) // for root mode, creates connections on-demand
	prevNdx     int32 // tracks previous positive NDX for compressed delta encoding
}

var _ fs.FS = (*Session)(nil) // compile-time interface check

// moduleInfo holds metadata about a single rsync module from the #list response.
type moduleInfo struct {
	name    string
	comment string
}

// Connect runs the full handshake with the server and returns an active session.
// The rw parameter is the underlying connection (e.g. a TCP socket or pipe).
// If rw is nil and ConnectFunc is set, ConnectFunc is called with the configured module name to create the connection automatically.
//
// In root mode (Module == ""), use [Client.OpenRoot] instead -- the server closes the connection after #list, so a single persistent connection is not possible.
func (c Client) Connect(rw io.ReadWriter) (*Session, error) {
	if c.Module == "" {
		return nil, fmt.Errorf("root mode requires OpenRoot, not Connect (server closes connection after #list)")
	}

	if rw == nil {
		if c.ConnectFunc == nil {
			return nil, fmt.Errorf("Connect called with nil io.ReadWriter but ConnectFunc is not set")
		}
		var err error
		rw, err = c.ConnectFunc(c.Module)
		if err != nil {
			return nil, fmt.Errorf("ConnectFunc: %w", err)
		}
	}

	c.applyDefaults()

	// --- Phase 1: Greeting Exchange ---

	// read server greeting
	serverGreetLine, err := readLine(rw)
	if err != nil {
		return nil, fmt.Errorf("read server greeting: %w", err)
	}

	serverGreeting, err := protocol.ParseGreeting(string(serverGreetLine))
	if err != nil {
		return nil, fmt.Errorf("parse server greeting: %w", err)
	}

	// send our greeting
	if _, err := rw.Write([]byte(c.Greeting.String())); err != nil {
		return nil, fmt.Errorf("send client greeting: %w", err)
	}

	version, _, digest, err := protocol.Negotiate(c.Greeting, *serverGreeting)
	if err != nil {
		return nil, fmt.Errorf("negotiate version: %w", err)
	}

	s := &Session{
		client:     &c,
		rw:         rw,
		version:    version,
		digest:     digest,
		moduleName: c.Module,
		prevNdx:    -1,
	}

	// --- Phase 2: Module Selection ---

	if _, err := rw.Write([]byte(c.Module + "\n")); err != nil {
		return nil, fmt.Errorf("send module request: %w", err)
	}

	// --- Authentication (if server responds with AUTHREQD) ---
	// when auth is configured on the server, it sends AUTHREQD after module selection.
	// when auth is not configured, the server sends nothing and proceeds to read arguments.
	// we need to peek at the next line to see if it's AUTHREQD.
	// since we can't truly peek, we read one line and check what it is.

	authLine, err := readLine(rw)
	if err != nil {
		return nil, fmt.Errorf("read auth response: %w", err)
	}

	lineStr := string(bytes.TrimSpace(authLine))
	if strings.HasPrefix(lineStr, "@RSYNCD: AUTHREQD ") {
		// server wants authentication
		if c.AuthUser == "" || c.AuthResponse == nil {
			return nil, fmt.Errorf("server requires authentication but no credentials provided")
		}

		challengeB64 := strings.TrimSpace(strings.TrimPrefix(lineStr, "@RSYNCD: AUTHREQD "))
		challenge, err := base64.StdEncoding.DecodeString(challengeB64)
		if err != nil {
			return nil, fmt.Errorf("decode auth challenge: %w", err)
		}

		responseHash, err := c.AuthResponse(challenge, digest)
		if err != nil {
			return nil, fmt.Errorf("compute auth response: %w", err)
		}

		responseB64 := base64.StdEncoding.EncodeToString(responseHash)
		respLine := fmt.Sprintf("%s %s\n", c.AuthUser, responseB64)
		if _, err := rw.Write([]byte(respLine)); err != nil {
			return nil, fmt.Errorf("send auth response: %w", err)
		}

		// read server auth result
		okLine, err := readLine(rw)
		if err != nil {
			return nil, fmt.Errorf("read auth result: %w", err)
		}

		okStr := string(bytes.TrimSpace(okLine))
		if okStr != "@RSYNCD: OK" {
			return nil, fmt.Errorf("authentication failed: %s", okStr)
		}
	} else if lineStr == "@RSYNCD: OK" {
		// no auth required, module selection succeeded
	} else if strings.HasPrefix(lineStr, "@ERROR:") {
		// check if this was an error from module selection (not auth)
		return nil, fmt.Errorf("server error: %s", strings.TrimPrefix(lineStr, "@ERROR: "))
	} else {
		return nil, fmt.Errorf("unexpected server response: %s", lineStr)
	}

	// --- Phase 3: Argument Transmission ---

	if err := s.sendArguments(version); err != nil {
		return nil, fmt.Errorf("send arguments: %w", err)
	}

	// --- Phase 4: Protocol Version Exchange (binary) ---

	// server sends its protocol version as int32 LE
	var remoteProtoBuf [4]byte
	if _, err := io.ReadFull(rw, remoteProtoBuf[:]); err != nil {
		return nil, fmt.Errorf("read remote protocol version: %w", err)
	}
	remoteProto := int32(binary.LittleEndian.Uint32(remoteProtoBuf[:]))
	if int(remoteProto) < version {
		s.version = int(remoteProto)
	}

	// we send our protocol version back
	var localProtoBuf [4]byte
	binary.LittleEndian.PutUint32(localProtoBuf[:], uint32(s.version))
	if _, err := rw.Write(localProtoBuf[:]); err != nil {
		return nil, fmt.Errorf("send local protocol version: %w", err)
	}

	// --- Switch to multiplexed I/O ---

	s.muxReader = mux.NewReader(rw)
	s.muxWriter = mux.NewWriter(rw)

	return s, nil
}

// OpenRoot returns a Session configured for root mode.
// Unlike [Client.Connect], this does not establish a live connection.
// Instead, the Session holds the client config and uses ConnectFunc to create fresh connections for each FS operation.
//
// The server closes the connection after #list, so root mode cannot use a single persistent connection. Each ls at the root and each module access gets its own connection.
func (c Client) OpenRoot() (*Session, error) {
	if c.Module != "" {
		return nil, fmt.Errorf("OpenRoot requires Module == \"\", got %q -- use Connect instead", c.Module)
	}
	if c.ConnectFunc == nil {
		return nil, fmt.Errorf("root mode requires ConnectFunc")
	}

	c.applyDefaults()

	// do a greeting exchange to negotiate version/digest, then close the connection
	// this gives us the negotiated params without holding a live connection
	//
	// TODO for now, skip the greeting probe and just use the configured defaults
	// a proper implementation would open a connection, do greeting exchange, then close

	return &Session{
		client:      &c,
		version:     c.Greeting.Version,
		digest:      c.Greeting.Digests[0],
		connectFunc: c.ConnectFunc,
	}, nil
}

// readModuleList reads the tab-separated module listing from the server.
// The caller is responsible for closing rw after this returns.
func readModuleList(rw io.ReadWriter) ([]moduleInfo, error) {
	var modules []moduleInfo

	for {
		line, err := readLine(rw)
		if err != nil {
			return nil, fmt.Errorf("read module list line: %w", err)
		}

		lineStr := string(bytes.TrimSpace(line))
		if lineStr == "" {
			continue
		}

		if lineStr == "@RSYNCD: EXIT" {
			break
		}

		// parse: "<name>         \t<comment>\n" (name left-padded to 15 chars + tab + comment)
		parts := strings.SplitN(lineStr, "\t", 2)
		name := strings.TrimSpace(parts[0])
		comment := ""
		if len(parts) > 1 {
			comment = parts[1]
		}

		modules = append(modules, moduleInfo{
			name:    name,
			comment: comment,
		})
	}

	return modules, nil
}

// doListRequest does a full #list handshake: greeting exchange, #list request, read modules.
// Returns the module list.  The connection is closed before returning (the server canonically closes it).
func doListRequest(connectFunc func(string) (io.ReadWriter, error), greet protocol.Greeting) ([]moduleInfo, error) {
	rw, err := connectFunc("") // empty string = #list request
	if err != nil {
		return nil, fmt.Errorf("open connection for #list: %w", err)
	}
	defer rw.(io.Closer).Close()

	// greeting exchange
	serverGreetLine, err := readLine(rw)
	if err != nil {
		return nil, fmt.Errorf("read server greeting: %w", err)
	}

	_, err = protocol.ParseGreeting(string(serverGreetLine))
	if err != nil {
		return nil, fmt.Errorf("parse server greeting: %w", err)
	}

	// send our greeting
	if _, err := rw.Write([]byte(greet.String())); err != nil {
		return nil, fmt.Errorf("send client greeting: %w", err)
	}

	// request module listing
	if _, err := rw.Write([]byte("#list\n")); err != nil {
		return nil, fmt.Errorf("send #list request: %w", err)
	}

	return readModuleList(rw)
}

// sendArguments sends the rsync command-line arguments to the server.
func (s *Session) sendArguments(version int) error {
	// minimal arguments: just "." (current directory)
	args := []string{"."}

	var terminator byte = 0
	if version < 30 {
		terminator = '\n'
	}

	for _, arg := range args {
		if _, err := s.rw.Write(append([]byte(arg), terminator)); err != nil {
			return err
		}
	}

	// double terminator
	_, err := s.rw.Write([]byte{terminator})
	return err
}

// Open implements the fs.FS interface for the session.
// In root mode (connectFunc != nil), modules are top-level directories.
// Otherwise, it opens the path within the connected module.
func (s *Session) Open(name string) (fs.File, error) {
	if s.connectFunc != nil {
		return s.openRootMode(name)
	}
	return s.openModule(name)
}

// rootDirEntry represents a virtual directory entry for a module in root mode.
type rootDirEntry struct {
	name    string
	comment string
}

// Name returns the module name.
func (e rootDirEntry) Name() string { return e.name }

// IsDir returns true -- modules are presented as directories.
func (e rootDirEntry) IsDir() bool { return true }

// Type returns the file mode type (directory).
func (e rootDirEntry) Type() fs.FileMode { return fs.ModeDir }

// Info returns fs.FileInfo for the entry.
func (e rootDirEntry) Info() (fs.FileInfo, error) {
	return rootFileInfo{
		name:    e.name,
		comment: e.comment,
	}, nil
}

// rootFileInfo implements fs.FileInfo for a virtual module directory entry.
type rootFileInfo struct {
	name    string
	comment string // TODO something clever with comment
}

var _ fs.FileInfo = (*rootFileInfo)(nil)

func (fi rootFileInfo) Name() string       { return fi.name }
func (fi rootFileInfo) Size() int64        { return 0 }
func (fi rootFileInfo) Mode() fs.FileMode  { return fs.ModeDir | 0o755 }
func (fi rootFileInfo) ModTime() time.Time { return time.Time{} }
func (fi rootFileInfo) Sys() any           { return nil }
func (rootFileInfo) IsDir() bool           { return true }
func (fi rootFileInfo) Comment() string    { return fi.comment } // TODO something clever with comments (this is not part of fs.FileInfo but is used by tests)

// rootDir implements fs.File for the virtual root directory in root mode.
type rootDir struct {
	entries []rootDirEntry
	pos     int
}

var _ fs.File = (*rootDir)(nil)

func newRootDir(modules []moduleInfo) *rootDir {
	entries := make([]rootDirEntry, len(modules))
	for i, m := range modules {
		entries[i] = rootDirEntry{name: m.name, comment: m.comment}
	}
	return &rootDir{entries: entries}
}

func (r *rootDir) Stat() (fs.FileInfo, error) {
	return rootFileInfo{name: ".", comment: "rsync modules"}, nil
}

func (r *rootDir) Read([]byte) (int, error) { return 0, fs.ErrInvalid }

func (r *rootDir) ReadDir(n int) ([]fs.DirEntry, error) {
	if r.pos >= len(r.entries) {
		return nil, io.EOF
	}

	if n <= 0 || n > len(r.entries)-r.pos {
		n = len(r.entries) - r.pos
	}

	result := make([]fs.DirEntry, n)
	for i := 0; i < n; i++ {
		result[i] = r.entries[r.pos+i]
	}
	r.pos += n
	return result, nil
}

func (r *rootDir) Close() error { return nil }
