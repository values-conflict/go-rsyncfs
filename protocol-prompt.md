# Prompt: Create Definitive Rsync Protocol Reference

## Context

You are working on `go-rsyncfs`, a Go implementation of the rsync daemon protocol. The upstream rsync source tree is in `.upstream/` (git submodule). A partial protocol reference exists in `protocol.md` but is incomplete and doesn't adequately distinguish rsync's internal IPC from the actual daemon socket protocol, and it doesn't cover all protocol versions or transport modes.

The recurring problem: rsync uses **three cooperating processes** during transfers (generator, receiver, daemon), supports **two transport modes** (daemon socket on port 873, SSH/rsh tunnel), and **thirteen protocol versions** (20–32), and the same function names (`write_buf`, `read_buf`, `write_ndx`, etc.) resolve to completely different wire formats depending on which process calls them, what I/O mode was set up, and what protocol version was negotiated. This causes repeated confusion about whether data on the wire is multiplexed (MSG_DATA frames) or raw bytes.

## Goal

Create a **definitive, exhaustive protocol reference** that covers every protocol version (20–32), both transport modes (daemon socket, SSH/rsh), and all three process roles. The output should enable accurate tracing of what bytes go on the wire, in what format, at what point, across **all supported protocol versions and transport modes**. The document must be organized for writing fresh implementations -- a reader should be able to implement a correct rsync daemon or client from the document alone.

## Output

Rewrite `protocol.md` from scratch. The current document is not serving our needs -- feel free to restructure, merge, or drop any existing content if the new organization is clearer. The goal is a single coherent reference, not preservation of the status quo.

### Section 1: Transport Modes (Daemon Socket vs SSH/rsh)

Document the **two fundamentally different transport modes** and how they differ:

#### Daemon Socket Transport (port 873)
- Text-based greeting exchange (`@RSYNCD:` protocol)
- Module selection, authentication, argument transmission
- Source: `.upstream/clientserver.c` (`exchange_protocols()`, `start_inband_exchange()`, `auth_server()`, `read_args()`)
- `remote_protocol` is set during the greeting exchange (via `sscanf(buf, "@RSYNCD: %d.%d", ...)`)
- `setup_protocol()` skips the binary version exchange because `remote_protocol != 0` (guard at `.upstream/compat.c:600`)

#### SSH/rsh Transport
- No greeting exchange -- rsync binary is invoked remotely via shell
- Binary version exchange: `write_int(f_out, protocol_version)` / `remote_protocol = read_int(f_in)` (`.upstream/compat.c:602-606`)
- This exchange ONLY happens when `remote_protocol == 0` (`.upstream/compat.c:600`)
- No module selection, no daemon authentication
- Arguments passed as command-line args to the remote rsync process
- Source: `.upstream/main.c` (`do_cmd()`, `launch_generator()`)

#### Key differences table

| Aspect | Daemon Socket | SSH/rsh |
|--------|---------------|---------|
| Greeting | `@RSYNCD: version.sub digests` (text) | None |
| Version exchange | During greeting (parsed from text) | Binary `write_int`/`read_int` (`.upstream/compat.c:602-606`) |
| Module selection | Yes (text) | No |
| Authentication | Yes (digest-based, optional) | No (delegated to SSH/rsh) |
| Argument format | Text (newline or null-terminated) | Command-line args |
| `remote_protocol` initial value | Set by greeting parse | 0 (triggers binary exchange) |
| `daemon_connection` value | 1 (via shell) or -1 (direct socket) | 0 |

### Section 2: Protocol Version Reference (20–32)

Document **every protocol version** (MIN_PROTOCOL_VERSION=20 through PROTOCOL_VERSION=32) and **every major behavioral change** at each version boundary. Use the upstream source code to find all `if (protocol_version >= N)` and `if (protocol_version < N)` gates.

**Minimum coverage per version:**

