package protocol

import (
	"io"
)

// WriteVstring writes a length-prefixed string to w.
// Format: 1-byte length if len < 128, 2-byte length if len >= 128.
// Max length is 32767 (0x7FFF).
// Source: .upstream/io.c:2482, `void write_vstring(int f, const char *str, int len)`
func WriteVstring(w io.Writer, s string) error {
	length := len(s)
	if length > 0x7FFF {
		return io.ErrShortWrite
	}

	if length > 0x7F {
		// 2-byte length: first byte has high bit set
		_, err := w.Write([]byte{byte(length>>8) | 0x80, byte(length)})
		if err != nil {
			return err
		}
	} else {
		// 1-byte length
		_, err := w.Write([]byte{byte(length)})
		if err != nil {
			return err
		}
	}

	if length > 0 {
		_, err := io.WriteString(w, s)
		return err
	}
	return nil
}

// ReadVstring reads a length-prefixed string from r.
// Format: 1-byte length if len < 128, 2-byte length if len >= 128.
// Source: .upstream/io.c:2174, `int read_vstring(int f, char *buf, int bufsize)`
func ReadVstring(r io.Reader) (string, error) {
	var b [1]byte
	if _, err := io.ReadFull(r, b[:]); err != nil {
		return "", err
	}

	length := int(b[0])
	if length&0x80 != 0 {
		if _, err := io.ReadFull(r, b[:]); err != nil {
			return "", err
		}
		length = (length & 0x7F)<<8 | int(b[0])
	}

	if length == 0 {
		return "", nil
	}

	data := make([]byte, length)
	if _, err := io.ReadFull(r, data); err != nil {
		return "", err
	}
	return string(data), nil
}
