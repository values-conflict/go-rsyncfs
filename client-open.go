package rsyncfs

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"io/fs"
	"strings"
	"time"

	"github.com/values-conflict/go-rsyncfs/protocol"
	"github.com/values-conflict/go-rsyncfs/protocol/mux"
)

// ErrNotExist is returned when a file or directory does not exist.
var ErrNotExist = fs.ErrNotExist

// fileListEntry holds parsed file list entry data.
type fileListEntry struct {
	name       string
	mode       fs.FileMode
	size       int64
	modTime    time.Time
	linkTarget string
	index      int
}

// flistReader reads and parses file list entries from a stream of raw bytes.
type flistReader struct {
	r       *bytes.Reader
	version int
	// delta-encoding state
	lastMode         fs.FileMode
	lastUID          int32
	lastGID          int32
	lastMtime        int64
	lastName         string
	varintFlistFlags bool
}

// newFlistReader creates a reader for parsing file list data.
func newFlistReader(data []byte, version int, varintFlistFlags bool) *flistReader {
	return &flistReader{
		r:                bytes.NewReader(data),
		version:          version,
		varintFlistFlags: varintFlistFlags,
	}
}

// readEntry reads the next file list entry. Returns nil entry and io.EOF at end-of-list.
func (fr *flistReader) readEntry(index int) (*fileListEntry, error) {
	// read xflags
	xflags, err := fr.readXflags()
	if err != nil {
		return nil, fmt.Errorf("read xflags: %w", err)
	}

	// xflags == 0 signals end-of-list
	if xflags == 0 {
		return nil, io.EOF
	}

	entry := &fileListEntry{index: index}

	// name encoding: prefix match + suffix
	prefixLen := 0
	if xflags&xmitSameName != 0 {
		b, err := fr.r.ReadByte()
		if err != nil {
			return nil, fmt.Errorf("read prefix len: %w", err)
		}
		prefixLen = int(b)
	}

	var suffixLen int
	if xflags&xmitLongName != 0 {
		v, err := protocol.ReadVarint(fr.r)
		if err != nil {
			return nil, fmt.Errorf("read long name suffix len: %w", err)
		}
		suffixLen = int(v)
	} else {
		b, err := fr.r.ReadByte()
		if err != nil {
			return nil, fmt.Errorf("read name suffix len: %w", err)
		}
		suffixLen = int(b)
	}

	suffix := make([]byte, suffixLen)
	if _, err := io.ReadFull(fr.r, suffix); err != nil {
		return nil, fmt.Errorf("read name suffix: %w", err)
	}
	entry.name = fr.lastName[:prefixLen] + string(suffix)

	// file size
	size, err := fr.readSize()
	if err != nil {
		return nil, fmt.Errorf("read size: %w", err)
	}
	entry.size = size

	// mtime
	if xflags&xmitSameTime == 0 {
		mtime, err := fr.readMtime()
		if err != nil {
			return nil, fmt.Errorf("read mtime: %w", err)
		}
		entry.modTime = time.Unix(mtime, 0)
		fr.lastMtime = mtime
	} else {
		entry.modTime = time.Unix(fr.lastMtime, 0)
	}

	// mode
	if xflags&xmitSameMode == 0 {
		mode, err := fr.readMode()
		if err != nil {
			return nil, fmt.Errorf("read mode: %w", err)
		}
		entry.mode = mode
		fr.lastMode = mode
	} else {
		entry.mode = fr.lastMode
	}

	// uid
	if xflags&xmitSameUID == 0 {
		_, err := fr.readID()
		if err != nil {
			return nil, fmt.Errorf("read uid: %w", err)
		}
	}

	// gid
	if xflags&xmitSameGID == 0 {
		_, err := fr.readID()
		if err != nil {
			return nil, fmt.Errorf("read gid: %w", err)
		}
	}

	// symlink target
	if entry.mode.Type() == fs.ModeSymlink {
		targetLen, err := protocol.ReadVarint(fr.r)
		if err != nil {
			return nil, fmt.Errorf("read symlink target len: %w", err)
		}
		if targetLen > 0 {
			target := make([]byte, targetLen)
			if _, err := io.ReadFull(fr.r, target); err != nil {
				return nil, fmt.Errorf("read symlink target: %w", err)
			}
			entry.linkTarget = string(target)
		}
	}

	// update delta state
	fr.lastName = entry.name

	return entry, nil
}

