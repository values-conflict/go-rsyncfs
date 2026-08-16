package rsyncfs

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

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
	if err := mw.Flush(); err != nil {
		return fmt.Errorf("flush file list: %w", err)
	}

	entries, err := walkFS(result.Module.FS, ".")
	if err != nil {
		return fmt.Errorf("walk fs: %w", err)
	}

	// --- Phase Exchange + Selector Loop ---
	// After the file list, the client and server exchange NDX_DONE markers to synchronize phases.
	// For proto >= 29, there are 2 phases; for older, just 1.
	// In each phase, the client sends NDX_DONE, the server reads it and responds with its own NDX_DONE.
	// After the phase exchange completes, the client sends file selectors (or NDX_DONE if no files to transfer).
	// The server reads selectors, sends file data, and loops until the client sends a final NDX_DONE that exceeds maxPhase.
	maxPhase := 1
	if result.Version >= 29 {
		maxPhase = 2
	}

	ndx := newNdxState()
	phase := 0
	for {
		sel, err := ndx.readSelector(mr, result.Version)
		if err != nil {
			return nil // connection closed or read error
		}
		if sel.ndx < 0 {
			// NDX_DONE: phase transition or end of transfer
			phase++
			if phase > maxPhase {
				break
			}
			// For proto >= 30: compressed NDX_DONE is single byte 0x00
			// For proto < 30: plain int32 LE of -1 (0xFFFFFFFF)
			if result.Version >= 30 {
				if _, err := mw.Write([]byte{0}); err != nil {
					return fmt.Errorf("write ndx done: %w", err)
				}
			} else {
				var ndxBuf [4]byte
				binary.LittleEndian.PutUint32(ndxBuf[:], 0xFFFFFFFF)
				if _, err := mw.Write(ndxBuf[:]); err != nil {
					return fmt.Errorf("write ndx done: %w", err)
				}
			}
			if err := mw.Flush(); err != nil {
				return fmt.Errorf("flush ndx done: %w", err)
			}
			continue
		}
		if int(sel.ndx) >= len(entries) {
			return nil // invalid selector, exit cleanly
		}

		entry := entries[sel.ndx]
		// Directories don't need file data transfer
		if entry.mode.IsDir() {
			continue
		}
		// Only transfer files with ITEM_TRANSFER flag set (0x8000)
		if sel.iflags&0x8000 == 0 {
			continue
		}

		f, err := result.Module.FS.Open(entry.name)
		if err != nil {
			return fmt.Errorf("open %q: %w", entry.name, err)
		}

		if err := sendFile(mr, mw, f, result.Version); err != nil {
			f.Close()
			return fmt.Errorf("send file %q: %w", entry.name, err)
		}
		f.Close()
	}

	// --- Final Goodbye Exchange ---
	// Server writes NDX_DONE to signal end of transfer, then reads the
	// client's final NDX_DONE. For proto >= 31, there's an extra
	// NDX_DONE round-trip (matching read_final_goodbye in upstream).
	if result.Version >= 30 {
		if _, err := mw.Write([]byte{0}); err != nil {
			return nil
		}
	} else {
		var ndxBuf [4]byte
		binary.LittleEndian.PutUint32(ndxBuf[:], 0xFFFFFFFF)
		if _, err := mw.Write(ndxBuf[:]); err != nil {
			return nil
		}
	}
	if err := mw.Flush(); err != nil {
		return nil
	}

	if result.Version >= 29 {
		_, err := ndx.readSelector(mr, result.Version)
		if err != nil {
			return nil // connection closed
		}
		if result.Version >= 31 {
			// Extra NDX_DONE round-trip for proto >= 31
			if _, err := mw.Write([]byte{0}); err != nil {
				return nil
			}
			if err := mw.Flush(); err != nil {
				return nil
			}
			_, err = ndx.readSelector(mr, result.Version)
			if err != nil {
				return nil
			}
		}
	}

	return nil
}

// handshakeResult contains the outcome of a successful server handshake.
type handshakeResult struct {
	Module             *ServerModule
	Version            int
	Digest             string
	VarintFlistFlags   bool
	NegotiatedChecksum string // empty when CF_VARINT_FLIST_FLAGS is not set
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

	// --- Compat Flags Exchange (proto >= 30) ---
	// the binary protocol version exchange (write_int/read_int) is skipped when the greeting was already exchanged (Phase 1), since remote_protocol is already set
	// the compat flags exchange is separate and always happens for proto >= 30
	var compatFlags int
	if version >= 30 {
		compatFlags = resolveCompatFlags(clientInfo)
		if err := protocol.WriteVarint(rw, int32(compatFlags)); err != nil {
			return nil, fmt.Errorf("send compat flags: %w", err)
		}
	}

	// --- Checksum Negotiation (when CF_VARINT_FLIST_FLAGS is set) ---
	// when the 'v' compat flag is negotiated, both sides exchange checksum algorithm lists as vstrings
	var negotiatedChecksum string
	if (compatFlags & cfVarintFlistFlags) != 0 {
		// server sends its checksum list as a vstring (space-separated names)
		serverChecksums := "md5 md4"
		if err := writeVstring(rw, serverChecksums); err != nil {
			return nil, fmt.Errorf("send checksum list: %w", err)
		}

		// server reads the client's checksum list as a vstring
		clientChecksums, err := readVstring(rw)
		if err != nil {
			return nil, fmt.Errorf("read client checksum list: %w", err)
		}

		// pick the first algorithm in the client's list that the server also supports
		negotiatedChecksum = negotiateChecksum(clientChecksums, serverChecksums)
	}

	// --- Checksum Seed Exchange ---
	// server sends a random seed for the block checksum algorithm
	checksumSeed := int32(time.Now().Unix()) ^ int32(os.Getpid()<<6)
	if err := writeInt32LE(rw, checksumSeed); err != nil {
		return nil, fmt.Errorf("send checksum seed: %w", err)
	}

	return &handshakeResult{
		Module:             selectedModule,
		Version:            version,
		Digest:             digest,
		VarintFlistFlags:   (compatFlags & cfVarintFlistFlags) != 0,
		NegotiatedChecksum: negotiatedChecksum,
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

// extractClientInfo extracts the client_info string from the combined short-options argument.
// The client embeds the feature flags as "e<version_sub><flags>" within a combined short-options argument like "-vlogDtpr.eiLsfxCIvu".
// The 'e' char is part of the combined flags, and everything after it is the client_info string.
func extractClientInfo(args []string) string {
	for _, arg := range args {
		// look for 'e' embedded in combined short options (e.g., "-vle.ifxCIvu")
		if idx := strings.IndexByte(arg, 'e'); idx >= 0 && len(arg) > idx+1 {
			return arg[idx+1:]
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

// negotiateChecksum picks the first algorithm in the client's list that the server also supports.
// Both lists are space-separated strings.
func negotiateChecksum(clientList, serverList string) string {
	clientAlgos := strings.Fields(clientList)
	serverSet := make(map[string]struct{})
	for _, a := range strings.Fields(serverList) {
		serverSet[a] = struct{}{}
	}
	for _, a := range clientAlgos {
		if _, ok := serverSet[a]; ok {
			return a
		}
	}
	// fallback: md5 for proto >= 30, md4 otherwise
	return "md5"
}

// writeInt32LE writes a 32-bit little-endian integer to w.
func writeInt32LE(w io.Writer, v int32) error {
	var buf [4]byte
	binary.LittleEndian.PutUint32(buf[:], uint32(v))
	_, err := w.Write(buf[:])
	return err
}