| Version | Release | Key changes (verify against source) |
|---------|---------|-------------------------------------|
| 20 | 2.3.0 (1999) | MIN_PROTOCOL_VERSION -- oldest still supported |
| 21 | -- | Checksum algorithm change (`.upstream/checksum.c:127`) |
| 22 | -- | File list / argument handling changes |
| 23 | -- | Multiplexed I/O layer introduced (`io_start_multiplex_in/out` gates) |
| 24 | -- | Final goodbye message (`write_ndx(f_out, NDX_DONE)` at `.upstream/main.c:1137`) |
| 25 | 2.5.0 (2001) | `@RSYNC EXIT` command, OLD_PROTOCOL_VERSION threshold |
| 26 | 2.4.6pre1 | Device number encoding changes (`.upstream/flist.c:661`) |
| 27 | 2.6.0 (2004) | Per-file strong checksum length (`s2length` in sum_head, `.upstream/io.c:2062`) |
| 28 | 2.6.1 (2004) | Extended xmit flags (`XMIT_EXTENDED_FLAGS`), device major/minor 32-bit |
| 29 | 2.6.4 (2005) | Phase exchange changes (`max_phase = 2`), iflags in selectors, keep-alive |
| 30 | 3.0.0 (2008) | Compressed NDX (`write_ndx`), varint xmit flags, compat flags exchange, subprotocol version, null-terminated args |
| 31 | 3.1.0 (2013) | Nanosecond timestamps (`XMIT_MOD_NSEC`), client keep-alive, delete phase |
| 32 | 3.2.7 (2024) | Security fix version number bump, digest name list on greeting |

For each version, document:
- I/O mode changes (which fds switch between buffered/multiplexed)
- Wire format changes (new fields, encoding changes)
- Feature gates (which options/features require which version)
- Exact source code references for every gate

**Use these commands to find all version gates:**
```bash
grep -rn "protocol_version >= 2[0-9]\|protocol_version < 2[0-9]\|protocol_version == 2[0-9]\|protocol_version >= 3[0-2]\|protocol_version < 3[0-2]" .upstream/*.c
```

### Section 3: Process Architecture Reference

For each of the 3 processes (generator, receiver, daemon), document:

- Purpose and when it runs
- All file descriptors it uses (socket to daemon, internal pipe, stdin/stdout)
- I/O mode per fd (multiplexed vs buffered) for **ALL protocol versions** (20, 21, 22, 23, 24, 25, 26, 27, 28, 29, 30, 31, 32)
- Source: exact `.upstream/main.c` line ranges where I/O mode is set for each process
- What each process reads/writes on each fd

Key source files: `main.c` (process forking, I/O setup around `client_run()`, `start_server()`, and the fork in `do_recv()`), `io.c` (`io_start_multiplex_in/out`, `io_start_buffering_in/out`)

### Section 4: Communication Channel Map

For each communication channel between processes, create a table:

| Channel | Transport | Output Mode | Wire Format | Input Mode | Proto Gate |
|---------|-----------|-------------|-------------|------------|------------|
| Generator → Daemon (selectors) | daemon socket | buffered | raw bytes | buffered/mux | all/proto >= 30 |
| Daemon → Receiver (file data) | daemon socket | multiplexed | MSG_DATA frames | multiplexed | >= 23 |
| Receiver → Generator (status) | internal pipe | multiplexed | MSG_DATA frames | multiplexed | all |
| ... | ... | ... | ... | ... | ... |

For each channel, note:
- What data flows on it (selectors, file data, checksums, status messages)
- Whether mux frame headers are visible to the reader or transparently unwrapped
- Protocol version dependencies (document EVERY version where behavior changes)

### Section 5: Wire Protocol Step-by-Step (Daemon Socket)

For a simple `rsync -av host::mod/ ./` pull, trace EVERY wire event on the daemon socket:

For each step:
1. Phase/step name (e.g., "greeting exchange", "file list transfer", "phase exchange", "selector loop")
2. Direction (client→server or server→client)
3. Which process generates it (generator, receiver, or daemon)
4. Raw wire format with hex example
5. I/O mode used (mux frame or raw bytes)
6. Source code: `.upstream/file.c:line` for both sender and receiver
7. **Protocol version differences for EVERY version (20–32)** -- if a step differs between versions, document each variant

### Section 6: Wire Protocol Step-by-Step (SSH/rsh Transport)

For a simple `rsync -av user@host:/path/ ./` pull, trace EVERY wire event:

1. Remote process launch (no wire data -- shell exec)
2. Binary version exchange (`write_int`/`read_int`)
3. Compat flags exchange (proto >= 30)
4. Checksum/compression negotiation (proto >= 30, `CF_VARINT_FLIST_FLAGS`)
5. Checksum seed exchange
6. Filter list transfer
7. File list transfer
8. Phase exchange
9. Selector loop
10. Final goodbye
11. Stats exchange

For each step, document:
- Wire format with hex example
- I/O mode used
- Source code references
- Protocol version differences

### Section 7: I/O Mode Resolution

Document how `write_buf()`/`read_buf()`/`write_int()`/`read_int()` resolve to different wire formats:

- When iobuf is multiplexed: `write_buf()` accumulates in `iobuf.out`, flushed as MSG_DATA frames; `read_buf()` transparently unwraps MSG_DATA frames
- When iobuf is buffered: `write_buf()` writes raw bytes directly; `read_buf()` reads raw bytes directly
- Source: `.upstream/io.c` function dispatch logic (`_write_buf`, `_read_buf`, multiplex dispatch)

### Section 8: Protocol Version I/O Mode Matrix

Create a **complete matrix** showing I/O mode per process, per direction, per **ALL protocol versions** (20–32):

| Process | Direction | Proto 20-22 | Proto 23-24 | Proto 25-26 | Proto 27 | Proto 28-29 | Proto 30 | Proto 31 | Proto 32 |
|---------|-----------|-------------|-------------|-------------|----------|-------------|----------|----------|----------|
| Generator | output → Daemon | buffered | buffered | buffered | buffered | buffered | buffered | buffered | buffered |
| Generator | input ← Receiver pipe | -- | -- | -- | -- | -- | multiplexed | multiplexed | multiplexed |
| Receiver | input ← Daemon | buffered | multiplexed | multiplexed | multiplexed | multiplexed | multiplexed | multiplexed | multiplexed |
| Receiver | output → Generator | buffered | multiplexed | multiplexed | multiplexed | multiplexed | multiplexed | multiplexed | multiplexed |
| Daemon (sender) | output → Client | buffered | multiplexed | multiplexed | multiplexed | multiplexed | multiplexed | multiplexed | multiplexed |
| Daemon (sender) | input ← Client | buffered | buffered | buffered | buffered | buffered | multiplexed | multiplexed | multiplexed |
| ... | ... | ... | ... | ... | ... | ... | ... | ... | ... |

Verify each cell against the actual `if (protocol_version >= N)` gates in `main.c`. Group versions where behavior is identical, but document every transition point.

### Section 9: Integer Encoding Formats

Document all integer encoding formats used on the wire:

- Fixed-width integers (`write_int`/`read_int`) -- 4 bytes LE, all versions
- Variable-length integers (`write_varint`/`read_varint`) -- proto >= 30
- Variable-length long integers (`write_varlong`/`read_varlong`) -- proto >= 30
- Legacy long integers (`write_longint`/`read_longint`) -- proto < 30
- Short integers (`write_shortint`/`read_shortint`) -- 2 bytes LE
- Compressed NDX (`write_ndx`/`read_ndx`) -- proto >= 30 (delta-encoded), proto < 30 (falls back to `write_int`)
- vstring (`write_vstring`/`read_vstring`) -- all versions

For each, document:
- Wire format with byte-level detail
- Protocol version gates
- Source code references
- Encoding/decoding algorithms (especially for compressed NDX)

### Section 10: Multiplexed I/O Layer

Document the multiplexed I/O layer in detail:

- Frame format: 4-byte header (`((MPLEX_BASE + msgCode) << 24) | length`) + payload
- Message codes (`enum msgcode`) -- document ALL codes with payload format and direction
- iobuf buffering model (circular buffers, batching, flush behavior)
- How `read_a_msg()` dispatches non-DATA messages
- Source: `.upstream/io.c` (multiplex functions), `.upstream/rsync.h` (`MPLEX_BASE`, `enum msgcode`)

### Section 11: File List Wire Format

Document the file list wire format in detail:

- Xmit flags encoding per protocol version (proto < 28, 28-29, >= 30 with varint)
- Xmit flag bits (all flags, with version gates)
- File entry wire layout (all fields, with conditional inclusion per flags/version)
- End-of-list markers (`NDX_DONE`, `NDX_FLIST_EOF`)
- Source: `.upstream/flist.c` (`send_file_list()`, `receive_file_entry()`)