// readXflags reads the xmit flags from the stream.
func (fr *flistReader) readXflags() (int, error) {
	if fr.varintFlistFlags {
		v, err := protocol.ReadVarint(fr.r)
		if err != nil {
			return 0, err
		}
		// varint encoding uses XMIT_EXTENDED_FLAGS as a sentinel for zero
		if v == xmitExtendedFlags {
			return 0, nil
		}
		return int(v), nil
	}

	b, err := fr.r.ReadByte()
	if err != nil {
		return 0, err
	}
	xflags := int(b)

	// check for extended flags (proto >= 28)
	if xflags&xmitExtendedFlags != 0 && fr.version >= 28 {
		// read second byte for extended flags
		b2, err := fr.r.ReadByte()
		if err != nil {
			return 0, err
		}
		xflags |= int(b2) << 8
	}

	return xflags, nil
}

// readSize reads the file size in the appropriate format.
func (fr *flistReader) readSize() (int64, error) {
	if fr.version >= 30 {
		return protocol.ReadVarlong(fr.r, 3)
	}
	return protocol.ReadLongInt(fr.r)
}

// readMtime reads the modification time in the appropriate format.
func (fr *flistReader) readMtime() (int64, error) {
	if fr.version >= 30 {
		return protocol.ReadVarlong(fr.r, 4)
	}
	// proto < 30: int32 LE
	b := make([]byte, 4)
	if _, err := io.ReadFull(fr.r, b); err != nil {
		return 0, err
	}
	return int64(binary.LittleEndian.Uint32(b)), nil
}

// readMode reads the file mode and converts to fs.FileMode.
func (fr *flistReader) readMode() (fs.FileMode, error) {
	b := make([]byte, 4)
	if _, err := io.ReadFull(fr.r, b); err != nil {
		return 0, err
	}
	mode := int32(binary.LittleEndian.Uint32(b))

	// convert wire mode to fs.FileMode
	var fm fs.FileMode
	switch {
	case mode&0o170000 == 0o040000: // S_IFDIR
		fm = fs.ModeDir
	case mode&0o170000 == 0o120000: // S_IFLNK
		fm = fs.ModeSymlink
	case mode&0o170000 == 0o010000: // S_IFIFO
		// not directly representable in fs.FileMode, treat as regular
	case mode&0o170000 == 0o020000: // S_IFCHR
		// not directly representable
	case mode&0o170000 == 0o060000: // S_IFBLK
		// not directly representable
	default:
		// S_IFREG or unknown
	}
	fm |= fs.FileMode(mode & 0o7777)

	return fm, nil
}

// readID reads an id (uid or gid) in the appropriate format for the protocol version.
func (fr *flistReader) readID() (int32, error) {
	if fr.version >= 30 {
		return protocol.ReadVarint(fr.r)
	}
	b := make([]byte, 4)
	if _, err := io.ReadFull(fr.r, b); err != nil {
		return 0, err
	}
	return int32(binary.LittleEndian.Uint32(b)), nil
}

