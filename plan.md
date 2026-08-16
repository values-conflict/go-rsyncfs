# Implementation Plan

Each task below is designed to be implementable and testable in a single focused session (~30-60 min).  Tasks build incrementally; earlier tasks unblock later ones.

As tasks (and phases) are completed, ~~strikethrough~~ their titles (`### Task N -- do some stuff` -> `### ~~Task N -- do some stuff~~`; `## Phase N: do stuff` -> `## ~~Phase N: do stuff~~`).

**No "Known Gaps" allowed.**  If a task is incomplete, its title is not strikethrough.  If implementation reveals the plan is wrong, update the plan.  If a feature is out of scope for a task, split the task.  Never document a gap as accepted debt -- either finish the work or leave the task unmarked.

## Phase 0: `protocol/mux` -- multiplexed I/O framing

### ~~Task 1 -- Mux layer: transparent buffered I/O~~

**Goal:** Implement the binary multiplexed framing protocol as a standalone sub-package with transparent buffered I/O matching upstream's iobuf model.

**Files:** `protocol/mux/mux.go`, `protocol/mux/mux_test.go`

**API:** Per [api-design.md](api-design.md) -- `Reader` (Read, RecvMsg, ReadDataChunk), `Writer` (Write, Flush, SendMsg), exported message code constants.

**Key details:**

- Frame header: `((MPLEX_BASE + msgCode) << 24) | length` as little-endian uint32
- `Writer.Write()` accumulates raw bytes; `Writer.Flush()` sends MSG_DATA frame(s)
- `Writer.SendMsg()` flushes pending data before sending control message
- `Reader.Read()` transparently fetches MSG_DATA frames
- `Reader.RecvMsg()` reads non-DATA messages, skipping MSG_DATA data
- Buffer size: 32KB matching upstream's `IO_BUFFER_SIZE`
- Message code constants exported (MsgData, MsgSuccess, MsgError, etc.)

**Tests:**

- Round-trip buffered writes through `io.Pipe()`
- Verify batching: multiple small Writes produce single MSG_DATA frame on Flush
- Verify SendMsg flushes pending data before sending control message
- Verify Read transparently spans multiple MSG_DATA frames
- Verify RecvMsg correctly skips MSG_DATA data
- Edge cases: zero writes, buffer exactly at limit, split reads across frames

## Phase 1: `protocol` -- low-level wire protocol

### ~~Task 2 -- Protocol constants~~

**Goal:** Define all protocol-level constants and version gates in the `protocol` package.

**Files:** `protocol/constants.go`

**API:** Per [api-design.md](api-design.md) -- protocol version constants (Min, Old, Current, Max), compat flag bits (CF_INC_RECURSE through CF_ID0_NAMES), IO error constants (IOERRGeneral, IOERRVanished, IOERRDelLimit, IOERRValidMask), xmit flag bits (XMIT_TOP_DIR through XMIT_CRTIME_EQ_MTIME), item flag bits (ITEM_REPORT_ATIME through ITEM_MATCHED), special NDX values (NDX_DONE, NDX_FLIST_EOF).

**Tests:**

- Verify constant values match upstream source
- Verify IOERRValidMask covers exactly the defined bits

### ~~Task 3 -- Integer wire encodings~~

**Goal:** Implement variable-length integer formats (varint, varlong) for protocol ≥ 30, legacy longint for older protocols, fixed-width integers, and compressed NDX stateful encoding.

**Files:** `protocol/wireint.go`, `protocol/wireint_test.go`

**API:** Per [api-design.md](api-design.md) -- WriteVarint/ReadVarint, WriteVarlong/ReadVarlong, WriteLongInt/ReadLongInt, WriteInt32/ReadInt32, WriteUint16/ReadUint16, NdxState with WriteNdx/ReadNdx.

**Key details:**

- All multi-byte integers are little-endian
- Varint uses `int_byte_extra[]` lookup table (64 entries, indexed by first_byte/4)
- Compressed NDX: stateful delta encoding with prev_positive/prev_negative trackers; NDX_DONE = single byte 0x00

**Tests:**

- Round-trip all values across full range of each encoding
- Varint: 0, -1, max-int32, min-int32 produce expected byte sequences
- Compressed NDX: sequential indices encode efficiently, NDX_DONE = 0x00
- Cross-check wire format against upstream source code

