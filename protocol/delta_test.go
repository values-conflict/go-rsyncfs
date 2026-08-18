package protocol

import (
	"bytes"
	"io"
	"net"
	"testing"
)

func TestDeltaWriter_ReadToken(t *testing.T) {
	t.Run("empty stream", func(t *testing.T) {
		var buf bytes.Buffer
		dw := NewDeltaWriter(&buf)
		if err := dw.WriteEnd(); err != nil {
			t.Fatal(err)
		}

		dr := NewDeltaReader(&buf)
		data, blockIdx, isEnd, err := dr.ReadToken()
		if err != nil {
			t.Fatal(err)
		}
		if !isEnd {
			t.Error("expected end of stream")
		}
		if data != nil {
			t.Error("expected nil data at end")
		}
		if blockIdx != -1 {
			t.Errorf("expected blockIdx -1, got %d", blockIdx)
		}
	})

	t.Run("literal only", func(t *testing.T) {
		var buf bytes.Buffer
		dw := NewDeltaWriter(&buf)
		if err := dw.WriteLiteral([]byte("hello")); err != nil {
			t.Fatal(err)
		}
		if err := dw.WriteEnd(); err != nil {
			t.Fatal(err)
		}

		dr := NewDeltaReader(&buf)
		data, _, isEnd, err := dr.ReadToken()
		if err != nil {
			t.Fatal(err)
		}
		if isEnd {
			t.Fatal("unexpected end of stream")
		}
		if !bytes.Equal(data, []byte("hello")) {
			t.Errorf("got %q, want %q", data, "hello")
		}

		// End marker.
		_, _, isEnd, err = dr.ReadToken()
		if err != nil || !isEnd {
			t.Fatalf("expected end, got err=%v, isEnd=%v", err, isEnd)
		}
	})

	t.Run("match only", func(t *testing.T) {
		var buf bytes.Buffer
		dw := NewDeltaWriter(&buf)
		if err := dw.WriteMatch(5); err != nil {
			t.Fatal(err)
		}
		if err := dw.WriteEnd(); err != nil {
			t.Fatal(err)
		}

		dr := NewDeltaReader(&buf)
		data, blockIdx, isEnd, err := dr.ReadToken()
		if err != nil {
			t.Fatal(err)
		}
		if isEnd {
			t.Fatal("unexpected end of stream")
		}
		if data != nil {
			t.Error("expected nil data for match")
		}
		if blockIdx != 5 {
			t.Errorf("expected blockIdx 5, got %d", blockIdx)
		}
	})

	t.Run("mixed literal and match", func(t *testing.T) {
		var buf bytes.Buffer
		dw := NewDeltaWriter(&buf)
		if err := dw.WriteLiteral([]byte("hello")); err != nil {
			t.Fatal(err)
		}
		if err := dw.WriteMatch(0); err != nil {
			t.Fatal(err)
		}
		if err := dw.WriteMatch(1); err != nil {
			t.Fatal(err)
		}
		if err := dw.WriteLiteral([]byte("world")); err != nil {
			t.Fatal(err)
		}
		if err := dw.WriteEnd(); err != nil {
			t.Fatal(err)
		}

		dr := NewDeltaReader(&buf)

		// Read literal "hello".
		data, _, isEnd, err := dr.ReadToken()
		if err != nil || isEnd || !bytes.Equal(data, []byte("hello")) {
			t.Fatalf("literal: err=%v, isEnd=%v, data=%q", err, isEnd, data)
		}

		// Read match 0.
		data, blockIdx, isEnd, err := dr.ReadToken()
		if err != nil || isEnd || data != nil || blockIdx != 0 {
			t.Fatalf("match 0: err=%v, isEnd=%v, data=%v, blockIdx=%d", err, isEnd, data, blockIdx)
		}

		// Read match 1.
		data, blockIdx, isEnd, err = dr.ReadToken()
		if err != nil || isEnd || data != nil || blockIdx != 1 {
			t.Fatalf("match 1: err=%v, isEnd=%v, data=%v, blockIdx=%d", err, isEnd, data, blockIdx)
		}

		// Read literal "world".
		data, _, isEnd, err = dr.ReadToken()
		if err != nil || isEnd || !bytes.Equal(data, []byte("world")) {
			t.Fatalf("literal: err=%v, isEnd=%v, data=%q", err, isEnd, data)
		}

		// Read end.
		_, _, isEnd, err = dr.ReadToken()
		if err != nil || !isEnd {
			t.Fatalf("end: err=%v, isEnd=%v", err, isEnd)
		}
	})

	t.Run("match block 0", func(t *testing.T) {
		// Verify wire format: match(0) = int32(-(0+1)) = int32(-1) = 0xFFFFFFFF
		var buf bytes.Buffer
		dw := NewDeltaWriter(&buf)
		if err := dw.WriteMatch(0); err != nil {
			t.Fatal(err)
		}
		if err := dw.WriteEnd(); err != nil {
			t.Fatal(err)
		}

		wire := buf.Bytes()
		if len(wire) != 8 {
			t.Fatalf("expected 8 bytes, got %d", len(wire))
		}
		// match(0) = -1 = 0xFFFFFFFF LE
		if wire[0] != 0xFF || wire[1] != 0xFF || wire[2] != 0xFF || wire[3] != 0xFF {
			t.Errorf("match(0) wire = %02x, want FF FF FF FF", wire[:4])
		}
		// end = 0 = 0x00000000 LE
		if wire[4] != 0 || wire[5] != 0 || wire[6] != 0 || wire[7] != 0 {
			t.Errorf("end wire = %02x, want 00 00 00 00", wire[4:8])
		}
	})

	t.Run("match block 255", func(t *testing.T) {
		// match(255) = -(255+1) = -256 = 0xFFFFFF00 LE
		var buf bytes.Buffer
		dw := NewDeltaWriter(&buf)
		if err := dw.WriteMatch(255); err != nil {
			t.Fatal(err)
		}
		if err := dw.WriteEnd(); err != nil {
			t.Fatal(err)
		}

		wire := buf.Bytes()
		// -256 in LE = 00 FF FF FF
		if wire[0] != 0x00 || wire[1] != 0xFF || wire[2] != 0xFF || wire[3] != 0xFF {
			t.Errorf("match(255) wire = %02x, want 00 FF FF FF", wire[:4])
		}
	})
}

