package protocol

import (
	"bytes"
	"io"
	"testing"
)

func TestChecksum1(t *testing.T) {
	tests := []struct {
		name string
		data []byte
		want uint32
	}{
		{"empty", []byte{}, 0x00000000},
		{"single-byte", []byte("a"), 0x00610061},
		{"three-bytes", []byte("abc"), 0x024A0126},
		{"hello-world", []byte("hello world"), 0x1A00045C},
		{"16x-A", func() []byte {
			d := make([]byte, 16)
			for i := range d {
				d[i] = 0x41
			}
			return d
		}(), 0x22880410},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := Checksum1(tc.data)
			if got != tc.want {
				t.Errorf("Checksum1(%q) = 0x%08X, want 0x%08X", tc.data, got, tc.want)
			}
		})
	}
}

func TestChecksum1_RoundTrip(t *testing.T) {
	// Verify that the rolling checksum is consistent across multiple calls.
	data := []byte("the quick brown fox jumps over the lazy dog")
	want := Checksum1(data)
	for i := 0; i < 100; i++ {
		if got := Checksum1(data); got != want {
			t.Fatalf("Checksum1 iteration %d = 0x%08X, want 0x%08X", i, got, want)
		}
	}
}

func TestChecksum2_MD4(t *testing.T) {
	tests := []struct {
		name     string
		data     []byte
		s2Length int
		seed     int32
		seedFix  bool
		want     []byte
	}{
		{
			name:     "no-seed",
			data:     []byte("test data"),
			s2Length: 16,
			seed:     0,
			seedFix:  false,
			want:     mustHex("99ebf48d202177937f084a873437b85e"),
		},
		{
			name:     "seed-prepended",
			data:     []byte("test data"),
			s2Length: 16,
			seed:     12345,
			seedFix:  true,
			want:     mustHex("eb879514747ef330c92f3464a972bb9d"),
		},
		{
			name:     "seed-appended",
			data:     []byte("test data"),
			s2Length: 16,
			seed:     12345,
			seedFix:  false,
			want:     mustHex("29c4b399069c9fba3eb7613adbcf9a12"),
		},
		{
			name:     "truncated-s2length",
			data:     []byte("test data"),
			s2Length: 8,
			seed:     0,
			seedFix:  false,
			want:     mustHex("99ebf48d20217793"),
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := Checksum2(tc.data, "md4", tc.s2Length, tc.seed, tc.seedFix)
			if !bytes.Equal(got, tc.want) {
				t.Errorf("Checksum2(md4, %s) = %x, want %x", tc.name, got, tc.want)
			}
		})
	}
}

func TestChecksum2_MD5(t *testing.T) {
	tests := []struct {
		name     string
		data     []byte
		s2Length int
		seed     int32
		seedFix  bool
		want     []byte
	}{
		{
			name:     "no-seed",
			data:     []byte("test data"),
			s2Length: 16,
			seed:     0,
			seedFix:  false,
			want:     mustHex("eb733a00c0c9d336e65691a37ab54293"),
		},
		{
			name:     "seed-prepended",
			data:     []byte("test data"),
			s2Length: 16,
			seed:     12345,
			seedFix:  true,
			want:     mustHex("5808913b27c272e48d9e88bc2af3b8d1"),
		},
		{
			name:     "seed-appended",
			data:     []byte("test data"),
			s2Length: 16,
			seed:     12345,
			seedFix:  false,
			want:     mustHex("20772956c594bb00d69e8200fd7c2756"),
		},
		{
			name:     "truncated-s2length",
			data:     []byte("test data"),
			s2Length: 8,
			seed:     0,
			seedFix:  false,
			want:     mustHex("eb733a00c0c9d336"),
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := Checksum2(tc.data, "md5", tc.s2Length, tc.seed, tc.seedFix)
			if !bytes.Equal(got, tc.want) {
				t.Errorf("Checksum2(md5, %s) = %x, want %x", tc.name, got, tc.want)
			}
		})
	}
}

func TestChecksum2_UnsupportedDigest(t *testing.T) {
	got := Checksum2([]byte("test"), "sha256", 32, 0, false)
	if got != nil {
		t.Errorf("Checksum2(sha256) = %v, want nil", got)
	}
}

func TestSumHead_Protocol27(t *testing.T) {
	sh := SumHead{
		Count:     5,
		BLength:   1000,
		S2Length:  16,
		Remainder: 350,
	}

	var buf bytes.Buffer
	if err := WriteSumHead(&buf, sh, 27); err != nil {
		t.Fatalf("WriteSumHead(27) failed: %v", err)
	}

	// proto >= 27: 16 bytes (4 int32s)
	if got := buf.Len(); got != 16 {
		t.Fatalf("WriteSumHead(27) wrote %d bytes, want 16", got)
	}

	got, err := ReadSumHead(&buf, 27)
	if err != nil {
		t.Fatalf("ReadSumHead(27) failed: %v", err)
	}
	if got != sh {
		t.Errorf("Roundtrip SumHead(27) = %+v, want %+v", got, sh)
	}
}

