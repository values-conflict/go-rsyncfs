package protocol

import (
	"io"
)

// FlistEntry is a parsed file list entry.
type FlistEntry struct {
	Name       string
	Mode       uint32 // raw wire mode (S_IFDIR | 0755, etc)
	Size       int64
	Mtime      int64 // seconds
	ModNsec    int32 // nanoseconds (proto >= 31, 0 if not present)
	Atime      int64 // seconds (only if atimes enabled)
	UID        int32
	GID        int32
	UserName   string // only if XmitUserNameFollows
	GroupName  string // only if XmitGroupNameFollows
	RdevMajor  uint32 // for devices
	RdevMinor  uint32 // for devices
	LinkTarget string // for symlinks
	HlinkNdx   int32  // hard link target index (proto >= 30)
	// For proto 28-29 hard links:
	Dev int64
	Ino int64
	// Checksum for always_checksum files
	Checksum []byte
}

// FlistReader reads file list entries from a byte stream.
type FlistReader struct {
	r                  io.Reader
	version            int
	varintFlistFlags   bool
	hasAtimes          bool
	hasCrtimes         bool
	preserveUID        bool
	preserveGID        bool
	preserveDevices    bool
	preserveLinks      bool
	preserveHardlinks  bool
	alwaysChecksum     bool
	incRecurse         bool
	lastMode           uint32
	lastUID            int32
	lastGID            int32
	lastMtime          int64
	lastAtime          int64
	lastRdevMajor      uint32
	lastName           string
	lastDir            int64 // proto 28-29 dev tracking
}

// NewFlistReader creates a new FlistReader.
func NewFlistReader(r io.Reader, version int, varintFlistFlags bool) *FlistReader {
	return &FlistReader{
		r:                r,
		version:          version,
		varintFlistFlags: varintFlistFlags,
		preserveUID:      true,
		preserveGID:      true,
		preserveDevices:  true,
		preserveLinks:    true,
		preserveHardlinks: true,
	}
}

// SetAtimes enables reading atime fields.
func (r *FlistReader) SetAtimes(enabled bool) { r.hasAtimes = enabled }

// SetCrtimes enables reading crtime fields.
func (r *FlistReader) SetCrtimes(enabled bool) { r.hasCrtimes = enabled }

// SetPreserveUID controls whether UID fields are read.
func (r *FlistReader) SetPreserveUID(enabled bool) { r.preserveUID = enabled }

// SetPreserveGID controls whether GID fields are read.
func (r *FlistReader) SetPreserveGID(enabled bool) { r.preserveGID = enabled }

// SetPreserveDevices controls whether device fields are read.
func (r *FlistReader) SetPreserveDevices(enabled bool) { r.preserveDevices = enabled }

// SetPreserveLinks controls whether symlink targets are read.
func (r *FlistReader) SetPreserveLinks(enabled bool) { r.preserveLinks = enabled }

// SetPreserveHardlinks controls whether hard link fields are read.
func (r *FlistReader) SetPreserveHardlinks(enabled bool) { r.preserveHardlinks = enabled }

// SetAlwaysChecksum controls whether checksum fields are read.
func (r *FlistReader) SetAlwaysChecksum(enabled bool) { r.alwaysChecksum = enabled }

// SetIncRecurse enables incremental recurse mode.
func (r *FlistReader) SetIncRecurse(enabled bool) { r.incRecurse = enabled }

