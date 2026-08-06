package rsyncfs

import (
	"bytes"
	"io"
	"io/fs"
	"net"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/values-conflict/go-rsyncfs/protocol"
)

// startServer starts a server connected via net.Pipe and returns the client connection and server error channel
func startServer(t *testing.T, mods []*ServerModule, opts HandleOptions) (net.Conn, <-chan error) {
	t.Helper()
	srv, err := NewServer(mods...)
	if err != nil {
		t.Fatalf("NewServer failed: %v", err)
	}

	serverConn, clientConn := net.Pipe()
	srvErr := make(chan error, 1)
	go func() {
		defer serverConn.Close()
		_, err := srv.HandleConnection(serverConn, opts)
		srvErr <- err
	}()

	t.Cleanup(func() { clientConn.Close() })
	return clientConn, srvErr
}

func TestClientConnect_BasicSuccess(t *testing.T) {
	conn, srvErr := startServer(t, []*ServerModule{
		{Name: "testmod", Comment: "Test Module", FS: fstest.MapFS{"file.txt": {Data: []byte("hello")}}},
	}, HandleOptions{
		LocalGreeting: protocol.Greeting{Version: 32, SubProtocol: 0, Digests: []string{"md5", "md4"}},
	})

	session, err := (&Client{Module: "testmod"}).Connect(conn)
	if err != nil {
		t.Fatalf("Connect failed: %v", err)
	}
	if session.version != 32 {
		t.Errorf("expected version 32, got %d", session.version)
	}
	if session.digest != "md5" {
		t.Errorf("expected digest md5, got %s", session.digest)
	}
	if err := <-srvErr; err != nil {
		t.Fatalf("server error: %v", err)
	}
}

func TestClientConnect_VersionNegotiation(t *testing.T) {
	tests := []struct {
		name        string
		clientVer   int
		serverVer   int
		wantVersion int
	}{
		{"both v32", 32, 32, 32},
		{"client v30 server v32", 30, 32, 30},
		{"client v32 server v30", 32, 30, 30},
		{"both v28", 28, 28, 28},
		{"client v20 server v32", 20, 32, 20},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			conn, srvErr := startServer(t, []*ServerModule{
				{Name: "testmod", FS: fstest.MapFS{}},
			}, HandleOptions{
				LocalGreeting: protocol.Greeting{Version: tt.serverVer, SubProtocol: 0, Digests: []string{"md5"}},
			})

			session, err := (&Client{
				Module: "testmod",
				Greeting: protocol.Greeting{
					Version:     tt.clientVer,
					SubProtocol: 0,
					Digests:     []string{"md5"},
				},
			}).Connect(conn)
			if err != nil {
				t.Fatalf("Connect failed: %v", err)
			}
			if session.version != tt.wantVersion {
				t.Errorf("expected version %d, got %d", tt.wantVersion, session.version)
			}
			if err := <-srvErr; err != nil {
				t.Fatalf("server error: %v", err)
			}
		})
	}
}

