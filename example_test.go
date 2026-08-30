package rsyncfs

import (
	"crypto/md5"
	"fmt"
	"io"
	"io/fs"
	"net"
	"strings"
	"testing/fstest"
)

// startExampleServer serves s's modules over a loopback TCP listener on an
// ephemeral port, running [Server.HandleConnection] in a goroutine per
// connection; the returned stop function closes the listener.  It returns
// the listener's "host:port" address.
func startExampleServer(s *Server) (string, func()) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		panic(err)
	}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				_ = s.HandleConnection(conn)
				_ = conn.Close()
			}()
		}
	}()
	return ln.Addr().String(), func() { _ = ln.Close() }
}

// ExampleServer creates a [Server] with a single module backed by [fstest.MapFS] and serves it over a loopback TCP socket, one [Server.HandleConnection] goroutine per connection, then pulls a file from it with a [Client].
func ExampleServer() {
	// a single module backed by a [fstest.MapFS]
	server, err := NewServer(&ServerModule{
		Name:    "data",
		Comment: "the example data",
		FS: fstest.MapFS{
			"hello.txt": &fstest.MapFile{Data: []byte("hello, rsync!")},
		},
	})
	if err != nil {
		panic(err)
	}

	// the library has no listener of its own -- the caller owns the
	// transport: listen on an ephemeral loopback port and hand every
	// accepted connection to [Server.HandleConnection] in its own goroutine
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		panic(err)
	}
	defer ln.Close()
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				_ = server.HandleConnection(conn)
				_ = conn.Close()
			}()
		}
	}()
	addr := ln.Addr().String()

	// connect and pull a file
	client := Client{
		Module: "data",
		ConnectFunc: func(string) (io.ReadWriter, error) {
			return net.Dial("tcp", addr)
		},
	}
	session, err := client.Connect(nil)
	if err != nil {
		panic(err)
	}
	f, err := session.Open("hello.txt")
	if err != nil {
		panic(err)
	}
	data, err := io.ReadAll(f)
	if err != nil {
		panic(err)
	}
	fmt.Println(string(data))

	// Output:
	// hello, rsync!
}

// ExampleServer_auth serves a module whose [ServerModule.AuthCallback] requires the "zerocool" username -- the rsync secrets-file flow, where the server generates a random challenge and the callback verifies the digest(password + challenge) the client responds with.
func ExampleServer_auth() {
	// a module that requires the "zerocool" username: the callback gets
	// the username the client sent and the challenge the server
	// generated, and returns the expected digest bytes to compare against
	// the client's response ("md5" being the auth digest the default
	// greetings negotiate at the current protocol version)
	server, err := NewServer(&ServerModule{
		Name:    "data",
		Comment: "the example data",
		FS: fstest.MapFS{
			"hello.txt": &fstest.MapFile{Data: []byte("hello, rsync!")},
		},
		AuthCallback: func(username string, challenge []byte) ([]byte, error) {
			if username != "zerocool" {
				return nil, fmt.Errorf("unknown user %q", username)
			}
			sum := md5.Sum(append([]byte("hack the planet"), challenge...))
			return sum[:], nil
		},
	})
	if err != nil {
		panic(err)
	}

	// serve the modules over loopback TCP (see ExampleServer)
	addr, stop := startExampleServer(server)
	defer stop()

	// a client answering the challenge with [PasswordAuth] pulls the file
	client := Client{
		Module:       "data",
		AuthUser:     "zerocool",
		AuthResponse: PasswordAuth("hack the planet"),
		ConnectFunc: func(string) (io.ReadWriter, error) {
			return net.Dial("tcp", addr)
		},
	}
	session, err := client.Connect(nil)
	if err != nil {
		panic(err)
	}
	f, err := session.Open("hello.txt")
	if err != nil {
		panic(err)
	}
	data, err := io.ReadAll(f)
	if err != nil {
		panic(err)
	}
	fmt.Println(string(data))

	// Output:
	// hello, rsync!
}

// ExampleClient pulls a file end-to-end over a TCP connection: the [Client.ConnectFunc] dials the daemon, [Client.Connect] runs the handshake on the resulting connection, and [Session.Open] triggers the transfer.
func ExampleClient() {
	// a single anonymous module, served over loopback TCP
	// (see ExampleServer)
	server, err := NewServer(&ServerModule{
		Name: "data",
		FS: fstest.MapFS{
			"hello.txt": &fstest.MapFile{Data: []byte("hello, rsync!")},
		},
	})
	if err != nil {
		panic(err)
	}
	addr, stop := startExampleServer(server)
	defer stop()

	// the [Client] is plain config: the module to open and a ConnectFunc
	// that produces a connection to the daemon -- here a plain TCP dial,
	// but an SSH session or any io.ReadWriter would work just as well
	client := Client{
		Module: "data",
		ConnectFunc: func(string) (io.ReadWriter, error) {
			return net.Dial("tcp", addr)
		},
	}

	// Connect(nil) calls the ConnectFunc itself, then runs the full
	// handshake
	session, err := client.Connect(nil)
	if err != nil {
		panic(err)
	}

	// Open is a complete, self-contained rsync transfer of the file
	f, err := session.Open("hello.txt")
	if err != nil {
		panic(err)
	}
	data, err := io.ReadAll(f)
	if err != nil {
		panic(err)
	}
	fmt.Println(string(data))

	// Output:
	// hello, rsync!
}