func TestDeltaWriter_LargeLiteral(t *testing.T) {
	// Literal larger than deltaChunkSize should be split into multiple tokens.
	data := make([]byte, deltaChunkSize+1000)
	for i := range data {
		data[i] = byte(i % 256)
	}

	var buf bytes.Buffer
	dw := NewDeltaWriter(&buf)
	if err := dw.WriteLiteral(data); err != nil {
		t.Fatal(err)
	}
	if err := dw.WriteEnd(); err != nil {
		t.Fatal(err)
	}

	dr := NewDeltaReader(&buf)
	var result []byte
	for {
		chunk, _, isEnd, err := dr.ReadToken()
		if err != nil {
			t.Fatal(err)
		}
		if isEnd {
			break
		}
		result = append(result, chunk...)
	}

	if !bytes.Equal(result, data) {
		t.Errorf("got %d bytes, want %d", len(result), len(data))
	}
}

func TestDeltaWriter_LiteralExactlyChunkSize(t *testing.T) {
	// Literal exactly at deltaChunkSize should produce a single token.
	data := make([]byte, deltaChunkSize)
	for i := range data {
		data[i] = byte(i % 256)
	}

	var buf bytes.Buffer
	dw := NewDeltaWriter(&buf)
	if err := dw.WriteLiteral(data); err != nil {
		t.Fatal(err)
	}
	if err := dw.WriteEnd(); err != nil {
		t.Fatal(err)
	}

	dr := NewDeltaReader(&buf)
	chunk, _, isEnd, err := dr.ReadToken()
	if err != nil {
		t.Fatal(err)
	}
	if isEnd {
		t.Fatal("unexpected end of stream")
	}
	if !bytes.Equal(chunk, data) {
		t.Errorf("got %d bytes, want %d", len(chunk), len(data))
	}

	// Should be followed by end.
	_, _, isEnd, err = dr.ReadToken()
	if err != nil || !isEnd {
		t.Fatalf("expected end, got err=%v, isEnd=%v", err, isEnd)
	}
}

