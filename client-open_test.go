package rsyncfs

import (
	"bytes"
	"errors"
	"io"
	"io/fs"
	"net"
	"testing"
	"testing/fstest"

	"github.com/values-conflict/go-rsyncfs/protocol"
	"github.com/values-conflict/go-rsyncfs/protocol/mux"
)

// setupTestServer creates a server goroutine connected via net.Pipe and returns the client side.
// The server handles the handshake, sends the file list, and optionally handles file transfers.
// Callers should close the returned connection when done.
func setupTestServer(t *testing.T, mod *ServerModule, handleFiles bool) net.Conn {
	t.Helper()
	srv, err := NewServer(mod)
	if err != nil {
		t.Fatalf("NewServer failed: %v", err)
	}

	serverConn, clientConn := net.Pipe()

	go func() {
		defer serverConn.Close()

		result, err := srv.HandleConnection(serverConn, HandleOptions{
			LocalGreeting: protocol.Greeting{Version: 32, SubProtocol: 0, Digests: []string{"md5", "md4"}},
		})
		if err != nil {
			return
		}

		// send file list
		mw := mux.NewWriter(serverConn)
		if err := sendFileList(mw, result.Module.FS, ".", result.Version, false); err != nil {
			return
		}

		if !handleFiles {
			return
		}

		// handle file transfer requests
		mr := mux.NewReader(serverConn)
		for {
			// read selector (raw, not via mux)
			var ndxBuf [1]byte
			if _, err := serverConn.Read(ndxBuf[:]); err != nil {
				return
			}
			var iflagsBuf [2]byte
			if result.Version >= 29 {
				if _, err := io.ReadFull(serverConn, iflagsBuf[:]); err != nil {
					return
				}
			}

			// figure out which file was requested by parsing the file list
			entries, _ := walkFS(result.Module.FS, ".")
			var targetFile string
			for _, e := range entries {
				if e.name != "." {
					targetFile = e.name
					break
				}
			}

			f, err := result.Module.FS.Open(targetFile)
			if err != nil {
				return
			}

			if err := sendFile(mr, mw, f, result.Version); err != nil {
				f.Close()
				return
			}
			f.Close()
		}
	}()

	return clientConn
}

func TestClientOpen_File(t *testing.T) {
	fileData := []byte("hello world")
	mod := &ServerModule{
		Name: "testmod",
		FS: fstest.MapFS{
			"file.txt": {Data: fileData},
		},
	}

	conn := setupTestServer(t, mod, true)
	defer conn.Close()

	client := &Client{Module: "testmod"}
	session, err := client.Connect(conn)
	if err != nil {
		t.Fatalf("Connect failed: %v", err)
	}

	f, err := session.Open("file.txt")
	if err != nil {
		t.Fatalf("Open(file.txt) failed: %v", err)
	}
	defer f.Close()

	data, err := io.ReadAll(f)
	if err != nil {
		t.Fatalf("ReadAll failed: %v", err)
	}
	if !bytes.Equal(data, fileData) {
		t.Errorf("file content = %q, want %q", data, fileData)
	}

	info, err := f.Stat()
	if err != nil {
		t.Fatalf("Stat failed: %v", err)
	}
	if info.Name() != "file.txt" {
		t.Errorf("file name = %q, want %q", info.Name(), "file.txt")
	}
}

