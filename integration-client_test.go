package rsyncfs

import (
	"fmt"
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

// daemonProtoVersion is the protocol version each pinned rsync binary
// advertises, from .upstream/old_versions/README.md.  The key is the
// binary's base name; the bare "rsync" entry is the current .upstream
// build (3.5.0).
var daemonProtoVersion = map[string]int{
	"rsync":       32,
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
			wantVer, known := daemonProtoVersion[base]
			if !known {
				t.Skipf("no known protocol version for %s", base)
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
	rsyncBin := bins[0]
	base := filepath.Base(rsyncBin)
	daemonVer, known := daemonProtoVersion[base]
	if !known || daemonVer < 30 {
		t.Skipf("daemon %s (proto %d) too old for a downgrade probe", base, daemonVer)
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
