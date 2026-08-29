package rsyncfs

import (
	"bytes"
	"fmt"
	"io"
	"io/fs"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/values-conflict/go-rsyncfs/protocol"
)

// daemonProtoVersion is the protocol version each pinned old_versions
// binary advertises, from .upstream/old_versions/README.md.  The key is the
// binary's base name.  The pinned builds have a fixed, documented protocol;
// the bare "rsync" (the .upstream build or an rsync found on PATH) is not
// pinned, so its advertised protocol is read from its own `--version` output
// (see daemonProtoVersionFor) instead of being listed here.
var daemonProtoVersion = map[string]int{
	"rsync_2.6.0": 27,
	"rsync_3.0.0": 30,
	"rsync_3.1.0": 31,
	"rsync_3.1.3": 31,
	"rsync_3.2.0": 31,
	"rsync_3.2.7": 31,
	"rsync_3.3.0": 31,
	"rsync_3.4.0": 32,
	"rsync_3.4.1": 32,
}

// daemonProtoVersionFor returns the protocol version the daemon at binPath
// advertises in its greeting -- the value the client negotiates down to when
// its own maximum is higher.  Pinned old_versions builds carry a documented
// protocol; any other binary (the bare "rsync") reports its own via
// `rsync --version`, whose first line reads "... protocol version N".  The
// second return value is false when the version cannot be determined.
func daemonProtoVersionFor(t *testing.T, binPath string) (int, bool) {
	t.Helper()
	base := filepath.Base(binPath)
	if v, known := daemonProtoVersion[base]; known {
		return v, true
	}
	out, err := exec.Command(binPath, "--version").Output()
	if err != nil {
		return 0, false
	}
	line := strings.SplitN(string(out), "\n", 2)[0]
	const marker = "protocol version "
	i := strings.Index(line, marker)
	if i < 0 {
		return 0, false
	}
	rest := line[i+len(marker):]
	j := 0
	for j < len(rest) && rest[j] >= '0' && rest[j] <= '9' {
		j++
	}
	if j == 0 {
		return 0, false
	}
	v, err := strconv.Atoi(rest[:j])
	if err != nil {
		return 0, false
	}
	return v, true
}

// startTestDaemon launches a real rsync daemon serving srcDir as
// moduleName on an ephemeral localhost port and returns its address and
// a stop function.  The daemon config pins the fixture module
// (read-only, no chroot) and points the motd at a nonexistent file so
// no extra greeting line is injected.
func startTestDaemon(t *testing.T, rsyncBin, srcDir, moduleName string) (string, func()) {
	t.Helper()

	// reserve an ephemeral port, then hand it to the daemon
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve port: %v", err)
	}
	_, port, _ := net.SplitHostPort(l.Addr().String())
	addr := "127.0.0.1:" + port
	l.Close()

	dir := t.TempDir()
	cfg := filepath.Join(dir, "rsyncd.conf")
	cfgBody := fmt.Sprintf(`motd file = %s
[%s]
path = %s
use chroot = no
read only = yes
`, filepath.Join(dir, "nonexistent-motd"), moduleName, srcDir)
	if err := os.WriteFile(cfg, []byte(cfgBody), 0o600); err != nil {
		t.Fatalf("write daemon config: %v", err)
	}

	stderr := new(strings.Builder)
	cmd := exec.Command(rsyncBin, "--daemon", "--no-detach", "--config="+cfg, "--port="+port)
	cmd.Stderr = stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start daemon: %v", err)
	}

	stop := func() {
		cmd.Process.Kill()
		cmd.Wait()
	}

	// wait until the daemon accepts connections
	deadline := time.Now().Add(10 * time.Second)
	for {
		conn, err := net.DialTimeout("tcp", addr, 200*time.Millisecond)
		if err == nil {
			conn.Close()
			break
		}
		if time.Now().After(deadline) {
			stop()
			t.Fatalf("daemon did not start listening on %s: %v\nstderr:\n%s", addr, err, stderr)
		}
		time.Sleep(50 * time.Millisecond)
	}

	return addr, stop
}

