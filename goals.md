- implement the rsync protocol in a pair of `Client` and `Server` structs, where the `Client` implements Go's `io/fs.FS` interface (eventually extended for writability) and the `Server` wraps an `io/fs.FS` (also eventually extended to writability)

- both implementations should support all protocol versions that `rsync` itself still does to the extent that is reasonably possible
  - optionally, a specific protocol version should be able to be specified on the struct, especially for testing specific version compatibility

- the "listen" loop for `Server` should live *outside* `Server` somehow with a clean interface for passing in a single connection/stream (which should also make testing the combination of the two simpler, if the `Client` can somehow pass one of those directly without opening sockets)
  - *neither* implementation should use goroutines directly without stopping to consult the user (tianon) about why they're necessary and confirming the use case -- any need for goroutines should be the caller's responsibility
  - it should be possible to hook up to an SSH implementation (such as https://pkg.go.dev/golang.org/x/crypto/ssh) and connect `Client` or serve `Server` over it (although this shouldn't be native functionality - it should just be minimal / modular enough for this to be *possible*, and we should document how)

- test fixtures with `go:embed`, cross-implementation tests to ensure they work together correctly
  - ie, `Client` connected directly to an instance of `Server`, and using `testing/fstest.TestFS` as an extra test above anything we write

- integration tests (skipped with `go test -short` or missing `rsync` binaries) should be added against each implementation using the `rsync` binary itself (starting `rsync` as a daemon for testing `Client` and starting it as a client for testing `Server`)
  - any test which starts an `rsync` process *must* correctly manage and kill it if the test finishes prematurely (or correctly/successfully)
  - if there is a way to run `rsync --daemon` such that it does not require a TCP socket / port binding, we should do that (how does `rsync` itself accomplish that when starting up a daemon over SSH?)

- for the `Client` implementation, it should be possible to specify a specific module, or to have some kind of "root" mode where the modules become the top-level folders and we can browse any of them from the same instance
  - in "root" mode, we should have *some* way to present the "comment" value for each module inside the filesystem representation -- exact shape of that TBD - the filesystem doesn't have a lot of great places for freeform data, but maybe we could use a filename that can't be a valid module name like `\t<module>\t<comment>` or something to present them?

- for the `Server` implementation, we should be able to provide any number of "module" to `io/fs.FS` mappings on a single instance, and each module should be able to be configured with an optional "comment" (so maybe some kind of `ServerModule` wrapping struct?)

- cache should be kept to a minimum / optional
  - perhaps that shows up in the form of an `os.Root` that can be written to?  (eventually anything that implements our writability-extended `fs.FS`, which should probably live in a separate module and have a wrapper for an `os.Root`)

- code should be organized into small files as much as possible / reasonable
  - if it makes sense to implement some parts of the code in a more modular way / as a library (for example, a dedicated library for the low-level protocol bits or shared helpers for client or server that might be useful to someone else *also* implementing an `rsync`-related project), those should live in separate sub-packages
  - packages like https://github.com/gokrazy/rsync *can* be considered as a dependency, but only if they truly meet the need and provide the primitives necessary to implement a filesystem (that project's README has a list of several other implementations we can check too)
