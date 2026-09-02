package rsyncfs

// Port of the upstream testsuite test proto-sender-selftest_test.py (upstream rsync @ 471e17dc, "Preparing for release of 3.5.0").
//
// What the upstream test is: a self-test / bitrot guard for the pure-Python rsync sender the testsuite uses to push (possibly malformed) file lists at a real daemon.  It pins the encoder against the rsync binary under test by checking that the daemon's recv_file_entry() actually parses the encoded bytes: a well-formed flat file list parses with no flist error, and an entry whose name length is encoded as XMIT_LONG_NAME + an absurd varint trips recv_file_entry's "overflow: xflags=..." guard -- which is only reachable if the flags byte and the varint name length were read exactly, proving the encoder/decoder pair is aligned (a desync would error elsewhere, or not at all).
//
// Our implementation: protocol.FlistWriter is the second hand-written implementation of the send_file_entry() wire format, and protocol.FlistReader is the recv_file_entry() equivalent.  The port keeps the same two-oracle shape: a well-formed list round-trips writer → reader with no error, and a hand-crafted XMIT_LONG_NAME + huge varint name length trips the ported overflow guard.  The guard itself is new in this code (upstream's overflow_exit is a process abort; our reader returns a clean error), which is why the port matters as much for the guard as for the encoder.
//
// Oracle: (1) the writer's output parses entry-for-entry with the reader -- names, modes, sizes intact, clean end of list; (2) the absurd name length is refused with "overflow: xflags=0x58" (the exact flag value the upstream test uses: XMIT_LONG_NAME | XMIT_SAME_UID | XMIT_SAME_GID), not a panic and not a mis-parsed entry.

import (
	"bytes"
	"io"
	"strconv"
	"strings"
	"testing"

	"github.com/values-conflict/go-rsyncfs/protocol"
)

// TestUpstream_ProtoSenderSelftest_WellFormed: a well-formed flat file list round-trips FlistWriter → FlistReader with no flist error.
func TestUpstream_ProtoSenderSelftest_WellFormed(t *testing.T) {
	for _, version := range []int{27, 30, 32} {
		t.Run("proto"+strconv.Itoa(version), func(t *testing.T) {
			entries := []*protocol.FlistEntry{
				{Name: ".", Mode: 0o040755, TopDir: true},
				{Name: "hello.txt", Mode: 0o100644, Size: 5, Mtime: 1700000000, UID: 1000, GID: 1000},
				{Name: "sub", Mode: 0o040755, Mtime: 1700000000},
				{Name: "sub/inner.bin", Mode: 0o100600, Size: 4096, Mtime: 1700000123, UID: 0, GID: 0},
			}
			var buf bytes.Buffer
			w := protocol.NewFlistWriter(&buf, version, version == 32)
			for _, e := range entries {
				if err := w.WriteEntry(e); err != nil {
					t.Fatalf("WriteEntry(%q): %v", e.Name, err)
				}
			}
			if err := w.WriteEndMarker(); err != nil {
				t.Fatalf("WriteEndMarker: %v", err)
			}

			r := protocol.NewFlistReader(&buf, version, version == 32)
			for i, want := range entries {
				got, err := r.ReadEntry()
				if err != nil {
					t.Fatalf("ReadEntry %d: %v", i, err)
				}
				if got.Name != want.Name || got.Mode != want.Mode || got.Size != want.Size || got.Mtime != want.Mtime || got.UID != want.UID || got.GID != want.GID {
					t.Fatalf("entry %d = %+v, want %+v", i, got, want)
				}
			}
			if _, err := r.ReadEntry(); err != io.EOF {
				t.Fatalf("end of list: %v, want io.EOF", err)
			}
		})
	}
}

// TestUpstream_ProtoSenderSelftest_Overflow: an XMIT_LONG_NAME entry whose varint name length is absurd must trip the ported recv_file_entry overflow guard, not a panic.
func TestUpstream_ProtoSenderSelftest_Overflow(t *testing.T) {
	// flags 0x58 = XMIT_LONG_NAME | XMIT_SAME_UID | XMIT_SAME_GID (the
	// upstream test's exact byte), then the absurd varint length
	// 0x7fffff and two payload bytes (never read -- the guard fires
	// before the suffix is)
	var buf bytes.Buffer
	if err := protocol.WriteVarint(&buf, 0x58); err != nil {
		t.Fatal(err)
	}
	if err := protocol.WriteVarint(&buf, 0x7fffff); err != nil {
		t.Fatal(err)
	}
	buf.WriteString("xx")

	r := protocol.NewFlistReader(&buf, 32, true)
	_, err := r.ReadEntry()
	if err == nil {
		t.Fatal("an absurd varint name length was accepted")
	}
	if !strings.Contains(err.Error(), "overflow: xflags=0x58") {
		t.Fatalf("ReadEntry error = %q, want the overflow guard (xflags=0x58)", err)
	}

	// a negative varint length is the same guard's business: it must be
	// refused, not panic in make([]byte, -n)
	var neg bytes.Buffer
	if err := protocol.WriteVarint(&neg, 0x58); err != nil {
		t.Fatal(err)
	}
	if err := protocol.WriteVarint(&neg, -1); err != nil {
		t.Fatal(err)
	}
	r = protocol.NewFlistReader(&neg, 32, true)
	if _, err := r.ReadEntry(); err == nil || !strings.Contains(err.Error(), "overflow") {
		t.Fatalf("negative name length: %v, want the overflow guard", err)
	}
}
