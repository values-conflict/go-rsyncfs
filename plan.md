# Implementation Plan

Each task below is designed to be implementable and testable in a single focused session (~30-60 min). Tasks build incrementally; earlier tasks unblock later ones.

As tasks (and phases) are completed, ~~strikethrough~~ their titles (`### Task N -- do some stuff` -> `### ~~Task N -- do some stuff~~`; `## Phase N: do stuff` -> `## ~~Phase N: do stuff~~`).

## Phase 0: Foundation

### ~~Task 1 -- Multiplexed I/O layer (`protocol/mux/`)~~

**Goal:** Implement the binary multiplexed framing protocol as a standalone sub-package. This is genuinely reusable (any rsync implementation needs it) and cleanly separable from Client/Server logic.

**Files:** `protocol/mux/mux.go`, `protocol/mux/mux_test.go`

**API sketch:**

```go
// Writer wraps an io.Writer with multiplexed frame encoding.
type Writer struct{ w io.Writer }

func (w *Writer) WriteMsg(code uint8, payload []byte) error
func (w *Writer) Close() error // flush any buffered data

// Reader reads multiplexed frames from an io.Reader.
type Reader struct{ r io.Reader }

func (r *Reader) ReadMsg() (code uint8, payload []byte, err error)
```

**Key details:**

- Frame header: `((MPLEX_BASE + msgCode) << 24) | length` where MPLEX_BASE=7. Written as a **little-endian** uint32 on the wire (confirmed from upstream source via `SIVAL()` in `byteorder.h`).
- Payload length in bits 23-0 (max ~16MB frames)
- Message codes defined as constants matching upstream `enum msgcode`

**Tests:**

- Round-trip all message types through `io.Pipe()`
- Verify frame header encoding/decoding matches spec exactly
- Edge cases: zero-length payload, max-size payload, truncated headers

### Task 2 -- Integer wire encodings (`protocol/wireint.go`)

**Goal:** Implement the variable-length integer formats used in protocol ≥ 30 (varint, varlong) and legacy longint for older protocols.

**Files:** `protocol/wireint.go`, `protocol/wireint_test.go`

**API sketch:**

```go
func WriteVarint(w io.Writer, v int32) error
func ReadVarint(r io.Reader) (int32, error)
func WriteVarlong(w io.Writer, v int64, minBytes byte) error
func ReadVarlong(r io.Reader, minBytes byte) (int64, error)
```

**Key details:**

- All integer encodings in rsync use **little-endian** on the wire (confirmed from upstream `SIVAL()`/`SIVAL64()` macros).

**Tests:**

- Round-trip all values across the full range of each encoding
- Verify wire format matches upstream source code exactly
- Cross-check: varint(0), varint(-1), varint(max-int32), etc. produce expected byte sequences

### Task 3 -- Greeting exchange (`protocol/greet.go`)

**Goal:** Implement Phase 1 of the rsync daemon protocol (text-based greeting negotiation).

**Files:** `protocol/greet.go`, `protocol/greet_test.go`

**API sketch:**

```go
type Greeting struct {
	Version     int
	SubProtocol byte
	Digests     []string // e.g. ["md5", "md4"]
}

func ParseGreeting(line string) (*Greeting, error)
func (g *Greeting) String() string // formats back to "@RSYNCD: V.S d1 d2\n"

// Negotiate picks the best common version and digest between two greetings.
func Negotiate(local, remote Greeting) (version int, subProtocol byte, digest string, err error)
```

**Tests:**

- Parse standard greeting formats (e.g. `@RSYNCD: 32.0 md5 md4\n`)
- Negotiation matrix: client v32 + server v30 → pick v30 with correct digest fallback
- Subprotocol mismatch causes version downgrade
- Error on malformed greeting lines

## Phase 1: Server (read-only, single module)

### Task 4 -- Server struct & `@ERROR` handling (`server.go`)

