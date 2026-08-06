package rsyncfs

import (
	"bytes"
	"encoding/binary"
	"io/fs"
	"testing"
	"testing/fstest"
	"time"

	"github.com/values-conflict/go-rsyncfs/protocol/mux"
)

// readFlistPayload reads the MSG_DATA payload from a mux-encoded buffer.
func readFlistPayload(t *testing.T, data []byte) []byte {
	t.Helper()
	r := mux.NewReader(bytes.NewReader(data))
	code, payload, err := r.ReadMsg()
	if err != nil {
		t.Fatalf("read msg: %v", err)
	}
	if code != mux.MsgData {
		t.Fatalf("expected MsgData, got code %d", code)
	}
	return payload
}

func TestSendFileList_EmptyDir(t *testing.T) {
	mapFS := fstest.MapFS{}
	srv, _ := NewServer(&ServerModule{Name: "test", FS: mapFS})

	var buf bytes.Buffer
	w := mux.NewWriter(&buf)

	err := sendFileList(w, srv.modules["test"].FS, ".", 30, false)
	if err != nil {
		t.Fatalf("sendFileList failed: %v", err)
	}

	payload := readFlistPayload(t, buf.Bytes())
	t.Logf("empty dir payload: %v", payload)

	// root entry "." is always included, followed by end marker + NDX_DONE
	// verify the payload ends with NDX_DONE (0x00 for proto >= 30)
	if len(payload) == 0 {
		t.Fatal("payload is empty")
	}
	if payload[len(payload)-1] != 0 {
		t.Errorf("last byte = 0x%02x, want 0x00 (NDX_DONE)", payload[len(payload)-1])
	}
}

func TestSendFileList_SingleFile(t *testing.T) {
	modTime := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	mapFS := fstest.MapFS{
		"hello.txt": {
			Data:    []byte("hello world"),
			ModTime: modTime,
		},
	}
	srv, _ := NewServer(&ServerModule{Name: "test", FS: mapFS})

	var buf bytes.Buffer
	w := mux.NewWriter(&buf)

	err := sendFileList(w, srv.modules["test"].FS, ".", 30, false)
	if err != nil {
		t.Fatalf("sendFileList failed: %v", err)
	}

	payload := readFlistPayload(t, buf.Bytes())
	t.Logf("payload: %v", payload)

	// verify structural properties
	// 1. starts with root entry xflags (0x18 = sameUID | sameGID)
	if payload[0] != 0x18 {
		t.Errorf("first byte (root xflags) = 0x%02x, want 0x18", payload[0])
	}

	// 2. root name suffix length = 1 (for ".")
	if payload[1] != 1 {
		t.Errorf("root name len = %d, want 1", payload[1])
	}

	// 3. root name = "."
	if payload[2] != '.' {
		t.Errorf("root name = %q, want %q", string(payload[2]), ".")
	}

	// 4. payload contains "hello.txt" as a substring
	if !bytes.Contains(payload, []byte("hello.txt")) {
		t.Error("payload missing hello.txt filename")
	}

	// 5. ends with NDX_DONE (0x00)
	if payload[len(payload)-1] != 0 {
		t.Errorf("last byte = 0x%02x, want 0x00 (NDX_DONE)", payload[len(payload)-1])
	}

	// 6. second-to-last byte is end-of-list marker (0x00)
	if payload[len(payload)-2] != 0 {
		t.Errorf("second-to-last byte = 0x%02x, want 0x00 (end marker)", payload[len(payload)-2])
	}
}

