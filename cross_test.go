package rsyncfs

import (
	"bytes"
	"embed"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
	"testing/fstest"
	"time"
)

//go:embed testdata
var crossTestdata embed.FS

// crossModuleName is the module name served by the servers in these tests.
const crossModuleName = "testmod"

// crossFixtureTime is the fixed ModTime given to fixture entries, so that
// every connection -- each one a fresh server-side walk -- reports
// identical metadata.
var crossFixtureTime = time.Date(2024, 3, 1, 12, 0, 0, 0, time.UTC)

// embeddedFixtureFiles returns the testdata/ fixture tree as a flat map of
// fixture-relative path to file content.
func embeddedFixtureFiles(t *testing.T) map[string][]byte {
	t.Helper()
	sub, err := fs.Sub(crossTestdata, "testdata")
	if err != nil {
		t.Fatalf("fs.Sub(testdata): %v", err)
	}
	files := make(map[string][]byte)
	err = fs.WalkDir(sub, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		data, err := fs.ReadFile(sub, path)
		if err != nil {
			return err
		}
		files[path] = data
		return nil
	})
	if err != nil {
		t.Fatalf("walk fixture tree: %v", err)
	}
	return files
}

// fixtureServerFS builds an fstest.MapFS from the embedded testdata fixtures
// with every entry's ModTime pinned to crossFixtureTime.
func fixtureServerFS(t *testing.T) fstest.MapFS {
	t.Helper()
	m := make(fstest.MapFS)
	for name, data := range embeddedFixtureFiles(t) {
		m[name] = &fstest.MapFile{Data: data, Mode: 0o644, ModTime: crossFixtureTime}
	}
	return m
}

// crossServer builds a Server that serves the crossModuleName module from
// fsys.
func crossServer(t *testing.T, fsys fs.FS) *Server {
	t.Helper()
	s, err := NewServer(&ServerModule{Name: crossModuleName, FS: fsys})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	return s
}

// crossClient returns a module-mode Client whose ConnectFunc wires every
// connection to a fresh [BufPipe] against s, with a server-side goroutine
// per connection. It also returns the channel those goroutines report on
// and a pointer counting how many connections were opened.
//
// Each FS operation consumes its connection, so multi-operation tests must
// use this (or something equivalent) rather than one shared pipe.
func crossClient(t *testing.T, s *Server) (*Client, <-chan error, *int) {
	t.Helper()
	doneChs := make(chan error, 256)
	connects := 0
	c := &Client{
		Module: crossModuleName,
		ConnectFunc: func(moduleName string) (io.ReadWriter, error) {
			connects++
			serverEnd, clientEnd := BufPipe()
			go func() {
				defer serverEnd.Close()
				doneChs <- s.HandleConnection(serverEnd)
			}()
			return clientEnd, nil
		},
	}
	return c, doneChs, &connects
}

// drainConns waits for n server-side goroutines to exit, logging each
// result. A clean transfer can leave the server seeing a peer close, which
// is not an error condition here.
func drainConns(t *testing.T, done <-chan error, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		select {
		case serr := <-done:
			t.Logf("server connection %d returned: %v", i+1, serr)
		case <-time.After(10 * time.Second):
			t.Fatalf("server connection %d/%d did not exit", i+1, n)
		}
	}
}

// TestCross_FullDirectoryTree pulls the entire embedded fixture tree --
// root files plus nested subdirectories -- over the wire and verifies every
// byte, and that the file-list-reported sizes match.
func TestCross_FullDirectoryTree(t *testing.T) {
	s := crossServer(t, fixtureServerFS(t))
	c, done, connects := crossClient(t, s)
	sess, err := c.Connect(nil)
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}

	want := embeddedFixtureFiles(t)
	for name, data := range want {
		t.Run(name, func(t *testing.T) {
			f, err := sess.Open(name)
			if err != nil {
				t.Fatalf("Open(%q): %v", name, err)
			}
			info, err := f.Stat()
			if err != nil {
				t.Fatalf("Stat(%q): %v", name, err)
			}
			if info.Size() != int64(len(data)) {
				t.Fatalf("Stat(%q).Size() = %d, want %d", name, info.Size(), len(data))
			}
			got, err := io.ReadAll(f)
			if err != nil {
				t.Fatalf("ReadAll(%q): %v", name, err)
			}
			if err := f.Close(); err != nil {
				t.Fatalf("Close(%q): %v", name, err)
			}
			if !bytes.Equal(got, data) {
				t.Fatalf("%q: content mismatch: got %d bytes, want %d", name, len(got), len(data))
			}
		})
	}
	drainConns(t, done, *connects)
}

