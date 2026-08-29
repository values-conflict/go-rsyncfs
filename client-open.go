package rsyncfs

import (
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"sort"
	"strings"
	"time"

	"github.com/values-conflict/go-rsyncfs/protocol"
)

// Open implements fs.FS.  In module mode (a Session from [Client.Connect])
// it opens the named file or directory within the module; in root mode (a
// Session from [Client.OpenRoot]) the name routes to a module and each
// operation runs on its own connection.
//
// A regular file open is a complete, self-contained rsync transfer: the
// connection's file list is read, one selector is sent, the data is pulled
// and verified, and the session is torn down (phase exchange, stats, final
// goodbye).  The rsync protocol is a single batch session per connection,
// so the connection is consumed by the end of the open and the next open
// establishes a fresh one via [Client.ConnectFunc] (module mode) or the
// root-mode connect func.
func (s *Session) Open(name string) (fs.File, error) {
	// Root mode: no live connection; each operation is its own connection.
	if s.rw == nil && s.connectFunc != nil {
		return s.openRoot(name)
	}

	if !fs.ValidPath(name) {
		return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrInvalid}
	}

	return s.withEntry(name, openFileFromContext)
}

// Lstat implements fs.ReadLinkFS: it returns a [fs.FileInfo] for the named
// entry without following symlinks, a complete operation in its own right.
func (s *Session) Lstat(name string) (fs.FileInfo, error) {
	if s.rw == nil && s.connectFunc != nil {
		return s.rootLstat(name)
	}
	if !fs.ValidPath(name) {
		return nil, &fs.PathError{Op: "lstat", Path: name, Err: fs.ErrInvalid}
	}
	var info fs.FileInfo
	_, err := s.withEntry(name, func(ctx *openContext) (fs.File, error) {
		info = flistInfo(baseName(ctx.name), ctx.entry)
		return nil, nil
	})
	if err != nil {
		return nil, err
	}
	return info, nil
}

// rootLstat resolves an Lstat in root mode.
func (s *Session) rootLstat(name string) (fs.FileInfo, error) {
	name = strings.TrimPrefix(name, "/")
	parts := strings.SplitN(name, "/", 2)
	if len(parts) < 2 || parts[1] == "" {
		// A bare module name: report it as a directory.
		return &flistFileInfo{name: parts[0], mode: fs.ModeDir, size: 0}, nil
	}
	rw, err := s.connectFunc(parts[0])
	if err != nil {
		return nil, fmt.Errorf("connect to module %q: %w", parts[0], err)
	}
	c := *s.client
	c.Module = parts[0]
	sess, err := c.Connect(rw)
	if err != nil {
		return nil, fmt.Errorf("handshake with module %q: %w", parts[0], err)
	}
	var info fs.FileInfo
	_, err = sess.withEntry(parts[1], func(ctx *openContext) (fs.File, error) {
		info = flistInfo(baseName(parts[1]), ctx.entry)
		return nil, nil
	})
	if err != nil {
		return nil, err
	}
	return info, nil
}

// ReadLink implements fs.ReadLinkFS: it returns the target of the symlink
// named name, a complete operation in its own right (the file list is read
// on a fresh connection and the session is torn down).
func (s *Session) ReadLink(name string) (string, error) {
	if s.rw == nil && s.connectFunc != nil {
		return s.rootReadLink(name)
	}
	if !fs.ValidPath(name) {
		return "", &fs.PathError{Op: "readlink", Path: name, Err: fs.ErrInvalid}
	}
	var target string
	_, err := s.withEntry(name, func(ctx *openContext) (fs.File, error) {
		if !wireModeIsSymlink(ctx.entry.Mode) {
			return nil, &fs.PathError{Op: "readlink", Path: name, Err: errors.New("not a symlink")}
		}
		target = ctx.entry.LinkTarget
		return nil, nil
	})
	if err != nil {
		return "", err
	}
	return target, nil
}

