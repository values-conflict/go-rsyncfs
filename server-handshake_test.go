package rsyncfs

import (
	"bytes"
	"io"
	"net"
	"testing"
	"testing/fstest"
	"time"
)

// mockRW implements io.ReadWriter with buffered read/write for testing.
type mockRW struct {
	readBuf  *bytes.Buffer
	writeBuf *bytes.Buffer
}

func (m *mockRW) Read(p []byte) (int, error) {
	return m.readBuf.Read(p)
}

func (m *mockRW) Write(p []byte) (int, error) {
	return m.writeBuf.Write(p)
}

func TestHandleConnection_ModuleList(t *testing.T) {
	mod1 := &ServerModule{Name: "mod1", Comment: "Comment 1", FS: fstest.MapFS{}}
	mod2 := &ServerModule{Name: "mod2", Comment: "Comment 2", FS: fstest.MapFS{}}
	s, _ := NewServer(mod1, mod2)

	rw := &mockRW{readBuf: bytes.NewBuffer(nil), writeBuf: bytes.NewBuffer(nil)}

	// simulate client greeting + #list request
	rw.readBuf.WriteString("@RSYNCD: 32.0 md5\n")
	rw.readBuf.WriteString("#list\n")

	opts := HandleOptions{}

	err := s.HandleConnection(rw, opts)
	if err != nil {
		t.Fatalf("HandleConnection failed: %v", err)
	}

	if !bytes.Contains(rw.writeBuf.Bytes(), []byte("@RSYNCD: EXIT")) {
		t.Error("Module list response missing @RSYNCD: EXIT terminator")
	}
	if !bytes.Contains(rw.writeBuf.Bytes(), []byte("mod1")) {
		t.Error("Module list missing mod1")
	}
	if !bytes.Contains(rw.writeBuf.Bytes(), []byte("mod2")) {
		t.Error("Module list missing mod2")
	}
}

func TestHandleConnection_UnknownModule(t *testing.T) {
	mod := &ServerModule{Name: "testmod", FS: fstest.MapFS{}}
	s, _ := NewServer(mod)

	rw := &mockRW{readBuf: bytes.NewBuffer(nil), writeBuf: bytes.NewBuffer(nil)}

	rw.readBuf.WriteString("@RSYNCD: 32.0 md5\n")
	rw.readBuf.WriteString("nonexistent\n")

	opts := HandleOptions{}

	err := s.HandleConnection(rw, opts)
	if err == nil {
		t.Fatal("Expected error for unknown module")
	}
	if !bytes.Contains(rw.writeBuf.Bytes(), []byte("@ERROR:")) {
		t.Error("Expected @ERROR response for unknown module")
	}
}

func TestHandleConnection_AuthSuccess(t *testing.T) {
	mod := &ServerModule{Name: "testmod", FS: fstest.MapFS{}}
	s, _ := NewServer(mod)

	serverConn, clientConn := net.Pipe()

	opts := HandleOptions{
		AuthCallback: func(username string, challenge []byte) ([]byte, error) {
			if username != "testuser" {
				return nil, nil
			}
			return challenge, nil
		},
	}

	go func() {
		defer serverConn.Close()
		_ = s.HandleConnection(serverConn, opts)
	}()

	// read server greeting
	line := readLineFrom(clientConn)
	if line == "" {
		t.Fatal("Failed to read server greeting")
	}

	// send client greeting
	_, _ = clientConn.Write([]byte("@RSYNCD: 32.0 md5\n"))

	// send module request
	_, _ = clientConn.Write([]byte("testmod\n"))

	// read auth challenge
	line = readLineFrom(clientConn)
	if line == "" {
		t.Fatal("Failed to read auth challenge")
	}
	if !bytes.Contains([]byte(line), []byte("@RSYNCD: AUTHREQD")) {
		t.Fatalf("Expected AUTHREQD, got %q", line)
	}

	// send auth response (echo challenge as hash)
	parts := bytes.SplitN([]byte(line), []byte(" "), 3)
	if len(parts) < 3 {
		t.Fatalf("Invalid AUTHREQD format: %q", line)
	}
	challengeB64 := string(bytes.TrimSpace(parts[2]))
	_, _ = clientConn.Write([]byte("testuser " + challengeB64 + "\n"))

	// read auth result
	line = readLineFrom(clientConn)
	if line == "" {
		t.Fatal("Failed to read auth result")
	}
	if line != "@RSYNCD: OK" {
		t.Fatalf("Expected '@RSYNCD: OK', got %q", line)
	}

	// send arguments (proto 30+)
	_, _ = clientConn.Write([]byte(".\x00\x00"))

	// read protocol version (4 bytes)
	var verBuf [4]byte
	_, err := io.ReadFull(clientConn, verBuf[:])
	if err != nil {
		t.Fatalf("Failed to read protocol version: %v", err)
	}

	// send protocol version back
	_, _ = clientConn.Write(verBuf[:])

	clientConn.Close()
}

func TestHandleConnection_AuthFailure(t *testing.T) {
	mod := &ServerModule{Name: "testmod", FS: fstest.MapFS{}}
	s, _ := NewServer(mod)

	serverConn, clientConn := net.Pipe()

	opts := HandleOptions{
		AuthCallback: func(username string, challenge []byte) ([]byte, error) {
			return nil, nil // always fail
		},
	}

	go func() {
		defer serverConn.Close()
		_ = s.HandleConnection(serverConn, opts)
	}()

	// read server greeting
	line := readLineFrom(clientConn)
	if line == "" {
		t.Fatal("Failed to read server greeting")
	}

	// send client greeting
	_, _ = clientConn.Write([]byte("@RSYNCD: 32.0 md5\n"))

	// send module request
	_, _ = clientConn.Write([]byte("testmod\n"))

	// read auth challenge
	line = readLineFrom(clientConn)
	if line == "" {
		t.Fatal("Failed to read auth challenge")
	}

	// send bad auth response
	_, _ = clientConn.Write([]byte("testuser wronghash\n"))

	// read auth result
	line = readLineFrom(clientConn)
	if line == "" {
		t.Fatal("Failed to read auth result")
	}
	if !bytes.Contains([]byte(line), []byte("@ERROR:")) {
		t.Fatalf("Expected @ERROR, got %q", line)
	}

	clientConn.Close()
}

func TestHandleConnection_ClientDisconnect(t *testing.T) {
	mod := &ServerModule{Name: "testmod", FS: fstest.MapFS{}}
	s, _ := NewServer(mod)

	serverConn, clientConn := net.Pipe()

	done := make(chan error, 1)
	go func() {
		defer serverConn.Close()
		done <- s.HandleConnection(serverConn, HandleOptions{})
	}()

	// close client side -- server should get EOF and return
	clientConn.Close()

	select {
	case err := <-done:
		// server returned cleanly (error is expected due to abrupt disconnect)
		_ = err
	case <-time.After(5 * time.Second):
		t.Fatal("server goroutine did not exit after client disconnect")
	}
}

func readLineFrom(r io.Reader) string {
	var buf []byte
	b := make([]byte, 1)
	for {
		n, err := r.Read(b)
		if err != nil || n == 0 {
			return string(buf)
		}
		if b[0] == '\n' {
			return string(buf)
		}
		buf = append(buf, b[0])
	}
}
