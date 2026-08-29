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
	// Hard link identity: the link count and the device/inode pair
	// (proto < 30 wire data; the writer decides presence from the
	// protocol version and preserve_hard_links).
	Nlink int64
	Dev   int64
	Ino   int64
	// Checksum for always_checksum files
	Checksum []byte

	// Directory classification (upstream FLAG_TOP_DIR / FLAG_NO_CONTENT_DIR,
	// carried by XMIT_TOP_DIR / XMIT_NO_CONTENT_DIR).  Only meaningful for
	// directory entries: below proto 30 a TOP_DIR bit on a non-directory is
	// just a non-zero placeholder and is not read back as a flag.
	TopDir       bool // the transfer's root directory
	NoContentDir bool // proto >= 30: contents are not in this list
}

// FlistReader reads file list entries from a byte stream.
type FlistReader struct {
	r                 io.Reader
	version           int
	varintFlistFlags  bool
	hasAtimes         bool
	hasCrtimes        bool
	preserveUID       bool
	preserveGID       bool
	preserveDevices   bool
	preserveLinks     bool
	preserveHardlinks bool
	alwaysChecksum    bool
	incRecurse        bool
	lastMode          uint32
	lastUID           int32
	lastGID           int32
	lastMtime         int64
	lastAtime         int64
	lastRdevMajor     uint32
	lastName          string
	lastDir           int64 // proto 28-29 dev tracking
}