// withEntry runs fn on the file list entry for name over a fresh (or live)
// connection, always tearing the session down by the end.  When the entry
// is not found fn is not called and an ErrNotExist is returned.
func (s *Session) withEntry(name string, fn func(*openContext) (fs.File, error)) (fs.File, error) {
	sess, err := s.acquireLive()
	if err != nil {
		return nil, err
	}
	defer sess.consume()

	entries, err := sess.readFileList()
	if err != nil {
		return nil, fmt.Errorf("read file list: %w", err)
	}
	// The daemon's NDX is the position in its sorted file list, not the
	// wire (walk) order.  Sort the received entries the same way (upstream
	// flist_sort_and_clean / f_name_cmp) so the index we send for a name is
	// the daemon's index for it.
	ndxByName := sortFlistEntries(entries, sess.version)

	ctx := &openContext{sess: sess, entries: entries, name: name, ndx: -1}
	if idx := findFlistEntry(entries, name); idx >= 0 {
		ctx.entry = entries[idx]
		ctx.ndx = ndxByName[entries[idx].Name]
	}

	var (
		f    fs.File
		oerr error
	)
	if ctx.entry != nil {
		f, oerr = fn(ctx)
	} else {
		oerr = &fs.PathError{Op: "open", Path: name, Err: fs.ErrNotExist}
	}
	if ferr := sess.finishSession(); ferr != nil && oerr == nil {
		oerr = fmt.Errorf("finish session: %w", ferr)
	}
	return f, oerr
}

// openContext carries the per-operation state: the live session, the parsed
// file list, and the resolved target entry (nil when not found).
type openContext struct {
	sess    *Session
	entries []*protocol.FlistEntry
	entry   *protocol.FlistEntry
	ndx     int32
	name    string
}

// openFileFromContext dispatches an opened entry to the right file type:
// a directory lists its children, a symlink carries its target, and a
// regular file is pulled over the wire.
func openFileFromContext(ctx *openContext) (fs.File, error) {
	e := ctx.entry
	switch {
	case wireModeIsDir(e.Mode):
		return newModuleDirFile(ctx.name, e, filterFlistChildren(ctx.entries, ctx.name)), nil
	case wireModeIsSymlink(e.Mode):
		return &symlinkFile{info: flistInfo(baseName(ctx.name), e), linkTarget: e.LinkTarget}, nil
	default:
		return ctx.sess.openFile(e, ctx.ndx)
	}
}

// acquireLive returns the session with a live, unconsumed connection,
// re-establishing one via [Client.ConnectFunc] when the current connection
// was already consumed by a transfer.
func (s *Session) acquireLive() (*Session, error) {
	if s.rw != nil && !s.consumed {
		return s, nil
	}
	if s.client == nil || s.client.ConnectFunc == nil {
		return nil, errors.New("no live connection and ConnectFunc is not set")
	}
	rw, err := s.client.ConnectFunc(s.client.Module)
	if err != nil {
		return nil, fmt.Errorf("ConnectFunc(%q): %w", s.client.Module, err)
	}
	if err := s.client.runHandshake(rw, s); err != nil {
		return nil, err
	}
	return s, nil
}

// consume closes the live connection and marks it used so the next open
// reconnects.  Any pending mux output (proto >= 30) is flushed first so the
// daemon's final goodbye reads see the last NDX_DONE rather than an early
// EOF.
func (s *Session) consume() {
	if s.mw != nil {
		_ = s.mw.Flush()
	}
	if s.rw != nil {
		if c, ok := s.rw.(io.Closer); ok {
			_ = c.Close()
		}
	}
	s.consumed = true
	s.rw = nil
	s.in = nil
	s.out = nil
	s.mr = nil
	s.mw = nil
}

// readFileList reads the file list from the daemon, parses every entry, and
// consumes the trailing id lists and io_error so the stream is positioned at
// the start of the selector phase.
func (s *Session) readFileList() ([]*protocol.FlistEntry, error) {
	fr := protocol.NewFlistReader(s.in, s.version, s.varintFlist)
	// The client requested -o (uid), -g (gid), -D (devices), and
	// -l (symlink targets), which the daemon echoes as preserved fields;
	// it did not request hard links (-H), atimes, or checksums, so those
	// are not on the wire.
	fr.SetPreserveHardlinks(false)

	var entries []*protocol.FlistEntry
	for {
		e, err := fr.ReadEntry()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		entries = append(entries, e)
	}
	if err := s.readFlistTrailer(); err != nil {
		return nil, err
	}
	return entries, nil
}