func TestClientConnect_ModuleList(t *testing.T) {
	mod1 := &ServerModule{Name: "mod1", Comment: "First Module", FS: fstest.MapFS{}}
	mod2 := &ServerModule{Name: "mod2", Comment: "Second Module", FS: fstest.MapFS{}}
	srv, err := NewServer(mod1, mod2)
	if err != nil {
		t.Fatalf("NewServer failed: %v", err)
	}

	srvErr := make(chan error, 1)

	// root mode client -- uses OpenRoot with ConnectFunc
	client := &Client{
		ConnectFunc: func(moduleName string) (io.ReadWriter, error) {
			serverConn, clientConn := net.Pipe()
			go func() {
				defer serverConn.Close()
				_, err := srv.HandleConnection(serverConn, HandleOptions{
					LocalGreeting: protocol.Greeting{Version: 32, SubProtocol: 0, Digests: []string{"md5"}},
				})
				srvErr <- err
			}()
			return clientConn, nil
		},
	}

	session, err := client.OpenRoot()
	if err != nil {
		t.Fatalf("OpenRoot failed: %v", err)
	}

	// Open(".") triggers a live #list call
	rootFile, err := session.Open(".")
	if err != nil {
		t.Fatalf("Open(.) failed: %v", err)
	}
	defer rootFile.Close()

	// read the directory entries
	entries, err := rootFile.(interface {
		ReadDir(n int) ([]fs.DirEntry, error)
	}).ReadDir(0)
	if err != nil {
		t.Fatalf("ReadDir failed: %v", err)
	}

	if len(entries) != 2 {
		t.Fatalf("expected 2 modules, got %d", len(entries))
	}

	// verify modules are present
	found := make(map[string]string)
	for _, e := range entries {
		info, err := e.Info()
		if err != nil {
			t.Fatalf("Info failed: %v", err)
		}
		found[e.Name()] = info.(interface {
			Comment() string
		}).Comment()
	}

	if c, ok := found["mod1"]; !ok || c != "First Module" {
		t.Errorf("mod1 not found or wrong comment: %q", c)
	}
	if c, ok := found["mod2"]; !ok || c != "Second Module" {
		t.Errorf("mod2 not found or wrong comment: %q", c)
	}

	// server closes connection after #list -- this is expected
	if err := <-srvErr; err != nil {
		if !strings.Contains(err.Error(), "connection closed after #list") {
			t.Fatalf("unexpected server error: %v", err)
		}
	}
}

func TestClientConnect_UnknownModule(t *testing.T) {
	conn, srvErr := startServer(t, []*ServerModule{
		{Name: "testmod", Comment: "Test", FS: fstest.MapFS{}},
	}, HandleOptions{
		LocalGreeting: protocol.Greeting{Version: 32, SubProtocol: 0, Digests: []string{"md5"}},
	})

	_, err := (&Client{Module: "nonexistent"}).Connect(conn)
	if err == nil {
		t.Fatal("expected error for unknown module, got nil")
	}
	<-srvErr
}

func TestClientConnect_AuthRequired(t *testing.T) {
	mod := &ServerModule{Name: "testmod", Comment: "Test", FS: fstest.MapFS{}}
	srv, _ := NewServer(mod)

	serverConn, clientConn := net.Pipe()
	srvErr := make(chan error, 1)
	go func() {
		defer serverConn.Close()
		_, err := srv.HandleConnection(serverConn, HandleOptions{
			LocalGreeting: protocol.Greeting{Version: 32, SubProtocol: 0, Digests: []string{"md5"}},
			AuthCallback: func(username string, challenge []byte) ([]byte, error) {
				if username == "alice" {
					return []byte("valid-hash"), nil
				}
				return nil, nil
			},
		})
		srvErr <- err
	}()

	// client without auth should fail
	client := &Client{Module: "testmod"}
	_, err := client.Connect(clientConn)
	if err == nil {
		t.Fatal("expected error when server requires auth but client has no credentials")
	}
	clientConn.Close()

	// server goroutine is blocked waiting for auth response that never comes
	// close serverConn to unblock it (already deferred, but let's wait)
	select {
	case <-srvErr:
	case <-time.After(2 * time.Second):
		// server still blocked -- that's expected since client disconnected
	}
}

func TestClientConnect_ProtocolVersionExchange(t *testing.T) {
	conn, srvErr := startServer(t, []*ServerModule{
		{Name: "testmod", FS: fstest.MapFS{}},
	}, HandleOptions{
		LocalGreeting: protocol.Greeting{Version: 30, SubProtocol: 0, Digests: []string{"md5"}},
	})

	session, err := (&Client{
		Module: "testmod",
		Greeting: protocol.Greeting{
			Version:     30,
			SubProtocol: 0,
			Digests:     []string{"md5"},
		},
	}).Connect(conn)
	if err != nil {
		t.Fatalf("Connect failed: %v", err)
	}
	if session.version != 30 {
		t.Errorf("expected version 30, got %d", session.version)
	}
	if err := <-srvErr; err != nil {
		t.Fatalf("server error: %v", err)
	}
}

