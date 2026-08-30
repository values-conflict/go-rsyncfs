package rsyncfs

import (
	"bytes"
	"io"
	"slices"
	"strconv"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/values-conflict/go-rsyncfs/protocol"
)

// startTestServer runs s.HandleConnection on the server end of a fresh
// [BufPipe] and returns the client-facing end.  The done channel
// receives the server's result (or is drained at test end).
func startTestServer(t *testing.T, s *Server) (client io.ReadWriteCloser, done <-chan error) {
	t.Helper()
	serverEnd, clientEnd := BufPipe()
	doneCh := make(chan error, 1)
	go func() {
		defer serverEnd.Close()
		doneCh <- s.HandleConnection(serverEnd)
	}()
	return clientEnd, doneCh
}

// testModuleFS is a small module fixture with a directory, a file, and a
// symlink.
func testModuleFS() fstest.MapFS {
	return fstest.MapFS{
		"hello.txt": &fstest.MapFile{
			Data: []byte("hello, daemon"),
			Mode: 0o644,
		},
		"sub/inner.txt": &fstest.MapFile{
			Data: []byte("inner"),
			Mode: 0o600,
		},
	}
}

// TestConnect_Success runs the full pre-transfer handshake against the
// in-repo server and verifies the negotiated session state, then pulls
// the file list through the session's mux input (which also exercises
// the filter-list flush).
func TestConnect_Success(t *testing.T) {
	mod := &ServerModule{Name: "testmod", FS: testModuleFS()}
	s, err := NewServer(mod)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	client, done := startTestServer(t, s)
	defer client.Close()

	c := Client{Module: "testmod"}
	sess, err := c.Connect(client)
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	if sess.version != 32 {
		t.Errorf("version = %d, want 32", sess.version)
	}
	if sess.checksum != "md5" {
		t.Errorf("checksum = %q, want md5 (negotiated via vstring)", sess.checksum)
	}
	if !sess.varintFlist {
		t.Error("varintFlist = false, want true (client advertises 'v')")
	}
	if sess.seed == 0 {
		t.Error("seed = 0, want non-zero")
	}
	if sess.moduleName != "testmod" {
		t.Errorf("moduleName = %q, want testmod", sess.moduleName)
	}
	if sess.subProtocol != 0 {
		t.Errorf("subProtocol = %d, want 0", sess.subProtocol)
	}

	// Flush the buffered filter list (an empty int32-0) to the server,
	// then pull one mux data chunk: the file list.
	if err := sess.mw.Flush(); err != nil {
		t.Fatalf("flush filter list: %v", err)
	}
	flist, err := sess.mr.ReadDataChunk()
	if err != nil {
		t.Fatalf("read file list chunk: %v", err)
	}
	if len(flist) == 0 {
		t.Fatal("file list is empty")
	}
	// The walked entries carry their names on the wire; with prefix
	// reuse the full name still appears at least once.
	if !bytes.Contains(flist, []byte("hello.txt")) {
		t.Error("file list does not contain hello.txt")
	}
	if !bytes.Contains(flist, []byte("inner.txt")) {
		t.Error("file list does not contain inner.txt")
	}

	client.Close()
	select {
	case serr := <-done:
		t.Logf("server returned: %v", serr)
	case <-time.After(5 * time.Second):
		t.Fatal("server goroutine did not exit")
	}
	_ = sess
}