// readFlistTrailer consumes the trailer after the file list end marker.  The
// client requested -o and -g, so the daemon sends a uid list and a gid list:
// each is a run of (id, name) pairs terminated by a zero id (upstream
// recv_id_list / recv_user_name).  The flist entries carry the numeric uid/
// gid, so the names are only consumed here to keep the stream aligned.  When
// CF_ID0_NAMES is negotiated the id-0 name follows each list's terminator,
// and below proto 30 an int32 io_error trailer closes the list.
func (s *Session) readFlistTrailer() error {
	if err := s.readIDList(true); err != nil {
		return fmt.Errorf("read uid list: %w", err)
	}
	if err := s.readIDList(true); err != nil {
		return fmt.Errorf("read gid list: %w", err)
	}
	if !s.varintFlist && s.version < 30 {
		if _, err := protocol.ReadInt32(s.in); err != nil {
			return fmt.Errorf("read io_error trailer: %w", err)
		}
	}
	return nil
}

// readIDList reads (and discards) one uid or gid id list: a sequence of
// (id, name) pairs terminated by a zero id.  When present, the id-0 name
// follows the terminator.
func (s *Session) readIDList(present bool) error {
	if !present {
		return nil
	}
	for {
		id, err := s.readIDVarint()
		if err != nil {
			return err
		}
		if id == 0 {
			break
		}
		if err := s.skipIDName(); err != nil {
			return err
		}
	}
	if s.id0Names {
		return s.skipIDName()
	}
	return nil
}

// readIDVarint reads one id-list id: a varint from proto 30 on, an int32
// below.
func (s *Session) readIDVarint() (int, error) {
	if s.version >= 30 {
		v, err := protocol.ReadVarint(s.in)
		if err != nil {
			return 0, err
		}
		return int(v), nil
	}
	v, err := protocol.ReadInt32(s.in)
	if err != nil {
		return 0, err
	}
	return int(v), nil
}

// skipIDName reads and discards one id name: a byte length followed by that
// many bytes (upstream recv_user_name / recv_group_name).
func (s *Session) skipIDName() error {
	var b [1]byte
	if _, err := io.ReadFull(s.in, b[:]); err != nil {
		return err
	}
	if b[0] == 0 {
		return nil
	}
	_, err := io.CopyN(io.Discard, s.in, int64(b[0]))
	return err
}

// openFile transfers one regular file: it sends the selector and a null sum
// head (the client has no local copy), reads the echoed selector, the
// daemon's sum head, and the delta stream, verifies the whole-file checksum,
// and returns a file carrying the data.
func (s *Session) openFile(e *protocol.FlistEntry, ndx int32) (fs.File, error) {
	// Selector: ITEM_TRANSFER (request the data) plus ITEM_MISSING_DATA
	// (no local copy).  Below proto 29 the iflags are not sent on the wire.
	if err := protocol.WriteSelector(s.out, s.ndx, s.version, &protocol.Selector{
		Ndx:    ndx,
		Iflags: protocol.ItemTransfer | protocol.ItemMissingData,
	}); err != nil {
		return nil, fmt.Errorf("write selector: %w", err)
	}
	// The generator's sum struct: a null head (count 0) tells the daemon the
	// client has no local copy, so the whole file comes back as literals.
	if err := protocol.WriteSumHead(s.out, protocol.SumHead{}, s.version); err != nil {
		return nil, fmt.Errorf("write sum head: %w", err)
	}

	// The daemon echoes the selector before the data (upstream
	// write_ndx_and_attrs); consume it so the stream stays aligned.  The echo
	// is encoded with the daemon's separate write NDX state, so decode it with
	// our separate receive state (s.recvNdx), not the state we sent with.
	if _, err := protocol.ReadSelector(s.in, s.recvNdx, s.version); err != nil {
		return nil, fmt.Errorf("read echoed selector: %w", err)
	}

	// The daemon's sum head describes how the delta stream is chunked.
	sh, err := protocol.ReadSumHead(s.in, s.version)
	if err != nil {
		return nil, fmt.Errorf("read sum head: %w", err)
	}
	s2len := int(sh.S2Length)
	if s2len <= 0 {
		s2len = 16 // proto < 27 carries no s2length; md4/md5 are both 16
	}

	// Rebuild the file from the delta stream.  With a null sum head the
	// daemon emits the whole file as 32KB literal chunks and an end token;
	// match tokens only appear when the client supplied block checksums,
	// which this client never does.
	dr := protocol.NewDeltaReader(s.in)
	var data []byte
	for {
		lit, _, isEnd, err := dr.ReadToken()
		if err != nil {
			return nil, fmt.Errorf("read delta: %w", err)
		}
		if isEnd {
			break
		}
		data = append(data, lit...)
	}

	// Whole-file checksum; the receiver recomputes and compares.
	want := make([]byte, s2len)
	if _, err := io.ReadFull(s.in, want); err != nil {
		return nil, fmt.Errorf("read file checksum: %w", err)
	}
	got := protocol.FileChecksum(data, s.checksum, s.version, s.seed)
	if !equalBytes(got, want) {
		return nil, &fs.PathError{Op: "open", Path: e.Name, Err: errors.New("checksum mismatch")}
	}

	return &flistFile{info: flistInfo(baseName(e.Name), e), data: data}, nil
}

