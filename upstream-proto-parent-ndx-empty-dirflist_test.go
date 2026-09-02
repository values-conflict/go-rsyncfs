package rsyncfs

// Port of the upstream testsuite test proto-parent-ndx-empty-dirflist_test.py (upstream rsync @ 471e17dc, "Preparing for release of 3.5.0").
//
// What the upstream test catches: under inc_recurse, the first file list's parent_ndx defaults to the dir_flist slot 0, and recv_file_list() trusted the peer's "." entry to be the transfer root.  A "." entry with a NON-directory mode never lands in dir_flist, so the slot stays uninitialised and the parent_ndx consumers (generate_files, recv_files, touch_up_dirs) dereference it -- a wild-pointer read on freshly realloc()'d heap.  The fix (upstream flist.c) refuses the entry outright in recv_file_entry: "rejecting non-directory transfer-root entry" (RERR_PROTOCOL).  Three layers stop the bug in wire order; this test gates the attack shape, i.e. that first-layer refusal.
//
// Role mapping: upstream drives a malicious SENDER at the daemon receiver; we drive a malicious DAEMON (hand-written proto-27 wire) at our Client, whose protocol.FlistReader is the recv_file_entry equivalent.  The attack bytes are identical in shape -- a file list whose "." entry carries a regular-file mode -- and the oracle is identical in behavior: the receiver refuses the entry with a clean protocol error instead of indexing a slot that was never filled.  Go makes the C symptom (uninitialised heap deref) unexpressible, but the protocol rule it encodes is the same one we now validate.
//
// Oracle: [Session.Open] on the module root fails with "rejecting non-directory transfer-root entry" (wrapped in "read file list"), no panic; the second entry ("a", a perfectly ordinary file) is never reached, which proves the refusal fired at the "." entry and not at a later stream desync.

import (
	"encoding/binary"
	"io"
	"net"
	"strings"
	"testing"
)

// runDotFileDaemon is the hand-written malicious daemon for this port: it completes the proto-27 handshake (greeting, module, OK, args, raw seed, filter list) and then streams one MSG_DATA-framed file list in which the transfer root "." is a REGULAR file, followed by an ordinary file "a" -- the exact entry pair from the upstream test (the "a" keeps the list from being diverted to the single-file special case).
func runDotFileDaemon(t *testing.T, conn net.Conn) {
	t.Helper()
	mustWrite := func(b []byte) {
		t.Helper()
		if _, err := conn.Write(b); err != nil {
			t.Errorf("daemon write: %v", err)
		}
	}

	// handshake, mirroring the honest flow (proto 27: no auth, no
	// compat flags, raw seed, raw filter list)
	if g, err := readLine(conn); err != nil || !strings.HasPrefix(g, "@RSYNCD: ") {
		t.Errorf("client greeting: %q (%v)", g, err)
		return
	}
	mustWrite([]byte("@RSYNCD: 27 md4\n"))
	if m, _ := readLine(conn); m != "mod" {
		t.Errorf("module request = %q", m)
		return
	}
	mustWrite([]byte("@RSYNCD: OK\n"))
	for {
		l, err := readLine(conn)
		if err != nil || l == "" {
			break
		}
	}
	mustWrite([]byte{0x2a, 0x00, 0x00, 0x00}) // checksum seed (0x2a, arbitrary non-zero)
	var filterLen [4]byte
	if _, err := io.ReadFull(conn, filterLen[:]); err != nil {
		t.Errorf("read filter list: %v", err)
		return
	}

	// the file list, one MSG_DATA frame (the daemon's output is
	// multiplexed from proto 23 on)
	//
	// entry 1: "." S_IFREG|0644 -- the transfer root as a regular file
	//   (xflags 0x01 = the TOP_DIR placeholder the pre-28 encoder uses
	//   to avoid a zero flag byte; l2=1; name; longint size; int32
	//   mtime; int32 mode; int32 uid; int32 gid)
	// entry 2: "a" S_IFREG|0644, everything delta-reused (SAME_TIME |
	//   SAME_MODE | SAME_UID | SAME_GID = 0x9A; l2=1; name; longint size)
	// end marker (0x00), then the uid list, gid list, and io_error
	// trailer (all int32 zero for proto < 30)
	payload := []byte{
		0x01, 0x01, 0x2e,
		0x64, 0x00, 0x00, 0x00, // size 100
		0xE8, 0x03, 0x00, 0x00, // mtime 1000
		0xA4, 0x81, 0x00, 0x00, // mode 0o100644
		0, 0, 0, 0, // uid 0
		0, 0, 0, 0, // gid 0
		0x9A, 0x01, 0x61,
		0x64, 0x00, 0x00, 0x00, // size 100
		0x00,       // end marker
		0, 0, 0, 0, // uid list terminator
		0, 0, 0, 0, // gid list terminator
		0, 0, 0, 0, // io_error trailer
	}
	var hdr [4]byte
	binary.LittleEndian.PutUint32(hdr[:], (7<<24)|uint32(len(payload))) // MPLEX_BASE + MSG_DATA
	mustWrite(hdr[:])
	mustWrite(payload)

	// the client will reject the list and close; drain until then
	_, _ = io.Copy(io.Discard, conn)
}

func TestUpstream_ProtoParentNdxEmptyDirflist(t *testing.T) {
	daemonConn, clientConn := net.Pipe()
	done := make(chan struct{}, 1)
	go func() {
		defer close(done)
		defer daemonConn.Close()
		runDotFileDaemon(t, daemonConn)
	}()
	defer clientConn.Close()

	sess, err := (Client{Module: "mod"}).Connect(clientConn)
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}

	_, err = sess.Open(".")
	if err == nil {
		t.Fatal("Open succeeded on a module whose transfer root is a regular file")
	}
	if !strings.Contains(err.Error(), "rejecting non-directory transfer-root entry") {
		t.Fatalf("Open error = %q, want the transfer-root refusal", err)
	}
	<-done
}
