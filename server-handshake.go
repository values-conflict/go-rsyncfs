package rsyncfs

import (
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"io"
	"io/fs"
	"os"
	"time"

	"github.com/values-conflict/go-rsyncfs/protocol"
	"github.com/values-conflict/go-rsyncfs/protocol/mux"
)

// HandleConnection runs the full rsync daemon protocol on a single connection.
// The rw is the underlying transport (TCP socket, pipe, etc).
// Returns when the connection is complete or an error occurs.
//
// Handles: greeting exchange, module selection (#list or named module),
// authentication, argument parsing, compat flags, checksum negotiation,
// file list transfer, selector loop, data transfer, final goodbye.
func (s *Server) HandleConnection(rw io.ReadWriter) error {
	s.Greeting.ApplyDefaults()

	result, err := s.doHandshake(rw)
	if err != nil {
		return err
	}
	if result == nil {
		return nil // clean disconnect or #list
	}

	mw := mux.NewWriter(rw)

	if err := s.sendFileList(mw, result.module.FS, ".", result.version, result.varintFlistFlags); err != nil {
		return fmt.Errorf("send file list: %w", err)
	}
	if err := mw.Flush(); err != nil {
		return fmt.Errorf("flush file list: %w", err)
	}

	return s.selectorLoop(rw, mw, result)
}

type handshakeResult struct {
	module              *ServerModule
	version             int
	digest              string
	varintFlistFlags    bool
	negotiatedChecksum  string
	seed                int32
}

// doHandshake runs the pre-transfer handshake (greeting, module, auth, args, compat, algorithms, seed).
// Returns nil result for clean disconnects or #list requests.
func (s *Server) doHandshake(rw io.ReadWriter) (*handshakeResult, error) {
	// step 1: greeting exchange
	if err := protocol.WriteGreeting(rw, s.Greeting); err != nil {
		return nil, fmt.Errorf("write greeting: %w", err)
	}

	clientGreeting, err := protocol.ReadGreeting(rw)
	if err != nil {
		_ = protocol.WriteError(rw, "invalid greeting")
		return nil, fmt.Errorf("read client greeting: %w", err)
	}

	version, _, digest, err := protocol.Negotiate(*clientGreeting, s.Greeting)
	if err != nil {
		_ = protocol.WriteError(rw, "protocol version negotiation failed")
		return nil, fmt.Errorf("negotiate version: %w", err)
	}

	// step 2: module selection
	moduleName, err := protocol.ReadModuleRequest(rw)
	if err != nil {
		if err == io.EOF {
			return nil, nil // clean disconnect
		}
		return nil, fmt.Errorf("read module request: %w", err)
	}

	// handle #list
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

	// step 3: authentication
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

	// step 4: argument transmission
	args, err := protocol.ReadArgs(rw, version)
	if err != nil {
		return nil, fmt.Errorf("read arguments: %w", err)
	}

	// step 5: compat flags exchange
	var clientInfo string
	if version >= 30 {
		clientInfo = protocol.ExtractClientInfo(args)
	}

	compatFlags := protocol.ResolveCompatFlags(serverCapabilities(), clientInfo)
	if err := protocol.WriteCompatFlags(rw, compatFlags, version); err != nil {
		return nil, fmt.Errorf("write compat flags: %w", err)
	}

	varintFlistFlags := (compatFlags & protocol.CompatVarintFlistFlags) != 0

	// step 6: algorithm negotiation
	var algos protocol.Algorithms
	if varintFlistFlags {
		algos, err = protocol.NegotiateAlgorithms(rw, protocol.SupportedDigests(), nil)
		if err != nil {
			return nil, fmt.Errorf("negotiate algorithms: %w", err)
		}
	} else {
		algos = protocol.DefaultAlgorithms(version)
	}

	// step 7: checksum seed exchange
	seed := int32(time.Now().UnixNano()) ^ int32(os.Getpid()<<6)
	if _, err := rand.Read(seedBuf[:]); err == nil {
		seed = int32(binary.LittleEndian.Uint32(seedBuf[:]))
	}
	if err := protocol.WriteChecksumSeed(rw, seed); err != nil {
		return nil, fmt.Errorf("write checksum seed: %w", err)
	}

	return &handshakeResult{
		module:             selected,
		version:            version,
		digest:             digest,
		varintFlistFlags:   varintFlistFlags,
		negotiatedChecksum: algos.Checksum,
		seed:               seed,
	}, nil
}

var seedBuf [4]byte

