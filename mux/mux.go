// Package mux implements the rsync multiplexed I/O framing protocol.
//
// Every frame is a 4-byte little-endian header followed by a variable-length payload:
//
//	bits [31..24] = MPLEX_BASE + msgCode (MPLEX_BASE = 7)
//	bits [23..0]  = payload length (max ~16MB per frame)
package mux

import (
	"encoding/binary"
	"errors"
	"io"
)

const mplexBase = 7

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
	MsgNoSend      uint8 = 102 // sender failed to open a file we wanted; payload is int32 ndx
)

// Writer wraps an [io.Writer] with multiplexed frame encoding.
type Writer struct {
	w io.Writer
}

func NewWriter(w io.Writer) *Writer { return &Writer{w: w} }

// WriteMsg encodes a single multiplexed frame and writes it to the underlying writer.
// code is one of the Msg* constants; payload is the raw message body (max 2^24-1 bytes).
func (w *Writer) WriteMsg(code uint8, payload []byte) error {
	if len(payload) > 0xFFFFFF {
		return ErrPayloadTooLarge
	}

	var hdr [4]byte
	binary.LittleEndian.PutUint32(hdr[:], uint32(mplexBase+code)<<24|uint32(len(payload)))
	_, err := w.w.Write(hdr[:])
	if err != nil {
		return err
	}

	if len(payload) > 0 {
		_, err = w.w.Write(payload)
	}
	return err
}

// Reader reads multiplexed frames from an [io.Reader].
type Reader struct {
	r io.Reader
}

func NewReader(r io.Reader) *Reader { return &Reader{r: r} }

// ReadMsg reads the next multiplexed frame and returns its message code, payload bytes, and any error.
func (r *Reader) ReadMsg() (code uint8, payload []byte, err error) {
	var hdr [4]byte
	if _, err = io.ReadFull(r.r, hdr[:]); err != nil {
		return 0, nil, err
	}

	val := binary.LittleEndian.Uint32(hdr[:])
	msgCode := val >> 24
	payloadLen := uint32(val & 0xFFFFFF)

	codeVal := msgCode - mplexBase

	payload = make([]byte, payloadLen)
	if payloadLen > 0 {
		if _, err = io.ReadFull(r.r, payload); err != nil {
			return 0, nil, err
		}
	}

	code = uint8(codeVal)
	return code, payload, nil
}