func TestClientOpen_Directory(t *testing.T) {
	mod := &ServerModule{
		Name: "testmod",
		FS: fstest.MapFS{
			"dir1/file1.txt": {Data: []byte("file1")},
			"dir1/file2.txt": {Data: []byte("file2")},
			"file3.txt":      {Data: []byte("file3")},
		},
	}

	conn := setupTestServer(t, mod, false)
	defer conn.Close()

	client := &Client{Module: "testmod"}
	session, err := client.Connect(conn)
	if err != nil {
		t.Fatalf("Connect failed: %v", err)
	}

	df, err := session.Open(".")
	if err != nil {
		t.Fatalf("Open(.) failed: %v", err)
	}
	defer df.Close()

	dirFile, ok := df.(interface {
		ReadDir(n int) ([]fs.DirEntry, error)
	})
	if !ok {
		t.Fatal("opened file does not support ReadDir")
	}

	entries, err := dirFile.ReadDir(0)
	if err != nil {
		t.Fatalf("ReadDir failed: %v", err)
	}

	names := make([]string, len(entries))
	for i, e := range entries {
		names[i] = e.Name()
	}

	foundDir1 := false
	foundFile3 := false
	for _, name := range names {
		if name == "dir1" {
			foundDir1 = true
		}
		if name == "file3.txt" {
			foundFile3 = true
		}
	}
	if !foundDir1 {
		t.Error("expected dir1 in root directory entries")
	}
	if !foundFile3 {
		t.Error("expected file3.txt in root directory entries")
	}
}

func TestClientOpen_SubDirectory(t *testing.T) {
	mod := &ServerModule{
		Name: "testmod",
		FS: fstest.MapFS{
			"dir1/file1.txt": {Data: []byte("file1")},
			"dir1/file2.txt": {Data: []byte("file2")},
		},
	}

	conn := setupTestServer(t, mod, false)
	defer conn.Close()

	client := &Client{Module: "testmod"}
	session, err := client.Connect(conn)
	if err != nil {
		t.Fatalf("Connect failed: %v", err)
	}

	df, err := session.Open("dir1")
	if err != nil {
		t.Fatalf("Open(dir1) failed: %v", err)
	}
	defer df.Close()

	dirFile, ok := df.(interface {
		ReadDir(n int) ([]fs.DirEntry, error)
	})
	if !ok {
		t.Fatal("opened file does not support ReadDir")
	}

	entries, err := dirFile.ReadDir(0)
	if err != nil {
		t.Fatalf("ReadDir failed: %v", err)
	}

	if len(entries) != 2 {
		t.Errorf("expected 2 entries in dir1, got %d", len(entries))
	}
}

func TestClientOpen_NotExist(t *testing.T) {
	mod := &ServerModule{
		Name: "testmod",
		FS:   fstest.MapFS{"file.txt": {Data: []byte("hello")}},
	}

	conn := setupTestServer(t, mod, false)
	defer conn.Close()

	client := &Client{Module: "testmod"}
	session, err := client.Connect(conn)
	if err != nil {
		t.Fatalf("Connect failed: %v", err)
	}

	_, err = session.Open("nonexistent.txt")
	if err == nil {
		t.Fatal("expected error for non-existent file, got nil")
	}
	if !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("expected ErrNotExist, got: %v", err)
	}
}

func TestClientOpen_EmptyFile(t *testing.T) {
	mod := &ServerModule{
		Name: "testmod",
		FS:   fstest.MapFS{"empty.txt": {Data: []byte{}}},
	}

	conn := setupTestServer(t, mod, true)
	defer conn.Close()

	client := &Client{Module: "testmod"}
	session, err := client.Connect(conn)
	if err != nil {
		t.Fatalf("Connect failed: %v", err)
	}

	f, err := session.Open("empty.txt")
	if err != nil {
		t.Fatalf("Open(empty.txt) failed: %v", err)
	}
	defer f.Close()

	data, err := io.ReadAll(f)
	if err != nil {
		t.Fatalf("ReadAll failed: %v", err)
	}
	if len(data) != 0 {
		t.Errorf("expected empty file, got %d bytes", len(data))
	}
}

func TestClientOpen_LargeFile(t *testing.T) {
	fileData := make([]byte, 2000)
	for i := range fileData {
		fileData[i] = byte(i % 256)
	}

	mod := &ServerModule{
		Name: "testmod",
		FS:   fstest.MapFS{"large.bin": {Data: fileData}},
	}

	conn := setupTestServer(t, mod, true)
	defer conn.Close()

	client := &Client{Module: "testmod"}
	session, err := client.Connect(conn)
	if err != nil {
		t.Fatalf("Connect failed: %v", err)
	}

	f, err := session.Open("large.bin")
	if err != nil {
		t.Fatalf("Open(large.bin) failed: %v", err)
	}
	defer f.Close()

	data, err := io.ReadAll(f)
	if err != nil {
		t.Fatalf("ReadAll failed: %v", err)
	}
	if !bytes.Equal(data, fileData) {
		t.Errorf("file content mismatch: got %d bytes, want %d", len(data), len(fileData))
	}
}

