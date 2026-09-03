# go-rsyncfs

The rsync daemon protocol, as a Go library.  `Client` presents a remote rsync module as an `io/fs.FS`; `Server` serves local `io/fs.FS` modules over the protocol.  Library-first: no CLI, no sockets, no encryption -- callers wire up transport (TCP, SSH, pipes, etc) themselves by handing each operation an `io.ReadWriter`.

Protocol versions 20 through 32 (headroom to 40), with optional pinning to a specific version.  Instances are constructed once and reused across operations; the library never spawns goroutines.

```go
// Serve a local directory as rsync module "stuff"
srv, _ := rsyncfs.NewServer(&rsyncfs.ServerModule{
    Name: "stuff",
    FS:   os.DirFS("/path/to/stuff"),
})
conn := netConn // caller-owned transport
srv.HandleConnection(conn)

// Browse a remote module as a filesystem
sess, _ := rsyncfs.Client{Module: "stuff"}.Connect(conn2) // or set ConnectFunc for lazy dialing
f, _ := sess.Open("subdir/file.txt")
```

Testing is trivial: `rsyncfs.BufPipe()` connects a `Client` directly to a `Server` in memory (plain `net.Pipe`/`io.Pipe` deadlock on the handshake, hence the bounded variant).

## Layout

- root package `rsyncfs` -- `Client`, `Server`, `Session`, the transport-agnostic API
- `protocol` -- wire protocol: greeting exchange, file list encoding, delta stream
- `protocol/mux` -- rsync's multiplexed I/O framing

## Status

v1: read-only transfers, metadata within what `io/fs.FS` covers (sizes, mtimes, symlinks), optional auth/ACL via callbacks.  Writability (push), hardlinks/xattrs/device files, partial-transfer caching, and on-the-wire compression are planned future work -- see `goals.md`.

Integration tests run against the real upstream `rsync` binaries (including old versions covering protocols 27-32 from `.upstream/old_versions/`); they skip under `go test -short`.