// finishSession completes the connection after the transfers: the NDX_DONE
// phase exchange, the stats, and the final goodbye (upstream generate_files
// phase loop, handle_stats, and read_final_goodbye).
func (s *Session) finishSession() error {
	maxPhase := 1
	if s.version >= 29 {
		maxPhase = 2
	}

	// The generator writes one NDX_DONE per phase boundary plus the one that
	// ends the daemon's selector loop (upstream generate_files: the
	// !inc_recurse marker, the post-phase marker, the early delay-updates
	// marker, and the proto-31 early delete marker).  The daemon echoes each
	// NDX_DONE it consumes in its loop and writes one trailing NDX_DONE after
	// the loop, so the loop's final read is that trailing echo.
	for i := 0; i <= maxPhase; i++ {
		if err := s.writePhaseNdx(); err != nil {
			return err
		}
		if err := s.readPhaseNdx(); err != nil {
			return err
		}
	}

	if err := s.readStats(); err != nil {
		return err
	}
	return s.finalGoodbye()
}

// writePhaseNdx writes one NDX_DONE to the daemon (compressed NDX from proto
// 30 on, a plain int32 below).
func (s *Session) writePhaseNdx() error {
	if s.version >= 30 {
		return s.ndx.WriteNdx(s.out, protocol.NDxDone)
	}
	return protocol.WriteInt32(s.out, protocol.NDxDone)
}

// readPhaseNdx reads one echoed NDX_DONE from the daemon.
func (s *Session) readPhaseNdx() error {
	if s.version >= 29 {
		sel, err := protocol.ReadSelector(s.in, s.recvNdx, s.version)
		if err != nil {
			return fmt.Errorf("read NDX_DONE: %w", err)
		}
		if sel.Ndx != protocol.NDxDone {
			return fmt.Errorf("expected NDX_DONE, got ndx %d", sel.Ndx)
		}
		return nil
	}
	v, err := protocol.ReadInt32(s.in)
	if err != nil {
		return fmt.Errorf("read NDX_DONE: %w", err)
	}
	if v != protocol.NDxDone {
		return fmt.Errorf("expected NDX_DONE, got %d", v)
	}
	return nil
}

// readStats consumes the daemon's transfer statistics (upstream handle_stats):
// three values always, five from proto 29 on; varlong30 from proto 30 on,
// longint before.
func (s *Session) readStats() error {
	n := 3
	if s.version >= 29 {
		n = 5
	}
	for i := 0; i < n; i++ {
		if s.version >= 30 {
			if _, err := protocol.ReadVarlong(s.in, 3); err != nil {
				return fmt.Errorf("read stat %d: %w", i, err)
			}
		} else {
			if _, err := protocol.ReadLongInt(s.in); err != nil {
				return fmt.Errorf("read stat %d: %w", i, err)
			}
		}
	}
	return nil
}