func TestClientOpen_RootModuleAccess(t *testing.T) {
	mod := &ServerModule{
		Name:    "mymod",
		Comment: "My Module",
		FS: fstest.MapFS{
			"file.txt": {Data: []byte("hello from mymod")},
		},
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

	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				_, _ = srv.HandleConnection(c, HandleOptions{
					LocalGreeting: protocol.Greeting{Version: 32, SubProtocol: 0, Digests: []string{"md5", "md4"}},
				})
				c.Close()
			}(conn)
		}
	}()

	client := &Client{
		ConnectFunc: func(moduleName string) (io.ReadWriter, error) {
			return net.Dial("tcp", ln.Addr().String())
		},
	}

	session, err := client.OpenRoot()
	if err != nil {
		t.Fatalf("OpenRoot failed: %v", err)
	}

	rootFile, err := session.Open(".")
	if err != nil {
		t.Fatalf("Open root failed: %v", err)
	}
	defer rootFile.Close()

	dirFile, ok := rootFile.(interface {
		ReadDir(n int) ([]fs.DirEntry, error)
	})
	if !ok {
		t.Fatal("root file does not support ReadDir")
	}

	entries, err := dirFile.ReadDir(0)
	if err != nil {
		t.Fatalf("ReadDir failed: %v", err)
	}

	found := false
	for _, e := range entries {
		if e.Name() == "mymod" {
			found = true
		}
	}
	if !found {
		t.Error("expected mymod in root directory entries")
	}
}

func TestClientOpen_RootModuleFileAccess(t *testing.T) {
	fileData := []byte("hello from mymod via root")
	mod := &ServerModule{
		Name:    "mymod",
		Comment: "My Module",
		FS: fstest.MapFS{
			"file.txt": {Data: fileData},
		},
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

	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				result, err := srv.HandleConnection(c, HandleOptions{
					LocalGreeting: protocol.Greeting{Version: 32, SubProtocol: 0, Digests: []string{"md5", "md4"}},
				})
				if err != nil {
					c.Close()
					return
				}

				mw := mux.NewWriter(c)
				mr := mux.NewReader(c)
				if err := sendFileList(mw, result.Module.FS, ".", result.Version, false); err != nil {
					c.Close()
					return
				}

				// read selector
				var ndxBuf [1]byte
				if _, err := c.Read(ndxBuf[:]); err != nil {
					c.Close()
					return
				}
				var iflagsBuf [2]byte
				if result.Version >= 29 {
					if _, err := io.ReadFull(c, iflagsBuf[:]); err != nil {
						c.Close()
						return
					}
				}

				f, err := result.Module.FS.Open("file.txt")
				if err != nil {
					c.Close()
					return
				}

				if err := sendFile(mr, mw, f, result.Version); err != nil {
					f.Close()
					c.Close()
					return
				}
				f.Close()
				c.Close()
			}(conn)
		}
	}()

	client := &Client{
		ConnectFunc: func(moduleName string) (io.ReadWriter, error) {
			return net.Dial("tcp", ln.Addr().String())
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
	if !bytes.Equal(data, fileData) {
		t.Errorf("file content = %q, want %q", data, fileData)
	}
}

func TestFlistReader_BasicEntry(t *testing.T) {
	var buf bytes.Buffer

	buf.WriteByte(xmitTopDir | xmitExtendedFlags)
	buf.WriteByte(0)

	buf.WriteByte(8)
	buf.WriteString("file.txt")

	if err := protocol.WriteVarlong(&buf, 12, 3); err != nil {
		t.Fatalf("WriteVarlong: %v", err)
	}
	if err := protocol.WriteVarlong(&buf, 1000000, 4); err != nil {
		t.Fatalf("WriteVarlong: %v", err)
	}
	writeInt32(&buf, int32(0o100644))
	if err := protocol.WriteVarint(&buf, 0); err != nil {
		t.Fatalf("WriteVarint: %v", err)
	}
	if err := protocol.WriteVarint(&buf, 0); err != nil {
		t.Fatalf("WriteVarint: %v", err)
	}
	buf.WriteByte(0)

	flr := newFlistReader(buf.Bytes(), 30)
	entry, err := flr.readEntry(0)
	if err != nil {
		t.Fatalf("readEntry failed: %v", err)
	}

	if entry.name != "file.txt" {
		t.Errorf("name = %q, want %q", entry.name, "file.txt")
	}
	if entry.size != 12 {
		t.Errorf("size = %d, want 12", entry.size)
	}

	_, err = flr.readEntry(1)
	if err != io.EOF {
		t.Errorf("expected EOF at end-of-list, got: %v", err)
	}
}

func TestFindEntry(t *testing.T) {
	entries := []fileListEntry{
		{name: ".", mode: fs.ModeDir, index: 0},
		{name: "dir1", mode: fs.ModeDir, index: 1},
		{name: "dir1/file1.txt", mode: 0o644, index: 2},
		{name: "file2.txt", mode: 0o644, index: 3},
	}

	tests := []struct {
		name  string
		wants int
	}{
		{".", 0},
		{"dir1", 1},
		{"dir1/file1.txt", 2},
		{"file2.txt", 3},
		{"nonexistent", -1},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := findEntry(entries, tt.name)
			if tt.wants < 0 {
				if got != nil {
					t.Errorf("expected nil for %q, got index %d", tt.name, got.index)
				}
			} else {
				if got == nil {
					t.Errorf("expected index %d for %q, got nil", tt.wants, tt.name)
				} else if got.index != tt.wants {
					t.Errorf("expected index %d for %q, got %d", tt.wants, tt.name, got.index)
				}
			}
		})
	}
}