// readFileList reads the file list from the server.
// It reads MSG_DATA frames containing file list entries and parses them into a slice of entries.
// After the file list, it reads and discards the NDX_DONE marker.
func (s *Session) readFileList() ([]fileListEntry, error) {
	code, payload, err := s.muxReader.ReadMsg()
	if err != nil {
		return nil, fmt.Errorf("read file list msg: %w", err)
	}
	if code != mux.MsgData {
		return nil, fmt.Errorf("expected MSG_DATA for file list, got code %d", code)
	}

	flr := newFlistReader(payload, s.version, s.varintFlistFlags)
	var entries []fileListEntry
	index := 0

	for {
		entry, err := flr.readEntry(index)
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("parse file list entry %d: %w", index, err)
		}
		entries = append(entries, *entry)
		index++
	}

	// the NDX_DONE marker is included in the MSG_DATA payload (after the end-of-list marker)
	// it's already consumed by the flistReader or left as trailing bytes -- either way, we're done

	return entries, nil
}

// writeNdx writes a compressed file index to the wire.
// Implements the upstream write_ndx() algorithm from io.c.
func writeNdx(w io.Writer, ndx int, version int, prevNdx *int32) error {
	if version < 30 {
		return writeIntLE(w, uint32(int32(ndx)), 4)
	}
	if ndx == -1 {
		_, err := w.Write([]byte{0})
		return err
	}

	var prefix byte
	absNdx := ndx
	if ndx < 0 {
		prefix = 0xFF
		absNdx = -ndx
	}
	diff := int32(absNdx) - *prevNdx
	*prevNdx = int32(absNdx)

	var buf []byte
	switch {
	case diff >= 1 && diff <= 253:
		buf = []byte{prefix, byte(diff)}
	case diff >= 0 && diff <= 0x7FFF:
		buf = []byte{prefix, 0xFE, byte(diff >> 8), byte(diff)}
	default:
		buf = []byte{
			prefix, 0xFE,
			byte((absNdx >> 24) | 0x80),
			byte(absNdx),
			byte(absNdx >> 8),
			byte(absNdx >> 16),
		}
	}
	if prefix == 0 {
		buf = buf[1:] // strip prefix byte for positive indices
	}

	_, err := w.Write(buf)
	return err
}

// writeSelector sends a file selector to the server, requesting a specific file for transfer.
// The selector consists of a compressed NDX followed by item flags (shortint for proto >= 29).
func (s *Session) writeSelector(ndx int, iflags int) error {
	if err := writeNdx(s.rw, ndx, s.version, &s.prevNdx); err != nil {
		return fmt.Errorf("write selector ndx: %w", err)
	}

	if s.version >= 29 {
		// item flags as shortint (2 bytes LE)
		buf := []byte{byte(iflags), byte(iflags >> 8)}
		_, err := s.rw.Write(buf)
		return err
	}

	return nil
}

// item flags for selector (matching upstream rsync.h ITEM_* defines)
const (
	itemBasisTypeFollows = 1 << 11
	itemXnameFollows     = 1 << 12
	itemIsNew            = 1 << 13
	itemLocalChange      = 1 << 14
	itemTransfer         = 1 << 15 // request file transfer
	itemMissingData      = 1 << 16 // client has no local copy
)

// openModule handles opens within a single connected module.
// It reads the file list from the server and opens the requested path.
func (s *Session) openModule(name string) (fs.File, error) {
	// read file list from server
	entries, err := s.readFileList()
	if err != nil {
		return nil, fmt.Errorf("read file list: %w", err)
	}

	// strip leading slash for path matching
	name = strings.TrimPrefix(name, "/")
	if name == "" || name == "." {
		name = "."
	}

	// find the target entry
	target := findEntry(entries, name)
	if target == nil {
		return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrNotExist}
	}

	// handle by file type
	switch {
	case target.mode.IsDir():
		// return directory entries (children of this directory)
		children := filterChildren(entries, name)
		return newModuleDirFile(children, name), nil

	case target.mode.Type() == fs.ModeSymlink:
		// return a symlink file
		return newSymlinkFile(*target), nil

	default:
		// regular file: trigger data transfer
		return s.openFile(target, entries)
	}
}