func TestClientConnect_NilRWWithConnectFunc(t *testing.T) {
	srv, err := NewServer(&ServerModule{Name: "testmod", FS: fstest.MapFS{}})
	if err != nil {
		t.Fatalf("NewServer failed: %v", err)
	}

	srvErr := make(chan error, 1)
	session, err := (&Client{
		Module: "testmod",
		ConnectFunc: func(moduleName string) (io.ReadWriter, error) {
			serverConn, clientConn := net.Pipe()
			go func() {
				defer serverConn.Close()
				_, err := srv.HandleConnection(serverConn, HandleOptions{
					LocalGreeting: protocol.Greeting{Version: 32, SubProtocol: 0, Digests: []string{"md5"}},
				})
				srvErr <- err
			}()
			return clientConn, nil
		},
	}).Connect(nil)
	if err != nil {
		t.Fatalf("Connect(nil) failed: %v", err)
	}
	if session.version != 32 {
		t.Errorf("expected version 32, got %d", session.version)
	}
	if err := <-srvErr; err != nil {
		t.Fatalf("server error: %v", err)
	}
}

func TestClientConnect_NilRWWithoutConnectFunc(t *testing.T) {
	client := &Client{Module: "testmod"}
	_, err := client.Connect(nil)
	if err == nil {
		t.Fatal("expected error when rw is nil and ConnectFunc is not set")
	}
}

func TestClientConnect_Defaults(t *testing.T) {
	client := &Client{Module: "testmod"}
	// defaults are applied lazily in Connect -- verify by checking the greeting
	// we can't call Connect without a server, so test applyDefaults directly
	client.applyDefaults()
	if client.Greeting.Version != 32 {
		t.Errorf("expected default version 32, got %d", client.Greeting.Version)
	}
	if len(client.Greeting.Digests) == 0 {
		t.Error("expected default digests to be set")
	}
}

func TestClientConnect_Options(t *testing.T) {
	client := &Client{
		Module:       "mymod",
		AuthUser:     "alice",
		AuthResponse: func(digest string, challenge []byte) ([]byte, error) { return []byte("hash"), nil },
		Greeting:     protocol.Greeting{Version: 28, SubProtocol: 0, Digests: []string{"md4"}},
	}

	if client.Module != "mymod" {
		t.Errorf("expected module mymod, got %q", client.Module)
	}
	if client.AuthUser != "alice" {
		t.Errorf("expected AuthUser alice, got %q", client.AuthUser)
	}
	if client.AuthResponse == nil {
		t.Error("expected AuthResponse to be set")
	}
	if client.Greeting.Version != 28 {
		t.Errorf("expected version 28, got %d", client.Greeting.Version)
	}
}

func TestClientRootDir_Stat(t *testing.T) {
	modules := []moduleInfo{
		{name: "mod1", comment: "First"},
		{name: "mod2", comment: "Second"},
	}

	rd := newRootDir(modules)
	info, err := rd.Stat()
	if err != nil {
		t.Fatalf("Stat failed: %v", err)
	}
	if !info.IsDir() {
		t.Error("expected root dir to be a directory")
	}
}