### ~~Task 4 -- Greeting exchange~~

**Goal:** Implement the text-based greeting negotiation: parsing, formatting, defaults, and version/digest negotiation.

**Files:** `protocol/greet.go`, `protocol/greet_test.go`

**API:** Per [api-design.md](api-design.md) -- `Greeting` struct (Version, SubProtocol, Digests), `ParseGreeting`, `Greeting.String`, `Greeting.ApplyDefaults`, `Negotiate(client, server Greeting)`.

**Key details:**

- Format: `@RSYNCD: <version>.<sub> <digest1> <digest2>...`
- ApplyDefaults: version → 32, subProtocol → 0, Digests → ["md5", "md4"]
- Negotiate: pick lower version, subprotocol mismatch causes downgrade, digest negotiation follows client preference

**Tests:**

- Parse standard greeting formats
- Negotiation matrix: client v32 + server v30 → v30 with correct digest
- Subprotocol mismatch causes version downgrade
- ApplyDefaults fills zero-value fields idempotently

### Task 5 -- vstring encoding

**Goal:** Implement vstring (length-prefixed string) encoding used in the wire protocol.

**Files:** `protocol/vstring.go`, `protocol/vstring_test.go`

**API:** Per [api-design.md](api-design.md) -- `WriteVstring(w io.Writer, s string)`, `ReadVstring(r io.Reader) (string, error)`.

**Key details:**

- Format: `length : uint8` (or 2 bytes if high bit set) + `data : raw[length]`
- If `len & 0x80`: actual length = `(len & 0x7F) * 256 + next_byte`

**Tests:**

- Round-trip strings of various lengths (0, 1, 127, 128, 255, 256, 32767)
- Verify wire format: short strings use 1-byte length, long strings use 2-byte length
- Empty string round-trips correctly

### Task 6 -- Checksum algorithms

**Goal:** Implement rsync's rolling checksum (checksum1), strong hash (checksum2), and SumHead wire format.

**Files:** `protocol/checksum.go`, `protocol/checksum_test.go`

**API:** Per [api-design.md](api-design.md) -- `Checksum1(data []byte) uint32`, `Checksum2(data []byte, digest string, s2Length int, seed int32, seedFix bool) []byte`, `SupportedDigests() []string`, `SumHead` struct with `WriteSumHead`/`ReadSumHead`.

**Key details:**

- Checksum1: Adler-32-inspired rolling checksum, returns 4-byte LE result
- Checksum2: strong hash with seed; seedFix controls order (seed+data vs data+seed)
- SupportedDigests: returns list of algorithms this library supports
- SumHead: count, blength, s2length (proto ≥ 27), remainder -- all int32 LE

**Tests:**

- Checksum1: verify against known test vectors from upstream
- Checksum2: verify MD4, MD5 with both seed orders
- SumHead: round-trip with and without s2length (proto < 27 vs ≥ 27)
- Empty file (count=0) produces correct SumHead

### Task 7 -- Delta stream

**Goal:** Implement the delta stream format: streaming API (DeltaWriter/DeltaReader) for hot-path transfer and batch API (ParseDeltaStream/WriteDeltaStream) for testing and tools.

**Files:** `protocol/delta.go`, `protocol/delta_test.go`

**API:** Per [api-design.md](api-design.md) -- `DeltaWriter` (WriteLiteral, WriteMatch, WriteEnd), `DeltaReader` (ReadToken), `DeltaToken` struct, `ParseDeltaStream`, `WriteDeltaStream`.

**Key details:**

- Literal token: `0x01` prefix + length + data
- Match token: `0x00` prefix + 4-byte LE block index
- End marker: `0xFF`
- Batch functions implemented on top of streaming ones

**Tests:**

- Round-trip delta streams through io.Pipe()
- Mixed literal and match tokens
- Empty delta stream (just end marker)
- Large literal data
- Batch API matches streaming API output

### Task 8 -- File list I/O

**Goal:** Implement file list entry wire format with delta-encoded xmit flags, supporting all protocol versions (20-32) and both byte and varint xflags encoding.

**Files:** `protocol/flist.go`, `protocol/flist_test.go`

