package protocol

// Protocol version constants matching upstream rsync.h.
const (
	MinProtocolVersion     = 20 // oldest supported
	OldProtocolVersion     = 25 // threshold for "very old" warning
	CurrentProtocolVersion = 32 // latest
	MaxProtocolVersion     = 40 // forward-compatibility headroom
)

// Compat flag constants matching upstream compat.c.
const (
	CompatIncRecurse        = 1 << 0 // 'i' -- incremental file list
	CompatSymlinkTimes      = 1 << 1 // 'L' -- receiver can set symlink times
	CompatSymlinkIconv      = 1 << 2 // 's' -- sender converts symlink content
	CompatSafeFlist         = 1 << 3 // 'f' -- safe incremental file list
	CompatAvoidXattrOptim   = 1 << 4 // 'x' -- avoid xattr optimization
	CompatChksumSeedFix     = 1 << 5 // 'C' -- proper seed order (seed + data)
	CompatInplacePartialDir = 1 << 6 // 'I' -- inplace partial dir
	CompatVarintFlistFlags  = 1 << 7 // 'v' -- varint xmit flags
	CompatId0Names          = 1 << 8 // 'u' -- send id0 names
)

// IO error constants matching upstream rsync.h.
const (
	IOERRGeneral  = 1 << 0 // general I/O error
	IOERRVanished = 1 << 1 // file vanished during transfer
	IOERRDelLimit = 1 << 2 // delete limit reached

	// Mask of all defined IOERR_* bits.  Sanitize peer-supplied
	// MSG_IO_ERROR payloads against this to prevent a malicious peer
	// from setting arbitrary undefined bits in the local io_error.
	IOERRValidMask = IOERRGeneral | IOERRVanished | IOERRDelLimit
)

// Xmit flag constants matching upstream rsync.h.
const (
	XmitTopDir           = 1 << 0
	XmitSameMode         = 1 << 1
	XmitExtendedFlags    = 1 << 2 // proto >= 28 (replaces XmitSameRdevPre28)
	XmitSameUID          = 1 << 3
	XmitSameGID          = 1 << 4
	XmitSameName         = 1 << 5
	XmitLongName         = 1 << 6
	XmitSameTime         = 1 << 7
	XmitSameRdevMajor    = 1 << 8 // proto 28+ devices
	XmitNoContentDir     = 1 << 8 // proto 30+ dirs: contents not in this list
	XmitHlinked          = 1 << 9
	XmitSameDevPre30     = 1 << 10 // proto 28-29
	XmitUserNameFollows  = 1 << 10 // proto 30+
	XmitRdevMinor8Pre30  = 1 << 11 // proto 28-29
	XmitGroupNameFollows = 1 << 11 // proto 30+
	XmitHlinkFirst       = 1 << 12 // proto 30+
	XmitIoErrorEndlist   = 1 << 12 // proto 31+ with extended flags
	XmitModNsec          = 1 << 13 // proto 31+
	XmitSameAtime        = 1 << 14
	XmitCrtimeEqMtime    = 1 << 17
)

// Item flag constants matching upstream rsync.h.
const (
	ItemReportAtime      = 1 << 0
	ItemReportChange     = 1 << 1
	ItemReportSize       = 1 << 2 // regular files / time-fail for symlinks
	ItemReportTime       = 1 << 3
	ItemReportPerms      = 1 << 4
	ItemReportOwner      = 1 << 5
	ItemReportGroup      = 1 << 6
	ItemReportACL        = 1 << 7
	ItemReportXattr      = 1 << 8
	ItemReportCrtime     = 1 << 10
	ItemBasisTypeFollows = 1 << 11
	ItemXnameFollows     = 1 << 12
	ItemIsNew            = 1 << 13
	ItemLocalChange      = 1 << 14
	ItemTransfer         = 1 << 15 // request file data transfer
	ItemMissingData      = 1 << 16 // client has no local copy
	ItemDeleted          = 1 << 17
	ItemMatched          = 1 << 18
)

// Special NDX values matching upstream rsync.h.
const (
	NDxDone     int32 = -1 // all file lists complete
	NDXFlistEOF int32 = -2 // end of sub-list (inc_recurse)
)
