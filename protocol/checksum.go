package protocol

import (
	"crypto/md5"
	"encoding/binary"
	"io"

	"golang.org/x/crypto/md4"
)

// Checksum1 computes the rsync rolling checksum (Adler-32-inspired).
// Returns a uint32 with s1 in the low 16 bits and s2 in the high 16 bits.
// CHAR_OFFSET is 0 in upstream, so no offset is added.
// Source: .upstream/checksum.c:308-317, `uint32 get_checksum1(char *buf1, int32 len)`.
func Checksum1(data []byte) uint32 {
	s1, s2 := uint32(0), uint32(0)

	// process 4 bytes at a time
	i := 0
	for i <= len(data)-4 {
		s2 += 4*(s1+uint32(data[i])) + 3*uint32(data[i+1]) + 2*uint32(data[i+2]) + uint32(data[i+3])
		s1 += uint32(data[i]) + uint32(data[i+1]) + uint32(data[i+2]) + uint32(data[i+3])
		i += 4
	}
	for ; i < len(data); i++ {
		s1 += uint32(data[i])
		s2 += s1
	}

	return (s1 & 0xffff) | (s2 << 16)
}

// Checksum2 computes the strong hash with seed.
// When seedFix is true (CF_CHKSUM_SEED_FIX), seed is prepended: hash(seed + data).
// When seedFix is false, seed is appended: hash(data + seed).
// Returns the first s2Length bytes of the digest.
//
// Supported digest names: "md4", "md5".
// Source: .upstream/checksum.c:320-384, `void get_checksum2(char *buf, int32 len, char *sum)`.
func Checksum2(data []byte, digest string, s2Length int, seed int32, seedFix bool) []byte {
	var result []byte

	switch digest {
	case "md4":
		h := md4.New()
		if seed != 0 {
			if seedFix {
				// seed prepended: hash(seed + data)
				var seedBuf [4]byte
				binary.LittleEndian.PutUint32(seedBuf[:], uint32(seed))
				h.Write(seedBuf[:])
			}
			h.Write(data)
			if !seedFix {
				// seed appended: hash(data + seed)
				var seedBuf [4]byte
				binary.LittleEndian.PutUint32(seedBuf[:], uint32(seed))
				h.Write(seedBuf[:])
			}
		} else {
			h.Write(data)
		}
		result = h.Sum(nil)

	case "md5":
		h := md5.New()
		if seed != 0 {
			if seedFix {
				// seed prepended: hash(seed + data)
				var seedBuf [4]byte
				binary.LittleEndian.PutUint32(seedBuf[:], uint32(seed))
				h.Write(seedBuf[:])
			}
			h.Write(data)
			if !seedFix {
				// seed appended: hash(data + seed)
				var seedBuf [4]byte
				binary.LittleEndian.PutUint32(seedBuf[:], uint32(seed))
				h.Write(seedBuf[:])
			}
		} else {
			h.Write(data)
		}
		result = h.Sum(nil)

	default:
		// unsupported digest -- return nil
		return nil
	}

	if s2Length < len(result) {
		return result[:s2Length]
	}
	return result
}

// SupportedDigests returns the list of checksum algorithms this library supports,
// in preference order (strongest first).
func SupportedDigests() []string {
	return []string{"md5", "md4"}
}

// SumHead is the checksum header sent before block checksums.
// All fields are int32 LE on the wire.
type SumHead struct {
	Count     int32 // block count (0 = empty file)
	BLength   int32 // block size
	S2Length  int32 // strong hash length (only if proto >= 27)
	Remainder int32 // final partial block size
}

// WriteSumHead writes the SumHead to w in wire format.
// For proto >= 27, all four fields are written (16 bytes).
// For proto < 27, s2length is omitted (12 bytes: count, blength, remainder).
// Source: .upstream/io.c:2257-2269, `void write_sum_head(int f, struct sum_struct *sum)`.
func WriteSumHead(w io.Writer, sh SumHead, version int) error {
	var b [16]byte
	binary.LittleEndian.PutUint32(b[0:4], uint32(sh.Count))
	binary.LittleEndian.PutUint32(b[4:8], uint32(sh.BLength))
	if version >= 27 {
		binary.LittleEndian.PutUint32(b[8:12], uint32(sh.S2Length))
		binary.LittleEndian.PutUint32(b[12:16], uint32(sh.Remainder))
		_, err := w.Write(b[:])
		return err
	}
	// proto < 27: no s2length field
	binary.LittleEndian.PutUint32(b[8:12], uint32(sh.Remainder))
	_, err := w.Write(b[:12])
	return err
}

// ReadSumHead reads a SumHead from r in wire format.
// For proto >= 27, reads all four fields (16 bytes).
// For proto < 27, reads count, blength, remainder (12 bytes); s2length is set to 0.
// Source: .upstream/io.c:2195-2253, `void read_sum_head(int f, struct sum_struct *sum)`.
func ReadSumHead(r io.Reader, version int) (SumHead, error) {
	var b [16]byte
	n := 12
	if version >= 27 {
		n = 16
	}
	if _, err := io.ReadFull(r, b[:n]); err != nil {
		return SumHead{}, err
	}

	sh := SumHead{
		Count:     int32(binary.LittleEndian.Uint32(b[0:4])),
		BLength:   int32(binary.LittleEndian.Uint32(b[4:8])),
		Remainder: int32(binary.LittleEndian.Uint32(b[8:12])),
	}
	if version >= 27 {
		sh.S2Length = int32(binary.LittleEndian.Uint32(b[8:12]))
		sh.Remainder = int32(binary.LittleEndian.Uint32(b[12:16]))
	}

	return sh, nil
}
