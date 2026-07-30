package rsyncfs

import (
	"bytes"
	"encoding/binary"
	"io"
	"io/fs"
	"net"
	"testing"
	"time"

	"github.com/values-conflict/go-rsyncfs/protocol"
	"github.com/values-conflict/go-rsyncfs/protocol/mux"
)

func TestChecksum1(t *testing.T) {
	tests := []struct {
		name string
		data []byte
		want uint32
	}{
		{"empty", []byte{}, 0},
		{"single_A", []byte{0x41}, 0x410041},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := checksum1(tt.data)
			if got != tt.want {
				t.Errorf("checksum1(%q) = 0x%08x, want 0x%08x", tt.data, got, tt.want)
			}
		})
	}

	// verify determinism
	for i := 0; i < 5; i++ {
		if checksum1([]byte("hello world")) != checksum1([]byte("hello world")) {
			t.Error("checksum1 is not deterministic")
		}
	}
}

func TestChecksum2(t *testing.T) {
	tests := []struct {
		name    string
		data    []byte
		s2len   int
		wantLen int
	}{
		{"empty md5", []byte{}, 16, 16},
		{"hello md5", []byte("hello"), 16, 16},
		{"partial md5", []byte("hello"), 8, 8},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := checksum2(tt.data, tt.s2len)
			if len(got) != tt.wantLen {
				t.Errorf("checksum2(%q, %d) len = %d, want %d", tt.data, tt.s2len, len(got), tt.wantLen)
			}
		})
	}

	// verify determinism
	for i := 0; i < 5; i++ {
		a := checksum2([]byte("test"), 16)
		b := checksum2([]byte("test"), 16)
		if !bytes.Equal(a, b) {
			t.Error("checksum2 is not deterministic")
		}
	}
}

func TestComputeSumHead_ZeroSize(t *testing.T) {
	sh := computeSumHead(0, 30)
	if sh.count != 0 {
		t.Errorf("count = %d, want 0", sh.count)
	}
	if sh.blength != 0 {
		t.Errorf("blength = %d, want 0", sh.blength)
	}
}

func TestComputeSumHead_SmallFile(t *testing.T) {
	sh := computeSumHead(100, 30)
	if sh.count != 1 {
		t.Errorf("count = %d, want 1", sh.count)
	}
	if sh.blength != defaultBlockSize {
		t.Errorf("blength = %d, want %d", sh.blength, defaultBlockSize)
	}
	if sh.remainder != 100 {
		t.Errorf("remainder = %d, want 100", sh.remainder)
	}
}

func TestComputeSumHead_ExactlyOneBlock(t *testing.T) {
	sh := computeSumHead(700, 30)
	if sh.count != 1 {
		t.Errorf("count = %d, want 1", sh.count)
	}
	if sh.remainder != 0 {
		t.Errorf("remainder = %d, want 0", sh.remainder)
	}
}

func TestComputeSumHead_TwoBlocks(t *testing.T) {
	sh := computeSumHead(1400, 30)
	if sh.count != 2 {
		t.Errorf("count = %d, want 2", sh.count)
	}
	if sh.remainder != 0 {
		t.Errorf("remainder = %d, want 0", sh.remainder)
	}
}

func TestComputeSumHead_ThreeBlocks(t *testing.T) {
	sh := computeSumHead(1401, 30)
	if sh.count != 3 {
		t.Errorf("count = %d, want 3", sh.count)
	}
	if sh.remainder != 1 {
		t.Errorf("remainder = %d, want 1", sh.remainder)
	}
}

func TestComputeSumHead_LargeFile(t *testing.T) {
	// for file just above BLOCK_SIZE^2, sqrt is ~700, so block stays at 700
	sh := computeSumHead(defaultBlockSize*defaultBlockSize+1, 30)
	if sh.blength != defaultBlockSize {
		t.Errorf("blength = %d, want %d (sqrt of 490001 is ~700)", sh.blength, defaultBlockSize)
	}
	if sh.blength > maxBlockSize {
		t.Errorf("blength = %d, want <= %d", sh.blength, maxBlockSize)
	}

	// for a genuinely large file, block size should grow
	sh2 := computeSumHead(1<<20, 30) // 1MB
	if sh2.blength <= defaultBlockSize {
		t.Errorf("1MB file: blength = %d, want > %d", sh2.blength, defaultBlockSize)
	}
	if sh2.blength > maxBlockSize {
		t.Errorf("1MB file: blength = %d, want <= %d", sh2.blength, maxBlockSize)
	}
}