func TestSendFileList_DeltaEncoding(t *testing.T) {
	modTime := time.Date(2024, 6, 15, 12, 0, 0, 0, time.UTC)
	mapFS := fstest.MapFS{
		"a.txt": {
			Data:    []byte("aaa"),
			ModTime: modTime,
			Mode:    0o644,
		},
		"b.txt": {
			Data:    []byte("bbb"),
			ModTime: modTime, // same mtime
			Mode:    0o644,   // same mode
		},
	}
	srv, _ := NewServer(&ServerModule{Name: "test", FS: mapFS})

	var buf bytes.Buffer
	w := mux.NewWriter(&buf)

	err := sendFileList(w, srv.modules["test"].FS, ".", 30, false)
	if err != nil {
		t.Fatalf("sendFileList failed: %v", err)
	}

	payload := readFlistPayload(t, buf.Bytes())
	t.Logf("payload: %v", payload)

	// entries: ".", "a.txt", "b.txt"
	// b.txt should have xmitSameMode and xmitSameTime set (same as a.txt)
	// we can verify by checking the xflags pattern
}

func TestSendFileList_NamePrefixReuse(t *testing.T) {
	modTime := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	mapFS := fstest.MapFS{
		"dir/file1.txt": {
			Data:    []byte("one"),
			ModTime: modTime,
		},
		"dir/file2.txt": {
			Data:    []byte("two"),
			ModTime: modTime,
		},
	}
	srv, _ := NewServer(&ServerModule{Name: "test", FS: mapFS})

	var buf bytes.Buffer
	w := mux.NewWriter(&buf)

	err := sendFileList(w, srv.modules["test"].FS, ".", 30, false)
	if err != nil {
		t.Fatalf("sendFileList failed: %v", err)
	}

	payload := readFlistPayload(t, buf.Bytes())
	t.Logf("payload: %v", payload)

	// dir/file2.txt should share prefix "dir/file" with dir/file1.txt
	// so xmitSameName should be set
}

func TestSendFileList_Directory(t *testing.T) {
	mapFS := fstest.MapFS{
		"subdir":       {Mode: fs.ModeDir | 0o755},
		"subdir/inner": {Data: []byte("inner")},
	}
	srv, _ := NewServer(&ServerModule{Name: "test", FS: mapFS})

	var buf bytes.Buffer
	w := mux.NewWriter(&buf)

	err := sendFileList(w, srv.modules["test"].FS, ".", 30, false)
	if err != nil {
		t.Fatalf("sendFileList failed: %v", err)
	}

	payload := readFlistPayload(t, buf.Bytes())
	t.Logf("payload: %v", payload)

	// verify end marker and NDX_DONE are present at the end
	if len(payload) < 2 {
		t.Fatal("payload too short")
	}
	// last byte should be 0x00 (NDX_DONE compressed)
	if payload[len(payload)-1] != 0 {
		t.Errorf("last byte = 0x%02x, want 0x00 (NDX_DONE)", payload[len(payload)-1])
	}
}

func TestSendFileList_ProtocolVersions(t *testing.T) {
	mapFS := fstest.MapFS{
		"file.txt": {Data: []byte("test")},
	}
	srv, _ := NewServer(&ServerModule{Name: "test", FS: mapFS})

	for _, version := range []int{20, 27, 28, 29, 30, 31, 32} {
		t.Run("", func(t *testing.T) {
			var buf bytes.Buffer
			w := mux.NewWriter(&buf)
			err := sendFileList(w, srv.modules["test"].FS, ".", version, false)
			if err != nil {
				t.Fatalf("sendFileList(version=%d) failed: %v", version, err)
			}
			payload := readFlistPayload(t, buf.Bytes())
			t.Logf("version %d payload: %v", version, payload)
		})
	}
}

func TestSendFileList_VarintFlistFlags(t *testing.T) {
	mapFS := fstest.MapFS{
		"file.txt": {Data: []byte("test")},
	}
	srv, _ := NewServer(&ServerModule{Name: "test", FS: mapFS})

	var buf bytes.Buffer
	w := mux.NewWriter(&buf)

	err := sendFileList(w, srv.modules["test"].FS, ".", 32, true)
	if err != nil {
		t.Fatalf("sendFileList(varintFlistFlags=true) failed: %v", err)
	}

	payload := readFlistPayload(t, buf.Bytes())
	t.Logf("varint xflags payload: %v", payload)
}

