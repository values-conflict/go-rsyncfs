package rsyncfs

// Port of the upstream testsuite test proto-msg-info-assert_test.py (upstream rsync @ 471e17dc, "Preparing for release of 3.5.0").
//
// What the upstream test catches: a remotely-reachable abort in rwrite() (log.c).  A daemon receiver child that receives a MSG_INFO/MSG_ERROR frame from the wire called rwrite() with is_utf8 = !am_generator = 1, and the send_msgs_to_gen branch used to assert(!is_utf8) -- so a peer that sent such a message to the receiver triggered a SIGABRT of the per-connection child.  Legitimate senders never put these tags on the wire.  The fix drops the assert: the message is forwarded to the sibling, logged, and the data stream simply continues -- control frames interleaved with data are not protocol violations.
//
// Role mapping: upstream injects a MSG_INFO into the daemon receiver's input stream; we inject the same frame into our Server's selector stream (proto-30, both channels multiplexed, exactly as the upstream test runs at greeting_version=30).  The ported rule lives in the mux layer, which plays the role of upstream's iobuf/read_a_msg: a data reader must skip peer logging / no-op control frames (MSG_INFO and siblings) rather than treat them as protocol errors.  The unique marker payload from the upstream test rides along so the oracle can prove the frame was consumed as a frame -- not mangled into the data stream.
//
// Oracle: the injection does not abort the connection -- the file transfer completes byte-for-byte before the frame, the whole connection (phase exchange, stats, final goodbye) runs to a clean nil error after it, and the server remains usable for the next connection.  A pre-fix reader fails the connection at the next selector read with "unexpected non-DATA message code".

import (
	"bytes"
	"crypto/md5"
	"encoding/binary"
	"io"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/values-conflict/go-rsyncfs/protocol"
	"github.com/values-conflict/go-rsyncfs/protocol/mux"
)

const msgInfoMarker = "SCANNER-0009-FORWARDED\n"