func TestSumHead_Protocol26(t *testing.T) {
	sh := SumHead{
		Count:     5,
		BLength:   1000,
		S2Length:  16,
		Remainder: 350,
	}

	var buf bytes.Buffer
	if err := WriteSumHead(&buf, sh, 26); err != nil {
		t.Fatalf("WriteSumHead(26) failed: %v", err)
	}

	// proto < 27: 12 bytes (3 int32s, no s2length)
	if got := buf.Len(); got != 12 {
		t.Fatalf("WriteSumHead(26) wrote %d bytes, want 12", got)
	}

	got, err := ReadSumHead(&buf, 26)
	if err != nil {
		t.Fatalf("ReadSumHead(26) failed: %v", err)
	}
	// s2length is not sent for proto < 27, so it should be 0
	want := SumHead{Count: 5, BLength: 1000, S2Length: 0, Remainder: 350}
	if got != want {
		t.Errorf("Roundtrip SumHead(26) = %+v, want %+v", got, want)
	}
}

func TestSumHead_EmptyFile(t *testing.T) {
	// Empty file: count=0, no data sent
	sh := SumHead{Count: 0}

	var buf bytes.Buffer
	if err := WriteSumHead(&buf, sh, 32); err != nil {
		t.Fatalf("WriteSumHead(empty) failed: %v", err)
	}

	got, err := ReadSumHead(&buf, 32)
	if err != nil {
		t.Fatalf("ReadSumHead(empty) failed: %v", err)
	}
	if got.Count != 0 {
		t.Errorf("Empty file SumHead count = %d, want 0", got.Count)
	}
}

func TestSumHead_PipeRoundTrip(t *testing.T) {
	sh := SumHead{
		Count:     42,
		BLength:   700,
		S2Length:  16,
		Remainder: 123,
	}

	pr, pw := io.Pipe()
	go func() {
		_ = WriteSumHead(pw, sh, 32)
		pw.Close()
	}()

	got, err := ReadSumHead(pr, 32)
	if err != nil {
		t.Fatalf("ReadSumHead(pipeline) failed: %v", err)
	}
	if got != sh {
		t.Errorf("Pipe roundtrip SumHead = %+v, want %+v", got, sh)
	}
}

func TestChecksum2_ZeroSeed(t *testing.T) {
	// seed=0 should produce the same result as no seed at all.
	data := []byte("test data")

	md4NoSeed := Checksum2(data, "md4", 16, 0, false)
	md4ZeroSeed := Checksum2(data, "md4", 16, 0, true)
	if !bytes.Equal(md4NoSeed, md4ZeroSeed) {
		t.Errorf("md4 seed=0 mismatch: no-seed=%x, zero-seed=%x", md4NoSeed, md4ZeroSeed)
	}

	md5NoSeed := Checksum2(data, "md5", 16, 0, false)
	md5ZeroSeed := Checksum2(data, "md5", 16, 0, true)
	if !bytes.Equal(md5NoSeed, md5ZeroSeed) {
		t.Errorf("md5 seed=0 mismatch: no-seed=%x, zero-seed=%x", md5NoSeed, md5ZeroSeed)
	}
}

func TestChecksum2_SeedFixDifference(t *testing.T) {
	// seedFix=true and seedFix=false must produce different results when seed != 0.
	data := []byte("test data")

	md4Prepended := Checksum2(data, "md4", 16, 12345, true)
	md4Appended := Checksum2(data, "md4", 16, 12345, false)
	if bytes.Equal(md4Prepended, md4Appended) {
		t.Error("md4 seedFix=true and seedFix=false produced identical results")
	}

	md5Prepended := Checksum2(data, "md5", 16, 12345, true)
	md5Appended := Checksum2(data, "md5", 16, 12345, false)
	if bytes.Equal(md5Prepended, md5Appended) {
		t.Error("md5 seedFix=true and seedFix=false produced identical results")
	}
}

func mustHex(s string) []byte {
	b := make([]byte, hexDecodedLen(s))
	for i := 0; i < len(s); i += 2 {
		b[i/2] = byte(hexVal(s[i])*16 + hexVal(s[i+1]))
	}
	return b
}

func hexVal(c byte) byte {
	if c >= '0' && c <= '9' {
		return c - '0'
	}
	if c >= 'a' && c <= 'f' {
		return c - 'a' + 10
	}
	if c >= 'A' && c <= 'F' {
		return c - 'A' + 10
	}
	return 0
}

func hexDecodedLen(s string) int {
	return len(s) / 2
}