// openFile triggers the data transfer protocol for a single file.
func (s *Session) openFile(target *fileListEntry, allEntries []fileListEntry) (*moduleFile, error) {
	// send selector to request this file
	iflags := itemTransfer | itemMissingData
	if err := s.writeSelector(target.index, iflags); err != nil {
		return nil, fmt.Errorf("write selector: %w", err)
	}

	// server responds with sum_head as MSG_DATA
	code, payload, err := s.muxReader.ReadMsg()
	if err != nil {
		return nil, fmt.Errorf("read sum head: %w", err)
	}
	if code != mux.MsgData {
		return nil, fmt.Errorf("expected MSG_DATA for sum head, got code %d", code)
	}

	// parse sum_head
	sh, err := parseSumHead(payload, s.version)
	if err != nil {
		return nil, fmt.Errorf("parse sum head: %w", err)
	}

	// read block checksums if count > 0
	if sh.count > 0 {
		code, _, err = s.muxReader.ReadMsg()
		if err != nil {
			return nil, fmt.Errorf("read block checksums: %w", err)
		}
		if code != mux.MsgData {
			return nil, fmt.Errorf("expected MSG_DATA for checksums, got code %d", code)
		}
	}

	// send delta stream - we have no local copy, so request all blocks
	// the delta stream format: for each block, send a token reference
	// token = -(blockIndex+1), encoded as int32 LE
	// followed by 0 to signal end of stream
	deltaBuf := new(bytes.Buffer)
	for i := int32(0); i < sh.count; i++ {
		token := -(i + 1) // token reference for block i
		var b [4]byte
		binary.LittleEndian.PutUint32(b[:], uint32(token))
		deltaBuf.Write(b[:])
	}
	// end of stream marker
	deltaBuf.Write([]byte{0, 0, 0, 0})

	if err := s.muxWriter.WriteMsg(mux.MsgData, deltaBuf.Bytes()); err != nil {
		return nil, fmt.Errorf("send delta stream: %w", err)
	}

	// server sends file data as MSG_DATA
	code, payload, err = s.muxReader.ReadMsg()
	if err != nil {
		return nil, fmt.Errorf("read file data: %w", err)
	}
	if code != mux.MsgData {
		return nil, fmt.Errorf("expected MSG_DATA for file data, got code %d", code)
	}

	// server sends file checksum for verification
	code, _, err = s.muxReader.ReadMsg()
	if err != nil {
		return nil, fmt.Errorf("read file checksum: %w", err)
	}
	if code != mux.MsgData {
		return nil, fmt.Errorf("expected MSG_DATA for file checksum, got code %d", code)
	}

	// send MSG_SUCCESS with file index
	var ndxBuf [4]byte
	binary.LittleEndian.PutUint32(ndxBuf[:], uint32(target.index))
	if err := s.muxWriter.WriteMsg(mux.MsgSuccess, ndxBuf[:]); err != nil {
		return nil, fmt.Errorf("send MSG_SUCCESS: %w", err)
	}

	return newModuleFile(*target, payload), nil
}

// parseSumHead parses the sum header from raw bytes.
func parseSumHead(data []byte, version int) (sumHead, error) {
	if len(data) < 12 {
		return sumHead{}, fmt.Errorf("sum head too short: %d bytes", len(data))
	}

	sh := sumHead{}
	sh.count = int32(binary.LittleEndian.Uint32(data[0:4]))
	sh.blength = int32(binary.LittleEndian.Uint32(data[4:8]))

	if version >= 27 {
		if len(data) < 16 {
			return sumHead{}, fmt.Errorf("sum head too short for proto >= 27: %d bytes", len(data))
		}
		sh.s2length = int32(binary.LittleEndian.Uint32(data[8:12]))
		sh.remainder = int32(binary.LittleEndian.Uint32(data[12:16]))
	} else {
		sh.s2length = 16 // default MD5 length
		sh.remainder = int32(binary.LittleEndian.Uint32(data[8:12]))
	}

	return sh, nil
}

