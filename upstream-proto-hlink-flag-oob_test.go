package rsyncfs

// Port of the upstream testsuite test proto-hlink-flag-oob_test.py (upstream rsync @ 471e17dc, "Preparing for release of 3.5.0").
//
// What the upstream test catches: a heap out-of-bounds WRITE in recv_file_entry() (flist.c) -- a malicious sender that sets XMIT_HLINKED on a file even though the receiver was not invoked with -H.  recv_file_entry() reserved the hard-link extra slots only under preserve_hard_links, but used to set FLAG_HLINKED straight from the peer's xflags; HLINK_BUMP() then offset F_SUM() past the start of the allocation and the checksum read wrote attacker-controlled bytes below it.  The fix gates FLAG_HLINKED on preserve_hard_links.  The ASan redzone oracle is C-specific and does not port; the protocol rule it encodes does: the peer's XMIT_HLINKED flag without hard-link preservation on our side must be *ignored* -- not honored, not fatal -- so the field layout a non-`-H` daemon would emit still parses and the stream stays in sync.
//
// Role mapping: the daemon side of the attack (a real rsync pushing an XMIT_HLINKED entry at a no-`-H` receiver) maps directly onto protocol.FlistReader with SetPreserveHardlinks(false) -- our client's actual configuration, since it never requests -H.  The wire bytes are hand-built the way a misbehaving (or simply honest, flag-carrying) daemon would send them: the flag set, the hard-link fields absent.
//
// Oracle: with preservation off, the entry parses as a plain file and the following entry / end marker land exactly where they should (proto 30 full-entry layout despite the flag; proto 28 with no dev/ino pair despite the flag).  A counter-case with preservation on and the pair present parses as well, proving the gate works both ways rather than skipping the field unconditionally.

import (
	"bytes"
	"io"
	"testing"

	"github.com/values-conflict/go-rsyncfs/protocol"
)

// TestUpstream_ProtoHlinkFlagOob_Proto30: a proto-30 entry carries XMIT_HLINKED but the hard-link reference is not on the wire (the daemon did not abbreviate, the receiver has no -H) -- the reader with preservation off must parse the full entry and stay in sync.
func TestUpstream_ProtoHlinkFlagOob_Proto30(t *testing.T) {
	// entry 1: "h.txt" S_IFREG|0644 with XMIT_HLINKED, full layout
	var b bytes.Buffer
	b.Write(xflagsBytes30(protocol.XmitHlinked))
	b.WriteByte(byte(len("h.txt")))
	b.WriteString("h.txt")
	if err := protocol.WriteVarlong(&b, 0, 3); err != nil {
		t.Fatal(err) // size
	}
	if err := protocol.WriteVarlong(&b, 1000, 4); err != nil {
		t.Fatal(err) // mtime
	}
	if err := protocol.WriteUint32(&b, 0o100644); err != nil {
		t.Fatal(err) // mode
	}
	if err := protocol.WriteVarint(&b, 0); err != nil {
		t.Fatal(err) // uid
	}
	if err := protocol.WriteVarint(&b, 0); err != nil {
		t.Fatal(err) // gid
	}
	// entry 2: "next" -- a plain file that must land exactly in sync
	//   (SAME_TIME | SAME_MODE | SAME_UID | SAME_GID = 0x9A: low byte,
	//   single-byte xflags form)
	b.Write([]byte{0x9A})
	b.WriteByte(byte(len("next")))
	b.WriteString("next")
	if err := protocol.WriteVarlong(&b, 42, 3); err != nil {
		t.Fatal(err)
	}
	b.WriteByte(0) // end marker (xflags 0 in the non-varint form)

	r := protocol.NewFlistReader(&b, 30, false)
	r.SetPreserveHardlinks(false) // the receiver has no -H
	e1, err := r.ReadEntry()
	if err != nil {
		t.Fatalf("entry 1: %v", err)
	}
	if e1.Name != "h.txt" || e1.Mode != 0o100644 || e1.Size != 0 {
		t.Fatalf("entry 1 = %+v, want h.txt/0o100644/0", e1)
	}
	e2, err := r.ReadEntry()
	if err != nil {
		t.Fatalf("entry 2 (stream desynced by the ignored flag?): %v", err)
	}
	if e2.Name != "next" || e2.Size != 42 {
		t.Fatalf("entry 2 = %+v, want next/42", e2)
	}
	if _, err := r.ReadEntry(); err != io.EOF {
		t.Fatalf("end of list: %v, want io.EOF", err)
	}
}

