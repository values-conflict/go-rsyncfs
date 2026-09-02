package rsyncfs

// Port of the upstream testsuite test proto-hlink-gnum_test.py (upstream rsync @ 471e17dc, "Preparing for release of 3.5.0").
//
// What the upstream test catches: with inc_recurse, a non-first hard-link entry carries first_hlink_ndx as its gnum, and a peer can back-reference a gnum that no earlier flist ever declared via XMIT_HLINK_FIRST.  recv_file_entry() only bounds it as [0, ndx_start + used), and the new-node branch in match_gnums() (hlink.c) then hit assert(gnum >= hlink_flist->ndx_start) -- a remotely-reachable abort() of the generator on default (assert-enabled) builds.  The fix replaces the assert with a clean RERR_PROTOCOL; the receiver-side half of the defense is recv_file_entry's bounds check itself ("hard-link reference out of range"), which is the rule this port pins.
//
// Role mapping: upstream pushes a crafted sub-flist at a real daemon receiver; we drive hand-written file list wire bytes at protocol.FlistReader, the recv_file_entry equivalent, with hard-link preservation on (the receiver's -H; without it, the abbreviated form is not interpreted at all -- see the FLAG_HLINKED gate ported in proto-hlink-flag-oob).  The reader negotiates a single flat list, so the flat-list collapse of the bound applies: the reference must point at an earlier entry of this list (ndx_start == 1, used == entries so far).  An abbreviated entry whose first_hlink_ndx points past the end of the list is the flat-mode incarnation of the upstream "gnum precedes flist start" attack.
//
// Oracle: the crafted entry is refused with "hard-link reference out of range" (a clean error, not a panic, not a silent accept), while the well-formed abbreviated entry that references the declared first-hlink just before it parses cleanly -- proving the reader stayed in sync through the abbreviated form and the bound is the specific thing that fired.  The boundary is pinned on both sides: references to every earlier entry parse, the first past-the-end reference is refused, and a negative one is refused.

import (
	"bytes"
	"encoding/binary"
	"io"
	"strings"
	"testing"

	"github.com/values-conflict/go-rsyncfs/protocol"
)

// xflagsBytes30 encodes one proto-30 xflags value in the byte/shortint (non-varint) form, mirroring protocol.writeXflags: the extended form (a uint16 with the XMIT_EXTENDED_FLAGS bit in the low byte) whenever any high-bit flag is set, so the first byte is never 0 (which would read as end-of-list).
func xflagsBytes30(xflags int) []byte {
	if xflags&0xFF00 != 0 || xflags == 0 {
		xflags |= protocol.XmitExtendedFlags
		var b [2]byte
		binary.LittleEndian.PutUint16(b[:], uint16(xflags))
		return b[:]
	}
	return []byte{byte(xflags)}
}

// hlinkFirstEntry30 encodes a full proto-30 file list entry flagged XMIT_HLINK_FIRST (the declared head of a hard-link group): name, varlong30 size, varlong4 mtime, int32 mode, varint uid, varint gid -- all zero/1000/0644 values, the int32-range form of each.
func hlinkFirstEntry30(name string) []byte {
	var b bytes.Buffer
	b.Write(xflagsBytes30(protocol.XmitHlinkFirst))
	b.WriteByte(byte(len(name)))
	b.WriteString(name)
	var four [4]byte
	binary.LittleEndian.PutUint32(four[:], 0) // size
	b.Write(four[:])
	binary.LittleEndian.PutUint32(four[:], 1000) // mtime
	b.Write(four[:])
	binary.LittleEndian.PutUint32(four[:], 0o100644) // mode
	b.Write(four[:])
	b.WriteByte(0) // uid
	b.WriteByte(0) // gid
	return b.Bytes()
}

// hlinkAbbrevEntry30 encodes an abbreviated proto-30 hard-link entry (XMIT_HLINKED without XMIT_HLINK_FIRST): name + varint first_hlink_ndx and nothing else -- the layout a malicious peer gets to choose the reference of.
func hlinkAbbrevEntry30(name string, ref int32) []byte {
	var b bytes.Buffer
	b.Write(xflagsBytes30(protocol.XmitHlinked))
	b.WriteByte(byte(len(name)))
	b.WriteString(name)
	if err := protocol.WriteVarint(&b, ref); err != nil {
		panic(err)
	}
	return b.Bytes()
}

// TestUpstream_ProtoHlinkGnum drives the upstream attack shape at the reader: a declared first (ndx 1 -- the first flist's ndx_start), a lawful reference to it, and the crafted reference far past the end of the list.
func TestUpstream_ProtoHlinkGnum(t *testing.T) {
	var buf bytes.Buffer
	buf.Write(hlinkFirstEntry30("hfirst"))
	buf.Write(hlinkAbbrevEntry30("hgood", 1)) // lawful: references entry ndx 1 ("hfirst")
	buf.Write(hlinkAbbrevEntry30("hbad", 5))  // crafted: past the end (used = 2)
	buf.WriteByte(0)                          // end marker

	r := protocol.NewFlistReader(&buf, 30, false)
	r.SetPreserveHardlinks(true)

	first, err := r.ReadEntry()
	if err != nil || first.Name != "hfirst" {
		t.Fatalf("first entry: %v (%+v)", err, first)
	}
	good, err := r.ReadEntry()
	if err != nil || good.Name != "hgood" || good.HlinkNdx != 1 {
		t.Fatalf("lawful abbreviated entry: %v (%+v)", err, good)
	}
	_, err = r.ReadEntry()
	if err == nil {
		t.Fatal("crafted reference past the end of the list was accepted")
	}
	if !strings.Contains(err.Error(), "hard-link reference out of range") {
		t.Fatalf("ReadEntry error = %q, want the hlink reference bounds refusal", err)
	}
}

// TestUpstream_ProtoHlinkGnum_Boundary pins the bound itself: references to earlier entries parse, the first past-the-end reference is refused, and so are a negative one and a reference before the flist start (the flat-mode "gnum precedes flist start").
func TestUpstream_ProtoHlinkGnum_Boundary(t *testing.T) {
	build := func(refs ...int32) *bytes.Buffer {
		buf := bytes.NewBuffer(nil)
		buf.Write(hlinkFirstEntry30("base"))
		for i, ref := range refs {
			buf.Write(hlinkAbbrevEntry30(string([]byte{'e', byte('0' + i)}), ref))
		}
		buf.WriteByte(0) // end marker
		return buf
	}

	cases := []struct {
		name string
		refs []int32
		want string
	}{
		{"reference to the declared first (ndx 1) parses", []int32{1}, ""},
		{"second entry also references the first", []int32{1, 1}, ""},
		{"reference to used (first past the end) is refused", []int32{2}, "hard-link reference out of range"},
		{"reference far past the end is refused", []int32{1, 5}, "hard-link reference out of range"},
		{"negative reference is refused", []int32{-1}, "hard-link reference out of range"},
		{"reference before the flist start is refused", []int32{0}, "hard-link gnum 0 precedes flist start 1"},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			r := protocol.NewFlistReader(build(tt.refs...), 30, false)
			r.SetPreserveHardlinks(true)
			var last *protocol.FlistEntry
			var err error
			for {
				e, eerr := r.ReadEntry()
				if eerr == io.EOF {
					break
				}
				if eerr != nil {
					err = eerr
					break
				}
				last = e
			}
			if tt.want == "" {
				if err != nil {
					t.Fatalf("ReadEntry: %v (last=%+v)", err, last)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("ReadEntry error = %v, want %q", err, tt.want)
			}
		})
	}
}
