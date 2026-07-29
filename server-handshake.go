package rsyncfs

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"io"
	"strings"

	"github.com/values-conflict/go-rsyncfs/protocol"
)

// HandshakeResult contains the outcome of a successful server handshake.
type HandshakeResult struct {
	Module  *ServerModule
	Version int
	Digest  string
}

// HandleOptions provides configuration for handling a connection's handshake.
type HandleOptions struct {
	LocalGreeting protocol.Greeting                                           // what version/digests we advertise
	AuthCallback  func(username string, challenge []byte) ([]byte, error) // nil = no auth required
}

// HandleConnection runs the full text-phase handshake on a single connection.
// It returns control to the caller when ready for data transfer (Phase 4), or an error at any point.
func (s *Server) HandleConnection(rw io.ReadWriter, opts HandleOptions) (*HandshakeResult, error) {
	// use a simple byte-by-byte reading approach to avoid over-reading into Phase 4

	// --- Phase 1: Greeting Exchange ---
	
	// send server greeting
	if _, err := rw.Write([]byte(opts.LocalGreeting.String())); err != nil {
		return nil, fmt.Errorf("failed to send greeting: %w", err)
	}

	// read client greeting
	clientGreetLine, err := readLine(rw)
	if err != nil {
		return nil, fmt.Errorf("failed to read client greeting: %w", err)
	}

	clientGreeting, err := protocol.ParseGreeting(string(clientGreetLine))
	if err != nil {
		// send error and return
		_ = s.SendError(rw, "invalid greeting")
		return nil, fmt.Errorf("failed to parse client greeting: %w", err)
	}

	version, _, digest, err := protocol.Negotiate(opts.LocalGreeting, *clientGreeting)
	if err != nil {
		_ = s.SendError(rw, "protocol version negotiation failed")
		return nil, fmt.Errorf("negotiation error: %w", err)
	}

	// --- Phase 2: Module Selection & Authentication ---

	var selectedModule *ServerModule

Loop:
	for {
		line, err := readLine(rw)
		if err != nil {
			return nil, fmt.Errorf("failed to read module request: %w", err)
		}
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}

		if string(line) == "#list" {
			// handle #list request
			if err := s.sendModuleList(rw); err != nil {
				return nil, fmt.Errorf("failed to send module list: %w", err)
			}
			continue Loop
		}

		moduleName := string(line)
		mod, ok := s.modules[moduleName]
		if !ok {
			_ = s.SendError(rw, "Unknown module")
			return nil, fmt.Errorf("unknown module: %s", moduleName)
		}
		selectedModule = mod
		break Loop
	}

	// authentication (if configured)
	if opts.AuthCallback != nil {
		challenge := make([]byte, 16) // in real rsync this is random data

		challengeB64 := base64.StdEncoding.EncodeToString(challenge)
		if _, err := rw.Write([]byte(fmt.Sprintf("@RSYNCD: AUTHREQD %s\n", challengeB64))); err != nil {
			return nil, fmt.Errorf("failed to send auth request: %w", err)
		}

		authLine, err := readLine(rw)
		if err != nil {
			return nil, fmt.Errorf("failed to read auth response: %w", err)
		}

		parts := strings.Fields(string(authLine))
		if len(parts) < 2 {
			_ = s.SendError(rw, "Authentication failed")
			return nil, fmt.Errorf("invalid auth response format")
		}

		username := parts[0]
		responseHashB64 := parts[1]
		responseHash, err := base64.StdEncoding.DecodeString(responseHashB64)
		if err != nil {
			_ = s.SendError(rw, "Authentication failed")
			return nil, fmt.Errorf("invalid auth response hash: %w", err)
		}

		expectedResponse, err := opts.AuthCallback(username, challenge)
		if err != nil || !bytes.Equal(responseHash, expectedResponse) {
			_ = s.SendError(rw, "Authentication failed")
			return nil, fmt.Errorf("authentication failed for user %s", username)
		}

		if _, err := rw.Write([]byte("@RSYNCD: OK\n")); err != nil {
			return nil, fmt.Errorf("failed to send auth success: %w", err)
		}
	} else if selectedModule != nil {
		// some rsync versions might still expect an "OK" or just proceed;
		// protocol.md says @RSYNCD: OK follows successful authentication
	}

	// --- Phase 3: Argument Transmission ---

	var args []string
	if version >= 30 {
		// null-terminated arguments
		args, err = readNullTerminatedArgs(rw)
	} else {
		// newline-terminated arguments
		args, err = readNewlineTerminatedArgs(rw)
	}

	if err != nil {
		return nil, fmt.Errorf("failed to read arguments: %w", err)
	}

	_ = args // mark as used for now until we need them in HandshakeResult

	return &HandshakeResult{
		Module:  selectedModule,
		Version: version,
		Digest:  digest,
	}, nil
}

func (s *Server) sendModuleList(rw io.Writer) error {
	for name, mod := range s.modules {
		// format: <name>           <comment>\n
		// name left-padded to 15 chars + tab + comment
		line := fmt.Sprintf("%-15s\t%s\n", name, mod.Comment)
		if _, err := rw.Write([]byte(line)); err != nil {
			return err
		}
	}
	_, err := rw.Write([]byte("@RSYNCD: EXIT\n"))
	return err
}

// readLine reads from rw until a newline character is encountered
func readLine(rw io.Reader) ([]byte, error) {
	var buf []byte
	b := make([]byte, 1)
	for {
		_, err := rw.Read(b)
		if err != nil {
			return nil, err
		}
		if b[0] == '\n' {
			break
		}
		buf = append(buf, b[0])
	}
	return buf, nil
}

// readNullTerminatedArgs reads arguments until double null
func readNullTerminatedArgs(rw io.Reader) ([]string, error) {
	var args []string
	var currentArg []byte
	lastWasNull := false

	b := make([]byte, 1)
	for {
		_, err := rw.Read(b)
		if err != nil {
			return nil, err
		}
		if b[0] == '\x00' {
			if lastWasNull {
				// Double null encountered: end of arguments
				break
			}
			lastWasNull = true
			if len(currentArg) > 0 {
				args = append(args, string(currentArg))
				currentArg = nil
			} else if len(args) == 0 {
				// First arg is always ".", but it might be empty?
				// Actually rsync usually sends "." as the first arg.
			}
		} else {
			lastWasNull = false
			currentArg = append(currentArg, b[0])
		}
	}

	if len(currentArg) > 0 {
		args = append(args, string(currentArg))
	}

	return args, nil
}

// readNewlineTerminatedArgs reads arguments until double newline
func readNewlineTerminatedArgs(rw io.Reader) ([]string, error) {
	var args []string
	var currentArg []byte
	lastWasNewline := false

	b := make([]byte, 1)
	for {
		_, err := rw.Read(b)
		if err != nil {
			return nil, err
		}
		if b[0] == '\n' {
			if lastWasNewline {
				// Double newline encountered: end of arguments
				break
			}
			lastWasNewline = true
			if len(currentArg) > 0 {
				args = append(args, string(currentArg))
				currentArg = nil
			}
		} else {
			lastWasNewline = false
			currentArg = append(currentArg, b[0])
		}
	}

	if len(currentArg) > 0 {
		args = append(args, string(currentArg))
	}

	return args, nil
}
