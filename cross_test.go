package rsyncfs

import (
	"bytes"
	"io"
	"io/fs"
	"net"
	"testing"
	"testing/fstest"

	"github.com/values-conflict/go-rsyncfs/protocol"
)

// crossSession creates a Session from a pre-configured server connection.
func crossSession(t *testing.T, conn net.Conn) *Session {
	t.Helper()
	session, err := (&Client{Module: "testmod"}).Connect(conn)
	if err != nil {
		t.Fatalf("Connect failed: %v", err)
	}
	return session
}

// fsWrapper wraps a function into an fs.FS for fstest.TestFS.
type fsWrapper struct {
	doOpen func(name string) (fs.File, error)
}

var _ fs.FS = (*fsWrapper)(nil)

func (w *fsWrapper) Open(name string) (fs.File, error) {
	return w.doOpen(name)
}

func TestCross_FullDirectoryTree(t *testing.T) {
	testFS := fstest.MapFS{
		"root.txt":      {Data: []byte("root file")},
		"alpha.txt":     {Data: []byte("alpha content here")},
		"beta.txt":      {Data: []byte("beta content here")},
		"gamma.txt":     {Data: []byte("gamma content here")},
		"empty.txt":     {Data: []byte{}},
		"sub/nested":    {Data: []byte("nested file content")},
		"sub/deep/more": {Data: []byte("deep file")},
	}

	conn, _ := startServer(t, &ServerModule{Name: "testmod", FS: testFS}, HandleOptions{})
	defer conn.Close()
	session := crossSession(t, conn)

	tests := []struct {
		path string
		want []byte
	}{
		{"root.txt", []byte("root file")},
		{"alpha.txt", []byte("alpha content here")},
		{"gamma.txt", []byte("gamma content here")},
		{"beta.txt", []byte("beta content here")},
		{"empty.txt", []byte{}},
		{"sub/nested", []byte("nested file content")},
		{"sub/deep/more", []byte("deep file")},
	}

	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			f, err := session.Open(tt.path)
			if err != nil {
				t.Fatalf("Open(%q) failed: %v", tt.path, err)
			}
			defer f.Close()

			got, err := io.ReadAll(f)
			if err != nil {
				t.Fatalf("ReadAll(%q) failed: %v", tt.path, err)
			}
			if !bytes.Equal(got, tt.want) {
				t.Errorf("file %q = %q, want %q", tt.path, got, tt.want)
			}
		})
	}
}

func TestCross_DirectoryListing(t *testing.T) {
	conn, _ := startServer(t, &ServerModule{Name: "testmod", FS: fstest.MapFS{
		"file1.txt":      {Data: []byte("one")},
		"file2.txt":      {Data: []byte("two")},
		"dir1/file3.txt": {Data: []byte("three")},
	}}, HandleOptions{})
	defer conn.Close()
	session := crossSession(t, conn)

	df, err := session.Open(".")
	if err != nil {
		t.Fatalf("Open(.) failed: %v", err)
	}
	defer df.Close()

	dirFile, ok := df.(interface {
		ReadDir(n int) ([]fs.DirEntry, error)
	})
	if !ok {
		t.Fatal("root does not support ReadDir")
	}

	entries, err := dirFile.ReadDir(0)
	if err != nil {
		t.Fatalf("ReadDir failed: %v", err)
	}
	if len(entries) != 3 {
		t.Fatalf("expected 3 entries, got %d", len(entries))
	}

	names := make(map[string]bool)
	for _, e := range entries {
		names[e.Name()] = true
	}
	for _, want := range []string{"file1.txt", "file2.txt", "dir1"} {
		if !names[want] {
			t.Errorf("expected %q in root", want)
		}
	}
}