**API:** Per [api-design.md](api-design.md) -- `FlistEntry` struct, `FlistReader` (NewFlistReader, ReadEntry), `FlistWriter` (NewFlistWriter, WriteEntry, WriteEndMarker).

**Key details:**

- Xmit flags encoding: varint when CF_VARINT_FLIST_FLAGS, byte/shortint for proto 28+, single byte for proto < 28
- Delta-encoded fields: mode, uid, gid, mtime, name reuse previous values when same
- End-of-list: xflags=0 + NDX_DONE (compressed for proto ≥ 30)
- Protocol version gates: mod_nsec (≥ 31), hlink_ndx (≥ 30), long_name, etc.

**Tests:**

- Round-trip file entries with various modes (regular, directory, symlink, device)
- Xmit flag reuse: consecutive files with same attributes skip fields
- Varint xflags decoded correctly when CF_VARINT_FLIST_FLAGS set
- End-of-list marker correctly terminates reading
- Protocol version differences: proto 27 vs 30 vs 31 wire format

### Task 9 -- Selector wire format

**Goal:** Implement the selector (file transfer request) wire format used in the selector loop.

**Files:** `protocol/selector.go`, `protocol/selector_test.go`

**API:** Per [api-design.md](api-design.md) -- `Selector` struct (Ndx, Iflags, BasisType, Xname), `ReadSelector(r io.Reader, ndx *NdxState, version int)`, `WriteSelector(w io.Writer, ndx *NdxState, version int, sel *Selector)`.

**Key details:**

- Proto ≥ 30: compressed NDX via NdxState
- Proto < 30: int32 LE for NDX
- Proto ≥ 29: iflags as uint16 LE
- Proto < 29: iflags defaults to ITEM_TRANSFER | ITEM_MISSING_DATA
- Optional fields: BasisType (if ITEM_BASIS_TYPE_FOLLOWS), Xname (if ITEM_XNAME_FOLLOWS)

**Tests:**

- Round-trip selectors with various iflags combinations
- Compressed NDX state tracking across multiple selectors
- Proto version differences: selector format at proto 28 vs 30 vs 32

### Task 10 -- Argument parsing

**Goal:** Implement rsync command-line argument reading/writing and client_info feature flag extraction.

**Files:** `protocol/args.go`, `protocol/args_test.go`

**API:** Per [api-design.md](api-design.md) -- `ReadArgs(r io.Reader, version int) ([]string, error)`, `WriteArgs(w io.Writer, args []string, version int) error`, `ExtractClientInfo(args []string) string`, `ResolveCompatFlags(serverCaps int, clientInfo string) int`.

**Key details:**

- Proto ≥ 30: null-terminated arguments
- Proto < 30: newline-terminated arguments
- Double delimiter (null or newline) terminates the list
- First arg is always "."
- `e` flag contains client_info feature flags (letters like i, L, s, f, x, C, I, v, u)
- ResolveCompatFlags maps client_info letters to compat flag bits

**Tests:**

- Round-trip argument lists for both null and newline formats
- ExtractClientInfo correctly parses -e argument
- ResolveCompatFlags maps feature letters to correct flag bits
- Empty argument list (just ".")

### Task 11 -- Handshake primitives

**Goal:** Implement building blocks for the full handshake: greeting I/O, module selection, authentication, compat flags, algorithm negotiation, and error parsing.

**Files:** `protocol/handshake.go`, `protocol/handshake_test.go`

**API:** Per [api-design.md](api-design.md) -- `ReadGreeting`/`WriteGreeting`, `ReadModuleRequest`, `WriteModuleList`/`ModuleInfo`, `ReadAuthChallenge`/`WriteAuthChallenge`, `WriteAuthOK`, `ReadAuthResponse`/`WriteAuthResponse`, `ReadCompatFlags`/`WriteCompatFlags`, `Algorithms` struct, `DefaultAlgorithms`, `NegotiateAlgorithms`, `ReadChecksumSeed`/`WriteChecksumSeed`, `ParseError`/`WriteError`, `ExchangeVersion`.

**Key details:**