// ReadEntry reads the next file list entry. Returns io.EOF at end-of-list.
func (r *FlistReader) ReadEntry() (*FlistEntry, error) {
	xflags, err := r.readXflags()
	if err != nil {
		return nil, err
	}
	if xflags == 0 {
		return nil, io.EOF
	}

	entry := &FlistEntry{}

	// name: prefix match + suffix
	l1 := 0
	if xflags&XmitSameName != 0 {
		b, err := readByte(r.r)
		if err != nil {
			return nil, err
		}
		l1 = int(b)
	}

	var l2 int
	if xflags&XmitLongName != 0 {
		v, err := ReadVarint(r.r)
		if err != nil {
			return nil, err
		}
		l2 = int(v)
	} else {
		b, err := readByte(r.r)
		if err != nil {
			return nil, err
		}
		l2 = int(b)
	}

	suffix := make([]byte, l2)
	if l2 > 0 {
		if _, err := io.ReadFull(r.r, suffix); err != nil {
			return nil, err
		}
	}
	entry.Name = r.lastName[:l1] + string(suffix)
	r.lastName = entry.Name

	// hard link first_ndx (proto >= 30, XMIT_HLINKED without XMIT_HLINK_FIRST)
	if r.version >= 30 && (xflags&XmitHlinked != 0) && (xflags&XmitHlinkFirst == 0) {
		ndx, err := ReadVarint(r.r)
		if err != nil {
			return nil, err
		}
		entry.HlinkNdx = ndx
		if ndx >= 0 {
			// abbreviated entry -- copy from first_hlink
			// for now, just record the ndx; caller resolves
			return entry, nil
		}
	}

	// file size
	entry.Size, err = ReadVarlong(r.r, 3)
	if err != nil {
		return nil, err
	}

	// mtime
	if xflags&XmitSameTime == 0 {
		if r.version >= 30 {
			entry.Mtime, err = ReadVarlong(r.r, 4)
			if err != nil {
				return nil, err
			}
		} else {
			v, err := ReadUint32(r.r)
			if err != nil {
				return nil, err
			}
			entry.Mtime = int64(v)
		}
		r.lastMtime = entry.Mtime
	} else {
		entry.Mtime = r.lastMtime
	}

	// mod_nsec (proto >= 31)
	if xflags&XmitModNsec != 0 {
		entry.ModNsec, err = ReadVarint(r.r)
		if err != nil {
			return nil, err
		}
	}

	// mode
	if xflags&XmitSameMode == 0 {
		v, err := ReadUint32(r.r)
		if err != nil {
			return nil, err
		}
		entry.Mode = v
		r.lastMode = v
	} else {
		entry.Mode = r.lastMode
	}

	// atime
	if r.hasAtimes && (entry.Mode&0o170000) != 0o040000 && xflags&XmitSameAtime == 0 {
		entry.Atime, err = ReadVarlong(r.r, 4)
		if err != nil {
			return nil, err
		}
		r.lastAtime = entry.Atime
	} else if r.hasAtimes && (entry.Mode&0o170000) != 0o040000 {
		entry.Atime = r.lastAtime
	}

	// uid
	if r.preserveUID && xflags&XmitSameUID == 0 {
		if r.version >= 30 {
			entry.UID, err = ReadVarint(r.r)
			if err != nil {
				return nil, err
			}
			if xflags&XmitUserNameFollows != 0 {
				b, err := readByte(r.r)
				if err != nil {
					return nil, err
				}
				nameData := make([]byte, b)
				if b > 0 {
					if _, err := io.ReadFull(r.r, nameData); err != nil {
						return nil, err
					}
				}
				entry.UserName = string(nameData)
			}
		} else {
			v, err := ReadUint32(r.r)
			if err != nil {
				return nil, err
			}
			entry.UID = int32(v)
		}
		r.lastUID = entry.UID
	} else if r.preserveUID {
		entry.UID = r.lastUID
	}

	// gid
	if r.preserveGID && xflags&XmitSameGID == 0 {
		if r.version >= 30 {
			entry.GID, err = ReadVarint(r.r)
			if err != nil {
				return nil, err
			}
			if xflags&XmitGroupNameFollows != 0 {
				b, err := readByte(r.r)
				if err != nil {
					return nil, err
				}
				nameData := make([]byte, b)
				if b > 0 {
					if _, err := io.ReadFull(r.r, nameData); err != nil {
						return nil, err
					}
				}
				entry.GroupName = string(nameData)
			}
		} else {
			v, err := ReadUint32(r.r)
			if err != nil {
				return nil, err
			}
			entry.GID = int32(v)
		}
		r.lastGID = entry.GID
	} else if r.preserveGID {
		entry.GID = r.lastGID
	}

	// device rdev
	isDevice := (entry.Mode&0o170000) == 0o020000 || (entry.Mode&0o170000) == 0o060000
	if r.preserveDevices && isDevice {
		if r.version < 28 {
			if xflags&XmitSameRdevMajor == 0 { // XMIT_SAME_RDEV_pre28 shares bit 8
				v, err := ReadUint32(r.r)
				if err != nil {
					return nil, err
				}
				entry.RdevMajor = v
			}
		} else {
			if xflags&XmitSameRdevMajor == 0 {
				v, err := ReadVarint(r.r)
				if err != nil {
					return nil, err
				}
				entry.RdevMajor = uint32(v)
			} else {
				entry.RdevMajor = r.lastRdevMajor
			}
			if r.version >= 30 {
				v, err := ReadVarint(r.r)
				if err != nil {
					return nil, err
				}
				entry.RdevMinor = uint32(v)
			} else if xflags&XmitRdevMinor8Pre30 != 0 {
				b, err := readByte(r.r)
				if err != nil {
					return nil, err
				}
				entry.RdevMinor = uint32(b)
			} else {
				v, err := ReadUint32(r.r)
				if err != nil {
					return nil, err
				}
				entry.RdevMinor = v
			}
		}
	}

	// symlink target
	isSymlink := (entry.Mode&0o170000) == 0o120000
	if r.preserveLinks && isSymlink {
		lenVal, err := ReadVarint(r.r)
		if err != nil {
			return nil, err
		}
		if lenVal > 0 {
			targetData := make([]byte, lenVal)
			if _, err := io.ReadFull(r.r, targetData); err != nil {
				return nil, err
			}
			entry.LinkTarget = string(targetData)
		}
	}

	// hard link dev/ino (proto 28-29)
	if r.preserveHardlinks && (xflags&XmitHlinked != 0) && r.version < 30 {
		if r.version < 26 {
			v1, err := ReadUint32(r.r)
			if err != nil {
				return nil, err
			}
			v2, err := ReadUint32(r.r)
			if err != nil {
				return nil, err
			}
			entry.Dev = int64(v1) - 1 // undo the +1 increment
			entry.Ino = int64(v2)
		} else {
			if xflags&XmitSameDevPre30 == 0 {
				entry.Dev, err = ReadLongInt(r.r)
				if err != nil {
					return nil, err
				}
				entry.Dev-- // undo the +1 increment
			}
			entry.Ino, err = ReadLongInt(r.r)
			if err != nil {
				return nil, err
			}
		}
	}

	// checksum (always_checksum)
	if r.alwaysChecksum && (entry.Mode&0o170000) == 0o100000 {
		// flist_csum_len is typically 16 (MD4) or 16 (MD5)
		// we read 16 bytes as the standard checksum length
		csum := make([]byte, 16)
		if _, err := io.ReadFull(r.r, csum); err != nil {
			return nil, err
		}
		entry.Checksum = csum
	}

	return entry, nil
}