// serverCapabilities returns the compat flag bits this server supports.
func serverCapabilities() int {
	return protocol.CompatIncRecurse |
		protocol.CompatSymlinkTimes |
		protocol.CompatSymlinkIconv |
		protocol.CompatSafeFlist |
		protocol.CompatChksumSeedFix |
		protocol.CompatVarintFlistFlags |
		protocol.CompatId0Names
}

// hashEqual compares two byte slices in constant time to avoid timing attacks.
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

// selectorLoop handles the phase exchange, selector reading, and data transfer.
func (s *Server) selectorLoop(rw io.ReadWriter, mw *mux.Writer, result *handshakeResult) error {
	version := result.version
	seed := result.seed
	checksum := result.negotiatedChecksum

	maxPhase := 1
	if version >= 29 {
		maxPhase = 2
	}

	ndxState := protocol.NewNdxState()
	phase := 0

	for {
		sel, err := protocol.ReadSelector(rw, ndxState, version)
		if err != nil {
			return nil // connection closed or read error
		}

		if sel.Ndx < 0 {
			// ndx done: phase transition or end of transfer
			phase++
			if phase > maxPhase {
				break
			}
			// echo ndx done
			if err := echoNdxDone(mw, version); err != nil {
				return fmt.Errorf("echo ndx done: %w", err)
			}
			continue
		}

		// echo selector to client
		if err := echoSelector(mw, ndxState, version, sel); err != nil {
			return fmt.Errorf("echo selector: %w", err)
		}

		// only transfer files with ITEM_TRANSFER flag
		if sel.Iflags&protocol.ItemTransfer == 0 {
			continue
		}

		if err := s.sendFile(rw, mw, result.module.FS, sel, version, seed, checksum); err != nil {
			return fmt.Errorf("send file %d: %w", sel.Ndx, err)
		}
	}

	// final goodbye exchange
	if err := echoNdxDone(mw, version); err != nil {
		return nil
	}

	if version >= 29 {
		_, err := ndxState.ReadNdx(rw)
		if err != nil {
			return nil // connection closed
		}
		if version >= 31 {
			if err := echoNdxDone(mw, version); err != nil {
				return nil
			}
			_, err = ndxState.ReadNdx(rw)
			if err != nil {
				return nil
			}
		}
	}

	// stats exchange
	return sendStats(mw, version)
}

// echoNdxDone writes an NDX_DONE marker via the mux writer.
func echoNdxDone(mw *mux.Writer, version int) error {
	if version >= 30 {
		_, err := mw.Write([]byte{0})
		return err
	}
	var buf [4]byte
	binary.LittleEndian.PutUint32(buf[:], 0xFFFFFFFF)
	_, err := mw.Write(buf[:])
	return err
}

// echoSelector re-encodes the selector via the mux writer.
func echoSelector(mw *mux.Writer, ndx *protocol.NdxState, version int, sel *protocol.Selector) error {
	return protocol.WriteSelector(mw, ndx, version, sel)
}

// sendFileList walks the backing FS and writes file list entries via the mux writer.
func (s *Server) sendFileList(mw *mux.Writer, rootFS fs.FS, basePath string, version int, varintFlistFlags bool) error {
	entries, err := walkFS(rootFS, basePath)
	if err != nil {
		return fmt.Errorf("walk fs: %w", err)
	}

	fw := protocol.NewFlistWriter(mw, version, varintFlistFlags)

	for _, e := range entries {
		flistEntry := &protocol.FlistEntry{
			Name:       e.name,
			Mode:       modeToWire(e.mode),
			Size:       e.size,
			Mtime:      e.modTime.Unix(),
			UID:        0,
			GID:        0,
			LinkTarget: e.linkTarget,
		}
		if err := fw.WriteEntry(flistEntry); err != nil {
			return fmt.Errorf("write entry %q: %w", e.name, err)
		}
	}

	if err := fw.WriteEndMarker(); err != nil {
		return fmt.Errorf("write end marker: %w", err)
	}

	// ndx done marker after file list
	if version >= 30 {
		_, err := mw.Write([]byte{0})
		return err
	}
	var buf [4]byte
	binary.LittleEndian.PutUint32(buf[:], 0xFFFFFFFF)
	_, err = mw.Write(buf[:])
	return err
}

// fileEntry holds stat info extracted from fs.DirEntry for wire encoding.
type fileEntry struct {
	name       string
	mode       fs.FileMode
	size       int64
	modTime    time.Time
	linkTarget string
}