- Greeting I/O: reads/writes text greeting lines
- Auth: base64-encoded challenge and digest
- Compat flags: varint for proto ≥ 30, no-op for older
- Algorithm negotiation: vstring exchange for checksums and compressions when CF_VARINT_FLIST_FLAGS set
- DefaultAlgorithms: "md5" (proto ≥ 30) or "md4" (proto < 30), compression "zlib"
- ParseError: checks for @ERROR: prefix, returns nil if not an error

**Tests:**

- Greeting round-trip through io.Pipe()
- Auth challenge/response flow with base64 encoding
- Algorithm negotiation: both sides pick strongest mutual choice
- DefaultAlgorithms returns correct defaults per protocol version
- ParseError correctly identifies @ERROR: lines
- Compat flags exchange for proto ≥ 30

## Phase 2: Server

### Task 12 -- Server struct & module configuration

**Goal:** Define the `Server` type with module-to-filesystem mapping and auth callback support.

**Files:** `server.go`, `server_test.go`

**API:** Per [api-design.md](api-design.md) -- `Server` struct (Greeting field), `NewServer(mods ...*ServerModule) (*Server, error)`, `ServerModule` struct (Name, Comment, FS, ReadOnly, AuthCallback).

**Key details:**

- Server is constructed once with all modules; reusable across connections
- AuthCallback: per-module, returns expected raw digest bytes or error
- Zero-value Greeting uses defaults (version 32, digests ["md5", "md4"])

**Tests:**

- NewServer with multiple modules
- Duplicate module name rejection
- Module lookup by name
- AuthCallback verification flow

### Task 13 -- Server: HandleConnection

**Goal:** Implement the full server-side connection handling: handshake (greeting, module selection, auth, args, compat flags, algorithms, seed), file list transfer, selector loop, data transfer, final goodbye, and stats exchange.

**Files:** `server-handshake.go`, `server-handshake_test.go`

**API:** Per [api-design.md](api-design.md) -- `func (s *Server) HandleConnection(rw io.ReadWriter) error`.

**Key details:**

- Single entry point -- one call per connection, Server is stateless and reusable
- Composes `protocol/` primitives into full handshake flow (see api-design.md composition diagram)
- After handshake: switch to multiplexed I/O for daemon→client channel
- Send file list, then enter selector loop
- Selector loop: read selectors from raw connection (buffered), echo via muxWriter, send file data for TRANSFER selectors
- Final goodbye exchange and stats exchange
- Handshake timeout (default 60 seconds) on pre-transfer handshake
- Peer-supplied MSG_IO_ERROR values masked against IOERRValidMask

**Tests:**

