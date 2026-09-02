package rsyncfs

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/values-conflict/go-rsyncfs/protocol"
	"github.com/values-conflict/go-rsyncfs/protocol/mux"
)

// transferState carries the per-connection state of the post-handshake
// transfer phase.  It is the Go equivalent of the daemon-side globals the
// upstream sender code keeps in options: the negotiated algorithms, the
// I/O mode split, and the walked file list.
type transferState struct {
	rw   io.ReadWriter
	in   io.Reader // selector/sums channel: mux reader for proto >= 30, raw before
	out  io.Writer // flist/data channel: mux writer for proto >= 23, raw before
	mw   *mux.Writer
	ver  int
	seed int32

	// seedFix mirrors upstream proper_seed_order (CF_CHKSUM_SEED_FIX).
	seedFix bool
	// checksum is the negotiated strong-hash name ("md5" or "md4").
	checksum string
	// varint is upstream xfer_flags_as_varint (CF_VARINT_FLIST_FLAGS).
	varint bool
	// id0Names is upstream xmit_id0_names (CF_ID0_NAMES).
	id0Names bool
	// preserve carries the client's uid/gid preservation settings.
	preserve [2]bool

	mod *ServerModule

	// preserveLinks mirrors the client's -l (the default; -L would
	// disable it, which this server does not honor).
	preserveLinks   bool
	preserveDevices bool
	preserveHlinks  bool

	// entries is the walked file list in upstream flist sort order
	// (flist_sort_and_clean / f_name_cmp).  Index i is the NDX of
	// entries[i] on the wire.
	entries []fileEntry
}

// blockSum is one client-supplied block checksum pair from a sum struct.
// len is blength except for the final partial block, which uses remainder.
type blockSum struct {
	sum1 uint32
	sum2 []byte
	len  int32
}

// doServerSender runs the daemon-side transfer phase after the handshake:
// filter list, file list, selector loop with data transfer, stats, and the
// final goodbye.  It is fully sequential -- the client's generator and the
// daemon exchange messages in a fixed order, so no concurrent read/write is
// needed (upstream do_server_sender in main.c).
func (s *Server) doServerSender(rw io.ReadWriter, h *handshakeResult) error {
	st := &transferState{
		rw:              rw,
		in:              rw,
		out:             rw,
		ver:             h.ver,
		seed:            h.seed,
		seedFix:         h.seedFix,
		checksum:        h.checksum,
		varint:          h.varint,
		id0Names:        h.id0Names,
		preserve:        h.preserve,
		preserveLinks:   true,
		preserveDevices: true,
		preserveHlinks:  h.preserveHlinks,
		mod:             h.module,
	}
	// The daemon's output is multiplexed on every supported protocol
	// version (upstream io_start_multiplex_out in rsync_module for proto
	// < 23 and in start_server for proto >= 23; the client's --sender
	// argument makes the daemon the sender, so the proto < 23 branch
	// always applies).  For proto < 23 the mux was already started in
	// doHandshake before the seed, so reuse its writer; otherwise the
	// mux starts here.  The client's output only becomes framed from
	// proto 30 on (upstream io_start_multiplex_out in client_run), so
	// below that the input stays a plain byte stream.
	if h.outMw != nil {
		st.mw = h.outMw
	} else {
		st.mw = mux.NewWriter(rw)
	}
	st.out = st.mw
	if h.ver >= 30 {
		st.in = mux.NewReader(rw)
	}
	// Push pending mux output to the wire before each read.  Upstream's
	// iobuf does the same inside perform_io: whenever it blocks waiting
	// for input it first writes the pending output, so an echo (an
	// NDX_DONE reply, say) can never sit in the send buffer while the
	// daemon blocks on the very read that the client is waiting on that
	// echo to unblock.
	st.in = &flushBeforeRead{inner: st.in, flush: st.flush}

	if err := st.recvFilterList(); err != nil {
		return err
	}
	if err := st.walk(); err != nil {
		return err
	}
	// An empty file list ends the connection immediately (upstream
	// do_server_sender exits when flist->used == 0).
	if len(st.entries) == 0 {
		return st.flush()
	}
	if err := st.sendFileList(); err != nil {
		return err
	}
	if err := st.sendFiles(); err != nil {
		return err
	}
	if err := st.flush(); err != nil {
		return err
	}
	if err := st.sendStats(); err != nil {
		return err
	}
	if err := st.readFinalGoodbye(); err != nil {
		return err
	}
	return st.flush()
}