// finalGoodbye performs the final NDX_DONE exchange (upstream
// read_final_goodbye): a single int32 below proto 29, a single NDX_DONE from
// 29 on, and a double NDX_DONE with an echo from 31 on.
func (s *Session) finalGoodbye() error {
	if s.version < 29 {
		return protocol.WriteInt32(s.out, protocol.NDxDone)
	}
	// proto >= 29: the generator writes NDX_DONE; the daemon (proto >= 31)
	// echoes it back and then reads a second NDX_DONE.
	if err := s.writePhaseNdx(); err != nil {
		return err
	}
	if s.version >= 31 {
		if err := s.readPhaseNdx(); err != nil {
			return err
		}
		if err := s.writePhaseNdx(); err != nil {
			return err
		}
	}
	return nil
}

// --- file types -----------------------------------------------------------

// flistFileInfo implements fs.FileInfo for a file list entry.
type flistFileInfo struct {
	name    string
	mode    fs.FileMode
	size    int64
	modTime time.Time
}

var (
	_ fs.FileInfo   = (*flistFileInfo)(nil)
	_ fs.File       = (*flistFile)(nil)
	_ fs.File       = (*moduleDirFile)(nil)
	_ fs.File       = (*symlinkFile)(nil)
	_ fs.File       = (*rootDir)(nil)
	_ fs.DirEntry   = (*flistDirEntry)(nil)
	_ fs.ReadLinkFS = (*Session)(nil)
)

func (fi *flistFileInfo) Name() string       { return fi.name }
func (fi *flistFileInfo) Size() int64        { return fi.size }
func (fi *flistFileInfo) Mode() fs.FileMode  { return fi.mode }
func (fi *flistFileInfo) ModTime() time.Time { return fi.modTime }
func (fi *flistFileInfo) IsDir() bool        { return fi.mode.IsDir() }
func (fi *flistFileInfo) Sys() any           { return nil }

// flistFile is a regular file whose data was pulled over the wire.
type flistFile struct {
	info *flistFileInfo
	data []byte
	off  int64
}

func (f *flistFile) Read(p []byte) (int, error) {
	if f.off >= int64(len(f.data)) {
		return 0, io.EOF
	}
	n := copy(p, f.data[f.off:])
	f.off += int64(n)
	return n, nil
}

func (f *flistFile) Stat() (fs.FileInfo, error) { return f.info, nil }

func (f *flistFile) Close() error { return nil }

func (f *flistFile) ReadDir(int) ([]fs.DirEntry, error) {
	return nil, &fs.PathError{Op: "readdir", Path: f.info.name, Err: fs.ErrInvalid}
}

// moduleDirFile is a directory whose children come from the file list.
type moduleDirFile struct {
	info     *flistFileInfo
	children []*protocol.FlistEntry
	off      int
}

func newModuleDirFile(name string, self *protocol.FlistEntry, children []*protocol.FlistEntry) *moduleDirFile {
	return &moduleDirFile{info: flistInfo(baseName(name), self), children: children}
}

func (d *moduleDirFile) Stat() (fs.FileInfo, error) { return d.info, nil }

func (d *moduleDirFile) Read([]byte) (int, error) { return 0, fs.ErrInvalid }

func (d *moduleDirFile) Close() error { return nil }

func (d *moduleDirFile) ReadDir(n int) ([]fs.DirEntry, error) {
	if d.off >= len(d.children) {
		if n <= 0 {
			return nil, nil
		}
		return nil, io.EOF
	}
	if n <= 0 || n > len(d.children)-d.off {
		n = len(d.children) - d.off
	}
	out := make([]fs.DirEntry, n)
	for i := 0; i < n; i++ {
		out[i] = &flistDirEntry{e: d.children[d.off+i]}
	}
	d.off += n
	return out, nil
}

// flistDirEntry implements fs.DirEntry for a file list child.
type flistDirEntry struct {
	e *protocol.FlistEntry
}

func (de *flistDirEntry) Name() string      { return baseName(de.e.Name) }
func (de *flistDirEntry) IsDir() bool       { return wireModeIsDir(de.e.Mode) }
func (de *flistDirEntry) Type() fs.FileMode { return wireModeToFS(de.e.Mode).Type() }
func (de *flistDirEntry) Info() (fs.FileInfo, error) {
	return flistInfo(baseName(de.e.Name), de.e), nil
}

// symlinkFile is a symlink; the target is carried alongside the info.
type symlinkFile struct {
	info       *flistFileInfo
	linkTarget string
}

func (f *symlinkFile) Stat() (fs.FileInfo, error) { return f.info, nil }

