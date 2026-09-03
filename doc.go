// Package rsyncfs implements the rsync daemon protocol as a Go library.  [Client] presents a remote rsync module as an [io/fs.FS], and [Server] serves local [io/fs.FS] modules over the protocol.  The library is transport-agnostic -- no CLI, no sockets, no encryption: callers wire up the transport (TCP, SSH, pipes, etc) themselves by handing each operation an [io.ReadWriter].
//
// It speaks protocol versions 20 through 32 (with headroom to 40), with optional pinning to a specific version.  Instances are constructed once and reused across operations, and the library never spawns goroutines.
//
//	// serve a local directory as rsync module "stuff"
//	srv, _ := rsyncfs.NewServer(&rsyncfs.ServerModule{
//	    Name: "stuff",
//	    FS:   os.DirFS("/path/to/stuff"),
//	})
//	srv.HandleConnection(conn)
//
//	// browse a remote module as a filesystem
//	sess, _ := rsyncfs.Client{Module: "stuff"}.Connect(conn2)
//	f, _ := sess.Open("subdir/file.txt")
//
// [BufPipe] connects a [Client] directly to a [Server] in memory, which makes tests trivial (plain [net.Pipe] and [io.Pipe] deadlock on the handshake, hence the bounded variant).
package rsyncfs
