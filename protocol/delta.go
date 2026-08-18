package protocol

import (
	"encoding/binary"
	"io"
)

// deltaChunkSize is the maximum literal data size per token in the simple
// (non-compressing) delta stream.  Matches upstream CHUNK_SIZE.
// Source: .upstream/rsync.h:158, `#define CHUNK_SIZE (32*1024)`.
const deltaChunkSize = 32 * 1024

// DeltaWriter sends delta tokens one at a time using the simple
// (non-compressing) wire format.  This matches upstream's
// simple_send_token() when do_compression == CPRES_NONE.
//
// The wire format per token is:
//   - Literal: int32(positive_len) + len bytes of data (chunked at 32KB)
//   - Match: int32(-(blockIdx+1)) (negative int32)
//   - End: int32(0)
//
// Source: .upstream/token.c:317-331, `static void simple_send_token(...)`.
type DeltaWriter struct {
	w io.Writer
}

// NewDeltaWriter creates a DeltaWriter that writes to w.
func NewDeltaWriter(w io.Writer) *DeltaWriter {
	return &DeltaWriter{w: w}
}

// WriteLiteral sends literal data.  Data larger than 32KB is split into
// multiple wire tokens (each capped at deltaChunkSize), matching upstream's
// CHUNK_SIZE limit in simple_send_token().
func (w *DeltaWriter) WriteLiteral(data []byte) error {
	for len(data) > 0 {
		n := len(data)
		if n > deltaChunkSize {
			n = deltaChunkSize
		}
		var b [4]byte
		binary.LittleEndian.PutUint32(b[:], uint32(n))
		if _, err := w.w.Write(b[:]); err != nil {
			return err
		}
		if _, err := w.w.Write(data[:n]); err != nil {
			return err
		}
		data = data[n:]
	}
	return nil
}

// WriteMatch sends a match token referencing block blockIdx from the basis
// file.  The wire encoding is int32(-(blockIdx+1)).
//
// Source: .upstream/token.c:330, `write_int(f, -(token+1))`.
func (w *DeltaWriter) WriteMatch(blockIdx int32) error {
	var b [4]byte
	binary.LittleEndian.PutUint32(b[:], uint32(-blockIdx-1))
	_, err := w.w.Write(b[:])
	return err
}

// WriteEnd sends the end-of-stream marker (int32(0)).
//
// Source: .upstream/token.c:330, `write_int(f, -(token+1))` with token=-1.
func (w *DeltaWriter) WriteEnd() error {
	var b [4]byte
	// token=-1 -> -(−1+1) = 0
	binary.LittleEndian.PutUint32(b[:], 0)
	_, err := w.w.Write(b[:])
	return err
}

// DeltaReader consumes delta tokens one at a time from the simple
// (non-compressing) wire format.  This matches upstream's
// simple_recv_token() when do_compression == CPRES_NONE.
//
// Source: .upstream/token.c:289-314, `static int32 simple_recv_token(...)`.
type DeltaReader struct {
	r       io.Reader
	residue int32  // remaining bytes in current literal
	buf     []byte // full buffer for current literal
	bufOff  int    // current read offset within buf
}

// NewDeltaReader creates a DeltaReader that reads from r.
func NewDeltaReader(r io.Reader) *DeltaReader {
	return &DeltaReader{r: r}
}

// ReadToken returns the next delta token from the stream.
//
// Returns:
//   - data non-nil: literal data of len(data) bytes
//   - data nil, blockIdx >= 0: match reference to basis block blockIdx
//   - data nil, blockIdx < 0, isEnd true: end of stream
//
// Large literals (>32KB) are returned in multiple calls, each returning a
// chunk up to deltaChunkSize bytes, matching upstream's CHUNK_SIZE behavior.
//
// Source: .upstream/token.c:289-314, `static int32 simple_recv_token(...)`.
func (r *DeltaReader) ReadToken() (data []byte, blockIdx int32, isEnd bool, err error) {
	if r.residue > 0 {
		// Return next chunk of current literal.
		n := r.residue
		if n > deltaChunkSize {
			n = deltaChunkSize
		}
		data = r.buf[r.bufOff : r.bufOff+int(n)]
		if _, err = io.ReadFull(r.r, data); err != nil {
			r.residue = 0
			r.buf = nil
			return nil, 0, false, err
		}
		r.residue -= n
		r.bufOff += int(n)
		return data, 0, false, nil
	}

	// Read the token header (int32 LE).
	var b [4]byte
	if _, err = io.ReadFull(r.r, b[:]); err != nil {
		return nil, 0, false, err
	}
	val := int32(binary.LittleEndian.Uint32(b[:]))

	if val == 0 {
		// end of stream
		return nil, -1, true, nil
	}
	if val < 0 {
		// match reference: blockIdx = -(val+1)
		return nil, -val - 1, false, nil
	}

	// Literal data: val is the total length.
	// Allocate buffer and read first chunk.
	r.residue = val
	r.buf = make([]byte, val)
	r.bufOff = 0
	n := val
	if n > deltaChunkSize {
		n = deltaChunkSize
	}
	data = r.buf[:n]
	if _, err = io.ReadFull(r.r, data); err != nil {
		r.residue = 0
		r.buf = nil
		return nil, 0, false, err
	}
	r.residue -= n
	r.bufOff += int(n)
	return data, 0, false, nil
}

// DeltaToken represents a single command in the delta stream.
// If Literal is non-nil, it's literal data.
// If Literal is nil, BlockIdx is the basis block reference.
type DeltaToken struct {
	Literal  []byte // non-nil for literal data
	BlockIdx int32  // valid when Literal is nil (block reference)
}

// ParseDeltaStream reads all delta tokens from r until the end marker.
// Convenience wrapper around DeltaReader that loads the full stream into memory.
func ParseDeltaStream(r io.Reader) ([]DeltaToken, error) {
	dr := NewDeltaReader(r)
	var tokens []DeltaToken

	for {
		data, blockIdx, isEnd, err := dr.ReadToken()
		if err != nil {
			return nil, err
		}
		if isEnd {
			return tokens, nil
		}
		if data != nil {
			// Copy data out since the reader may reuse the buffer.
			cp := make([]byte, len(data))
			copy(cp, data)
			tokens = append(tokens, DeltaToken{Literal: cp})
		} else {
			tokens = append(tokens, DeltaToken{BlockIdx: blockIdx})
		}
	}
}

// WriteDeltaStream writes delta tokens to w.
// Convenience wrapper around DeltaWriter.
func WriteDeltaStream(w io.Writer, tokens []DeltaToken) error {
	dw := NewDeltaWriter(w)
	for _, t := range tokens {
		if t.Literal != nil {
			if err := dw.WriteLiteral(t.Literal); err != nil {
				return err
			}
		} else {
			if err := dw.WriteMatch(t.BlockIdx); err != nil {
				return err
			}
		}
	}
	return dw.WriteEnd()
}
