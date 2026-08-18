package protocol

import (
	"bytes"
	"io"
	"testing"
)

func TestVstring(t *testing.T) {
	tests := []struct {
		name string
		val  string
	}{
		{"empty", ""},
		{"one", "x"},
		{"hello", "hello"},
		{"127", string(make([]byte, 127))},
		{"128", string(make([]byte, 128))},
		{"255", string(make([]byte, 255))},
		{"256", string(make([]byte, 256))},
		{"32767", string(make([]byte, 32767))},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			if err := WriteVstring(&buf, tc.val); err != nil {
				t.Fatalf("WriteVstring(len=%d) failed: %v", len(tc.val), err)
			}

			got, err := ReadVstring(&buf)
			if err != nil {
				t.Fatalf("ReadVstring for len=%d failed: %v", len(tc.val), err)
			}
			if got != tc.val {
				t.Errorf("Roundtrip Vstring(len=%d): got len=%d", len(tc.val), len(got))
			}
		})
	}
}

func TestVstring_WireFormat(t *testing.T) {
	// Verify specific byte sequences against upstream encoding rules.
	tests := []struct {
		name string
		val  string
		want []byte
	}{
		// 1-byte length: len < 128
		{"empty", "", []byte{0}},
		{"one-byte", "A", []byte{1, 'A'}},
		{"three-bytes", "ABC", []byte{3, 'A', 'B', 'C'}},
		// boundary: 127 uses 1-byte length (actual 127-byte string)
		// verified in separate subtest below
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			if err := WriteVstring(&buf, tc.val); err != nil {
				t.Fatalf("WriteVstring(%q) failed: %v", tc.val, err)
			}
			got := buf.Bytes()
			if !bytes.Equal(got, tc.want) {
				t.Errorf("WriteVstring(%q) = %v, want %v", tc.val, got, tc.want)
			}
		})
	}

	// 2-byte length: len >= 128. Verify the length prefix bytes.
	t.Run("128-uses-two-byte-length", func(t *testing.T) {
		s := string(make([]byte, 128))
		var buf bytes.Buffer
		if err := WriteVstring(&buf, s); err != nil {
			t.Fatalf("WriteVstring(len=128) failed: %v", err)
		}
		got := buf.Bytes()
		// length 128 = 0x0080 -> first byte: (0 >> 8) | 0x80 = 0x80, second byte: 0x80
		if len(got) != 130 {
			t.Errorf("WriteVstring(len=128) wrote %d bytes, want 130", len(got))
		}
		if got[0] != 0x80 || got[1] != 0x80 {
			t.Errorf("WriteVstring(len=128) length prefix = %02x %02x, want 80 80", got[0], got[1])
		}
	})

	t.Run("127-uses-one-byte-length", func(t *testing.T) {
		s := string(make([]byte, 127))
		var buf bytes.Buffer
		if err := WriteVstring(&buf, s); err != nil {
			t.Fatalf("WriteVstring(len=127) failed: %v", err)
		}
		got := buf.Bytes()
		// length 127 fits in 1 byte
		if len(got) != 128 {
			t.Errorf("WriteVstring(len=127) wrote %d bytes, want 128", len(got))
		}
		if got[0] != 127 {
			t.Errorf("WriteVstring(len=127) length byte = %02x, want 7f", got[0])
		}
	})

	t.Run("256-uses-two-byte-length", func(t *testing.T) {
		s := string(make([]byte, 256))
		var buf bytes.Buffer
		if err := WriteVstring(&buf, s); err != nil {
			t.Fatalf("WriteVstring(len=256) failed: %v", err)
		}
		got := buf.Bytes()
		// length 256 = 0x0100 -> first byte: (256 >> 8) | 0x80 = 0x81, second byte: 0x00
		if got[0] != 0x81 || got[1] != 0x00 {
			t.Errorf("WriteVstring(len=256) length prefix = %02x %02x, want 81 00", got[0], got[1])
		}
	})
}

func TestVstring_PipeRoundTrip(t *testing.T) {
	pr, pw := io.Pipe()
	defer pr.Close()
	defer pw.Close()

	go func() {
		strings := []string{"hello", "", "world", "test data with more bytes"}
		for _, s := range strings {
			if err := WriteVstring(pw, s); err != nil {
				pw.CloseWithError(err)
				return
			}
		}
		pw.Close()
	}()

	want := []string{"hello", "", "world", "test data with more bytes"}
	for _, expected := range want {
		got, err := ReadVstring(pr)
		if err != nil {
			t.Fatalf("ReadVstring for %q failed: %v", expected, err)
		}
		if got != expected {
			t.Errorf("ReadVstring = %q, want %q", got, expected)
		}
	}
}

func TestVstring_TooLong(t *testing.T) {
	// Strings longer than 32767 bytes must fail to write.
	s := string(make([]byte, 32768))
	err := WriteVstring(io.Discard, s)
	if err != io.ErrShortWrite {
		t.Errorf("WriteVstring(len=32768) = %v, want io.ErrShortWrite", err)
	}
}
