package protocol

import (
	"bytes"
	"io"
	"math"
	"testing"
)

func TestInt32(t *testing.T) {
	tests := []struct {
		name string
		val  int32
	}{
		{"zero", 0},
		{"one", 1},
		{"negative-one", -1},
		{"max", math.MaxInt32},
		{"min", math.MinInt32},
		{"positive-mid", 12345678},
		{"negative-mid", -12345678},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			if err := WriteInt32(&buf, tc.val); err != nil {
				t.Fatalf("WriteInt32(%d) failed: %v", tc.val, err)
			}

			if got := buf.Len(); got != 4 {
				t.Fatalf("WriteInt32(%d) wrote %d bytes, want 4", tc.val, got)
			}

			got, err := ReadInt32(&buf)
			if err != nil {
				t.Fatalf("ReadInt32 for %d failed: %v", tc.val, err)
			}
			if got != tc.val {
				t.Errorf("Roundtrip Int32(%d): got %d", tc.val, got)
			}
		})
	}
}

func TestUint16(t *testing.T) {
	tests := []struct {
		name string
		val  uint16
	}{
		{"zero", 0},
		{"one", 1},
		{"max", math.MaxUint16},
		{"mid", 0x1234},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			if err := WriteUint16(&buf, tc.val); err != nil {
				t.Fatalf("WriteUint16(%d) failed: %v", tc.val, err)
			}

			if got := buf.Len(); got != 2 {
				t.Fatalf("WriteUint16(%d) wrote %d bytes, want 2", tc.val, got)
			}

			got, err := ReadUint16(&buf)
			if err != nil {
				t.Fatalf("ReadUint16 for %d failed: %v", tc.val, err)
			}
			if got != tc.val {
				t.Errorf("Roundtrip Uint16(%d): got %d", tc.val, got)
			}
		})
	}
}

func TestVarint(t *testing.T) {
	tests := []struct {
		name string
		val  int32
	}{
		{"zero", 0},
		{"one", 1},
		{"negative-one", -1},
		{"127", 127},
		{"negative-128", -128},
		{"256", 256},
		{"negative-256", -256},
		{"16384", 16384},
		{"negative-16384", -16384},
		{"2097152", 2097152},
		{"negative-2097152", -2097152},
		{"max-int32", math.MaxInt32},
		{"min-int32", math.MinInt32},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			if err := WriteVarint(&buf, tc.val); err != nil {
				t.Fatalf("WriteVarint(%d) failed: %v", tc.val, err)
			}

			got, err := ReadVarint(&buf)
			if err != nil {
				t.Fatalf("ReadVarint for %d failed: %v", tc.val, err)
			}
			if got != tc.val {
				t.Errorf("Roundtrip Varint(%d): got %d", tc.val, got)
			}
		})
	}
}

func TestVarint_WireFormat(t *testing.T) {
	// Verify specific byte sequences against upstream encoding rules.
	tests := []struct {
		name string
		val  int32
		want []byte
	}{
		{"zero", 0, []byte{0}},
		{"one", 1, []byte{1}},
		{"127", 127, []byte{127}},
		// 128 needs 2 bytes: first byte 0x80 indicates 1 extra byte, data = 0x80
		{"128", 128, []byte{0x80, 0x80}},
		// -1 encodes as 5 bytes (full range needed for negative)
		{"negative-one", -1, []byte{0xF0, 0xFF, 0xFF, 0xFF, 0xFF}},
		// max-int32: 0x7FFFFFFF needs 5 bytes
		{"max-int32", math.MaxInt32, []byte{0xF0, 0xFF, 0xFF, 0xFF, 0x7F}},
		// min-int32: 0x80000000 needs 5 bytes
		{"min-int32", math.MinInt32, []byte{0xF0, 0x00, 0x00, 0x00, 0x80}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			if err := WriteVarint(&buf, tc.val); err != nil {
				t.Fatalf("WriteVarint(%d) failed: %v", tc.val, err)
			}
			got := buf.Bytes()
			if !bytes.Equal(got, tc.want) {
				t.Errorf("WriteVarint(%d) = %v, want %v", tc.val, got, tc.want)
			}
		})
	}
}