func TestCross_SubDirectoryListing(t *testing.T) {
	conn, _ := startServer(t, &ServerModule{Name: "testmod", FS: fstest.MapFS{
		"a/x.txt": {Data: []byte("x")},
		"a/y.txt": {Data: []byte("y")},
		"b/z.txt": {Data: []byte("z")},
	}}, HandleOptions{})
	defer conn.Close()
	session := crossSession(t, conn)

	df, err := session.Open("a")
	if err != nil {
		t.Fatalf("Open(a) failed: %v", err)
	}
	defer df.Close()

	dirFile, ok := df.(interface {
		ReadDir(n int) ([]fs.DirEntry, error)
	})
	if !ok {
		t.Fatal("a does not support ReadDir")
	}

	entries, err := dirFile.ReadDir(0)
	if err != nil {
		t.Fatalf("ReadDir failed: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("expected 2 entries in a/, got %d", len(entries))
	}
}

func TestCross_Symlinks(t *testing.T) {
	conn, _ := startServer(t, &ServerModule{Name: "testmod", FS: fstest.MapFS{
		"target.txt": {Data: []byte("target content")},
		"link.txt":   {Data: []byte("target.txt"), Mode: fs.ModeSymlink | 0o777},
	}}, HandleOptions{})
	defer conn.Close()
	session := crossSession(t, conn)

	f, err := session.Open("link.txt")
	if err != nil {
		t.Fatalf("Open(link.txt) failed: %v", err)
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		t.Fatalf("Stat failed: %v", err)
	}
	if info.Mode()&fs.ModeSymlink == 0 {
		t.Errorf("expected symlink mode, got %v", info.Mode())
	}
}

func TestCross_LargeFile(t *testing.T) {
	fileData := make([]byte, 2000)
	for i := range fileData {
		fileData[i] = byte(i % 256)
	}

	conn, _ := startServer(t, &ServerModule{Name: "testmod", FS: fstest.MapFS{"large.bin": {Data: fileData}}}, HandleOptions{})
	defer conn.Close()
	session := crossSession(t, conn)

	f, err := session.Open("large.bin")
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	defer f.Close()

	got, err := io.ReadAll(f)
	if err != nil {
		t.Fatalf("ReadAll failed: %v", err)
	}
	if !bytes.Equal(got, fileData) {
		t.Errorf("mismatch: got %d bytes, want %d", len(got), len(fileData))
	}
}

func TestCross_DirectoryStat(t *testing.T) {
	conn, _ := startServer(t, &ServerModule{Name: "testmod", FS: fstest.MapFS{"subdir/file.txt": {Data: []byte("hello")}}}, HandleOptions{})
	defer conn.Close()
	session := crossSession(t, conn)

	df, err := session.Open("subdir")
	if err != nil {
		t.Fatalf("Open(subdir) failed: %v", err)
	}
	defer df.Close()

	info, err := df.Stat()
	if err != nil {
		t.Fatalf("Stat failed: %v", err)
	}
	if !info.IsDir() {
		t.Error("expected subdir to be a directory")
	}
}

func TestCross_FileNotFound(t *testing.T) {
	conn, _ := startServer(t, &ServerModule{Name: "testmod", FS: fstest.MapFS{"exists.txt": {Data: []byte("hello")}}}, HandleOptions{})
	defer conn.Close()
	session := crossSession(t, conn)

	_, err := session.Open("does_not_exist.txt")
	if err == nil {
		t.Fatal("expected error for non-existent file")
	}
}

func TestCross_LeadingSlashRejected(t *testing.T) {
	conn, _ := startServer(t, &ServerModule{Name: "testmod", FS: fstest.MapFS{"file.txt": {Data: []byte("hello")}}}, HandleOptions{})
	defer conn.Close()
	session := crossSession(t, conn)

	_, err := session.Open("/file.txt")
	if err == nil {
		t.Fatal("expected error for path with leading slash")
	}
}

func TestCross_FstestTestFS(t *testing.T) {
	conn, _ := startServer(t, &ServerModule{Name: "testmod", FS: fstest.MapFS{
		"root.txt":  {Data: []byte("root level file")},
		"empty.txt": {Data: []byte{}},
	}}, HandleOptions{})
	defer conn.Close()
	session := crossSession(t, conn)

	if err := fstest.TestFS(&fsWrapper{doOpen: func(name string) (fs.File, error) {
		return session.Open(name)
	}}, "root.txt", "empty.txt"); err != nil {
		t.Fatalf("TestFS failed: %v", err)
	}
}

func TestCross_FileInfo(t *testing.T) {
	fileData := []byte("hello world")

	conn, _ := startServer(t, &ServerModule{
		Name: "testmod",
		FS:   fstest.MapFS{"file.txt": {Data: fileData}},
	}, HandleOptions{})
	defer conn.Close()
	session := crossSession(t, conn)

	f, err := session.Open("file.txt")
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		t.Fatalf("Stat failed: %v", err)
	}
	if info.Name() != "file.txt" {
		t.Errorf("Name = %q, want %q", info.Name(), "file.txt")
	}
	if info.Size() != int64(len(fileData)) {
		t.Errorf("Size = %d, want %d", info.Size(), len(fileData))
	}
	if !info.Mode().IsRegular() {
		t.Errorf("Mode = %v, expected regular file", info.Mode())
	}
}