// readXflags reads the xmit flags according to protocol version.
// Returns 0 for end-of-list marker.
func (r *FlistReader) readXflags() (int, error) {
	if r.varintFlistFlags {
		v, err := ReadVarint(r.r)
		if err != nil {
			return 0, err
		}
		return int(v), nil
	}

	b, err := readByte(r.r)
	if err != nil {
		return 0, err
	}
	if b == 0 {
		return 0, nil // end-of-list
	}

	if r.version >= 28 && b&XmitExtendedFlags != 0 {
		high, err := readByte(r.r)
		if err != nil {
			return 0, err
		}
		return int(b) | int(high)<<8, nil
	}

	return int(b), nil
}

// FlistWriter writes file list entries to a writer.
type FlistWriter struct {
	w                  io.Writer
	version            int
	varintFlistFlags   bool
	hasAtimes          bool
	hasCrtimes         bool
	preserveUID        bool
	preserveGID        bool
	preserveDevices    bool
	preserveLinks      bool
	preserveHardlinks  bool
	alwaysChecksum     bool
	lastMode           uint32
	lastUID            int32
	lastGID            int32
	lastMtime          int64
	lastAtime          int64
	lastRdevMajor      uint32
	lastName           string
	lastDir            int64 // proto 28-29 dev tracking
}

