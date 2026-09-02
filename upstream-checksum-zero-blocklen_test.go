package rsyncfs

// Port of the upstream testsuite test checksum-zero-blocklen_test.py (upstream rsync @ 471e17dc, "Preparing for release of 3.5.0").
//
// What the upstream test catches: a malicious RECEIVER (its generator) can put a count>0 / blength=0 checksum header on the receiver→sender path.  Upstream's read_sum_head() (io.c) used to hand that zero block length to the hash-table and block-size arithmetic without a guard, so the test pins the guard: "Invalid zero block length" (RERR_PROTOCOL), no crash.
//
// Direction note: the upstream attack flows generator → sender -- the malicious end is the daemon side and the vulnerable code is the *client's* read_sum_head.  We have no client-side sender to play that role, so the port flips the roles in the one place the same guard exists in our code: our Server (the daemon, playing the sender) reads the generator's sum head in receiveSums, and protocol.ReadSumHead carries the ported read_sum_head guards.  A malicious *client* pushes a count>0/blength=0 head at our server's selector loop.  Same protocol rule, same code layer: the side that parses a peer-supplied sum struct validates it before use.
//
// Oracle: the connection is rejected with an error naming the zero block length (not a panic, not a clean transfer), and the Server is still usable for the next connection.  A companion table test pins every guard the ported read_sum_head now carries (negative count, block length past MAX_BLOCK_SIZE / OLD_MAX_BLOCK_SIZE, count*blength past an int32 file length, and s2length past the digest length), each rejected with a clean error.

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/values-conflict/go-rsyncfs/protocol"
)

// runZeroBlocklenTransfer is the full-connection half of the port: a hand-written proto-27 client talks to a real Server over net.Pipe, pulls the file list, then asks for the file's data with a count=1 / blength=0 sum head -- the exact attack shape from the upstream test.  It returns the Server and the error HandleConnection reported.
func runZeroBlocklenTransfer(t *testing.T) (*Server, error) {
	t.Helper()
	mod := &ServerModule{
		Name: "mod",
		FS: fstest.MapFS{
			"f": &fstest.MapFile{Data: []byte("hello world")},
		},
	}
	s, err := NewServer(mod)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}

	serverConn, clientConn := net.Pipe()
	done := make(chan error, 1)
	go func() {
		defer serverConn.Close()
		done <- s.HandleConnection(serverConn)
	}()
	defer clientConn.Close()

	// handshake: proto 27, no auth, newline-terminated args, no compat
	// flags or algorithm negotiation, raw seed
	line := func() string {
		t.Helper()
		l, err := readLine(clientConn)
		if err != nil {
			t.Fatalf("read line: %v", err)
		}
		return l
	}
	if g := line(); !strings.HasPrefix(g, "@RSYNCD: ") {
		t.Fatalf("server greeting = %q", g)
	}
	mustWrite := func(b []byte) {
		t.Helper()
		if _, err := clientConn.Write(b); err != nil {
			t.Fatalf("write: %v", err)
		}
	}
	mustWrite([]byte("@RSYNCD: 27\n"))
	mustWrite([]byte("mod\n"))
	if l := line(); l != "@RSYNCD: OK" {
		t.Fatalf("auth response = %q", l)
	}
	mustWrite([]byte(".\n--server\n--sender\n-vlogDtpre\n.\n\n"))
	var seedBuf [4]byte
	if _, err := io.ReadFull(clientConn, seedBuf[:]); err != nil {
		t.Fatalf("read seed: %v", err)
	}
	// legacy exclude list: a single zero length
	mustWrite([]byte{0, 0, 0, 0})

	// the file list arrives as one MSG_DATA frame (daemon output is
	// multiplexed from proto 23 on)
	var hdr [4]byte
	if _, err := io.ReadFull(clientConn, hdr[:]); err != nil {
		t.Fatalf("read flist frame header: %v", err)
	}
	tag := binary.LittleEndian.Uint32(hdr[:])
	if uint8(tag>>24) != 7 { // MPLEX_BASE + MSG_DATA
		t.Fatalf("frame tag = %#x, want 7 (MSG_DATA)", tag)
	}
	if _, err := io.CopyN(io.Discard, clientConn, int64(tag&0xFFFFFF)); err != nil {
		t.Fatalf("drain flist frame: %v", err)
	}

	// the attack: a transfer selector for the file (ndx 1), then the
	// malicious sum head -- count=1, blength=0 (proto >= 27: 16 bytes
	// including s2length)
	int32 := func(v int32) []byte {
		var b [4]byte
		binary.LittleEndian.PutUint32(b[:], uint32(v))
		return b[:]
	}
	mustWrite(int32(1))                                                                 // ndx of "f"; proto < 29 defaults iflags to a transfer
	mustWrite(append(append(append(int32(1), int32(0)...), int32(16)...), int32(0)...)) // count=1, blength=0, s2length=16, remainder=0

	select {
	case err := <-done:
		return s, err
	case <-upstreamTestTimeout(t):
		t.Fatal("server goroutine did not exit")
		return s, nil
	}
}

