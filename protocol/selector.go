package protocol

import "io"

// Selector is a file transfer request sent by the generator.
type Selector struct {
	Ndx       int32  // file list index (NDxDone = -1)
	Iflags    int    // item flags (proto >= 29; for older, defaults to ItemTransfer|ItemMissingData)
	BasisType byte   // if ItemBasisTypeFollows set in Iflags
	Xname     string // if ItemXnameFollows set in Iflags
}

// ReadSelector reads a selector from r.
// For proto < 30, Ndx is read as int32 LE.  For proto >= 30, compressed NDX.
// For proto < 29, iflags defaults to ItemTransfer | ItemMissingData.
func ReadSelector(r io.Reader, ndx *NdxState, version int) (*Selector, error) {
	var ndxVal int32
	var err error

	if version >= 30 {
		ndxVal, err = ndx.ReadNdx(r)
	} else {
		ndxVal, err = ReadInt32(r)
	}
	if err != nil {
		return nil, err
	}

	iflags := ItemTransfer | ItemMissingData
	if version >= 29 && ndxVal >= 0 {
		val, err := ReadUint16(r)
		if err != nil {
			return nil, err
		}
		iflags = int(val)
	}

	sel := &Selector{
		Ndx:    ndxVal,
		Iflags: iflags,
	}

	if iflags&ItemBasisTypeFollows != 0 {
		var b [1]byte
		if _, err := io.ReadFull(r, b[:]); err != nil {
			return nil, err
		}
		sel.BasisType = b[0]
	}

	if iflags&ItemXnameFollows != 0 {
		xname, err := ReadVstring(r)
		if err != nil {
			return nil, err
		}
		sel.Xname = xname
	}

	return sel, nil
}

// WriteSelector writes a selector to w.
// For proto < 30, Ndx is written as int32 LE.  For proto >= 30, compressed NDX.
// For proto < 29, iflags is not written.
func WriteSelector(w io.Writer, ndx *NdxState, version int, sel *Selector) error {
	if version >= 30 {
		if err := ndx.WriteNdx(w, sel.Ndx); err != nil {
			return err
		}
	} else {
		if err := WriteInt32(w, sel.Ndx); err != nil {
			return err
		}
	}

	if version >= 29 && sel.Ndx >= 0 {
		if err := WriteUint16(w, uint16(sel.Iflags)); err != nil {
			return err
		}
	}

	if sel.Iflags&ItemBasisTypeFollows != 0 {
		if _, err := w.Write([]byte{sel.BasisType}); err != nil {
			return err
		}
	}

	if sel.Iflags&ItemXnameFollows != 0 {
		if err := WriteVstring(w, sel.Xname); err != nil {
			return err
		}
	}

	return nil
}