// NewFlistWriter creates a new FlistWriter.
func NewFlistWriter(w io.Writer, version int, varintFlistFlags bool) *FlistWriter {
	return &FlistWriter{
		w:                 w,
		version:           version,
		varintFlistFlags:  varintFlistFlags,
		preserveUID:       true,
		preserveGID:       true,
		preserveDevices:   true,
		preserveLinks:     true,
		preserveHardlinks: true,
	}
}

// SetAtimes enables writing atime fields.
func (w *FlistWriter) SetAtimes(enabled bool) { w.hasAtimes = enabled }

// SetCrtimes enables writing crtime fields.
func (w *FlistWriter) SetCrtimes(enabled bool) { w.hasCrtimes = enabled }

// SetPreserveUID controls whether UID fields are written.
func (w *FlistWriter) SetPreserveUID(enabled bool) { w.preserveUID = enabled }

// SetPreserveGID controls whether GID fields are written.
func (w *FlistWriter) SetPreserveGID(enabled bool) { w.preserveGID = enabled }

// SetPreserveDevices controls whether device fields are written.
func (w *FlistWriter) SetPreserveDevices(enabled bool) { w.preserveDevices = enabled }

// SetPreserveLinks controls whether symlink targets are written.
func (w *FlistWriter) SetPreserveLinks(enabled bool) { w.preserveLinks = enabled }

// SetPreserveHardlinks controls whether hard link fields are written.
func (w *FlistWriter) SetPreserveHardlinks(enabled bool) { w.preserveHardlinks = enabled }

// SetAlwaysChecksum controls whether checksum fields are written.
func (w *FlistWriter) SetAlwaysChecksum(enabled bool) { w.alwaysChecksum = enabled }

