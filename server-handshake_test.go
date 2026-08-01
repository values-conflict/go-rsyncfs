package rsyncfs

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"net"
	"strings"
	"testing"

	"github.com/values-conflict/go-rsyncfs/protocol"
)

type mockRW struct {
	readBuf  *bytes.Buffer
	writeBuf *bytes.Buffer
}

func (m *mockRW) Read(p []byte) (n int, err error) {
	return m.readBuf.Read(p)
}

func (m *mockRW) Write(p []byte) (n int, err error) {
	return m.writeBuf.Write(p)
}

// protoVersionBytes returns the protocol version as int32 LE (4 bytes).
func protoVersionBytes(ver int) []byte {
	b := make([]byte, 4)
	binary.LittleEndian.PutUint32(b, uint32(ver))
	return b
}

func TestHandleConnection_BasicSuccess(t *testing.T) {
	mod := &ServerModule{Name: "testmod", Comment: "Test Module"}
	s, _ := NewServer(mod)

	// client greeting + module name + null-terminated args + client protocol version response
	rw := &mockRW{
		readBuf:  bytes.NewBuffer(append([]byte("@RSYNCD: 32.0 md5\ntestmod\n.\x00\x00"), protoVersionBytes(32)...)),
		writeBuf: new(bytes.Buffer),
	}

	opts := HandleOptions{
		LocalGreeting: protocol.Greeting{Version: 32, SubProtocol: 0, Digests: []string{"md5"}},
	}

	res, err := s.HandleConnection(rw, opts)
	if err != nil {
		t.Fatalf("Handshake failed: %v", err)
	}

	if res.Module != mod {
		t.Errorf("Expected module testmod, got %v", res.Module)
	}
	if res.Version != 32 {
		t.Errorf("Expected version 32, got %d", res.Version)
	}
	if res.Digest != "md5" {
		t.Errorf("Expected digest md5, got %s", res.Digest)
	}

	expectedGreeting := "@RSYNCD: 32.0 md5\n"
	if !bytes.HasPrefix(rw.writeBuf.Bytes(), []byte(expectedGreeting)) {
		t.Errorf("Server did not send correct greeting, got %q", rw.writeBuf.String())
	}
}

func TestHandleConnection_ModuleList(t *testing.T) {
	mod1 := &ServerModule{Name: "mod1", Comment: "Comment 1"}
	mod2 := &ServerModule{Name: "mod2", Comment: "Comment 2"}
	s, _ := NewServer(mod1, mod2)

	// #list causes the server to close the connection (upstream exits the child process)
	// so we only send the greeting and #list, nothing after
	rw := &mockRW{
		readBuf:  bytes.NewBuffer([]byte("@RSYNCD: 32.0 md5\n#list\n")),
		writeBuf: new(bytes.Buffer),
	}

	opts := HandleOptions{
		LocalGreeting: protocol.Greeting{Version: 32, SubProtocol: 0, Digests: []string{"md5"}},
	}

	_, err := s.HandleConnection(rw, opts)
	if err == nil {
		t.Fatal("expected error after #list (connection closed)")
	}
	if !strings.Contains(err.Error(), "connection closed after #list") {
		t.Fatalf("unexpected error: %v", err)
	}

	output := rw.writeBuf.String()
	if !bytes.Contains([]byte(output), []byte("mod1")) || !bytes.Contains([]byte(output), []byte("mod2")) {
		t.Errorf("Module list missing entries, got %q", output)
	}
	if !bytes.Contains([]byte(output), []byte("@RSYNCD: EXIT\n")) {
		t.Errorf("Module list missing EXIT terminator")
	}
}