**Goal:** Define the `Server` type and implement error response formatting. This establishes the server's basic shape before any protocol logic.

**Files:** `server.go`, `server_test.go`

**API sketch:**

```go
type Server struct {
	// modules maps module name to its config + backing FS
	modules map[string]*ServerModule
}

type ServerModule struct {
	Name     string
	Comment  string
	FS       fs.FS
	ReadOnly bool
}

func (s *Server) AddModule(m *ServerModule) error
```

**Tests:**

- Adding/removing modules, duplicate name rejection
- `@ERROR:` line formatting matches upstream format exactly

### Task 5 -- Server: full handshake (`server-handshake.go`)

**Goal:** Implement the server-side connection handshake: greeting exchange → module selection → auth (if configured) → argument parsing. Returns control to caller when ready for data transfer, or an error at any point.

**Files:** `server-handshake.go`, `server-handshake_test.go`

**API sketch:**

```go
type HandshakeResult struct {
	Module  *ServerModule
	Version int
	Digest  string
}

// HandleConnection runs the full text-phase handshake on a single connection.
func (s *Server) HandleConnection(rw io.ReadWriter, opts HandleOptions) (*HandshakeResult, error)

type HandleOptions struct {
	LocalGreeting Greeting                                                // what version/digests we advertise
	AuthCallback  func(username string, challenge []byte) ([]byte, error) // nil = no auth required
}
```

**Tests:**

- Full handshake round-trip through `io.Pipe()` with a hand-written client greeting/module request
- Module listing (`#list`) returns correct tab-separated format + EXIT terminator
- Unknown module → `@ERROR: Unknown module`
- Auth challenge/response flow (with and without callback)
- Argument parsing: null-terminated (proto ≥ 30) vs newline-terminated

### Task 6 -- Server: file list generation (`server-flist.go`)

**Goal:** Walk the backing FS and emit a file list in rsync wire format. This is the server-side equivalent of `send_files()` for the listing phase.

**Files:** `server-flist.go`, `server-flist_test.go`

**API sketch:**

```go
// SendFileList walks rootFS starting at basePath and writes entries to w.
func sendFileList(w *mux.Writer, rootFS fs.FS, basePath string, version int) error
```

**Key details:**

- Xmit flags encoding varies by protocol version (varint for ≥ 32, byte+extended for 28-31, single byte < 28)
- Delta-encoded: mode/uid/gid/mtime/name reuse previous values when same
- End-of-list marker (`NDX_DONE` = -1) with compressed NDX encoding for proto ≥ 30

**Tests:**

- Walk a simple in-memory FS (use `testing/fstest.MapFS`) and verify wire output byte-for-byte against expected format
- Xmit flag reuse: consecutive files with same mode/uid/gid/mtime should skip those fields
- Symlink entries encode target correctly
- Empty directory produces correct end-of-list marker

### Task 7 -- Server: file data transfer (`server-transfer.go`)

**Goal:** Implement the server-side data sender: given a file, compute block checksums and handle delta requests from the client. This is the core of rsync's efficient transfer algorithm.

**Files:** `server-transfer.go`, `server-transfer_test.go`

**API sketch:**

```go
// SendFile sends one file via the multiplexed I/O layer using rsync's block checksum protocol.
func sendFile(w *mux.Writer, f fs.File, version int) error
```

**Key details:**

- Compute SumHead (block count, block length, remainder, checksum lengths)
- Send rolling checksums (sum1 = Adler-like, sum2 = MD4/MD5 depending on negotiated digest)
- Read delta map from client and transmit only mismatched blocks
- Send `MSG_SUCCESS` with file index when done

**Tests:**

- Full transfer of a known file through mux via io.Pipe() -- verify checksums match
- Zero-byte file (count=0, no data sent)
- File that matches perfectly on receiver side (no gaps to fill)
- File that differs entirely (all blocks transmitted)

## Phase 2: Client (`io/fs.FS` implementation)