func TestClientRootDir_ReadDir(t *testing.T) {
	modules := []moduleInfo{
		{name: "mod1", comment: "First"},
		{name: "mod2", comment: "Second"},
		{name: "mod3", comment: "Third"},
	}

	rd := newRootDir(modules)
	entries, err := rd.ReadDir(2)
	if err != nil {
		t.Fatalf("ReadDir(2) failed: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(entries))
	}
	if entries[0].Name() != "mod1" {
		t.Errorf("expected first entry mod1, got %s", entries[0].Name())
	}
	if entries[1].Name() != "mod2" {
		t.Errorf("expected second entry mod2, got %s", entries[1].Name())
	}

	// second read should get remaining entry
	entries, err = rd.ReadDir(-1)
	if err != nil {
		t.Fatalf("ReadDir(-1) failed: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(entries))
	}
	if entries[0].Name() != "mod3" {
		t.Errorf("expected third entry mod3, got %s", entries[0].Name())
	}

	// third read should return EOF
	entries, err = rd.ReadDir(-1)
	if err != io.EOF {
		t.Errorf("expected EOF, got %v (entries: %d)", err, len(entries))
	}
}

func TestClientConnect_LegacyProtocol(t *testing.T) {
	conn, srvErr := startServer(t, []*ServerModule{
		{Name: "testmod", FS: fstest.MapFS{}},
	}, HandleOptions{
		LocalGreeting: protocol.Greeting{Version: 28, SubProtocol: 0, Digests: []string{"md4"}},
	})

	session, err := (&Client{
		Module: "testmod",
		Greeting: protocol.Greeting{
			Version:     28,
			SubProtocol: 0,
			Digests:     []string{"md4"},
		},
	}).Connect(conn)
	if err != nil {
		t.Fatalf("Connect failed: %v", err)
	}
	if session.version != 28 {
		t.Errorf("expected version 28, got %d", session.version)
	}
	if session.digest != "md4" {
		t.Errorf("expected digest md4, got %s", session.digest)
	}
	if err := <-srvErr; err != nil {
		t.Fatalf("server error: %v", err)
	}
}

func TestClientSession_OpenModule_NotImplemented(t *testing.T) {
	mod := &ServerModule{
		Name: "testmod",
		FS:   fstest.MapFS{"file.txt": {Data: []byte("hello")}},
	}
	srv, _ := NewServer(mod)

	serverConn, clientConn := net.Pipe()
	srvErr := make(chan error, 1)
	go func() {
		defer serverConn.Close()
		_, err := srv.HandleConnection(serverConn, HandleOptions{
			LocalGreeting: protocol.Greeting{Version: 32, SubProtocol: 0, Digests: []string{"md5"}},
		})
		srvErr <- err
	}()

	client := &Client{Module: "testmod"}
	session, err := client.Connect(clientConn)
	if err != nil {
		t.Fatalf("Connect failed: %v", err)
	}

	// Open is not yet implemented (Task 9)
	_, err = session.Open("file.txt")
	if err == nil {
		t.Error("expected error from unimplemented Open")
	}

	<-srvErr
}

func TestClientSession_OpenRootMode_NotImplemented(t *testing.T) {
	mod1 := &ServerModule{Name: "mod1", Comment: "First", FS: fstest.MapFS{}}
	mod2 := &ServerModule{Name: "mod2", Comment: "Second", FS: fstest.MapFS{}}
	srv, _ := NewServer(mod1, mod2)

	client := &Client{
		ConnectFunc: func(moduleName string) (io.ReadWriter, error) {
			serverConn, clientConn := net.Pipe()
			go func() {
				defer serverConn.Close()
				_, _ = srv.HandleConnection(serverConn, HandleOptions{
					LocalGreeting: protocol.Greeting{Version: 32, SubProtocol: 0, Digests: []string{"md5"}},
				})
			}()
			return clientConn, nil
		},
	}
	session, err := client.OpenRoot()
	if err != nil {
		t.Fatalf("OpenRoot failed: %v", err)
	}

	// root directory should work
	rootFile, err := session.Open(".")
	if err != nil {
		t.Fatalf("Open root failed: %v", err)
	}
	defer rootFile.Close()

	entries, err := rootFile.(interface {
		ReadDir(n int) ([]fs.DirEntry, error)
	}).ReadDir(-1)
	if err != nil {
		t.Fatalf("ReadDir failed: %v", err)
	}
	if len(entries) != 2 {
		t.Errorf("expected 2 module entries, got %d", len(entries))
	}

	// non-root path not yet implemented
	_, err = session.Open("mod1/somefile")
	if err == nil {
		t.Error("expected error from unimplemented module open in root mode")
	}
}

func TestPasswordAuth(t *testing.T) {
	tests := []struct {
		name      string
		digest    string
		password  string
		challenge []byte
	}{
		{"md5", "md5", "secret", []byte("challenge")},
		{"md4", "md4", "secret", []byte("challenge")},
		{"md5 empty password", "md5", "", []byte("challenge")},
		{"md4 empty challenge", "md4", "secret", []byte{}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			authFn := PasswordAuth(tt.password)
			if authFn == nil {
				t.Fatal("PasswordAuth returned nil")
			}

			hash, err := authFn(tt.digest, tt.challenge)
			if err != nil {
				t.Fatalf("PasswordAuth(%q)(%q, %q) error: %v", tt.password, tt.digest, tt.challenge, err)
			}

			// verify against independently computed hash
			want, err := computeAuthHash(tt.digest, tt.password, tt.challenge)
			if err != nil {
				t.Fatalf("computeAuthHash (expected): %v", err)
			}
			if !bytes.Equal(hash, want) {
				t.Errorf("PasswordAuth(%q)(%q, %q) = %v, want %v", tt.password, tt.digest, tt.challenge, hash, want)
			}
		})
	}

	t.Run("unsupported digest", func(t *testing.T) {
		authFn := PasswordAuth("secret")
		_, err := authFn("blake3", []byte("challenge"))
		if err == nil {
			t.Fatal("expected error for unsupported digest, got nil")
		}
	})

	t.Run("closure captures password", func(t *testing.T) {
		// verify the closure correctly captures the password (not a reference to a mutable variable)
		authFn := PasswordAuth("original")
		hash1, err := authFn("md5", []byte("challenge"))
		if err != nil {
			t.Fatalf("first call: %v", err)
		}

		// calling PasswordAuth again with different password should not affect the first closure
		_ = PasswordAuth("different")

		hash2, err := authFn("md5", []byte("challenge"))
		if err != nil {
			t.Fatalf("second call: %v", err)
		}
		if !bytes.Equal(hash1, hash2) {
			t.Error("PasswordAuth closure should produce consistent results")
		}
	})
}

func TestComputeAuthHash(t *testing.T) {
	tests := []struct {
		name      string
		password  string
		challenge []byte
		digest    string
		wantLen   int
	}{
		{
			name:      "md5 basic",
			password:  "secret",
			challenge: []byte("challenge"),
			digest:    "md5",
			wantLen:   16,
		},
		{
			name:      "md4 basic",
			password:  "secret",
			challenge: []byte("challenge"),
			digest:    "md4",
			wantLen:   16,
		},
		{
			name:      "md5 empty password",
			password:  "",
			challenge: []byte("challenge"),
			digest:    "md5",
			wantLen:   16,
		},
		{
			name:      "md4 empty challenge",
			password:  "secret",
			challenge: []byte{},
			digest:    "md4",
			wantLen:   16,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			hash, err := computeAuthHash(tt.digest, tt.password, tt.challenge)
			if err != nil {
				t.Fatalf("computeAuthHash(%q, %q, %q) error: %v", tt.digest, tt.password, tt.challenge, err)
			}
			if len(hash) != tt.wantLen {
				t.Errorf("computeAuthHash(%q, %q, %q) len = %d, want %d", tt.digest, tt.password, tt.challenge, len(hash), tt.wantLen)
			}
		})
	}

	t.Run("unsupported digest", func(t *testing.T) {
		_, err := computeAuthHash("blake3", "password", []byte("challenge"))
		if err == nil {
			t.Fatal("expected error for unsupported digest, got nil")
		}
	})
}