func TestVarlong(t *testing.T) {
	tests := []struct {
		name     string
		val      int64
		minBytes byte
	}{
		{"zero-min3", 0, 3},
		{"one-min3", 1, 3},
		{"negative-one-min3", -1, 3},
		{"256-min3", 256, 3},
		{"negative-256-min3", -256, 3},
		{"1M-min3", 1 << 20, 3},
		{"negative-1M-min3", -(1 << 20), 3},
		{"1T-min3", 1 << 40, 3},
		{"negative-1T-min3", -(1 << 40), 3},
		{"max-int64-min3", math.MaxInt64, 3},
		{"min-int64-min3", math.MinInt64, 3},
		{"zero-min4", 0, 4},
		{"1G-min4", 1 << 30, 4},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			if err := WriteVarlong(&buf, tc.val, tc.minBytes); err != nil {
				t.Fatalf("WriteVarlong(%d, %d) failed: %v", tc.val, tc.minBytes, err)
			}

			got, err := ReadVarlong(&buf, tc.minBytes)
			if err != nil {
				t.Fatalf("ReadVarlong for (%d, %d) failed: %v", tc.val, tc.minBytes, err)
			}
			if got != tc.val {
				t.Errorf("Roundtrip Varlong(%d, min=%d): got %d", tc.val, tc.minBytes, got)
			}
		})
	}
}

func TestLongInt(t *testing.T) {
	tests := []struct {
		name string
		val  int64
	}{
		{"zero", 0},
		{"one", 1},
		{"negative-one", -1},
		{"max-int32", math.MaxInt32},
		{"min-int32", math.MinInt32},
		{"just-above-max-int32", math.MaxInt32 + 1},
		{"just-below-min-int32", math.MinInt32 - 1},
		{"max-int64", math.MaxInt64},
		{"min-int64", math.MinInt64},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			if err := WriteLongInt(&buf, tc.val); err != nil {
				t.Fatalf("WriteLongInt(%d) failed: %v", tc.val, err)
			}

			got, err := ReadLongInt(&buf)
			if err != nil {
				t.Fatalf("ReadLongInt for %d failed: %v", tc.val, err)
			}
			if got != tc.val {
				t.Errorf("Roundtrip LongInt(%d): got %d", tc.val, got)
			}
		})
	}
}

func TestLongInt_WireFormat(t *testing.T) {
	// Verify wire format: small values are 4 bytes, large values use sentinel.
	tests := []struct {
		name string
		val  int64
		want []byte
	}{
		{"zero", 0, []byte{0, 0, 0, 0}},
		{"one", 1, []byte{1, 0, 0, 0}},
		{"max-int32", math.MaxInt32, []byte{0xFF, 0xFF, 0xFF, 0x7F}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			if err := WriteLongInt(&buf, tc.val); err != nil {
				t.Fatalf("WriteLongInt(%d) failed: %v", tc.val, err)
			}
			got := buf.Bytes()
			if !bytes.Equal(got, tc.want) {
				t.Errorf("WriteLongInt(%d) = %v, want %v", tc.val, got, tc.want)
			}
		})
	}

	// Negative values use sentinel (0xFFFFFFFF + 8 bytes LE).
	t.Run("negative-one-sentinel", func(t *testing.T) {
		var buf bytes.Buffer
		if err := WriteLongInt(&buf, -1); err != nil {
			t.Fatalf("WriteLongInt(-1) failed: %v", err)
		}
		got := buf.Bytes()
		// sentinel + int64(-1) as LE
		want := []byte{0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF}
		if !bytes.Equal(got, want) {
			t.Errorf("WriteLongInt(-1) = %v, want %v", got, want)
		}
	})

	// Value above int32 max uses sentinel.
	t.Run("above-max-int32-sentinel", func(t *testing.T) {
		var buf bytes.Buffer
		val := int64(math.MaxInt32 + 1)
		if err := WriteLongInt(&buf, val); err != nil {
			t.Fatalf("WriteLongInt(%d) failed: %v", val, err)
		}
		got := buf.Bytes()
		if len(got) != 12 {
			t.Errorf("WriteLongInt(%d) wrote %d bytes, want 12 (sentinel path)", val, len(got))
		}
		if got[0] != 0xFF || got[1] != 0xFF || got[2] != 0xFF || got[3] != 0xFF {
			t.Errorf("WriteLongInt(%d) missing sentinel prefix: %v", val, got[:4])
		}
	})
}