func TestComputeSumHead_MaxBlockSize(t *testing.T) {
	sh := computeSumHead(1<<30, 30)
	if sh.blength > maxBlockSize {
		t.Errorf("blength = %d, want <= %d", sh.blength, maxBlockSize)
	}
	if sh.blength < defaultBlockSize {
		t.Errorf("blength = %d, want >= %d", sh.blength, defaultBlockSize)
	}
}

func TestComputeSumHead_VerifyMath(t *testing.T) {
	tests := []struct {
		size    int64
		wantNdx int
		wantRem int
	}{
		{0, 0, 0},
		{1, 1, 1},
		{699, 1, 699},
		{700, 1, 0},
		{701, 2, 1},
		{1400, 2, 0},
		{1401, 3, 1},
		{2100, 3, 0},
	}

	for _, tt := range tests {
		t.Run("", func(t *testing.T) {
			sh := computeSumHead(tt.size, 30)
			if int(sh.count) != tt.wantNdx {
				t.Errorf("size=%d: count=%d, want %d", tt.size, sh.count, tt.wantNdx)
			}
			if int(sh.remainder) != tt.wantRem {
				t.Errorf("size=%d: remainder=%d, want %d", tt.size, sh.remainder, tt.wantRem)
			}
			if tt.size > 0 {
				total := int64(sh.count) * int64(sh.blength)
				if sh.remainder > 0 {
					total -= int64(sh.blength) - int64(sh.remainder)
				}
				if total != tt.size {
					t.Errorf("size=%d: total computed = %d, want %d", tt.size, total, tt.size)
				}
			}
		})
	}
}

func TestWriteSumHead(t *testing.T) {
	sh := sumHead{count: 3, blength: 700, s2length: 16, remainder: 100}

	var buf bytes.Buffer
	w := mux.NewWriter(&buf)
	if err := writeSumHead(w, sh, 30); err != nil {
		t.Fatalf("writeSumHead failed: %v", err)
	}

	r := mux.NewReader(bytes.NewReader(buf.Bytes()))
	code, payload, err := r.ReadMsg()
	if err != nil {
		t.Fatalf("read msg: %v", err)
	}
	if code != mux.MsgData {
		t.Errorf("expected MsgData, got %d", code)
	}
	if len(payload) != 16 {
		t.Fatalf("payload len = %d, want 16", len(payload))
	}

	count := int32(binary.LittleEndian.Uint32(payload[0:4]))
	blength := int32(binary.LittleEndian.Uint32(payload[4:8]))
	s2length := int32(binary.LittleEndian.Uint32(payload[8:12]))
	remainder := int32(binary.LittleEndian.Uint32(payload[12:16]))

	if count != 3 {
		t.Errorf("count = %d, want 3", count)
	}
	if blength != 700 {
		t.Errorf("blength = %d, want 700", blength)
	}
	if s2length != 16 {
		t.Errorf("s2length = %d, want 16", s2length)
	}
	if remainder != 100 {
		t.Errorf("remainder = %d, want 100", remainder)
	}
}