func TestCommonPrefixLen(t *testing.T) {
	tests := []struct {
		a, b string
		want int
	}{
		{"", "hello", 0},
		{"hello", "", 0},
		{"hello", "hello", 5},
		{"hello", "world", 0},
		{"dir/file1", "dir/file2", 8},
		{"abc", "abcdef", 3},
		{"abcdef", "abc", 3},
	}
	for _, tt := range tests {
		t.Run("", func(t *testing.T) {
			got := commonPrefixLen(tt.a, tt.b)
			if got != tt.want {
				t.Errorf("commonPrefixLen(%q, %q) = %d, want %d", tt.a, tt.b, got, tt.want)
			}
		})
	}
}

func TestWriteIntLE(t *testing.T) {
	tests := []struct {
		name string
		val  uint32
		size int
		want []byte
	}{
		{"int32", 0x01020304, 4, []byte{0x04, 0x03, 0x02, 0x01}},
		{"int16", 0x0102, 2, []byte{0x02, 0x01}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer
			if err := writeIntLE(&buf, tt.val, tt.size); err != nil {
				t.Fatalf("writeIntLE failed: %v", err)
			}
			if !bytes.Equal(buf.Bytes(), tt.want) {
				t.Errorf("writeIntLE(0x%08x, %d) = %v, want %v", tt.val, tt.size, buf.Bytes(), tt.want)
			}
		})
	}
}

func TestWriteMode(t *testing.T) {
	tests := []struct {
		mode fs.FileMode
		want uint32
	}{
		{0o644, 0o100644},                  // regular file
		{fs.ModeDir | 0o755, 0o040755},     // directory
		{fs.ModeSymlink | 0o777, 0o120777}, // symlink
	}
	for _, tt := range tests {
		t.Run("", func(t *testing.T) {
			var buf bytes.Buffer
			err := writeMode(&buf, tt.mode)
			if err != nil {
				t.Fatalf("writeMode failed: %v", err)
			}
			got := binary.LittleEndian.Uint32(buf.Bytes())
			if got != tt.want {
				t.Errorf("writeMode(%v) = 0o%o, want 0o%o", tt.mode, got, tt.want)
			}
		})
	}
}

// TestWriteXflags_ProtocolLessThan28 verifies that proto < 28 xflags fallback
// matches upstream: dirs get XMIT_LONG_NAME (0x40), non-dirs get XMIT_TOP_DIR (0x01).
// Verified against upstream flist.c send_file_entry():
//
//	if (!(xflags & 0xFF))
//	    xflags |= S_ISDIR(mode) ? XMIT_LONG_NAME : XMIT_TOP_DIR;
func TestWriteXflags_ProtocolLessThan28(t *testing.T) {
	tests := []struct {
		name string
		mode fs.FileMode
		want byte
		desc string
	}{
		{"regular_file", 0o644, 0x01, "XMIT_TOP_DIR (harmless on files)"},
		{"directory", fs.ModeDir | 0o755, 0x40, "XMIT_LONG_NAME for dirs"},
		{"symlink", fs.ModeSymlink | 0o777, 0x01, "XMIT_TOP_DIR for non-dirs"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer
			err := writeXflags(&buf, 0, tt.mode, 27, false)
			if err != nil {
				t.Fatalf("writeXflags failed: %v", err)
			}
			got := buf.Bytes()
			if len(got) != 1 {
				t.Fatalf("proto < 28 xflags should be 1 byte, got %d bytes: %v", len(got), got)
			}
			if got[0] != tt.want {
				t.Errorf("writeXflags(xflags=0, mode=%v, version=27) = 0x%02x, want 0x%02x (%s)",
					tt.mode, got[0], tt.want, tt.desc)
			}
		})
	}
}
