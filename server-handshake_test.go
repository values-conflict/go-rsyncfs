package rsyncfs

import (
	"bytes"
	"encoding/binary"
	"io"
	"net"
	"testing"
	"testing/fstest"
	"time"

	"golang.org/x/crypto/md4"

	"github.com/values-conflict/go-rsyncfs/protocol"
)

func TestHandleConnection_ModuleList(t *testing.T) {
	mod1 := &ServerModule{Name: "mod1", Comment: "Comment 1", FS: fstest.MapFS{}}
	mod2 := &ServerModule{Name: "mod2", Comment: "Comment 2", FS: fstest.MapFS{}}
	s, err := NewServer(mod1, mod2)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}

	serverConn, clientConn := net.Pipe()
	done := make(chan error, 1)
	go func() {
		defer serverConn.Close()
		done <- s.HandleConnection(serverConn)
	}()

	// read server greeting
	greet, err := readLine(clientConn)
	if err != nil {
		t.Fatalf("read greeting: %v", err)
	}
	if _, err := protocol.ParseGreeting(greet); err != nil {
		t.Fatalf("parse server greeting: %v", err)
	}

	// send client greeting
	_, _ = clientConn.Write([]byte("@RSYNCD: 32.0 md5\n"))

	// send #list request
	_, _ = clientConn.Write([]byte("#list\n"))

	// read module listing
	var listing bytes.Buffer
	_, err = io.Copy(&listing, clientConn)
	if err != nil && err.Error() != "read |0: file already closed" {
		t.Logf("copy listing: %v", err)
	}
	clientConn.Close()

	// wait for server to finish
	select {
	case err = <-done:
		if err != nil {
			t.Logf("server returned: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("server goroutine did not exit")
	}

	if !bytes.Contains(listing.Bytes(), []byte("@RSYNCD: EXIT")) {
		t.Error("module list missing @RSYNCD: EXIT terminator")
	}
	if !bytes.Contains(listing.Bytes(), []byte("mod1")) {
		t.Error("module list missing mod1")
	}
	if !bytes.Contains(listing.Bytes(), []byte("mod2")) {
		t.Error("module list missing mod2")
	}
}

func TestHandleConnection_UnknownModule(t *testing.T) {
	mod := &ServerModule{Name: "testmod", FS: fstest.MapFS{}}
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

	// read server greeting
	_, _ = readLine(clientConn)

	// send client greeting
	_, _ = clientConn.Write([]byte("@RSYNCD: 32.0 md5\n"))

	// send unknown module
	_, _ = clientConn.Write([]byte("nonexistent\n"))

	// read error response
	line, _ := readLine(clientConn)
	clientConn.Close()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("server goroutine did not exit")
	}

	if !bytes.Contains([]byte(line), []byte("@ERROR:")) {
		t.Fatalf("expected @ERROR response, got %q", line)
	}
}

