package rsyncfs

import (
	"crypto/md5"
	"encoding/binary"
	"fmt"
	"io"
	"io/fs"

	"github.com/values-conflict/go-rsyncfs/protocol/mux"
)

// defaultBlockSize is the standard rsync block size for files <= BLOCK_SIZE^2 bytes.
const defaultBlockSize = 700

// maxBlockSize is the maximum block size for protocol >= 30 (1 << 17 = 131072).
const maxBlockSize = 1 << 17

// sumHead holds the checksum header sent before block checksums.
type sumHead struct {
	count     int32 // number of blocks (0 = empty file, -1 = error)
	blength   int32 // block size in bytes
	s2length  int32 // length of second checksum hash
	remainder int32 // size of final partial block
}

// computeSumHead calculates the block parameters for a file of the given size.
func computeSumHead(fileSize int64, version int) sumHead {
	if fileSize == 0 {
		return sumHead{count: 0}
	}

	blength := int32(defaultBlockSize)
	if fileSize > defaultBlockSize*defaultBlockSize {
		// match upstream rsync block size computation: rounded square root of file size
		// C: for (c = 1, l = len; l >>= 2; c <<= 1) {}
		c := int32(1)
		for l := fileSize; ; {
			l >>= 2
			if l == 0 {
				break
			}
			c <<= 1
		}
		if c >= maxBlockSize {
			blength = maxBlockSize
		} else {
			blength = 0
			for {
				blength |= c
				if fileSize < int64(blength)*int64(blength) {
					blength &= ^c
				}
				if c < 8 {
					break
				}
				c >>= 1
			}
			if blength < defaultBlockSize {
				blength = defaultBlockSize
			}
		}
	}

	count := int32(fileSize / int64(blength))
	remainder := int32(fileSize % int64(blength))
	if remainder > 0 {
		count++
	}

	// s2length: MD5 = 16 bytes
	s2length := int32(16)

	return sumHead{
		count:     count,
		blength:   blength,
		s2length:  s2length,
		remainder: remainder,
	}
}

