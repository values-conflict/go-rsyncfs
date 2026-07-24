# go-rsyncfs Goals

## Core Objective

Implement the rsync daemon protocol as a pair of `Client` and `Server` structs in Go, where:

- **`Client`** implements `io/fs.FS` (eventually extended for writability) to present a remote rsync module as a local filesystem
- **`Server`** wraps an `io/fs.FS` (also eventually writable) to serve one or more modules over the rsync protocol

Both implementations should be library-first: no CLI, no TCP handling, no encryption -- just clean primitives that callers can wire up however they need.

## Protocol Support

- Support all protocol versions that upstream `rsync` currently supports (`MIN_PROTOCOL_VERSION` **20** through current `PROTOCOL_VERSION` **32**, with forward-compatibility headroom to `MAX_PROTOCOL_VERSION` **40**)
- Optionally, a specific protocol version can be pinned on the struct (especially useful for testing compatibility against specific versions)

## Transport Model

Neither implementation handles transport directly. The caller is responsible for:

- TCP connection management (accepting/listening or dialing)
- Encryption (TLS, SSH tunneling, etc.)
- Any framing/proxy protocol handling

The library accepts a single `io.ReadWriter` (or similar stream interface -- TBD during implementation) per operation. This means:

- The "listen" loop for `Server` lives *outside* the library entirely; callers pass in one connection/stream at a time via a clean method call
- `Client` operations similarly receive a stream from the caller rather than opening sockets themselves
- This design makes testing trivial (pipe Client directly to Server without any network stack) and SSH integration straightforward (just wrap an `ssh.Session`'s stdin/stdout)

### Reusable Instances

A single `*Client` or `*Server` instance should be reusable across multiple operations. The struct holds configuration state (module selection, protocol version preferences, auth callbacks, etc.) while each operation receives its own stream from the caller. Exact shape of this is TBD during implementation but the invariant is: **construct once, operate many times**.

## Concurrency Constraints

- *Neither* implementation should use goroutines directly without stopping to consult Tianon about why they're necessary and confirming the use case
- Any need for concurrency should be the caller's responsibility (e.g., a caller managing multiple connections can spawn its own goroutines per connection)

## Authentication / Access Control

Authentication and access control are **optional features provided via callbacks** supplied by the user, not implemented natively in the library. This means:

- `Server` accepts optional callback functions for auth challenges (username/password verification), ACL checks (`hosts allow/deny` equivalent), etc.
- A caller implementing a full rsync daemon can wire up `secrets file` parsing and host matching externally
- If no callbacks are provided, the server operates in open/anonymous mode

## Module Configuration

### Client Side

It should be possible to:

1. Specify a specific module name (the common case -- connect to one module)
2. Use "root" mode where modules become top-level directories and any of them can be browsed from the same `FS` instance

In root mode, there should be some way to present each module's "comment" value inside the filesystem representation. Exact shape is TBD (the filesystem doesn't have great places for freeform metadata) -- possibilities include a filename that cannot be a valid module name like `<module>\t<comment>` (matching the `#list` protocol itself, sorting correctly in `ls`), or a virtual file at the root, etc.

### Server Side

A single `Server` instance should accept any number of module-to-`io/fs.FS` mappings. Each module is configured via some kind of `ServerModule` wrapping struct that includes:

- Module name
- The backing `io/fs.FS`
- Optional "comment" string
- Other minimal config surface (see below)

### Config Surface Philosophy

Model the *minimal* amount of rsyncd.conf directives necessary. Prefer callbacks where appropriate; only implement options directly in the library when we genuinely have to. Examples:

- `read only` -- direct option on ServerModule (maps cleanly to FS writability state)
- `auth users` / `secrets file` -- callback-based (caller parses secrets files)
- `hosts allow/deny` -- callback-based (caller does IP matching)
- `max connections` -- TBD, possibly callback or caller-managed (because "simultaneous connections" aren't something this library manages - it only gets one at a time)

## File Metadata Support

### Phase 1 (v1)

Support everything that `io/fs.FS` already has native interfaces for:

- Regular files with size and modification time (`fs.FileInfo`, `fs.FileInfoSys`)
- Directories with traversal support (`ReadDir`, `OpenFile` if writable)
- Symlinks (`fs.ModeSymlink` -- note that standard `io/fs.FS` **does** natively resolve symlinks since Go 1.25: https://pkg.go.dev/io/fs#ReadLinkFS)

### Future Phases

Eventually support everything rsync supports: hardlinks, ACLs, xattrs, extended timestamps (nanosecond precision), device files, sockets. This will likely require extending beyond `io/fs.FS` into a custom writable FS interface (see below).

## Writability / Bidirectional Transfers

Both Client and Server should eventually support bidirectional transfers:

- **Client as writer**: push files *to* the remote rsync daemon
- **Server as writer**: accept pushed files from clients

This will require extending `io/fs.FS` with writability. The extended writable FS interface should probably live in a separate module and include an `os.Root` wrapper (or similar). Details TBD during implementation but the goal is clear: both ends eventually read *and* write.

## Caching / Partial Transfer Support

Caching should be kept to a minimum and optional. The envisioned shape:

- A cache can be provided as something like an `os.Root` that files can be written to (eventually anything implementing our writable FS extension)
- **Partial transfer**: if we're configured with a cache on either end, *and* that cache has a partial/incomplete transfer of the file being pulled/read, we should utilize it for rsync's delta-transfer algorithm
- **Cache invalidation**: TBD (size limit? LRU eviction?)

This is explicitly a later-phase goal. The shape of partial transfer support when the primary API is `io/fs.FS`-shaped needs design work. We should have enough notes in code/comments that we don't forget about it, but v1 does not need to implement it.

## Compression / Checksums

- **Compression** (`--compress` equivalent): out-of-scope initially, in-scope eventually
  - Ideally this could be implemented externally (e.g., caller wraps the stream with `gzip.Reader`/`gzip.Writer`) but that may not be feasible given rsync's multiplexed I/O layer -- TBD during implementation
- **Checksum algorithms**: support whatever digest algorithms are negotiated per protocol version (MD4, MD5, SHA variants as advertised in greeting exchange)

## Package Organization

### Naming Conventions

- Filenames use hyphens: `foo-bar.go`, `bar-baz_test.go`
- Tests live alongside source with matching names: `client.go` + `client_test.go`
- Packages are lowercase, single word when possible; avoid `util`, `common`, `helpers`

### Expected Layout (TBD during implementation)

```text
go-rsyncfs/
  client.go          -- Client struct and io/fs.FS implementation entry points
  server.go          -- Server struct and module serving logic
  protocol/          -- low-level wire protocol parsing/writing (only if it makes impl/testing cleaner)
  ...                -- further splits as needed during implementation
```

The *main* API of the library is the `io/fs.FS` implementations. A dedicated `protocol` sub-package should only be created if it genuinely improves modularity or testability for the wire protocol bits (greeting exchange, multiplexed I/O layer, file list encoding, etc.). If those helpers are simpler inline in the root package, keep them there.

Code should generally be organized into small files as much as is reasonable. If parts of the code make sense as standalone libraries useful to others implementing rsync-related projects (e.g., a generic multiplexed I/O layer library), those belong in separate sub-packages.

### Third-Party Dependencies

Packages like `github.com/gokrazy/rsync` *can* be considered as dependencies, but only if they truly meet the need and provide primitives necessary to implement a filesystem. Do not add dependencies for convenience alone -- standard library first.

When exploring this, that repository's README contains a list of other potential (Go-based) libraries to consider.

## Testing Strategy

### Cross-Implementation Tests (with `go:embed` fixtures)

Test Client connected directly to Server instances using in-memory streams (no network). Use embedded test fixtures (`go:embed`) for file content verification. Run `testing/fstest.TestFS` as an additional validation layer above anything we write ourselves.

### Integration Tests Against Upstream `rsync` Binary

Skipped with `go test -short` or when `rsync` binaries are missing:

- **Client integration**: start `rsync --daemon`, connect our Client to it, verify filesystem operations match expectations
- **Server integration**: start our Server behind a stream, drive it with the real `rsync` client binary, verify transfers work correctly

### Process Management Requirements

Any test that starts an `rsync` process *must* correctly manage and kill it if the test finishes prematurely or successfully. No orphaned processes.

If there is a way to run `rsync --daemon` without TCP socket/port binding (e.g., via Unix sockets, pipes, or similar), prefer that over real network ports. Investigate how rsync itself accomplishes daemon mode over SSH for inspiration.

## Future Considerations (Not v1)

- Example CLI under `cmd/` for exercise/demonstration purposes
- Full ACL/xattr/hardlink/device/socket support beyond what `io/fs.FS` provides natively
- Partial transfer / delta-transfer resume via cache layer
- On-the-wire compression support
- Authentication/access control sub-modules (if demand arises, could be separate packages that provide callbacks to wire into the core library)
  - Configuration file / upstream syntax parsing sub-modules
- Use upstream's `old_versions` folder full of old `rsync_X.Y.Z` binaries to verify old protocol compatibility via our integration tests
