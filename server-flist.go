package rsyncfs

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"io/fs"
	"time"

	"github.com/values-conflict/go-rsyncfs/protocol"
	"github.com/values-conflict/go-rsyncfs/protocol/mux"
)

// xmit flag bits matching upstream rsync.h
const (
	xmitTopDir        = 1 << 0
	xmitSameMode      = 1 << 1
	xmitExtendedFlags = 1 << 2 // proto >= 28
	xmitSameUID       = 1 << 3
	xmitSameGID       = 1 << 4
	xmitSameName      = 1 << 5
	xmitLongName      = 1 << 6
	xmitSameTime      = 1 << 7
	xmitSameRdevMajor = 1 << 8 // devices
	xmitNoContentDir  = 1 << 8 // dirs, proto >= 30
	xmitHlinked       = 1 << 9
	xmitHlinkFirst    = 1 << 12 // proto >= 30
	xmitModNsec       = 1 << 13 // proto >= 31
	xmitSameAtime     = 1 << 14
)

// end-of-list index markers
const (
	ndxDone     = -1
	ndxFlistEof = -2
)

// fileEntry holds the stat-like info extracted from an fs.DirEntry for wire encoding.
type fileEntry struct {
	name       string
	mode       fs.FileMode
	size       int64
	modTime    time.Time
	linkTarget string // non-empty for symlinks
}

// sendFileList walks rootFS starting at basePath and writes file list entries to w.
// It uses the negotiated protocol version to determine wire encoding.
// A varintFlistFlags value of true enables varint xflags encoding (proto-32 compat flag).
func sendFileList(w *mux.Writer, rootFS fs.FS, basePath string, version int, varintFlistFlags bool) error {
	entries, err := walkFS(rootFS, basePath)
	if err != nil {
		return fmt.Errorf("walk fs %s: %w", basePath, err)
	}

	buf := new(bytes.Buffer)

	var (
		lastMode  fs.FileMode
		lastUID   int32
		lastGID   int32
		lastMtime int64
		lastName  string
	)

	for _, entry := range entries {
		xflags := computeXflags(entry, lastMode, lastUID, lastGID, lastMtime, lastName)

		if err := writeXflags(buf, xflags, entry.mode, version, varintFlistFlags); err != nil {
			return fmt.Errorf("write xflags for %s: %w", entry.name, err)
		}

		// name encoding: prefix match + suffix
		l1 := commonPrefixLen(lastName, entry.name)
		l2 := len(entry.name) - l1

		if xflags&xmitSameName != 0 {
			buf.WriteByte(byte(l1))
		}

		if xflags&xmitLongName != 0 {
			// varint30 for long name suffix length
			if err := protocol.WriteVarint(buf, int32(l2)); err != nil {
				return err
			}
		} else {
			buf.WriteByte(byte(l2))
		}
		_, _ = buf.WriteString(entry.name[l1:])

		// file size
		if err := writeSize(buf, entry.size, version); err != nil {
			return fmt.Errorf("write size for %s: %w", entry.name, err)
		}

		// mtime
		modTimeSec := entry.modTime.Unix()
		if xflags&xmitSameTime == 0 {
			if err := writeMtime(buf, modTimeSec, version); err != nil {
				return fmt.Errorf("write mtime for %s: %w", entry.name, err)
			}
		}

		// mode
		if xflags&xmitSameMode == 0 {
			if err := writeMode(buf, entry.mode); err != nil {
				return fmt.Errorf("write mode for %s: %w", entry.name, err)
			}
		}

		// uid (always sent for non-matching, default 0)
		curUID := int32(0)
		if xflags&xmitSameUID == 0 {
			if err := writeUID(buf, curUID, version); err != nil {
				return fmt.Errorf("write uid for %s: %w", entry.name, err)
			}
		}

		// gid (always sent for non-matching, default 0)
		curGID := int32(0)
		if xflags&xmitSameGID == 0 {
			if err := writeGID(buf, curGID, version); err != nil {
				return fmt.Errorf("write gid for %s: %w", entry.name, err)
			}
		}

		// symlink target
		if entry.mode.Type() == fs.ModeSymlink && entry.linkTarget != "" {
			if err := writeSymlinkTarget(buf, entry.linkTarget); err != nil {
				return fmt.Errorf("write symlink target for %s: %w", entry.name, err)
			}
		}

		// update last values for delta encoding
		lastMode = entry.mode
		lastUID = curUID
		lastGID = curGID
		lastMtime = modTimeSec
		lastName = entry.name
	}

	// end-of-list marker: xflags = 0
	if err := writeEndMarker(buf); err != nil {
		return fmt.Errorf("write end marker: %w", err)
	}

	// compressed NDX_DONE
	if err := writeNdxDone(buf, version); err != nil {
		return fmt.Errorf("write ndx done: %w", err)
	}

	return w.WriteMsg(mux.MsgData, buf.Bytes())
}

// walkFS performs a pre-order walk of rootFS rooted at basePath, returning entries
// in sorted order (matching upstream rsync sort behavior).
func walkFS(rootFS fs.FS, basePath string) ([]fileEntry, error) {
	var entries []fileEntry

	err := fs.WalkDir(rootFS, basePath, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		info, err := d.Info()
		if err != nil {
			return err
		}

		// strip basePath prefix to match rsync behavior
		ename := path
		if basePath != "." && len(basePath) > 0 {
			if stripped, ok := bytes.CutPrefix([]byte(path), []byte(basePath)); ok {
				if len(stripped) > 0 && stripped[0] == '/' {
					stripped = stripped[1:]
				}
				ename = string(stripped)
			}
		}
		if ename == "" {
			ename = "."
		}

		entry := fileEntry{
			name:    ename,
			mode:    info.Mode(),
			size:    info.Size(),
			modTime: info.ModTime(),
		}

		// read symlink target if available via fs.ReadLinkFS (Go 1.25+)
		if info.Mode().Type() == fs.ModeSymlink {
			if rlfs, ok := rootFS.(interface {
				ReadLink(name string) (string, error)
			}); ok {
				target, err := rlfs.ReadLink(path)
				if err == nil {
					entry.linkTarget = target
				}
			}
		}

		entries = append(entries, entry)
		return nil
	})

	return entries, err
}