// TestUpstream_ProtoHlinkFlagOob_Proto28: the proto-28/29 incarnation -- XMIT_HLINKED on a regular file whose dev/ino pair is not on the wire; preservation off must not read the pair, and the list must end cleanly.
func TestUpstream_ProtoHlinkFlagOob_Proto28(t *testing.T) {
	// entry 1: "h.txt" S_IFREG|0644, XMIT_HLINKED (two-byte xflags
	// form: [0x04, 0x02]), NO dev/ino pair on the wire
	var b bytes.Buffer
	b.Write(xflagsBytes30(protocol.XmitHlinked))
	b.WriteByte(byte(len("h.txt")))
	b.WriteString("h.txt")
	if err := protocol.WriteLongInt(&b, 0); err != nil {
		t.Fatal(err) // size
	}
	if err := protocol.WriteUint32(&b, 1000); err != nil {
		t.Fatal(err) // mtime
	}
	if err := protocol.WriteUint32(&b, 0o100644); err != nil {
		t.Fatal(err) // mode
	}
	if err := protocol.WriteUint32(&b, 0); err != nil {
		t.Fatal(err) // uid
	}
	if err := protocol.WriteUint32(&b, 0); err != nil {
		t.Fatal(err) // gid
	}
	b.WriteByte(0) // end marker

	r := protocol.NewFlistReader(&b, 28, false)
	r.SetPreserveHardlinks(false)
	e1, err := r.ReadEntry()
	if err != nil {
		t.Fatalf("entry 1: %v", err)
	}
	if e1.Name != "h.txt" || e1.Mode != 0o100644 {
		t.Fatalf("entry 1 = %+v", e1)
	}
	if _, err := r.ReadEntry(); err != io.EOF {
		t.Fatalf("end of list: %v, want io.EOF (the reader tried to read a dev/ino pair the sender never sent)", err)
	}
}

// TestUpstream_ProtoHlinkFlagOob_Counter: with preservation ON and the pair on the wire, the same flag is honored -- proving the gate selects the layout rather than skipping the field unconditionally.
func TestUpstream_ProtoHlinkFlagOob_Counter(t *testing.T) {
	// entry 1: "h.txt" S_IFREG|0644, XMIT_HLINKED, dev/ino pair present
	//   (longint dev 7, longint ino 9 -- the proto-28/29 layout)
	var b bytes.Buffer
	b.Write(xflagsBytes30(protocol.XmitHlinked))
	b.WriteByte(byte(len("h.txt")))
	b.WriteString("h.txt")
	if err := protocol.WriteLongInt(&b, 0); err != nil {
		t.Fatal(err)
	}
	if err := protocol.WriteUint32(&b, 1000); err != nil {
		t.Fatal(err)
	}
	if err := protocol.WriteUint32(&b, 0o100644); err != nil {
		t.Fatal(err)
	}
	if err := protocol.WriteUint32(&b, 0); err != nil {
		t.Fatal(err)
	}
	if err := protocol.WriteUint32(&b, 0); err != nil {
		t.Fatal(err)
	}
	if err := protocol.WriteLongInt(&b, 7); err != nil {
		t.Fatal(err) // dev
	}
	if err := protocol.WriteLongInt(&b, 9); err != nil {
		t.Fatal(err) // ino
	}
	b.WriteByte(0) // end marker

	r := protocol.NewFlistReader(&b, 28, false)
	r.SetPreserveHardlinks(true)
	e1, err := r.ReadEntry()
	if err != nil {
		t.Fatalf("entry 1: %v", err)
	}
	if e1.Name != "h.txt" || e1.Dev != 7 || e1.Ino != 9 {
		t.Fatalf("entry 1 = %+v, want h.txt dev=7 ino=9", e1)
	}
	if _, err := r.ReadEntry(); err != io.EOF {
		t.Fatalf("end of list: %v, want io.EOF", err)
	}
}

// the reader must also refuse the abbreviated proto-30 form when the
// reference is out of range -- that is the hlink-gnum port's rule, and
// it applies inside the preservation-on branch this file is pinning
func TestUpstream_ProtoHlinkFlagOob_AbbrevGated(t *testing.T) {
	// an abbreviated entry (name + varint ref only) sent to a reader
	// WITHOUT preservation must NOT be parsed as abbreviated: the flag
	// is ignored, so the reader consumes the full-entry fields from the
	// bytes that follow, and the "reference" byte is just the size field
	var b bytes.Buffer
	b.Write(xflagsBytes30(protocol.XmitHlinked))
	b.WriteByte(byte(len("h.txt")))
	b.WriteString("h.txt")
	// full-entry fields (the daemon did not abbreviate even though it
	// set the flag) -- the 0 here doubles as the size
	if err := protocol.WriteVarlong(&b, 0, 3); err != nil {
		t.Fatal(err)
	}
	if err := protocol.WriteVarlong(&b, 1000, 4); err != nil {
		t.Fatal(err)
	}
	if err := protocol.WriteUint32(&b, 0o100644); err != nil {
		t.Fatal(err)
	}
	if err := protocol.WriteVarint(&b, 0); err != nil {
		t.Fatal(err)
	}
	if err := protocol.WriteVarint(&b, 0); err != nil {
		t.Fatal(err)
	}
	b.WriteByte(0) // end marker

	r := protocol.NewFlistReader(&b, 30, false)
	r.SetPreserveHardlinks(false)
	e1, err := r.ReadEntry()
	if err != nil {
		t.Fatalf("entry 1: %v", err)
	}
	if e1.Name != "h.txt" || e1.Mode != 0o100644 || e1.Size != 0 {
		t.Fatalf("entry 1 = %+v, want the full-entry parse (flag ignored)", e1)
	}
	if e1.HlinkNdx != 0 {
		t.Fatalf("HlinkNdx = %d, want 0 (the abbreviated form was not honored)", e1.HlinkNdx)
	}
	if _, err := r.ReadEntry(); err != io.EOF {
		t.Fatalf("end of list: %v, want io.EOF", err)
	}
}