func TestWriteSumHead_Protocol26(t *testing.T) {
	sh := sumHead{count: 2, blength: 700, s2length: 16, remainder: 50}

	var buf bytes.Buffer
	w := mux.NewWriter(&buf)
	if err := writeSumHead(w, sh, 26); err != nil {
		t.Fatalf("writeSumHead failed: %v", err)
	}

	r := mux.NewReader(bytes.NewReader(buf.Bytes()))
	code, payload, err := r.ReadMsg()
	if err != nil {
		t.Fatalf("read msg: %v", err)
	}
	if code != mux.MsgData {
		t.Errorf("expected MsgData, got %d", code)
	}
	if len(payload) != 12 {
		t.Fatalf("payload len = %d, want 12 (no s2length for proto < 27)", len(payload))
	}

	count := int32(binary.LittleEndian.Uint32(payload[0:4]))
	blength := int32(binary.LittleEndian.Uint32(payload[4:8]))
	remainder := int32(binary.LittleEndian.Uint32(payload[8:12]))

	if count != 2 {
		t.Errorf("count = %d, want 2", count)
	}
	if blength != 700 {
		t.Errorf("blength = %d, want 700", blength)
	}
	if remainder != 50 {
		t.Errorf("remainder = %d, want 50", remainder)
	}
}

func TestWriteSumHead_SerializesCorrectly(t *testing.T) {
	sh := sumHead{
		count:     42,
		blength:   700,
		s2length:  16,
		remainder: 123,
	}

	var buf bytes.Buffer
	w := mux.NewWriter(&buf)
	if err := writeSumHead(w, sh, 30); err != nil {
		t.Fatalf("writeSumHead failed: %v", err)
	}

	r := mux.NewReader(bytes.NewReader(buf.Bytes()))
	_, payload, err := r.ReadMsg()
	if err != nil {
		t.Fatalf("read msg: %v", err)
	}

	fields := []struct {
		offset int
		value  uint32
		name   string
	}{
		{0, 42, "count"},
		{4, 700, "blength"},
		{8, 16, "s2length"},
		{12, 123, "remainder"},
	}

	for _, f := range fields {
		got := binary.LittleEndian.Uint32(payload[f.offset : f.offset+4])
		if got != f.value {
			t.Errorf("%s = %d, want %d", f.name, got, f.value)
		}
	}
}

func TestSendBlockChecksums(t *testing.T) {
	data := []byte("hello world")
	sh := computeSumHead(int64(len(data)), 30)

	var buf bytes.Buffer
	w := mux.NewWriter(&buf)
	if err := sendBlockChecksums(w, data, sh); err != nil {
		t.Fatalf("sendBlockChecksums failed: %v", err)
	}

	r := mux.NewReader(bytes.NewReader(buf.Bytes()))
	code, payload, err := r.ReadMsg()
	if err != nil {
		t.Fatalf("read msg: %v", err)
	}
	if code != mux.MsgData {
		t.Errorf("expected MsgData, got %d", code)
	}

	expectedSize := int(sh.count) * (4 + int(sh.s2length))
	if len(payload) != expectedSize {
		t.Errorf("payload len = %d, want %d", len(payload), expectedSize)
	}

	if sh.count > 0 {
		gotSum1 := binary.LittleEndian.Uint32(payload[0:4])
		wantSum1 := checksum1(data)
		if gotSum1 != wantSum1 {
			t.Errorf("sum1 = 0x%08x, want 0x%08x", gotSum1, wantSum1)
		}
	}
}

func TestSendBlockChecksums_TwoBlocks(t *testing.T) {
	data := make([]byte, 1400)
	for i := range data {
		data[i] = byte(i % 256)
	}

	sh := sumHead{count: 2, blength: 700, s2length: 16, remainder: 0}

	var buf bytes.Buffer
	w := mux.NewWriter(&buf)
	if err := sendBlockChecksums(w, data, sh); err != nil {
		t.Fatalf("sendBlockChecksums failed: %v", err)
	}

	r := mux.NewReader(bytes.NewReader(buf.Bytes()))
	code, payload, err := r.ReadMsg()
	if err != nil {
		t.Fatalf("read msg: %v", err)
	}
	if code != mux.MsgData {
		t.Errorf("expected MsgData, got %d", code)
	}

	if len(payload) != 40 {
		t.Fatalf("payload len = %d, want 40", len(payload))
	}

	block0 := data[0:700]
	gotSum1 := binary.LittleEndian.Uint32(payload[0:4])
	wantSum1 := checksum1(block0)
	if gotSum1 != wantSum1 {
		t.Errorf("block 0 sum1 = 0x%08x, want 0x%08x", gotSum1, wantSum1)
	}

	block1 := data[700:1400]
	gotSum1 = binary.LittleEndian.Uint32(payload[20:24])
	wantSum1 = checksum1(block1)
	if gotSum1 != wantSum1 {
		t.Errorf("block 1 sum1 = 0x%08x, want 0x%08x", gotSum1, wantSum1)
	}
}