### Section 12: Checksum & Delta Transfer Protocol

Document the checksum and delta transfer protocol:

- SumHead format (`write_sum_head`/`read_sum_head`)
- Block checksums format
- Checksum algorithms (checksum1 rolling, checksum2 strong)
- Delta fill data format
- Source: `.upstream/io.c` (sum_head functions), `.upstream/checksum.c`

### Section 13: Selector Protocol

Document the selector protocol (phase 13):

- Selector wire format per protocol version
- Item flags (all flags, with version gates)
- How selectors flow: generator → daemon (buffered), daemon echo → receiver (multiplexed)
- Source: `.upstream/generator.c` (selector sending), `.upstream/sender.c` (selector reading/echoing)

### Section 14: Known Pitfalls

Document known confusion points with source references:

- Generator uses `write_ndx()` (compressed NDX) to daemon socket, but receiver uses `write_int()` (4-byte LE) to generator pipe -- same semantic meaning (NDX_DONE), different wire format
- `read_ndx_and_attrs(f_in, f_out)` reads from one fd and echoes to another -- the echo may use different I/O mode than the read
- Mux frame headers are transparent to application code but visible on the wire -- a raw-byte reader will see them as data
- `sock_f_out` vs `f_out` -- generator redirects `f_in` to internal pipe after fork but `sock_f_out` still points to daemon socket
- `io_start_buffering_out(f_out)` in generator overrides earlier `io_start_multiplex_out(f_out)` from `client_run()`
- `need_messages_from_generator` is set unconditionally for proto >= 30 sender connections (NOT just `inc_recurse`)
- Daemon socket protocol (text greeting) vs SSH/rsh protocol (binary version exchange) -- `remote_protocol == 0` gate

### Section 15: Verification Answers

After writing, verify by answering (with source references):

1. When the real rsync client (proto 32) connects to our server via daemon socket, what I/O mode does the generator use to send selectors? (Answer: buffered/raw bytes)
2. What I/O mode does the daemon use to send file data to the receiver? (Answer: multiplexed/MSG_DATA frames)
3. Does the receiver read from the daemon socket using mux or buffered input? (Answer: multiplexed for proto >= 23)
4. What wire format does NDX_DONE have on the daemon socket vs the internal pipe? (Answer: 1 byte `0x00` on socket for proto >= 30, 4 bytes `0xFFFFFFFF` on pipe)
5. What I/O mode does the daemon use to read selectors from the generator on proto 32? (Answer: multiplexed, because `need_messages_from_generator` is always 1 for proto >= 30 senders)
6. How does the SSH/rsh version exchange differ from the daemon socket greeting? (Answer: binary `write_int`/`read_int` when `remote_protocol == 0`, vs text `@RSYNCD:` parse)
7. What changed at protocol version 23? (Answer: multiplexed I/O layer introduced)
8. What changed at protocol version 30? (Answer: compressed NDX, varint xmit flags, compat flags, subprotocol version, null-terminated args)

If you can't answer all eight with source references, the document is incomplete.

## Constraints

- Every claim MUST have a source code reference (`.upstream/file.c:line`)
- Verify against the actual `.upstream/` source tree, not memory or documentation
- Cover **BOTH** transport modes (daemon socket AND SSH/rsh)
- Cover **ALL** protocol versions (20–32), not just a subset
- Document **every** protocol version gate/turning point (use `grep -rn "protocol_version" .upstream/*.c` to find them all)
- Include both the common path and version-specific branches
- When in doubt, `grep -n` the upstream source and quote the relevant code
- Use `grep -rn "io_start_multiplex\|io_start_buffering" .upstream/main.c` to find I/O mode setup
- Use `grep -rn "protocol_version [><=]" .upstream/*.c` to find ALL version gates
- Cross-check with `.upstream/io.c` for I/O mode implementation details
- Cross-check with `.upstream/csprotocol.txt` for informal protocol documentation
- Cross-check with `.upstream/NEWS.md` for protocol version change history
- The document must be usable as a standalone implementation reference -- a reader should not need the upstream source to understand the protocol