// TestIntegration_ClientConnect_RealDaemon runs [Client.Connect] against
// every available real rsync daemon -- the current build plus each pinned
// old version -- and checks the negotiated session: the version must be
// the daemon's advertised protocol (the client's 32 negotiates down), the
// seed must be non-zero, and the file list pulled through the session's
// multiplexed input must contain the fixture file.  The full transfer
// phase (selectors, data, stats, goodbye) is exercised in the Open tests.
func TestIntegration_ClientConnect_RealDaemon(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in -short mode")
	}

	content := []byte("hello from our client\n")

	for _, rsyncBin := range rsyncBinPaths(t) {
		base := filepath.Base(rsyncBin)
		t.Run(base, func(t *testing.T) {
			wantVer, known := daemonProtoVersionFor(t, rsyncBin)
			if !known {
				t.Skipf("could not determine protocol version for %s", base)
			}

			srcDir := t.TempDir()
			if err := os.WriteFile(filepath.Join(srcDir, "hello.txt"), content, 0o644); err != nil {
				t.Fatalf("write fixture: %v", err)
			}

			addr, stop := startTestDaemon(t, rsyncBin, srcDir, "testmod")
			defer stop()

			conn, err := net.Dial("tcp", addr)
			if err != nil {
				t.Fatalf("dial daemon: %v", err)
			}
			defer conn.Close()

			c := Client{Module: "testmod"}
			sess, err := c.Connect(conn)
			if err != nil {
				t.Fatalf("Connect: %v", err)
			}
			if sess.version != wantVer {
				t.Errorf("version = %d, want %d (daemon's advertised protocol)", sess.version, wantVer)
			}
			if sess.seed == 0 {
				t.Error("seed = 0, want non-zero")
			}
			if (sess.version >= 30) != (sess.mw != nil) {
				t.Errorf("mux output = %v, want %v (proto %d)", sess.mw != nil, sess.version >= 30, sess.version)
			}

			// Pull the file list.  Proto >= 30 flushes the buffered
			// filter list first; below that the filter list went out raw
			// during Connect.  Either way the flist arrives as one or
			// more mux data chunks.
			var flist []byte
			for i := 0; i < 8; i++ {
				if sess.mw != nil {
					if err := sess.mw.Flush(); err != nil {
						t.Fatalf("flush: %v", err)
					}
				}
				chunk, err := sess.mr.ReadDataChunk()
				if err != nil {
					t.Fatalf("read file list chunk: %v", err)
				}
				flist = append(flist, chunk...)
				if strings.Contains(string(flist), "hello.txt") {
					break
				}
			}
			if !strings.Contains(string(flist), "hello.txt") {
				t.Errorf("file list (first 200 bytes: %q) does not contain hello.txt", flist[:min(200, len(flist))])
			}
		})
	}
}

// TestIntegration_ClientVersionDowngrade checks the handshake against a
// real daemon when the client deliberately advertises a lower protocol
// than its maximum: the negotiated version must be the client's, the
// client_info -e option must be absent below proto 30, and the file list
// must still arrive.
func TestIntegration_ClientVersionDowngrade(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in -short mode")
	}

	bins := rsyncBinPaths(t)
	if len(bins) == 0 {
		t.Skip("no rsync binary found")
	}
	// The probe negotiates down to proto 31, so it needs a daemon that
	// advertises at least 31.
	var rsyncBin string
	for _, b := range bins {
		if v, known := daemonProtoVersionFor(t, b); known && v >= 31 {
			rsyncBin = b
			break
		}
	}
	if rsyncBin == "" {
		t.Skip("no rsync binary with a known protocol >= 31")
	}

	srcDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(srcDir, "hello.txt"), []byte("downgrade\n"), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	addr, stop := startTestDaemon(t, rsyncBin, srcDir, "testmod")
	defer stop()

	for _, wantVer := range []int{27, 29, 30, 31} {
		t.Run(strconv.Itoa(wantVer), func(t *testing.T) {
			conn, err := net.Dial("tcp", addr)
			if err != nil {
				t.Fatalf("dial daemon: %v", err)
			}
			defer conn.Close()

			c := Client{
				Module:   "testmod",
				Greeting: protocol.Greeting{Version: wantVer, Digests: protocol.SupportedDigests()},
			}
			sess, err := c.Connect(conn)
			if err != nil {
				t.Fatalf("Connect: %v", err)
			}
			if sess.version != wantVer {
				t.Fatalf("version = %d, want %d", sess.version, wantVer)
			}
			if wantVer < 30 && sess.mw != nil {
				t.Errorf("mux output at proto %d, want raw", wantVer)
			}

			var flist []byte
			for i := 0; i < 8; i++ {
				if sess.mw != nil {
					if err := sess.mw.Flush(); err != nil {
						t.Fatalf("flush: %v", err)
					}
				}
				chunk, err := sess.mr.ReadDataChunk()
				if err != nil {
					t.Fatalf("read file list chunk: %v", err)
				}
				flist = append(flist, chunk...)
				if strings.Contains(string(flist), "hello.txt") {
					break
				}
			}
			if !strings.Contains(string(flist), "hello.txt") {
				t.Errorf("file list does not contain hello.txt")
			}
		})
	}
}

