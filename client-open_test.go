package rsyncfs

import (
	"bytes"
	"errors"
	"io"
	"io/fs"
	"strconv"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/values-conflict/go-rsyncfs/protocol"
)

// drainServer waits for the server goroutine to finish and reports its
// result without failing the test (a clean transfer can leave the daemon
// seeing a peer close, which is not an error condition here).
func drainServer(t *testing.T, done <-chan error) {
	t.Helper()
	select {
	case serr := <-done:
		t.Logf("server returned: %v", serr)
	case <-time.After(10 * time.Second):
		t.Fatal("server goroutine did not exit")
	}
}

// openModuleFile connects to a module served by s and reads the whole file
// at path, returning its content.
func openModuleFile(t *testing.T, s *Server, mod, path string) ([]byte, *Session, io.ReadWriteCloser, <-chan error) {
	t.Helper()
	client, done := startTestServer(t, s)
	c := Client{Module: mod}
	sess, err := c.Connect(client)
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	f, err := sess.Open(path)
	if err != nil {
		t.Fatalf("Open(%q): %v", path, err)
	}
	defer f.Close()
	data, err := io.ReadAll(f)
	if err != nil {
		t.Fatalf("ReadAll(%q): %v", path, err)
	}
	return data, sess, client, done
}

func TestOpen_File(t *testing.T) {
	content := []byte("hello from the daemon\n")
	mod := &ServerModule{Name: "testmod", FS: fstest.MapFS{
		"file.txt": &fstest.MapFile{Data: content, Mode: 0o644},
	}}
	s, err := NewServer(mod)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	data, _, client, done := openModuleFile(t, s, "testmod", "file.txt")
	defer client.Close()
	drainServer(t, done)

	if !bytes.Equal(data, content) {
		t.Errorf("content = %q, want %q", data, content)
	}
}

func TestOpen_FileStat(t *testing.T) {
	mod := &ServerModule{Name: "testmod", FS: fstest.MapFS{
		"file.txt": &fstest.MapFile{Data: []byte("0123456789"), Mode: 0o640},
	}}
	s, err := NewServer(mod)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	client, done := startTestServer(t, s)

	c := Client{Module: "testmod"}
	sess, err := c.Connect(client)
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	f, err := sess.Open("file.txt")
	if err != nil {
		client.Close()
		t.Fatalf("Open: %v", err)
	}
	info, err := f.Stat()
	f.Close()
	if err != nil {
		client.Close()
		t.Fatalf("Stat: %v", err)
	}
	if info.Name() != "file.txt" {
		t.Errorf("Name = %q, want file.txt", info.Name())
	}
	if info.Size() != 10 {
		t.Errorf("Size = %d, want 10", info.Size())
	}
	if info.Mode().Perm() != 0o640 {
		t.Errorf("Mode perm = %o, want 640", info.Mode().Perm())
	}
	if info.IsDir() {
		t.Error("IsDir = true, want false")
	}
	client.Close()
	drainServer(t, done)
}

func TestOpen_EmptyFile(t *testing.T) {
	mod := &ServerModule{Name: "testmod", FS: fstest.MapFS{
		"empty.txt": &fstest.MapFile{Data: []byte{}, Mode: 0o644},
	}}
	s, err := NewServer(mod)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	data, _, client, done := openModuleFile(t, s, "testmod", "empty.txt")
	defer client.Close()
	drainServer(t, done)

	if len(data) != 0 {
		t.Errorf("empty file = %d bytes, want 0", len(data))
	}
}

func TestOpen_LargeFile(t *testing.T) {
	// Larger than the 32KB delta chunk so the data spans multiple
	// literal tokens.
	content := make([]byte, 200_000)
	for i := range content {
		content[i] = byte(i % 251)
	}
	mod := &ServerModule{Name: "testmod", FS: fstest.MapFS{
		"large.bin": &fstest.MapFile{Data: content, Mode: 0o644},
	}}
	s, err := NewServer(mod)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	data, _, client, done := openModuleFile(t, s, "testmod", "large.bin")
	defer client.Close()
	drainServer(t, done)

	if !bytes.Equal(data, content) {
		t.Errorf("large file: got %d bytes, want %d (first mismatch at content)", len(data), len(content))
	}
}