// TestCross_Symlinks checks that symlinks survive the trip: the client sees
// them as symlinks with the correct targets, and the files they point at are
// still readable as usual.
func TestCross_Symlinks(t *testing.T) {
	m := fixtureServerFS(t)
	m["link-root.txt"] = &fstest.MapFile{Data: []byte("hello.txt"), Mode: fs.ModeSymlink, ModTime: crossFixtureTime}
	m["sub/link-sub.txt"] = &fstest.MapFile{Data: []byte("notes.txt"), Mode: fs.ModeSymlink, ModTime: crossFixtureTime}
	s := crossServer(t, m)
	c, done, connects := crossClient(t, s)
	sess, err := c.Connect(nil)
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}

	checkLink := func(name, wantTarget string) {
		t.Helper()
		target, err := fs.ReadLink(sess, name)
		if err != nil {
			t.Fatalf("ReadLink(%q): %v", name, err)
		}
		if target != wantTarget {
			t.Fatalf("ReadLink(%q) = %q, want %q", name, target, wantTarget)
		}
		info, err := sess.Lstat(name)
		if err != nil {
			t.Fatalf("Lstat(%q): %v", name, err)
		}
		if info.Mode().Type() != fs.ModeSymlink {
			t.Fatalf("Lstat(%q).Mode().Type() = %v, want %v", name, info.Mode().Type(), fs.ModeSymlink)
		}
		// A symlink's size is the length of its target string.
		if info.Size() != int64(len(wantTarget)) {
			t.Fatalf("Lstat(%q).Size() = %d, want %d", name, info.Size(), len(wantTarget))
		}
	}
	t.Run("root symlink", func(t *testing.T) {
		checkLink("link-root.txt", "hello.txt")
	})
	t.Run("subdirectory symlink", func(t *testing.T) {
		checkLink("sub/link-sub.txt", "notes.txt")
	})
	t.Run("link target readable", func(t *testing.T) {
		f, err := sess.Open("hello.txt")
		if err != nil {
			t.Fatalf("Open(hello.txt): %v", err)
		}
		got, err := io.ReadAll(f)
		if err != nil {
			t.Fatalf("ReadAll(hello.txt): %v", err)
		}
		want := embeddedFixtureFiles(t)["hello.txt"]
		if !bytes.Equal(got, want) {
			t.Fatalf("hello.txt: content mismatch: got %q, want %q", got, want)
		}
	})
	drainConns(t, done, *connects)
}

// TestCross_LargeFile checks files larger than the 32 KiB delta block size,
// which forces multiple literal (and, with real client data, matched)
// blocks in the transfer.
func TestCross_LargeFile(t *testing.T) {
	const blockSize = 32 * 1024
	want := embeddedFixtureFiles(t)
	if len(want["large.bin"]) <= blockSize {
		t.Fatalf("fixture guard: large.bin is %d bytes, must exceed one %d-byte block", len(want["large.bin"]), blockSize)
	}
	s := crossServer(t, fixtureServerFS(t))
	c, done, connects := crossClient(t, s)
	sess, err := c.Connect(nil)
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}

	for _, name := range []string{"binary.bin", "large.bin"} {
		t.Run(name, func(t *testing.T) {
			f, err := sess.Open(name)
			if err != nil {
				t.Fatalf("Open(%q): %v", name, err)
			}
			got, err := io.ReadAll(f)
			if err != nil {
				t.Fatalf("ReadAll(%q): %v", name, err)
			}
			if err := f.Close(); err != nil {
				t.Fatalf("Close(%q): %v", name, err)
			}
			if !bytes.Equal(got, want[name]) {
				t.Fatalf("%q: content mismatch: got %d bytes, want %d", name, len(got), len(want[name]))
			}
		})
	}
	drainConns(t, done, *connects)
}

