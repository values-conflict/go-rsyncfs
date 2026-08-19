package rsyncfs

import (
	"fmt"
	"io"
	"io/fs"

	"github.com/values-conflict/go-rsyncfs/protocol"
)

// Server represents an rsync daemon that serves one or more modules.
// Construct with NewServer.  A single Server handles multiple connections.
type Server struct {
	modules map[string]*ServerModule

	// Greeting is the greeting the server advertises on every connection.
	// Zero-value fields are filled by [protocol.Greeting.ApplyDefaults].
	Greeting protocol.Greeting
}

// ServerModule wraps a backing filesystem with rsync module configuration.
type ServerModule struct {
	Name     string // module name
	Comment  string // displayed in #list
	FS       fs.FS  // backing filesystem
	ReadOnly bool   // true = reject push operations

	// AuthCallback verifies a username+challenge response for this module.
	// Returns the expected raw digest bytes, or an error to reject.
	// Nil means no authentication required for this module.
	// Matches rsync's per-module secrets file model.
	AuthCallback func(username string, challenge []byte) ([]byte, error)
}

// NewServer creates a new rsync daemon server with the provided modules.
// Returns an error if any two modules share the same name.
func NewServer(mods ...*ServerModule) (*Server, error) {
	modules := make(map[string]*ServerModule, len(mods))
	for _, m := range mods {
		if _, exists := modules[m.Name]; exists {
			return nil, fmt.Errorf("duplicate module name %q", m.Name)
		}
		modules[m.Name] = m
	}
	return &Server{modules: modules}, nil
}

// HandleConnection runs the full rsync daemon protocol on a single connection.
// The rw is the underlying transport (TCP socket, pipe, etc).
// Returns when the connection is complete or an error occurs.
//
// Handles: greeting exchange, module selection (#list or named module),
// authentication, argument parsing, compat flags, checksum negotiation,
// file list transfer, selector loop, data transfer, final goodbye.
func (s *Server) HandleConnection(rw io.ReadWriter) error {
	// TODO implement (Task 13)
	return nil
}
