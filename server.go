package rsyncfs

import (
	"fmt"
	"io"
	"io/fs"
)

// Server represents an rsync daemon server that serves one or more modules.
type Server struct {
	modules map[string]*ServerModule
}

// ServerModule represents a single rsync module configuration and its backing filesystem.
type ServerModule struct {
	Name     string
	Comment  string
	FS       fs.FS
	ReadOnly bool
}

// NewServer creates a new rsync daemon server with the provided modules.
func NewServer(mods ...*ServerModule) (*Server, error) {
	modules := make(map[string]*ServerModule)
	for _, m := range mods {
		if _, exists := modules[m.Name]; exists {
			return nil, fmt.Errorf("module %q already exists", m.Name)
		}
		modules[m.Name] = m
	}

	return &Server{
		modules: modules,
	}, nil
}

// formatError returns the rsync-style @ERROR line for a given message.
func (s *Server) formatError(msg string) string {
	return fmt.Sprintf("@ERROR: %s\n", msg)
}

// SendError writes an error response to the provided writer and closes it if necessary.
func (s *Server) SendError(w io.Writer, msg string) error {
	_, err := w.Write([]byte(s.formatError(msg)))
	return err
}