### Task 8 -- Client struct & connection setup (`client.go`)

**Goal:** Define the `Client` type and implement connection establishment + handshake from the client side. This mirrors Task 5 but in reverse direction.

**Files:** `client.go`, `client_test.go`

**API sketch:**

```go
type Client struct {
	// config for connecting to a specific module or root mode
}

func NewClient(opts ...ClientOption) *Client

// Connect runs the handshake and returns an active session ready for FS operations.
func (c *Client) Connect(rw io.ReadWriter) (*Session, error)

type Session struct {
	client  *Client
	rw      io.ReadWriter
	version int
	// ... mux readers/writers derived from rw
}
```

**Tests:**

- Client connects to our Server (Task 5) through `io.Pipe()` -- full handshake round-trip
- Version negotiation works correctly in both directions
- Module listing via client returns expected results

### Task 9 -- Client: `Open` implementation (`client-open.go`)

**Goal:** Implement `fs.FS.Open` for the client side. Opening a file triggers the server-side data transfer protocol (Tasks 6 + 7).

**Files:** `client-open.go`, `client-open_test.go`

**API sketch:**

```go
func (s *Session) Open(name string) (fs.File, error)
```

**Key details:**

- For directories: request file list from server, parse wire format into directory entries
- For regular files: trigger data transfer protocol -- send checksum header, read delta map and data blocks through mux layer
- The returned `fs.File` implements both `Read()` (for regular files) and `Readdir()` (for directories)

**Tests:**

- Open a file served by our Server → content matches exactly
- Open a directory → ReadDir returns correct entries with FileInfo
- Symlink: Open resolves correctly, fs.ReadLink works if available
- Error cases: non-existent path, permission denied from server side

### Task 10 -- Cross-implementation tests (`cross_test.go`)

**Goal:** Integration tests connecting Client directly to Server through `io.Pipe()` with embedded test fixtures. Run `testing/fstest.TestFS` as additional validation.

**Files:** `cross_test.go`, plus any fixture files under `testdata/`

**Tests:**

- Full directory tree transfer: create a MapFS on server, open via client, verify all content matches byte-for-byte
- Symlinks preserved through the protocol
- Large file (> block size) transfers correctly with delta algorithm
- Empty directories handled without errors
- Run `testing/fstest.TestFS` against our Client+Server pair

## Phase 3: Integration & Polish

### Task 11 -- Upstream rsync integration tests (`integration_test.go`)

**Goal:** Tests that connect our library to the real `rsync` binary. Skipped with `-short` or when `rsync` is not found.

**Files:** `integration_test.go`

**Tests (client-side):**

- Start `rsync --daemon`, connect our Client, verify FS operations match expectations
- Use Unix sockets if possible to avoid port management

**Tests (server-side):**

- Our Server behind a stream, driven by real `rsync` client binary
- Verify transfers work correctly in both directions (pull from server)

**Process management:** All started rsync processes must be killed on test completion. No orphans.

### Task 12 -- Protocol version coverage (`version_test.go`)

**Goal:** Systematic tests across the supported protocol version range (20-32). Verify that negotiation, encoding differences, and feature gates work correctly per version.

**Files:** `version_test.go`

**Tests:**

- Matrix: client@vN × server@vM → correct negotiated version for all pairs in [20..32]
- Version-specific wire format tests (e.g., varint only appears ≥ 30, extended xflags ≥ 28)
- Fallback behavior when features are unavailable at lower versions

## Future Phases (not yet planned into tasks)

These are noted from goals.md but deferred until Phase 1-3 are complete:

- **Writability:** bidirectional transfers (push files to server, accept pushes on server)
- **Root mode:** modules as top-level directories in a single FS instance
- **Caching / partial transfer:** delta-transfer resume with cache layer
- **Compression:** `--compress` equivalent
- **Extended metadata:** hardlinks, ACLs, xattrs, nanosecond timestamps, device files