func TestHandleConnection_UnknownModule(t *testing.T) {
	mod := &ServerModule{Name: "testmod", Comment: "Test Module"}
	s, _ := NewServer(mod)

	rw := &mockRW{
		readBuf:  bytes.NewBufferString("@RSYNCD: 32.0 md5\nunknownmod\n"),
		writeBuf: new(bytes.Buffer),
	}

	opts := HandleOptions{
		LocalGreeting: protocol.Greeting{Version: 32, SubProtocol: 0, Digests: []string{"md5"}},
	}

	_, err := s.HandleConnection(rw, opts)
	if err == nil {
		t.Fatal("Expected error for unknown module, got nil")
	}

	expectedErr := "@ERROR: Unknown module\n"
	if !bytes.Contains(rw.writeBuf.Bytes(), []byte(expectedErr)) {
		t.Errorf("Server did not send correct ERROR response, got %q", rw.writeBuf.String())
	}
}

func TestHandleConnection_AuthSuccess(t *testing.T) {
	mod := &ServerModule{Name: "testmod", Comment: "Test Module"}
	s, _ := NewServer(mod)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	opts := HandleOptions{
		LocalGreeting: protocol.Greeting{Version: 32, SubProtocol: 0, Digests: []string{"md5"}},
		AuthCallback: func(username string, challenge []byte) ([]byte, error) {
			if username == "alice" {
				return []byte("valid-hash"), nil
			}
			return nil, fmt.Errorf("invalid user")
		},
	}

	errChan := make(chan error, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			errChan <- err
			return
		}
		defer conn.Close()
		_, err = s.HandleConnection(conn, opts)
		errChan <- err
	}()

	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	buf := make([]byte, 1024)
	n, err := conn.Read(buf)
	if err != nil {
		t.Fatalf("failed to read server greeting: %v", err)
	}
	_ = string(buf[:n])

	conn.Write([]byte("@RSYNCD: 32.0 md5\n"))
	conn.Write([]byte("testmod\n"))

	buf = make([]byte, 1024)
	n, err = conn.Read(buf)
	if err != nil {
		t.Fatalf("failed to read auth request: %v", err)
	}
	resp := string(buf[:n])
	if !bytes.Contains([]byte(resp), []byte("@RSYNCD: AUTHREQD")) {
		t.Fatalf("expected AUTHREQD, got %q", resp)
	}

	authResponse := fmt.Sprintf("alice %s\n", base64.StdEncoding.EncodeToString([]byte("valid-hash")))
	conn.Write([]byte(authResponse))

	// read @RSYNCD: OK
	buf = make([]byte, 1024)
	n, err = conn.Read(buf)
	if err != nil {
		t.Fatalf("failed to read auth OK: %v", err)
	}
	okResp := string(buf[:n])
	if !bytes.Contains([]byte(okResp), []byte("@RSYNCD: OK")) {
		t.Fatalf("expected OK, got %q", okResp)
	}

	// send arguments
	conn.Write([]byte(".\x00\x00"))

	// read server protocol version
	buf = make([]byte, 4)
	n, err = conn.Read(buf)
	if err != nil {
		t.Fatalf("failed to read server protocol version: %v", err)
	}
	_ = n
	_ = binary.LittleEndian.Uint32(buf[:n]) // server protocol version

	// send client protocol version response
	conn.Write(protoVersionBytes(32))

	if err := <-errChan; err != nil {
		t.Fatalf("Handshake failed: %v", err)
	}
}

func TestHandleConnection_AuthFailure(t *testing.T) {
	mod := &ServerModule{Name: "testmod", Comment: "Test Module"}
	s, _ := NewServer(mod)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	opts := HandleOptions{
		LocalGreeting: protocol.Greeting{Version: 32, SubProtocol: 0, Digests: []string{"md5"}},
		AuthCallback: func(username string, challenge []byte) ([]byte, error) {
			return []byte("valid-hash"), nil
		},
	}

	errChan := make(chan error, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			errChan <- err
			return
		}
		defer conn.Close()
		_, err = s.HandleConnection(conn, opts)
		errChan <- err
	}()

	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	buf := make([]byte, 1024)
	_, _ = conn.Read(buf)

	conn.Write([]byte("@RSYNCD: 32.0 md5\n"))
	conn.Write([]byte("testmod\n"))

	buf = make([]byte, 1024)
	_, _ = conn.Read(buf)

	authResponse := fmt.Sprintf("alice %s\n", base64.StdEncoding.EncodeToString([]byte("wrong-hash")))
	conn.Write([]byte(authResponse))

	err = <-errChan
	if err == nil {
		t.Fatal("Expected error for auth failure, got nil")
	}
	if !strings.Contains(err.Error(), "authentication failed") {
		t.Errorf("Unexpected error message: %v", err)
	}
}

