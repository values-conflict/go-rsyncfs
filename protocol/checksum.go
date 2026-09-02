package protocol

import (
	"crypto/md5"
	"encoding/binary"
	"fmt"
	"io"
	"math"

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

// Checksum2 computes the strong hash with the checksum seed.  The seed is a 4-byte little-endian value mixed into the digest only when non-zero.  For MD4 the seed is always appended after the data (hash(data || seed)); for MD5 the seedFix flag (CF_CHKSUM_SEED_FIX negotiated) controls the order: true prepends the seed (hash(seed || data)), false appends it (hash(data || seed)).  Returns the first s2Length bytes of the digest.
//
// Supported digest names: "md4", "md5".
// Source: .upstream/checksum.c, `void get_checksum2(char *buf, int32 len, char *sum)` -- the MD4 path copies data into a scratch buffer and appends the seed in place, so it is always data-then-seed regardless of proper_seed_order; only the MD5 path branches on it.
func Checksum2(data []byte, digest string, s2Length int, seed int32, seedFix bool) []byte {
	var result []byte
	var seedBuf [4]byte
	if seed != 0 {
		binary.LittleEndian.PutUint32(seedBuf[:], uint32(seed))
	}

	switch digest {
	case "md4":
		h := md4.New()
		h.Write(data)
		if seed != 0 {
			h.Write(seedBuf[:])
		}
		result = h.Sum(nil)

	case "md5":
		h := md5.New()
		if seed != 0 && seedFix {
			h.Write(seedBuf[:])
		}
		h.Write(data)
		if seed != 0 && !seedFix {
			h.Write(seedBuf[:])
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

// FileChecksum computes the whole-file transfer checksum that the sender
// appends after each delta.  The receiver rebuilds the file, feeds it
// through its own streaming sum, and compares -- so this must mirror the
// receiver's sum_init/sum_update/sum_end for the negotiated protocol
// version.  From proto 30 on the streaming sum ignores the seed (plain
// digest of the content); below that the legacy MD4 streaming sum feeds
// the seed into the context first, so the digest is MD4(seed || data).
//
// Supported digest names: "md4", "md5" (only md4 applies below proto 30).
// Source: .upstream/checksum.c, `void sum_init(int seed)` in v3.1.x (the
// pre-proto-30 branch seeds the MD4 context before any data is fed in).
func FileChecksum(data []byte, digest string, version int, seed int32) []byte {
	var seedBuf [4]byte
	if seed != 0 {
		binary.LittleEndian.PutUint32(seedBuf[:], uint32(seed))
	}

	switch digest {
	case "md4":
		h := md4.New()
		if version < 30 && seed != 0 {
			h.Write(seedBuf[:])
		}
		h.Write(data)
		return h.Sum(nil)
	case "md5":
		h := md5.New()
		h.Write(data)
		return h.Sum(nil)
	default:
		// unsupported digest -- return nil
		return nil
	}
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

// maxBlockLength is the peer-supplied block length cap (upstream rsync.h): OLD_MAX_BLOCK_SIZE below proto 30, MAX_BLOCK_SIZE from there on.
func maxBlockLength(version int) int32 {
	if version >= 30 {
		return 1 << 17
	}
	return 1 << 29
}

// MaxStrongHashLength is the longest digest length this library supports (md4 and md5 are both 16 bytes), used to bound a peer-supplied s2length.
const MaxStrongHashLength = 16

// ReadSumHead reads a SumHead from r in wire format, rejecting out-of-range values the way upstream read_sum_head does before any of them are used (a malicious peer's sum struct must fail the connection with a clean error, not corrupt a downstream allocation or hash search).
//
// For proto >= 27, reads all four fields (16 bytes).
// For proto < 27, reads count, blength, remainder (12 bytes); s2length is set to 0.
//
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

	// upstream read_sum_head's validation guards, in wire order
	if sh.Count < 0 {
		return SumHead{}, fmt.Errorf("invalid checksum count %d", sh.Count)
	}
	if sh.BLength < 0 || sh.BLength > maxBlockLength(version) {
		return SumHead{}, fmt.Errorf("invalid block length %d", sh.BLength)
	}
	if sh.Count > 0 && sh.BLength == 0 {
		return SumHead{}, fmt.Errorf("invalid zero block length")
	}
	// the blocks describe the file's contents, and a file length is a
	// varlong30 (int32) -- count*blength past that would overflow any
	// downstream arithmetic on the product
	if sh.BLength > 0 && sh.Count > math.MaxInt32/sh.BLength {
		return SumHead{}, fmt.Errorf("invalid checksum count %d (count*block length exceeds a file length)", sh.Count)
	}
	if version >= 27 && (sh.S2Length < 0 || sh.S2Length > MaxStrongHashLength) {
		return SumHead{}, fmt.Errorf("invalid checksum length %d", sh.S2Length)
	}

	return sh, nil
}