- Full handshake round-trip through io.Pipe() with hand-written client
- Module listing (#list) returns correct tab-separated format + EXIT terminator
- Unknown module → @ERROR: Unknown module
- Auth challenge/response flow with and without callback
- Argument parsing: null-terminated (proto ≥ 30) vs newline-terminated
- Compat flags exchange
- Algorithm negotiation when CF_VARINT_FLIST_FLAGS set

### Task 14 -- Server: file list generation & data transfer

**Goal:** Implement the server-side file list walker and file data sender: walk backing FS to emit file list in rsync wire format, compute block checksums, and handle delta requests.

**Files:** `server-send.go`, `server-send_test.go`

**API:** Internal helpers composed within HandleConnection.

**Key details:**

- Walk backing fs.FS and emit file list entries via protocol.FlistWriter
- Xmit flags encoding: varint when CF_VARINT_FLIST_FLAGS, byte/shortint otherwise
- For data transfer: compute SumHead, send block checksums, read delta stream from client, transmit only mismatched blocks
- Send MSG_SUCCESS with file index when done
- Server reads selectors from raw connection (buffered I/O) and writes data through mux layer

**Tests:**

- Walk a MapFS and verify file list wire output
- Xmit flag reuse: consecutive files with same attributes skip fields
- Full file transfer through mux: verify checksums match
- Zero-byte file (count=0, no data sent)
- File that matches perfectly (no gaps to fill)
- File that differs entirely (all blocks transmitted)

## Phase 3: Client

### Task 15 -- Client struct & Connect

**Goal:** Define the `Client` config struct and implement connection establishment + handshake from the client side. Returns a `Session` ready for FS operations.

**Files:** `client.go`, `client-connect.go`, `client_test.go`

**API:** Per [api-design.md](api-design.md) -- `Client` struct (Module, Greeting, AuthUser, AuthResponse, ConnectFunc), `func (c Client) Connect(rw io.ReadWriter) (*Session, error)`, `func (c Client) OpenRoot() (*Session, error)`, `func PasswordAuth(password string) func(digest string, challenge []byte) ([]byte, error)`, `Session` struct.

**Key details:**

- Client is a plain config struct -- no constructor, value semantics, zero-value fields use defaults
- Connect runs full handshake (greeting, module, auth, args, compat flags, algorithms, seed) and returns Session
- Connect accepts nil rw when ConnectFunc is set
- OpenRoot returns Session for root mode (no live connection)
- PasswordAuth: standard password+challenge digest flow (md4 via golang.org/x/crypto/md4, md5 via crypto/md5)
- Session holds live connection state (muxReader, muxWriter, version, digest, etc.)

**Tests:**

- Client connects to Server through io.Pipe() -- full handshake round-trip
- Version negotiation works correctly in both directions
- Connect(nil) with ConnectFunc creates connection automatically
- Connect(nil) without ConnectFunc returns error
- PasswordAuth produces correct md4/md5 digests
- Compat flags: client sends -e argument with feature flags, reads server compat flags

### Task 16 -- Client: Session.Open

**Goal:** Implement `fs.FS.Open` for the client side. Opening a directory reads the file list; opening a file triggers the rsync data transfer protocol.

**Files:** `client-open.go`, `client-open_test.go`

**API:** Per [api-design.md](api-design.md) -- `func (s *Session) Open(name string) (fs.File, error)`.

**Key details:**

- Session implements fs.FS (not Client)
- For directories: read file list from server, parse with protocol.FlistReader, return directory entries
- For regular files: send selector (ItemTransfer|ItemMissingData), read sum_head + block checksums, write delta stream, read file data, verify checksum, send MSG_SUCCESS
- Phase exchange: send NDX_DONE after file list, read NDX_DONE from server
- Root mode: Session acts as config holder; each Open creates fresh connection via ConnectFunc
- Metadata mapping: rsync wire-format modes to Go os.FileMode
- Symlinks: return appropriate Mode() flags with target information

**Tests:**

- Open a file served by Server → content matches exactly
- Open a directory → ReadDir returns correct entries with FileInfo
- Symlink: Open resolves correctly, fs.ReadLink works
- Error cases: non-existent path, permission denied from server
- Root mode: ReadDir on root does live #list call (fresh connection)
- Root mode: entering a module directory opens separate connection to that module

## Phase 4: Integration & Polish

### Task 17 -- Cross-implementation tests

**Goal:** Integration tests connecting Client directly to Server through `net.Pipe()` with embedded test fixtures. Run `testing/fstest.TestFS` as additional validation.

**Files:** `cross_test.go`, `testdata/` (embedded fixtures)

**Tests:**

- Full directory tree transfer: MapFS on server, open via client, verify all content byte-for-byte
- Symlinks preserved through the protocol
- Large file (> block size) transfers correctly with delta algorithm
- Empty directories handled without errors
- Run `testing/fstest.TestFS` against Client+Server pair

### Task 18 -- Upstream rsync integration tests

**Goal:** Tests that connect our library to the real `rsync` binary. Skipped with `-short` or when `rsync` is not found.

**Files:** `integration_test.go`

**Tests (client-side):**

- Start `rsync --daemon`, connect our Client, verify FS operations match expectations
- Prefer Unix sockets to avoid port management

**Tests (server-side):**

- Our Server behind a stream, driven by real `rsync` client binary
- Verify transfers work correctly (pull from server)

**Process management:** All started rsync processes must be killed on test completion. No orphans.

### Task 19 -- Protocol version coverage

**Goal:** Systematic tests across the supported protocol version range (20-32). Verify negotiation, encoding differences, and feature gates work correctly per version.

**Files:** `version_test.go`

**Tests:**

- Matrix: client@vN × server@vM → correct negotiated version for all pairs in [20..32]
- Version-specific wire format tests (varint only ≥ 30, extended xflags ≥ 28, mod_nsec ≥ 31)
- Fallback behavior when features unavailable at lower versions
- Compat flag negotiation per version
- Compressed NDX only for proto ≥ 30
