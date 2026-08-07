package rsyncfs

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"io"
	"strings"

	"github.com/values-conflict/go-rsyncfs/protocol"
	"github.com/values-conflict/go-rsyncfs/protocol/mux"
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

// HandleOptions provides configuration for handling a connection.
type HandleOptions struct {
	LocalGreeting protocol.Greeting                                       // what version/digests we advertise
	AuthCallback  func(username string, challenge []byte) ([]byte, error) // nil = no auth required
}

// HandleConnection runs the full rsync protocol on a single connection:
//
// - handshake (Phases 1-4)
// - file list transfer
// - file data transfers
//
// It returns when the connection is closed or an error occurs.
//
// If opts.LocalGreeting is the zero value, defaults are applied via [protocol.Greeting.ApplyDefaults].
func (s *Server) HandleConnection(rw io.ReadWriter, opts HandleOptions) error {
	opts.LocalGreeting.ApplyDefaults()

	// --- Phases 1-4: Handshake ---

	result, err := s.doHandshake(rw, opts)
	if err != nil {
		return err
	}
	if result == nil {
		return nil // clean disconnect or #list
	}

	// --- Phase 5: Data Transfer ---

	mw := mux.NewWriter(rw)
	mr := mux.NewReader(rw)

	if err := sendFileList(mw, result.Module.FS, ".", result.Version, result.VarintFlistFlags); err != nil {
		return fmt.Errorf("send file list: %w", err)
	}

	entries, err := walkFS(result.Module.FS, ".")
	if err != nil {
		return fmt.Errorf("walk fs: %w", err)
	}

	// handle file transfer requests (selectors are MSG_DATA mux frames)
	ndx := newNdxState()
	for {
		sel, err := ndx.readSelector(mr, result.Version)
		if err != nil {
			return nil // connection closed or read error
		}
		if sel.ndx < 0 || int(sel.ndx) >= len(entries) {
			return nil // invalid selector, exit cleanly
		}

		f, err := result.Module.FS.Open(entries[sel.ndx].name)
		if err != nil {
			return fmt.Errorf("open %q: %w", entries[sel.ndx].name, err)
		}

		if err := sendFile(mr, mw, f, result.Version); err != nil {
			f.Close()
			return fmt.Errorf("send file %q: %w", entries[sel.ndx].name, err)
		}
		f.Close()
	}
}

// handshakeResult contains the outcome of a successful server handshake.
type handshakeResult struct {
	Module           *ServerModule
	Version          int
	Digest           string
	VarintFlistFlags bool
}

// doHandshake runs Phases 1-4 of the rsync protocol and returns the result.
// Returns nil result for clean disconnects or #list requests.
func (s *Server) doHandshake(rw io.ReadWriter, opts HandleOptions) (*handshakeResult, error) {
	// --- Phase 1: Greeting Exchange ---

	if _, err := rw.Write([]byte(opts.LocalGreeting.String())); err != nil {
		return nil, fmt.Errorf("send greeting: %w", err)
	}

	clientGreetLine, err := readLine(rw)
	if err != nil {
		return nil, fmt.Errorf("read client greeting: %w", err)
	}

	clientGreeting, err := protocol.ParseGreeting(string(clientGreetLine))
	if err != nil {
		_ = s.SendError(rw, "invalid greeting")
		return nil, fmt.Errorf("parse client greeting: %w", err)
	}

	version, _, digest, err := protocol.Negotiate(*clientGreeting, opts.LocalGreeting)
	if err != nil {
		_ = s.SendError(rw, "protocol version negotiation failed")
		return nil, fmt.Errorf("negotiation: %w", err)
	}

	// --- Phase 2: Module Selection & Authentication ---

	var selectedModule *ServerModule

Loop:
	for {
		line, err := readLine(rw)
		if err != nil {
			if err == io.EOF {
				return nil, nil // clean disconnect
			}
			return nil, fmt.Errorf("read module request: %w", err)
		}
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}

		if string(line) == "#list" {
			if err := s.sendModuleList(rw); err != nil {
				return nil, fmt.Errorf("send module list: %w", err)
			}
			return nil, nil // connection closes after #list
		}

		mod, ok := s.modules[string(line)]
		if !ok {
			_ = s.SendError(rw, "Unknown module")
			return nil, fmt.Errorf("unknown module: %s", line)
		}
		selectedModule = mod
		break Loop
	}

	// Authentication (if configured)
	if opts.AuthCallback != nil {
		challenge := make([]byte, 16)

		challengeB64 := base64.StdEncoding.EncodeToString(challenge)
		if _, err := rw.Write([]byte(fmt.Sprintf("@RSYNCD: AUTHREQD %s\n", challengeB64))); err != nil {
			return nil, fmt.Errorf("send auth request: %w", err)
		}

		authLine, err := readLine(rw)
		if err != nil {
			return nil, fmt.Errorf("read auth response: %w", err)
		}

		parts := strings.Fields(string(authLine))
		if len(parts) < 2 {
			_ = s.SendError(rw, "Authentication failed")
			return nil, fmt.Errorf("invalid auth response format")
		}

		username := parts[0]
		responseHash, err := base64.StdEncoding.DecodeString(parts[1])
		if err != nil {
			_ = s.SendError(rw, "Authentication failed")
			return nil, fmt.Errorf("decode auth hash: %w", err)
		}

		expectedResponse, err := opts.AuthCallback(username, challenge)
		if err != nil || !bytes.Equal(responseHash, expectedResponse) {
			_ = s.SendError(rw, "Authentication failed")
			return nil, fmt.Errorf("authentication failed for user %s", username)
		}

		if _, err := rw.Write([]byte("@RSYNCD: OK\n")); err != nil {
			return nil, fmt.Errorf("send auth success: %w", err)
		}
	} else {
		if _, err := rw.Write([]byte("@RSYNCD: OK\n")); err != nil {
			return nil, fmt.Errorf("send OK: %w", err)
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
		return nil, fmt.Errorf("read arguments: %w", err)
	}

	var clientInfo string
	if version >= 30 {
		clientInfo = extractClientInfo(args)
	}

	// --- Phase 4: Protocol Version Exchange (binary) ---

	var protoBuf [4]byte
	binary.LittleEndian.PutUint32(protoBuf[:], uint32(version))
	if _, err := rw.Write(protoBuf[:]); err != nil {
		return nil, fmt.Errorf("send protocol version: %w", err)
	}

	if _, err := io.ReadFull(rw, protoBuf[:]); err != nil {
		return nil, fmt.Errorf("read client protocol version: %w", err)
	}
	clientProto := int(binary.LittleEndian.Uint32(protoBuf[:]))
	if clientProto < version {
		version = clientProto
	}

	// Compat Flags Exchange (proto >= 30)
	var compatFlags int
	if version >= 30 {
		compatFlags = resolveCompatFlags(clientInfo)
		if err := protocol.WriteVarint(rw, int32(compatFlags)); err != nil {
			return nil, fmt.Errorf("send compat flags: %w", err)
		}
	}

	return &handshakeResult{
		Module:           selectedModule,
		Version:          version,
		Digest:           digest,
		VarintFlistFlags: (compatFlags & cfVarintFlistFlags) != 0,
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

// resolveCompatFlags builds the server's compat flags based on its capabilities and the client's advertised feature flags in clientInfo.
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