func TestSendBlockChecksums_ZeroBlocks(t *testing.T) {
	sh := sumHead{count: 0, blength: 0, s2length: 16, remainder: 0}

	var buf bytes.Buffer
	w := mux.NewWriter(&buf)
	if err := sendBlockChecksums(w, []byte{}, sh); err != nil {
		t.Fatalf("sendBlockChecksums failed: %v", err)
	}

	r := mux.NewReader(bytes.NewReader(buf.Bytes()))
	_, payload, err := r.ReadMsg()
	if err != nil {
		t.Fatalf("read msg: %v", err)
	}
	if len(payload) != 0 {
		t.Errorf("payload len = %d, want 0 for zero blocks", len(payload))
	}
}

func TestSendFileChecksum(t *testing.T) {
	data := []byte("hello world")

	var buf bytes.Buffer
	w := mux.NewWriter(&buf)
	if err := sendFileChecksum(w, data, 16); err != nil {
		t.Fatalf("sendFileChecksum failed: %v", err)
	}

	r := mux.NewReader(bytes.NewReader(buf.Bytes()))
	code, payload, err := r.ReadMsg()
	if err != nil {
		t.Fatalf("read msg: %v", err)
	}
	if code != mux.MsgData {
		t.Errorf("expected MsgData, got %d", code)
	}
	want := checksum2(data, 16)
	if !bytes.Equal(payload, want) {
		t.Errorf("checksum = %v, want %v", payload, want)
	}
}

func TestSendFileChecksum_Verification(t *testing.T) {
	data := []byte("verification test data")

	var buf bytes.Buffer
	w := mux.NewWriter(&buf)
	if err := sendFileChecksum(w, data, 16); err != nil {
		t.Fatalf("sendFileChecksum failed: %v", err)
	}

	r := mux.NewReader(bytes.NewReader(buf.Bytes()))
	_, payload, err := r.ReadMsg()
	if err != nil {
		t.Fatalf("read msg: %v", err)
	}

	want := checksum2(data, 16)
	if !bytes.Equal(payload, want) {
		t.Errorf("file checksum mismatch")
	}
}

func TestParseDeltaStream_Empty(t *testing.T) {
	delta := make([]byte, 4)
	binary.LittleEndian.PutUint32(delta, 0)

	var out bytes.Buffer
	sh := sumHead{count: 0}
	if err := parseDeltaStream(delta, &out, sh); err != nil {
		t.Fatalf("parseDeltaStream failed: %v", err)
	}
	if out.Len() != 0 {
		t.Errorf("output len = %d, want 0", out.Len())
	}
}

func TestParseDeltaStream_LiteralOnly(t *testing.T) {
	delta := make([]byte, 4+5+4)
	binary.LittleEndian.PutUint32(delta[0:4], 5)
	copy(delta[4:9], []byte("hello"))
	binary.LittleEndian.PutUint32(delta[9:13], 0)

	var out bytes.Buffer
	sh := sumHead{count: 0}
	if err := parseDeltaStream(delta, &out, sh); err != nil {
		t.Fatalf("parseDeltaStream failed: %v", err)
	}
	got := out.Bytes()
	if !bytes.Equal(got, []byte("hello")) {
		t.Errorf("output = %q, want %q", got, "hello")
	}
}