// ExampleClient_auth uses [PasswordAuth] to answer the daemon's auth challenge with the "zerocool" / "hack the planet" credentials, then shows the wrong username and password being rejected.
func ExampleClient_auth() {
	// the module requires the "zerocool" username (the AuthCallback side
	// is shown in ExampleServer_auth)
	server, err := NewServer(&ServerModule{
		Name: "data",
		FS: fstest.MapFS{
			"hello.txt": &fstest.MapFile{Data: []byte("hello, rsync!")},
		},
		AuthCallback: func(username string, challenge []byte) ([]byte, error) {
			if username != "zerocool" {
				return nil, fmt.Errorf("unknown user %q", username)
			}
			sum := md5.Sum(append([]byte("hack the planet"), challenge...))
			return sum[:], nil
		},
	})
	if err != nil {
		panic(err)
	}
	addr, stop := startExampleServer(server)
	defer stop()

	// the client answers the challenge with digest(password + challenge):
	// PasswordAuth wraps the password in an AuthResponse for it
	client := Client{
		Module:       "data",
		AuthUser:     "zerocool",
		AuthResponse: PasswordAuth("hack the planet"),
		ConnectFunc: func(string) (io.ReadWriter, error) {
			return net.Dial("tcp", addr)
		},
	}
	session, err := client.Connect(nil)
	if err != nil {
		panic(err)
	}
	f, err := session.Open("hello.txt")
	if err != nil {
		panic(err)
	}
	data, err := io.ReadAll(f)
	if err != nil {
		panic(err)
	}
	fmt.Println(string(data))

	// the wrong username and password: the server answers the auth
	// response with an @ERROR and the connection is aborted
	bad := Client{
		Module:       "data",
		AuthUser:     "werner",
		AuthResponse: PasswordAuth("my voice is my passport"),
		ConnectFunc: func(string) (io.ReadWriter, error) {
			return net.Dial("tcp", addr)
		},
	}
	if _, err = bad.Connect(nil); err != nil {
		fmt.Println(err)
	}

	// Output:
	// hello, rsync!
	// read server response: Authentication failed
}

// ExampleClient_rootMode creates a [Client] in root mode (Module is empty), where the modules are top-level directories and every FS operation runs on its own connection: it lists the available modules via [Session.Open] on "." and then opens a file within a specific module.
func ExampleClient_rootMode() {
	// two modules over loopback TCP
	server, err := NewServer(
		&ServerModule{
			Name:    "public",
			Comment: "public data",
			FS: fstest.MapFS{
				"welcome.txt": &fstest.MapFile{Data: []byte("welcome!")},
			},
		},
		&ServerModule{
			Name:    "data",
			Comment: "the example data",
			FS: fstest.MapFS{
				"hello.txt": &fstest.MapFile{Data: []byte("hello, rsync!")},
			},
		},
	)
	if err != nil {
		panic(err)
	}
	addr, stop := startExampleServer(server)
	defer stop()

	// root mode: no module, so the modules themselves become top-level
	// directories and each operation dials the daemon fresh
	root, err := Client{
		ConnectFunc: func(string) (io.ReadWriter, error) {
			return net.Dial("tcp", addr)
		},
	}.OpenRoot()
	if err != nil {
		panic(err)
	}

	// list the available modules: each appears once as a directory and
	// again as a hidden ".<module>\t<comment>" symlink that carries the
	// module's comment (the hidden entries are dropped below)
	d, err := root.Open(".")
	if err != nil {
		panic(err)
	}
	entries, err := d.(fs.ReadDirFile).ReadDir(-1)
	if err != nil {
		panic(err)
	}
	for _, e := range entries {
		if !strings.HasPrefix(e.Name(), ".") {
			fmt.Println(e.Name())
		}
	}

	// open a file within a specific module: "data/hello.txt" dials the
	// "data" module and pulls the file
	f, err := root.Open("data/hello.txt")
	if err != nil {
		panic(err)
	}
	data, err := io.ReadAll(f)
	if err != nil {
		panic(err)
	}
	fmt.Println(string(data))

	// Output:
	// data
	// public
	// hello, rsync!
}