// writeClientFixture writes a small directory tree to dir and returns the
// expected file contents: a top-level file, a subdirectory holding a file,
// a file large enough to span several 32KB delta chunks, and a symlink.
func writeClientFixture(t *testing.T, dir string) map[string][]byte {
	t.Helper()
	files := map[string][]byte{}
	files["hello.txt"] = []byte("hello from the client\n")
	if err := os.WriteFile(filepath.Join(dir, "hello.txt"), files["hello.txt"], 0o644); err != nil {
		t.Fatalf("write hello.txt: %v", err)
	}
	if err := os.Mkdir(filepath.Join(dir, "sub"), 0o755); err != nil {
		t.Fatalf("mkdir sub: %v", err)
	}
	files["sub/inner.txt"] = []byte("inner file\n")
	if err := os.WriteFile(filepath.Join(dir, "sub", "inner.txt"), files["sub/inner.txt"], 0o644); err != nil {
		t.Fatalf("write sub/inner.txt: %v", err)
	}
	big := make([]byte, 100_000)
	for i := range big {
		big[i] = byte(i % 251)
	}
	files["big.bin"] = big
	if err := os.WriteFile(filepath.Join(dir, "big.bin"), big, 0o644); err != nil {
		t.Fatalf("write big.bin: %v", err)
	}
	if err := os.Symlink("hello.txt", filepath.Join(dir, "link.txt")); err != nil {
		t.Fatalf("symlink link.txt: %v", err)
	}
	return files
}

// readDirAll is the ReadDir(n int) ([]fs.DirEntry, error) method set
// implemented by directory files.
type readDirFS interface {
	ReadDir(int) ([]fs.DirEntry, error)
}

// TestIntegration_ClientOpen_RealDaemon runs [Session.Open] -- file pulls,
// directory listings, and symlink resolution -- against every available
// real rsync daemon.  Each Open is a self-contained transfer on its own
// TCP connection (ConnectFunc re-dials the daemon per operation), so this
// exercises the full client-side transfer phase (flist, selector, delta,
// stats, goodbye) against a live daemon.
func TestIntegration_ClientOpen_RealDaemon(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in -short mode")
	}

	for _, rsyncBin := range rsyncBinPaths(t) {
		base := filepath.Base(rsyncBin)
		t.Run(base, func(t *testing.T) {
			if _, known := daemonProtoVersionFor(t, rsyncBin); !known {
				t.Skipf("could not determine protocol version for %s", base)
			}

			srcDir := t.TempDir()
			files := writeClientFixture(t, srcDir)
			addr, stop := startTestDaemon(t, rsyncBin, srcDir, "testmod")
			defer stop()

			c := Client{
				Module: "testmod",
				ConnectFunc: func(moduleName string) (io.ReadWriter, error) {
					return net.Dial("tcp", addr)
				},
			}
			sess, err := c.Connect(nil)
			if err != nil {
				t.Fatalf("Connect: %v", err)
			}

			// pull a top-level file
			if data := openAndRead(t, sess, "hello.txt"); !bytes.Equal(data, files["hello.txt"]) {
				t.Errorf("hello.txt content mismatch: got %d bytes, want %d", len(data), len(files["hello.txt"]))
			}

			// list the module root
			df, err := sess.Open(".")
			if err != nil {
				t.Fatalf("Open(.): %v", err)
			}
			rootRD, ok := df.(readDirFS)
			if !ok {
				df.Close()
				t.Fatal("opened root does not implement ReadDir")
			}
			rootEntries, err := rootRD.ReadDir(0)
			df.Close()
			if err != nil {
				t.Fatalf("ReadDir(.): %v", err)
			}
			rootNames := map[string]fs.FileMode{}
			for _, e := range rootEntries {
				rootNames[e.Name()] = e.Type()
			}
			for _, want := range []string{"hello.txt", "sub", "big.bin", "link.txt"} {
				if _, ok := rootNames[want]; !ok {
					t.Errorf("root listing missing %q (got %v)", want, rootNames)
				}
			}
			if rootNames["sub"].IsDir() != true {
				t.Errorf("sub not reported as a directory (mode %v)", rootNames["sub"])
			}
			if rootNames["link.txt"]&fs.ModeSymlink == 0 {
				t.Errorf("link.txt not reported as a symlink (mode %v)", rootNames["link.txt"])
			}

			// list a subdirectory
			sdf, err := sess.Open("sub")
			if err != nil {
				t.Fatalf("Open(sub): %v", err)
			}
			subRD, ok := sdf.(readDirFS)
			if !ok {
				sdf.Close()
				t.Fatal("opened sub does not implement ReadDir")
			}
			subEntries, err := subRD.ReadDir(0)
			sdf.Close()
			if err != nil {
				t.Fatalf("ReadDir(sub): %v", err)
			}
			if len(subEntries) != 1 || subEntries[0].Name() != "inner.txt" {
				t.Errorf("sub listing = %v, want exactly [inner.txt]", dirEntryNames(subEntries))
			}

			// pull a file from a subdirectory
			if data := openAndRead(t, sess, "sub/inner.txt"); !bytes.Equal(data, files["sub/inner.txt"]) {
				t.Errorf("sub/inner.txt content mismatch")
			}

			// pull the large file (spans multiple delta chunks)
			if data := openAndRead(t, sess, "big.bin"); !bytes.Equal(data, files["big.bin"]) {
				t.Errorf("big.bin content mismatch: got %d bytes, want %d", len(data), len(files["big.bin"]))
			}

			// resolve the symlink
			target, err := fs.ReadLink(sess, "link.txt")
			if err != nil {
				t.Fatalf("ReadLink(link.txt): %v", err)
			}
			if target != "hello.txt" {
				t.Errorf("ReadLink = %q, want hello.txt", target)
			}
		})
	}
}