func TestOpen_Directory(t *testing.T) {
	mod := &ServerModule{Name: "testmod", FS: fstest.MapFS{
		"sub/inner.txt": &fstest.MapFile{Data: []byte("inner"), Mode: 0o600},
		"file.txt":      &fstest.MapFile{Data: []byte("file"), Mode: 0o644},
	}}
	s, err := NewServer(mod)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	client, done := startTestServer(t, s)

	c := Client{Module: "testmod"}
	sess, err := c.Connect(client)
	if err != nil {
		client.Close()
		t.Fatalf("Connect: %v", err)
	}
	df, err := sess.Open(".")
	if err != nil {
		client.Close()
		t.Fatalf("Open(.): %v", err)
	}
	defer df.Close()

	rd, ok := df.(interface {
		ReadDir(int) ([]fs.DirEntry, error)
	})
	if !ok {
		t.Fatal("opened root does not implement ReadDir")
	}
	entries, err := rd.ReadDir(0)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	names := map[string]bool{}
	for _, e := range entries {
		names[e.Name()] = e.IsDir()
	}
	if isDir, ok := names["sub"]; !ok || !isDir {
		t.Errorf("expected sub/ (a directory) in root listing, got %v", names)
	}
	if isDir, ok := names["file.txt"]; !ok || isDir {
		t.Errorf("expected file.txt (a file) in root listing, got %v", names)
	}
	client.Close()
	drainServer(t, done)
}

func TestOpen_SubDirectory(t *testing.T) {
	mod := &ServerModule{Name: "testmod", FS: fstest.MapFS{
		"sub/inner.txt":  &fstest.MapFile{Data: []byte("inner"), Mode: 0o600},
		"sub/second.txt": &fstest.MapFile{Data: []byte("second"), Mode: 0o600},
		"other.txt":      &fstest.MapFile{Data: []byte("other"), Mode: 0o644},
	}}
	s, err := NewServer(mod)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	client, done := startTestServer(t, s)

	c := Client{Module: "testmod"}
	sess, err := c.Connect(client)
	if err != nil {
		client.Close()
		t.Fatalf("Connect: %v", err)
	}
	df, err := sess.Open("sub")
	if err != nil {
		client.Close()
		t.Fatalf("Open(sub): %v", err)
	}
	defer df.Close()

	rd := df.(interface {
		ReadDir(int) ([]fs.DirEntry, error)
	})
	entries, err := rd.ReadDir(0)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("sub has %d entries, want 2", len(entries))
	}
	// ReadDir returns ascending name order
	if entries[0].Name() != "inner.txt" || entries[1].Name() != "second.txt" {
		t.Errorf("entry order = %q, %q; want inner.txt, second.txt", entries[0].Name(), entries[1].Name())
	}
	client.Close()
	drainServer(t, done)
}

func TestOpen_DirReadToEnd(t *testing.T) {
	mod := &ServerModule{Name: "testmod", FS: fstest.MapFS{
		"a.txt": &fstest.MapFile{Data: []byte("a"), Mode: 0o644},
		"b.txt": &fstest.MapFile{Data: []byte("b"), Mode: 0o644},
	}}
	s, err := NewServer(mod)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	client, done := startTestServer(t, s)

	c := Client{Module: "testmod"}
	sess, err := c.Connect(client)
	if err != nil {
		client.Close()
		t.Fatalf("Connect: %v", err)
	}
	df, err := sess.Open(".")
	if err != nil {
		client.Close()
		t.Fatalf("Open: %v", err)
	}
	defer df.Close()
	rd := df.(interface {
		ReadDir(int) ([]fs.DirEntry, error)
	})

	// n > 0: read one at a time until io.EOF
	seen := 0
	for {
		entries, err := rd.ReadDir(1)
		if len(entries) > 0 {
			seen += len(entries)
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("ReadDir: %v", err)
		}
	}
	if seen != 2 {
		t.Errorf("saw %d entries, want 2", seen)
	}
	client.Close()
	drainServer(t, done)
}

func TestOpen_NotExist(t *testing.T) {
	mod := &ServerModule{Name: "testmod", FS: fstest.MapFS{
		"file.txt": &fstest.MapFile{Data: []byte("x"), Mode: 0o644},
	}}
	s, err := NewServer(mod)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	client, done := startTestServer(t, s)

	c := Client{Module: "testmod"}
	sess, err := c.Connect(client)
	if err != nil {
		client.Close()
		t.Fatalf("Connect: %v", err)
	}
	_, err = sess.Open("nonexistent.txt")
	if !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("Open(nonexistent) err = %v, want ErrNotExist", err)
	}
	client.Close()
	drainServer(t, done)
}

func TestOpen_InvalidPath(t *testing.T) {
	mod := &ServerModule{Name: "testmod", FS: fstest.MapFS{
		"file.txt": &fstest.MapFile{Data: []byte("x"), Mode: 0o644},
	}}
	s, err := NewServer(mod)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	client, done := startTestServer(t, s)

	c := Client{Module: "testmod"}
	sess, err := c.Connect(client)
	if err != nil {
		client.Close()
		t.Fatalf("Connect: %v", err)
	}
	for _, name := range []string{"/abs", "a//b", ""} {
		if _, err := sess.Open(name); !errors.Is(err, fs.ErrInvalid) {
			t.Errorf("Open(%q) err = %v, want ErrInvalid", name, err)
		}
	}
	client.Close()
	drainServer(t, done)
}