func TestHandleConnection_AuthSuccess(t *testing.T) {
	mod := &ServerModule{
		Name: "testmod",
		FS:   fstest.MapFS{},
		AuthCallback: func(username string, challenge []byte) ([]byte, error) {
			if username != "testuser" {
				return nil, nil
			}
			return challenge, nil // echo challenge as expected hash
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

	// read server greeting
	_, _ = readLine(clientConn)

	// send client greeting
	_, _ = clientConn.Write([]byte("@RSYNCD: 32.0 md5\n"))

	// send module request
	_, _ = clientConn.Write([]byte("testmod\n"))

	// read auth challenge
	line, err := readLine(clientConn)
	if err != nil {
		t.Fatalf("read auth challenge: %v", err)
	}
	if !bytes.Contains([]byte(line), []byte("@RSYNCD: AUTHREQD")) {
		t.Fatalf("expected AUTHREQD, got %q", line)
	}

	// parse challenge
	parts := bytes.SplitN([]byte(line), []byte(" "), 3)
	if len(parts) < 3 {
		t.Fatalf("invalid AUTHREQD format: %q", line)
	}
	challengeB64 := string(bytes.TrimSpace(parts[2]))

	// compute auth response (echo challenge as hash)
	_, _ = clientConn.Write([]byte("testuser " + challengeB64 + "\n"))

	// read auth result
	line, err = readLine(clientConn)
	if err != nil {
		t.Fatalf("read auth result: %v", err)
	}
	if line != "@RSYNCD: OK" {
		t.Fatalf("expected '@RSYNCD: OK', got %q", line)
	}

	// send arguments (proto 30+, null-terminated)
	// no 'v' in client_info to skip algorithm negotiation (avoids net.Pipe deadlock)
	_, _ = clientConn.Write([]byte(".\x00--server\x00--sender\x00-vlogDtpre.iLsfxCIu\x00.\x00\x00"))

	// read compat flags varint
	compatFlags, err := protocol.ReadVarint(clientConn)
	if err != nil {
		t.Fatalf("read compat flags: %v", err)
	}
	t.Logf("compat flags: 0x%x", compatFlags)

	// without 'v' flag, no algorithm negotiation -- defaults used
	// read checksum seed
	var seedBuf [4]byte
	if _, err := io.ReadFull(clientConn, seedBuf[:]); err != nil {
		t.Fatalf("read checksum seed: %v", err)
	}
	t.Logf("checksum seed: 0x%x", seedBuf)

	clientConn.Close()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("server goroutine did not exit")
	}
}

func TestHandleConnection_AuthFailure(t *testing.T) {
	mod := &ServerModule{
		Name: "testmod",
		FS:   fstest.MapFS{},
		AuthCallback: func(username string, challenge []byte) ([]byte, error) {
			return nil, nil // always fail
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

	// read server greeting
	_, _ = readLine(clientConn)

	// send client greeting
	_, _ = clientConn.Write([]byte("@RSYNCD: 32.0 md5\n"))

	// send module request
	_, _ = clientConn.Write([]byte("testmod\n"))

	// read auth challenge
	_, _ = readLine(clientConn)

	// send bad auth response
	_, _ = clientConn.Write([]byte("testuser d3JvbmdoYXNo\n"))

	// read error response
	line, _ := readLine(clientConn)
	clientConn.Close()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("server goroutine did not exit")
	}

	if !bytes.Contains([]byte(line), []byte("@ERROR:")) {
		t.Fatalf("expected @ERROR, got %q", line)
	}
}

func TestHandleConnection_NoAuth(t *testing.T) {
	mod := &ServerModule{Name: "openmod", FS: fstest.MapFS{}}
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

	// read server greeting
	_, _ = readLine(clientConn)

	// send client greeting
	_, _ = clientConn.Write([]byte("@RSYNCD: 32.0 md5\n"))

	// send module request
	_, _ = clientConn.Write([]byte("openmod\n"))

	// should get OK directly (no auth challenge)
	line, err := readLine(clientConn)
	if err != nil {
		t.Fatalf("read auth response: %v", err)
	}
	if line != "@RSYNCD: OK" {
		t.Fatalf("expected '@RSYNCD: OK', got %q", line)
	}

	clientConn.Close()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("server goroutine did not exit")
	}
}

func TestHandleConnection_ClientDisconnect(t *testing.T) {
	mod := &ServerModule{Name: "testmod", FS: fstest.MapFS{}}
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

	// close client side immediately -- server should get EOF and return
	clientConn.Close()

	select {
	case err := <-done:
		// server returned (error expected due to abrupt disconnect)
		if err == nil {
			t.Log("server returned nil on client disconnect")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("server goroutine did not exit after client disconnect")
	}
}

func TestHandleConnection_VersionNegotiation(t *testing.T) {
	mod := &ServerModule{Name: "testmod", FS: fstest.MapFS{}}
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

	// read server greeting
	_, _ = readLine(clientConn)

	// send client greeting with older version
	_, _ = clientConn.Write([]byte("@RSYNCD: 30.0 md5\n"))

	// send module request
	_, _ = clientConn.Write([]byte("testmod\n"))

	// should get OK
	line, err := readLine(clientConn)
	if err != nil {
		t.Fatalf("read auth response: %v", err)
	}
	if line != "@RSYNCD: OK" {
		t.Fatalf("expected '@RSYNCD: OK', got %q", line)
	}

	// send arguments (proto 30+, null-terminated, no 'v' flag to skip algorithm negotiation)
	_, _ = clientConn.Write([]byte(".\x00--server\x00--sender\x00-vlogDtpre.iLsfxCIu\x00.\x00\x00"))

	// read compat flags
	_, err = protocol.ReadVarint(clientConn)
	if err != nil {
		t.Fatalf("read compat flags: %v", err)
	}

	// read checksum seed
	var seedBuf [4]byte
	if _, err := io.ReadFull(clientConn, seedBuf[:]); err != nil {
		t.Fatalf("read checksum seed: %v", err)
	}

	clientConn.Close()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("server goroutine did not exit")
	}
}

func TestHandleConnection_AlgorithmNegotiation(t *testing.T) {
	// test the CF_VARINT_FLIST_FLAGS path with algorithm negotiation
	// uses a custom bidirectional pipe to avoid net.Pipe deadlock
	mod := &ServerModule{Name: "testmod", FS: fstest.MapFS{}}
	s, err := NewServer(mod)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}

	serverRW, clientRW := net.Pipe()

	done := make(chan error, 1)
	go func() {
		defer serverRW.Close()
		done <- s.HandleConnection(serverRW)
	}()

	// read server greeting
	_, _ = readLine(clientRW)

	// send client greeting
	_, _ = clientRW.Write([]byte("@RSYNCD: 32.0 md5\n"))

	// send module request
	_, _ = clientRW.Write([]byte("testmod\n"))

	// no auth for this module
	line, _ := readLine(clientRW)
	if line != "@RSYNCD: OK" {
		t.Fatalf("expected '@RSYNCD: OK', got %q", line)
	}

	// send arguments WITH 'v' flag in client_info (triggers algorithm negotiation)
	_, _ = clientRW.Write([]byte(".\x00--server\x00--sender\x00-vlogDtpre.iLsfxCIvu\x00.\x00\x00"))

	// read compat flags
	compatFlags, err := protocol.ReadVarint(clientRW)
	if err != nil {
		t.Fatalf("read compat flags: %v", err)
	}
	if compatFlags&protocol.CompatVarintFlistFlags == 0 {
		t.Error("expected CF_VARINT_FLIST_FLAGS in compat flags")
	}

	// algorithm negotiation: read server's checksum list, send client's list
	// must read server's first to avoid deadlock (server sends first)
	serverChecksums, err := protocol.ReadVstring(clientRW)
	if err != nil {
		t.Fatalf("read server checksum list: %v", err)
	}
	t.Logf("server checksums: %s", serverChecksums)

	// send client's checksum list
	_ = protocol.WriteVstring(clientRW, "md5 md4")

	// read checksum seed
	var seedBuf [4]byte
	if _, err := io.ReadFull(clientRW, seedBuf[:]); err != nil {
		t.Fatalf("read checksum seed: %v", err)
	}
	t.Logf("checksum seed: 0x%x", seedBuf)

	clientRW.Close()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("server goroutine did not exit")
	}
}

