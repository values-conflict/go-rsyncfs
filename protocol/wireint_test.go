package protocol

import (
	"bytes"
	"testing"
)

func TestVarint(t *testing.T) {
	tests := []int32{0, 1, -1, 127, -128, 256, -256, 16384, -16384, 2097152, -2097152, 2147483647, -2147483648}
	for _, tc := range tests {
		var buf bytes.Buffer
		if err := WriteVarint(&buf, tc); err != nil {
			t.Fatalf("WriteVarint(%d) failed: %v", tc, err)
		}

		got, err := ReadVarint(&buf)
		if err != nil {
			t.Fatalf("ReadVarint for %d failed: %v", tc, err)
		}
		if got != tc {
			t.Errorf("Roundtrip varint(%d): got %d", tc, got)
		}
	}
}

func TestVarlong(t *testing.T) {
	tests := []struct {
		val      int64
		minBytes byte
	}{
		{0, 3},
		{1, 3},
		{-1, 3},
		{256, 3},
		{-256, 3},
		{1 << 20, 3},
		{- (1 << 20), 3},
		{1 << 40, 3},
		{- (1 << 40), 3},
		{9223372036854775807, 3},
		{-9223372036854775808, 3},
		{0, 4},
		{1 << 30, 4},
	}

	for _, tc := range tests {
		var buf bytes.Buffer
		if err := WriteVarlong(&buf, tc.val, tc.minBytes); err != nil {
			t.Fatalf("WriteVarlong(%d, %d) failed: %v", tc.val, tc.minBytes, err)
		}

		got, err := ReadVarlong(&buf, tc.minBytes)
		if err != nil {
			t.Fatalf("ReadVarlong for (%d, %d) failed: %v", tc.val, tc.minBytes, err)
		}
		if got != tc.val {
			t.Errorf("Roundtrip varlong(%d, min=%d): got %d", tc.val, tc.minBytes, got)
		}
	}
}

func TestLongInt(t *testing.T) {
	tests := []int64{0, 1, -1, 2147483647, -2147483648, 2147483648, -2147483649, 9223372036854775807, -9223372036854775808}
	for _, tc := range tests {
		var buf bytes.Buffer
		if err := WriteLongInt(&buf, tc); err != nil {
			t.Fatalf("WriteLongInt(%d) failed: %v", tc, err)
		}

		got, err := ReadLongInt(&buf)
		if err != nil {
			t.Fatalf("ReadLongInt for %d failed: %v", tc, err)
		}
		if got != tc {
			t.Errorf("Roundtrip longint(%d): got %d", tc, got)
		}
	}
}