func TestNdxState(t *testing.T) {
	tests := []struct {
		name  string
		ndx   int32
		first bool // whether this is the first write (no prior state)
	}{
		{"done", NDxDone, true},
		{"zero", 0, true},
		{"one", 1, true},
		{"flist-eof", NDXFlistEOF, true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			state := NewNdxState()
			var buf bytes.Buffer
			if err := state.WriteNdx(&buf, tc.ndx); err != nil {
				t.Fatalf("WriteNdx(%d) failed: %v", tc.ndx, err)
			}

			state2 := NewNdxState()
			got, err := state2.ReadNdx(&buf)
			if err != nil {
				t.Fatalf("ReadNdx for %d failed: %v", tc.ndx, err)
			}
			if got != tc.ndx {
				t.Errorf("Roundtrip Ndx(%d): got %d", tc.ndx, got)
			}
		})
	}
}

func TestNdxState_DoneByte(t *testing.T) {
	// NDX_DONE must be exactly one byte: 0x00
	state := NewNdxState()
	var buf bytes.Buffer
	if err := state.WriteNdx(&buf, NDxDone); err != nil {
		t.Fatalf("WriteNdx(NDxDone) failed: %v", err)
	}
	got := buf.Bytes()
	if len(got) != 1 || got[0] != 0 {
		t.Errorf("WriteNdx(NDxDone) = %v, want [0x00]", got)
	}
}

func TestNdxState_Sequential(t *testing.T) {
	// Sequential indices should encode efficiently (1 byte each after the first).
	state := NewNdxState()
	var buf bytes.Buffer

	indices := []int32{0, 1, 2, 3, 4, 5, 10, 20, 50, 100}
	for _, idx := range indices {
		if err := state.WriteNdx(&buf, idx); err != nil {
			t.Fatalf("WriteNdx(%d) failed: %v", idx, err)
		}
	}

	state2 := NewNdxState()
	for _, want := range indices {
		got, err := state2.ReadNdx(&buf)
		if err != nil {
			t.Fatalf("ReadNdx for %d failed: %v", want, err)
		}
		if got != want {
			t.Errorf("ReadNdx = %d, want %d", got, want)
		}
	}

	// Verify efficiency: first index uses 2 bytes (0xFE prefix + diff),
	// subsequent sequential indices use 1 byte each.
	data := buf.Bytes()
	// 0 (first positive): 0xFE 0x00 0x00 (3 bytes, diff=1 from prevPositive=-1)
	// 1: diff=1, single byte 0x01
	// 2: diff=1, single byte 0x01
	// etc.
	if len(data) > len(indices)+2 {
		t.Errorf("Sequential NDX used %d bytes, expected ~%d (first=3, rest=1 each)", len(data), len(indices)+2)
	}
}

func TestNdxState_Negative(t *testing.T) {
	state := NewNdxState()
	var buf bytes.Buffer

	indices := []int32{NDXFlistEOF, -5, -3, -100}
	for _, idx := range indices {
		if err := state.WriteNdx(&buf, idx); err != nil {
			t.Fatalf("WriteNdx(%d) failed: %v", idx, err)
		}
	}

	state2 := NewNdxState()
	for _, want := range indices {
		got, err := state2.ReadNdx(&buf)
		if err != nil {
			t.Fatalf("ReadNdx for %d failed: %v", want, err)
		}
		if got != want {
			t.Errorf("ReadNdx = %d, want %d", got, want)
		}
	}
}

func TestNdxState_Mixed(t *testing.T) {
	state := NewNdxState()
	var buf bytes.Buffer

	indices := []int32{0, 5, -2, 10, NDXFlistEOF, 15, NDxDone}
	for _, idx := range indices {
		if err := state.WriteNdx(&buf, idx); err != nil {
			t.Fatalf("WriteNdx(%d) failed: %v", idx, err)
		}
	}

	state2 := NewNdxState()
	for _, want := range indices {
		got, err := state2.ReadNdx(&buf)
		if err != nil {
			t.Fatalf("ReadNdx for %d failed: %v", want, err)
		}
		if got != want {
			t.Errorf("ReadNdx = %d, want %d", got, want)
		}
	}
}

