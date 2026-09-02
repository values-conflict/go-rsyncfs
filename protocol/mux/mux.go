// Package mux implements the rsync multiplexed I/O framing protocol.
//
// Every frame is a 4-byte little-endian header followed by a variable-length payload:
//
//	bits [31..24] = MPLEX_BASE + msgCode (MPLEX_BASE = 7)
//	bits [23..0]  = payload length (max ~16MB per frame)
//
// The Writer and Reader provide transparent buffered I/O that matches upstream's iobuf model.
// Normal protocol data (selectors, sum_heads, deltas, file data) flows through Write()/Read(), which automatically batches into MSG_DATA frames.
// Control messages (MSG_SUCCESS, MSG_ERROR, etc.) use SendMsg()/RecvMsg().
package mux

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

const (
	mplexBase = 7

	// defaultBufSize matches upstream's IO_BUFFER_SIZE (32KB).
	defaultBufSize = 32 * 1024
)

var ErrPayloadTooLarge = errors.New("payload exceeds maximum frame size") // max is 2^24-1 bytes (~16MB); the header only has room for a 24-bit length field

// Message codes matching upstream rsync enum msgcode.
const (
	MsgData        uint8 = 0   // raw data on the multiplexed stream
	MsgErrorXfer   uint8 = 1   // remote logging -- transfer error message
	MsgInfo        uint8 = 2   // remote logging -- informational log message
	MsgError       uint8 = 3   // protocol-30 remote logging -- error
	MsgWarning     uint8 = 4   // protocol-30 remote logging -- warning
	MsgErrorSocket uint8 = 5   // sibling logging -- socket error
	MsgLog         uint8 = 6   // sibling logging -- log-only message
	MsgClient      uint8 = 7   // never transmitted (server converts to MsgInfo)
	MsgErrorUTF8   uint8 = 8   // sibling logging -- UTF-8 conversion error
	MsgRedo        uint8 = 9   // reprocess indicated flist index; payload is int32 LE
	MsgStats       uint8 = 10  // message has stats data for generator; payload is int64 total_read
	MsgIOError     uint8 = 22  // the sending side had an I/O error; payload is int32 bitmask
	MsgIOTimeout   uint8 = 33  // tell client about a daemon's timeout value; payload is int32 seconds
	MsgNoop        uint8 = 42  // do-nothing message (legacy protocol-30 only); zero-length payload
	MsgErrorExit   uint8 = 86  // synchronize an error exit (siblings and protocol >= 31)
	MsgSuccess     uint8 = 100 // successfully updated indicated flist index; payload is int32 ndx
	MsgDeleted     uint8 = 101 // successfully deleted a file on receiving side; payload is filename text
	MsgNoSend      uint8 = 102 // sender failed to open a requested file; payload is int32 ndx
)

// Writer wraps an [io.Writer] with multiplexed frame encoding.
//
// Normal data is accumulated via Write() and sent as MSG_DATA frames on Flush().
// Control messages use SendMsg(), which flushes pending data first.
//
// The buffer is bounded by the writer's configured size (default 32KB matching
// upstream's IO_BUFFER_SIZE).  When Write() would exceed this limit, the buffer
// is flushed automatically.  This prevents unbounded memory growth during large
// file transfers and ensures the remote end receives data incrementally.
type Writer struct {
	w       io.Writer
	buf     bytes.Buffer // accumulates raw writes for batching into MSG_DATA frames
	bufSize int          // max buffer size before auto-flush (0 = unlimited)
}

func NewWriter(w io.Writer) *Writer {
	return &Writer{w: w, bufSize: defaultBufSize}
}

// SetBufferSize sets the maximum buffer size before auto-flush.
// A value of 0 disables auto-flush (buffer grows unbounded).
// The default is 32KB, matching upstream's IO_BUFFER_SIZE.
func (w *Writer) SetBufferSize(size int) {
	w.bufSize = size
}