// flushBeforeRead wraps a reader and pushes pending mux output to the
// wire before each read that would otherwise block on the transport.
// It mirrors the upstream iobuf behavior of draining the output buffer
// while waiting for input (perform_io), which keeps the daemon and the
// client's generator from deadlocking on an unflushed echo.
type flushBeforeRead struct {
	inner io.Reader
	flush func() error
}

func (f *flushBeforeRead) Read(p []byte) (int, error) {
	if err := f.flush(); err != nil {
		return 0, err
	}
	return f.inner.Read(p)
}

// recvFilterList consumes the pattern list the generator sends before the
// file list: the legacy exclude list below proto 29 (upstream
// recv_exclude_list) or the full filter list from proto 29 on (upstream
// recv_filter_list).  Both are a run of int32-length-prefixed strings
// terminated by a zero length, so the same drain works for both; the rules
// themselves are not needed server-side.
func (st *transferState) recvFilterList() error {
	for {
		len32, err := protocol.ReadInt32(st.in)
		if err != nil {
			return fmt.Errorf("read filter list length: %w", err)
		}
		if len32 == 0 {
			return nil
		}
		if len32 < 0 {
			return fmt.Errorf("negative filter rule length %d", len32)
		}
		if _, err := io.CopyN(io.Discard, st.in, int64(len32)); err != nil {
			return fmt.Errorf("read filter rule: %w", err)
		}
	}
}

// fileEntry holds the stat information of one file list entry.
type fileEntry struct {
	name       string
	mode       fs.FileMode
	size       int64
	modTime    time.Time
	linkTarget string
	uid        uint32
	gid        uint32
	dev        int64
	ino        int64
	nlink      int64
}

// walk performs a pre-order walk of the module FS and fills st.entries in
// upstream flist sort order: the top directory "." first, then every
// level's non-directories in byte order, then its subdirectories (each
// immediately followed by its subtree), matching f_name_cmp (flist.c) for
// proto >= 29 and plain name order before.
func (st *transferState) walk() error {
	var raw []fileEntry
	err := fs.WalkDir(st.mod.FS, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		e := fileEntry{
			name:    path,
			mode:    info.Mode(),
			size:    info.Size(),
			modTime: info.ModTime(),
		}
		if stt, ok := info.Sys().(*syscall.Stat_t); ok {
			e.uid = stt.Uid
			e.gid = stt.Gid
			e.dev = int64(stt.Dev)
			e.ino = int64(stt.Ino)
			e.nlink = int64(stt.Nlink)
		}
		if e.modTime.IsZero() {
			// synthetic filesystems (fstest.MapFS) may report a zero
			// time; varlong would encode it as a large negative
			// number, so normalize to now
			e.modTime = time.Now()
		}
		if info.Mode().Type() == fs.ModeSymlink {
			if rl, ok := st.mod.FS.(interface {
				ReadLink(name string) (string, error)
			}); ok {
				if target, err := rl.ReadLink(path); err == nil {
					e.linkTarget = target
				}
			}
		}
		raw = append(raw, e)
		return nil
	})
	if err != nil {
		return fmt.Errorf("walk module fs: %w", err)
	}

	sortable := make([]sortableEntry, len(raw))
	for i, e := range raw {
		sortable[i] = sortableEntry{entry: e, isDir: e.mode.IsDir()}
	}
	sortFlist(sortable, st.ver)
	st.entries = make([]fileEntry, len(raw))
	for i, se := range sortable {
		st.entries[i] = se.entry
	}
	return nil
}

// sortableEntry is a fileEntry plus its directory flag, which the flist
// sort needs to separate path elements.
type sortableEntry struct {
	entry fileEntry
	isDir bool
}

