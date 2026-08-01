package rsyncfs

import (
	"io"
	"io/fs"
	"net"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/values-conflict/go-rsyncfs/protocol"
)

func TestClientConnect_BasicSuccess(t *testing.T) {
	mod := &ServerModule{
		Name:    "testmod",
		Comment: "Test Module",
		FS:      fstest.MapFS{"file.txt": {Data: []byte("hello")}},
	}
	srv, err := NewServer(mod)
	if err != nil {
		t.Fatalf("NewServer failed: %v", err)
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	srvErr := make(chan error, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			srvErr <- err
			return
		}
		defer conn.Close()
		_, err = srv.HandleConnection(conn, HandleOptions{
			LocalGreeting: protocol.Greeting{Version: 32, SubProtocol: 0, Digests: []string{"md5", "md4"}},
		})
		srvErr <- err
	}()

	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	client := NewClient(ClientConfig{Module: "testmod"})
	session, err := client.Connect(conn)
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
			mod := &ServerModule{
				Name: "testmod",
				FS:   fstest.MapFS{},
			}
			srv, _ := NewServer(mod)

			ln, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer ln.Close()

			srvErr := make(chan error, 1)
			go func() {
				conn, err := ln.Accept()
				if err != nil {
					srvErr <- err
					return
				}
				defer conn.Close()
				_, err = srv.HandleConnection(conn, HandleOptions{
					LocalGreeting: protocol.Greeting{Version: tt.serverVer, SubProtocol: 0, Digests: []string{"md5"}},
				})
				srvErr <- err
			}()

			conn, err := net.Dial("tcp", ln.Addr().String())
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()

			client := NewClient(ClientConfig{
				Module: "testmod",
				Greeting: protocol.Greeting{
					Version:     tt.clientVer,
					SubProtocol: 0,
					Digests:     []string{"md5"},
				},
			})
			session, err := client.Connect(conn)
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

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	srvErr := make(chan error, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			srvErr <- err
			return
		}
		defer conn.Close()
		_, err = srv.HandleConnection(conn, HandleOptions{
			LocalGreeting: protocol.Greeting{Version: 32, SubProtocol: 0, Digests: []string{"md5"}},
		})
		srvErr <- err
	}()

	// root mode client -- uses OpenRoot with ConnectFunc
	client := NewClient(ClientConfig{
		ConnectFunc: func(moduleName string) (io.ReadWriter, error) {
			return net.Dial("tcp", ln.Addr().String())
		},
	})

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
	mod := &ServerModule{Name: "testmod", Comment: "Test", FS: fstest.MapFS{}}
	srv, _ := NewServer(mod)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	srvErr := make(chan error, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			srvErr <- err
			return
		}
		defer conn.Close()
		_, err = srv.HandleConnection(conn, HandleOptions{
			LocalGreeting: protocol.Greeting{Version: 32, SubProtocol: 0, Digests: []string{"md5"}},
		})
		srvErr <- err
	}()

	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	client := NewClient(ClientConfig{Module: "nonexistent"})
	_, err = client.Connect(conn)
	if err == nil {
		t.Fatal("expected error for unknown module, got nil")
	}

	// drain server error channel
	<-srvErr
}