func (f *symlinkFile) Read([]byte) (int, error) { return 0, fs.ErrInvalid }

func (f *symlinkFile) Close() error { return nil }

func (f *symlinkFile) ReadDir(int) ([]fs.DirEntry, error) {
	return nil, &fs.PathError{Op: "readdir", Path: f.info.name, Err: fs.ErrInvalid}
}

// --- root mode ------------------------------------------------------------

// openRoot handles opens in root mode, where modules are top-level
// directories and each operation runs on its own connection.  The root
// (".") lists the modules; a module path opens the module.
func (s *Session) openRoot(name string) (fs.File, error) {
	if name == "." || name == "" {
		modules, err := s.listModules()
		if err != nil {
			return nil, err
		}
		return &rootDir{modules: modules}, nil
	}

	name = strings.TrimPrefix(name, "/")
	parts := strings.SplitN(name, "/", 2)
	modName := parts[0]
	modulePath := ""
	if len(parts) > 1 {
		modulePath = parts[1]
	}

	// A bare module name opens the module's root directory.
	if modulePath == "" {
		return s.rootOpenModule(modName, ".")
	}
	return s.rootOpenModule(modName, modulePath)
}

// rootOpenModule opens modulePath within modName on a fresh connection to
// that module.
func (s *Session) rootOpenModule(modName, modulePath string) (fs.File, error) {
	rw, err := s.connectFunc(modName)
	if err != nil {
		return nil, fmt.Errorf("connect to module %q: %w", modName, err)
	}
	c := *s.client
	c.Module = modName
	sess, err := c.Connect(rw)
	if err != nil {
		return nil, fmt.Errorf("handshake with module %q: %w", modName, err)
	}
	if modulePath == "" {
		modulePath = "."
	}
	return sess.withEntry(modulePath, openFileFromContext)
}

// rootReadLink resolves a symlink target in root mode.
func (s *Session) rootReadLink(name string) (string, error) {
	name = strings.TrimPrefix(name, "/")
	parts := strings.SplitN(name, "/", 2)
	if len(parts) < 2 || parts[1] == "" {
		return "", &fs.PathError{Op: "readlink", Path: name, Err: errors.New("not a module path")}
	}
	rw, err := s.connectFunc(parts[0])
	if err != nil {
		return "", fmt.Errorf("connect to module %q: %w", parts[0], err)
	}
	c := *s.client
	c.Module = parts[0]
	sess, err := c.Connect(rw)
	if err != nil {
		return "", fmt.Errorf("handshake with module %q: %w", parts[0], err)
	}
	var target string
	_, err = sess.withEntry(parts[1], func(ctx *openContext) (fs.File, error) {
		if !wireModeIsSymlink(ctx.entry.Mode) {
			return nil, &fs.PathError{Op: "readlink", Path: name, Err: errors.New("not a symlink")}
		}
		target = ctx.entry.LinkTarget
		return nil, nil
	})
	if err != nil {
		return "", err
	}
	return target, nil
}

// listModules runs a #list request on a fresh connection and returns the
// modules with their comments.
func (s *Session) listModules() ([]moduleInfo, error) {
	rw, err := s.connectFunc("")
	if err != nil {
		return nil, fmt.Errorf("connect for #list: %w", err)
	}
	c := *s.client
	c.Module = ""
	return doListRequest(c, rw)
}

// rootDir lists the modules as top-level directory entries, each paired with
// a hidden ".<module>\t<comment>" symlink that carries the module's comment.
type rootDir struct {
	modules []moduleInfo
	off     int
}

type moduleInfo struct {
	name    string
	comment string
}

func (d *rootDir) Stat() (fs.FileInfo, error) {
	return &flistFileInfo{name: ".", mode: fs.ModeDir, size: 0}, nil
}

func (d *rootDir) Read([]byte) (int, error) { return 0, fs.ErrInvalid }

func (d *rootDir) Close() error { return nil }