func TestUpstream_ChecksumZeroBlocklen(t *testing.T) {
	s, err := runZeroBlocklenTransfer(t)
	if err == nil {
		t.Fatal("server accepted a count>0 / blength=0 checksum header and completed the connection")
	}
	if !strings.Contains(err.Error(), "invalid zero block length") {
		t.Fatalf("server rejected the header, but not via the zero-block guard: %v", err)
	}

	// the Server is stateless across connections: the same instance must
	// still serve a clean #list afterwards
	serverConn, clientConn := net.Pipe()
	done := make(chan error, 1)
	go func() {
		defer serverConn.Close()
		done <- s.HandleConnection(serverConn)
	}()
	defer clientConn.Close()
	if g, _ := readLine(clientConn); !strings.HasPrefix(g, "@RSYNCD: ") {
		t.Fatalf("server greeting = %q", g)
	}
	clientConn.Write([]byte("@RSYNCD: 27\n"))
	clientConn.Write([]byte("#list\n"))
	// the module list is a line per module plus the exit marker; the
	// connection only unwinds once the client side is closed
	if l, err := readLine(clientConn); err != nil || !strings.HasPrefix(l, "mod") {
		t.Fatalf("module list = %q (err %v), want a line starting with \"mod\"", l, err)
	}
	if l, err := readLine(clientConn); err != nil || l != "@RSYNCD: EXIT" {
		t.Fatalf("module list terminator = %q (err %v), want @RSYNCD: EXIT", l, err)
	}
	clientConn.Close()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Server not reusable after the malicious connection: %v", err)
		}
	case <-upstreamTestTimeout(t):
		t.Fatal("server goroutine did not exit")
	}
}

// TestUpstream_ChecksumZeroBlocklen_SumHeadGuards pins the full guard set the ported read_sum_head now carries, straight at the byte level.
func TestUpstream_ChecksumZeroBlocklen_SumHeadGuards(t *testing.T) {
	head := func(count, blength, s2length, remainder int32) []byte {
		var b [16]byte
		binary.LittleEndian.PutUint32(b[0:4], uint32(count))
		binary.LittleEndian.PutUint32(b[4:8], uint32(blength))
		binary.LittleEndian.PutUint32(b[8:12], uint32(s2length))
		binary.LittleEndian.PutUint32(b[12:16], uint32(remainder))
		return b[:]
	}

	cases := []struct {
		name    string
		version int
		b       []byte
		want    string // substring; empty = must parse
	}{
		{"zero head is a legal null sum", 32, head(0, 0, 0, 0), ""},
		{"normal head parses", 32, head(4, 700, 16, 100), ""},
		{"negative count", 32, head(-1, 700, 16, 0), "invalid checksum count"},
		{"negative block length", 32, head(1, -1, 16, 0), "invalid block length"},
		{"block length past MAX_BLOCK_SIZE (proto >= 30)", 32, head(1, 1<<17+1, 16, 0), "invalid block length"},
		{"block length at OLD_MAX_BLOCK_SIZE (proto < 30)", 27, head(1, 1<<29, 16, 0), ""},
		{"block length past OLD_MAX_BLOCK_SIZE (proto < 30)", 27, head(1, 1<<29+1, 16, 0), "invalid block length"},
		{"count>0 with zero block length", 32, head(1, 0, 16, 0), "invalid zero block length"},
		{"count>0 with zero block length (proto < 27, 12-byte head)", 26, head(1, 0, 0, 0)[:12], "invalid zero block length"},
		{"count*blength past an int32 file length", 32, head(1<<30, 4, 16, 0), "count*block length exceeds a file length"},
		{"count*blength at the int32 boundary", 32, head(1073741823, 2, 16, 0), ""},
		{"s2length past the digest length", 32, head(1, 700, 32, 0), "invalid checksum length"},
		{"negative s2length", 32, head(1, 700, -1, 0), "invalid checksum length"},
	}

	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			sh, err := protocol.ReadSumHead(bytes.NewReader(tt.b), tt.version)
			if tt.want == "" {
				if err != nil {
					t.Fatalf("ReadSumHead: %v (head=%+v)", err, sh)
				}
				return
			}
			if err == nil {
				t.Fatalf("ReadSumHead accepted %s: %+v", tt.name, sh)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("ReadSumHead error = %q, want substring %q", err, tt.want)
			}
		})
	}
}

// TestUpstream_ChecksumZeroBlocklen_Truncated: a truncated stream after a bad head must report an EOF, not a guard or a panic.
func TestUpstream_ChecksumZeroBlocklen_Truncated(t *testing.T) {
	var b [4]byte
	binary.LittleEndian.PutUint32(b[:], 1) // count = 1
	_, err := protocol.ReadSumHead(bytes.NewReader(b[:]), 32)
	if err == nil {
		t.Fatal("ReadSumHead on a truncated head must not succeed")
	}
	if !errors.Is(err, io.ErrUnexpectedEOF) && !errors.Is(err, io.EOF) {
		t.Fatalf("ReadSumHead error = %v, want an EOF flavor", err)
	}
}