func TestHandleConnection_OldProtocol(t *testing.T) {
	mod := &ServerModule{Name: "testmod", FS: fstest.MapFS{}}
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

	// greeting exchange
	_, _ = readLine(clientConn)
	_, _ = clientConn.Write([]byte("@RSYNCD: 27\n"))

	// module selection
	_, _ = clientConn.Write([]byte("testmod\n"))
	line, err := readLine(clientConn)
	if err != nil {
		t.Fatalf("read auth response: %v", err)
	}
	if line != "@RSYNCD: OK" {
		t.Fatalf("expected '@RSYNCD: OK', got %q", line)
	}

	// arguments (proto < 30, newline-terminated, doubled to end)
	_, _ = clientConn.Write([]byte(".\n--server\n--sender\n-vlogDtpre\n.\n\n"))

	// no compat flags and no algorithm negotiation for proto < 30, but
	// the checksum seed is written on every protocol version
	var seedBuf [4]byte
	if _, err := io.ReadFull(clientConn, seedBuf[:]); err != nil {
		t.Fatalf("read checksum seed: %v", err)
	}
	if binary.LittleEndian.Uint32(seedBuf[:]) == 0 {
		t.Fatal("checksum seed must be non-zero")
	}

	// the legacy exclude list: a single zero length ends it
	if _, err := clientConn.Write([]byte{0, 0, 0, 0}); err != nil {
		t.Fatalf("write exclude list: %v", err)
	}

	// from here on the daemon's output is legacy-multiplexed (proto
	// 23-29: only the daemon's stream is framed) while the client's
	// stays plain bytes.  readFrame pulls one MSG_DATA frame.
	readFrame := func(wantLen int) []byte {
		t.Helper()
		var hdr [4]byte
		if _, err := io.ReadFull(clientConn, hdr[:]); err != nil {
			t.Fatalf("read frame header: %v", err)
		}
		tag := binary.LittleEndian.Uint32(hdr[:])
		if uint8(tag>>24) != 7 { // MPLEX_BASE
			t.Fatalf("frame tag = %d, want 7 (MSG_DATA)", tag>>24)
		}
		payload := make([]byte, tag&0xFFFFFF)
		if _, err := io.ReadFull(clientConn, payload); err != nil {
			t.Fatalf("read frame payload: %v", err)
		}
		if wantLen >= 0 && len(payload) != wantLen {
			t.Fatalf("frame payload = %d bytes, want %d: % x", len(payload), wantLen, payload)
		}
		return payload
	}
	int32le := func(b []byte, off int) int32 {
		return int32(binary.LittleEndian.Uint32(b[off : off+4]))
	}

	// the file list frame: one entry ("."), the end marker, the uid/gid
	// list terminators (the -o/-g in the args), and the io_error
	// trailer.  The entry layout for proto 27 is: xflags byte, l2 byte,
	// name, longint size, int32 mtime, int32 mode, then the preserved
	// uid and gid as int32 each.
	flist := readFrame(-1)
	const flistLen = 36
	if len(flist) != flistLen {
		t.Fatalf("flist frame = %d bytes, want %d: % x", len(flist), flistLen, flist)
	}
	want := [3]byte{0x01, 0x01, 0x2e} // TOP_DIR, l2=1, "."
	if !bytes.Equal(flist[:3], want[:]) {
		t.Fatalf("flist entry header = % x, want % x", flist[:3], want[:])
	}
	if v := int32le(flist, 3); v != 0 {
		t.Fatalf("entry size = %d, want 0", v)
	}
	mode := int32le(flist, 11)
	if uint32(mode) != 0o040555 { // S_IFDIR | 0555 (MapFS root)
		t.Fatalf("entry mode = %#o, want %#o", uint32(mode), 0o040555)
	}
	t.Logf("flist entry: uid=%d gid=%d mode=%#o", int32le(flist, 15), int32le(flist, 19), uint32(mode))
	if flist[23] != 0 { // end marker: a single zero byte
		t.Fatalf("flist end marker = %d, want 0", flist[23])
	}
	for off, what := range map[int]string{24: "uid list terminator", 28: "gid list terminator", 32: "io_error trailer"} {
		if v := int32le(flist, off); v != 0 {
			t.Fatalf("%s = %d, want 0", what, v)
		}
	}

	// selector loop: the generator sends a (null) sum struct for the
	// directory item, gets it echoed back with an empty body, and then
	// finishes with two NDX_DONE packets and the final goodbye.
	writeInt32 := func(v int32) {
		t.Helper()
		var b [4]byte
		binary.LittleEndian.PutUint32(b[:], uint32(v))
		if _, err := clientConn.Write(b[:]); err != nil {
			t.Fatalf("write int32: %v", err)
		}
	}

	writeInt32(0) // ndx of "."
	for i := 0; i < 4; i++ {
		writeInt32(0) // null sum head (proto 27)
	}

	// the response frame: echoed ndx, echoed sum head, the delta end
	// token, and the 16-byte whole-file checksum
	resp := readFrame(40)
	if v := int32le(resp, 0); v != 0 {
		t.Fatalf("echoed ndx = %d, want 0", v)
	}
	for off := 4; off < 20; off += 4 {
		if v := int32le(resp, off); v != 0 {
			t.Fatalf("echoed sum head %d = %d, want 0", (off-4)/4, v)
		}
	}
	if v := int32le(resp, 20); v != 0 {
		t.Fatalf("delta end token = %d, want 0", v)
	}
	// below proto 30 the whole-file checksum is the legacy streaming
	// MD4, which feeds the seed into the context before the data --
	// for a directory item (no data) that is just the digest of the
	// seed bytes
	var wantSum [16]byte
	md4h := md4.New()
	md4h.Write(seedBuf[:])
	copy(wantSum[:], md4h.Sum(nil))
	if !bytes.Equal(resp[24:], wantSum[:]) {
		t.Fatalf("file checksum = % x, want % x", resp[24:], wantSum[:])
	}

	writeInt32(-1) // phase 0 done
	if v := int32le(readFrame(4), 0); v != -1 {
		t.Fatalf("NDX_DONE echo = %d, want -1", v)
	}
	writeInt32(-1) // redo phase done
	if v := int32le(readFrame(4), 0); v != -1 {
		t.Fatalf("post-loop NDX_DONE = %d, want -1", v)
	}

	// stats frame: three longints (proto < 29)
	stats := readFrame(12)
	for off, want := range []int32{0, 0, 0} {
		if v := int32le(stats, off*4); v != want {
			t.Fatalf("stat %d = %d, want %d", off, v, want)
		}
	}

	writeInt32(-1) // final goodbye

	clientConn.Close()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("HandleConnection: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("server goroutine did not exit")
	}
}

// readLine reads from r until a newline character is encountered.
func readLine(r io.Reader) (string, error) {
	var buf []byte
	for {
		b := make([]byte, 1)
		n, err := r.Read(b)
		if err != nil {
			return string(buf), err
		}
		if n == 0 {
			continue
		}
		if b[0] == '\n' {
			return string(buf), nil
		}
		buf = append(buf, b[0])
	}
}
