package protocol

import (
	"encoding/binary"
	"io"
)

// Lookup table for variable-length integer decoding.
// Indexed by first_byte / 4, returns the number of extra bytes to read.
// Source: .upstream/io.c:167
var intByteExtra = [64]byte{
	0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, // (00 - 3F)/4
	0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, // (40 - 7F)/4
	1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, // (80 - BF)/4
	2, 2, 2, 2, 2, 2, 2, 2, 3, 3, 3, 3, 4, 4, 5, 6, // (C0 - FF)/4
}

// WriteInt32 writes a signed 32-bit integer as 4 bytes little-endian.
func WriteInt32(w io.Writer, v int32) error {
	var b [4]byte
	binary.LittleEndian.PutUint32(b[:], uint32(v))
	_, err := w.Write(b[:])
	return err
}

// ReadInt32 reads a signed 32-bit integer from 4 bytes little-endian.
func ReadInt32(r io.Reader) (int32, error) {
	var b [4]byte
	if _, err := io.ReadFull(r, b[:]); err != nil {
		return 0, err
	}
	return int32(binary.LittleEndian.Uint32(b[:])), nil
}

// WriteUint16 writes an unsigned 16-bit integer as 2 bytes little-endian (shortint).
func WriteUint16(w io.Writer, v uint16) error {
	var b [2]byte
	b[0] = byte(v)
	b[1] = byte(v >> 8)
	_, err := w.Write(b[:])
	return err
}

// ReadUint16 reads an unsigned 16-bit integer from 2 bytes little-endian (shortint).
func ReadUint16(r io.Reader) (uint16, error) {
	var b [2]byte
	if _, err := io.ReadFull(r, b[:]); err != nil {
		return 0, err
	}
	return uint16(b[0]) | uint16(b[1])<<8, nil
}

// WriteVarint writes a signed int32 using rsync's variable-length encoding (proto >= 30).
func WriteVarint(w io.Writer, x int32) error {
	var b [5]byte
	binary.LittleEndian.PutUint32(b[1:], uint32(x))

	cnt := 4
	for cnt > 1 && b[cnt] == 0 {
		cnt--
	}

	bit := byte(1 << (7 - cnt + 1))
	if b[cnt] >= bit {
		cnt++
		b[0] = ^(bit - 1)
	} else if cnt > 1 {
		b[0] = b[cnt] | ^(bit*2-1)
	} else {
		b[0] = b[1]
	}

	_, err := w.Write(b[:cnt])
	return err
}

// ReadVarint reads a signed int32 using rsync's variable-length encoding (proto >= 30).
func ReadVarint(r io.Reader) (int32, error) {
	var ch [1]byte
	if _, err := io.ReadFull(r, ch[:]); err != nil {
		return 0, err
	}

	extra := int(intByteExtra[ch[0]/4])

	var u [5]byte
	if extra > 0 {
		if _, err := io.ReadFull(r, u[:extra]); err != nil {
			return 0, err
		}
		bit := byte(1 << (8 - extra))
		u[extra] = ch[0] & (bit - 1)
	} else {
		u[0] = ch[0]
	}

	return int32(binary.LittleEndian.Uint32(u[:])), nil
}

// WriteVarlong writes a signed int64 using rsync's variable-length encoding with a
// minimum byte count.  The minimum is typically 3 or 4.
func WriteVarlong(w io.Writer, x int64, minBytes byte) error {
	var b [9]byte
	binary.LittleEndian.PutUint64(b[1:], uint64(x))

	cnt := 8
	for cnt > int(minBytes) && b[cnt] == 0 {
		cnt--
	}

	bit := byte(1 << (7 - cnt + int(minBytes)))
	if b[cnt] >= bit {
		cnt++
		b[0] = ^(bit - 1)
	} else if cnt > int(minBytes) {
		b[0] = b[cnt] | ^(bit*2-1)
	} else {
		b[0] = b[cnt]
	}

	_, err := w.Write(b[:cnt])
	return err
}

// ReadVarlong reads a signed int64 using rsync's variable-length encoding with a
// minimum byte count.
func ReadVarlong(r io.Reader, minBytes byte) (int64, error) {
	b2 := make([]byte, minBytes)
	if _, err := io.ReadFull(r, b2); err != nil {
		return 0, err
	}

	extra := int(intByteExtra[b2[0]/4])

	var u [9]byte
	copy(u[:], b2[1:]) // C: memcpy(u.b, b2+1, min_bytes-1)

	if extra > 0 {
		buf := make([]byte, extra)
		if _, err := io.ReadFull(r, buf); err != nil {
			return 0, err
		}
		copy(u[int(minBytes)-1:], buf) // C: read_buf(f, u.b + min_bytes - 1, extra)
		bit := byte(1 << (8 - extra))
		u[int(minBytes)+extra-1] = b2[0] & (bit - 1)
	} else {
		u[int(minBytes)-1] = b2[0]
	}

	return int64(binary.LittleEndian.Uint64(u[:])), nil
}

// WriteLongInt writes a signed integer using rsync's legacy longint encoding (proto < 30).
// Values in [0, 0x7FFFFFFF] are written as 4 bytes LE.  All other values use a
// sentinel (0xFFFFFFFF) followed by the full 8-byte LE representation.
func WriteLongInt(w io.Writer, x int64) error {
	if x >= 0 && x <= 0x7FFFFFFF {
		var b [4]byte
		binary.LittleEndian.PutUint32(b[:], uint32(x))
		_, err := w.Write(b[:])
		return err
	}

	// sentinel + full int64 LE
	if _, err := w.Write([]byte{0xFF, 0xFF, 0xFF, 0xFF}); err != nil {
		return err
	}
	var b [8]byte
	binary.LittleEndian.PutUint64(b[:], uint64(x))
	_, err := w.Write(b[:])
	return err
}