// TestConnect_VersionMatrix connects at every supported protocol version
// range and checks the negotiated state.  The server advertises 32, so
// the client's version wins (or is capped at the server's below).
func TestConnect_VersionMatrix(t *testing.T) {
	mod := &ServerModule{Name: "testmod", FS: testModuleFS()}
	s, err := NewServer(mod)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}

	cases := []struct {
		version int
		wantVer int
		wantCS  string
		wantVar bool
	}{
		{20, 20, "md4", false},
		{21, 21, "md4", false},
		{22, 22, "md4", false}, // muxed seed path
		{23, 23, "md4", false},
		{27, 27, "md4", false},
		{29, 29, "md4", false},
		{30, 30, "md5", true},
		{31, 31, "md5", true},
		{32, 32, "md5", true},
	}
	for _, tc := range cases {
		t.Run(strconv.Itoa(tc.version), func(t *testing.T) {
			client, done := startTestServer(t, s)
			defer client.Close()

			c := Client{
				Module:   "testmod",
				Greeting: protocol.Greeting{Version: tc.version, SubProtocol: 0, Digests: protocol.SupportedDigests()},
			}
			sess, err := c.Connect(client)
			if err != nil {
				t.Fatalf("Connect: %v", err)
			}
			if sess.version != tc.wantVer {
				t.Errorf("version = %d, want %d", sess.version, tc.wantVer)
			}
			if sess.checksum != tc.wantCS {
				t.Errorf("checksum = %q, want %q", sess.checksum, tc.wantCS)
			}
			if sess.varintFlist != tc.wantVar {
				t.Errorf("varintFlist = %v, want %v", sess.varintFlist, tc.wantVar)
			}
			if sess.seed == 0 {
				t.Error("seed = 0, want non-zero")
			}
			// proto < 30: raw output, no mux writer; proto >= 30: muxed
			if (sess.version >= 30) != (sess.mw != nil) {
				t.Errorf("mw = %v, want %v", sess.mw != nil, sess.version >= 30)
			}
			if sess.mr == nil {
				t.Error("mr = nil, want mux reader")
			}

			client.Close()
			select {
			case serr := <-done:
				t.Logf("server returned: %v", serr)
			case <-time.After(5 * time.Second):
				t.Fatal("server goroutine did not exit")
			}
		})
	}
}

// TestConnect_VersionDowngrade runs the client at 32 against servers
// advertising older versions; the negotiated version must be the
// server's.
func TestConnect_VersionDowngrade(t *testing.T) {
	cases := []int{27, 22, 20}
	for _, serverVer := range cases {
		t.Run(strconv.Itoa(serverVer), func(t *testing.T) {
			mod := &ServerModule{Name: "testmod", FS: testModuleFS()}
			s, err := NewServer(mod)
			if err != nil {
				t.Fatalf("NewServer: %v", err)
			}
			s.Greeting = protocol.Greeting{
				Version: serverVer,
				Digests: protocol.SupportedDigests(),
			}
			client, done := startTestServer(t, s)
			defer client.Close()

			c := Client{
				Module:   "testmod",
				Greeting: protocol.Greeting{Version: 32, SubProtocol: 0, Digests: protocol.SupportedDigests()},
			}
			sess, err := c.Connect(client)
			if err != nil {
				t.Fatalf("Connect: %v", err)
			}
			if sess.version != serverVer {
				t.Errorf("version = %d, want %d (server-advertised)", sess.version, serverVer)
			}
			if sess.seed == 0 {
				t.Error("seed = 0, want non-zero")
			}

			client.Close()
			select {
			case serr := <-done:
				t.Logf("server returned: %v", serr)
			case <-time.After(5 * time.Second):
				t.Fatal("server goroutine did not exit")
			}
		})
	}
}

// TestConnect_AuthPassword verifies the AUTHREQD challenge/response loop
// with a PasswordAuth client against a server module whose AuthCallback
// computes the same digest.
func TestConnect_AuthPassword(t *testing.T) {
	mod := &ServerModule{
		Name: "securemod",
		FS:   testModuleFS(),
		AuthCallback: func(username string, challenge []byte) ([]byte, error) {
			if username != "tianon" {
				return nil, nil
			}
			return computeAuthHash("md5", "s3cret", challenge)
		},
	}
	s, err := NewServer(mod)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	client, done := startTestServer(t, s)
	defer client.Close()

	c := Client{
		Module:       "securemod",
		AuthUser:     "tianon",
		AuthResponse: PasswordAuth("s3cret"),
	}
	sess, err := c.Connect(client)
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	if sess.version != 32 {
		t.Errorf("version = %d, want 32", sess.version)
	}

	client.Close()
	select {
	case serr := <-done:
		t.Logf("server returned: %v", serr)
	case <-time.After(5 * time.Second):
		t.Fatal("server goroutine did not exit")
	}
}