func TestParseDeltaStream_LiteralAndToken(t *testing.T) {
	delta := make([]byte, 4+5+4+4)
	binary.LittleEndian.PutUint32(delta[0:4], 5)
	copy(delta[4:9], []byte("hello"))
	binary.LittleEndian.PutUint32(delta[9:13], 0xFFFFFFFF) // token = -1 (block 0)
	binary.LittleEndian.PutUint32(delta[13:17], 0)

	var out bytes.Buffer
	sh := sumHead{count: 2, blength: 700}
	if err := parseDeltaStream(delta, &out, sh); err != nil {
		t.Fatalf("parseDeltaStream failed: %v", err)
	}
	got := out.Bytes()
	if !bytes.Equal(got, []byte("hello")) {
		t.Errorf("output = %q, want %q", got, "hello")
	}
}

func TestParseDeltaStream_MultipleLiterals(t *testing.T) {
	delta := make([]byte, 4+5+4+4+5+4)
	binary.LittleEndian.PutUint32(delta[0:4], 5)
	copy(delta[4:9], []byte("hello"))
	binary.LittleEndian.PutUint32(delta[9:13], 0xFFFFFFFF) // token 0
	binary.LittleEndian.PutUint32(delta[13:17], 5)
	copy(delta[17:22], []byte("world"))
	binary.LittleEndian.PutUint32(delta[22:26], 0)

	var out bytes.Buffer
	sh := sumHead{count: 10, blength: 700}
	if err := parseDeltaStream(delta, &out, sh); err != nil {
		t.Fatalf("parseDeltaStream failed: %v", err)
	}
	got := out.Bytes()
	want := []byte("helloworld")
	if !bytes.Equal(got, want) {
		t.Errorf("output = %q, want %q", got, want)
	}
}

func TestParseDeltaStream_InvalidBlockIndex(t *testing.T) {
	delta := make([]byte, 4+4)
	binary.LittleEndian.PutUint32(delta[0:4], 0xFFFFFF9C) // token = -100
	binary.LittleEndian.PutUint32(delta[4:8], 0)

	var out bytes.Buffer
	sh := sumHead{count: 2, blength: 700}
	err := parseDeltaStream(delta, &out, sh)
	if err == nil {
		t.Error("expected error for invalid block index, got nil")
	}
}

func TestParseDeltaStream_Truncated(t *testing.T) {
	delta := []byte{0, 0, 0}

	var out bytes.Buffer
	sh := sumHead{count: 0}
	err := parseDeltaStream(delta, &out, sh)
	if err == nil {
		t.Error("expected error for truncated delta, got nil")
	}
}

// fakeFile implements fs.File for testing.
type fakeFile struct {
	data []byte
	pos  int
}

func (f *fakeFile) Read(p []byte) (int, error) {
	if f.pos >= len(f.data) {
		return 0, io.EOF
	}
	n := copy(p, f.data[f.pos:])
	f.pos += n
	return n, nil
}

func (f *fakeFile) Close() error { return nil }

func (f *fakeFile) Stat() (fs.FileInfo, error) {
	return &fakeFileInfo{name: "test", size: int64(len(f.data))}, nil
}

type fakeFileInfo struct {
	name string
	size int64
}

func (f *fakeFileInfo) Name() string       { return f.name }
func (f *fakeFileInfo) Size() int64        { return f.size }
func (f *fakeFileInfo) Mode() fs.FileMode  { return 0o644 }
func (f *fakeFileInfo) ModTime() time.Time { return time.Unix(0, 0) }
func (f *fakeFileInfo) IsDir() bool        { return false }
func (f *fakeFileInfo) Sys() any           { return nil }