// walkFS performs a pre-order walk of rootFS rooted at basePath.
func walkFS(rootFS fs.FS, basePath string) ([]fileEntry, error) {
	var entries []fileEntry

	err := fs.WalkDir(rootFS, basePath, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		info, err := d.Info()
		if err != nil {
			return err
		}

		ename := path
		if basePath != "." && basePath != "/" {
			if stripped, ok := cutPrefixBytes(path, basePath); ok {
				if len(stripped) > 0 && stripped[0] == '/' {
					stripped = stripped[1:]
				}
				ename = string(stripped)
			}
		}
		if ename == "" {
			ename = "."
		}

		entry := fileEntry{
			name:    ename,
			mode:    info.Mode(),
			size:    info.Size(),
			modTime: info.ModTime(),
		}

		if info.Mode().Type() == fs.ModeSymlink {
			if rlfs, ok := rootFS.(interface {
				ReadLink(name string) (string, error)
			}); ok {
				if target, err := rlfs.ReadLink(path); err == nil {
					entry.linkTarget = target
				}
			}
		}

		entries = append(entries, entry)
		return nil
	})

	return entries, err
}

// cutPrefixBytes is a helper for bytes.CutPrefix compatibility.
func cutPrefixBytes(s, prefix string) ([]byte, bool) {
	bs := []byte(s)
	bp := []byte(prefix)
	if len(bs) < len(bp) {
		return nil, false
	}
	for i, b := range bp {
		if bs[i] != b {
			return nil, false
		}
	}
	return bs[len(bp):], true
}

// modeToWire converts fs.FileMode to raw wire mode (S_IFDIR | 0755, etc).
func modeToWire(mode fs.FileMode) uint32 {
	unixMode := uint32(mode.Perm())
	switch {
	case mode.IsDir():
		unixMode |= 0o040000 // S_IFDIR
	case mode.Type() == fs.ModeSymlink:
		unixMode |= 0o120000 // S_IFLNK
	default:
		unixMode |= 0o100000 // S_IFREG
	}
	return unixMode
}

// sendFile handles the data transfer for a single file.
//
// Protocol flow:
//  1. Send sum_head via mux writer
//  2. Send block checksums via mux writer
//  3. Flush
//  4. Read delta stream from raw connection
//  5. Send file data via mux writer
//  6. Send file checksum via mux writer
//  7. Flush
//  8. Send MSG_SUCCESS via mux writer
func (s *Server) sendFile(rw io.ReadWriter, mw *mux.Writer, fileFS fs.FS, sel *protocol.Selector, version int, seed int32, checksum string) error {
	// find the file path from the walk order -- we need to open it
	// for now, we walk to find the entry by index
	entries, err := walkFS(fileFS, ".")
	if err != nil {
		return fmt.Errorf("walk fs: %w", err)
	}

	if int(sel.Ndx) >= len(entries) {
		return fmt.Errorf("selector ndx %d out of range (max %d)", sel.Ndx, len(entries)-1)
	}

	entry := entries[sel.Ndx]

	// skip directories
	if entry.mode.IsDir() {
		return nil
	}

	f, err := fileFS.Open(entry.name)
	if err != nil {
		return fmt.Errorf("open %q: %w", entry.name, err)
	}
	defer f.Close()

	data, err := io.ReadAll(f)
	if err != nil {
		return fmt.Errorf("read file data: %w", err)
	}

	sh := computeSumHead(int64(len(data)), version)

	// step 1: send sum_head
	if err := protocol.WriteSumHead(mw, sh, version); err != nil {
		return fmt.Errorf("write sum head: %w", err)
	}

	// step 2: send block checksums
	if sh.Count > 0 {
		if err := sendBlockChecksums(mw, data, sh, checksum, seed); err != nil {
			return fmt.Errorf("send block checksums: %w", err)
		}
	}

	if err := mw.Flush(); err != nil {
		return fmt.Errorf("flush sum data: %w", err)
	}

	// step 3: read delta stream from raw connection
	if err := readDeltaStream(rw, sh); err != nil {
		return fmt.Errorf("read delta stream: %w", err)
	}

	// step 4: send file data
	if _, err := mw.Write(data); err != nil {
		return fmt.Errorf("send file data: %w", err)
	}

	// step 5: send file checksum
	s2Length := int(sh.S2Length)
	if s2Length == 0 {
		s2Length = 16 // default MD4/MD5 length
	}
	fileChecksum := protocol.Checksum2(data, checksum, s2Length, seed, true)
	if _, err := mw.Write(fileChecksum); err != nil {
		return fmt.Errorf("send file checksum: %w", err)
	}

	if err := mw.Flush(); err != nil {
		return fmt.Errorf("flush file data: %w", err)
	}

	// step 6: send MSG_SUCCESS
	payload := make([]byte, 4)
	binary.LittleEndian.PutUint32(payload, uint32(sel.Ndx))
	if err := mw.SendMsg(mux.MsgSuccess, payload); err != nil {
		return fmt.Errorf("send success: %w", err)
	}

	return nil
}