func (d *rootDir) ReadDir(n int) ([]fs.DirEntry, error) {
	type entry struct {
		name      string
		isSymlink bool
		target    string
	}
	all := make([]entry, 0, len(d.modules)*2)
	for _, m := range d.modules {
		all = append(all,
			entry{name: m.name},
			entry{name: "." + m.name + "\t" + m.comment, isSymlink: true, target: m.name},
		)
	}
	sort.Slice(all, func(i, j int) bool { return all[i].name < all[j].name })

	if d.off >= len(all) {
		if n <= 0 {
			return nil, nil
		}
		return nil, io.EOF
	}
	if n <= 0 || n > len(all)-d.off {
		n = len(all) - d.off
	}
	out := make([]fs.DirEntry, n)
	for i := 0; i < n; i++ {
		e := all[d.off+i]
		mode := fs.ModeDir
		if e.isSymlink {
			mode = fs.ModeSymlink
		}
		out[i] = &rootDirEntry{name: e.name, mode: mode, target: e.target}
	}
	d.off += n
	return out, nil
}

// rootDirEntry is a module (or its comment symlink) in the root listing.
type rootDirEntry struct {
	name   string
	mode   fs.FileMode
	target string
}

func (e *rootDirEntry) Name() string      { return e.name }
func (e *rootDirEntry) IsDir() bool       { return e.mode.IsDir() }
func (e *rootDirEntry) Type() fs.FileMode { return e.mode.Type() }
func (e *rootDirEntry) Info() (fs.FileInfo, error) {
	return &flistFileInfo{name: e.name, mode: e.mode, size: int64(len(e.target))}, nil
}

// doListRequest runs a #list request over rw and returns the modules.  The
// server closes the connection after the listing, so rw is not reused.
func doListRequest(c Client, rw io.ReadWriter) ([]moduleInfo, error) {
	greet := c.Greeting
	greet.ApplyDefaults()
	if err := protocol.WriteGreeting(rw, greet); err != nil {
		return nil, fmt.Errorf("write greeting: %w", err)
	}
	serverGreet, err := protocol.ReadGreeting(rw)
	if err != nil {
		return nil, fmt.Errorf("read server greeting: %w", err)
	}
	if _, _, _, err := protocol.Negotiate(greet, *serverGreet); err != nil {
		return nil, fmt.Errorf("negotiate: %w", err)
	}
	if err := protocol.WriteModuleRequest(rw, "#list"); err != nil {
		return nil, fmt.Errorf("write #list: %w", err)
	}
	// The daemon answers the #list line by writing the listing directly
	// (no @RSYNCD: OK), then "@RSYNCD: EXIT" on proto >= 25 (upstream
	// send_listing); below 25 it may just drop the connection.  Each
	// non-special line is a module listing or an indistinguishable MOTD
	// line (upstream has no way to tell them apart either).
	var modules []moduleInfo
	for {
		line, err := readTextLine(rw)
		if err != nil {
			if err == io.EOF {
				break // pre-25 server dropped the connection after the list
			}
			return nil, fmt.Errorf("read module list: %w", err)
		}
		if strings.HasPrefix(line, "@RSYNCD: EXIT") {
			break
		}
		if strings.HasPrefix(line, "@ERROR") {
			return nil, fmt.Errorf("server error: %s", line)
		}
		if strings.HasPrefix(line, "@RSYNCD: AUTHREQD ") {
			challenge := strings.TrimSpace(strings.TrimPrefix(line, "@RSYNCD: AUTHREQD "))
			data, err := base64.StdEncoding.DecodeString(challenge)
			if err != nil {
				return nil, fmt.Errorf("decode auth challenge: %w", err)
			}
			if c.AuthUser == "" || c.AuthResponse == nil {
				return nil, errors.New("#list requires authentication but AuthUser/AuthResponse are not set")
			}
			digestBytes, err := c.AuthResponse("", data)
			if err != nil {
				return nil, fmt.Errorf("compute auth response: %w", err)
			}
			if err := protocol.WriteAuthResponse(rw, c.AuthUser, digestBytes); err != nil {
				return nil, fmt.Errorf("send auth response: %w", err)
			}
			continue
		}
		name, comment, _ := strings.Cut(line, "\t")
		name = strings.TrimSpace(name)
		modules = append(modules, moduleInfo{name: name, comment: comment})
	}
	if cl, ok := rw.(io.Closer); ok {
		_ = cl.Close()
	}
	return modules, nil
}