func TestNdxState_LargeValues(t *testing.T) {
	state := NewNdxState()
	var buf bytes.Buffer

	indices := []int32{100000, 200000, math.MaxInt32}
	for _, idx := range indices {
		if err := state.WriteNdx(&buf, idx); err != nil {
			t.Fatalf("WriteNdx(%d) failed: %v", idx, err)
		}
	}

	state2 := NewNdxState()
	for _, want := range indices {
		got, err := state2.ReadNdx(&buf)
		if err != nil {
			t.Fatalf("ReadNdx for %d failed: %v", want, err)
		}
		if got != want {
			t.Errorf("ReadNdx = %d, want %d", got, want)
		}
	}
}

func TestNdxState_PipeRoundTrip(t *testing.T) {
	// Full round-trip through io.Pipe to verify streaming behavior.
	pr, pw := io.Pipe()
	defer pr.Close()
	defer pw.Close()

	state := NewNdxState()
	go func() {
		indices := []int32{0, 1, 2, 100, -2, NDxDone}
		for _, idx := range indices {
			if err := state.WriteNdx(pw, idx); err != nil {
				pw.CloseWithError(err)
				return
			}
		}
		pw.Close()
	}()

	state2 := NewNdxState()
	indices := []int32{0, 1, 2, 100, -2, NDxDone}
	for _, want := range indices {
		got, err := state2.ReadNdx(pr)
		if err != nil {
			t.Fatalf("ReadNdx for %d failed: %v", want, err)
		}
		if got != want {
			t.Errorf("ReadNdx = %d, want %d", got, want)
		}
	}
}

func TestVarint_FullRange(t *testing.T) {
	// Test boundary values around encoding transitions.
	boundaries := []int32{
		-128, -127, -1, 0, 1, 127, 128,
		-256, -255, 255, 256,
		-16384, -16383, 16383, 16384,
		-2097152, -2097151, 2097151, 2097152,
		math.MinInt32, math.MaxInt32,
	}

	for _, val := range boundaries {
		t.Run("", func(t *testing.T) {
			var buf bytes.Buffer
			if err := WriteVarint(&buf, val); err != nil {
				t.Fatalf("WriteVarint(%d) failed: %v", val, err)
			}
			got, err := ReadVarint(&buf)
			if err != nil {
				t.Fatalf("ReadVarint for %d failed: %v", val, err)
			}
			if got != val {
				t.Errorf("Roundtrip Varint(%d): got %d", val, got)
			}
		})
	}
}

func TestVarlong_FullRange(t *testing.T) {
	boundaries := []int64{
		-128, -127, -1, 0, 1, 127, 128,
		-256, -255, 255, 256,
		-16384, -16383, 16383, 16384,
		math.MinInt64, math.MaxInt64,
	}

	for _, val := range boundaries {
		t.Run("", func(t *testing.T) {
			var buf bytes.Buffer
			if err := WriteVarlong(&buf, val, 3); err != nil {
				t.Fatalf("WriteVarlong(%d, 3) failed: %v", val, err)
			}
			got, err := ReadVarlong(&buf, 3)
			if err != nil {
				t.Fatalf("ReadVarlong for %d failed: %v", val, err)
			}
			if got != val {
				t.Errorf("Roundtrip Varlong(%d, min=3): got %d", val, got)
			}
		})
	}
}

func TestInt32_LE(t *testing.T) {
	// Verify WriteInt32 produces standard little-endian output.
	var buf bytes.Buffer
	if err := WriteInt32(&buf, 0x01020304); err != nil {
		t.Fatalf("WriteInt32 failed: %v", err)
	}
	got := buf.Bytes()
	want := []byte{0x04, 0x03, 0x02, 0x01}
	if !bytes.Equal(got, want) {
		t.Errorf("WriteInt32(0x01020304) = %v, want %v", got, want)
	}

	// Verify ReadInt32 matches binary.LittleEndian.
	data := []byte{0xAB, 0xCD, 0xEF, 0x01}
	gotVal, err := ReadInt32(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("ReadInt32 failed: %v", err)
	}
	if gotVal != 0x01EFCDAB {
		t.Errorf("ReadInt32 = 0x%08X, want 0x01EFCDAB", gotVal)
	}
}