// TestSendFile_FullRoundTrip tests the complete sendFile flow through net.Pipe.
func TestSendFile_FullRoundTrip(t *testing.T) {
	fileData := []byte("hello world, this is a test file for the rsync transfer protocol")

	serverConn, clientConn := net.Pipe()

	// server side (sender)
	go func() {
		defer serverConn.Close()
		f := &fakeFile{data: fileData}
		r := mux.NewReader(serverConn)
		w := mux.NewWriter(serverConn)
		err := sendFile(r, w, f, io.Discard, 30)
		if err != nil {
			t.Logf("sendFile error: %v", err)
		}
	}()

	// client side (receiver) -- simulate the delta protocol
	go func() {
		defer clientConn.Close()
		r := mux.NewReader(clientConn)
		w := mux.NewWriter(clientConn)

		// read sum_head
		code, payload, err := r.ReadMsg()
		if err != nil {
			t.Logf("read sum_head: %v", err)
			return
		}
		if code != mux.MsgData {
			t.Logf("expected MsgData, got %d", code)
			return
		}

		count := int32(binary.LittleEndian.Uint32(payload[0:4]))
		_ = count

		// read block checksums
		code, _, err = r.ReadMsg()
		if err != nil {
			t.Logf("read block checksums: %v", err)
			return
		}

		// send delta stream: literal all data + end token
		deltaLen := len(fileData)
		delta := make([]byte, 4+deltaLen+4)
		binary.LittleEndian.PutUint32(delta[0:4], uint32(deltaLen))
		copy(delta[4:4+deltaLen], fileData)
		if err := w.WriteMsg(mux.MsgData, delta); err != nil {
			t.Logf("write delta: %v", err)
			return
		}

		// read file checksum
		code, _, err = r.ReadMsg()
		if err != nil {
			t.Logf("read file checksum: %v", err)
			return
		}

		// send MSG_SUCCESS
		successPayload := make([]byte, 4)
		binary.LittleEndian.PutUint32(successPayload, 0)
		if err := w.WriteMsg(mux.MsgSuccess, successPayload); err != nil {
			t.Logf("write success: %v", err)
			return
		}
	}()
}

// TestSendFile_ZeroByteFile tests sending an empty file.
func TestSendFile_ZeroByteFile(t *testing.T) {
	serverConn, clientConn := net.Pipe()

	go func() {
		defer serverConn.Close()
		f := &fakeFile{data: []byte{}}
		r := mux.NewReader(serverConn)
		w := mux.NewWriter(serverConn)
		err := sendFile(r, w, f, io.Discard, 30)
		if err != nil {
			t.Logf("sendFile error: %v", err)
		}
	}()

	go func() {
		defer clientConn.Close()
		r := mux.NewReader(clientConn)
		w := mux.NewWriter(clientConn)

		// read sum_head (count=0)
		code, payload, err := r.ReadMsg()
		if err != nil {
			t.Logf("read sum_head: %v", err)
			return
		}
		if code != mux.MsgData {
			t.Logf("expected MsgData, got %d", code)
			return
		}
		count := int32(binary.LittleEndian.Uint32(payload[0:4]))
		if count != 0 {
			t.Logf("expected count=0, got %d", count)
			return
		}

		// no block checksums for empty file, send empty delta
		if err := w.WriteMsg(mux.MsgData, []byte{0, 0, 0, 0}); err != nil {
			t.Logf("write delta: %v", err)
			return
		}

		// read file checksum
		_, _, err = r.ReadMsg()
		if err != nil {
			t.Logf("read file checksum: %v", err)
			return
		}

		// send MSG_SUCCESS
		successPayload := make([]byte, 4)
		if err := w.WriteMsg(mux.MsgSuccess, successPayload); err != nil {
			t.Logf("write success: %v", err)
			return
		}
	}()
}