// NewFlistReader creates a new FlistReader.
func NewFlistReader(r io.Reader, version int, varintFlistFlags bool) *FlistReader {
	return &FlistReader{
		r:                 r,
		version:           version,
		varintFlistFlags:  varintFlistFlags,
		preserveUID:       true,
		preserveGID:       true,
		preserveDevices:   true,
		preserveLinks:     true,
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
		// For varint flags, consume the io_error value after the end marker.
		// For non-varint flags, io_error is sent separately (XMIT_IO_ERROR_ENDLIST).
		if r.varintFlistFlags {
			_, _ = ReadVarint(r.r) // io_error, typically 0
		}
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

	// file size: longint below proto 30, varlong30 from there on
	if r.version < 30 {
		entry.Size, err = ReadLongInt(r.r)
	} else {
		entry.Size, err = ReadVarlong(r.r, 3)
	}
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

	// directory classification (upstream sets these flags only for
	// S_ISDIR entries; on non-dirs the TOP_DIR bit is a non-zero
	// placeholder below proto 30)
	if (entry.Mode & 0o170000) == 0o040000 {
		entry.TopDir = xflags&XmitTopDir != 0
		entry.NoContentDir = xflags&XmitNoContentDir != 0
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
			// the full packed dev_t as a single int32
			if xflags&XmitSameRdevMajor == 0 { // XMIT_SAME_RDEV_pre28 shares bit 8
				v, err := ReadUint32(r.r)
				if err != nil {
					return nil, err
				}
				entry.RdevMajor = v >> 8
				entry.RdevMinor = v & 0xFF
			}
		} else if r.version < 30 {
			if xflags&XmitSameRdevMajor == 0 {
				v, err := ReadUint32(r.r)
				if err != nil {
					return nil, err
				}
				entry.RdevMajor = v
			} else {
				entry.RdevMajor = r.lastRdevMajor
			}
			if xflags&XmitRdevMinor8Pre30 != 0 {
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
			v, err := ReadVarint(r.r)
			if err != nil {
				return nil, err
			}
			entry.RdevMinor = uint32(v)
		}
	}

	// symlink target -- length is varint30: an int32 below proto 30, a
	// varint from there on
	isSymlink := (entry.Mode & 0o170000) == 0o120000
	if r.preserveLinks && isSymlink {
		var lenVal int32
		if r.version < 30 {
			v, e := ReadInt32(r.r)
			if e != nil {
				return nil, e
			}
			lenVal = v
		} else {
			v, e := ReadVarint(r.r)
			if e != nil {
				return nil, e
			}
			lenVal = v
		}
		if lenVal > 0 {
			targetData := make([]byte, lenVal)
			if _, err := io.ReadFull(r.r, targetData); err != nil {
				return nil, err
			}
			entry.LinkTarget = string(targetData)
		}
	}

	// hard link dev/ino (proto < 30).  Below proto 28 every regular
	// file carries the pair unconditionally; proto 28-29 only the
	// flagged entries do.
	// The dev is transmitted as-is (the 1-incremented convention only
	// exists in the >= 3.1 daemons' outgoing stream; the 2.6.x peers
	// that negotiate sub-30 protocols read it raw).
	isReg := (entry.Mode & 0o170000) == 0o100000
	readHlink := r.version < 30 &&
		((r.version < 28 && r.preserveHardlinks && isReg) ||
			(r.version >= 28 && xflags&XmitHlinked != 0))
	if readHlink {
		if r.version < 26 {
			v1, err := ReadUint32(r.r)
			if err != nil {
				return nil, err
			}
			v2, err := ReadUint32(r.r)
			if err != nil {
				return nil, err
			}
			entry.Dev = int64(v1)
			entry.Ino = int64(v2)
		} else {
			if xflags&XmitSameDevPre30 == 0 {
				entry.Dev, err = ReadLongInt(r.r)
				if err != nil {
					return nil, err
				}
			}
			entry.Ino, err = ReadLongInt(r.r)
			if err != nil {
				return nil, err
			}
		}
	}

	// checksum (always_checksum) -- proto < 28 sent a (null) checksum for
	// every entry, not just regular files
	if r.alwaysChecksum && ((entry.Mode&0o170000) == 0o100000 || r.version < 28) {
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
	w                 io.Writer
	version           int
	varintFlistFlags  bool
	hasAtimes         bool
	hasCrtimes        bool
	preserveUID       bool
	preserveGID       bool
	preserveDevices   bool
	preserveLinks     bool
	preserveHardlinks bool
	alwaysChecksum    bool
	id0Names          bool
	lastMode          uint32
	lastUID           int32
	lastGID           int32
	lastMtime         int64
	lastAtime         int64
	lastRdevMajor     uint32
	lastDev           int64 // proto 28-29 hard link dev tracking
	lastName          string
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

// WriteEntry writes a file list entry, matching upstream send_file_entry()
// field order and delta-encoding rules.
func (w *FlistWriter) WriteEntry(e *FlistEntry) error {
	xflags := w.computeXflags(e)

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

	// file size: longint below proto 30, varlong30 from there on
	if w.version < 30 {
		if err := WriteLongInt(w.w, e.Size); err != nil {
			return err
		}
	} else {
		if err := WriteVarlong(w.w, e.Size, 3); err != nil {
			return err
		}
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
			// the full packed dev_t as a single int32
			if xflags&XmitSameRdevMajor == 0 { // XMIT_SAME_RDEV_pre28 shares bit 8
				if err := WriteUint32(w.w, packRdev(e.RdevMajor, e.RdevMinor)); err != nil {
					return err
				}
			}
		} else if w.version < 30 {
			if xflags&XmitSameRdevMajor == 0 {
				if err := WriteUint32(w.w, e.RdevMajor); err != nil {
					return err
				}
				w.lastRdevMajor = e.RdevMajor
			}
			if xflags&XmitRdevMinor8Pre30 != 0 {
				if err := writeByte(w.w, byte(e.RdevMinor)); err != nil {
					return err
				}
			} else {
				if err := WriteUint32(w.w, e.RdevMinor); err != nil {
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
			if err := WriteVarint(w.w, int32(e.RdevMinor)); err != nil {
				return err
			}
		}
	}

	// symlink target -- length is varint30: an int32 below proto 30, a
	// varint from there on
	isSymlink := (e.Mode & 0o170000) == 0o120000
	if w.preserveLinks && isSymlink {
		varLen := int32(len(e.LinkTarget))
		if w.version < 30 {
			if err := WriteInt32(w.w, varLen); err != nil {
				return err
			}
		} else {
			if err := WriteVarint(w.w, varLen); err != nil {
				return err
			}
		}
		if len(e.LinkTarget) > 0 {
			if _, err := w.w.Write([]byte(e.LinkTarget)); err != nil {
				return err
			}
		}
	}

	// hard link dev/ino (proto < 30).  Below proto 28 every regular
	// file carries the pair unconditionally (no flag byte); proto 28-29
	// carry it only for the flagged (multiple-link) entries, the flag
	// being computed in computeXflags.  The 2.6.x peers that negotiate
	// sub-30 protocols expect the dev number as-is (the 1-incremented
	// variant exists only in the outgoing stream of >= 3.1 daemons).
	isReg := (e.Mode & 0o170000) == 0o100000
	hlink := w.version < 30 &&
		((w.version < 28 && w.preserveHardlinks && isReg) ||
			(w.version >= 28 && xflags&XmitHlinked != 0))
	if hlink {
		if w.version < 26 {
			if err := WriteUint32(w.w, uint32(e.Dev)); err != nil {
				return err
			}
			if err := WriteUint32(w.w, uint32(e.Ino)); err != nil {
				return err
			}
		} else {
			if xflags&XmitSameDevPre30 == 0 {
				if err := WriteLongInt(w.w, e.Dev); err != nil {
					return err
				}
			}
			if err := WriteLongInt(w.w, e.Ino); err != nil {
				return err
			}
		}
	}

	// checksum (always_checksum) -- proto < 28 sent a (null) checksum for
	// every entry, not just regular files
	if w.alwaysChecksum && ((e.Mode&0o170000) == 0o100000 || w.version < 28) {
		csum := e.Checksum
		if len(csum) == 0 {
			csum = make([]byte, 16)
		}
		if _, err := w.w.Write(csum); err != nil {
			return err
		}
	}

	return nil
}

// SetID0Names enables the id-0 name extension (CF_ID0_NAMES negotiated):
// each id list terminator carries an (empty) name for id 0.
func (w *FlistWriter) SetID0Names(enabled bool) { w.id0Names = enabled }

// WriteIDLists writes the uid/gid name lists that follow the end marker when
// uid/gid preservation is on (upstream send_id_lists).  This writer never
// resolves names, so each preserved id space contributes only its terminator:
// a zero id (int32 for proto < 30, varint for proto >= 30) followed by an
// empty name when [FlistWriter.SetID0Names] is enabled.
func (w *FlistWriter) WriteIDLists() error {
	for _, preserve := range [2]bool{w.preserveUID, w.preserveGID} {
		if !preserve {
			continue
		}
		if w.version >= 30 {
			if err := WriteVarint(w.w, 0); err != nil {
				return err
			}
		} else {
			if err := WriteInt32(w.w, 0); err != nil {
				return err
			}
		}
		if w.id0Names {
			if err := writeByte(w.w, 0); err != nil {
				return err
			}
		}
	}
	return nil
}

// WriteIOErrorTrailer writes the int32 io_error value that terminates the
// file list for proto < 30 (upstream send_file_list).  For proto >= 30 the
// io_error travels as an MSG_IO_ERROR message and the trailer is a no-op.
func (w *FlistWriter) WriteIOErrorTrailer(ioError int) error {
	if w.version >= 30 {
		return nil
	}
	return WriteInt32(w.w, int32(ioError))
}

// WriteEndMarker writes the end-of-list marker (xflags = 0).
func (w *FlistWriter) WriteEndMarker() error {
	if w.varintFlistFlags {
		// varint flags: end marker (0) + io_error (0)
		if err := WriteVarint(w.w, 0); err != nil {
			return err
		}
		return WriteVarint(w.w, 0) // io_error = 0
	}
	return writeByte(w.w, 0)
}

// computeXflags calculates the delta-encoding xmit flags for a file entry,
// mirroring upstream send_file_entry() flag construction.
func (w *FlistWriter) computeXflags(e *FlistEntry) int {
	xflags := 0
	lastName := w.lastName
	isDir := (e.Mode & 0o170000) == 0o040000

	// the base flag of a directory (upstream send_file_entry):
	// proto >= 30 content dirs carry only the TOP_DIR bit when they are
	// the root; non-content dirs carry NO_CONTENT_DIR (or both, for an
	// implied parent); below proto 30 only TOP_DIR exists
	if isDir {
		if w.version >= 30 && e.NoContentDir {
			xflags |= XmitNoContentDir
			if e.TopDir {
				xflags |= XmitTopDir
			}
		} else if e.TopDir {
			xflags |= XmitTopDir
		}
	}

	if e.Mode == w.lastMode {
		xflags |= XmitSameMode
	}
	// uid/gid: the SAME flag is set whenever the field is not preserved at
	// all, or the value matches the previous entry (which must exist)
	if !w.preserveUID || (lastName != "" && e.UID == w.lastUID) {
		xflags |= XmitSameUID
	}
	if !w.preserveGID || (lastName != "" && e.GID == w.lastGID) {
		xflags |= XmitSameGID
	}
	if e.Mtime == w.lastMtime {
		xflags |= XmitSameTime
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

	// mod_nsec -- only exists in proto 31 and later
	if w.version >= 31 && e.ModNsec != 0 {
		xflags |= XmitModNsec
	}

	// atime -- only when atimes are tracked and entry is not a directory
	if w.hasAtimes && !isDir && e.Atime == w.lastAtime {
		xflags |= XmitSameAtime
	}

	// hard link flag (proto 28-29): carried by the multiple-link
	// non-directory entries; the dev number is omitted when it repeats
	// the previous one (upstream XMIT_SAME_DEV_pre30)
	if w.version >= 28 && w.version < 30 && w.preserveHardlinks && !isDir && e.Nlink > 1 {
		if e.Dev == w.lastDev {
			xflags |= XmitSameDevPre30
		} else {
			w.lastDev = e.Dev
		}
		xflags |= XmitHlinked
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
		isDir := (mode & 0o170000) == 0o040000
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
	isDir := (mode & 0o170000) == 0o040000
	if xflags == 0 {
		if isDir {
			xflags = XmitLongName
		} else {
			xflags = XmitTopDir
		}
	}
	return writeByte(w, byte(xflags))
}

// packRdev packs a device major/minor pair into the legacy 32-bit dev_t
// layout used by the pre-28 device encoding (major in the high bits).
func packRdev(major, minor uint32) uint32 {
	return (major << 8) | (minor & 0xFF)
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
