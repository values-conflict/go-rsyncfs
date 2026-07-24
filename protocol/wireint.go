package protocol

import (
	"encoding/binary"
	"io"
)

var intByteExtra = [64]byte{
	0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, // (00 - 3F)/4
	0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, // (40 - 7F)/4
	1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, // (80 - BF)/4
	2, 2, 2, 2, 2, 2, 2, 2, 3, 3, 3, 3, 4, 4, 5, 6, // (C0 - FF)/4
}

// WriteVarint writes a signed int32 using rsync's variable-length encoding.
func WriteVarint(w io.Writer, x int32) error {
	b := make([]byte, 5)
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

// ReadVarint reads a signed int32 using rsync's variable-length encoding.
func ReadVarint(r io.Reader) (int32, error) {
	var ch [1]byte
	if _, err := io.ReadFull(r, ch[:]); err != nil {
		return 0, err
	}

	extra := int(intByteExtra[ch[0]/4])
	u := make([]byte, 5)
	if extra > 0 {
		if _, err := io.ReadFull(r, u[:extra]); err != nil {
			return 0, err
		}
		bit := byte(1 << (8 - extra))
		u[extra] = ch[0] & (bit - 1)
	} else {
		u[0] = ch[0]
	}

	return int32(binary.LittleEndian.Uint32(u)), nil
}

// WriteVarlong writes a signed int64 using rsync's variable-length encoding with a minimum byte count.
func WriteVarlong(w io.Writer, x int64, minBytes byte) error {
	b := make([]byte, 9)
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

// ReadVarlong reads a signed int64 using rsync's variable-length encoding with a minimum byte count.
func ReadVarlong(r io.Reader, minBytes byte) (int64, error) {
	b2 := make([]byte, minBytes)
	if _, err := io.ReadFull(r, b2); err != nil {
		return 0, err
	}

	extra := int(intByteExtra[b2[0]/4])
	u := make([]byte, 9)
	copy(u, b2[1:]) // Copy bytes from index 1 of b2 to start of u. (C: memcpy(u.b, b2+1, min_bytes-1))

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

	return int64(binary.LittleEndian.Uint64(u)), nil
}

// WriteLongInt writes a signed integer using rsync's legacy longint encoding (protocol < 30).
func WriteLongInt(w io.Writer, x int64) error {
	if x >= 0 && x <= 0x7FFFFFFF {
		b := make([]byte, 4)
		binary.LittleEndian.PutUint32(b, uint32(x))
		_, err := w.Write(b)
		return err
	}

	// Sentinel + full int64 LE
	sentinel := [4]byte{0xFF, 0xFF, 0xFF, 0xFF}
	if _, err := w.Write(sentinel[:]); err != nil {
		return err
	}
	b := make([]byte, 8)
	binary.LittleEndian.PutUint64(b, uint64(x))
	_, err := w.Write(b)
	return err
}

// ReadLongInt reads a signed integer using rsync's legacy longint encoding (protocol < 30).
func ReadLongInt(r io.Reader) (int64, error) {
	b := make([]byte, 4)
	if _, err := io.ReadFull(r, b); err != nil {
		return 0, err
	}

	val32 := int32(binary.LittleEndian.Uint32(b))
	if val32 != -1 {
		return int64(val32), nil
	}

	// Sentinel found, read full int64 LE
	b64 := make([]byte, 8)
	if _, err := io.ReadFull(r, b64); err != nil {
		return 0, err
	}
	return int64(binary.LittleEndian.Uint64(b64)), nil
}