// defaultBlockSize is the standard rsync block size for files <= BLOCK_SIZE^2 bytes.
const defaultBlockSize = 700

// maxBlockSize is the maximum block size for protocol >= 30.
const maxBlockSize = 1 << 17

// computeSumHead calculates block parameters for a file of the given size.
func computeSumHead(fileSize int64, version int) protocol.SumHead {
	if fileSize == 0 {
		return protocol.SumHead{Count: 0}
	}

	blength := int32(defaultBlockSize)
	maxBl := int32(maxBlockSize)
	if version < 30 {
		maxBl = 1 << 29 // OLD_MAX_BLOCK_SIZE
	}

	if fileSize > defaultBlockSize*defaultBlockSize {
		c := int32(1)
		for l := fileSize; ; {
			l >>= 2
			if l == 0 {
				break
			}
			c <<= 1
		}
		if c >= maxBl {
			blength = maxBl
		} else {
			blength = 0
			for {
				blength |= c
				if fileSize < int64(blength)*int64(blength) {
					blength &= ^c
				}
				if c < 8 {
					break
				}
				c >>= 1
			}
			if blength < defaultBlockSize {
				blength = defaultBlockSize
			}
		}
	}

	count := int32(fileSize / int64(blength))
	remainder := int32(fileSize % int64(blength))
	if remainder > 0 {
		count++
	}

	s2length := int32(16) // MD4/MD5 digest length

	return protocol.SumHead{
		Count:     count,
		BLength:   blength,
		S2Length:  s2length,
		Remainder: remainder,
	}
}

// sendBlockChecksums writes per-block checksums to the mux writer.
func sendBlockChecksums(mw *mux.Writer, data []byte, sh protocol.SumHead, digest string, seed int32) error {
	s2Length := int(sh.S2Length)
	bufSize := sh.Count * (4 + int32(s2Length))
	buf := make([]byte, bufSize)

	offset := int64(0)
	bufOffset := int32(0)
	for i := int32(0); i < sh.Count; i++ {
		var blockEnd int64
		if i == sh.Count-1 && sh.Remainder > 0 {
			blockEnd = offset + int64(sh.Remainder)
		} else {
			blockEnd = offset + int64(sh.BLength)
		}

		block := data[offset:blockEnd]

		// sum1: rolling checksum (4 bytes LE)
		s1 := protocol.Checksum1(block)
		binary.LittleEndian.PutUint32(buf[bufOffset:bufOffset+4], s1)
		bufOffset += 4

		// sum2: strong hash
		s2 := protocol.Checksum2(block, digest, s2Length, seed, true)
		copy(buf[bufOffset:bufOffset+int32(len(s2))], s2)
		bufOffset += int32(len(s2))

		offset = blockEnd
	}

	_, err := mw.Write(buf)
	return err
}

// readDeltaStream reads the delta stream from raw connection.
// The delta stream format (non-compressed) is:
//   - Literal data: int32(len) > 0, followed by len bytes
//   - Token reference: int32(-(blockIndex+1)), means "I need block blockIndex"
//   - End of stream: int32(0)
func readDeltaStream(r io.Reader, sh protocol.SumHead) error {
	for {
		var buf [4]byte
		if _, err := io.ReadFull(r, buf[:]); err != nil {
			return fmt.Errorf("read delta command: %w", err)
		}
		cmd := int32(binary.LittleEndian.Uint32(buf[:]))
		if cmd == 0 {
			break // end of stream
		}
		if cmd > 0 {
			// literal data: skip cmd bytes
			literal := make([]byte, cmd)
			if _, err := io.ReadFull(r, literal); err != nil {
				return fmt.Errorf("read literal data: %w", err)
			}
		} else {
			// token reference: validate block index
			blockIdx := -cmd - 1
			if blockIdx < 0 || blockIdx >= sh.Count {
				return fmt.Errorf("invalid block index %d (count=%d)", blockIdx, sh.Count)
			}
		}
	}
	return nil
}

// sendStats writes transfer stats via the mux writer.
// For now, sends zeros; real stats would be tracked during transfer.
func sendStats(mw *mux.Writer, version int) error {
	// total_read, total_written, total_size
	for _, v := range []int64{0, 0, 0} {
		if err := protocol.WriteVarlong(mw, v, 3); err != nil {
			return fmt.Errorf("write stat: %w", err)
		}
	}
	if version >= 29 {
		// flist_buildtime, flist_xfertime
		for _, v := range []int64{0, 0} {
			if err := protocol.WriteVarlong(mw, v, 3); err != nil {
				return fmt.Errorf("write stat: %w", err)
			}
		}
	}
	return mw.Flush()
}