// TestOpen_Symlink verifies that a symlink entry reports its mode and that
// fs.ReadLink resolves its target.  Each operation (Open, ReadLink) consumes
// its connection, so ConnectFunc re-establishes one per operation.
func TestOpen_Symlink(t *testing.T) {
	mod := &ServerModule{Name: "testmod", FS: fstest.MapFS{
		"target.txt": &fstest.MapFile{Data: []byte("target content"), Mode: 0o644},
		"link.txt":   &fstest.MapFile{Data: []byte("target.txt"), Mode: fs.ModeSymlink | 0o777},
	}}
	s, err := NewServer(mod)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	doneChs := make(chan error, 4)
	c := Client{
		Module: "testmod",
		ConnectFunc: func(moduleName string) (io.ReadWriter, error) {
			serverEnd, clientEnd := BufPipe()
			go func() {
				defer serverEnd.Close()
				doneChs <- s.HandleConnection(serverEnd)
			}()
			return clientEnd, nil
		},
	}
	sess, err := c.Connect(nil)
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}

	f, err := sess.Open("link.txt")
	if err != nil {
		t.Fatalf("Open(link.txt): %v", err)
	}
	info, err := f.Stat()
	f.Close()
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if info.Mode()&fs.ModeSymlink == 0 {
		t.Errorf("mode = %v, want symlink bit set", info.Mode())
	}

	// fs.ReadLink resolves the target on a fresh connection
	target, err := fs.ReadLink(sess, "link.txt")
	if err != nil {
		t.Fatalf("ReadLink: %v", err)
	}
	if target != "target.txt" {
		t.Errorf("ReadLink = %q, want target.txt", target)
	}

	// Two connections: one from Connect, one for ReadLink (Open reuses the
	// Connect connection).
	for i := 0; i < 2; i++ {
		select {
		case d := <-doneChs:
			if d != nil {
				t.Logf("server: %v", d)
			}
		case <-time.After(10 * time.Second):
			t.Fatalf("server connection %d did not finish", i+1)
		}
	}
}

// TestOpen_MultiFileOverConnectFunc verifies that each file open is its own
// transfer session: with a ConnectFunc, opening several files in sequence
// re-establishes a connection per open.
func TestOpen_MultiFileOverConnectFunc(t *testing.T) {
	mod := &ServerModule{Name: "testmod", FS: fstest.MapFS{
		"a.txt": &fstest.MapFile{Data: []byte("alpha"), Mode: 0o644},
		"b.txt": &fstest.MapFile{Data: []byte("beta"), Mode: 0o644},
	}}
	s, err := NewServer(mod)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	doneChs := make(chan error, 8)
	c := Client{
		Module: "testmod",
		ConnectFunc: func(moduleName string) (io.ReadWriter, error) {
			serverEnd, clientEnd := BufPipe()
			go func() {
				defer serverEnd.Close()
				doneChs <- s.HandleConnection(serverEnd)
			}()
			return clientEnd, nil
		},
	}
	sess, err := c.Connect(nil)
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}

	for _, want := range []struct{ path, data string }{
		{"a.txt", "alpha"},
		{"b.txt", "beta"},
		{"a.txt", "alpha"},
	} {
		f, err := sess.Open(want.path)
		if err != nil {
			t.Fatalf("Open(%q): %v", want.path, err)
		}
		data, err := io.ReadAll(f)
		f.Close()
		if err != nil {
			t.Fatalf("ReadAll(%q): %v", want.path, err)
		}
		if string(data) != want.data {
			t.Errorf("Open(%q) = %q, want %q", want.path, data, want.data)
		}
	}
	// One connection from Connect plus one per subsequent Open (the first
	// Open reuses the Connect connection).
	for i := 0; i < 3; i++ {
		select {
		case d := <-doneChs:
			if d != nil {
				t.Logf("server: %v", d)
			}
		case <-time.After(10 * time.Second):
			t.Fatalf("server connection %d did not finish", i+1)
		}
	}
}

// --- root mode ------------------------------------------------------------