// findEntry finds a file list entry by path name.
func findEntry(entries []fileListEntry, name string) *fileListEntry {
	for _, e := range entries {
		if e.name == name {
			return &e
		}
	}
	return nil
}

// filterChildren returns entries that are direct children of the given directory.
func filterChildren(entries []fileListEntry, dirName string) []fileListEntry {
	if dirName == "." {
		// root directory: return all entries except "."
		var children []fileListEntry
		for _, e := range entries {
			if e.name != "." && !strings.Contains(e.name, "/") {
				children = append(children, e)
			}
		}
		return children
	}

	prefix := dirName + "/"
	var children []fileListEntry
	for _, e := range entries {
		if strings.HasPrefix(e.name, prefix) {
			relName := strings.TrimPrefix(e.name, prefix)
			if !strings.Contains(relName, "/") {
				// direct child - rewrite name to relative
				e.name = relName
				children = append(children, e)
			}
		}
	}
	return children
}

// moduleFile implements fs.File for a regular file opened via rsync protocol.
type moduleFile struct {
	entry  fileListEntry
	data   []byte
	offset int64
}

var _ fs.File = (*moduleFile)(nil)
var _ io.ReaderAt = (*moduleFile)(nil)

func newModuleFile(entry fileListEntry, data []byte) *moduleFile {
	return &moduleFile{entry: entry, data: data}
}

func (f *moduleFile) Read(p []byte) (int, error) {
	if f.offset >= int64(len(f.data)) {
		return 0, io.EOF
	}
	n := copy(p, f.data[f.offset:])
	f.offset += int64(n)
	return n, nil
}

func (f *moduleFile) ReadAt(p []byte, off int64) (int, error) {
	if off >= int64(len(f.data)) {
		return 0, io.EOF
	}
	if off < 0 {
		return 0, io.EOF
	}
	n := copy(p, f.data[off:])
	return n, nil
}

func (f *moduleFile) Stat() (fs.FileInfo, error) {
	return &fileInfo{
		name:    baseName(f.entry.name),
		mode:    f.entry.mode,
		size:    f.entry.size,
		modTime: f.entry.modTime,
	}, nil
}

func (f *moduleFile) Close() error { return nil }

func (f *moduleFile) ReadDir(n int) ([]fs.DirEntry, error) {
	return nil, fmt.Errorf("readDir on non-directory file %q", f.entry.name)
}

// moduleDirFile implements fs.File for a directory opened via rsync protocol.
type moduleDirFile struct {
	entries []fileListEntry
	pos     int
	name    string
}

var _ fs.File = (*moduleDirFile)(nil)

func newModuleDirFile(entries []fileListEntry, name string) *moduleDirFile {
	return &moduleDirFile{entries: entries, name: name}
}

func (d *moduleDirFile) Stat() (fs.FileInfo, error) {
	return &fileInfo{
		name:    baseName(d.name),
		mode:    fs.ModeDir | 0o755,
		size:    0,
		modTime: time.Time{},
	}, nil
}

func (d *moduleDirFile) Read([]byte) (int, error) { return 0, fs.ErrInvalid }

func (d *moduleDirFile) ReadDir(n int) ([]fs.DirEntry, error) {
	if d.pos >= len(d.entries) {
		return nil, io.EOF
	}

	if n <= 0 || n > len(d.entries)-d.pos {
		n = len(d.entries) - d.pos
	}

	result := make([]fs.DirEntry, n)
	for i := 0; i < n; i++ {
		e := d.entries[d.pos+i]
		result[i] = &dirEntry{entry: e}
	}
	d.pos += n
	return result, nil
}

func (d *moduleDirFile) Close() error { return nil }

// dirEntry implements fs.DirEntry for a file list entry.
type dirEntry struct {
	entry fileListEntry
}