// WriteEntry writes a file list entry.
func (w *FlistWriter) WriteEntry(e *FlistEntry) error {
	xflags := computeXflags(e, w.lastMode, w.lastUID, w.lastGID, w.lastMtime, w.lastAtime, w.lastRdevMajor, w.lastName, w.hasAtimes)

	// write xflags
	if err := writeXflags(w.w, xflags, e.Mode, w.version, w.varintFlistFlags); err != nil {
		return err
	}

	// name: prefix match + suffix
	l1 := commonPrefixLen(w.lastName, e.Name)
	l2 := len(e.Name) - l1

	if xflags&XmitSameName != 0 {
		if err := writeByte(w.w, byte(l1)); err != nil {
			return err
		}
	}
	if xflags&XmitLongName != 0 {
		if err := WriteVarint(w.w, int32(l2)); err != nil {
			return err
		}
	} else {
		if err := writeByte(w.w, byte(l2)); err != nil {
			return err
		}
	}
	if l2 > 0 {
		if _, err := w.w.Write([]byte(e.Name[l1:])); err != nil {
			return err
		}
	}
	w.lastName = e.Name

	// hard link first_ndx (proto >= 30, abbreviated)
	if w.version >= 30 && (xflags&XmitHlinked != 0) && (xflags&XmitHlinkFirst == 0) && e.HlinkNdx >= 0 {
		if err := WriteVarint(w.w, e.HlinkNdx); err != nil {
			return err
		}
		// abbreviated entry -- skip rest
		return nil
	}

	// file size
	if err := WriteVarlong(w.w, e.Size, 3); err != nil {
		return err
	}

	// mtime
	if xflags&XmitSameTime == 0 {
		if w.version >= 30 {
			if err := WriteVarlong(w.w, e.Mtime, 4); err != nil {
				return err
			}
		} else {
			if err := WriteUint32(w.w, uint32(e.Mtime)); err != nil {
				return err
			}
		}
		w.lastMtime = e.Mtime
	}

	// mod_nsec (proto >= 31)
	if xflags&XmitModNsec != 0 {
		if err := WriteVarint(w.w, e.ModNsec); err != nil {
			return err
		}
	}

	// mode
	if xflags&XmitSameMode == 0 {
		if err := WriteUint32(w.w, e.Mode); err != nil {
			return err
		}
		w.lastMode = e.Mode
	}

	// atime
	if w.hasAtimes && (e.Mode&0o170000) != 0o040000 && xflags&XmitSameAtime == 0 {
		if err := WriteVarlong(w.w, e.Atime, 4); err != nil {
			return err
		}
		w.lastAtime = e.Atime
	}

	// uid
	if w.preserveUID && xflags&XmitSameUID == 0 {
		if w.version >= 30 {
			if err := WriteVarint(w.w, e.UID); err != nil {
				return err
			}
			if xflags&XmitUserNameFollows != 0 {
				if err := writeByte(w.w, byte(len(e.UserName))); err != nil {
					return err
				}
				if len(e.UserName) > 0 {
					if _, err := w.w.Write([]byte(e.UserName)); err != nil {
						return err
					}
				}
			}
		} else {
			if err := WriteUint32(w.w, uint32(e.UID)); err != nil {
				return err
			}
		}
		w.lastUID = e.UID
	}

	// gid
	if w.preserveGID && xflags&XmitSameGID == 0 {
		if w.version >= 30 {
			if err := WriteVarint(w.w, e.GID); err != nil {
				return err
			}
			if xflags&XmitGroupNameFollows != 0 {
				if err := writeByte(w.w, byte(len(e.GroupName))); err != nil {
					return err
				}
				if len(e.GroupName) > 0 {
					if _, err := w.w.Write([]byte(e.GroupName)); err != nil {
						return err
					}
				}
			}
		} else {
			if err := WriteUint32(w.w, uint32(e.GID)); err != nil {
				return err
			}
		}
		w.lastGID = e.GID
	}

	// device rdev
	isDevice := (e.Mode&0o170000) == 0o020000 || (e.Mode&0o170000) == 0o060000
	if w.preserveDevices && isDevice {
		if w.version < 28 {
			if xflags&XmitSameRdevMajor == 0 { // XMIT_SAME_RDEV_pre28 shares bit 8
				if err := WriteUint32(w.w, e.RdevMajor); err != nil {
					return err
				}
			}
		} else {
			if xflags&XmitSameRdevMajor == 0 {
				if err := WriteVarint(w.w, int32(e.RdevMajor)); err != nil {
					return err
				}
				w.lastRdevMajor = e.RdevMajor
			}
			if w.version >= 30 {
				if err := WriteVarint(w.w, int32(e.RdevMinor)); err != nil {
					return err
				}
			} else if xflags&XmitRdevMinor8Pre30 != 0 {
				if err := writeByte(w.w, byte(e.RdevMinor)); err != nil {
					return err
				}
			} else {
				if err := WriteUint32(w.w, e.RdevMinor); err != nil {
					return err
				}
			}
		}
	}

	// symlink target
	isSymlink := (e.Mode&0o170000) == 0o120000
	if w.preserveLinks && isSymlink {
		if err := WriteVarint(w.w, int32(len(e.LinkTarget))); err != nil {
			return err
		}
		if len(e.LinkTarget) > 0 {
			if _, err := w.w.Write([]byte(e.LinkTarget)); err != nil {
				return err
			}
		}
	}

	// hard link dev/ino (proto 28-29)
	if w.preserveHardlinks && (xflags&XmitHlinked != 0) && w.version < 30 {
		if w.version < 26 {
			if err := WriteUint32(w.w, uint32(e.Dev+1)); err != nil { // +1 increment
				return err
			}
			if err := WriteUint32(w.w, uint32(e.Ino)); err != nil {
				return err
			}
		} else {
			if xflags&XmitSameDevPre30 == 0 {
				if err := WriteLongInt(w.w, e.Dev+1); err != nil { // +1 increment
					return err
				}
			}
			if err := WriteLongInt(w.w, e.Ino); err != nil {
				return err
			}
		}
	}

	// checksum (always_checksum)
	if w.alwaysChecksum && (e.Mode&0o170000) == 0o100000 {
		if len(e.Checksum) > 0 {
			if _, err := w.w.Write(e.Checksum); err != nil {
				return err
			}
		}
	}

	return nil
}

