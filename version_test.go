package rsyncfs

import (
	"bytes"
	"io"
	"strconv"
	"testing"
	"testing/fstest"
	"time"

	"github.com/values-conflict/go-rsyncfs/protocol"
)

// pinnedVersionClient returns a Client that advertises exactly version in
// its greeting (SubProtocol 0, the full supported digest list), so the
// negotiated version is the minimum of the client's and the server's
// advertised versions.
func pinnedVersionClient(version int) Client {
	return Client{
		Module:   "testmod",
		Greeting: protocol.Greeting{Version: version, SubProtocol: 0, Digests: protocol.SupportedDigests()},
	}
}

// pinnedVersionServer returns a Server whose greeting advertises exactly
// version, serving the testmod module from [testModuleFS].
func pinnedVersionServer(t *testing.T, version int) *Server {
	t.Helper()
	s, err := NewServer(&ServerModule{Name: "testmod", FS: testModuleFS()})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	s.Greeting = protocol.Greeting{Version: version, SubProtocol: 0, Digests: protocol.SupportedDigests()}
	return s
}

// TestVersionMatrix_Negotiated connects client@vN to server@vM for every
// pair in [MinProtocolVersion..CurrentProtocolVersion] (13x13 = 169
// connections) and verifies the negotiated version is min(N, M), with the
// version-gated handshake state (auth digest, strong checksum, varint
// file-list flags, mux writer presence) that follows from it.
func TestVersionMatrix_Negotiated(t *testing.T) {
	servers := make([]int, 0, protocol.CurrentProtocolVersion-protocol.MinProtocolVersion+1)
	serverFor := make(map[int]*Server)
	for m := protocol.MinProtocolVersion; m <= protocol.CurrentProtocolVersion; m++ {
		serverFor[m] = pinnedVersionServer(t, m)
		servers = append(servers, m)
	}

	doneChs := make(chan error, 1024)
	total := 0
	for _, n := range servers {
		for _, m := range servers {
			total++
			t.Run(strconv.Itoa(n)+"-x-"+strconv.Itoa(m), func(t *testing.T) {
				serverEnd, clientEnd := BufPipe()
				go func() {
					defer serverEnd.Close()
					doneChs <- serverFor[m].HandleConnection(serverEnd)
				}()
				defer clientEnd.Close()

				sess, err := pinnedVersionClient(n).Connect(clientEnd)
				if err != nil {
					t.Fatalf("Connect: %v", err)
				}

				want := n
				if m < n {
					want = m
				}
				if sess.version != want {
					t.Errorf("version = %d, want %d (min of %d and %d)", sess.version, want, n, m)
				}
				// both sides advertise the same digest list in the same
				// order, so the client's first entry always wins
				if sess.digest != "md5" {
					t.Errorf("digest = %q, want md5", sess.digest)
				}
				wantChecksum := "md4"
				if want >= 30 {
					wantChecksum = "md5"
				}
				if sess.checksum != wantChecksum {
					t.Errorf("checksum = %q, want %q", sess.checksum, wantChecksum)
				}
				if sess.varintFlist != (want >= 30) {
					t.Errorf("varintFlist = %v, want %v", sess.varintFlist, want >= 30)
				}
				// the client's output is raw below proto 30, muxed from
				// there on (the daemon's output is muxed on every version)
				if (sess.mw != nil) != (want >= 30) {
					t.Errorf("mux writer present = %v, want %v", sess.mw != nil, want >= 30)
				}
				if sess.mr == nil {
					t.Error("mux reader = nil, want one")
				}
				if sess.seed == 0 {
					t.Error("seed = 0, want non-zero")
				}
			})
		}
	}

	// drain every server-side goroutine: closing the client end unblocks
	// the selector loop, so all of them must exit
	for i := 0; i < total; i++ {
		select {
		case serr := <-doneChs:
			t.Logf("server connection %d returned: %v", i+1, serr)
		case <-time.After(10 * time.Second):
			t.Fatalf("server connection %d/%d did not exit", i+1, total)
		}
	}
}

