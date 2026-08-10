package mux

import (
	"bytes"
	"io"
	"testing"
)

func TestWriter_Batching(t *testing.T) {
	var buf bytes.Buffer
	w := NewWriter(&buf)

	// Multiple small writes
	w.Write([]byte("a"))
	w.Write([]byte("b"))
	w.Write([]byte("c"))

	// Flush -- should produce a SINGLE MSG_DATA frame
	if err := w.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}

	// Verify: 4-byte header + 3-byte payload
	if buf.Len() != 7 {
		t.Errorf("expected 7 bytes (4 header + 3 payload), got %d", buf.Len())
	}

	// Parse the frame
	r := NewReader(&buf)
	got := make([]byte, 3)
	if _, err := io.ReadFull(r, got); err != nil {
		t.Fatalf("Read: %v", err)
	}
	if string(got) != "abc" {
		t.Errorf("got %q, want %q", got, "abc")
	}
}

func TestWriter_SendMsgFlushesFirst(t *testing.T) {
	var buf bytes.Buffer
	w := NewWriter(&buf)

	// Write data (not flushed yet)
	w.Write([]byte("data"))

	// SendMsg should flush data first, then send the message
	if err := w.SendMsg(MsgSuccess, []byte{1, 2, 3, 4}); err != nil {
		t.Fatalf("SendMsg: %v", err)
	}

	// Should have two frames: MSG_DATA + MSG_SUCCESS
	r := NewReader(&buf)

	// Read the data
	data := make([]byte, 4)
	if _, err := io.ReadFull(r, data); err != nil {
		t.Fatalf("Read data: %v", err)
	}
	if string(data) != "data" {
		t.Errorf("data: got %q, want %q", data, "data")
	}

	// Read the message
	code, payload, err := r.RecvMsg()
	if err != nil {
		t.Fatalf("RecvMsg: %v", err)
	}
	if code != MsgSuccess {
		t.Errorf("code: got %d, want %d", code, MsgSuccess)
	}
	if !bytes.Equal(payload, []byte{1, 2, 3, 4}) {
		t.Errorf("payload: got %v, want %v", payload, []byte{1, 2, 3, 4})
	}
}

func TestReader_SpansMultipleFrames(t *testing.T) {
	var buf bytes.Buffer
	w := NewWriter(&buf)

	// Write and flush twice (two separate MSG_DATA frames)
	w.Write([]byte("hello"))
	w.Flush()
	w.Write([]byte(" world"))
	w.Flush()

	// Read should span both frames transparently
	r := NewReader(&buf)
	got := make([]byte, 11)
	if _, err := io.ReadFull(r, got); err != nil {
		t.Fatalf("Read: %v", err)
	}
	if string(got) != "hello world" {
		t.Errorf("got %q, want %q", got, "hello world")
	}
}

func TestReader_RecvMsgSkipsData(t *testing.T) {
	var buf bytes.Buffer
	w := NewWriter(&buf)

	// Write data and a message
	w.Write([]byte("skip this"))
	w.Flush()
	w.SendMsg(MsgSuccess, []byte{42})

	r := NewReader(&buf)

	// RecvMsg should skip the data and return the message
	code, payload, err := r.RecvMsg()
	if err != nil {
		t.Fatalf("RecvMsg: %v", err)
	}
	if code != MsgSuccess {
		t.Errorf("code: got %d, want %d", code, MsgSuccess)
	}
	if len(payload) != 1 || payload[0] != 42 {
		t.Errorf("payload: got %v, want [42]", payload)
	}
}

func TestEmptyFlush(t *testing.T) {
	var buf bytes.Buffer
	w := NewWriter(&buf)

	// Flush with empty buffer should be a no-op
	if err := w.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	if buf.Len() != 0 {
		t.Errorf("expected 0 bytes after empty Flush, got %d", buf.Len())
	}
}

func TestLargeWrite(t *testing.T) {
	// Write more than max frame size (0xFFFFFF)
	var buf bytes.Buffer
	w := NewWriter(&buf)

	data := make([]byte, 0x1000000) // 16MB +
	for i := range data {
		data[i] = byte(i % 256)
	}

	w.Write(data)
	if err := w.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}

	// Should be split into multiple frames
	r := NewReader(&buf)
	got, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if !bytes.Equal(got, data) {
		t.Errorf("large write round-trip failed: got %d bytes, want %d", len(got), len(data))
	}
}

func TestBatchedSelectorAndSumHead(t *testing.T) {
	// Simulate upstream batching: selector + sum_head in one MSG_DATA frame
	var buf bytes.Buffer
	w := NewWriter(&buf)

	selector := []byte{0x01, 0x00, 0x80} // ndx=1, ITEM_TRANSFER
	sumHead := make([]byte, 16)          // 4 int32s: count, blength, s2length, remainder

	w.Write(selector)
	w.Write(sumHead)
	w.Flush() // batched into one MSG_DATA frame

	// Reader should be able to read selector and sum_head independently
	r := NewReader(&buf)

	// Read selector
	sel := make([]byte, 3)
	if _, err := io.ReadFull(r, sel); err != nil {
		t.Fatalf("Read selector: %v", err)
	}
	if !bytes.Equal(sel, selector) {
		t.Errorf("selector: got %v, want %v", sel, selector)
	}

	// Read sum_head
	sh := make([]byte, 16)
	if _, err := io.ReadFull(r, sh); err != nil {
		t.Fatalf("Read sum_head: %v", err)
	}
	if !bytes.Equal(sh, sumHead) {
		t.Errorf("sum_head: got %v, want %v", sh, sumHead)
	}
}

func TestEmptyDataframe(t *testing.T) {
	// Empty MSG_DATA frame should be handled gracefully
	var buf bytes.Buffer
	w := NewWriter(&buf)

	// Flush with no data -- should be a no-op (no frame written)
	w.Flush()
	if buf.Len() != 0 {
		t.Errorf("expected 0 bytes for empty Flush, got %d", buf.Len())
	}

	// Write empty then flush -- still no frame
	w.Write([]byte{})
	w.Flush()
	if buf.Len() != 0 {
		t.Errorf("expected 0 bytes for empty Write+Flush, got %d", buf.Len())
	}
}