// flistNameCmp compares two flist names the way f_name_cmp (flist.c)
// does.  Names are compared component by component.  A component that
// continues the path (a directory component) compares with an implied
// trailing slash, so a directory sorts immediately before its contents.
// From proto 29 on, final directory components carry the implied slash
// as well, a non-directory component sorts before a directory component
// at the same position regardless of name, and the top directory "." --
// which upstream compares as a nameless non-directory -- sorts before
// everything.  Below proto 29 there is no type priority and final
// components carry no implied slash, so "." sorts by its literal name.
func flistNameCmp(nameA string, dirA bool, nameB string, dirB bool, version int) int {
	compsA := strings.Split(nameA, "/")
	compsB := strings.Split(nameB, "/")

	for i := 0; i < len(compsA) || i < len(compsB); i++ {
		if i >= len(compsA) {
			return -1 // A is an ancestor directory of B
		}
		if i >= len(compsB) {
			return 1
		}
		a, b := compsA[i], compsB[i]

		if version < 29 {
			// only path-continuing components get an implied slash
			if i < len(compsA)-1 {
				a += "/"
			}
			if i < len(compsB)-1 {
				b += "/"
			}
			if c := strings.Compare(a, b); c != 0 {
				return c
			}
			continue
		}

		aDir := i < len(compsA)-1 || dirA
		bDir := i < len(compsB)-1 || dirB

		// the "." directory sorts before any other entry
		if a == "." && aDir {
			return -1
		}
		if b == "." && bDir {
			return 1
		}
		if aDir != bDir {
			// type priority: non-directory before directory
			if aDir {
				return 1
			}
			return -1
		}
		if aDir {
			// directory components (intermediate or final) compare with
			// their implied trailing slashes; an equal pair falls through
			// to the next component
			if c := strings.Compare(a+"/", b+"/"); c != 0 {
				return c
			}
			continue
		}
		return strings.Compare(a, b)
	}
	return 0
}

// sortFlist orders entries the way flist_sort_and_clean's fsort step
// does, using the f_name_cmp ordering.
func sortFlist(entries []sortableEntry, version int) {
	slices.SortStableFunc(entries, func(a, b sortableEntry) int {
		if c := flistNameCmp(a.entry.name, a.isDir, b.entry.name, b.isDir, version); c != 0 {
			return c
		}
		return 0
	})
}

// sendFileList writes the walked file list in rsync wire format: entries,
// end marker, id lists, and the proto < 30 io_error trailer (upstream
// send_file_list).
func (st *transferState) sendFileList() error {
	fw := protocol.NewFlistWriter(st.out, st.ver, st.varint)
	fw.SetPreserveUID(st.preserve[0])
	fw.SetPreserveGID(st.preserve[1])
	fw.SetPreserveDevices(st.preserveDevices)
	fw.SetPreserveLinks(st.preserveLinks)
	fw.SetPreserveHardlinks(st.preserveHlinks)
	fw.SetID0Names(st.id0Names)

	for _, e := range st.entries {
		we := &protocol.FlistEntry{
			Name:       e.name,
			Mode:       modeToWire(e.mode),
			Size:       e.size,
			Mtime:      e.modTime.Unix(),
			ModNsec:    int32(e.modTime.Nanosecond()),
			LinkTarget: e.linkTarget,
			UID:        int32(e.uid),
			GID:        int32(e.gid),
			Dev:        e.dev,
			Ino:        e.ino,
			Nlink:      e.nlink,
			TopDir:     e.name == ".",
		}
		if err := fw.WriteEntry(we); err != nil {
			return fmt.Errorf("write flist entry %q: %w", e.name, err)
		}
	}
	if err := fw.WriteEndMarker(); err != nil {
		return fmt.Errorf("write flist end marker: %w", err)
	}
	if err := fw.WriteIDLists(); err != nil {
		return fmt.Errorf("write id lists: %w", err)
	}
	if err := fw.WriteIOErrorTrailer(0); err != nil {
		return fmt.Errorf("write io_error trailer: %w", err)
	}
	return st.flush()
}

