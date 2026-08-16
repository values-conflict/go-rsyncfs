package rsyncfs

import (
	"crypto/md5"
	"fmt"
	"hash"
	"io"
	"io/fs"

	"golang.org/x/crypto/md4"

	"github.com/values-conflict/go-rsyncfs/protocol"
)

// Client configures a connection to an rsync daemon module.
// Construct directly with &Client{...}; zero-value fields use sensible defaults applied lazily during [Client.Connect] or [Client.OpenRoot].
type Client struct {
	// Module is the rsync module name to connect to. Empty
	// string enables root mode (all modules as top-level directories).
	Module string

	// Greeting is the greeting sent to the server. Zero-value fields
	// are filled by [protocol.Greeting.ApplyDefaults].
	Greeting protocol.Greeting

	// AuthUser is the username for module authentication. Empty
	// string means anonymous access.
	AuthUser string

	// AuthResponse computes the auth response hash given
	// the server's challenge and the negotiated digest algorithm.
	// The returned raw hash bytes are base64-encoded by the
	// library before sending. Nil means anonymous access (AuthUser
	// must also be empty). Use [PasswordAuth] for the standard
	// password+challenge digest flow.
	AuthResponse func(digest string, challenge []byte) ([]byte, error)

	// ConnectFunc creates a new connection to the rsync server.
	// Required for root mode (Module == ""). The moduleName argument
	// is the target module name, or empty string for a #list request.
	// The caller is responsible for closing the returned ReadWriter.
	//
	// In root mode, each FS operation (listing modules, opening a
	// module) gets its own connection -- the server closes the
	// connection after #list, so a single persistent connection is
	// not possible.
	//
	// When used with [Client.Connect] and a nil io.ReadWriter,
	// ConnectFunc is called with the configured Module name to create
	// the connection.
	ConnectFunc func(moduleName string) (io.ReadWriter, error)
}

// PasswordAuth returns an AuthResponse function that computes the standard rsync auth hash: digest(password + challenge) using the negotiated algorithm.
func PasswordAuth(password string) func(digest string, challenge []byte) ([]byte, error) {
	return func(digest string, challenge []byte) ([]byte, error) {
		return computeAuthHash(digest, password, challenge)
	}
}

// computeAuthHash computes the rsync auth response hash: digest(password + challenge).
func computeAuthHash(digest string, password string, challenge []byte) ([]byte, error) {
	var h hash.Hash
	switch digest {
	case "md4":
		h = md4.New()
	case "md5":
		h = md5.New()
	default:
		return nil, fmt.Errorf("auth digest %q not supported", digest)
	}
	if _, err := io.WriteString(h, password); err != nil {
		return nil, err
	}
	if _, err := h.Write(challenge); err != nil {
		return nil, err
	}
	return h.Sum(nil), nil
}

// Session holds an active connection to an rsync daemon, ready for FS operations.
// In root mode (Module == ""), the Session is a config holder -- connectFunc creates fresh connections for each operation.
//
// Session is not safe for concurrent use.  The rsync protocol is sequential:
// selectors are sent one-at-a-time through a single-phase loop, and the
// compressed NDX encoder maintains shared delta state.
type Session struct{}

var _ fs.FS = (*Session)(nil)

// Connect runs the full handshake and returns an active session.
// If rw is nil and Client.ConnectFunc is set, ConnectFunc creates the connection.
// For root mode (Module == ""), use OpenRoot instead.
func (c Client) Connect(rw io.ReadWriter) (*Session, error) {
	// TODO implement (Task 15)
	return nil, nil
}

// OpenRoot returns a Session for root mode (modules as top-level directories).
// Does not establish a live connection -- each FS operation gets its own.
// Requires Client.ConnectFunc to be set.
func (c Client) OpenRoot() (*Session, error) {
	// TODO implement (Task 15)
	return nil, nil
}

// Open implements fs.FS.  Opens the named file or directory within the module.
func (s *Session) Open(name string) (fs.File, error) {
	// TODO implement (Task 16)
	return nil, nil
}
