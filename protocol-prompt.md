# Prompt: Create Definitive Rsync Protocol Reference

## Context

You are working on `go-rsyncfs`, a Go implementation of the rsync daemon protocol. The upstream rsync source tree is in `.upstream/` (git submodule). A partial protocol reference exists in `protocol.md` but is incomplete and doesn't adequately distinguish rsync's internal IPC from the actual daemon socket protocol.

The recurring problem: rsync uses **three cooperating processes** during transfers (generator, receiver, daemon), and the same function names (`write_buf`, `read_buf`, `write_ndx`, etc.) resolve to completely different wire formats depending on which process calls them and what I/O mode was set up. This causes repeated confusion about whether data on the wire is multiplexed (MSG_DATA frames) or raw bytes.

## Goal

Create a definitive protocol reference that eliminates confusion between rsync's internal IPC (generator↔receiver pipe) and the actual daemon socket protocol. The output should enable accurate tracing of what bytes go on the wire, in what format, at what point, across all supported protocol versions.

## Output

Rewrite `protocol.md` from scratch. The current document is not serving our needs — feel free to restructure, merge, or drop any existing content if the new organization is clearer. The goal is a single coherent reference, not preservation of the status quo.

### Section: Process Architecture Reference

For each of the 3 processes (generator, receiver, daemon), document:

- Purpose and when it runs
- All file descriptors it uses (socket to daemon, internal pipe, stdin/stdout)
- I/O mode per fd (multiplexed vs buffered) for proto 27, 30, 31, 32
- Source: exact `.upstream/main.c` line ranges where I/O mode is set for each process
- What each process reads/writes on each fd

Key source files: `main.c` (process forking, I/O setup around `client_run()`, `start_server()`, and the fork in `do_recv()`), `io.c` (`io_start_multiplex_in/out`, `io_start_buffering_in/out`)

### Section: Communication Channel Map

For each communication channel between processes, create a table:

| Channel | Transport | Output Mode | Wire Format | Input Mode | Proto Gate |
|---------|-----------|-------------|-------------|------------|------------|
| Generator → Daemon (selectors) | daemon socket | buffered | raw bytes | buffered | all |
| Daemon → Receiver (file data) | daemon socket | multiplexed | MSG_DATA frames | multiplexed | >= 23 |
| Receiver → Generator (status) | internal pipe | multiplexed | MSG_DATA frames | multiplexed | all |
| ... | ... | ... | ... | ... | ... |

For each channel, note:
- What data flows on it (selectors, file data, checksums, status messages)
- Whether mux frame headers are visible to the reader or transparently unwrapped
- Protocol version dependencies

### Section: Wire Protocol Step-by-Step

For a simple `rsync -av host::mod/ ./` pull, trace EVERY wire event on the daemon socket:

For each step:
1. Phase/step name (e.g., "greeting exchange", "file list transfer", "phase exchange", "selector loop")
2. Direction (client→server or server→client)
3. Which process generates it (generator, receiver, or daemon)
4. Raw wire format with hex example
5. I/O mode used (mux frame or raw bytes)
6. Source code: `.upstream/file.c:line` for both sender and receiver
7. Protocol version differences (if any)

Focus on the daemon socket only (not the internal pipe).

### Section: I/O Mode Resolution

Document how `write_buf()`/`read_buf()`/`write_int()`/`read_int()` resolve to different wire formats:

- When iobuf is multiplexed: `write_buf()` accumulates in `iobuf.out`, flushed as MSG_DATA frames; `read_buf()` transparently unwraps MSG_DATA frames
- When iobuf is buffered: `write_buf()` writes raw bytes directly; `read_buf()` reads raw bytes directly
- Source: `.upstream/io.c` function dispatch logic (`_write_buf`, `_read_buf`, multiplex dispatch)

### Section: Protocol Version I/O Mode Matrix

Create a complete matrix showing I/O mode per process, per direction, per protocol version:

| Process | Direction | Proto 27 | Proto 30 | Proto 31 | Proto 32 |
|---------|-----------|----------|----------|----------|----------|
| Generator | output → Daemon | buffered | buffered | buffered | buffered |
| Generator | input ← Daemon | buffered | multiplexed | multiplexed | multiplexed |
| Receiver | input ← Daemon | buffered | multiplexed | multiplexed | multiplexed |
| Receiver | output → Generator | buffered | multiplexed | multiplexed | multiplexed |
| Daemon (sender) | output → Client | multiplexed | multiplexed | multiplexed | multiplexed |
| Daemon (sender) | input ← Client | buffered | buffered | buffered* | buffered* |

\* buffered unless `need_messages_from_generator`

Verify each cell against the actual `if (protocol_version >= N)` gates in `main.c`.

### Section: Known Pitfalls

Document known confusion points with source references:

- Generator uses `write_ndx()` (compressed NDX) to daemon socket, but receiver uses `write_int()` (4-byte LE) to generator pipe -- same semantic meaning (NDX_DONE), different wire format
- `read_ndx_and_attrs(f_in, f_out)` reads from one fd and echoes to another -- the echo may use different I/O mode than the read
- Mux frame headers are transparent to application code but visible on the wire -- a raw-byte reader will see them as data
- `sock_f_out` vs `f_out` -- generator redirects `f_in` to internal pipe after fork but `sock_f_out` still points to daemon socket
- `io_start_buffering_out(f_out)` in generator overrides earlier `io_start_multiplex_out(f_out)` from `client_run()`

## Constraints

- Every claim MUST have a source code reference (`.upstream/file.c:line`)
- Verify against the actual `.upstream/` source tree, not memory or documentation
- Focus on the daemon protocol (not SSH/rsh transport)
- Include both the common path and version-specific branches
- When in doubt, `grep -n` the upstream source and quote the relevant code
- Use `grep -rn "io_start_multiplex\|io_start_buffering" .upstream/main.c` to find I/O mode setup
- Use `grep -rn "protocol_version >=" .upstream/main.c` to find version gates
- Cross-check with `.upstream/io.c` for I/O mode implementation details

## Verification

After writing, verify by answering:

1. When the real rsync client (proto 32) connects to our server, what I/O mode does the generator use to send selectors? (Answer should be: buffered/raw bytes)
2. What I/O mode does the daemon use to send file data to the receiver? (Answer should be: multiplexed/MSG_DATA frames)
3. Does the receiver read from the daemon socket using mux or buffered input? (Answer should be: multiplexed for proto >= 23)
4. What wire format does NDX_DONE have on the daemon socket vs the internal pipe? (Answer: 1 byte `0x00` on socket for proto >= 30, 4 bytes `0xFFFFFFFF` on pipe)

If you can't answer all four with source references, the document is incomplete.