// computeXflags calculates the delta-encoding xmit flags for a file entry.
func computeXflags(entry fileEntry, lastMode fs.FileMode, lastUID, lastGID int32, lastMtime int64, lastName string) int {
	xflags := 0

	if entry.mode == lastMode {
		xflags |= xmitSameMode
	}
	if lastMtime == entry.modTime.Unix() {
		xflags |= xmitSameTime
	}
	// uid/gid default to 0, same as last after first entry
	if lastUID == 0 && lastGID == 0 {
		xflags |= xmitSameUID | xmitSameGID
	}

	// name prefix matching
	l1 := commonPrefixLen(lastName, entry.name)
	l2 := len(entry.name) - l1
	if l1 > 0 {
		xflags |= xmitSameName
	}
	if l2 > 255 {
		xflags |= xmitLongName
	}

	return xflags
}

// commonPrefixLen returns the length of the common prefix between a and b, capped at 255.
func commonPrefixLen(a, b string) int {
	max := len(a)
	if len(b) < max {
		max = len(b)
	}
	if max > 255 {
		max = 255
	}
	for i := 0; i < max; i++ {
		if a[i] != b[i] {
			return i
		}
	}
	return max
}

// writeXflags encodes the xflags value according to the protocol version.
func writeXflags(w io.Writer, xflags int, mode fs.FileMode, version int, varintFlistFlags bool) error {
	if varintFlistFlags {
		// proto-32 compat: varint encoding
		if xflags == 0 {
			xflags = xmitExtendedFlags // sentinel for zero
		}
		return protocol.WriteVarint(w, int32(xflags))
	}

	if version >= 28 {
		// avoid sending zero (signals end-of-list)
		if xflags == 0 && mode.Type() != fs.ModeDir {
			xflags |= xmitTopDir
		}
		if (xflags&0xFF00 != 0) || xflags == 0 {
			xflags |= xmitExtendedFlags
			return writeShortint(w, uint16(xflags))
		}
		_, err := w.Write([]byte{byte(xflags)})
		return err
	}

	// proto < 28: single byte, avoid zero
	if xflags == 0 {
		if mode.Type() == fs.ModeDir {
			xflags = xmitTopDir
		} else {
			xflags = xmitLongName
		}
	}
	_, err := w.Write([]byte{byte(xflags)})
	return err
}

// writeShortint writes a 16-bit unsigned integer in little-endian byte order.
func writeShortint(w io.Writer, x uint16) error {
	b := [2]byte{byte(x), byte(x >> 8)}
	_, err := w.Write(b[:])
	return err
}

// writeSize writes the file size in the appropriate format for the protocol version.
func writeSize(w io.Writer, size int64, version int) error {
	if version >= 30 {
		return protocol.WriteVarlong(w, size, 3)
	}
	return protocol.WriteLongInt(w, size)
}

// writeMtime writes the modification time in the appropriate format.
func writeMtime(w io.Writer, mtime int64, version int) error {
	if version >= 30 {
		return protocol.WriteVarlong(w, mtime, 4)
	}
	// proto < 30: int32 LE
	return writeInt32(w, int32(mtime))
}

// writeMode writes the file mode as a 32-bit little-endian integer.
// The mode is sent as-is (matching to_wire_mode which is identity on Linux).
func writeMode(w io.Writer, mode fs.FileMode) error {
	// convert fs.FileMode to Unix-style mode with type bits
	unixMode := uint32(mode.Perm())
	switch {
	case mode.IsDir():
		unixMode |= 0o040000 // S_IFDIR
	case mode.Type() == fs.ModeSymlink:
		unixMode |= 0o120000 // S_IFLNK
	default:
		unixMode |= 0o100000 // S_IFREG
	}
	return writeInt32(w, int32(unixMode))
}

// writeUID writes the uid in the appropriate format.
func writeUID(w io.Writer, uid int32, version int) error {
	if version >= 30 {
		return protocol.WriteVarint(w, uid)
	}
	return writeInt32(w, uid)
}

// writeGID writes the gid in the appropriate format.
func writeGID(w io.Writer, gid int32, version int) error {
	if version >= 30 {
		return protocol.WriteVarint(w, gid)
	}
	return writeInt32(w, gid)
}

// writeSymlinkTarget writes the symlink target length and data.
func writeSymlinkTarget(w io.Writer, target string) error {
	if err := protocol.WriteVarint(w, int32(len(target))); err != nil {
		return err
	}
	_, err := io.WriteString(w, target)
	return err
}

// writeEndMarker writes the end-of-list xflags = 0 marker.
func writeEndMarker(w io.Writer) error {
	_, err := w.Write([]byte{0})
	return err
}

// writeNdxDone writes the compressed NDX_DONE marker.
func writeNdxDone(w io.Writer, version int) error {
	if version < 30 {
		return writeInt32(w, ndxDone)
	}
	// compressed ndx: single byte 0x00 means NDX_DONE
	_, err := w.Write([]byte{0})
	return err
}

// writeInt32 writes a 32-bit signed integer in little-endian byte order.
func writeInt32(w io.Writer, x int32) error {
	var b [4]byte
	binary.LittleEndian.PutUint32(b[:], uint32(x))
	_, err := w.Write(b[:])
	return err
}