// sendFiles runs the selector loop until the generator is finished
// (upstream send_files): echo each selector, serve the sum-struct-driven
// delta transfer for ITEM_TRANSFER selectors, and echo NDX_DONE between
// phases.
func (st *transferState) sendFiles() error {
	inNdx := protocol.NewNdxState()
	outNdx := protocol.NewNdxState()

	phase := 0
	maxPhase := 1
	if st.ver >= 29 {
		maxPhase = 2
	}

	for {
		sel, err := protocol.ReadSelector(st.in, inNdx, st.ver)
		if err != nil {
			return fmt.Errorf("read selector: %w", err)
		}

		if sel.Ndx == protocol.NDxDone {
			phase++
			if phase > maxPhase {
				break
			}
			if err := st.echoNdx(outNdx, sel.Ndx); err != nil {
				return err
			}
			continue
		}
		if sel.Ndx == protocol.NDXFlistEOF {
			// incremental-recursive sub-lists are not supported; the
			// client only uses them when it negotiated CF_INC_RECURSE,
			// which this server never advertises
			return fmt.Errorf("unexpected NDX_FLIST_EOF (unsupported inc-recursive mode)")
		}
		if sel.Ndx == ndxDelStats {
			if err := st.echoDelStats(inNdx, outNdx); err != nil {
				return err
			}
			continue
		}
		if sel.Ndx < 0 {
			return fmt.Errorf("invalid file index %d", sel.Ndx)
		}
		// proto-29 keep-alive: a selector with ndx == len(flist) and
		// iflags == ITEM_IS_NEW is discarded without an echo
		// (upstream read_ndx_and_attrs read_loop)
		if st.ver < 30 && sel.Ndx == int32(len(st.entries)) && sel.Iflags == protocol.ItemIsNew {
			continue
		}
		if int(sel.Ndx) >= len(st.entries) {
			return fmt.Errorf("file-list index %d out of range (0 - %d)", sel.Ndx, len(st.entries)-1)
		}

		entry := st.entries[sel.Ndx]

		// non-transfer selectors (metadata-only, e.g. ITEM_IS_NEW
		// without ITEM_TRANSFER) are echoed and dropped
		if sel.Iflags&protocol.ItemTransfer == 0 {
			if err := st.echoSelector(outNdx, sel); err != nil {
				return err
			}
			continue
		}
		if phase == 2 {
			return fmt.Errorf("got transfer request in phase 2")
		}

		if err := st.sendFile(entry, sel, outNdx); err != nil {
			return err
		}
	}

	// upstream send_files ends by writing one more NDX_DONE after the
	// loop (it arrives as the receiver's phase-3 marker)
	return st.echoNdx(outNdx, protocol.NDxDone)
}

// ndxDelStats is the special NDX value for delete statistics (proto 31+
// delete passes).
const ndxDelStats int32 = -3

// echoDelStats reads the generator's delete statistics and echoes them
// back with the daemon's own (all-zero) counters, matching
// read_ndx_and_attrs' NDX_DEL_STATS handling.
func (st *transferState) echoDelStats(inNdx *protocol.NdxState, outNdx *protocol.NdxState) error {
	for i := 0; i < 5; i++ {
		if _, err := protocol.ReadVarint(st.in); err != nil {
			return fmt.Errorf("read del_stats varint %d: %w", i, err)
		}
	}
	if err := st.echoNdx(outNdx, ndxDelStats); err != nil {
		return err
	}
	for i := 0; i < 5; i++ {
		if err := protocol.WriteVarint(st.out, 0); err != nil {
			return fmt.Errorf("write del_stats varint %d: %w", i, err)
		}
	}
	return nil
}

// echoNdx writes a bare NDX value (NDX_DONE echoes, NDX_DEL_STATS) with the
// outgoing NDX state.
func (st *transferState) echoNdx(outNdx *protocol.NdxState, ndx int32) error {
	if st.ver >= 30 {
		return outNdx.WriteNdx(st.out, ndx)
	}
	return protocol.WriteInt32(st.out, ndx)
}

// echoSelector re-encodes a selector to the client (upstream
// write_ndx_and_attrs, minus the xattr request which this server never
// sends).
func (st *transferState) echoSelector(outNdx *protocol.NdxState, sel *protocol.Selector) error {
	return protocol.WriteSelector(st.out, outNdx, st.ver, sel)
}

