package mux_test

import (
	"bytes"
	"encoding/binary"
	"io"
	"testing"

	"github.com/values-conflict/go-rsyncfs/protocol/mux"
)

func TestRoundTrip(t *testing.T) {
	tests := []struct {
		name    string
		code    uint8
		payload []byte
	}{
		{"data-empty", mux.MsgData, nil},
		{"data-hello", mux.MsgData, []byte("hello world")},
		{"error-xfer", mux.MsgErrorXfer, []byte("transfer failed")},
		{"info", mux.MsgInfo, []byte("some info message")},
		{"success-zero-ndx", mux.MsgSuccess, makeLE32(0)},
		{"redo", mux.MsgRedo, makeLE32(42)},
		{"noop-empty", mux.MsgNoop, nil},
		{"deleted", mux.MsgDeleted, []byte("/path/to/file")},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pr, pw := io.Pipe()
			defer pr.Close()
			defer pw.Close()

			w := mux.NewWriter(pw)
			var errCh chan error = make(chan error, 1)
			go func() {
				errCh <- w.WriteMsg(tt.code, tt.payload)
			}()

			r := mux.NewReader(pr)
			gotCode, gotPayload, err := r.ReadMsg()
			if err != nil {
				t.Fatalf("ReadMsg: %v", err)
			}
			writeErr := <-errCh
			if writeErr != nil {
				t.Fatalf("WriteMsg: %v", writeErr)
			}

			if gotCode != tt.code {
				t.Errorf("code = %d, want %d", gotCode, tt.code)
			}
			if !bytes.Equal(gotPayload, tt.payload) {
				t.Errorf("payload = %q, want %q", gotPayload, tt.payload)
			}
		})
	}
}

func TestHeaderEncoding(t *testing.T) {
	var buf bytes.Buffer
	w := mux.NewWriter(&buf)
	if err := w.WriteMsg(mux.MsgData, []byte("test")); err != nil {
		t.Fatal(err)
	}

	got := buf.Bytes()
	if len(got) != 8 { // 4-byte header + 4 bytes payload
		t.Fatalf("frame length = %d, want 8", len(got))
	}

	val := binary.LittleEndian.Uint32(got[:4])
	msgCode := val >> 24                 // should be MPLEX_BASE(7) + MSG_DATA(0) = 7
	payloadLen := uint32(val & 0xFFFFFF) // should be 4

	if msgCode != 7 {
		t.Errorf("header high byte = %d, want 7 (MPLEX_BASE + MsgData)", msgCode)
	}
	if payloadLen != 4 {
		t.Errorf("payload length in header = %d, want 4", payloadLen)
	}

	gotPayload := got[4:]
	if string(gotPayload) != "test" {
		t.Errorf("payload = %q, want \"test\"", gotPayload)
	}
}

func TestZeroLengthPayload(t *testing.T) {
	var buf bytes.Buffer
	w := mux.NewWriter(&buf)
	if err := w.WriteMsg(mux.MsgNoop, nil); err != nil {
		t.Fatal(err)
	}

	got := buf.Bytes()
	if len(got) != 4 { // header only, no payload
		t.Fatalf("frame length = %d, want 4", len(got))
	}

	val := binary.LittleEndian.Uint32(got[:4])
	msgCode := val >> 24 // MPLEX_BASE(7) + MSG_NOOP(42) = 49
	payloadLen := uint32(val & 0xFFFFFF)

	if msgCode != 49 {
		t.Errorf("header high byte = %d, want 49 (MPLEX_BASE + MsgNoop)", msgCode)
	}
	if payloadLen != 0 {
		t.Errorf("payload length in header = %d, want 0", payloadLen)
	}

	r := mux.NewReader(bytes.NewReader(got))
	code, payload, err := r.ReadMsg()
	if err != nil {
		t.Fatal(err)
	}
	if code != mux.MsgNoop {
		t.Errorf("code = %d, want %d", code, mux.MsgNoop)
	}
	if len(payload) != 0 {
		t.Errorf("payload length = %d, want 0", len(payload))
	}
}

func TestTruncatedHeader(t *testing.T) {
	r := mux.NewReader(bytes.NewReader([]byte{0x07})) // only 1 byte of header; need 4
	code, payload, err := r.ReadMsg()
	if err == nil {
		t.Fatal("expected error for truncated header")
	}
	if code != 0 || len(payload) > 0 {
		t.Errorf("unexpected return values: code=%d, payloadLen=%d", code, len(payload))
	}
}

func TestTruncatedPayload(t *testing.T) {
	// Write a frame header claiming 10 bytes of payload but only provide 3 actual bytes.
	header := make([]byte, 4)
	binary.LittleEndian.PutUint32(header, uint32(7)<<24|10) // MPLEX_BASE + MsgData = 7, len=10
	data := append(header, 'a', 'b', 'c')                   // only 3 of promised 10 bytes

	r := mux.NewReader(bytes.NewReader(data))
	code, payload, err := r.ReadMsg()
	if err == nil {
		t.Fatal("expected error for truncated payload")
	}
	if code != 0 || len(payload) > 0 {
		t.Errorf("unexpected return values: code=%d, payloadLen=%d", code, len(payload))
	}
}

func TestPayloadTooLarge(t *testing.T) {
	var buf bytes.Buffer
	w := mux.NewWriter(&buf)
	big := make([]byte, 0xFFFFFF+1) // one byte over the limit
	err := w.WriteMsg(mux.MsgData, big)
	if err == nil {
		t.Fatal("expected error for oversized payload")
	}
}

func TestMultipleFrames(t *testing.T) {
	var buf bytes.Buffer
	w := mux.NewWriter(&buf)
	for i := uint8(0); i < 3; i++ {
		if err := w.WriteMsg(mux.MsgData, []byte{i}); err != nil {
			t.Fatal(err)
		}
	}

	r := mux.NewReader(&buf)
	for wantCode := uint8(0); wantCode < 3; wantCode++ {
		code, payload, err := r.ReadMsg()
		if err != nil {
			t.Fatalf("frame %d: ReadMsg: %v", wantCode, err)
		}
		if code != mux.MsgData {
			t.Errorf("frame %d: code = %d, want MsgData(%d)", wantCode, code, mux.MsgData)
		}
		if len(payload) != 1 || payload[0] != wantCode {
			t.Errorf("frame %d: payload = %v, want [%d]", wantCode, payload, wantCode)
		}
	}

	// EOF after all frames consumed.
	code, _, err := r.ReadMsg()
	if err != io.EOF {
		t.Fatalf("expected EOF after consuming all frames; got code=%d, err=%v", code, err)
	}
}

func makeLE32(v int32) []byte {
	b := make([]byte, 4)
	binary.LittleEndian.PutUint32(b, uint32(v))
	return b
}