func (e *dirEntry) Name() string               { return e.entry.name }
func (e *dirEntry) IsDir() bool                { return e.entry.mode.IsDir() }
func (e *dirEntry) Type() fs.FileMode          { return e.entry.mode.Type() }
func (e *dirEntry) Info() (fs.FileInfo, error) { return e.entryInfo(), nil }

func (e *dirEntry) entryInfo() *fileInfo {
	return &fileInfo{
		name:    e.entry.name,
		mode:    e.entry.mode,
		size:    e.entry.size,
		modTime: e.entry.modTime,
	}
}

// symlinkFile implements fs.File for a symlink.
type symlinkFile struct {
	entry fileListEntry
}

var _ fs.File = (*symlinkFile)(nil)

func newSymlinkFile(entry fileListEntry) *symlinkFile {
	return &symlinkFile{entry: entry}
}

func (f *symlinkFile) Read([]byte) (int, error) { return 0, fs.ErrInvalid }

func (f *symlinkFile) Stat() (fs.FileInfo, error) {
	return &fileInfo{
		name:    baseName(f.entry.name),
		mode:    f.entry.mode,
		size:    int64(len(f.entry.linkTarget)),
		modTime: f.entry.modTime,
	}, nil
}

func (f *symlinkFile) ReadDir(n int) ([]fs.DirEntry, error) {
	return nil, fmt.Errorf("readDir on symlink %q", f.entry.name)
}

func (f *symlinkFile) Close() error { return nil }

// fileInfo implements fs.FileInfo for file list entries.
type fileInfo struct {
	name    string
	mode    fs.FileMode
	size    int64
	modTime time.Time
}

var _ fs.FileInfo = (*fileInfo)(nil)

func (fi *fileInfo) Name() string       { return fi.name }
func (fi *fileInfo) Size() int64        { return fi.size }
func (fi *fileInfo) Mode() fs.FileMode  { return fi.mode }
func (fi *fileInfo) ModTime() time.Time { return fi.modTime }
func (fi *fileInfo) IsDir() bool        { return fi.mode.IsDir() }
func (fi *fileInfo) Sys() any           { return nil }

// baseName returns the last component of a path.
func baseName(path string) string {
	if idx := strings.LastIndex(path, "/"); idx >= 0 {
		return path[idx+1:]
	}
	return path
}

// openRootMode handles opens in root mode where modules are top-level directories.
// Each operation opens a fresh connection -- the server closes after #list.
func (s *Session) openRootMode(name string) (fs.File, error) {
	if name == "." || name == "/" {
		// do a live #list to get current modules
		modules, err := doListRequest(s.connectFunc, s.client.Greeting)
		if err != nil {
			return nil, fmt.Errorf("list modules: %w", err)
		}
		return newRootDir(modules), nil
	}

	// strip leading slash for routing
	name = strings.TrimPrefix(name, "/")

	// split into module path
	parts := strings.SplitN(name, "/", 2)
	modName := parts[0]
	modulePath := ""
	if len(parts) > 1 {
		modulePath = parts[1]
	}

	// connect to the specific module and open the path within it
	rw, err := s.connectFunc(modName)
	if err != nil {
		return nil, fmt.Errorf("connect to module %q: %w", modName, err)
	}

	// do full handshake for this module
	client := &Client{
		Module:       modName,
		Greeting:     s.client.Greeting,
		AuthUser:     s.client.AuthUser,
		AuthResponse: s.client.AuthResponse,
	}

	session, err := client.Connect(rw)
	if err != nil {
		return nil, fmt.Errorf("handshake with module %q: %w", modName, err)
	}

	// open the path within the module
	if modulePath == "" {
		// opening the module itself -- treat as directory
		// read file list and return root directory
		entries, err := session.readFileList()
		if err != nil {
			return nil, fmt.Errorf("read file list for module %q: %w", modName, err)
		}
		return newModuleDirFile(filterChildren(entries, "."), modName), nil
	}

	return session.openModule(modulePath)
}