// openAndRead opens path and returns its full content, failing the test on
// any error.
func openAndRead(t *testing.T, sess *Session, path string) []byte {
	t.Helper()
	f, err := sess.Open(path)
	if err != nil {
		t.Fatalf("Open(%q): %v", path, err)
	}
	data, err := io.ReadAll(f)
	f.Close()
	if err != nil {
		t.Fatalf("ReadAll(%q): %v", path, err)
	}
	return data
}

// dirEntryNames returns the names of a DirEntry slice for error messages.
func dirEntryNames(entries []fs.DirEntry) []string {
	names := make([]string, len(entries))
	for i, e := range entries {
		names[i] = e.Name()
	}
	return names
}

// TestIntegration_ClientOpenRoot_RealDaemon exercises root mode against a
// real daemon: the module listing (a #list request) and opening a file
// through a module path (each on its own connection).
func TestIntegration_ClientOpenRoot_RealDaemon(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in -short mode")
	}

	for _, rsyncBin := range rsyncBinPaths(t) {
		base := filepath.Base(rsyncBin)
		t.Run(base, func(t *testing.T) {
			if _, known := daemonProtoVersionFor(t, rsyncBin); !known {
				t.Skipf("could not determine protocol version for %s", base)
			}

			srcDir := t.TempDir()
			files := map[string][]byte{}
			files["hello.txt"] = []byte("root mode file\n")
			if err := os.WriteFile(filepath.Join(srcDir, "hello.txt"), files["hello.txt"], 0o644); err != nil {
				t.Fatalf("write fixture: %v", err)
			}
			addr, stop := startTestDaemon(t, rsyncBin, srcDir, "testmod")
			defer stop()

			c := Client{
				ConnectFunc: func(moduleName string) (io.ReadWriter, error) {
					return net.Dial("tcp", addr)
				},
			}
			sess, err := c.OpenRoot()
			if err != nil {
				t.Fatalf("OpenRoot: %v", err)
			}

			// the root listing is a #list request; it must contain the module
			df, err := sess.Open(".")
			if err != nil {
				t.Fatalf("Open(.): %v", err)
			}
			rootRD, ok := df.(readDirFS)
			if !ok {
				df.Close()
				t.Fatal("root does not implement ReadDir")
			}
			entries, err := rootRD.ReadDir(0)
			df.Close()
			if err != nil {
				t.Fatalf("ReadDir(.): %v", err)
			}
			names := dirEntryNames(entries)
			if !containsString(names, "testmod") {
				t.Errorf("root listing %v does not contain module testmod", names)
			}

			// open a file through the module path
			if data := openAndRead(t, sess, "testmod/hello.txt"); !bytes.Equal(data, files["hello.txt"]) {
				t.Errorf("testmod/hello.txt content mismatch: got %q", data)
			}
		})
	}
}

func containsString(slice []string, s string) bool {
	for _, v := range slice {
		if v == s {
			return true
		}
	}
	return false
}