// WriteEndMarker writes the end-of-list marker (xflags = 0).
func (w *FlistWriter) WriteEndMarker() error {
	if w.varintFlistFlags {
		return WriteVarint(w.w, 0)
	}
	return writeByte(w.w, 0)
}

// computeXflags calculates the delta-encoding xmit flags for a file entry.
func computeXflags(e *FlistEntry, lastMode uint32, lastUID, lastGID int32, lastMtime, lastAtime int64, lastRdevMajor uint32, lastName string, hasAtimes bool) int {
	xflags := 0

	if e.Mode == lastMode {
		xflags |= XmitSameMode
	}
	if e.Mtime == lastMtime {
		xflags |= XmitSameTime
	}
	if e.UID == lastUID {
		xflags |= XmitSameUID
	}
	if e.GID == lastGID {
		xflags |= XmitSameGID
	}

	// name prefix matching
	l1 := commonPrefixLen(lastName, e.Name)
	l2 := len(e.Name) - l1
	if l1 > 0 {
		xflags |= XmitSameName
	}
	if l2 > 255 {
		xflags |= XmitLongName
	}

	// mod_nsec
	if e.ModNsec != 0 {
		xflags |= XmitModNsec
	}

	// atime -- only when atimes are tracked and entry is not a directory
	isDir := (e.Mode&0o170000) == 0o040000
	if hasAtimes && !isDir && e.Atime == lastAtime {
		xflags |= XmitSameAtime
	}

	// rdev major -- only for device files
	isDevice := (e.Mode&0o170000) == 0o020000 || (e.Mode&0o170000) == 0o060000
	if isDevice && e.RdevMajor == lastRdevMajor {
		xflags |= XmitSameRdevMajor
	}

	return xflags
}

// writeXflags encodes the xflags value according to the protocol version.
func writeXflags(w io.Writer, xflags int, mode uint32, version int, varintFlistFlags bool) error {
	if varintFlistFlags {
		// proto-32 compat: varint encoding, zero sent as sentinel
		if xflags == 0 {
			xflags = XmitExtendedFlags
		}
		return WriteVarint(w, int32(xflags))
	}

	if version >= 28 {
		// avoid sending zero (signals end-of-list)
		isDir := (mode&0o170000) == 0o040000
		if xflags == 0 && !isDir {
			xflags |= XmitTopDir
		}
		if (xflags&0xFF00 != 0) || xflags == 0 {
			xflags |= XmitExtendedFlags
			return WriteUint16(w, uint16(xflags))
		}
		return writeByte(w, byte(xflags))
	}

	// proto < 28: single byte, avoid zero
	isDir := (mode&0o170000) == 0o040000
	if xflags == 0 {
		if isDir {
			xflags = XmitLongName
		} else {
			xflags = XmitTopDir
		}
	}
	return writeByte(w, byte(xflags))
}

// commonPrefixLen returns the length of the common prefix between a and b, capped at 255.
func commonPrefixLen(a, b string) int {
	m := len(a)
	if len(b) < m {
		m = len(b)
	}
	if m > 255 {
		m = 255
	}
	for i := 0; i < m; i++ {
		if a[i] != b[i] {
			return i
		}
	}
	return m
}

// readByte reads a single byte from r.
func readByte(r io.Reader) (byte, error) {
	var b [1]byte
	if _, err := io.ReadFull(r, b[:]); err != nil {
		return 0, err
	}
	return b[0], nil
}

// writeByte writes a single byte to w.
func writeByte(w io.Writer, b byte) error {
	_, err := w.Write([]byte{b})
	return err
}