func TestDeltaWriter_EmptyLiteral(t *testing.T) {
	var buf bytes.Buffer
	dw := NewDeltaWriter(&buf)
	if err := dw.WriteLiteral(nil); err != nil {
		t.Fatal(err)
	}
	if err := dw.WriteEnd(); err != nil {
		t.Fatal(err)
	}

	// Should just have the end marker.
	wire := buf.Bytes()
	if len(wire) != 4 {
		t.Errorf("expected 4 bytes (end marker only), got %d", len(wire))
	}
}

func TestParseDeltaStream(t *testing.T) {
	t.Run("empty stream", func(t *testing.T) {
		var buf bytes.Buffer
		dw := NewDeltaWriter(&buf)
		if err := dw.WriteEnd(); err != nil {
			t.Fatal(err)
		}

		tokens, err := ParseDeltaStream(&buf)
		if err != nil {
			t.Fatal(err)
		}
		if len(tokens) != 0 {
			t.Errorf("expected 0 tokens, got %d", len(tokens))
		}
	})

	t.Run("mixed tokens", func(t *testing.T) {
		tokens := []DeltaToken{
			{Literal: []byte("hello")},
			{BlockIdx: 0},
			{BlockIdx: 1},
			{Literal: []byte("world")},
			{BlockIdx: 2},
		}

		var buf bytes.Buffer
		if err := WriteDeltaStream(&buf, tokens); err != nil {
			t.Fatal(err)
		}

		got, err := ParseDeltaStream(&buf)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != len(tokens) {
			t.Fatalf("got %d tokens, want %d", len(got), len(tokens))
		}
		for i := range tokens {
			if tokens[i].Literal != nil {
				if !bytes.Equal(got[i].Literal, tokens[i].Literal) {
					t.Errorf("token %d: got %q, want %q", i, got[i].Literal, tokens[i].Literal)
				}
			} else {
				if got[i].BlockIdx != tokens[i].BlockIdx {
					t.Errorf("token %d: got block %d, want %d", i, got[i].BlockIdx, tokens[i].BlockIdx)
				}
			}
		}
	})
}

func TestWriteDeltaStream(t *testing.T) {
	tokens := []DeltaToken{
		{Literal: []byte("abc")},
		{BlockIdx: 10},
	}

	var buf bytes.Buffer
	if err := WriteDeltaStream(&buf, tokens); err != nil {
		t.Fatal(err)
	}

	// Verify wire format manually.
	wire := buf.Bytes()
	// Token 1: literal "abc" -> int32(3) + "abc"
	if len(wire) < 7 {
		t.Fatalf("wire too short: %d", len(wire))
	}
	if wire[0] != 3 || wire[1] != 0 || wire[2] != 0 || wire[3] != 0 {
		t.Errorf("literal len wire = %02x, want 03 00 00 00", wire[:4])
	}
	if string(wire[4:7]) != "abc" {
		t.Errorf("literal data = %q, want %q", wire[4:7], "abc")
	}

	// Token 2: match(10) = -(10+1) = -11 = 0xFFFFFFF5 LE
	if wire[7] != 0xF5 || wire[8] != 0xFF || wire[9] != 0xFF || wire[10] != 0xFF {
		t.Errorf("match(10) wire = %02x, want F5 FF FF FF", wire[7:11])
	}

	// Token 3: end = 0x00000000 LE
	if wire[11] != 0 || wire[12] != 0 || wire[13] != 0 || wire[14] != 0 {
		t.Errorf("end wire = %02x, want 00 00 00 00", wire[11:15])
	}
}

