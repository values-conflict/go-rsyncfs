# API Design

Exported surface of the final go-rsyncfs library.  Designed from the protocol reference in [protocol.md](protocol.md), not from the current implementation.

## Package layout

```
rsyncfs/                   -- root package: Client, Server, FS integration
  client.go                -- Client config struct
  client_connect.go        -- Connect(), OpenRoot(), Session
  client_open.go           -- Session.Open(), fs.FS implementation
  client_send.go           -- push / send-side (future writability)
  server.go                -- Server, ServerModule
  server_handshake.go      -- HandleConnection(), handshake logic
  server_send.go           -- Daemon sender: file list, data transfer
  server_recv.go           -- Daemon receiver: accept pushed files (future)

protocol/                  -- low-level wire protocol (independently reusable)
  greet.go                 -- Greeting type, ParseGreeting, Negotiate
  wireint.go               -- Varint, Varlong, LongInt, compressed NDX
  vstring.go               -- vstring encoding (length-prefixed strings)
  checksum.go              -- checksum1 (rolling), checksum2 (strong), SumHead
  delta.go                 -- delta stream parsing and generation

protocol/mux/              -- multiplexed I/O framing (standalone library)
  mux.go                   -- Reader, Writer, message codes
```

The `protocol/` package implements every low-level wire format detail so that the root `rsyncfs/` package is just protocol orchestration plus `io/fs.FS` mapping.  The `protocol/` and `protocol/mux/` packages are usable independently by other rsync-shaped tools (batch-mode sync, rsync-over-custom-transport, protocol analysis, etc).

## `protocol/mux` -- multiplexed I/O framing

Standalone library with no dependency on anything else in rsyncfs.  It implements the MSG_DATA framing layer with transparent buffered I/O matching upstream's iobuf model.

### Constants

```go
// Message codes matching upstream rsync enum msgcode.
const (
    MsgData        uint8 = 0   // raw protocol data
    MsgErrorXfer   uint8 = 1   // transfer error message
    MsgInfo        uint8 = 2   // informational log message
    MsgError       uint8 = 3   // protocol-30 error
    MsgWarning     uint8 = 4   // protocol-30 warning
    MsgErrorSocket uint8 = 5   // socket error
    MsgLog         uint8 = 6   // log-only message
    MsgClient      uint8 = 7   // client log (never transmitted)
    MsgErrorUTF8   uint8 = 8   // UTF-8 conversion error
    MsgRedo        uint8 = 9   // reprocess flist index; payload: int32 LE ndx
    MsgStats       uint8 = 10  // transfer stats; payload: int64 total_read
    MsgIOError     uint8 = 22  // I/O error bitmask; payload: int32
    MsgIOTimeout   uint8 = 33  // timeout value; payload: int32 seconds
    MsgNoop        uint8 = 42  // keep-alive (proto 30 only); zero-length
    MsgErrorExit   uint8 = 86  // error exit signal; payload: 0 or int32
    MsgSuccess     uint8 = 100 // transfer complete; payload: int32 LE ndx
    MsgDeleted     uint8 = 101 // file deleted; payload: filename text
    MsgNoSend      uint8 = 102 // failed to open file; payload: int32 LE ndx
)
```

### Writer

Accumulates raw bytes via `Write()` and sends them as MSG_DATA frames on `Flush()`.  Control messages use `SendMsg()`, which flushes pending data first.

