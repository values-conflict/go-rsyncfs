package rsyncfs

import (
	"bytes"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// wireTrace logs every byte read/written with callstacks when the
// RSYNC_TEST_WIRE environment variable is set.  It exists to debug
// wire-format disagreements against a real rsync client; the default
// test run stays quiet.
type wireTrace struct {
	rw io.ReadWriter
}

func (w *wireTrace) Read(p []byte) (n int, err error) {
	n, err = w.rw.Read(p)
	if n > 0 && os.Getenv("RSYNC_TEST_WIRE") != "" {
		buf := bytes.NewBufferString(fmt.Sprintf("SERVER-READ  %d bytes: % x\n", n, p[:n]))
		wireStack(buf)
		os.Stderr.Write(buf.Bytes())
	}
	return
}

func (w *wireTrace) Write(p []byte) (int, error) {
	if os.Getenv("RSYNC_TEST_WIRE") != "" {
		buf := bytes.NewBufferString(fmt.Sprintf("SERVER-WRITE %d bytes: % x\n", len(p), p))
		wireStack(buf)
		os.Stderr.Write(buf.Bytes())
	}
	return w.rw.Write(p)
}

func wireStack(w io.Writer) {
	pc := make([]uintptr, 12)
	nn := runtime.Callers(3, pc)
	if nn == 0 {
		return
	}
	frames := runtime.CallersFrames(pc[:nn])
	for i := 0; i < 5; i++ {
		f, more := frames.Next()
		fmt.Fprintf(w, "  %s:%d %s\n", f.File, f.Line, f.Function)
		if !more {
			break
		}
	}
}

// rsyncBinPaths returns every rsync binary available for testing: the
// built .upstream/rsync (when present) followed by the pinned static
// builds in .upstream/old_versions/, oldest protocol first.  The test
// skips when none are found.
func rsyncBinPaths(t *testing.T) []string {
	t.Helper()
	repoRoot := repoRootDir(t)
	var bins []string
	build := filepath.Join(repoRoot, ".upstream", "rsync")
	if info, err := os.Stat(build); err == nil && !info.IsDir() {
		bins = append(bins, build)
	}
	oldVersions := filepath.Join(repoRoot, ".upstream", "old_versions")
	if dirs, err := os.ReadDir(oldVersions); err == nil {
		for _, d := range dirs {
			if !d.IsDir() && strings.HasPrefix(d.Name(), "rsync_") {
				bins = append(bins, filepath.Join(oldVersions, d.Name()))
			}
		}
	}
	if len(bins) == 0 {
		t.Skip("no rsync binary found in .upstream/ or .upstream/old_versions/")
	}
	return bins
}

// repoRootDir finds the repository root by looking for the .upstream
// directory.
func repoRootDir(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal("getwd: ", err)
	}
	for i := 0; i < 10; i++ {
		if _, err := os.Stat(filepath.Join(dir, ".upstream")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	t.Fatal("could not find repository root (no .upstream directory)")
	return ""
}

// TestIntegration_RsyncClientPull pulls a real directory tree from the
// server with real rsync client binaries -- the current build plus every
// pinned old version -- and checks that each transfer completes and the
// pulled bytes match.  The module's FS is a real on-disk directory: the
// root's permissions come from the directory itself, which keeps the
// client from chmod'ing the destination read-only (a fstest.MapFS root
// would be reported as 0555 and the client honors that).
func TestIntegration_RsyncClientPull(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in -short mode")
	}

	content := []byte("hello from go-rsyncfs\n")

	for _, rsyncBin := range rsyncBinPaths(t) {
		t.Run(filepath.Base(rsyncBin), func(t *testing.T) {
			srcDir := t.TempDir()
			if err := os.WriteFile(filepath.Join(srcDir, "hello.txt"), content, 0o644); err != nil {
				t.Fatalf("write source file: %v", err)
			}
			mod := &ServerModule{
				Name:    "testmod",
				Comment: "integration test module",
				FS:      os.DirFS(srcDir),
			}
			s, err := NewServer(mod)
			if err != nil {
				t.Fatalf("NewServer: %v", err)
			}

			l, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatalf("listen: %v", err)
			}
			defer l.Close()

			host, port, _ := net.SplitHostPort(l.Addr().String())

			handled := make(chan error, 1)
			go func() {
				conn, err := l.Accept()
				if err != nil {
					handled <- err
					return
				}
				defer conn.Close()
				handled <- s.HandleConnection(&wireTrace{rw: conn})
			}()

			// pull into an existing directory (trailing slash: the
			// module's contents land inside it, and the generator never
			// requests the root directory itself as a transfer)
			destDir := t.TempDir() + "/"

			cmd := exec.Command(rsyncBin, "-a",
				"--port="+port,
				host+"::testmod", destDir)
			cmd.Stderr = os.Stderr
			rsErr := cmd.Run()

			select {
			case hErr := <-handled:
				if hErr != nil {
					t.Errorf("server error: %v", hErr)
				}
			case <-time.After(10 * time.Second):
				t.Fatal("server did not finish handling connection")
			}

			if rsErr != nil {
				t.Fatalf("rsync exited with: %v", rsErr)
			}

			data, err := os.ReadFile(filepath.Join(destDir, "hello.txt"))
			if err != nil {
				t.Fatalf("read pulled file: %v", err)
			}
			if string(data) != string(content) {
				t.Errorf("file content mismatch: got %q, want %q", data, content)
			}
		})
	}
}