// Write accumulates raw bytes.  They will be sent as MSG_DATA on Flush().
// This matches upstream's write_buf() behavior -- the caller never sees mux headers.
// Multiple small Writes are batched into larger frames for efficiency.
// If the buffer is full, it is flushed before more data is written, keeping
// buffered memory bounded by the configured buffer size.
func (w *Writer) Write(p []byte) (n int, err error) {
	if w.bufSize == 0 {
		return w.buf.Write(p)
	}

	for len(p) > 0 {
		if w.buf.Len() >= w.bufSize {
			if err := w.Flush(); err != nil {
				return n, err
			}
		}
		remaining := w.bufSize - w.buf.Len()
		chunk := p
		if len(chunk) > remaining {
			chunk = chunk[:remaining]
		}
		wn, err := w.buf.Write(chunk)
		n += wn
		if err != nil {
			return n, err
		}
		p = p[wn:]
	}
	return n, nil
}

// Flush sends accumulated data as MSG_DATA frame(s).
// If the buffer is empty, Flush is a no-op.
func (w *Writer) Flush() error {
	if w.buf.Len() == 0 {
		return nil
	}
	data := w.buf.Bytes()
	w.buf.Reset()

	// Split into chunks of max frame size
	maxPayload := 0xFFFFFF
	for len(data) > 0 {
		chunk := data
		if len(chunk) > maxPayload {
			chunk = chunk[:maxPayload]
		}
		if err := w.writeFrame(MsgData, chunk); err != nil {
			return err
		}
		data = data[len(chunk):]
	}
	return nil
}

// SendMsg sends a non-DATA message (MSG_SUCCESS, MSG_ERROR, etc.).
// It flushes any pending buffered data first, ensuring MSG_DATA frames precede control messages.
// This matches upstream's send_msg() behavior.
func (w *Writer) SendMsg(code uint8, payload []byte) error {
	if err := w.Flush(); err != nil {
		return err
	}
	return w.writeFrame(code, payload)
}

// writeFrame writes a single mux frame to the underlying writer.
// Header format: 4-byte LE int32 with high byte = MPLEX_BASE + code, low 24 bits = length.
// This matches upstream's SIVAL(hdr, 0, ((MPLEX_BASE+code)<<24) + len) format.
// The header and payload are combined into a single write to avoid TCP segmentation
// issues where the peer might process the header before the payload arrives.
func (w *Writer) writeFrame(code uint8, payload []byte) error {
	if len(payload) > 0xFFFFFF {
		return ErrPayloadTooLarge
	}

	// Combine header + payload into a single write.
	// Header is a LE int32: high byte = code, low 24 bits = length.
	combined := make([]byte, 4+len(payload))
	binary.LittleEndian.PutUint32(combined, uint32(mplexBase+code)<<24|uint32(len(payload)))
	if len(payload) > 0 {
		copy(combined[4:], payload)
	}
	_, err := w.w.Write(combined)
	return err
}

// Reader reads multiplexed frames from an [io.Reader].
//
// Normal data is read via Read(), which transparently fetches MSG_DATA frames.
// Control messages use RecvMsg().
type Reader struct {
	r   io.Reader
	buf bytes.Buffer // accumulated payload from MSG_DATA frames
}

func NewReader(r io.Reader) *Reader { return &Reader{r: r} }

// ReadDataChunk reads the next MSG_DATA frame's payload as a single chunk.
// Returns the payload bytes without blocking for more frames.
// This is useful for reading bounded messages like file lists where the
// caller needs to know the frame boundary.
func (r *Reader) ReadDataChunk() ([]byte, error) {
	// Drain any buffered data first
	var combined []byte
	if r.buf.Len() > 0 {
		combined = append(combined, r.buf.Bytes()...)
		r.buf.Reset()
	}

	// Read the next MSG_DATA frame, dropping any interleaved logging /
	// no-op frames (see Read) -- from the application's point of view
	// they are invisible
	var code uint8
	var payload []byte
	var err error
	for {
		code, payload, err = r.readFrame()
		if err != nil {
			return nil, err
		}
		if code == MsgData {
			break
		}
		if !isInfoMessage(code) {
			return nil, fmt.Errorf("expected MSG_DATA, got code %d", code)
		}
	}

	if len(combined) > 0 {
		combined = append(combined, payload...)
		return combined, nil
	}
	return payload, nil
}