func TestHandleConnection_ArgumentsProto30Plus(t *testing.T) {
	mod := &ServerModule{Name: "testmod", Comment: "Test Module"}
	s, _ := NewServer(mod)

	rw := &mockRW{
		readBuf:  bytes.NewBuffer(append([]byte("@RSYNCD: 32.0 md5\ntestmod\narg1\x00arg2\x00.\x00\x00"), protoVersionBytes(32)...)),
		writeBuf: new(bytes.Buffer),
	}

	opts := HandleOptions{
		LocalGreeting: protocol.Greeting{Version: 32, SubProtocol: 0, Digests: []string{"md5"}},
	}

	res, err := s.HandleConnection(rw, opts)
	if err != nil {
		t.Fatalf("Handshake failed: %v", err)
	}

	if res.Version != 32 {
		t.Errorf("Expected version 32, got %d", res.Version)
	}
}

func TestHandleConnection_ArgumentsProtoLegacy(t *testing.T) {
	mod := &ServerModule{Name: "testmod", Comment: "Test Module"}
	s, _ := NewServer(mod)

	rw := &mockRW{
		readBuf:  bytes.NewBuffer(append([]byte("@RSYNCD: 29.0 md5\ntestmod\narg1\narg2\n.\n\n"), protoVersionBytes(29)...)),
		writeBuf: new(bytes.Buffer),
	}

	opts := HandleOptions{
		LocalGreeting: protocol.Greeting{Version: 32, SubProtocol: 0, Digests: []string{"md5"}},
	}

	res, err := s.HandleConnection(rw, opts)
	if err != nil {
		t.Fatalf("Handshake failed: %v", err)
	}

	if res.Version > 29 {
		t.Errorf("Expected version <= 29, got %d", res.Version)
	}
}

func TestHandleConnection_NoAuth(t *testing.T) {
	// verify that server sends @RSYNCD: OK when no auth is configured
	mod := &ServerModule{Name: "testmod", Comment: "Test Module"}
	s, _ := NewServer(mod)

	rw := &mockRW{
		readBuf:  bytes.NewBuffer(append([]byte("@RSYNCD: 32.0 md5\ntestmod\n.\x00\x00"), protoVersionBytes(32)...)),
		writeBuf: new(bytes.Buffer),
	}

	opts := HandleOptions{
		LocalGreeting: protocol.Greeting{Version: 32, SubProtocol: 0, Digests: []string{"md5"}},
	}

	_, err := s.HandleConnection(rw, opts)
	if err != nil {
		t.Fatalf("Handshake failed: %v", err)
	}

	output := rw.writeBuf.String()
	if !bytes.Contains([]byte(output), []byte("@RSYNCD: OK\n")) {
		t.Errorf("Server did not send OK, got %q", output)
	}
}

func TestHandleConnection_VersionDowngrade(t *testing.T) {
	// server sends v32 but client responds with v28 protocol version
	mod := &ServerModule{Name: "testmod", Comment: "Test Module"}
	s, _ := NewServer(mod)

	rw := &mockRW{
		readBuf:  bytes.NewBuffer(append([]byte("@RSYNCD: 32.0 md5\ntestmod\n.\x00\x00"), protoVersionBytes(28)...)),
		writeBuf: new(bytes.Buffer),
	}

	opts := HandleOptions{
		LocalGreeting: protocol.Greeting{Version: 32, SubProtocol: 0, Digests: []string{"md5"}},
	}

	res, err := s.HandleConnection(rw, opts)
	if err != nil {
		t.Fatalf("Handshake failed: %v", err)
	}

	if res.Version != 28 {
		t.Errorf("Expected downgraded version 28, got %d", res.Version)
	}
}
