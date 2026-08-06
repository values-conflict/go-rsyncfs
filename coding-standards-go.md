# Go Project Maintenance Guide

This document covers code style, design philosophy, testing, and idiomatic patterns for Go projects.  It is intended for human contributors and LLM assistants alike.  Where LLMs tend to default to patterns from other languages (Java, TypeScript, Python), those patterns are called out explicitly.

This document is intentionally project-generic so it can be reused across Go projects.  Keep examples and terminology generic -- domain-specific concepts belong in project-specific docs, not here.

## Philosophy

Go is opinionated by design.  Fighting the language costs more than it saves.  When something feels awkward in Go -- wrapping every return in a result type, adding a base class, injecting a logger into every struct -- that friction is usually a signal to reconsider the approach, not to push through.

**Simple over clever.** Code is read far more than it is written.  A boring solution that a newcomer can read in 30 seconds beats a sophisticated one that requires context to understand.

**Explicit over implicit.** Go has no magic.  No annotations that wire things together at runtime, no hidden inheritance chains, no framework lifecycle hooks.  When something happens, you can see it in the call stack.

## Code Style

### Formatting

All Go code is formatted with `gofmt` (or `goimports`).  There is no style debate about indentation, brace placement, or line length.  If a tool can decide it, let the tool decide it -- do not reconfigure it.

Run `goimports` (not just `gofmt`) to keep imports sorted and grouped:

1. Standard library
2. Third-party modules
3. Internal packages (same module)

### Naming

- **Packages**: lowercase, single word when possible (`store`, `httputil`, `auth`).  Avoid `util`, `common`, `helpers`, `misc` -- these are symptom packages that collect things with no real relationship.
- **Functions and methods**: `MixedCase` for exported, `mixedCase` for unexported.  Acronyms are all-caps: `HTTPClient`, `parseURL`, `userID`.
- **Interfaces**: name by behavior, not by noun.  `Reader`, `Stringer`, `Handler` -- not `IReader`, `ReaderInterface`, `AbstractHandler`.
- **Error variables**: `ErrNotFound`, `ErrTimeout` -- prefix with `Err`.
- **Boolean variables**: name as assertions: `isReady`, `hasMore`, `ok`.
- **Receiver names**: short, consistent, derived from the type name.  `c` for `Client`, `s` for `Server`.  Never `self` or `this`.
- **Loop variables**: short is fine in tight scopes (`i`, `v`, `k`).  Longer names only when the variable escapes or the scope is large.

### Line length

There is no enforced limit, but prefer to keep lines under ~100 characters.  Break long function signatures, not long import paths.  Never shorten a name to fit a line.

### Comments

Comments are for the *why*, not the *what*.  If the code already says what it does, the comment adds nothing.

```go
// Bad: the code already says this
// Increment i by 1
i++

// Good: explains a non-obvious constraint
// Skip the first byte -- the framing protocol reserves it as a version tag.
data = data[1:]
```

Exported identifiers need doc comments (`// TypeName ...` or `// FuncName ...`). Unexported identifiers only need comments when the logic is subtle.

## Design Principles

### Avoid premature interfaces

This is the single most common mistake imported from Java/TypeScript.  In Go, **do not define an interface until you have at least two concrete types that need to satisfy it, or until a caller genuinely cannot depend on the concrete type**.

```go
// Bad: interface defined alongside its only implementation
type UserStore interface {
    GetUser(id int) (*User, error)
    SaveUser(u *User) error
}
type sqliteUserStore struct { ... }

// Good: start with the concrete type
type UserStore struct { db *sql.DB }
func (s *UserStore) GetUser(id int) (*User, error) { ... }
func (s *UserStore) SaveUser(u *User) error { ... }
```

Extract an interface when a second implementation appears (a mock for tests, a different backend, a decorator).  Not before.

The exception: interfaces from the standard library that callers already accept (`io.Reader`, `io.Writer`, `http.Handler`).  Accept those where they fit.

### Accept interfaces, return concrete types

Function parameters should be as general as is useful.  Return values should be as specific as possible.  Callers can always widen a concrete type; they cannot narrow an interface.

```go
// Accept the narrowest interface that satisfies the requirement
func Process(r io.Reader) (*Result, error)

// Return the concrete type so callers can use all of it
func NewClient(cfg Config) *Client
```