// TestVersionCompatFlags pins both sides to the same version across the
// full supported range and checks the negotiated compat flag state.  Below
// proto 30 the compat flags exchange does not exist at all (the client
// sends no -e argument and the daemon writes no flags); from 30 on the
// client's client_info (.fxCv) intersects the server's capabilities to
// exactly f, x, C, and v.
func TestVersionCompatFlags(t *testing.T) {
	const wantFlags = protocol.CompatSafeFlist |
		protocol.CompatAvoidXattrOptim |
		protocol.CompatChksumSeedFix |
		protocol.CompatVarintFlistFlags

	doneChs := make(chan error, 1024)
	total := 0
	for v := protocol.MinProtocolVersion; v <= protocol.CurrentProtocolVersion; v++ {
		total++
		t.Run(strconv.Itoa(v), func(t *testing.T) {
			serverEnd, clientEnd := BufPipe()
			go func() {
				defer serverEnd.Close()
				doneChs <- pinnedVersionServer(t, v).HandleConnection(serverEnd)
			}()
			defer clientEnd.Close()

			sess, err := pinnedVersionClient(v).Connect(clientEnd)
			if err != nil {
				t.Fatalf("Connect: %v", err)
			}

			if v < 30 {
				if sess.compatFlags != 0 {
					t.Errorf("compatFlags = 0x%x, want 0 (no exchange below proto 30)", sess.compatFlags)
				}
				if sess.varintFlist || sess.seedFix || sess.id0Names {
					t.Errorf("varintFlist=%v seedFix=%v id0Names=%v, want all false below proto 30",
						sess.varintFlist, sess.seedFix, sess.id0Names)
				}
			} else {
				if sess.compatFlags != wantFlags {
					t.Errorf("compatFlags = 0x%x, want 0x%x (client_info .fxCv against server capabilities)",
						sess.compatFlags, wantFlags)
				}
				if !sess.varintFlist {
					t.Error("varintFlist = false, want true (v negotiated)")
				}
				if !sess.seedFix {
					t.Error("seedFix = false, want true (C negotiated)")
				}
				if sess.id0Names {
					t.Error("id0Names = true, want false (u not in client_info)")
				}
			}
		})
	}
	for i := 0; i < total; i++ {
		select {
		case serr := <-doneChs:
			t.Logf("server connection %d returned: %v", i+1, serr)
		case <-time.After(10 * time.Second):
			t.Fatalf("server connection %d/%d did not exit", i+1, total)
		}
	}
}