// checksum1 computes the rsync rolling checksum (Adler-like) for a buffer.
func checksum1(data []byte) uint32 {
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

// checksum2 computes the strong hash (MD5) for a buffer.
// Returns the first s2length bytes of the digest.
func checksum2(data []byte, s2length int) []byte {
	h := md5.Sum(data)
	return h[:s2length]
}

// sendFile sends one file via the multiplexed I/O layer using rsync's block checksum protocol.
// The server acts as the sender: it provides block checksums, receives a delta stream from the
// client, sends the file data, and verifies with a final checksum.
//
// The protocol flow is:
//  1. Server sends sum_head (block count, block size, etc.) as MSG_DATA
//  2. Server sends per-block checksums (sum1 + sum2 for each block) as MSG_DATA
//  3. Client sends delta stream (token references for blocks it needs) as MSG_DATA
//  4. Server sends file data as MSG_DATA
//  5. Server sends file checksum as MSG_DATA
//  6. Client sends MSG_SUCCESS with file index
func sendFile(r *mux.Reader, w *mux.Writer, f fs.File, version int) error {
	// read the entire file into memory for checksum computation
	data, err := io.ReadAll(f)
	if err != nil {
		return fmt.Errorf("read file data: %w", err)
	}

	fileSize := int64(len(data))
	sh := computeSumHead(fileSize, version)

	// --- step 1: send sum_head ---
	if err := writeSumHead(w, sh, version); err != nil {
		return fmt.Errorf("write sum head: %w", err)
	}

	// --- step 2: send per-block checksums ---
	if sh.count > 0 {
		if err := sendBlockChecksums(w, data, sh); err != nil {
			return fmt.Errorf("send block checksums: %w", err)
		}
	}

	// --- step 3: read delta stream from client ---
	// the delta stream tells us which blocks the client needs.
	// for a fresh pull (no local copy), the client sends token references for all blocks.
	if err := readDeltaStream(r, sh); err != nil {
		return fmt.Errorf("read delta stream: %w", err)
	}

	// --- step 4: send file data ---
	// send the full file data as MSG_DATA
	if err := w.WriteMsg(mux.MsgData, data); err != nil {
		return fmt.Errorf("send file data: %w", err)
	}

	// --- step 5: send file checksum for verification ---
	if err := sendFileChecksum(w, data, int(sh.s2length)); err != nil {
		return fmt.Errorf("send file checksum: %w", err)
	}

	// --- step 6: wait for MSG_SUCCESS ---
	code, payload, err := r.ReadMsg()
	if err != nil {
		return fmt.Errorf("read success msg: %w", err)
	}
	if code != mux.MsgSuccess {
		return fmt.Errorf("expected MSG_SUCCESS, got code %d", code)
	}
	if len(payload) < 4 {
		return fmt.Errorf("MSG_SUCCESS payload too short: %d bytes", len(payload))
	}
	ndx := int32(binary.LittleEndian.Uint32(payload))
	_ = ndx // file index, logged but not used further

	return nil
}

// writeSumHead writes the sum header as a MSG_DATA frame.
func writeSumHead(w *mux.Writer, sh sumHead, version int) error {
	buf := make([]byte, 16) // 4 int32s
	binary.LittleEndian.PutUint32(buf[0:4], uint32(sh.count))
	binary.LittleEndian.PutUint32(buf[4:8], uint32(sh.blength))
	if version >= 27 {
		binary.LittleEndian.PutUint32(buf[8:12], uint32(sh.s2length))
		binary.LittleEndian.PutUint32(buf[12:16], uint32(sh.remainder))
		return w.WriteMsg(mux.MsgData, buf)
	}
	// proto < 27: no s2length field
	binary.LittleEndian.PutUint32(buf[8:12], uint32(sh.remainder))
	return w.WriteMsg(mux.MsgData, buf[:12])
}

// sendBlockChecksums writes per-block checksums as a single MSG_DATA frame.
// Each block gets: sum1 (4 bytes LE) + sum2 (s2length bytes).
func sendBlockChecksums(w *mux.Writer, data []byte, sh sumHead) error {
	bufSize := sh.count * (4 + int32(sh.s2length))
	buf := make([]byte, bufSize)

	offset := int64(0)
	bufOffset := int32(0)
	for i := int32(0); i < sh.count; i++ {
		var blockEnd int64
		if i == sh.count-1 && sh.remainder > 0 {
			blockEnd = offset + int64(sh.remainder)
		} else {
			blockEnd = offset + int64(sh.blength)
		}

		block := data[offset:blockEnd]

		// sum1: rolling checksum (4 bytes LE)
		s1 := checksum1(block)
		binary.LittleEndian.PutUint32(buf[bufOffset:bufOffset+4], s1)
		bufOffset += 4

		// sum2: strong hash (s2length bytes)
		s2 := checksum2(block, int(sh.s2length))
		copy(buf[bufOffset:bufOffset+int32(len(s2))], s2)
		bufOffset += int32(len(s2))

		offset = blockEnd
	}

	return w.WriteMsg(mux.MsgData, buf)
}

// readDeltaStream reads the delta stream from the client.
// The delta stream tells the server which blocks the client needs.
// For a fresh pull (no local copy), the client sends token references for all blocks.
//
// The delta stream format (non-compressed) is:
//   - Literal data: int32(len) > 0, followed by len bytes of data
//   - Token reference: int32(-(blockIndex+1)), means "I need block blockIndex"
//   - End of stream: int32(0)
func readDeltaStream(r *mux.Reader, sh sumHead) error {
	code, payload, err := r.ReadMsg()
	if err != nil {
		return fmt.Errorf("read delta msg: %w", err)
	}
	if code != mux.MsgData {
		return fmt.Errorf("expected MSG_DATA for delta, got code %d", code)
	}

	return parseDeltaStream(payload, sh)
}

// parseDeltaStream parses the raw delta stream bytes and validates the tokens.
// The server uses this to know which blocks the client needs, then sends the full file data.
func parseDeltaStream(data []byte, sh sumHead) error {
	offset := 0

	for offset < len(data) {
		if offset+4 > len(data) {
			return fmt.Errorf("truncated delta token at offset %d", offset)
		}

		token := int32(binary.LittleEndian.Uint32(data[offset : offset+4]))
		offset += 4

		if token == 0 {
			// end of stream
			break
		}

		if token > 0 {
			// literal data: token is the length
			if offset+int(token) > len(data) {
				return fmt.Errorf("truncated literal data: need %d bytes at offset %d", token, offset)
			}
			offset += int(token)
		} else {
			// token reference: blockIndex = -(token+1)
			blockIndex := -(token + 1)
			if blockIndex < 0 || blockIndex >= sh.count {
				return fmt.Errorf("invalid block index %d (count=%d)", blockIndex, sh.count)
			}
		}
	}

	return nil
}

// sendFileChecksum computes and sends the final file checksum for verification.
func sendFileChecksum(w *mux.Writer, data []byte, s2length int) error {
	cksum := checksum2(data, s2length)
	return w.WriteMsg(mux.MsgData, cksum)
}