// Read from the transparent buffer.  Fetches more MSG_DATA frames as needed.
// This matches upstream's read_buf() behavior -- the caller never sees mux headers.
// Peer logging and no-op control frames interleaved with the data (MSG_INFO and siblings) are skipped the way upstream's read_a_msg() forwards them to the log inline; any other non-DATA message still returns an error so the caller can use RecvMsg.
// Returns io.EOF only when the underlying reader returns EOF with no buffered data.
func (r *Reader) Read(p []byte) (n int, err error) {
	// Return buffered data first
	if r.buf.Len() > 0 {
		return r.buf.Read(p)
	}

	// Fetch the next MSG_DATA frame
	for {
		code, payload, err := r.readFrame()
		if err != nil {
			return 0, err
		}
		if code != MsgData {
			if isInfoMessage(code) {
				// peer logging / no-op frame -- dropped (there is no log
				// channel in this library to forward it to)
				continue
			}
			// Non-DATA message -- return error so caller can use RecvMsg
			return 0, &unexpectedMsgError{code: code}
		}
		// Copy MSG_DATA payload into buffer
		r.buf.Write(payload)
		if r.buf.Len() > 0 {
			break // have data to return
		}
		// Empty MSG_DATA frame -- fetch next
	}

	return r.buf.Read(p)
}

// RecvMsg reads a non-DATA message (MSG_SUCCESS, MSG_ERROR, etc.).
// It skips any pending MSG_DATA data and any MSG_DATA frames on the wire.
func (r *Reader) RecvMsg() (code uint8, payload []byte, err error) {
	// Discard any pending MSG_DATA data in buffer
	r.buf.Reset()

	// Skip MSG_DATA frames until we find a non-DATA message
	for {
		code, payload, err = r.readFrame()
		if err != nil {
			return 0, nil, err
		}
		if code != MsgData {
			return code, payload, nil
		}
		// Skip this MSG_DATA frame, read next
	}
}

// readFrame reads a single mux frame from the underlying reader.
func (r *Reader) readFrame() (code uint8, payload []byte, err error) {
	var hdr [4]byte
	if _, err = io.ReadFull(r.r, hdr[:]); err != nil {
		return 0, nil, err
	}

	// Header is a LE int32: high byte = MPLEX_BASE + code, low 24 bits = length.
	tag := binary.LittleEndian.Uint32(hdr[:])
	code = uint8((tag >> 24) - mplexBase)
	payloadLen := tag & 0xFFFFFF

	payload = make([]byte, payloadLen)
	if payloadLen > 0 {
		if _, err = io.ReadFull(r.r, payload); err != nil {
			return 0, nil, err
		}
	}
	return code, payload, nil
}

// isInfoMessage reports whether the peer's control frame is one of the logging / no-op messages that upstream's read_a_msg() forwards to the log (or ignores) inline, without interrupting the data stream -- a peer may send them at any point on a multiplexed channel (legitimate or otherwise), so a data reader must not treat them as protocol errors.
func isInfoMessage(code uint8) bool {
	switch code {
	case MsgInfo, MsgError, MsgWarning, MsgErrorSocket, MsgLog, MsgClient, MsgErrorUTF8, MsgNoop:
		return true
	}
	return false
}

// unexpectedMsgError is returned by Read() when a non-DATA message arrives.
type unexpectedMsgError struct {
	code uint8
}

func (e *unexpectedMsgError) Error() string {
	return "unexpected non-DATA message code " + string(e.code)
}