// TestVersionSelectorWire pins the selector encoding at each of its gates
// (iflags from 29, compressed NDX from 30) and round-trips each form
// through ReadSelector:
//
//	ndx 0, ITEM_TRANSFER|ITEM_MISSING_DATA (0x18000):
//	  proto 20-28: 00 00 00 00                 (int32 ndx, no iflags field)
//	  proto 29:    00 00 00 00 00 80           (int32 ndx + uint16 iflags,
//	                                            the field truncates to 0x8000)
//	  proto 30+:   01 00 80                    (compressed ndx: fresh
//	                                            NdxState, delta 1 = 1 byte)
//
//	NDX_DONE:
//	  proto 20-29: ff ff ff ff
//	  proto 30+:   00
func TestVersionSelectorWire(t *testing.T) {
	const wantIflags = protocol.ItemTransfer | protocol.ItemMissingData
	for _, tc := range []struct {
		version  int
		wantNdx  []byte
		wantDone []byte
	}{
		{20, []byte{0, 0, 0, 0}, []byte{0xff, 0xff, 0xff, 0xff}},
		{28, []byte{0, 0, 0, 0}, []byte{0xff, 0xff, 0xff, 0xff}},
		{29, []byte{0, 0, 0, 0, 0x00, 0x80}, []byte{0xff, 0xff, 0xff, 0xff}},
		{30, []byte{0x01, 0x00, 0x80}, []byte{0x00}},
		{32, []byte{0x01, 0x00, 0x80}, []byte{0x00}},
	} {
		t.Run(strconv.Itoa(tc.version), func(t *testing.T) {
			var buf bytes.Buffer
			if err := protocol.WriteSelector(&buf, protocol.NewNdxState(), tc.version,
				&protocol.Selector{Ndx: 0, Iflags: wantIflags}); err != nil {
				t.Fatalf("WriteSelector: %v", err)
			}
			if !bytes.Equal(buf.Bytes(), tc.wantNdx) {
				t.Errorf("ndx 0 bytes = % x, want % x", buf.Bytes(), tc.wantNdx)
			}

			// round-trip
			got, err := protocol.ReadSelector(bytes.NewReader(buf.Bytes()), protocol.NewNdxState(), tc.version)
			if err != nil {
				t.Fatalf("ReadSelector: %v", err)
			}
			if got.Ndx != 0 {
				t.Errorf("round-trip ndx = %d, want 0", got.Ndx)
			}
			// the iflags field is a 16-bit shortint, so the round-tripped
			// value is the truncation of what was sent (ITEM_MISSING_DATA,
			// bit 16, does not survive -- the same read_shortint
			// truncation upstream performs); below proto 29 the field is
			// absent and the reader fills in the same default
			var iflags32 int32 = wantIflags
			wantRoundTrip := int(uint16(iflags32))
			if tc.version < 29 {
				wantRoundTrip = wantIflags
			}
			if got.Iflags != wantRoundTrip {
				t.Errorf("round-trip iflags = 0x%x, want 0x%x", got.Iflags, wantRoundTrip)
			}

			buf.Reset()
			if err := protocol.WriteSelector(&buf, protocol.NewNdxState(), tc.version,
				&protocol.Selector{Ndx: protocol.NDxDone}); err != nil {
				t.Fatalf("WriteSelector(NDX_DONE): %v", err)
			}
			if !bytes.Equal(buf.Bytes(), tc.wantDone) {
				t.Errorf("NDX_DONE bytes = % x, want % x", buf.Bytes(), tc.wantDone)
			}
		})
	}
}