// TestConnect_AuthFailure checks that a wrong password surfaces the
// server's @ERROR and aborts Connect.
func TestConnect_AuthFailure(t *testing.T) {
	mod := &ServerModule{
		Name: "securemod",
		FS:   testModuleFS(),
		AuthCallback: func(username string, challenge []byte) ([]byte, error) {
			return computeAuthHash("md5", "right-password", challenge)
		},
	}
	s, err := NewServer(mod)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	client, done := startTestServer(t, s)
	defer client.Close()

	c := Client{
		Module:       "securemod",
		AuthUser:     "tianon",
		AuthResponse: PasswordAuth("wrong-password"),
	}
	if _, err := c.Connect(client); err == nil {
		t.Fatal("Connect succeeded, want auth failure")
	} else if !strings.Contains(err.Error(), "Authentication failed") {
		t.Errorf("error = %q, want it to mention 'Authentication failed'", err)
	}

	client.Close()
	<-done
}

// TestConnect_AuthRequiredButNotConfigured checks that a module requiring
// auth rejects a client with no AuthUser/AuthResponse.
func TestConnect_AuthRequiredButNotConfigured(t *testing.T) {
	mod := &ServerModule{
		Name: "securemod",
		FS:   testModuleFS(),
		AuthCallback: func(username string, challenge []byte) ([]byte, error) {
			return computeAuthHash("md5", "x", challenge)
		},
	}
	s, err := NewServer(mod)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	client, done := startTestServer(t, s)
	defer client.Close()

	c := Client{Module: "securemod"}
	if _, err := c.Connect(client); err == nil {
		t.Fatal("Connect succeeded, want an auth-configuration error")
	} else if !strings.Contains(err.Error(), "AuthUser/AuthResponse") {
		t.Errorf("error = %q, want it to mention AuthUser/AuthResponse", err)
	}

	client.Close()
	<-done
}

// TestConnect_UnknownModule checks the @ERROR path for a module the
// server does not have.
func TestConnect_UnknownModule(t *testing.T) {
	s, err := NewServer(&ServerModule{Name: "testmod", FS: testModuleFS()})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	client, done := startTestServer(t, s)
	defer client.Close()

	c := Client{Module: "nosuchmodule"}
	if _, err := c.Connect(client); err == nil {
		t.Fatal("Connect succeeded, want unknown-module error")
	} else if !strings.Contains(err.Error(), "Unknown module") {
		t.Errorf("error = %q, want it to mention 'Unknown module'", err)
	}

	client.Close()
	<-done
}

// TestConnect_NilRW_WithConnectFunc checks that Connect(nil) uses
// ConnectFunc to create the connection.
func TestConnect_NilRW_WithConnectFunc(t *testing.T) {
	s, err := NewServer(&ServerModule{Name: "testmod", FS: testModuleFS()})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	client, done := startTestServer(t, s)

	gotModule := ""
	c := Client{
		Module: "testmod",
		ConnectFunc: func(moduleName string) (io.ReadWriter, error) {
			gotModule = moduleName
			return client, nil
		},
	}
	sess, err := c.Connect(nil)
	if err != nil {
		t.Fatalf("Connect(nil): %v", err)
	}
	if gotModule != "testmod" {
		t.Errorf("ConnectFunc module = %q, want testmod", gotModule)
	}
	if sess.version != 32 {
		t.Errorf("version = %d, want 32", sess.version)
	}

	client.Close()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("server goroutine did not exit")
	}
}