// sendFile serves one file transfer (upstream send_files' transfer
// branch + receive_sums + match_sums).  The wire order matches upstream:
// read the generator's sum struct first, then echo the selector and the
// sum head back, and finally stream the delta (match tokens or literal
// data) and the whole-file checksum.  The delta direction is
// daemon-to-client: the generator's block checksums describe the client's
// local file, and the daemon streams either match tokens (reuse a local
// block) or literal data.
func (st *transferState) sendFile(entry fileEntry, sel *protocol.Selector, outNdx *protocol.NdxState) error {
	// read the generator's sum struct (upstream receive_sums)
	sh, sums, err := st.receiveSums()
	if err != nil {
		return err
	}

	// Below proto 29 the generator sends a (null) sum struct for every
	// item, directories included; there is no data to transfer for a
	// non-regular entry, so serve an empty body.  From proto 30 on a
	// transfer request for a non-regular file is a protocol error
	// (upstream send_files).
	var data []byte
	if entry.mode.IsRegular() {
		f, err := st.mod.FS.Open(entry.name)
		if err != nil {
			return fmt.Errorf("open %q: %w", entry.name, err)
		}
		defer f.Close()
		data, err = io.ReadAll(f)
		if err != nil {
			return fmt.Errorf("read %q: %w", entry.name, err)
		}
	} else if st.ver >= 30 {
		return fmt.Errorf("transfer request for non-regular file %q", entry.name)
	}

	// echo the selector (upstream write_ndx_and_attrs), then the sum
	// head back to the receiver (upstream write_sum_head)
	if err := st.echoSelector(outNdx, sel); err != nil {
		return err
	}
	if err := protocol.WriteSumHead(st.out, sh, st.ver); err != nil {
		return fmt.Errorf("write sum head: %w", err)
	}

	if err := st.matchSums(data, sh, sums); err != nil {
		return err
	}
	return st.flush()
}

// receiveSums reads the generator's sum head and, when count > 0, the
// per-block checksums (upstream receive_sums).  The head's field values are
// already validated by protocol.ReadSumHead's read_sum_head guards.
func (st *transferState) receiveSums() (protocol.SumHead, []blockSum, error) {
	sh, err := protocol.ReadSumHead(st.in, st.ver)
	if err != nil {
		return protocol.SumHead{}, nil, fmt.Errorf("read sum head: %w", err)
	}
	if sh.Count <= 0 {
		return sh, nil, nil
	}
	s2len := sh.S2Length
	if s2len <= 0 {
		// proto < 27 carries no s2length; the digest length is the
		// negotiated algorithm's (md4/md5 are both 16)
		s2len = 16
	}
	sums := make([]blockSum, sh.Count)
	for i := range sums {
		s1, err := protocol.ReadInt32(st.in)
		if err != nil {
			return protocol.SumHead{}, nil, fmt.Errorf("read block sum1 %d: %w", i, err)
		}
		s2 := make([]byte, s2len)
		if _, err := io.ReadFull(st.in, s2); err != nil {
			return protocol.SumHead{}, nil, fmt.Errorf("read block sum2 %d: %w", i, err)
		}
		sums[i] = blockSum{
			sum1: uint32(s1),
			sum2: s2,
			len:  sh.BLength,
		}
	}
	if sh.Remainder != 0 {
		sums[len(sums)-1].len = sh.Remainder
	}
	return sh, sums, nil
}

// deltaChunkSize is the max literal data per wire token (upstream
// CHUNK_SIZE, 32KB).
const deltaChunkSize = 32 * 1024

// matchSums streams the delta for data (upstream match_sums with
// do_compression == CPRES_NONE): when the client supplied block
// checksums, hash_search emits match tokens and literal fills; otherwise
// the whole file goes out as 32KB literal chunks.  The stream ends with
// the int32(0) token and the whole-file checksum.
func (st *transferState) matchSums(data []byte, sh protocol.SumHead, sums []blockSum) error {
	dw := protocol.NewDeltaWriter(st.out)

	if len(data) > 0 && sh.Count > 0 {
		if err := st.hashSearch(dw, data, sh.BLength, sums); err != nil {
			return err
		}
	} else {
		// whole-file literal, 32KB chunks (upstream match_sums else
		// branch: matched(...,-2) per chunk, then matched(len, -1))
		if err := dw.WriteLiteral(data); err != nil {
			return fmt.Errorf("send literal data: %w", err)
		}
	}
	if err := dw.WriteEnd(); err != nil {
		return fmt.Errorf("send end token: %w", err)
	}

	// whole-file checksum (upstream sum_end + write_buf file_sum).  The
	// streaming sum is unseeded from proto 30 on; below that the legacy
	// MD4 context is seeded before the data, so the digest is
	// MD4(seed || content).
	csum := protocol.FileChecksum(data, st.checksum, st.ver, st.seed)
	_, err := st.out.Write(csum)
	return err
}