// TestVersionFlistEntryWire pins the wire bytes of one regular file entry
// at each of the gates its layout crosses (the xflags width at 28, the
// size and mtime encodings at 30, the mod_nsec field at 31).  The expected
// byte sequences below are the upstream wire formats
// pinned per version group (xflags, name, size, mtime, mod_nsec, mode,
// uid, gid in order; dev/ino for pre-28 regular files at the end):
//
//	proto 20-27: 01 05 61 2e 74 78 74 | 05 00 00 00 | 00 f1 53 65 |
//	             a4 81 00 00 | 0c 00 00 00 | 22 00 00 00 |
//	             00 00 00 00 00 00 00 00
//	    (byte xflags, longint size, uint32 mtime/uid/gid, unconditional
//	     dev/ino longints)
//	proto 28-29: 01 05 61 2e 74 78 74 | 05 00 00 00 | 00 f1 53 65 |
//	             a4 81 00 00 | 0c 00 00 00 | 22 00 00 00
//	    (no hlink flag for a single link, no dev/ino)
//	proto 30+:   01 05 61 2e 74 78 74 | 00 05 00 | 65 00 f1 53 |
//	             a4 81 00 00 | 0c | 22
//	    (varlong30 size, varlong4 mtime, varint uid/gid)
func TestVersionFlistEntryWire(t *testing.T) {
	entry := &protocol.FlistEntry{
		Name:    "a.txt",
		Mode:    0o100644,
		Size:    5,
		Mtime:   1700000000,
		UID:     12,
		GID:     34,
		ModNsec: 123456789,
	}

	for _, tc := range []struct {
		versions []int
		want     []byte
	}{
		{
			[]int{20, 27},
			[]byte{
				0x01,
				0x05, 'a', '.', 't', 'x', 't',
				0x05, 0x00, 0x00, 0x00,
				0x00, 0xf1, 0x53, 0x65,
				0xa4, 0x81, 0x00, 0x00,
				0x0c, 0x00, 0x00, 0x00,
				0x22, 0x00, 0x00, 0x00,
				0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
			},
		},
		{
			[]int{28, 29},
			[]byte{
				0x01,
				0x05, 'a', '.', 't', 'x', 't',
				0x05, 0x00, 0x00, 0x00,
				0x00, 0xf1, 0x53, 0x65,
				0xa4, 0x81, 0x00, 0x00,
				0x0c, 0x00, 0x00, 0x00,
				0x22, 0x00, 0x00, 0x00,
			},
		},
		{
			[]int{30},
			[]byte{
				0x01,
				0x05, 'a', '.', 't', 'x', 't',
				0x00, 0x05, 0x00,
				0x65, 0x00, 0xf1, 0x53,
				0xa4, 0x81, 0x00, 0x00,
				0x0c,
				0x22,
			},
		},
		{
			// proto 31+ sets XMIT_MOD_NSEC (bit 13), promoting the
			// xflags to a 2-byte shortint (low byte gaining
			// XMIT_EXTENDED_FLAGS), with the nsec varint between
			// mtime and mode
			[]int{31, 32},
			[]byte{
				0x04, 0x20,
				0x05, 'a', '.', 't', 'x', 't',
				0x00, 0x05, 0x00,
				0x65, 0x00, 0xf1, 0x53,
				0xe7, 0x15, 0xcd, 0x5b,
				0xa4, 0x81, 0x00, 0x00,
				0x0c,
				0x22,
			},
		},
	} {
		for _, v := range tc.versions {
			t.Run(strconv.Itoa(v), func(t *testing.T) {
				var buf bytes.Buffer
				w := protocol.NewFlistWriter(&buf, v, false)
				if err := w.WriteEntry(entry); err != nil {
					t.Fatalf("WriteEntry: %v", err)
				}
				if !bytes.Equal(buf.Bytes(), tc.want) {
					t.Errorf("entry bytes = % x, want % x", buf.Bytes(), tc.want)
				}
			})
		}
	}

	// the 28-29 hard-link gate: Nlink > 1 sets XMIT_HLINKED (bit 9, high
	// byte 0x02, gaining XMIT_EXTENDED_FLAGS) only in proto 28-29; below
	// 28 regular files carry the dev/ino longints unconditionally with no
	// flag, and from 30 on the encoding moves to the back-reference form
	// (no flag on an entry without HlinkNdx)
	hlinked := *entry
	hlinked.Nlink = 2
	hlinked.Dev = 7
	hlinked.Ino = 42
	for _, tc := range []struct {
		version int
		want    []byte
	}{
		{
			27,
			[]byte{
				0x01,
				0x05, 'a', '.', 't', 'x', 't',
				0x05, 0x00, 0x00, 0x00,
				0x00, 0xf1, 0x53, 0x65,
				0xa4, 0x81, 0x00, 0x00,
				0x0c, 0x00, 0x00, 0x00,
				0x22, 0x00, 0x00, 0x00,
				0x07, 0x00, 0x00, 0x00,
				0x2a, 0x00, 0x00, 0x00,
			},
		},
		{
			28,
			[]byte{
				0x04, 0x02,
				0x05, 'a', '.', 't', 'x', 't',
				0x05, 0x00, 0x00, 0x00,
				0x00, 0xf1, 0x53, 0x65,
				0xa4, 0x81, 0x00, 0x00,
				0x0c, 0x00, 0x00, 0x00,
				0x22, 0x00, 0x00, 0x00,
				0x07, 0x00, 0x00, 0x00,
				0x2a, 0x00, 0x00, 0x00,
			},
		},
		{
			30,
			[]byte{
				0x01,
				0x05, 'a', '.', 't', 'x', 't',
				0x00, 0x05, 0x00,
				0x65, 0x00, 0xf1, 0x53,
				0xa4, 0x81, 0x00, 0x00,
				0x0c,
				0x22,
			},
		},
	} {
		t.Run("hlinked-"+strconv.Itoa(tc.version), func(t *testing.T) {
			var buf bytes.Buffer
			w := protocol.NewFlistWriter(&buf, tc.version, false)
			if err := w.WriteEntry(&hlinked); err != nil {
				t.Fatalf("WriteEntry: %v", err)
			}
			if !bytes.Equal(buf.Bytes(), tc.want) {
				t.Errorf("entry bytes = % x, want % x", buf.Bytes(), tc.want)
			}
		})
	}

	// the varint end-marker gate: CF_VARINT_FLIST_FLAGS encodes the
	// end-of-list marker as varint 0 plus a varint io_error (two 0x00
	// bytes); without the flag it is a single 0x00 byte
	for _, tc := range []struct {
		name   string
		varint bool
		want   []byte
	}{
		{"byte-endmarker", false, []byte{0x00}},
		{"varint-endmarker", true, []byte{0x00, 0x00}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			w := protocol.NewFlistWriter(&buf, 32, tc.varint)
			if err := w.WriteEndMarker(); err != nil {
				t.Fatalf("WriteEndMarker: %v", err)
			}
			if !bytes.Equal(buf.Bytes(), tc.want) {
				t.Errorf("end marker = % x, want % x", buf.Bytes(), tc.want)
			}
		})
	}
}