func TestFilterChildren_Root(t *testing.T) {
	entries := []fileListEntry{
		{name: ".", mode: fs.ModeDir, index: 0},
		{name: "dir1", mode: fs.ModeDir, index: 1},
		{name: "dir1/file1.txt", mode: 0o644, index: 2},
		{name: "file.txt", mode: 0o644, index: 3},
	}

	children := filterChildren(entries, ".")
	if len(children) != 2 {
		t.Errorf("expected 2 children of root, got %d", len(children))
	}
}

func TestFilterChildren_SubDir(t *testing.T) {
	entries := []fileListEntry{
		{name: ".", mode: fs.ModeDir, index: 0},
		{name: "dir1", mode: fs.ModeDir, index: 1},
		{name: "dir1/file1.txt", mode: 0o644, index: 2},
		{name: "dir1/file2.txt", mode: 0o644, index: 3},
		{name: "dir2", mode: fs.ModeDir, index: 4},
	}

	children := filterChildren(entries, "dir1")
	if len(children) != 2 {
		t.Errorf("expected 2 children of dir1, got %d", len(children))
	}
}


// TestWriteNdx_Compressed verifies compressed NDX encoding matches upstream io.c write_ndx().
// Each test case is independent (writeNdx takes prevNdx as a pointer, not static state).
func TestWriteNdx_Compressed(t *testing.T) {
	tests := []struct {
		name      string
		ndx       int
		version   int
		prevNdx   int32
		wantPrev  int32
		wantBytes []byte
	}{
		// Proto < 30: plain int32 LE (no compression)
		{"proto28_zero", 0, 28, 42, 42, []byte{0, 0, 0, 0}},
		{"proto28_neg1", -1, 28, 42, 42, []byte{0xff, 0xff, 0xff, 0xff}},

		// Proto >= 30: compressed NDX
		// NDX_DONE: single byte 0x00, no state change
		{"ndx_done", -1, 30, 42, 42, []byte{0x00}},

		// Single-byte diff (1-253)
		{"first_positive", 0, 30, -1, 0, []byte{0x01}},       // diff = 0-(-1) = 1
		{"diff_one", 1, 30, 0, 1, []byte{0x01}},              // diff = 1-0 = 1
		{"diff_253", 254, 30, 1, 254, []byte{0xfd}},          // diff = 254-1 = 253

		// 2-byte diff (0 or 254-32767)
		{"diff_zero", 100, 30, 100, 100, []byte{0xfe, 0x00, 0x00}},
		{"diff_254", 256, 30, 2, 256, []byte{0xfe, 0x00, 0xfe}},

		// 4-byte form (diff < 0 or diff > 32767)
		{"diff_negative_4byte", 5, 30, 100, 5, []byte{0xfe, 0x80, 0x05, 0x00, 0x00}},

		// Negative indices: 0xFF prefix, then same diff encoding on abs value
		// prev_negative starts at 1 (absolute value of last negative seen)
		{"negative_first", -2, 30, 1, 2, []byte{0xff, 0x01}},  // abs=2, diff=2-1=1
		{"negative_second", -3, 30, 2, 3, []byte{0xff, 0x01}},  // abs=3, diff=3-2=1
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer
			prevNdx := tt.prevNdx
			err := writeNdx(&buf, tt.ndx, tt.version, &prevNdx)
			if err != nil {
				t.Fatalf("writeNdx(%d) failed: %v", tt.ndx, err)
			}
			got := buf.Bytes()
			if !bytes.Equal(got, tt.wantBytes) {
				t.Errorf("writeNdx(%d, v=%d, prev=%d) = %v, want %v",
					tt.ndx, tt.version, tt.prevNdx, got, tt.wantBytes)
			}
			if prevNdx != tt.wantPrev {
				t.Errorf("writeNdx(%d) updated prevNdx to %d, want %d",
					tt.ndx, prevNdx, tt.wantPrev)
			}
		})
	}
}