// hashSearch walks data with the rolling checksum, looking each block up
// in the client's checksum table, and emits match tokens or literal data
// (upstream hash_search with do_compression == CPRES_NONE and
// updating_basis_file false).
func (st *transferState) hashSearch(dw *protocol.DeltaWriter, data []byte, blen int32, sums []blockSum) error {
	// build the hash table: sum1 -> chain of block indices (upstream
	// build_hash_table with the traditional 64K table)
	const tableSize = 1 << 16
	table := make([]int32, tableSize)
	for i := range table {
		table[i] = -1
	}
	chain := make([]int32, len(sums))
	for i, s := range sums {
		// SUM2HASH2: fold the two 16-bit halves together
		entry := int((s.sum1&0xFFFF + s.sum1>>16) & 0xFFFF)
		chain[i] = table[entry]
		table[entry] = int32(i)
	}

	blenI := int(blen)
	k := minInt(len(data), blenI)
	s1, s2 := rollingChecksum(data[:k])

	offset := 0
	end := len(data) + 1 - int(sums[len(sums)-1].len)
	lastMatch := 0

	for {
		matchedIdx := -1
		for i := int(table[(int(s1)+int(s2))&(tableSize-1)]); i >= 0; i = int(chain[i]) {
			s := sums[i]
			if s.sum1 != s1|s2 {
				continue
			}
			l := minInt(blenI, len(data)-offset)
			if int(s.len) != l {
				continue
			}
			got := protocol.Checksum2(data[offset:offset+l], st.checksum, len(s.sum2), st.seed, st.seedFix)
			if !bytesEqual(got, s.sum2) {
				continue
			}
			matchedIdx = i
			break
		}

		if matchedIdx >= 0 {
			// literal fill up to the match, then the match token
			if err := st.sendLiteralFill(dw, data, lastMatch, offset); err != nil {
				return err
			}
			if err := dw.WriteMatch(int32(matchedIdx)); err != nil {
				return fmt.Errorf("send match token: %w", err)
			}
			// upstream recomputes the checksum at offset+L-1 and then
			// rolls one byte, so the next window starts at the match end
			lastMatch = offset + int(sums[matchedIdx].len)
			offset = lastMatch - 1
			k = minInt(blenI, len(data)-offset)
			if k < 0 {
				k = 0
			}
			var s1f, s2f uint32
			s1f, s2f = rollingChecksum(data[offset : offset+k])
			b0 := uint32(data[offset])
			s1f -= b0
			s2f -= uint32(k) * b0
			if offset+k < len(data) {
				bk := uint32(data[offset+k])
				s1f += bk
				s2f += s1f
			} else {
				k--
			}
			s1, s2 = s1f, s2f
			offset++
			if offset >= end {
				break
			}
			continue
		} else {
			// no match: roll the window one byte
			b0 := data[offset]
			s1 -= uint32(b0)
			s2 -= uint32(k) * uint32(b0)
			if offset+k < len(data) {
				bk := data[offset+k]
				s1 += uint32(bk)
				s2 += s1
			} else {
				k--
			}
			// early literal flush (upstream "match early" optimization)
			if offset-lastMatch >= blenI+deltaChunkSize && end-offset > deltaChunkSize {
				if err := st.sendLiteralFill(dw, data, lastMatch, offset-blenI); err != nil {
					return err
				}
				lastMatch = offset - blenI
			}
		}
		offset++
		if offset >= end {
			break
		}
	}

	// trailing literal (upstream matched(f, s, buf, len, -1); the end
	// token itself is written by matchSums)
	return st.sendLiteralFill(dw, data, lastMatch, len(data))
}