// TestVersionFlistRoundTrip walks a small representative file list (top
// directory, regular files with a same-mode reuse, a symlink) through
// FlistWriter and back through FlistReader on every supported protocol
// version, checking that the delta encoding round-trips and that the
// version-gated fields (mod_nsec from 31, hard-link dev/ino below 30,
// varint xflags from 30) behave as the gates require.
func TestVersionFlistRoundTrip(t *testing.T) {
	entries := []*protocol.FlistEntry{
		{Name: ".", Mode: 0o040755, TopDir: true},
		{Name: "a.txt", Mode: 0o100644, Size: 5, Mtime: 1700000000, UID: 12, GID: 34, ModNsec: 123456789},
		{Name: "b.txt", Mode: 0o100644, Size: 10, Mtime: 1700000000, UID: 12, GID: 34, Nlink: 2, Dev: 7, Ino: 42},
		{Name: "link", Mode: 0o120777, Mtime: 1700000000, UID: 12, GID: 34, LinkTarget: "a.txt"},
	}

	for v := protocol.MinProtocolVersion; v <= protocol.CurrentProtocolVersion; v++ {
		varint := v >= 30
		name := "v" + strconv.Itoa(v)
		if varint {
			name += "-varint"
		}
		t.Run(name, func(t *testing.T) {
			var buf bytes.Buffer
			w := protocol.NewFlistWriter(&buf, v, varint)
			for _, e := range entries {
				if err := w.WriteEntry(e); err != nil {
					t.Fatalf("WriteEntry(%s): %v", e.Name, err)
				}
			}
			if err := w.WriteIDLists(); err != nil {
				t.Fatalf("WriteIDLists: %v", err)
			}
			if err := w.WriteEndMarker(); err != nil {
				t.Fatalf("WriteEndMarker: %v", err)
			}

			r := protocol.NewFlistReader(&buf, v, varint)
			for i, want := range entries {
				got, err := r.ReadEntry()
				if err != nil {
					t.Fatalf("ReadEntry %d: %v", i, err)
				}
				if got.Name != want.Name {
					t.Errorf("entry %d name = %q, want %q", i, got.Name, want.Name)
				}
				if got.Mode != want.Mode {
					t.Errorf("entry %d mode = %#o, want %#o", i, got.Mode, want.Mode)
				}
				if got.Size != want.Size {
					t.Errorf("entry %d size = %d, want %d", i, got.Size, want.Size)
				}
				if got.Mtime != want.Mtime {
					t.Errorf("entry %d mtime = %d, want %d", i, got.Mtime, want.Mtime)
				}
				wantNsec := want.ModNsec
				if v < 31 {
					wantNsec = 0 // XMIT_MOD_NSEC does not exist below proto 31
				}
				if got.ModNsec != wantNsec {
					t.Errorf("entry %d mod_nsec = %d, want %d (proto %d)", i, got.ModNsec, wantNsec, v)
				}
				if got.UID != want.UID || got.GID != want.GID {
					t.Errorf("entry %d uid/gid = %d/%d, want %d/%d", i, got.UID, got.GID, want.UID, want.GID)
				}
				if got.LinkTarget != want.LinkTarget {
					t.Errorf("entry %d link target = %q, want %q", i, got.LinkTarget, want.LinkTarget)
				}
				// hard-link identity: transmitted as dev/ino below proto
				// 30 (flagged at 28-29, unconditional below 28 for regular
				// files), replaced by the back-reference encoding from 30
				if v < 30 && want.Nlink > 1 {
					if got.Dev != want.Dev || got.Ino != want.Ino {
						t.Errorf("entry %d dev/ino = %d/%d, want %d/%d", i, got.Dev, got.Ino, want.Dev, want.Ino)
					}
				}
			}
			// the end marker terminates the list
			if _, err := r.ReadEntry(); err != io.EOF {
				t.Errorf("ReadEntry after end marker = %v, want io.EOF", err)
			}
		})
	}
}