func TestCross_VersionNegotiation(t *testing.T) {
	mod := &ServerModule{Name: "testmod", FS: fstest.MapFS{}}
	srv, _ := NewServer(mod)

	tests := []struct{ clientVer, serverVer, wantVer int }{
		{32, 32, 32}, {30, 32, 30}, {32, 30, 30}, {28, 28, 28},
	}

	for _, tt := range tests {
		t.Run("", func(t *testing.T) {
			serverConn, clientConn := net.Pipe()
			go func() {
				defer serverConn.Close()
				_ = srv.HandleConnection(serverConn, HandleOptions{
					LocalGreeting: protocol.Greeting{Version: tt.serverVer, SubProtocol: 0, Digests: []string{"md5"}},
				})
			}()

			session, err := (&Client{
				Module:   "testmod",
				Greeting: protocol.Greeting{Version: tt.clientVer, SubProtocol: 0, Digests: []string{"md5"}},
			}).Connect(clientConn)
			if err != nil {
				t.Fatalf("Connect failed: %v", err)
			}
			if session.version != tt.wantVer {
				t.Errorf("version = %d, want %d", session.version, tt.wantVer)
			}
			clientConn.Close()
		})
	}
}

func TestCross_RootMode_ModuleListing(t *testing.T) {
	mod1 := &ServerModule{Name: "alpha", Comment: "Alpha", FS: fstest.MapFS{"a.txt": {Data: []byte("a")}}}
	mod2 := &ServerModule{Name: "beta", Comment: "Beta", FS: fstest.MapFS{"b.txt": {Data: []byte("b")}}}
	srv, _ := NewServer(mod1, mod2)

	client := &Client{
		ConnectFunc: func(moduleName string) (io.ReadWriter, error) {
			serverConn, clientConn := net.Pipe()
			go func() {
				defer serverConn.Close()
				_ = srv.HandleConnection(serverConn, HandleOptions{})
			}()
			return clientConn, nil
		},
	}

	session, err := client.OpenRoot()
	if err != nil {
		t.Fatalf("OpenRoot failed: %v", err)
	}

	rootFile, err := session.Open(".")
	if err != nil {
		t.Fatalf("Open(.) failed: %v", err)
	}
	defer rootFile.Close()

	dirFile, ok := rootFile.(interface {
		ReadDir(n int) ([]fs.DirEntry, error)
	})
	if !ok {
		t.Fatal("root does not support ReadDir")
	}

	entries, err := dirFile.ReadDir(0)
	if err != nil {
		t.Fatalf("ReadDir failed: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("expected 2 modules, got %d", len(entries))
	}
}

func TestCross_RootMode_ModuleFileAccess(t *testing.T) {
	mod := &ServerModule{Name: "mymod", FS: fstest.MapFS{"file.txt": {Data: []byte("hello from mymod")}}}
	srv, _ := NewServer(mod)

	client := &Client{
		ConnectFunc: func(moduleName string) (io.ReadWriter, error) {
			serverConn, clientConn := net.Pipe()
			go func() {
				defer serverConn.Close()
				_ = srv.HandleConnection(serverConn, HandleOptions{})
			}()
			return clientConn, nil
		},
	}

	session, err := client.OpenRoot()
	if err != nil {
		t.Fatalf("OpenRoot failed: %v", err)
	}

	f, err := session.Open("mymod/file.txt")
	if err != nil {
		t.Fatalf("Open(mymod/file.txt) failed: %v", err)
	}
	defer f.Close()

	data, err := io.ReadAll(f)
	if err != nil {
		t.Fatalf("ReadAll failed: %v", err)
	}
	if !bytes.Equal(data, []byte("hello from mymod")) {
		t.Errorf("content = %q, want %q", data, "hello from mymod")
	}
}

func TestCross_EmptyModule(t *testing.T) {
	conn, _ := startServer(t, &ServerModule{Name: "testmod", FS: fstest.MapFS{}}, HandleOptions{})
	defer conn.Close()
	session := crossSession(t, conn)

	df, err := session.Open(".")
	if err != nil {
		t.Fatalf("Open(.) failed: %v", err)
	}
	defer df.Close()

	dirFile, ok := df.(interface {
		ReadDir(n int) ([]fs.DirEntry, error)
	})
	if !ok {
		t.Fatal("does not support ReadDir")
	}

	entries, err := dirFile.ReadDir(0)
	if err != nil {
		t.Fatalf("ReadDir failed: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("expected 0 entries, got %d", len(entries))
	}
}

func TestCross_FileContentIntegrity(t *testing.T) {
	var fileData bytes.Buffer
	for i := 0; i < 1024; i++ {
		fileData.WriteByte(byte(i % 256))
	}

	conn, _ := startServer(t, &ServerModule{Name: "testmod", FS: fstest.MapFS{"binary.bin": {Data: fileData.Bytes()}}}, HandleOptions{})
	defer conn.Close()
	session := crossSession(t, conn)

	f, err := session.Open("binary.bin")
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	defer f.Close()

	got, err := io.ReadAll(f)
	if err != nil {
		t.Fatalf("ReadAll failed: %v", err)
	}
	if !bytes.Equal(got, fileData.Bytes()) {
		t.Errorf("mismatch: got %d bytes, want %d", len(got), len(fileData.Bytes()))
	}
}

func TestCross_ReadAtEOF(t *testing.T) {
	data := []byte("hello")

	conn, _ := startServer(t, &ServerModule{Name: "testmod", FS: fstest.MapFS{"file.txt": {Data: data}}}, HandleOptions{})
	defer conn.Close()
	session := crossSession(t, conn)

	f, err := session.Open("file.txt")
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	defer f.Close()

	reader, ok := f.(io.ReaderAt)
	if !ok {
		t.Fatal("file does not support ReadAt")
	}

	// read at exact end: should return (0, EOF)
	n, err := reader.ReadAt(nil, int64(len(data)))
	if n != 0 || err != io.EOF {
		t.Errorf("ReadAt(end, nil) = %d, %v, want 0, EOF", n, err)
	}

	// read past end: should return (0, EOF)
	n, err = reader.ReadAt(make([]byte, 10), int64(len(data)))
	if n != 0 || err != io.EOF {
		t.Errorf("ReadAt(end, buf) = %d, %v, want 0, EOF", n, err)
	}

	// read last byte: should return (1, EOF)
	var buf [1]byte
	n, err = reader.ReadAt(buf[:], int64(len(data)-1))
	if n != 1 || err != io.EOF {
		t.Errorf("ReadAt(last) = %d, %v, want 1, EOF", n, err)
	}
}

func TestCross_MultiFileSingleConnection(t *testing.T) {
	conn, _ := startServer(t, &ServerModule{Name: "testmod", FS: fstest.MapFS{
		"first.txt":  {Data: []byte("first file content")},
		"second.txt": {Data: []byte("second file content")},
		"third.txt":  {Data: []byte("third file content")},
	}}, HandleOptions{})
	defer conn.Close()
	session := crossSession(t, conn)

	for _, tt := range []struct {
		path string
		want []byte
	}{
		{"first.txt", []byte("first file content")},
		{"second.txt", []byte("second file content")},
		{"third.txt", []byte("third file content")},
	} {
		t.Run(tt.path, func(t *testing.T) {
			f, err := session.Open(tt.path)
			if err != nil {
				t.Fatalf("Open(%q) failed: %v", tt.path, err)
			}
			defer f.Close()

			got, err := io.ReadAll(f)
			if err != nil {
				t.Fatalf("ReadAll(%q) failed: %v", tt.path, err)
			}
			if !bytes.Equal(got, tt.want) {
				t.Errorf("file %q = %q, want %q", tt.path, got, tt.want)
			}
		})
	}
}

func TestCross_ProtocolVersions(t *testing.T) {
	testFS := fstest.MapFS{
		"file.txt": {Data: []byte("test data for protocol version testing")},
	}
	mod := &ServerModule{Name: "testmod", FS: testFS}
	srv, _ := NewServer(mod)

	// Only test proto 30-32 (older versions have different file list/selector formats)
	for _, version := range []int{30, 31, 32} {
		t.Run("", func(t *testing.T) {
			serverConn, clientConn := net.Pipe()
			go func() {
				defer serverConn.Close()
				_ = srv.HandleConnection(serverConn, HandleOptions{
					LocalGreeting: protocol.Greeting{Version: version, SubProtocol: 0, Digests: []string{"md5"}},
				})
			}()

			session, err := (&Client{
				Module:   "testmod",
				Greeting: protocol.Greeting{Version: version, SubProtocol: 0, Digests: []string{"md5"}},
			}).Connect(clientConn)
			if err != nil {
				t.Fatalf("Connect failed: %v", err)
			}
			defer clientConn.Close()

			f, err := session.Open("file.txt")
			if err != nil {
				t.Fatalf("Open(file.txt) failed: %v", err)
			}
			defer f.Close()

			got, err := io.ReadAll(f)
			if err != nil {
				t.Fatalf("ReadAll failed: %v", err)
			}
			if !bytes.Equal(got, testFS["file.txt"].Data) {
				t.Errorf("file data mismatch: got %d bytes, want %d", len(got), len(testFS["file.txt"].Data))
			}
		})
	}
}