func TestClientConnect_AuthRequired(t *testing.T) {
	mod := &ServerModule{Name: "testmod", Comment: "Test", FS: fstest.MapFS{}}
	srv, _ := NewServer(mod)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	srvErr := make(chan error, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			srvErr <- err
			return
		}
		defer conn.Close()
		_, err = srv.HandleConnection(conn, HandleOptions{
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

	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	// client without auth should fail
	client := NewClient(ClientConfig{Module: "testmod"})
	_, err = client.Connect(conn)
	if err == nil {
		t.Fatal("expected error when server requires auth but client has no credentials")
	}

	// server goroutine is blocked waiting for auth response that never comes
	// close the listener to unblock it
	ln.Close()
	select {
	case <-srvErr:
	case <-time.After(2 * time.Second):
		// server still blocked -- that's expected since client disconnected
	}
}

func TestClientConnect_ProtocolVersionExchange(t *testing.T) {
	// verify that the binary protocol version exchange happens correctly
	mod := &ServerModule{
		Name: "testmod",
		FS:   fstest.MapFS{},
	}
	srv, _ := NewServer(mod)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	srvErr := make(chan error, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			srvErr <- err
			return
		}
		defer conn.Close()
		_, err = srv.HandleConnection(conn, HandleOptions{
			LocalGreeting: protocol.Greeting{Version: 30, SubProtocol: 0, Digests: []string{"md5"}},
		})
		srvErr <- err
	}()

	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	client := NewClient(ClientConfig{
		Module: "testmod",
		Greeting: protocol.Greeting{
			Version:     30,
			SubProtocol: 0,
			Digests:     []string{"md5"},
		},
	})
	session, err := client.Connect(conn)
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
	// test with protocol < 30 (newline-terminated arguments)
	mod := &ServerModule{
		Name: "testmod",
		FS:   fstest.MapFS{},
	}
	srv, _ := NewServer(mod)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	srvErr := make(chan error, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			srvErr <- err
			return
		}
		defer conn.Close()
		_, err = srv.HandleConnection(conn, HandleOptions{
			LocalGreeting: protocol.Greeting{Version: 28, SubProtocol: 0, Digests: []string{"md4"}},
		})
		srvErr <- err
	}()

	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	client := NewClient(ClientConfig{
		Module: "testmod",
		Greeting: protocol.Greeting{
			Version:     28,
			SubProtocol: 0,
			Digests:     []string{"md4"},
		},
	})
	session, err := client.Connect(conn)
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

func TestNewClient_Defaults(t *testing.T) {
	client := NewClient(ClientConfig{})
	if client.cfg.Greeting.Version != 32 {
		t.Errorf("expected default version 32, got %d", client.cfg.Greeting.Version)
	}
	if len(client.cfg.Greeting.Digests) == 0 {
		t.Error("expected default digests to be set")
	}
}

func TestNewClient_Options(t *testing.T) {
	client := NewClient(ClientConfig{
		Module:       "mymod",
		AuthUser:     "alice",
		AuthResponse: func(challenge []byte, digest string) ([]byte, error) { return []byte("hash"), nil },
		Greeting:     protocol.Greeting{Version: 28, SubProtocol: 0, Digests: []string{"md4"}},
	})

	if client.cfg.Module != "mymod" {
		t.Errorf("expected module mymod, got %q", client.cfg.Module)
	}
	if client.cfg.AuthUser != "alice" {
		t.Errorf("expected AuthUser alice, got %q", client.cfg.AuthUser)
	}
	if client.cfg.AuthResponse == nil {
		t.Error("expected AuthResponse to be set")
	}
	if client.cfg.Greeting.Version != 28 {
		t.Errorf("expected version 28, got %d", client.cfg.Greeting.Version)
	}
}

func TestClientSession_OpenModule_NotImplemented(t *testing.T) {
	mod := &ServerModule{
		Name: "testmod",
		FS:   fstest.MapFS{"file.txt": {Data: []byte("hello")}},
	}
	srv, _ := NewServer(mod)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	srvErr := make(chan error, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			srvErr <- err
			return
		}
		defer conn.Close()
		_, err = srv.HandleConnection(conn, HandleOptions{
			LocalGreeting: protocol.Greeting{Version: 32, SubProtocol: 0, Digests: []string{"md5"}},
		})
		srvErr <- err
	}()

	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	client := NewClient(ClientConfig{Module: "testmod"})
	session, err := client.Connect(conn)
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

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	// accept connections but don't do anything with them
	// (root mode opens fresh connections for each operation)
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				_, _ = srv.HandleConnection(conn, HandleOptions{
					LocalGreeting: protocol.Greeting{Version: 32, SubProtocol: 0, Digests: []string{"md5"}},
				})
				conn.Close()
			}()
		}
	}()

	client := NewClient(ClientConfig{
		ConnectFunc: func(moduleName string) (io.ReadWriter, error) {
			return net.Dial("tcp", ln.Addr().String())
		},
	})
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