func TestDeltaStream_FullRoundTrip(t *testing.T) {
	// Test through net.Pipe to verify no buffering issues.
	serverConn, clientConn := net.Pipe()

	go func() {
		defer serverConn.Close()
		dw := NewDeltaWriter(serverConn)
		if err := dw.WriteLiteral([]byte("prefix")); err != nil {
			return
		}
		if err := dw.WriteMatch(0); err != nil {
			return
		}
		if err := dw.WriteMatch(1); err != nil {
			return
		}
		if err := dw.WriteLiteral([]byte("suffix")); err != nil {
			return
		}
		if err := dw.WriteEnd(); err != nil {
			return
		}
	}()

	dr := NewDeltaReader(clientConn)

	// Read literal "prefix".
	data, _, isEnd, err := dr.ReadToken()
	if err != nil || isEnd || !bytes.Equal(data, []byte("prefix")) {
		t.Fatalf("literal: err=%v, isEnd=%v, data=%q", err, isEnd, data)
	}

	// Read match 0.
	data, blockIdx, isEnd, err := dr.ReadToken()
	if err != nil || isEnd || data != nil || blockIdx != 0 {
		t.Fatalf("match 0: err=%v, isEnd=%v, data=%v, blockIdx=%d", err, isEnd, data, blockIdx)
	}

	// Read match 1.
	data, blockIdx, isEnd, err = dr.ReadToken()
	if err != nil || isEnd || data != nil || blockIdx != 1 {
		t.Fatalf("match 1: err=%v, isEnd=%v, data=%v, blockIdx=%d", err, isEnd, data, blockIdx)
	}

	// Read literal "suffix".
	data, _, isEnd, err = dr.ReadToken()
	if err != nil || isEnd || !bytes.Equal(data, []byte("suffix")) {
		t.Fatalf("literal: err=%v, isEnd=%v, data=%q", err, isEnd, data)
	}

	// Read end.
	_, _, isEnd, err = dr.ReadToken()
	if err != nil || !isEnd {
		t.Fatalf("end: err=%v, isEnd=%v", err, isEnd)
	}

	clientConn.Close()
}

func TestDeltaReader_Truncated(t *testing.T) {
	// Truncated literal: header says 10 bytes but only 5 provided.
	var buf bytes.Buffer
	buf.Write([]byte{10, 0, 0, 0})   // literal length = 10
	buf.Write([]byte{1, 2, 3, 4, 5}) // only 5 bytes

	dr := NewDeltaReader(&buf)
	_, _, _, err := dr.ReadToken()
	if err == nil {
		t.Fatal("expected error for truncated literal, got nil")
	}
}

func TestDeltaReader_TruncatedHeader(t *testing.T) {
	// Truncated token header.
	dr := NewDeltaReader(bytes.NewReader([]byte{1, 2, 3}))
	_, _, _, err := dr.ReadToken()
	if !bytes.Contains([]byte(err.Error()), []byte("unexpected EOF")) {
		t.Fatalf("expected EOF error, got: %v", err)
	}
}

func TestDeltaReader_EOF(t *testing.T) {
	dr := NewDeltaReader(bytes.NewReader(nil))
	_, _, _, err := dr.ReadToken()
	if err != io.EOF {
		t.Fatalf("expected io.EOF, got: %v", err)
	}
}

func TestDeltaWriter_MatchEdgeCases(t *testing.T) {
	tests := []struct {
		name     string
		blockIdx int32
		wantWire []byte // just the int32 portion
	}{
		{"block 0", 0, []byte{0xFF, 0xFF, 0xFF, 0xFF}},       // -1
		{"block 1", 1, []byte{0xFE, 0xFF, 0xFF, 0xFF}},       // -2
		{"block 255", 255, []byte{0x00, 0xFF, 0xFF, 0xFF}},   // -256
		{"block 1000", 1000, []byte{0x17, 0xFC, 0xFF, 0xFF}}, // -1001
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer
			dw := NewDeltaWriter(&buf)
			if err := dw.WriteMatch(tt.blockIdx); err != nil {
				t.Fatal(err)
			}
			got := buf.Bytes()
			if !bytes.Equal(got, tt.wantWire) {
				t.Errorf("match(%d) wire = %02x, want %02x", tt.blockIdx, got, tt.wantWire)
			}
		})
	}
}