The buffer is bounded by a configurable size (default 32KB matching upstream's IO_BUFFER_SIZE).  When `Write()` would exceed this limit, the buffer is flushed automatically before more data is written.  This prevents unbounded memory growth during large file transfers and ensures the remote end receives data incrementally.

```go
// Writer wraps an io.Writer with multiplexed frame encoding.
// Normal data flows through Write() and is batched into MSG_DATA frames.
// Control messages use SendMsg().
type Writer struct{}

func NewWriter(w io.Writer) *Writer

// Write accumulates raw bytes for batching into MSG_DATA frames.
// Matches upstream write_buf() -- caller never sees mux headers.
// If the buffer is full, it is flushed before more data is written,
// keeping buffered memory bounded by the configured buffer size.
func (w *Writer) Write(p []byte) (n int, err error)

// Flush sends accumulated data as MSG_DATA frame(s).
// No-op if buffer is empty.
func (w *Writer) Flush() error

// SendMsg sends a non-DATA message.  Flushes pending buffered data first
// to ensure MSG_DATA frames precede control messages.
func (w *Writer) SendMsg(code uint8, payload []byte) error

// SetBufferSize sets the maximum buffer size before auto-flush.
// A value of 0 disables auto-flush (buffer grows unbounded).
// The default is 32KB, matching upstream's IO_BUFFER_SIZE.
func (w *Writer) SetBufferSize(size int)
```

### Reader

Transparently unwraps MSG_DATA frames via `Read()`.  Control messages use `RecvMsg()`.

```go
// Reader reads multiplexed frames from an io.Reader.
// Normal data flows through Read(), which transparently fetches MSG_DATA frames.
// Control messages use RecvMsg().
type Reader struct{}

func NewReader(r io.Reader) *Reader

// Read from the transparent buffer.  Fetches more MSG_DATA frames as needed.
// Returns an error (not io.EOF) when a non-DATA message arrives, so the
// caller can switch to RecvMsg().
func (r *Reader) Read(p []byte) (n int, err error)

// RecvMsg reads a non-DATA message, skipping any pending MSG_DATA data.
func (r *Reader) RecvMsg() (code uint8, payload []byte, err error)

// ReadDataChunk reads the next MSG_DATA frame payload as a single chunk.
// Useful for bounded reads (file lists) where the caller needs the frame boundary.
func (r *Reader) ReadDataChunk() ([]byte, error)
```

## `protocol` -- low-level wire protocol

The `protocol` package implements every wire encoding format and checksum algorithm.  It has no knowledge of `io/fs.FS` or high-level protocol flow, so it is usable by any rsync implementation.

### Greeting exchange

```go
// Greeting represents the rsync daemon greeting line.
type Greeting struct {
    Version     int      // protocol version (20-40)
    SubProtocol byte     // 0 = final release, nonzero = pre-release
    Digests     []string // supported auth digests in preference order
}

func ParseGreeting(line string) (*Greeting, error)
func (g *Greeting) String() string // "@RSYNCD: V.S d1 d2\n"

// ApplyDefaults fills zero-value fields: Version -> CurrentProtocolVersion,
// SubProtocol -> 0, Digests -> SupportedDigests().  Idempotent.
func (g *Greeting) ApplyDefaults()

// Negotiate picks the best common version and digest between two greetings.
// Client's greeting must be first argument (digest negotiation follows
// client preference).  Returns negotiated version, subprotocol, and digest.
func Negotiate(client, server Greeting) (version int, subProtocol byte, digest string, err error)
```

### Wire encodings

All multi-byte integers are little-endian on the wire.

```go
// --- Fixed-width integers ---

func WriteInt32(w io.Writer, v int32) error   // 4 bytes LE
func ReadInt32(r io.Reader) (int32, error)    // 4 bytes LE
func WriteUint16(w io.Writer, v uint16) error // 2 bytes LE (shortint)
func ReadUint16(r io.Reader) (uint16, error)  // 2 bytes LE (shortint)

// --- Variable-length integers (protocol >= 30) ---

func WriteVarint(w io.Writer, v int32) error
func ReadVarint(r io.Reader) (int32, error)
func WriteVarlong(w io.Writer, v int64, minBytes byte) error
func ReadVarlong(r io.Reader, minBytes byte) (int64, error)

// --- Legacy longint (protocol < 30) ---

func WriteLongInt(w io.Writer, v int64) error
func ReadLongInt(r io.Reader) (int64, error)

// --- vstring (length-prefixed string) ---

func WriteVstring(w io.Writer, s string) error
func ReadVstring(r io.Reader) (string, error)

// --- Compressed NDX (protocol >= 30) ---

// NdxState tracks delta-encoding state for compressed NDX.
// Zero value is valid: prevPositive=-1, prevNegative=1.
type NdxState struct{}

func NewNdxState() *NdxState

// WriteNdx writes a compressed NDX.  NDX_DONE (-1) is a single byte 0x00.
func (s *NdxState) WriteNdx(w io.Writer, ndx int32) error

// ReadNdx reads a compressed NDX.  Returns -1 for NDX_DONE.
func (s *NdxState) ReadNdx(r io.Reader) (int32, error)
```

### Protocol version constants

```go
const (
    MinProtocolVersion  = 20 // oldest supported
    OldProtocolVersion  = 25 // threshold for "very old" warning
    CurrentProtocolVersion = 32 // latest
    MaxProtocolVersion  = 40 // forward-compatibility headroom
)
```

### Compat flag constants

```go
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

### IO error constants

Bitmask values carried in `MSG_IO_ERROR`.  Peer-supplied values must be
masked with `IOERRValidMask` before use (rsync 3.5.0+).

```go
const (
    IOERRGeneral   = 1 << 0 // general I/O error
    IOERRVanished  = 1 << 1 // file vanished during transfer
    IOERRDelLimit  = 1 << 2 // delete limit reached

    // Mask of all defined IOERR_* bits.  Sanitize peer-supplied
    // MSG_IO_ERROR payloads against this to prevent a malicious peer
    // from setting arbitrary undefined bits in the local io_error.
    IOERRValidMask = IOERRGeneral | IOERRVanished | IOERRDelLimit
)
```

### Xmit flag constants

```go
const (
    XmitTopDir           = 1 << 0
    XmitSameMode         = 1 << 1
    XmitExtendedFlags    = 1 << 2 // proto >= 28 (replaces XmitSameRdevPre28)
    XmitSameUID          = 1 << 3
    XmitSameGID          = 1 << 4
    XmitSameName         = 1 << 5
    XmitLongName         = 1 << 6
    XmitSameTime         = 1 << 7
    XmitSameRdevMajor    = 1 << 8  // proto 28+ devices / proto 30+ dirs (NoContentDir)
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
```

### Item flag constants

```go
const (
    ItemReportAtime         = 1 << 0
    ItemReportChange        = 1 << 1
    ItemReportSize          = 1 << 2  // regular files / time-fail for symlinks
    ItemReportTime          = 1 << 3
    ItemReportPerms         = 1 << 4
    ItemReportOwner         = 1 << 5
    ItemReportGroup         = 1 << 6
    ItemReportACL           = 1 << 7
    ItemReportXattr         = 1 << 8
    ItemReportCrtime        = 1 << 10
    ItemBasisTypeFollows    = 1 << 11
    ItemXnameFollows        = 1 << 12
    ItemIsNew               = 1 << 13
    ItemLocalChange         = 1 << 14
    ItemTransfer            = 1 << 15 // request file data transfer
    ItemMissingData         = 1 << 16 // client has no local copy
    ItemDeleted             = 1 << 17
    ItemMatched             = 1 << 18
)
```

### Special NDX values

```go
const (
    NdxDone     int32 = -1 // all file lists complete
    NdxFlistEOF int32 = -2 // end of sub-list (inc_recurse)
)
```

### Checksum algorithms

```go
// SumHead is the checksum header sent before block checksums.
// All fields are int32 LE on the wire.
type SumHead struct {
    Count     int32 // block count (0 = empty file)
    BLength   int32 // block size
    S2Length  int32 // strong hash length (only if proto >= 27)
    Remainder int32 // final partial block size
}

func WriteSumHead(w io.Writer, sh SumHead, version int) error
func ReadSumHead(r io.Reader, version int) (SumHead, error)

// Checksum1 computes the rsync rolling checksum (Adler-32-inspired).
// Returns a 4-byte LE result.
func Checksum1(data []byte) uint32

// Checksum2 computes the strong hash with seed.
// When seedFix is true (CF_CHKSUM_SEED_FIX), seed is prepended: hash(seed + data).
// When seedFix is false, seed is appended: hash(data + seed).
// Returns the first s2Length bytes of the digest.
func Checksum2(data []byte, digest string, s2Length int, seed int32, seedFix bool) []byte

// SupportedDigests returns the list of checksum algorithms this library supports.
func SupportedDigests() []string
```

### Delta stream

Two APIs: streaming (hot path during transfer) and batch (testing, batch-mode tools).  The batch functions are implemented on top of the streaming ones.

```go
// --- Single-token streaming ---

// DeltaWriter sends delta tokens one at a time.
// The generator uses this during hash_search() to emit tokens as they are
// discovered, avoiding buffering the entire stream.
type DeltaWriter struct{}

func NewDeltaWriter(w io.Writer) *DeltaWriter

// WriteLiteral sends a literal data token (new bytes the sender doesn't have).
func (w *DeltaWriter) WriteLiteral(data []byte) error

// WriteMatch sends a match token ("reuse block N from the basis file").
func (w *DeltaWriter) WriteMatch(blockIdx int32) error

// WriteEnd sends the end-of-stream marker.
func (w *DeltaWriter) WriteEnd() error

// DeltaReader consumes delta tokens one at a time.
// The receiver uses this to reconstruct the file without buffering the
// full token list.
type DeltaReader struct{}

func NewDeltaReader(r io.Reader) *DeltaReader

// ReadToken returns the next token.  If data is non-nil it's a literal
// (len(data) > 0); if data is nil and blockIdx >= 0 it's a match reference;
// if isEnd is true the stream is complete.
func (r *DeltaReader) ReadToken() (data []byte, blockIdx int32, isEnd bool, err error)

// --- Batch API (convenience) ---

// DeltaToken represents a single command in the delta stream.
type DeltaToken struct {
    Literal []byte      // non-nil for literal data
    BlockIdx int32      // valid when Literal is nil (token reference)
}

// ParseDeltaStream reads all delta tokens from r until the end marker.
// Convenience wrapper around DeltaReader -- loads full stream into memory.
func ParseDeltaStream(r io.Reader) ([]DeltaToken, error)

// WriteDeltaStream writes delta tokens to w.
// Convenience wrapper around DeltaWriter.
func WriteDeltaStream(w io.Writer, tokens []DeltaToken) error
```

### File list wire format

```go
// FlistEntry is a parsed file list entry.
type FlistEntry struct {
    Name       string
    Mode       uint32  // raw wire mode (S_IFDIR | 0755, etc)
    Size       int64
    Mtime      int64   // seconds
    ModNsec    int32   // nanoseconds (proto >= 31, 0 if not present)
    Atime      int64   // seconds (only if atimes enabled)
    UID        int32
    GID        int32
    UserName   string  // only if XmitUserNameFollows
    GroupName  string  // only if XmitGroupNameFollows
    RdevMajor  uint32  // for devices
    RdevMinor  uint32  // for devices
    LinkTarget string  // for symlinks
    HlinkNdx   int32   // hard link target index (proto >= 30)
    // For proto 28-29 hard links:
    Dev  int64
    Ino  int64
    // Checksum for always_checksum files
    Checksum []byte
}

// FlistReader reads file list entries from a byte stream.
type FlistReader struct{}

func NewFlistReader(r io.Reader, version int, varintFlistFlags bool) *FlistReader
func (r *FlistReader) ReadEntry() (*FlistEntry, error) // returns io.EOF at end-of-list

// FlistWriter writes file list entries to a writer.
type FlistWriter struct{}

func NewFlistWriter(w io.Writer, version int, varintFlistFlags bool) *FlistWriter
func (w *FlistWriter) WriteEntry(e *FlistEntry) error
func (w *FlistWriter) WriteEndMarker() error // xflags=0 + NDX_DONE
```

### Selector wire format

```go
// Selector is a file transfer request sent by the generator.
type Selector struct {
    Ndx   int32  // file list index (NDX_DONE = -1)
    Iflags int   // item flags (proto >= 29; for older, defaults to ItemTransfer|ItemMissingData)
    // Optional fields (only present when corresponding iflags bits are set):
    BasisType byte    // if ItemBasisTypeFollows
    Xname     string  // if ItemXnameFollows
}

// ReadSelector reads a selector from r.
// For proto < 30, Ndx is read as int32 LE.  For proto >= 30, compressed NDX.
// For proto < 29, iflags defaults to ItemTransfer | ItemMissingData.
func ReadSelector(r io.Reader, ndx *NdxState, version int) (*Selector, error)

// WriteSelector writes a selector to w.
// For proto < 30, Ndx is written as int32 LE.  For proto >= 30, compressed NDX.
// For proto < 29, iflags is not written.
func WriteSelector(w io.Writer, ndx *NdxState, version int, sel *Selector) error
```

### Argument parsing

```go
// ReadArgs reads null-terminated (proto >= 30) or newline-terminated (proto < 30)
// rsync command-line arguments.  Terminated by double delimiter.
func ReadArgs(r io.Reader, version int) ([]string, error)

// WriteArgs writes arguments in the appropriate format for the protocol version.
func WriteArgs(w io.Writer, args []string, version int) error

// ExtractClientInfo extracts the client_info feature flags from the -e argument
// in the argument list.  Returns "" if no -e argument is found.
func ExtractClientInfo(args []string) string

// ResolveCompatFlags builds the server's compat flags based on its capabilities
// and the client's advertised feature flags from clientInfo.
func ResolveCompatFlags(serverCaps int, clientInfo string) int
```

### Handshake primitives

These are building blocks for the full handshake.  The root `rsyncfs/` package composes them.

```go
// ReadGreeting reads a greeting line from r.
func ReadGreeting(r io.Reader) (*Greeting, error)

// WriteGreeting writes a greeting line to w.
func WriteGreeting(w io.Writer, g Greeting) error

// ReadModuleRequest reads the module name (#list or actual module).
func ReadModuleRequest(r io.Reader) (string, error)

// WriteModuleList writes the tab-separated module listing followed by EXIT.
func WriteModuleList(w io.Writer, modules []ModuleInfo) error

type ModuleInfo struct {
    Name    string
    Comment string
}

// ReadAuthChallenge reads an AUTHREQD line and returns the base64-decoded challenge.
func ReadAuthChallenge(r io.Reader) ([]byte, error) // returns nil, nil if no auth required

// WriteAuthChallenge writes an AUTHREQD line with base64-encoded challenge.
func WriteAuthChallenge(w io.Writer, challenge []byte) error

// WriteAuthOK writes the @RSYNCD: OK response.
func WriteAuthOK(w io.Writer) error

// ReadAuthResponse reads the username and base64-encoded digest from the client.
func ReadAuthResponse(r io.Reader) (username string, digest []byte, err error)

// WriteAuthResponse writes the username and base64-encoded digest to the server.
func WriteAuthResponse(w io.Writer, username string, digest []byte) error

// ReadCompatFlags reads the compat flags varint from the server (proto >= 30).
// Returns 0 for proto < 30.
func ReadCompatFlags(r io.Reader, version int) (int, error)

// WriteCompatFlags writes the compat flags varint to the client (proto >= 30).
// No-op for proto < 30.
func WriteCompatFlags(w io.Writer, flags int, version int) error

// Algorithms holds the negotiated result for both algorithm categories.
type Algorithms struct {
    Checksum string // e.g. "md5"
    Compress string // e.g. "zlib" (empty if compression not negotiated)
}

// DefaultAlgorithms returns the default algorithms for the given protocol
// version without any wire exchange.  Checksum is "md5" (proto >= 30) or "md4"
// (proto < 30); compression is always "zlib".  No data is sent or received.
// Use this when CF_VARINT_FLIST_FLAGS is not set.
//
// Use together with NegotiateAlgorithms to handle both negotiated and
// non-negotiated paths:
//
//	var algos protocol.Algorithms
//	if compatFlags&protocol.CompatVarintFlistFlags != 0 {
//	    algos, err = protocol.NegotiateAlgorithms(rw, myChecksums, myCompressions)
//	} else {
//	    algos = protocol.DefaultAlgorithms(version)
//	}
func DefaultAlgorithms(version int) Algorithms

// NegotiateAlgorithms performs the full vstring exchange for both checksums
// and compression in a single call.  Both sides send their lists before
// reading the peer's lists to avoid deadlock.  Each side picks its own
// most-preferred algorithm that also appears in the peer's list (not the
// client's first acceptable choice).  When both sides emit their list in
// table (strongest-first) order, they converge on the strongest mutual
// choice; a peer that front-loads a weaker name only desyncs itself.
//
// myChecksums is always required.  myCompressions is only used when
// compression is enabled; pass nil to skip compression negotiation.
//
// This function is only called when CF_VARINT_FLIST_FLAGS is set.  When the
// flag is not set, use DefaultAlgorithms instead (no wire data exchanged).
func NegotiateAlgorithms(rw io.ReadWriter, myChecksums []string, myCompressions []string) (Algorithms, error)

// ReadChecksumSeed reads the 4-byte LE checksum seed.
func ReadChecksumSeed(r io.Reader) (int32, error)

// WriteChecksumSeed writes the 4-byte LE checksum seed.
func WriteChecksumSeed(w io.Writer, seed int32) error

// ParseError checks if line is an @ERROR: response.  Returns nil if not an
// error line, or an error with the message text otherwise.
// Callers invoke preemptively at any protocol point where an error is possible.
//
// Usage:
//
//	line, _ := readline(r)
//	if err := ParseError(line); err != nil {
//	    return nil, err
//	}
//	// handle success path
func ParseError(line string) error {
    msg, ok := strings.CutPrefix(line, "@ERROR: ")
    if !ok {
        return nil
    }
    return errors.New(msg)
}

// WriteError writes an @ERROR: line.
func WriteError(w io.Writer, msg string) error
```

### Binary version exchange (SSH/rsh transport)

```go
// ExchangeVersion performs the binary version exchange used by SSH/rsh transport.
// Sends our version, reads remote version, returns negotiated version.
func ExchangeVersion(rw io.ReadWriter, ourVersion int) (int, error)
```

## `rsyncfs` -- root package (Client / Server)

The main API of the library.  It builds on `protocol/` for all wire details and presents `io/fs.FS` interfaces for filesystem-shaped access.

### Server

```go
// Server represents an rsync daemon that serves one or more modules.
// Construct with NewServer.  A single Server handles multiple connections.
type Server struct {
    // Greeting is the greeting the server advertises on every connection.
    // Zero-value fields are filled by [protocol.Greeting.ApplyDefaults].
    Greeting protocol.Greeting
}

func NewServer(mods ...*ServerModule) (*Server, error)

// ServerModule wraps a backing filesystem with rsync module configuration.
type ServerModule struct {
    Name     string  // module name
    Comment  string  // displayed in #list
    FS       fs.FS   // backing filesystem
    ReadOnly bool    // true = reject push operations

    // AuthCallback verifies a username+challenge response for this module.
    // Returns the expected raw digest bytes, or an error to reject.
    // Nil means no authentication required for this module.
    // Matches rsync's per-module secrets file model.
    AuthCallback func(username string, challenge []byte) ([]byte, error)
}

// HandleConnection runs the full rsync daemon protocol on a single connection.
// The rw is the underlying transport (TCP socket, pipe, etc).
// Returns when the connection is complete or an error occurs.
//
// Handles: greeting exchange, module selection (#list or named module),
// authentication, argument parsing, compat flags, checksum negotiation,
// file list transfer, selector loop, data transfer, final goodbye.
func (s *Server) HandleConnection(rw io.ReadWriter) error
```

**Design notes for Server:**

- The server implements the daemon socket protocol exclusively.  SSH/rsh transport uses a different protocol flow (no greeting, no module selection, no auth, no argument transmission, binary version exchange instead) and is not supported.  If SSH/rsh support is needed, it would require a separate API.
- `HandleConnection` is the single entry point -- one call per connection.  The Server itself is stateless and reusable.
- `Server.Greeting` and `ServerModule.AuthCallback` are direct fields, not passed per-call.  This prevents per-connection config mutation and keeps the API surface minimal -- there's no `HandleOptions` struct to wire up on every call.
- Auth is per-module (via `ServerModule.AuthCallback`), matching rsync's per-module secrets file model.
- The server reads selectors from the raw connection (buffered I/O) and writes data through the mux layer.  This I/O mode split is internal to `HandleConnection`.
- The server enforces a handshake timeout (default 60 seconds) on the pre-transfer handshake (greeting, module selection, auth, argument reading).  This prevents an unauthenticated peer from holding a connection slot open indefinitely.  The timeout is internal and not currently configurable.
- Peer-supplied `MSG_IO_ERROR` values are masked against `IOERRValidMask` before use, preventing a malicious peer from setting arbitrary undefined bits in the local `io_error`.

### Client

```go
// Client configures a connection to an rsync daemon module.
// Construct directly with &Client{...}; zero-value fields use sensible defaults.
type Client struct {
    // Module is the rsync module name.  Empty string enables root mode
    // (modules as top-level directories, each operation gets its own connection).
    Module string

    // Greeting is sent to the server during the greeting exchange.
    // Zero-value fields are filled by [protocol.Greeting.ApplyDefaults].
    Greeting protocol.Greeting

    // AuthUser is the username for module authentication.
    // Empty string means anonymous access.
    AuthUser string

    // AuthResponse computes the auth response hash given the digest
    // algorithm and the server's challenge.  Returns raw hash bytes
    // (base64 encoding is handled by the library).
    // Nil means anonymous access.  Use PasswordAuth for standard flow.
    AuthResponse func(digest string, challenge []byte) ([]byte, error)

    // ConnectFunc creates a new connection to the rsync server.
    // The moduleName argument is the target module, or "" for #list.
    // Required for root mode.  Also used by Connect(nil).
    //
    // In root mode, each FS operation (listing modules, opening a
    // module) gets its own connection -- the server closes the
    // connection after #list, so a single persistent connection is
    // not possible.
    //
    // When used with [Client.Connect] and a nil io.ReadWriter,
    // ConnectFunc is called with the configured Module name to create
    // the connection.
    ConnectFunc func(moduleName string) (io.ReadWriter, error)
}

// PasswordAuth returns an AuthResponse function for the standard
// password+challenge digest flow: digest(password + challenge).
func PasswordAuth(password string) func(digest string, challenge []byte) ([]byte, error)
```

**Design notes for Client:**

- `Client` is a plain config struct -- no constructor, no hidden state.  Value semantics.
- Zero-value fields get defaults lazily during `Connect()` or `OpenRoot()`.
- `ConnectFunc` is the transport abstraction: the caller provides TCP dialing, SSH session creation, etc.

### Root mode: module comment representation

In root mode (`Module == ""`), modules are presented as top-level directories.  To preserve the module's "comment" metadata through the `fs.FS` interface (without type assertions or custom methods), each module emits **two** directory entries:

- `<module>` -- the canonical directory (the actual module, `Open` target)
- `.<module>\t<comment>` -- a **symlink** pointing to `<module>` (hidden by default, preserves `#list` tab-separated format, visible in `ls -la`, sorts predictably)

This keeps `ReadDir` → `Open` isomorphic: the symlink name is openable (follows to the module directory), and the canonical module name is always the direct `Open` target.

### Session

```go
// Session holds an active connection to an rsync daemon, ready for FS operations.
// In root mode, Session is a config holder (no live connection) --
// each FS operation creates its own connection via ConnectFunc.
//
// Session is not safe for concurrent use.  The rsync protocol is sequential:
// selectors are sent one-at-a-time through a single-phase loop, and the
// compressed NDX encoder maintains shared delta state.  The mux framing layer
// allows the daemon to interleave control messages with data, but does not
// provide request/response correlation or concurrent transfer support.
//
// For concurrent access (e.g., FUSE), use one of these patterns:
//
// Session pool: maintain a pool of Sessions (one per Connect() call) and
// dispatch operations across them.  Each Session handles its own sequential
// selector loop independently.  This allows multiple file transfers to
// proceed in parallel across connections, at the cost of handshake overhead
// per connection.
//
// Cache-backed single session: use a single Session for browsing (file list
// and directory traversal via inc_recurse), but cache transferred file data
// locally.  When a file is opened, trigger its transfer into a temp file or
// page cache, then serve subsequent reads from the cache.  This matches the
// rsync pull-once model and minimizes connection churn.
//
// For FUSE specifically: the kernel issues requests concurrently across
// worker threads.  Since rsync supports interleaved (but not concurrent)
// selectors, a single Session can serve readdir A, readdir B, open A/C
// sequentially.  However, two simultaneous file transfers will serialize.
// A session pool with per-connection caching is the most practical approach.
type Session struct{}

var _ fs.FS = (*Session)(nil)

// Connect runs the full handshake and returns an active session.
// If rw is nil and Client.ConnectFunc is set, ConnectFunc creates the connection.
// For root mode (Module == ""), use OpenRoot instead.
func (c Client) Connect(rw io.ReadWriter) (*Session, error)

// OpenRoot returns a Session for root mode (modules as top-level directories).
// Does not establish a live connection -- each FS operation gets its own.
// Requires Client.ConnectFunc to be set.
func (c Client) OpenRoot() (*Session, error)

// Open implements fs.FS.  Opens the named file or directory within the module.
// For directories, returns a file implementing ReadDirFile.
// For regular files, triggers the rsync data transfer protocol.
func (s *Session) Open(name string) (fs.File, error)
```

**Design notes for Session:**

- `Session` implements `fs.FS` (not `Client`).  The `Client` is immutable config; `Session` holds the live connection state.
- In root mode, `Session` holds no connection -- it delegates to `ConnectFunc` per operation.
- `Open` on a directory reads the file list from the server and returns directory entries.
- `Open` on a regular file sends a selector and reads the data through the delta transfer protocol.
- `Session` is not safe for concurrent use (the connection state is shared).  See the Session godoc for patterns (session pool, cache-backed single session).
- `io.ReaderAt` is intentionally **not** implemented on files returned by `Open`.  The rsync protocol has no random-access primitive -- a selector triggers a full sequential transfer.  Supporting `ReadAt` would require buffering the entire file in memory or on disk, which is a caching concern that belongs in the caller's cache layer, not the protocol library.

### Future: writability (push operations)

When bidirectional transfers are implemented, the Client gains push capability:

```go
// Send sends the source FS to the remote module.
// Each file in src is transferred to the server using the rsync push protocol.
// The rw is the underlying connection.  If nil, Client.ConnectFunc is used.
func (c Client) Send(rw io.ReadWriter, src fs.FS) error
```

And the Server gains a receive callback:

```go
type ServerModule struct {
    // ... existing fields ...

    // WriteFS is the backing filesystem for push operations.
    // If nil and ReadOnly is false, the module's FS must implement
    // a writable interface (TBD -- likely a separate module).
    WriteFS any // fs.FS for read, WritableFS for write
}
```

## Protocol flow composition

The root `rsyncfs/` package composes `protocol/` primitives into the full handshake.  Here is the composition for `Server.HandleConnection`:

```
1. s.Greeting.ApplyDefaults()
2. protocol.WriteGreeting(rw, s.Greeting)
3. protocol.ReadGreeting(rw)  -> clientGreeting
4. protocol.Negotiate(clientGreeting, s.Greeting) -> version, subProto, digest
5. protocol.ReadModuleRequest(rw) -> moduleName (#list or actual)
   if #list: protocol.WriteModuleList(rw, modules); return
6. if module.AuthCallback != nil: protocol.WriteAuthChallenge(rw, challenge)
                                  protocol.ReadAuthResponse(rw) -> username, responseHash
                                  verify via module.AuthCallback
                                  protocol.WriteAuthOK(rw)
   else: protocol.WriteAuthOK(rw)
7. protocol.ReadArgs(rw, version) -> args
8. if version >= 30: clientInfo = protocol.ExtractClientInfo(args)
                     compatFlags = protocol.ResolveCompatFlags(caps, clientInfo)
                     protocol.WriteCompatFlags(rw, compatFlags, version)
9. if CF_VARINT_FLIST_FLAGS: protocol.NegotiateAlgorithms(rw, checksums, compressions)
   else: protocol.DefaultAlgorithms(version)
10. protocol.WriteChecksumSeed(rw, seed)
11. <- switch to multiplexed I/O for daemon->client channel
12. sendFileList(muxWriter, module.FS, ".", version, varintFlags)
13. <- phase exchange + selector loop
    for each phase:
        read selectors from raw connection (buffered)
        for each selector with ItemTransfer:
            echo selector via muxWriter
            sendFile(muxReader, muxWriter, file, version, seed)
        read NDX_DONE from raw connection
        echo NDX_DONE via muxWriter
14. <- final goodbye exchange
15. <- stats exchange
```

And for `Client.Connect` + `Session.Open`:

```
Connect():
1. clientGreeting.ApplyDefaults()
2. protocol.WriteGreeting(rw, clientGreeting)
3. protocol.ReadGreeting(rw)  -> serverGreeting
4. protocol.Negotiate(clientGreeting, serverGreeting) -> version, subProto, digest
5. send module name as text line
6. read server response: AUTHREQD, OK, or ERROR
7. if AUTHREQD: compute auth response, protocol.WriteAuthResponse(rw, user, hash)
8. protocol.WriteArgs(rw, args, version)  -- --server --sender flags . e0v
9. if version >= 30: protocol.ReadCompatFlags(rw, version) -> compatFlags
10. if CF_VARINT_FLIST_FLAGS: protocol.NegotiateAlgorithms(rw, checksums, compressions)
    else: protocol.DefaultAlgorithms(version)
11. protocol.ReadChecksumSeed(rw) -> seed
12. <- switch to multiplexed I/O for daemon->client channel
13. return Session

Session.Open(name):
1. readFileList() -- read from muxReader, parse with protocol.FlistReader
2. phaseExchange() -- send NDX_DONE raw, read NDX_DONE from mux
3. find target entry by name
4. if directory: return directory entries from file list
5. if regular file:
   a. writeSelector(ndx, ItemTransfer|ItemMissingData) -- raw bytes
   b. read sum_head from muxReader
   c. read block checksums from muxReader
   d. write delta stream to raw connection -- raw bytes
   e. read file data from muxReader
   f. read file checksum from muxReader (verify)
   g. send MSG_SUCCESS with ndx via muxWriter
   h. return file with data
```

## Key design decisions

### Why `protocol/` is a separate package

The wire protocol details (varint encoding, compressed NDX, checksum algorithms, mux framing) are genuinely reusable.  A batch-mode rsync client, a protocol analyzer, or an rsync-over-gRPC shim can import `protocol/` without pulling in the `io/fs.FS` machinery.  The mux layer is even more standalone -- it's just a framing protocol.

### Why `Client` is a value struct, not a pointer

`Client` is immutable configuration.  Value semantics prevent accidental shared state between `Connect()` calls.  `Session` (the result of `Connect()`) holds the live connection state and is naturally a pointer.

### Why `HandleConnection` takes `io.ReadWriter`, not `net.Conn`

The caller controls transport.  Tests use `io.Pipe()` or `net.Pipe()`.  Production uses TCP.  SSH integration wraps an `ssh.Session`'s combined stdin/stdout.  No transport logic leaks into the library.

### Why auth is callback-based, not built-in

The library doesn't parse `secrets file` format or manage user databases.  The `AuthCallback` on the server and `AuthResponse` on the client let the caller plug in any authentication mechanism.  `PasswordAuth` provides the common case (standard rsync password+challenge digest).

### Why `ConnectFunc` instead of a dialer field

`ConnectFunc` is a simple function type that the caller implements.  It receives the module name (or `""` for `#list`) and returns an `io.ReadWriter`.  This is more flexible than embedding `net.Dialer` -- the caller can use TCP, Unix sockets, SSH sessions, or any custom transport.

### Why no goroutines in the library

The library is synchronous and connection-oriented.  `HandleConnection` blocks until the connection is done.  `Connect` blocks until the handshake completes.  The caller manages concurrency (one goroutine per connection, connection pooling, etc).  This matches the goals.md constraint and keeps the API simple.

### Why `Session` implements `fs.FS`, not `Client`

`Client` is config; `Session` is the live connection.  `fs.FS.Open()` operates on a connection -- it reads file lists and transfers data.  Separating config from connection state makes the API clean: construct `Client` once, call `Connect()` multiple times for independent sessions.

### Why the delta stream is in `protocol/`

The delta stream format (token references and literal data) is a pure wire protocol detail.  Exposing `ParseDeltaStream` and `WriteDeltaStream` lets callers implement custom delta strategies (partial transfer with a cache, or batch-mode sync with local file comparison).

### What's not in the protocol package

- **Filter list transfer** -- the filter rule format is complex and tightly coupled to rsync's exclude/include logic.  For now, this is handled in the root package.  If filter rules become important for tools beyond the FS use case, they can be extracted.
- **Stats exchange** -- simple varlong reads/writes, handled inline in the root package.