// sendLiteralFill writes data[a:b] as literal tokens (32KB chunks, each
// length-prefixed) without a trailing token, matching upstream
// simple_send_token's data-only (-2) path.
func (st *transferState) sendLiteralFill(dw *protocol.DeltaWriter, data []byte, a, b int) error {
	if b <= a {
		return nil
	}
	return dw.WriteLiteral(data[a:b])
}

// rollingChecksum computes get_checksum1 (checksum.c) over data: the
// Adler-32-inspired pair returned as (s1, s2), each truncated to 16 bits.
func rollingChecksum(data []byte) (uint32, uint32) {
	var s1, s2 uint32
	i := 0
	for i+4 <= len(data) {
		s2 += 4*s1 + 4*uint32(data[i]) + 3*uint32(data[i+1]) + 2*uint32(data[i+2]) + uint32(data[i+3])
		s1 += uint32(data[i]) + uint32(data[i+1]) + uint32(data[i+2]) + uint32(data[i+3])
		i += 4
	}
	for ; i < len(data); i++ {
		s1 += uint32(data[i])
		s2 += s1
	}
	return s1 & 0xffff, s2 & 0xffff
}

// sendStats writes the daemon's transfer statistics (upstream handle_stats
// for am_daemon && am_sender): three values always, five for proto >= 29.
// varlong30 from proto 30 on, longint before.
func (st *transferState) sendStats() error {
	totalSize := int64(0)
	for _, e := range st.entries {
		if e.mode.IsRegular() || e.mode.Type() == fs.ModeSymlink {
			totalSize += e.size
		}
	}
	vals := []int64{0, 0, totalSize}
	if st.ver >= 29 {
		vals = append(vals, 0, 0) // flist_buildtime, flist_xfertime
	}
	for _, v := range vals {
		if st.ver >= 30 {
			if err := protocol.WriteVarlong(st.out, v, 3); err != nil {
				return fmt.Errorf("write stat: %w", err)
			}
		} else {
			if err := protocol.WriteLongInt(st.out, v); err != nil {
				return fmt.Errorf("write stat: %w", err)
			}
		}
	}
	return st.flush()
}

// readFinalGoodbye consumes the generator's final NDX_DONE exchange
// (upstream read_final_goodbye): a plain int for proto < 29, a compressed
// NDX for proto 29+, and a double NDX_DONE with an echo for proto 31+.
func (st *transferState) readFinalGoodbye() error {
	inNdx := protocol.NewNdxState()
	outNdx := protocol.NewNdxState()

	var first int32
	if st.ver < 29 {
		first, _ = st.readNdx(inNdx)
	} else {
		sel, err := protocol.ReadSelector(st.in, inNdx, st.ver)
		if err != nil {
			return fmt.Errorf("read final goodbye: %w", err)
		}
		first = sel.Ndx
	}
	if first != protocol.NDxDone {
		return fmt.Errorf("invalid packet at end of run (%d)", first)
	}
	if st.ver >= 31 {
		if err := st.echoNdx(outNdx, protocol.NDxDone); err != nil {
			return err
		}
		sel, err := protocol.ReadSelector(st.in, inNdx, st.ver)
		if err != nil {
			// the generator's final NDX_DONE arrives as an EOF on some
			// connection teardown paths; upstream's kluge_around_eof
			// tolerates it, so treat a clean EOF as the second goodbye
			if errors.Is(err, io.EOF) {
				return nil
			}
			return fmt.Errorf("read final goodbye (second): %w", err)
		}
		if sel.Ndx != protocol.NDxDone {
			return fmt.Errorf("invalid packet at end of run (%d)", sel.Ndx)
		}
	}
	return nil
}

// readNdx reads a bare NDX value (int32 for proto < 30) with the given
// state.
func (st *transferState) readNdx(inNdx *protocol.NdxState) (int32, error) {
	if st.ver >= 30 {
		return inNdx.ReadNdx(st.in)
	}
	return protocol.ReadInt32(st.in)
}

// flush pushes pending mux output to the wire.
func (st *transferState) flush() error {
	if st.mw != nil {
		return st.mw.Flush()
	}
	return nil
}

// modeToWire converts fs.FileMode to the raw wire mode (S_IFDIR | 0755,
// etc).
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

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func bytesEqual(a, b []byte) bool {
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