// TestVersionFallback_ModNsec checks the end-to-end fallback for the
// nanosecond timestamp feature: a file whose mtime has a nanosecond
// component arrives intact on proto 31 and later, and the sub-second
// component is dropped (the seconds are kept) when the connection
// negotiates proto 30, where XMIT_MOD_NSEC does not exist.  The file
// content is pulled in full on both paths to confirm the transfer itself
// is unaffected by the dropped field.
func TestVersionFallback_ModNsec(t *testing.T) {
	const wantNsec = 123456789
	m := fstest.MapFS{
		"file.txt": &fstest.MapFile{Data: []byte("nanosecond test"), Mode: 0o644, ModTime: time.Date(2024, 3, 1, 12, 0, 0, wantNsec, time.UTC)},
	}

	for _, tc := range []struct {
		version   int
		wantNsec  int
		wantEpoch int64
	}{
		{30, 0, 1709294400}, // sub-second component dropped, seconds kept
		{31, wantNsec, 1709294400},
	} {
		t.Run(strconv.Itoa(tc.version), func(t *testing.T) {
			s, err := NewServer(&ServerModule{Name: "testmod", FS: m})
			if err != nil {
				t.Fatalf("NewServer: %v", err)
			}
			s.Greeting = protocol.Greeting{Version: tc.version, SubProtocol: 0, Digests: protocol.SupportedDigests()}
			serverEnd, clientEnd := BufPipe()
			doneCh := make(chan error, 1)
			go func() {
				defer serverEnd.Close()
				doneCh <- s.HandleConnection(serverEnd)
			}()
			defer clientEnd.Close()

			sess, err := pinnedVersionClient(tc.version).Connect(clientEnd)
			if err != nil {
				t.Fatalf("Connect: %v", err)
			}
			f, err := sess.Open("file.txt")
			if err != nil {
				t.Fatalf("Open: %v", err)
			}
			data, err := io.ReadAll(f)
			if err != nil {
				t.Fatalf("ReadAll: %v", err)
			}
			if string(data) != "nanosecond test" {
				t.Errorf("content = %q, want the original", data)
			}
			info, err := f.Stat()
			if err != nil {
				t.Fatalf("Stat: %v", err)
			}
			got := info.ModTime()
			if got.Unix() != tc.wantEpoch {
				t.Errorf("ModTime().Unix() = %d, want %d", got.Unix(), tc.wantEpoch)
			}
			if got.Nanosecond() != tc.wantNsec {
				t.Errorf("ModTime().Nanosecond() = %d, want %d (proto %d)", got.Nanosecond(), tc.wantNsec, tc.version)
			}

			select {
			case serr := <-doneCh:
				t.Logf("server returned: %v", serr)
			case <-time.After(10 * time.Second):
				t.Fatal("server did not exit")
			}
		})
	}
}