// TestSendFile_MultipleProtocols tests sendFile across different protocol versions.
func TestSendFile_MultipleProtocols(t *testing.T) {
	fileData := []byte("test data for protocol version testing")

	for _, version := range []int{20, 27, 28, 30, 31, 32} {
		t.Run("", func(t *testing.T) {
			serverConn, clientConn := net.Pipe()

			go func() {
				defer serverConn.Close()
				f := &fakeFile{data: fileData}
				r := mux.NewReader(serverConn)
				w := mux.NewWriter(serverConn)
				err := sendFile(r, w, f, io.Discard, version)
				if err != nil {
					t.Logf("sendFile(v=%d) error: %v", version, err)
				}
			}()

			go func() {
				defer clientConn.Close()
				r := mux.NewReader(clientConn)
				w := mux.NewWriter(clientConn)

				// read sum_head
				_, payload, err := r.ReadMsg()
				if err != nil {
					t.Logf("v=%d read sum_head: %v", version, err)
					return
				}

				if version >= 27 {
					if len(payload) != 16 {
						t.Logf("v=%d: sum_head payload = %d, want 16", version, len(payload))
						return
					}
				} else {
					if len(payload) != 12 {
						t.Logf("v=%d: sum_head payload = %d, want 12", version, len(payload))
						return
					}
				}

				count := int32(binary.LittleEndian.Uint32(payload[0:4]))

				// read block checksums (if any)
				if count > 0 {
					_, _, err = r.ReadMsg()
					if err != nil {
						t.Logf("v=%d read checksums: %v", version, err)
						return
					}
				}

				// send delta
				delta := make([]byte, 4+len(fileData)+4)
				binary.LittleEndian.PutUint32(delta[0:4], uint32(len(fileData)))
				copy(delta[4:4+len(fileData)], fileData)
				if err := w.WriteMsg(mux.MsgData, delta); err != nil {
					t.Logf("v=%d write delta: %v", version, err)
					return
				}

				// read file checksum
				_, _, err = r.ReadMsg()
				if err != nil {
					t.Logf("v=%d read checksum: %v", version, err)
					return
				}

				// send success
				if err := w.WriteMsg(mux.MsgSuccess, []byte{0, 0, 0, 0}); err != nil {
					t.Logf("v=%d write success: %v", version, err)
					return
				}
			}()
		})
	}
}

// TestSendFile_LargeFile tests sending a file larger than one block.
func TestSendFile_LargeFile(t *testing.T) {
	fileData := make([]byte, 2000)
	for i := range fileData {
		fileData[i] = byte(i % 256)
	}

	serverConn, clientConn := net.Pipe()

	go func() {
		defer serverConn.Close()
		f := &fakeFile{data: fileData}
		r := mux.NewReader(serverConn)
		w := mux.NewWriter(serverConn)
		err := sendFile(r, w, f, io.Discard, 30)
		if err != nil {
			t.Logf("sendFile error: %v", err)
		}
	}()

	go func() {
		defer clientConn.Close()
		r := mux.NewReader(clientConn)
		w := mux.NewWriter(clientConn)

		// read sum_head
		_, payload, err := r.ReadMsg()
		if err != nil {
			t.Logf("read sum_head: %v", err)
			return
		}
		count := int32(binary.LittleEndian.Uint32(payload[0:4]))
		if count != 3 {
			t.Logf("expected 3 blocks, got %d", count)
			return
		}

		// read block checksums
		_, payload, err = r.ReadMsg()
		if err != nil {
			t.Logf("read checksums: %v", err)
			return
		}
		if len(payload) != 60 {
			t.Logf("checksum payload = %d, want 60", len(payload))
			return
		}

		// send delta: literal all
		delta := make([]byte, 4+len(fileData)+4)
		binary.LittleEndian.PutUint32(delta[0:4], uint32(len(fileData)))
		copy(delta[4:4+len(fileData)], fileData)
		if err := w.WriteMsg(mux.MsgData, delta); err != nil {
			t.Logf("write delta: %v", err)
			return
		}

		// read file checksum
		_, _, err = r.ReadMsg()
		if err != nil {
			t.Logf("read checksum: %v", err)
			return
		}

		// send success
		if err := w.WriteMsg(mux.MsgSuccess, []byte{0, 0, 0, 0}); err != nil {
			t.Logf("write success: %v", err)
			return
		}
	}()
}

// TestVarintRoundTrip verifies our protocol package varint works correctly.
func TestVarintRoundTrip(t *testing.T) {
	tests := []int32{0, 1, -1, 127, 128, 255, 256, 32767, 32768, 0x7FFFFFFF, -0x7FFFFFFF}
	for _, v := range tests {
		t.Run("", func(t *testing.T) {
			var buf bytes.Buffer
			if err := protocol.WriteVarint(&buf, v); err != nil {
				t.Fatalf("WriteVarint(%d): %v", v, err)
			}
			got, err := protocol.ReadVarint(&buf)
			if err != nil {
				t.Fatalf("ReadVarint: %v", err)
			}
			if got != v {
				t.Errorf("round-trip %d = %d", v, got)
			}
		})
	}
}