// TestConnect_NilRW_WithoutConnectFunc checks the error for a nil
// transport and no ConnectFunc.
func TestConnect_NilRW_WithoutConnectFunc(t *testing.T) {
	c := Client{Module: "testmod"}
	if _, err := c.Connect(nil); err == nil {
		t.Fatal("Connect(nil) succeeded, want an error")
	} else if !strings.Contains(err.Error(), "ConnectFunc") {
		t.Errorf("error = %q, want it to mention ConnectFunc", err)
	}
}

// TestConnect_EmptyModule checks that Connect refuses root mode (use
// OpenRoot instead).
func TestConnect_EmptyModule(t *testing.T) {
	c := Client{ConnectFunc: func(string) (io.ReadWriter, error) { return nil, nil }}
	if _, err := c.Connect(nil); err == nil {
		t.Fatal("Connect succeeded with empty Module, want an error")
	} else if !strings.Contains(err.Error(), "OpenRoot") {
		t.Errorf("error = %q, want it to mention OpenRoot", err)
	}
}

// TestOpenRoot checks the config-holder session for root mode: no live
// connection is opened, the negotiated version comes from the greeting
// defaults, and ConnectFunc is retained.
func TestOpenRoot(t *testing.T) {
	called := 0
	c := Client{
		ConnectFunc: func(string) (io.ReadWriter, error) {
			called++
			return nil, nil
		},
	}
	sess, err := c.OpenRoot()
	if err != nil {
		t.Fatalf("OpenRoot: %v", err)
	}
	if called != 0 {
		t.Errorf("ConnectFunc called %d times, want 0 (no connection in OpenRoot)", called)
	}
	if sess.connectFunc == nil {
		t.Error("connectFunc = nil, want the caller's ConnectFunc")
	}
	if sess.version != protocol.CurrentProtocolVersion {
		t.Errorf("version = %d, want %d", sess.version, protocol.CurrentProtocolVersion)
	}
	if sess.rw != nil || sess.mr != nil || sess.mw != nil {
		t.Error("OpenRoot session carries live connection state")
	}
}

// TestOpenRoot_Errors checks the validation errors for OpenRoot.
func TestOpenRoot_Errors(t *testing.T) {
	c := Client{
		Module:      "testmod",
		ConnectFunc: func(string) (io.ReadWriter, error) { return nil, nil },
	}
	if _, err := c.OpenRoot(); err == nil {
		t.Fatal("OpenRoot with Module set succeeded, want an error")
	} else if !strings.Contains(err.Error(), "Connect") {
		t.Errorf("error = %q, want it to steer to Connect", err)
	}

	c2 := Client{}
	if _, err := c2.OpenRoot(); err == nil {
		t.Fatal("OpenRoot without ConnectFunc succeeded, want an error")
	} else if !strings.Contains(err.Error(), "ConnectFunc") {
		t.Errorf("error = %q, want it to mention ConnectFunc", err)
	}
}

// TestComputeAuthHash checks the digest computation for both supported
// algorithms and the error for an unknown one.
func TestComputeAuthHash(t *testing.T) {
	challenge := []byte("0123456789abcdef")
	for digest, wantLen := range map[string]int{"md4": 16, "md5": 16} {
		got, err := computeAuthHash(digest, "password", challenge)
		if err != nil {
			t.Fatalf("computeAuthHash(%s): %v", digest, err)
		}
		if len(got) != wantLen {
			t.Errorf("computeAuthHash(%s) len = %d, want %d", digest, len(got), wantLen)
		}
	}
	if _, err := computeAuthHash("sha256", "password", challenge); err == nil {
		t.Error("computeAuthHash(sha256) succeeded, want an unsupported-digest error")
	}

	// the same input must produce the same digest (deterministic)
	a, _ := computeAuthHash("md5", "password", challenge)
	b, _ := computeAuthHash("md5", "password", challenge)
	if !slices.Equal(a, b) {
		t.Error("computeAuthHash is not deterministic")
	}
}
