package rsyncfs

import (
	"bytes"
	"io"
	"net"
	"testing"
	"testing/fstest"
	"time"

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

	serverRW, clientRW := bidirectionalPipe()

	done := make(chan error, 1)
	go func() {
		defer serverRW.close()
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

	clientRW.close()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("server goroutine did not exit")
	}
}

// rwCloser is an io.ReadWriter that can be closed.
type rwCloser interface {
	io.ReadWriter
	close() error
}

// bidirectionalPipe creates two connected rwClosers using io.Pipe.
func bidirectionalPipe() (server, client rwCloser) {
	c2sR, c2sW := io.Pipe() // client→server
	s2cR, s2cW := io.Pipe() // server→client

	return &pipeRW{r: s2cR, w: c2sW}, &pipeRW{r: c2sR, w: s2cW}
}

type pipeRW struct {
	r *io.PipeReader
	w *io.PipeWriter
}

func (p *pipeRW) Read(data []byte) (int, error)    { return p.r.Read(data) }
func (p *pipeRW) Write(data []byte) (int, error)   { return p.w.Write(data) }
func (p *pipeRW) close() error                     { p.r.Close(); p.w.Close(); return nil }

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

	// read server greeting
	_, _ = readLine(clientConn)

	// send client greeting with proto 27 (no compat flags, newline args)
	_, _ = clientConn.Write([]byte("@RSYNCD: 27.0 md4\n"))

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

	// send arguments (proto < 30, newline-terminated)
	_, _ = clientConn.Write([]byte(".\n--server\n--sender\n-v\nlogDtpre\n.\n\n"))

	// no compat flags for proto < 30
	// read checksum seed (4 bytes LE)
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