// TestWriteSelector_ItemFlags verifies item flags match upstream rsync.h defines
// and produce correct wire format (shortint LE).
func TestWriteSelector_ItemFlags(t *testing.T) {
	// Verify constants match upstream rsync.h
	if itemTransfer != 1<<15 {
		t.Errorf("itemTransfer = 0x%04x, want 0x%04x (ITEM_TRANSFER = 1<<15)", itemTransfer, 1<<15)
	}
	if itemMissingData != 1<<16 {
		t.Errorf("itemMissingData = 0x%04x, want 0x%04x (ITEM_MISSING_DATA = 1<<16)", itemMissingData, 1<<16)
	}

	// Wire format for proto >= 29: compressed NDX + shortint(iflags) LE
	// ITEM_TRANSFER = 1<<15 = 0x8000 fits in uint16
	// ITEM_MISSING_DATA = 1<<16 is outside uint16 range (upstream comment: "used by log_formatted()")
	// so only ITEM_TRANSFER is sent on the wire as shortint
	// ndx=0 (diff from -1 = 1) + iflags=0x8000 (ITEM_TRANSFER only, LE = [0x00, 0x80])
	// Expected: [0x01, 0x00, 0x80]
	var buf bytes.Buffer
	s := &Session{rw: &buf, version: 30, prevNdx: -1}
	if err := s.writeSelector(0, itemTransfer); err != nil {
		t.Fatalf("writeSelector failed: %v", err)
	}
	got := buf.Bytes()
	want := []byte{0x01, 0x00, 0x80}
	if !bytes.Equal(got, want) {
		t.Errorf("writeSelector(0, ITEM_TRANSFER) = %v, want %v", got, want)
	}

	// Proto < 29: no iflags sent, NDX is plain int32 LE
	buf.Reset()
	s.version = 28
	s.prevNdx = -1
	if err := s.writeSelector(0, 0); err != nil {
		t.Fatalf("writeSelector(proto=28) failed: %v", err)
	}
	got = buf.Bytes()
	want = []byte{0, 0, 0, 0} // plain int32 LE for ndx=0
	if !bytes.Equal(got, want) {
		t.Errorf("writeSelector(proto=28) = %v, want %v", got, want)
	}
}