// ReadLongInt reads a signed integer using rsync's legacy longint encoding (proto < 30).
func ReadLongInt(r io.Reader) (int64, error) {
	var b [4]byte
	if _, err := io.ReadFull(r, b[:]); err != nil {
		return 0, err
	}

	val32 := int32(binary.LittleEndian.Uint32(b[:]))
	if val32 != -1 {
		return int64(val32), nil
	}

	// sentinel found, read full int64 LE
	var b64 [8]byte
	if _, err := io.ReadFull(r, b64[:]); err != nil {
		return 0, err
	}
	return int64(binary.LittleEndian.Uint64(b64[:])), nil
}

// NdxState tracks delta-encoding state for compressed NDX (proto >= 30).
// Zero value is valid: prevPositive = -1, prevNegative = 1.
type NdxState struct {
	prevPositive int32
	prevNegative int32
}

// NewNdxState returns a new NdxState with default zero-value trackers.
func NewNdxState() *NdxState {
	return &NdxState{prevPositive: -1, prevNegative: 1}
}

// WriteNdx writes a compressed NDX index.  NDX_DONE (-1) is encoded as a
// single byte 0x00 with no side effects on the state trackers.
func (s *NdxState) WriteNdx(w io.Writer, ndx int32) error {
	var b [6]byte
	cnt := 0

	if ndx >= 0 {
		diff := ndx - s.prevPositive
		s.prevPositive = ndx
		if diff < 0xFE && diff > 0 {
			b[cnt] = byte(diff)
			cnt++
		} else if diff < 0 || diff > 0x7FFF {
			b[cnt] = 0xFE
			cnt++
			b[cnt] = byte((ndx>>24)|0x80)
			cnt++
			b[cnt] = byte(ndx)
			cnt++
			b[cnt] = byte(ndx >> 8)
			cnt++
			b[cnt] = byte(ndx >> 16)
			cnt++
		} else {
			b[cnt] = 0xFE
			cnt++
			b[cnt] = byte(diff >> 8)
			cnt++
			b[cnt] = byte(diff)
			cnt++
		}
	} else if ndx == NDxDone {
		b[0] = 0
		cnt = 1
	} else {
		b[cnt] = 0xFF
		cnt++
		ndx = -ndx
		diff := ndx - s.prevNegative
		s.prevNegative = ndx
		if diff < 0xFE && diff > 0 {
			b[cnt] = byte(diff)
			cnt++
		} else if diff < 0 || diff > 0x7FFF {
			b[cnt] = 0xFE
			cnt++
			b[cnt] = byte((ndx>>24)|0x80)
			cnt++
			b[cnt] = byte(ndx)
			cnt++
			b[cnt] = byte(ndx >> 8)
			cnt++
			b[cnt] = byte(ndx >> 16)
			cnt++
		} else {
			b[cnt] = 0xFE
			cnt++
			b[cnt] = byte(diff >> 8)
			cnt++
			b[cnt] = byte(diff)
			cnt++
		}
	}

	_, err := w.Write(b[:cnt])
	return err
}

// ReadNdx reads a compressed NDX index.  Returns NDxDone (-1) for the
// end-of-list marker.  A peer-supplied index that overflows signed int32
// is rejected with an error.
func (s *NdxState) ReadNdx(r io.Reader) (int32, error) {
	var b [4]byte
	if _, err := io.ReadFull(r, b[:1]); err != nil {
		return 0, err
	}

	if b[0] == 0xFF {
		if _, err := io.ReadFull(r, b[1:2]); err != nil {
			return 0, err
		}
		return s.readNdxPositive(r, b[1:], &s.prevNegative, true)
	} else if b[0] == 0 {
		return NDxDone, nil
	}

	return s.readNdxPositive(r, b[:1], &s.prevPositive, false)
}

// readNdxPositive reads the rest of a positive NDX value.
// firstByte is the first data byte already read (after the 0xFF prefix if negative).
// prev is the pointer to the appropriate tracker (prevPositive or prevNegative).
// negate controls whether the final result is negated.
func (s *NdxState) readNdxPositive(r io.Reader, firstByte []byte, prev *int32, negate bool) (int32, error) {
	var num uint32
	if firstByte[0] == 0xFE {
		var b [2]byte
		if _, err := io.ReadFull(r, b[:]); err != nil {
			return 0, err
		}
		if b[0]&0x80 != 0 {
			// full 4-byte absolute value -- upstream uses non-standard byte order:
			// wire: [B3|0x80, B0, B1, B2] -> LE read as [B0, B1, B2, B3&0x7F]
			var full [4]byte
			full[0] = b[1]       // B0
			if _, err := io.ReadFull(r, full[1:3]); err != nil {
				return 0, err
			}
			full[3] = b[0] &^ 0x80 // B3 & 0x7F
			num = binary.LittleEndian.Uint32(full[:])
		} else {
			num = uint32(b[0])<<8 + uint32(b[1]) + uint32(*prev)
		}
	} else {
		num = uint32(firstByte[0]) + uint32(*prev)
	}

	if num > uint32(2147483647) {
		return 0, io.ErrUnexpectedEOF // protocol violation: index overflows signed int32
	}

	result := int32(num)
	*prev = result
	if negate {
		result = -result
	}
	return result, nil
}