// runMsgInfoTransfer is the full proto-30 connection: handshake, file list, one benign transfer, the MSG_INFO injection mid-protocol, and the rest of the session to completion.  It returns the error HandleConnection reported (nil = the injection was tolerated) and the pulled file bytes.
func runMsgInfoTransfer(t *testing.T) (*Server, error, []byte) {
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

	// BufPipe rather than net.Pipe: the algorithm negotiation has both
	// sides send their vstring before reading the peer's, so a
	// zero-capacity pipe deadlocks on the handshake
	serverConn, clientConn := BufPipe()
	done := make(chan error, 1)
	go func() {
		defer serverConn.Close()
		done <- s.HandleConnection(serverConn)
	}()
	defer clientConn.Close()
	mustWrite := func(b []byte) {
		t.Helper()
		if _, err := clientConn.Write(b); err != nil {
			t.Fatalf("write: %v", err)
		}
	}
	mustRead := func(n int) []byte {
		t.Helper()
		b := make([]byte, n)
		if _, err := io.ReadFull(clientConn, b); err != nil {
			t.Fatalf("read %d: %v", n, err)
		}
		return b
	}

	// handshake: proto 30, no auth, NUL-terminated args with the -e
	// client_info (so CF_VARINT_FLIST_FLAGS is negotiated and the
	// algorithm vstrings are exchanged), compat flags, vstrings, seed
	mustWrite([]byte("@RSYNCD: 30.0 md5 md4\n"))
	if g, err := readLine(clientConn); err != nil || !strings.HasPrefix(g, "@RSYNCD: ") {
		t.Fatalf("server greeting: %q (%v)", g, err)
	}
	mustWrite([]byte("mod\n"))
	if l, err := readLine(clientConn); err != nil || l != "@RSYNCD: OK" {
		t.Fatalf("auth response: %q (%v)", l, err)
	}
	mustWrite([]byte("--server\x00--sender\x00-logDtpre.fxvC\x00.\x00mod/\x00\x00"))
	compat, err := protocol.ReadCompatFlags(clientConn, 30)
	if err != nil {
		t.Fatalf("read compat flags: %v", err)
	}
	if compat&protocol.CompatVarintFlistFlags == 0 {
		t.Fatalf("compat flags = %#x, want CF_VARINT_FLIST_FLAGS", compat)
	}
	// the client sends its checksum list before reading the server's
	// (both sides send first; BufPipe carries the crossing writes)
	if err := protocol.WriteVstring(clientConn, "md5 md4"); err != nil {
		t.Fatalf("write checksum list: %v", err)
	}
	if _, err := protocol.ReadVstring(clientConn); err != nil {
		t.Fatalf("read server checksum list: %v", err)
	}
	mustRead(4) // checksum seed (raw, pre-mux)

	// from here the client's output is multiplexed: selectors, sum
	// heads, and the phase exchange all ride MSG_DATA frames
	mw := mux.NewWriter(clientConn)
	mr := mux.NewReader(clientConn)
	writeMux := func(b []byte) {
		t.Helper()
		if _, err := mw.Write(b); err != nil {
			t.Fatalf("mux write: %v", err)
		}
		if err := mw.Flush(); err != nil {
			t.Fatalf("mux flush: %v", err)
		}
	}

	// filter list: a single int32(0)
	writeMux([]byte{0, 0, 0, 0})
	// the file list frame: drain it (its layout is covered by the
	// proto-27 test; here we only need the stream positioned)
	if _, err := mr.ReadDataChunk(); err != nil {
		t.Fatalf("read flist frame: %v", err)
	}

	// the transfer: selector for ndx 1 (compressed NDX: the first
	// positive value starts the delta chain at -1, so ndx 1 encodes as
	// the single byte 0x02; iflags uint16 = ITEM_TRANSFER) + a null
	// sum head (16 zero bytes)
	var selBuf bytes.Buffer
	selBuf.WriteByte(0x02) // NDX 1
	var iflags [2]byte
	binary.LittleEndian.PutUint16(iflags[:], uint16(protocol.ItemTransfer))
	selBuf.Write(iflags[:])
	selBuf.Write(make([]byte, 16)) // null sum head
	writeMux(selBuf.Bytes())

	// the response: echoed selector, echoed null sum head, the whole file
	// as one literal token (null sums -> whole-file literal), the end
	// token, and the 16-byte whole-file checksum
	resp, err := mr.ReadDataChunk()
	if err != nil {
		t.Fatalf("read transfer response: %v", err)
	}
	rp := bytes.NewReader(resp)
	echoNdx, err := protocol.NewNdxState().ReadNdx(rp)
	if err != nil || echoNdx != 1 {
		t.Fatalf("echoed ndx: %v (%d)", err, echoNdx)
	}
	var echoIflags [2]byte
	if _, err := io.ReadFull(rp, echoIflags[:]); err != nil {
		t.Fatalf("read echoed iflags: %v", err)
	}
	if binary.LittleEndian.Uint16(echoIflags[:]) != uint16(protocol.ItemTransfer) {
		t.Fatalf("echoed iflags = %#x", binary.LittleEndian.Uint16(echoIflags[:]))
	}
	_, err = protocol.ReadSumHead(rp, 30)
	if err != nil {
		t.Fatalf("echoed sum head: %v", err)
	}
	var tok [4]byte
	if _, err := io.ReadFull(rp, tok[:]); err != nil {
		t.Fatalf("read literal token: %v", err)
	}
	if n := int32(binary.LittleEndian.Uint32(tok[:])); n != 11 {
		t.Fatalf("literal token = %d, want 11", n)
	}
	data := make([]byte, 11)
	if _, err := io.ReadFull(rp, data); err != nil {
		t.Fatalf("read literal data: %v", err)
	}
	if _, err := io.ReadFull(rp, tok[:]); err != nil {
		t.Fatalf("read end token: %v", err)
	}
	if int32(binary.LittleEndian.Uint32(tok[:])) != 0 {
		t.Fatalf("end token = %d, want 0", int32(binary.LittleEndian.Uint32(tok[:])))
	}
	fileSum := make([]byte, 16)
	if _, err := io.ReadFull(rp, fileSum); err != nil {
		t.Fatalf("read file checksum: %v", err)
	}
	// proto >= 30 servers default to md5, so the whole-file checksum
	// rides the md5 scheme
	h := md5.New()
	h.Write(data)
	want := h.Sum(nil)
	if !bytes.Equal(fileSum, want) {
		t.Fatalf("file checksum = % x, want % x", fileSum, want)
	}

	// the attack: a peer MSG_INFO frame on the selector channel, with the
	// upstream test's marker payload
	var msgHdr [4]byte
	binary.LittleEndian.PutUint32(msgHdr[:], (uint32(7+mux.MsgInfo)<<24)|uint32(len(msgInfoMarker)))
	mustWrite(append(msgHdr[:], msgInfoMarker...))

	// the session must continue as if the frame had not arrived: the
	// three-phase NDX_DONE exchange (proto 30: compressed NDX, one byte
	// each).  The server echoes the first two phase markers and breaks
	// on the third, then writes one trailing NDX_DONE and the stats
	for i := 0; i < 3; i++ {
		writeMux([]byte{0x00}) // NDX_DONE
		if i < 2 {
			if _, err := mr.ReadDataChunk(); err != nil { // echoed NDX_DONE
				t.Fatalf("phase %d: %v", i, err)
			}
		}
	}
	tail, err := mr.ReadDataChunk()
	if err != nil {
		t.Fatalf("read trailing NDX_DONE: %v", err)
	}
	if !bytes.Equal(tail, []byte{0x00}) {
		t.Fatalf("trailing marker = % x, want the single NDX_DONE byte", tail)
	}
	stats, err := mr.ReadDataChunk()
	if err != nil {
		t.Fatalf("read stats frame: %v", err)
	}
	// five varlong30 stats: (0, 0, totalSize, 0, 0) with totalSize = 11.
	// Each varlong30 is at least 3 bytes, so 0 encodes as 00 00 00 and 11
	// as 00 0b 00
	if !bytes.Equal(stats, []byte{0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x0B, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00}) {
		t.Fatalf("stats = % x, want varlong30 (0, 0, 11, 0, 0)", stats)
	}
	writeMux([]byte{0x00}) // final goodbye NDX_DONE (proto 30: single)

	select {
	case err := <-done:
		return s, err, data
	case <-upstreamTestTimeout(t):
		t.Fatal("server goroutine did not exit")
		return s, nil, data
	}
}

func TestUpstream_ProtoMsgInfoAssert(t *testing.T) {
	s, err, data := runMsgInfoTransfer(t)
	if err != nil {
		t.Fatalf("connection aborted by the peer MSG_INFO frame (the ported read_a_msg rule is not in effect): %v", err)
	}
	if string(data) != "hello world" {
		t.Fatalf("pulled data = %q, want the file intact (the frame must not corrupt the data stream)", data)
	}

	// the server is still usable: a clean #list on the same instance
	serverConn, clientConn := BufPipe()
	done := make(chan error, 1)
	go func() {
		defer serverConn.Close()
		done <- s.HandleConnection(serverConn)
	}()
	defer clientConn.Close()
	clientConn.Write([]byte("@RSYNCD: 30.0 md5 md4\n"))
	if g, _ := readLine(clientConn); !strings.HasPrefix(g, "@RSYNCD: ") {
		t.Fatalf("server greeting = %q", g)
	}
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
			t.Fatalf("Server not reusable after the injected message: %v", err)
		}
	case <-upstreamTestTimeout(t):
		t.Fatal("server goroutine did not exit")
	}
}
