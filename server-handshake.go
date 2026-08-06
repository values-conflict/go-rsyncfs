package rsyncfs

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"io"
	"strings"

	"github.com/values-conflict/go-rsyncfs/protocol"
)

// compat flag bits matching upstream rsync compat_flags
const (
	cfIncRecurse        = 1 << 0 // 'i'
	cfSymlinkTimes      = 1 << 1 // 'L'
	cfSymlinkIconv      = 1 << 2 // 's'
	cfSafeFlist         = 1 << 3 // 'f'
	cfAvoidXattrOptim   = 1 << 4 // 'x'
	cfChksumSeedFix     = 1 << 5 // 'C'
	cfInplacePartialDir = 1 << 6 // 'I'
	cfVarintFlistFlags  = 1 << 7 // 'v'
	cfId0Names          = 1 << 8 // 'u'
)

// HandshakeResult contains the outcome of a successful server handshake.
type HandshakeResult struct {
	Module           *ServerModule
	Version          int
	Digest           string
	VarintFlistFlags bool // true when CF_VARINT_FLIST_FLAGS is negotiated
}

// HandleOptions provides configuration for handling a connection's handshake.
type HandleOptions struct {
	LocalGreeting protocol.Greeting                                       // what version/digests we advertise
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

	// Negotiate always takes (client, server) -- client preference wins digest selection
	version, _, digest, err := protocol.Negotiate(*clientGreeting, opts.LocalGreeting)
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
			if err == io.EOF {
				// client disconnected after greeting exchange
				// treat as clean disconnect rather than an error
				return nil, nil
			}
			return nil, fmt.Errorf("failed to read module request: %w", err)
		}
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}

		if string(line) == "#list" {
			// handle #list request
			// the server closes the connection after #list (upstream exits the child process)
			if err := s.sendModuleList(rw); err != nil {
				return nil, fmt.Errorf("failed to send module list: %w", err)
			}
			return nil, fmt.Errorf("connection closed after #list")
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
	} else {
		// no auth required -- send OK to signal module selection succeeded
		// this matches real rsync behavior: server always sends AUTHREQD or OK after module selection
		if _, err := rw.Write([]byte("@RSYNCD: OK\n")); err != nil {
			return nil, fmt.Errorf("failed to send OK: %w", err)
		}
	}

	// --- Phase 3: Argument Transmission ---

	var args []string
	if version >= 30 {
		args, err = readDelimitedArgs(rw, 0)
	} else {
		args, err = readDelimitedArgs(rw, '\n')
	}

	if err != nil {
		return nil, fmt.Errorf("failed to read arguments: %w", err)
	}

	// extract client_info from -e argument (proto >= 30)
	var clientInfo string
	if version >= 30 {
		clientInfo = extractClientInfo(args)
	}

	// --- Phase 4: Protocol Version Exchange (binary) ---
	// server sends its protocol version as int32 LE
	var protoBuf [4]byte
	binary.LittleEndian.PutUint32(protoBuf[:], uint32(version))
	if _, err := rw.Write(protoBuf[:]); err != nil {
		return nil, fmt.Errorf("failed to send protocol version: %w", err)
	}

	// read client's protocol version response
	if _, err := io.ReadFull(rw, protoBuf[:]); err != nil {
		return nil, fmt.Errorf("failed to read client protocol version: %w", err)
	}
	clientProto := int(binary.LittleEndian.Uint32(protoBuf[:]))
	if clientProto < version {
		version = clientProto
	}

	// --- Compat Flags Exchange (proto >= 30) ---
	// resolve compat flags based on server capabilities and client's advertised features
	var compatFlags int
	if version >= 30 {
		compatFlags = resolveCompatFlags(clientInfo)
		// send compat flags as varint
		if err := protocol.WriteVarint(rw, int32(compatFlags)); err != nil {
			return nil, fmt.Errorf("failed to send compat flags: %w", err)
		}
	}

	varintFlistFlags := (compatFlags & cfVarintFlistFlags) != 0

	return &HandshakeResult{
		Module:           selectedModule,
		Version:          version,
		Digest:           digest,
		VarintFlistFlags: varintFlistFlags,
	}, nil
}

func (s *Server) sendModuleList(rw io.Writer) error {
	for name, mod := range s.modules {
		line := fmt.Sprintf("%-15s\t%s\n", name, mod.Comment)
		if _, err := rw.Write([]byte(line)); err != nil {
			return err
		}
	}
	_, err := rw.Write([]byte("@RSYNCD: EXIT\n"))
	return err
}

// readLine reads from rw until a newline character is encountered.
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

// readDelimitedArgs reads arguments from rw until a double delimiter is encountered.
func readDelimitedArgs(rw io.Reader, delim byte) ([]string, error) {
	var args []string
	var currentArg []byte
	lastWasDelim := false

	b := make([]byte, 1)
	for {
		_, err := rw.Read(b)
		if err != nil {
			return nil, err
		}
		if b[0] == delim {
			if lastWasDelim {
				break
			}
			lastWasDelim = true
			if len(currentArg) > 0 {
				args = append(args, string(currentArg))
				currentArg = nil
			}
		} else {
			lastWasDelim = false
			currentArg = append(currentArg, b[0])
		}
	}

	if len(currentArg) > 0 {
		args = append(args, string(currentArg))
	}

	return args, nil
}

// extractClientInfo extracts the client_info string from the -e argument.
// The format is "e<version_sub><flags>" where flags are single characters.
func extractClientInfo(args []string) string {
	for _, arg := range args {
		if len(arg) >= 2 && arg[0] == 'e' {
			return arg[1:]
		}
	}
	return ""
}

// resolveCompatFlags builds the server's compat flags based on its capabilities
// and the client's advertised feature flags in clientInfo.
func resolveCompatFlags(clientInfo string) int {
	flags := 0

	// server supports CF_VARINT_FLIST_FLAGS when the client advertises 'v'
	for _, ch := range clientInfo {
		switch ch {
		case 'i':
			flags |= cfIncRecurse
		case 'L':
			flags |= cfSymlinkTimes
		case 's':
			flags |= cfSymlinkIconv
		case 'f':
			flags |= cfSafeFlist
		case 'x':
			flags |= cfAvoidXattrOptim
		case 'C':
			flags |= cfChksumSeedFix
		case 'I':
			flags |= cfInplacePartialDir
		case 'v':
			flags |= cfVarintFlistFlags
		case 'u':
			flags |= cfId0Names
		}
	}

	return flags
}