**Connection-oriented code should accept `io.ReadWriter` or `net.Conn`.** When a library manages a protocol over a connection, the library should accept the connection as a parameter rather than creating it internally.  This lets callers control transport details (TCP, TLS, Unix sockets, etc.) and makes testing straightforward -- tests can pass a `net.Pipe` instead of spinning up real network listeners.  See the [`net.Pipe` testing pattern](#netpipe-for-testing-connection-oriented-code) below.

### Compiler enforcement of interface satisfaction

Every type that claims to implement an interface gets a compile-time assertion adjacent to the type definition:

```go
var _ Store = (*SQLiteStore)(nil)
```

Fails to compile if `*SQLiteStore` stops implementing `Store`.  No test infrastructure required; this is the Go standard for this pattern.

### Small, focused packages

A package should have a clear, single responsibility that can be stated in one sentence.  If the sentence requires "and", split the package.

Package-level `init()` functions are almost always the wrong choice.  They run opaquely, in an order that callers cannot control, and they make testing hard.  Prefer explicit initialization.

### Struct design

Embed only when the embedded type's full API *belongs* on the outer type.  Embedding for code reuse (to avoid writing delegation methods) leads to leaky abstractions -- callers get methods they didn't expect, and the inner type's API becomes part of the outer type's contract.

```go
// Bad: embedding sync.Mutex to avoid writing Lock()/Unlock() wrappers
type Cache struct {
    sync.Mutex   // now callers can Lock() a Cache directly -- unintended
    data map[string]string
}

// Good: embed unexported, or use a field
type Cache struct {
    mu   sync.Mutex
    data map[string]string
}
```

Zero-value usefulness: design structs so that `var x T` is valid and sensible.  `sync.Mutex`, `bytes.Buffer`, and `sync.WaitGroup` all work at zero value.  Reach for this where it makes initialization simpler.

**Zero-value as "explicitly set" sentinel.** Add a separate `fooSet bool` field alongside a value *only* when the zero-value of the field is a legitimately set value.  When the zero-value unambiguously means "not set" -- empty string, nil pointer, zero for a counter that cannot meaningfully be zero -- the value itself carries that information and the bool is redundant noise.

```go
// Bad: bool is redundant because "" is never a valid hostname
type Config struct {
    hostname    string
    hostnameSet bool  // unnecessary -- "" already means "not set"
}

// Good: "" means "use default"; any non-empty value is explicitly set
type Config struct {
    hostname string  // "" = use default; non-empty = explicitly set
}
```

When the zero-value is a valid set value (e.g. `Timeout` of `0` meaning "no timeout"), you have two options:

- **`*T` (pointer)** -- `nil` means unset.  One field, self-documenting, no sync risk.  Costs a nil check and an indirection on every access.  Prefer for config structs constructed once and read many times.
- **`T` + `fooSet bool`** -- direct value access, no nil checks, no allocation.  Costs a second field that can drift out of sync.  Prefer on hot paths, frequently copied structs, or where the struct must be comparable.

### Config structs over functional options

For a closed, finite, spec-driven option space, prefer a **plain `Config` struct** over functional options.  A `Config` struct puts all fields in one place in godoc, eliminates closure-per-field allocation overhead, and avoids the hidden-invariant problem where some options set unexported tracking fields that field-direct mutation silently bypasses.

Functional options (`func WithX(v T) Option`) are appropriate when the option space is truly open-ended and grows over time across independent contributors.  When the set of options is known and bounded, a `Config` struct is simpler, more readable, and easier to reason about.

```go
// Good: all fields visible at a glance, zero allocations
client, err := NewClient(Config{
    Timeout:    30 * time.Second,
    MaxRetries: 3,
    BaseURL:    "https://api.example.com",
})

// Functional options -- only when the option space is genuinely open-ended and multiple independent packages must contribute options to a type they don't own.
// Example: a base logger package + independent middleware packages each providing With*.
```

**Fluent builders on the config type are discouraged but acceptable if they earn their weight.**  Methods on the configuration type (e.g. `Spec.ReadOnly()`, `Spec.AsOverlay()`) group naturally in godoc and work well with `.` autocomplete.  These are not the same as free-standing `With*` functional options -- they mutate and return the same struct value.

Avoid sub-packages for option types -- they hurt discoverability (callers must know the package name to find anything) and make interface satisfaction across packages painful.  Use a consistent naming prefix (`FooOptBar`) as the fallback when methods on a type are genuinely awkward.  Bare package-level functions in a large package are the worst option: they scatter alphabetically with unrelated functions and cannot be discovered by browsing.

### `string` vs `io.Reader`, `[]byte` vs `io.Writer`

Choose parameter types based on how the data will actually arrive and how large it might be:

**Input:**

- `string` -- identity, names, keys, short config values.  Always fully materialized in memory; copying is cheap; comparison with `==` works.
- `[]byte` -- raw bytes that the caller already has in memory, often when working with encoding/decoding at a low level.
- `io.Reader` -- content of unknown or potentially large size: files, network responses, stdin.  Lets the caller stream without buffering the whole thing.

**Output:**

- Return `string` or `[]byte` for small, fully-formed results.
- Accept an `io.Writer` when the function produces a stream or when the caller should choose where it goes (file, buffer, stdout, HTTP response body).

The practical rule: if you are tempted to write `ioutil.ReadAll(r)` inside your function just to get a `[]byte`, consider whether the function should accept `[]byte` directly and leave the reading to the caller.  Conversely, if a function currently takes a `string` but callers always do `os.ReadFile(path)` before calling it, the function should accept an `io.Reader` and let callers pass the file directly.

```go
// Bad: forces callers to read the whole file into memory
func Parse(data string) (*Config, error)

// Good: callers can pass a file, a bytes.Reader, or an http.Response.Body
func Parse(r io.Reader) (*Config, error)

// Fine: a name is not content
func (s *Store) Lookup(key string) (*Record, error)
```

The same logic applies to functions that produce output: `func Render(w io.Writer, data any) error` composes with files, buffers, and HTTP writers without allocating an intermediate string.

### Constructors accept a Config struct

When a constructor has more than ~3 optional parameters, pass a `Config` struct.  This is the default choice.  Functional options are only appropriate when the option space is genuinely open-ended and multiple independent packages must contribute options to a type they don't own.

```go
type Config struct {
    Timeout    time.Duration
    MaxRetries int
    BaseURL    string
}
func New(cfg Config) *Client { ... }
```

Do not mix functional options and config structs in the same constructor.

### Error handling

Return errors.  **Do not panic in library code** -- not even for programmer errors.  A panic in an exported function forces callers to use `recover`, which is fragile and untestable.  A panic in a goroutine owned by a library terminates the whole process; callers cannot recover across goroutine boundaries.  Always return a descriptive error instead.

The only acceptable use of `panic` is in unexported package-level init expressions (`var _ = mustCompile(...)`, `init()`) where there is genuinely no caller to receive an error and the failure indicates a deployment misconfiguration that should halt the process at startup.

Every `must*`/`Must*` helper must have a non-panic counterpart that returns an error.  The panicking form is a thin wrapper:

```go
func compile(s string) (*regexp.Regexp, error) { return regexp.Compile(s) }
func mustCompile(s string) *regexp.Regexp {
    re, err := compile(s)
    if err != nil { panic(err) }
    return re
}
```

Tests cover `compile` (the real logic); `mustCompile` is trivially verifiable by inspection.  A `Must*` with no testable counterpart is untestable by definition and should not exist.

Wrap errors with context using `fmt.Errorf("operation: %w", err)`.  The `%w` verb makes the original error unwrappable via `errors.Is` and `errors.As`.

```go
// Bad: error message starts with capital letter, ends with punctuation
return fmt.Errorf("Failed to open file: %w.", err)

// Good: lowercase, no trailing punctuation, gives context
return fmt.Errorf("open config: %w", err)
```

Error strings are not sentences.  They are fragments that compose into a chain: `"parse config: open file: no such file or directory"`.

Define sentinel errors (`var ErrNotFound = errors.New("not found")`) for conditions callers need to check.  Define error types (`type ValidationError struct { ... }`) when callers need structured information.  Do not define error types just to add a message.

Do not discard errors with `_` unless the error is genuinely irrelevant (e.g. closing a read-only file).  When you discard an error, add a comment explaining why.

### Library-first tooling

When writing a CLI tool or any runnable program, put all meaningful logic in an importable package.  Keep `main` as a thin layer that parses arguments, wires dependencies, and calls into the library.  The binary is just one consumer of the library; tests, other tools, and future callers are others.

```
myapp/
  cmd/myapp/main.go   // flag parsing, os.Exit, wires everything together
  internal/app/       // all real logic -- importable, testable
```

`main` should do almost nothing that requires testing.  If you find yourself wanting to test logic that lives in `main.go`, move it into the library.

This pattern has concrete payoffs:

- **Testability**: library functions take inputs and return outputs; no need to exec a subprocess or capture stdout to test behavior.
- **Reusability**: another tool or service can import the library directly without shelling out.
- **Error handling**: libraries return errors; `main` decides whether to log and continue or print and `os.Exit(1)`.  The policy lives in one place.
- **Separation of concerns**: flag names, env var names, and output formatting are UI decisions that belong in `main`, not buried in library code.

Avoid `os.Exit` anywhere except `main`.  A library that calls `os.Exit` cannot be used in a larger program without killing the whole process.  Similarly, avoid writing to `os.Stdout` or `os.Stderr` from library code -- accept an `io.Writer` parameter if output is needed, or return the data and let the caller decide what to do with it.

### Embeddable CLIs

A CLI built on the library-first pattern can go one step further: make the command structure itself composable and embeddable.  Instead of a monolithic `main` that owns all subcommands, define each command as a plain struct with a `Run(ctx context.Context, args []string) error` method (or similar).  The top-level `main` wires them together; another program can import and reuse individual commands without re-implementing the logic.

```go
// cmd/myapp/main.go -- wires and runs
// internal/cmd/serve.go -- ServeCommand struct, exported Run method
// internal/cmd/migrate.go -- MigrateCommand struct, exported Run method
```

The payoff is that commands become testable as units: construct the struct, call `Run`, assert on the result -- no subprocess, no captured output, no temporary binaries.  It also makes it straightforward to embed your tool's functionality into a larger host binary (e.g. a debug or admin server that bundles several sub-tools).

Output should flow through an `io.Writer` field on the command struct, not directly to `os.Stdout`.  This is the seam that makes testing and embedding work: in production, pass `os.Stdout`; in tests, pass a `bytes.Buffer`.

**CLI framework conventions must not leak.** Design every CLI decision -- subcommand organization, naming, flag placement, error messages -- from the user's perspective, not the framework's.  If a user or operator can tell which CLI library was used by looking at the command tree or help output, the UX has failed.  For example, users should never need to know or care which CLI framework (Cobra, etc) this project uses.

### Concurrency

Do not add concurrency because it might be faster.  Add it when you have a measured bottleneck or an inherently concurrent problem (serving multiple requests, watching multiple file descriptors).

Goroutines are cheap to start but not free to reason about.  Every goroutine needs a clear owner and a clear exit path.  Goroutines that leak (no way to signal them to stop) are bugs.

The `context.Context` pattern is the standard way to propagate cancellation.  Accept `ctx context.Context` as the first parameter of any function that does I/O, makes network calls, or runs for an unbounded time.  Pass it down; do not store it in a struct.

```go
// Bad: context stored in struct
type Fetcher struct {
    ctx context.Context
}

// Good: context passed per-call
func (f *Fetcher) Fetch(ctx context.Context, url string) ([]byte, error)
```

Prefer channels for ownership transfer and `sync.Mutex` for shared state protection.  Do not use channels as a substitute for mutexes on shared data.

## Testing

### Test structure

Use table-driven tests for any function with more than two or three meaningful input cases.  Each entry should have a name (`name string`) so failing tests identify themselves.

```go
func TestAdd(t *testing.T) {
    tests := []struct {
        name string
        a, b int
        want int
    }{
        {"positive", 1, 2, 3},
        {"negative", -1, -2, -3},
        {"zero", 0, 0, 0},
    }
    for _, tt := range tests {
        t.Run(tt.name, func(t *testing.T) {
            got := Add(tt.a, tt.b)
            if got != tt.want {
                t.Errorf("Add(%d, %d) = %d, want %d", tt.a, tt.b, got, tt.want)
            }
        })
    }
}
```

Use subtests (`t.Run`) even for small tables.  They give test isolation, parallel-safe naming, and filterable output (`go test -run TestAdd/zero`).

### What to test

Test behavior, not implementation.  Tests that reach into unexported fields or call unexported functions are brittle.  If you need to test an unexported function directly, consider whether it should be exported, or whether the test belongs in the same package (using `package foo` not `package foo_test`).

Test the contract of a function: given these inputs, produce this output or error.  Do not test that a particular internal function was called (use `_test.go` stubs only when the external contract depends on the side effect).

### Mocking and fakes

Mocks are often overused.  Before writing a mock, ask:

- Can I use the real implementation with a test database / temp dir / in-memory backend?
- Can I refactor to accept an interface and pass a simple hand-written fake?
- Is the behavior I am testing actually about this dependency, or about my code?

When you do write a fake, write it by hand as a struct implementing an interface.  Auto-generated mocks (mockery, gomock) are heavier than they look: they encode call sequences and argument matchers, which makes tests brittle when refactoring.

### `net.Pipe` for testing connection-oriented code

When library code accepts `io.ReadWriter` or `net.Conn` for connections -- [which it should](#accept-interfaces-return-concrete-types) -- use `net.Pipe()` in tests instead of `net.Listen`/`net.Dial` with TCP.  `net.Pipe()` returns two connected `net.Conn` values -- one for each side -- with no network stack, no port allocation, no timing races, and no risk of port conflicts.

**Single connection:** create one pipe, run the server on one end, the client on the other:

```go
serverConn, clientConn := net.Pipe()

errCh := make(chan error, 1)
go func() {
    defer serverConn.Close()
    _, err := srv.HandleConnection(serverConn, opts)
    errCh <- err
}()

// use clientConn for the client side
result, err := client.Connect(clientConn)
```

**Multiple connections:** when the client opens fresh connections per operation (e.g. a connection-per-request pattern), pre-allocate the server and create a new pipe pair for each connection:

```go
srv := NewServer(config)

client := &Client{
    ConnectFunc: func() (io.ReadWriter, error) {
        serverConn, clientConn := net.Pipe()
        go func() {
            defer serverConn.Close()
            _, _ = srv.HandleConnection(serverConn, opts)
        }()
        return clientConn, nil
    },
}
```

Each call to `ConnectFunc` gets its own isolated pipe pair with its own server goroutine.  No TCP listener needed, and each connection is independent.

**Synchronous behavior exposes protocol bugs.** `net.Pipe` is synchronous with no internal buffering -- a write blocks until the other end reads the data (or closes its end, which fails the write with `io.ErrClosedPipe`).  This is a benefit: it catches protocol-level synchronization bugs that TCP's socket buffer would silently hide.  If the server writes a response and the client never reads it, the test hangs instead of passing -- which means the protocol has a real bug.  Fix: always drain the connection on both sides, or close the unused end to unblock the writer.  In tests that expect a server error response, read the error on the client side before asserting:

```go
// send bad auth
clientConn.Write(wrongAuth)

// drain the error response so the server goroutine can unblock
var buf [1024]byte
_, _ = clientConn.Read(buf[:])

// now the server can finish and send its error
err := <-errCh
```

### Subtests and t.Helper

Use `t.Helper()` in test helper functions so that failures report the line in the test body, not inside the helper:

```go
func assertEqual(t *testing.T, got, want int) {
    t.Helper()
    if got != want {
        t.Errorf("got %d, want %d", got, want)
    }
}
```

### Benchmarks

Benchmarks live in `_test.go` files alongside unit tests.  Use them when you have a performance claim to verify.  Always run with `-benchmem` to catch allocations.  A benchmark that does not call `b.ResetTimer()` after setup is measuring setup time, not what you intend.

### Test coverage

Coverage is a floor, not a ceiling.  80% coverage with meaningful tests is better than 100% coverage with tests that only exercise the happy path.  Do not write tests solely to hit a coverage number.

## Idiomatic Patterns

### The "comma ok" idiom

Use it consistently for map lookups, type assertions, and channel receives:

```go
v, ok := m[key]
if !ok { /* key absent */ }

n, ok := i.(int)
if !ok { /* not an int */ }

v, ok := <-ch
if !ok { /* channel closed */ }
```

### Defer for cleanup

`defer` is not only for file closing.  Use it for any "undo on exit" pattern: unlock a mutex, stop a timer, close a response body, remove a temp file.  Place the defer immediately after the resource is acquired, not at the top of the function.

Note: `defer` in a hot loop (millions of calls) adds overhead.  In those cases, call cleanup explicitly.

### init() is almost always wrong

`init()` runs at program start, in dependency order, before `main()`.  It is hard to test, impossible to mock, and invisible to callers.  The only legitimate uses are registering codecs with a known registry (e.g. SQL drivers, image formats) and initializing unexported package-level variables that truly cannot fail.

For anything else, use an explicit constructor (`New...`) or a `sync.Once`.

### sync.Once for lazy singletons

```go
var (
    instance *Thing
    once     sync.Once
)

func GetThing() *Thing {
    once.Do(func() {
        instance = newThing()
    })
    return instance
}
```

### String building

Use `strings.Builder` for building strings in a loop.  Do not concatenate with `+` in a loop -- each `+` allocates a new string.  `fmt.Sprintf` is fine for one-off formatting but is slower than direct writes to a `Builder` or a `bytes.Buffer`.

### Slices and maps

Initialize slices with `make([]T, 0, n)` when the length is known ahead of time -- this avoids repeated reallocations.  Initialize maps similarly with `make(map[K]V, n)`.

A nil slice and an empty slice are different in a `== nil` check but identical for `len`, `cap`, `range`, and `append`.  Return nil (not `[]T{}`) when there are no results, unless the caller marshals to JSON (where nil becomes `null` and `[]T{}` becomes `[]`).

Do not modify a slice or map while ranging over it.  Collect keys/indices to modify first, then apply changes.

## Common Mistakes (for humans and LLMs)

### Over-abstracting early

The most common Go code quality problem is premature abstraction: interfaces for every type, factories for every constructor, layers of indirection added "for flexibility".  Write the concrete code first.  Abstract when the duplication is real and the abstraction has a clear name.

### Returning `interface{}` / `any`

Avoid `any` return types in new APIs.  They push type assertions to callers and eliminate compile-time safety.  Generics (Go 1.18+) solve most cases that previously required `interface{}`.

### Ignored errors

Every error return must be checked or explicitly discarded with a comment.  `_` is only appropriate when the function contract guarantees no meaningful error in that context, or when the caller genuinely has no way to recover.

### Goroutine leaks

A goroutine that never exits is a memory and resource leak.  Common causes:

- Sending to an unbuffered channel with no receiver
- Receiving on a channel that is never closed or sent to
- Waiting on a `context.Context` that is never cancelled

Always verify that goroutines started by your code have a path to exit.

### Nil pointer dereference in receivers

A method with a pointer receiver can be called on a nil pointer (Go allows this).  If the method does not guard against nil, it panics at the first field access.  Either document that nil is invalid, or guard explicitly:

```go
func (c *Client) Do() error {
    if c == nil {
        return errors.New("nil client")
    }
    ...
}
```

### Not using `errors.Is` / `errors.As`

Comparing errors with `==` breaks when errors are wrapped.  Use `errors.Is` to check for a sentinel value anywhere in the chain, and `errors.As` to extract a typed error.

```go
// Bad: breaks with wrapped errors
if err == ErrNotFound { ... }

// Good
if errors.Is(err, ErrNotFound) { ... }
```

## Module and Dependency Management

Keep the dependency list short.  Every dependency is code you did not write and cannot fully control.  Before adding a package, ask whether the standard library already covers the use case.

Pin to a specific version in `go.mod`.  Do not use pseudo-versions (`v0.0.0-20230101...`) for packages that have tagged releases; update to the tagged version.

Run `go mod tidy` before committing.  An untidy `go.mod`/`go.sum` means the module graph is stale.

Do not vendor dependencies unless the build environment requires it (air-gapped CI, specific reproducibility requirements).  In most cases `go.sum` provides sufficient reproducibility.

## Build System

### Prefer the standard toolchain

The Go toolchain is a build system.  `go build`, `go test`, `go vet`, and `go generate` handle the overwhelming majority of what a Makefile would do -- and they do it without an extra tool in the dependency chain.

Before reaching for a Makefile, check whether the toolchain already covers the need:

| Task | Standard tool |
|---|---|
| Build a binary | `go build -o bin/myapp ./cmd/myapp` |
| Run all tests | `go test ./...` |
| Run tests with the race detector | `go test -race ./...` |
| Run a specific test | `go test -run TestFoo ./pkg/...` |
| Static analysis | `go vet ./...` |
| Code generation | `go generate ./...` |
| Tidy dependencies | `go mod tidy` |
| Format code (local) | `gofmt -s -w .` or `goimports -w .` |
| Format code (CI check) | `gofmt -s -w . && git diff --exit-code` |
| Cross-compile | `GOOS=linux GOARCH=amd64 go build ./...` |

A Makefile that wraps these commands one-for-one provides no value -- it is just extra syntax to learn and maintain, and it hides what is actually running.

### Do not use Makefiles

Make is a build system for C. It understands file timestamps and C compilation units.  It does not understand Go modules, import graphs, or the Go toolchain's built-in dependency tracking.  Using Make for a Go project means maintaining a parallel, inferior build system on top of one that already works.

When the need is "run a sequence of commands", the right tools are a shell script or a small Go program under `cmd/`.  Both are readable by anyone on the project without knowledge of Make's syntax, tab-sensitivity rules, or implicit variable semantics.  A `cmd/release/main.go` that builds, signs, and uploads is explicit Go code -- it can be tested, it has proper error handling, and it does not require Make to be installed.

### go generate

Use `//go:generate` directives for code generation (protobuf, stringer, mocks).  The directive lives in the source file next to the code it generates, which makes the relationship explicit.  Run `go generate ./...` to execute all directives.

Do not run `go generate` as part of the normal build.  Generated files are committed to the repository so that `go build` works without any extra tools installed.  Regenerate explicitly when the source changes.

### Build tags

Use build tags to conditionally compile platform-specific code or test-only helpers that should not appear in production builds:

```go
//go:build linux

package sys
```

The modern syntax (`//go:build`) is preferred over the old `// +build` form.  `gofmt` will add or update the constraint automatically since Go 1.17.

## Documentation

Write `go doc`-compatible comments for all exported symbols.  The convention:

```go
// Client manages connections to the upstream service.
// A zero-value Client is not valid; use New to construct one.
type Client struct { ... }

// Fetch retrieves the resource at path and returns its contents.
// It cancels the underlying request when ctx is done.
func (c *Client) Fetch(ctx context.Context, path string) ([]byte, error) { ... }
```

The first sentence of a doc comment is used as the summary in `go doc` output.  Make it a complete sentence that can stand alone.

Package-level doc comments go in a file named `doc.go` (if the package is large) or above the `package` declaration in the main file.  They describe the package's purpose and the main entry points.

## Notes for LLM Assistants

When generating or editing Go code in this project:

- **Do not add interfaces speculatively.** If there is one concrete type, define a concrete type.  Extract an interface only when a second implementation exists or a test double is needed.
- **Do not add `// TODO` comments** for things the current task does not require.  Leave the code clean.
- **Do not add logging to library code.** Libraries accept a `logger` if they must, or they return errors.  Printing to stderr from a library is almost always wrong.
- **Do not add `context.Background()` as a default.** If a function takes a context, require it from the caller.  Do not paper over the gap with `context.Background()` or `context.TODO()` inside library code.
- **Match the existing style** in the file you are editing.  If the file uses `errors.New`, do not introduce `fmt.Errorf`.
- **Return the real type, not an interface**, unless the function is part of an existing API that already returns an interface.
- **Do not add error wrapping that strips information.** `fmt.Errorf("error: %v", err)` without `%w` loses the original error type.  Use `%w` unless you intentionally want to hide the underlying error.
- **Prefer `t.Fatal` over `t.Error` + `return`** in tests when continuing after a failure would cause a nil dereference or meaningless subsequent failures.
- **Do not change `go.mod` or `go.sum`** unless the task explicitly requires a new dependency.  If a standard library alternative exists, use it.