// TestCross_EmptyDirAndFile checks that empty directories and empty files
// round-trip: the directory is listed and opens with zero entries, and the
// empty file reads back as zero bytes.
func TestCross_EmptyDirAndFile(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "empty"), 0o755); err != nil {
		t.Fatalf("Mkdir(empty): %v", err)
	}
	if err := os.MkdirAll(filepath.Join(root, "nested", "empty"), 0o755); err != nil {
		t.Fatalf("MkdirAll(nested/empty): %v", err)
	}
	// fstest.MapFS cannot express empty directories, so the whole tree is
	// on-disk; the empty file comes from the embedded fixtures.
	if err := os.WriteFile(filepath.Join(root, "empty.txt"), embeddedFixtureFiles(t)["empty.txt"], 0o644); err != nil {
		t.Fatalf("WriteFile(empty.txt): %v", err)
	}
	s := crossServer(t, os.DirFS(root))
	c, done, connects := crossClient(t, s)
	sess, err := c.Connect(nil)
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}

	checkEmptyDir := func(name string) {
		t.Helper()
		d, err := sess.Open(name)
		if err != nil {
			t.Fatalf("Open(%q): %v", name, err)
		}
		entries, err := d.(fs.ReadDirFile).ReadDir(-1)
		if err != nil {
			t.Fatalf("ReadDir(%q): %v", name, err)
		}
		if len(entries) != 0 {
			t.Fatalf("ReadDir(%q) = %d entries, want 0", name, len(entries))
		}
		if info, err := d.Stat(); err != nil {
			t.Fatalf("Stat(%q): %v", name, err)
		} else if !info.IsDir() {
			t.Fatalf("Stat(%q): IsDir() = false, want true", name)
		}
		if err := d.Close(); err != nil {
			t.Fatalf("Close(%q): %v", name, err)
		}
	}
	t.Run("empty directory", func(t *testing.T) {
		checkEmptyDir("empty")
	})
	t.Run("nested empty directory", func(t *testing.T) {
		checkEmptyDir("nested/empty")
	})
	t.Run("empty file", func(t *testing.T) {
		f, err := sess.Open("empty.txt")
		if err != nil {
			t.Fatalf("Open(empty.txt): %v", err)
		}
		got, err := io.ReadAll(f)
		if err != nil {
			t.Fatalf("ReadAll(empty.txt): %v", err)
		}
		if len(got) != 0 {
			t.Fatalf("empty.txt: got %d bytes, want 0", len(got))
		}
	})
	drainConns(t, done, *connects)
}

// TestCross_FstestTestFS runs the standard library's fstest.TestFS against a
// Client session in front of a Server, exercising Open, ReadDir (including
// fragmented reads), ReadAll, Lstat, and ReadLink consistency across many
// independent connections.
//
// The backing tree is a real temp directory rather than an fstest.MapFS:
// synthetic MapFS directories have no ModTime, and the server's zero-time
// normalization would stamp each connection's walk with a different
// time.Now(), which TestFS's cross-operation metadata comparisons reject.
func TestCross_FstestTestFS(t *testing.T) {
	root := t.TempDir()
	write := func(name string, data []byte) {
		t.Helper()
		p := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatalf("MkdirAll(%s): %v", name, err)
		}
		if err := os.WriteFile(p, data, 0o644); err != nil {
			t.Fatalf("WriteFile(%s): %v", name, err)
		}
		if err := os.Chtimes(p, crossFixtureTime, crossFixtureTime); err != nil {
			t.Fatalf("Chtimes(%s): %v", name, err)
		}
	}
	for name, data := range embeddedFixtureFiles(t) {
		write(name, data)
	}
	// An empty directory, which fstest.MapFS cannot express at all.
	if err := os.Mkdir(filepath.Join(root, "empty"), 0o755); err != nil {
		t.Fatalf("Mkdir(empty): %v", err)
	}
	if err := os.Chtimes(filepath.Join(root, "empty"), crossFixtureTime, crossFixtureTime); err != nil {
		t.Fatalf("Chtimes(empty): %v", err)
	}
	// A symlink; go:embed cannot carry one into the fixtures.
	if err := os.Symlink("hello.txt", filepath.Join(root, "link.txt")); err != nil {
		t.Fatalf("Symlink: %v", err)
	}

	s := crossServer(t, os.DirFS(root))
	c, done, connects := crossClient(t, s)
	sess, err := c.Connect(nil)
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}

	// Only top-level names: TestFS's fs.Sub sub-test needs an Open method on
	// directory files, which the module file API does not provide.
	if err := fstest.TestFS(sess, "hello.txt", "large.bin", "sub", "empty", "link.txt"); err != nil {
		t.Fatalf("fstest.TestFS: %v", err)
	}
	drainConns(t, done, *connects)
}