// readTextLine reads a newline-terminated line (the trailing newline is
// dropped).
func readTextLine(r io.Reader) (string, error) {
	var sb strings.Builder
	buf := make([]byte, 1)
	for {
		n, err := io.ReadFull(r, buf)
		if n > 0 {
			if buf[0] == '\n' {
				return sb.String(), nil
			}
			sb.WriteByte(buf[0])
		}
		if err != nil {
			return "", err
		}
	}
}

// --- helpers --------------------------------------------------------------

// findFlistEntry returns the index of the entry named name, or -1.
func findFlistEntry(entries []*protocol.FlistEntry, name string) int {
	for i, e := range entries {
		if e.Name == name {
			return i
		}
	}
	return -1
}

// sortFlistEntries orders the received file list the way the daemon does
// (upstream flist_sort_and_clean / f_name_cmp, reused from the server side) and
// returns each entry's position in that order -- the NDX the daemon assigns
// to it.  The wire order of the list the daemon sends is its walk order, so
// the position is not the wire index; it must be recomputed by sorting.
func sortFlistEntries(entries []*protocol.FlistEntry, version int) map[string]int32 {
	order := make([]*protocol.FlistEntry, len(entries))
	copy(order, entries)
	sort.SliceStable(order, func(i, j int) bool {
		return flistNameCmp(order[i].Name, wireModeIsDir(order[i].Mode), order[j].Name, wireModeIsDir(order[j].Mode), version) < 0
	})
	ndxByName := make(map[string]int32, len(order))
	for i, e := range order {
		ndxByName[e.Name] = int32(i)
	}
	return ndxByName
}

// filterFlistChildren returns the direct children of dirName, sorted by
// base name (fs.ReadDir requires ascending name order).
func filterFlistChildren(entries []*protocol.FlistEntry, dirName string) []*protocol.FlistEntry {
	var children []*protocol.FlistEntry
	if dirName == "." {
		for _, e := range entries {
			if e.Name == "." || strings.Contains(e.Name, "/") {
				continue
			}
			children = append(children, e)
		}
	} else {
		prefix := dirName + "/"
		for _, e := range entries {
			if !strings.HasPrefix(e.Name, prefix) {
				continue
			}
			rel := e.Name[len(prefix):]
			if strings.Contains(rel, "/") {
				continue
			}
			children = append(children, e)
		}
	}
	sort.Slice(children, func(i, j int) bool {
		return baseName(children[i].Name) < baseName(children[j].Name)
	})
	return children
}

// baseName returns the last path component.
func baseName(path string) string {
	if i := strings.LastIndex(path, "/"); i >= 0 {
		return path[i+1:]
	}
	return path
}

// flistInfo builds a flistFileInfo for an entry; a symlink's size is the
// target length, not the flist size field.
func flistInfo(name string, e *protocol.FlistEntry) *flistFileInfo {
	size := e.Size
	if wireModeIsSymlink(e.Mode) {
		size = int64(len(e.LinkTarget))
	}
	return &flistFileInfo{
		name:    name,
		mode:    wireModeToFS(e.Mode),
		size:    size,
		modTime: time.Unix(e.Mtime, int64(e.ModNsec)),
	}
}

// wireModeIsDir reports whether a raw wire mode is a directory.
func wireModeIsDir(mode uint32) bool { return mode&0o170000 == 0o040000 }

// wireModeIsSymlink reports whether a raw wire mode is a symlink.
func wireModeIsSymlink(mode uint32) bool { return mode&0o170000 == 0o120000 }

// wireModeToFS converts a raw wire mode to an fs.FileMode.
func wireModeToFS(mode uint32) fs.FileMode {
	var m fs.FileMode
	switch mode & 0o170000 {
	case 0o040000: // S_IFDIR
		m |= fs.ModeDir
	case 0o120000: // S_IFLNK
		m |= fs.ModeSymlink
	case 0o020000: // S_IFCHR
		m |= fs.ModeDevice | fs.ModeCharDevice
	case 0o060000: // S_IFBLK
		m |= fs.ModeDevice
	case 0o010000: // S_IFIFO
		m |= fs.ModeNamedPipe
	case 0o140000: // S_IFSOCK
		m |= fs.ModeSocket
		// 0o100000 (S_IFREG): no special bit
	}
	return m | fs.FileMode(mode&0o7777).Perm()
}

func equalBytes(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