func TestOpenRoot_List(t *testing.T) {
	mods := []*ServerModule{
		{Name: "alpha", Comment: "first module", FS: fstest.MapFS{"a.txt": {Data: []byte("a")}}},
		{Name: "beta", Comment: "second module", FS: fstest.MapFS{"b.txt": {Data: []byte("b")}}},
	}
	s, err := NewServer(mods...)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	c := Client{
		ConnectFunc: func(moduleName string) (io.ReadWriter, error) {
			serverEnd, clientEnd := BufPipe()
			go func() {
				defer serverEnd.Close()
				_ = s.HandleConnection(serverEnd)
			}()
			return clientEnd, nil
		},
	}
	sess, err := c.OpenRoot()
	if err != nil {
		t.Fatalf("OpenRoot: %v", err)
	}

	df, err := sess.Open(".")
	if err != nil {
		t.Fatalf("Open(.): %v", err)
	}
	defer df.Close()
	rd := df.(interface {
		ReadDir(int) ([]fs.DirEntry, error)
	})
	entries, err := rd.ReadDir(0)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	joined := strings.Join(names, ",")
	for _, want := range []string{"alpha", ".alpha\tfirst module", "beta", ".beta\tsecond module"} {
		if !strings.Contains(joined, want) {
			t.Errorf("root listing %q missing %q", joined, want)
		}
	}
}

func TestOpenRoot_ModuleFile(t *testing.T) {
	mod := &ServerModule{Name: "mymod", Comment: "c", FS: fstest.MapFS{
		"file.txt": &fstest.MapFile{Data: []byte("hello from mymod"), Mode: 0o644},
	}}
	s, err := NewServer(mod)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	c := Client{
		ConnectFunc: func(moduleName string) (io.ReadWriter, error) {
			serverEnd, clientEnd := BufPipe()
			go func() {
				defer serverEnd.Close()
				_ = s.HandleConnection(serverEnd)
			}()
			return clientEnd, nil
		},
	}
	sess, err := c.OpenRoot()
	if err != nil {
		t.Fatalf("OpenRoot: %v", err)
	}

	f, err := sess.Open("mymod/file.txt")
	if err != nil {
		t.Fatalf("Open(mymod/file.txt): %v", err)
	}
	defer f.Close()
	data, err := io.ReadAll(f)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if string(data) != "hello from mymod" {
		t.Errorf("content = %q, want %q", data, "hello from mymod")
	}
}

func TestOpenRoot_ModuleDir(t *testing.T) {
	mod := &ServerModule{Name: "mymod", Comment: "c", FS: fstest.MapFS{
		"sub/inner.txt": &fstest.MapFile{Data: []byte("inner"), Mode: 0o644},
		"top.txt":       &fstest.MapFile{Data: []byte("top"), Mode: 0o644},
	}}
	s, err := NewServer(mod)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	c := Client{
		ConnectFunc: func(moduleName string) (io.ReadWriter, error) {
			serverEnd, clientEnd := BufPipe()
			go func() {
				defer serverEnd.Close()
				_ = s.HandleConnection(serverEnd)
			}()
			return clientEnd, nil
		},
	}
	sess, err := c.OpenRoot()
	if err != nil {
		t.Fatalf("OpenRoot: %v", err)
	}

	df, err := sess.Open("mymod")
	if err != nil {
		t.Fatalf("Open(mymod): %v", err)
	}
	defer df.Close()
	rd := df.(interface {
		ReadDir(int) ([]fs.DirEntry, error)
	})
	entries, err := rd.ReadDir(0)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("mymod has %d entries, want 2", len(entries))
	}
}

// TestOpen_VersionMatrix runs a file open against the in-repo server at
// several protocol versions, exercising the version-gated wire paths
// (raw vs mux output, int32 vs compressed NDX, longint vs varlong stats).
func TestOpen_VersionMatrix(t *testing.T) {
	content := []byte("version matrix payload\n")
	for _, ver := range []int{20, 23, 27, 29, 30, 31, 32} {
		t.Run("v"+strconv.Itoa(ver), func(t *testing.T) {
			mod := &ServerModule{Name: "testmod", FS: fstest.MapFS{
				"file.txt": &fstest.MapFile{Data: content, Mode: 0o644},
			}}
			s, err := NewServer(mod)
			if err != nil {
				t.Fatalf("NewServer: %v", err)
			}
			client, done := startTestServer(t, s)
			defer client.Close()

			c := Client{
				Module:   "testmod",
				Greeting: protocol.Greeting{Version: ver},
			}
			sess, err := c.Connect(client)
			if err != nil {
				t.Fatalf("Connect(v%d): %v", ver, err)
			}
			if sess.version != ver {
				t.Fatalf("version = %d, want %d", sess.version, ver)
			}
			f, err := sess.Open("file.txt")
			if err != nil {
				t.Fatalf("Open(v%d): %v", ver, err)
			}
			data, err := io.ReadAll(f)
			f.Close()
			if err != nil {
				t.Fatalf("ReadAll(v%d): %v", ver, err)
			}
			if !bytes.Equal(data, content) {
				t.Errorf("v%d content mismatch: got %d bytes", ver, len(data))
			}
			drainServer(t, done)
		})
	}
}
