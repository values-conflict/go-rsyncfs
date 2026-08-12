# Rsync Daemon Protocol Reference

Definitive reference for implementing go-rsyncfs.  Every claim is verified against the upstream rsync source tree in `.upstream/`.

## Byte Order

**All multi-byte integers on the wire are little-endian.**  This includes mux frame headers, fixed-width ints (`write_int`/`read_int`), varint, and varlong.  Confirmed via `SIVAL()`/`SIVAL64()` macros in `.upstream/byteorder.h`.

## Process Architecture Reference

Rsync uses **three cooperating processes** during a data transfer.  Understanding which process owns which file descriptor and I/O mode is essential for correct implementation.

### Generator (client-side)

Connects to the remote daemon, receives the file list, sends selectors (file transfer requests) to the daemon, and reads status/completion messages from the receiver.  The parent process becomes the generator after `do_recv()` forks on the client side (`.upstream/main.c:1107`).

**File descriptors (after fork):**
- `sock_f_out` (daemon socket, write) -- set by `io_set_sock_fds()` in `client_run()` (`.upstream/main.c:1297`).  Remains open through the generator's lifetime, used to send selectors and NDX_DONE to the daemon.
- `f_in` (internal pipe from receiver) -- redirected from the daemon socket to `error_pipe[0]` after fork (`.upstream/main.c:1119`).  Used to read status messages (MSG_STATS, MSG_SUCCESS), NDX_DONE, and file list data (inc_recurse forwarding via `start_flist_forward()`) from the receiver.
- `f_out` (daemon socket, write) -- `f_out` is unchanged from pre-fork; it remains the daemon socket.  `io_start_buffering_out(f_out)` at `.upstream/main.c:1121` sets buffered output on the daemon socket.

**I/O mode (after fork, `.upstream/main.c:1121-1122`):**
- Output (`f_out` → daemon socket) -- **buffered** (raw bytes).  Set by `io_start_buffering_out(f_out)` at `.upstream/main.c:1121`.  This overrides any earlier `io_start_multiplex_out(f_out)` that may have been set in `client_run()` before the fork.
- Input (`f_in` ← receiver pipe) -- **multiplexed** (MSG_DATA frames).  Set by `io_start_multiplex_in(f_in)` at `.upstream/main.c:1122`.

**Key detail:** In `generate_files(f_out, local_name)`, the `f_out` parameter is the daemon socket fd (not the internal pipe).  The generator writes selectors and NDX_DONE to the daemon socket via `write_ndx(f_out, ndx)` (`.upstream/generator.c:2390`).  The generator reads status messages, NDX_DONE, and file list data (inc_recurse) from the receiver via `wait_for_receiver()` (`.upstream/io.c:1749`), which reads from `iobuf.in_fd` (the internal pipe, set up as `f_in`).

**Source:** `.upstream/main.c:1107-1138` (generator side of fork), `.upstream/generator.c:2246-2458` (`generate_files()`).

### Receiver (client-side)

Reads file data from the daemon socket, writes files to disk, and sends completion status to the generator.  The child process becomes the receiver after `do_recv()` forks on the client side (`.upstream/main.c:1056`).

**File descriptors:**
- `f_in` (daemon socket, read) -- inherits the daemon socket from the parent, used to read echoed selectors, file data, and checksums from the daemon.
- `f_out` (internal pipe to generator) -- set to `error_pipe[1]` (`.upstream/main.c:1066`).  Used to send MSG_SUCCESS, NDX_DONE, and file list data (inc_recurse forwarding) to the generator.
- `sock_f_out` -- set to -1 after fork (`.upstream/main.c:1065`).  The receiver does not write to the daemon socket.

**I/O mode (`.upstream/main.c:1071-1072`):**
- Input (`f_in` ← daemon socket) -- **multiplexed** (MSG_DATA frames).  Inherited from the pre-fork `client_run()` setup at `.upstream/main.c:1361` (`io_start_multiplex_in(f_in)` for proto ≥ 23).  If `read_batch`, overridden to buffered by `io_start_buffering_in(f_in)` at `.upstream/main.c:1071`.
- Output (`f_out` → generator pipe) -- **multiplexed** (MSG_DATA frames).  Set by `io_start_multiplex_out(f_out)` at `.upstream/main.c:1072`.

**Key detail:** The receiver reads from the daemon socket using multiplexed input (transparently unwraps MSG_DATA frames) and writes to the generator pipe using multiplexed output (wraps in MSG_DATA frames).  When the receiver sends `write_int(f_out, NDX_DONE)` it writes 4 bytes (`0xFFFFFFFF`) to the generator pipe, wrapped in a MSG_DATA frame.

**Source:** `.upstream/main.c:1055-1104` (receiver side of fork), `.upstream/receiver.c:632-1147` (`recv_files()`).

### Daemon (server-side)

Serves file data to clients.  Runs as either a sender (pull -- client requests files) or receiver (push -- client sends files).  Activated when `start_server()` is called on the server side (`.upstream/main.c:1257`).

**File descriptors:**
- `f_in` (daemon socket, read) -- reads selectors from the client generator.
- `f_out` (daemon socket, write) -- sends file data, echoed selectors, and checksums to the client.

**I/O mode (`.upstream/main.c:1265-1275`):**
- Output (`f_out` → client socket) -- **multiplexed** (MSG_DATA frames) for proto ≥ 23.  Set by `io_start_multiplex_out(f_out)` at `.upstream/main.c:1266`.
- Input (`f_in` ← client socket) -- depends on mode:
  - If `am_sender && need_messages_from_generator`: **multiplexed** (`.upstream/main.c:1273`)
  - Otherwise: **buffered** (raw bytes) (`.upstream/main.c:1275`)

**`need_messages_from_generator` is set when:**
- `protocol_version >= 30` and `am_sender` (`.upstream/compat.c:777`) -- set unconditionally for all sender connections at proto 30+, inside the `if (am_sender)` block within the `} else if (protocol_version >= 30) {` block in `setup_protocol()`.
- `remove_source_files` (`--remove-source-files`) is set (`.upstream/options.c:2250`)

For a standard pull (`rsync -av host::mod/ ./`) with proto >= 30, `need_messages_from_generator` is 1 (set by compat.c:777), so daemon input is multiplexed.  For proto < 30, it remains 0 and daemon input is buffered.

**Source:** `.upstream/main.c:1257-1281` (`start_server()`).

## Communication Channel Map

### Channel 1: Generator → Daemon (selectors)

| Field | Value |
|-------|-------|
| Transport | Daemon socket |
| Output mode (generator) | Buffered (raw bytes) |
| Input mode (daemon) | Buffered (raw bytes) for proto < 30, multiplexed (MSG_DATA frames) for proto >= 30 |
| Wire format | Raw bytes from generator -- `write_ndx()` produces compressed NDX, `write_shortint()` produces 2-byte LE.  Daemon reads raw bytes (proto < 30) or transparently unwraps MSG_DATA frames (proto >= 30). |
| Proto gate | All versions |
| Data | Selectors (NDX + iflags + optional attrs), NDX_DONE |

**Source:** Generator output: `.upstream/main.c:1121` (`io_start_buffering_out`).  Daemon input: `.upstream/main.c:1272-1275` (`if (need_messages_from_generator) io_start_multiplex_in(f_in); else io_start_buffering_in(f_in);`).  For proto >= 30, `need_messages_from_generator` is always 1 (`.upstream/compat.c:777`), so daemon input is multiplexed.

### Channel 2: Daemon → Receiver (file data, echoed selectors)

| Field | Value |
|-------|-------|
| Transport | Daemon socket |
| Output mode (daemon) | Multiplexed (MSG_DATA frames) |
| Input mode (receiver) | Multiplexed (MSG_DATA frames) |
| Wire format | MSG_DATA frames wrapping raw protocol bytes |
| Proto gate | Proto ≥ 23 |
| Data | Echoed selectors, sum_head, block checksums, delta fill data, MSG_SUCCESS, MSG_REDO, MSG_NO_SEND |

**Source:** Daemon output: `.upstream/main.c:1266` (`io_start_multiplex_out`).  Receiver input: inherited from `.upstream/main.c:1361` (`io_start_multiplex_in` in `client_run()` receiver path, proto ≥ 23).

### Channel 3: Receiver → Generator (status, file list forwarding)

| Field | Value |
|-------|-------|
| Transport | Internal pipe (`error_pipe`) |
| Output mode (receiver) | Multiplexed (MSG_DATA frames) |
| Input mode (generator) | Multiplexed (MSG_DATA frames) |
| Wire format | MSG_DATA frames wrapping raw protocol bytes |
| Proto gate | All versions |
| Data | `write_int(f_out, NDX_DONE)` (4-byte LE), MSG_STATS, MSG_SUCCESS, file list data (inc_recurse forwarding via `start_flist_forward()`) |

**Source:** Receiver output: `.upstream/main.c:1072` (`io_start_multiplex_out`).  Generator input: `.upstream/main.c:1122` (`io_start_multiplex_in`).

### Channel 4: Daemon → Generator (non-selector messages, conditional)

| Field | Value |
|-------|-------|
| Transport | Daemon socket |
| Output mode (daemon) | Multiplexed (MSG_DATA frames) |
| Input mode (generator) | N/A -- generator does not read from daemon socket after fork |
| Wire format | MSG_* frames (MSG_ERROR, MSG_WARNING, MSG_INFO, etc.) |
| Proto gate | Proto ≥ 23 (for mux), always for stderr messages |
| Data | Error messages, warnings, info messages |

**Note:** The generator does NOT read from the daemon socket after the fork.  Non-DATA messages sent by the daemon (MSG_ERROR, MSG_WARNING, etc.) are read by the receiver via its multiplexed input and forwarded to the generator if needed.  The generator reads only from the internal pipe (Channel 3).

### Mux frame visibility

When I/O mode is multiplexed, `write_buf()`/`read_buf()` transparently wrap and unwrap MSG_DATA frames, so application code never sees mux headers.  A raw socket capture will show 4-byte mux headers before each MSG_DATA payload.  In buffered mode, there are no mux headers and raw bytes go directly on the wire.

## Wire Protocol Step-by-Step

Trace for `rsync -av host::mod/ ./` (pull, proto 32).  Focus on the daemon socket only.

### Step 1: Greeting Exchange (text)

**Direction:** Simultaneous (both sides send, then read)

**Wire format:**
```
@RSYNCD: 32.0 md5 md4\n
```

**Process:** Client (pre-fork) ↔ Daemon

**Source:** `.upstream/compat.c:842` (greeting sent via `io_printf(f_out, "@RSYNCD: %d.%d %s\n", ...)`), `.upstream/clientserver.c:180` (greeting parsed via `sscanf(buf, "@RSYNCD: %d.%d", ...)`).

**Details:**
- Both sides send their greeting simultaneously (simultaneous write, then read).
- Parse version, subprotocol, and digest list.
- Negotiate down to the lower version; subprotocol mismatch causes version downgrade.
- Digest list: space-separated, client preference wins.

### Step 2: Module Selection (text)

**Direction:** Client → Daemon

**Wire format:**
```
mod\n
```

**Process:** Client (pre-fork) → Daemon

**Source:** `.upstream/clientserver.c:233-320` (`start_inband_exchange()` sends module name via `io_printf()`).

### Step 3: Authentication (text, optional)

**Direction:** Daemon → Client (challenge), Client → Daemon (response)

**Wire format:**
```
@RSYNCD: AUTHREQD <base64-challenge>\n
<username> <base64-digest>\n
@RSYNCD: OK\n
```

**Process:** Daemon ↔ Client (pre-fork)

**Source:** `.upstream/clientserver.c:369-377` (auth challenge/response parsed inline), `.upstream/clientserver.c:765` (`auth_server()` sends challenge).

### Step 4: Argument Transmission (text → binary transition)

**Direction:** Client → Daemon

**Wire format (proto ≥ 30, null-terminated):**
```
.\x00-a\x00-v\x00-e.ifxCIvu\x00\x00
```

**Wire format (proto < 30, newline-terminated):**
```
.\n-a\n-v\n\n
```

**Process:** Client (pre-fork) → Daemon

**Source:** `.upstream/clientserver.c:233-320` (`start_inband_exchange()` sends arguments), `.upstream/clientserver.c:1077` (`read_args()` parses arguments).  Null-terminated for proto ≥ 30 (`rl_nulls` set at `.upstream/clientserver.c:229`), newline-terminated otherwise.

**Details:**
- First arg is always `"."` (current directory).
- The `e` flag: everything after `e` in the combined short-options arg is the `client_info` string (feature flags like `i`, `L`, `s`, `f`, `x`, `C`, `I`, `v`, `u`).
- Double null (`\x00\x00`) or double newline (`\n\n`) terminates.


(The binary protocol version exchange at `.upstream/compat.c:600-610` is **skipped for daemon connections** because `remote_protocol` is already set during the greeting exchange.  The guard `if (remote_protocol == 0)` at `.upstream/compat.c:600` is false for daemon connections.  This exchange only happens for SSH/rsh transport.)

### Step 5: Compat Flags Exchange (binary, proto ≥ 30)

**Direction:** Daemon → Client

**Wire format:**
```
Server → Client: varint(compat_flags)
```

**Process:** Daemon → Client (pre-fork)

**Source:** `.upstream/compat.c:712-755` (compat flags setup and exchange in `setup_protocol()`).

**Details:**
- Server builds `compat_flags` based on compile-time capabilities and client's advertised feature flags (from `-e` argument).
- Sent as `write_varint()` (proto ≥ 30) or `write_byte()` (pre-release `V` flag support).
- Client reads as `read_varint()`.
- If `CF_VARINT_FLIST_FLAGS` (`v` flag) is set, xmit flags use varint encoding.

### Step 6: Checksum/Compression Negotiation (binary, when `CF_VARINT_FLIST_FLAGS` set)

**Direction:** Bidirectional

**Wire format:**
```
Server → Client: vstring("md5 md4")       -- checksum list
Server → Client: vstring("zlib")          -- compression list (if compression enabled)
Client → Server: vstring("md5 md4")       -- checksum list
Client → Server: vstring("zlib")          -- compression list (if server sent one)
```

**Process:** Client (pre-fork) ↔ Daemon

**Source:** `.upstream/compat.c:535-571` (`negotiate_the_strings()`).

**Details:**
- A **vstring** is: `length : uint8` (or 2 bytes if high bit set) followed by `data : raw[length]`.
- Each side picks the first algorithm in the **client's list** that also appears in the server's list.
- If `do_negotiated_strings` is 0 (no `v` compat flag), defaults to `"md5"` for proto ≥ 30, `"md4"` otherwise.

### Step 7: Checksum Seed Exchange (binary)

**Direction:** Daemon → Client

**Wire format:**
```
Server → Client: 0x9F 0x3A 0x01 0x00  (checksum_seed as int32 LE, example value)
```

**Process:** Daemon → Client (pre-fork)

**Source:** `.upstream/compat.c:811-817` (checksum seed exchange in `setup_protocol()`).

**Details:**
- Server generates seed as `time(NULL) ^ (getpid() << 6)` if not already set.
- Sent as `write_int(f_out, checksum_seed)` (4 bytes LE).
- Client reads as `read_int(f_in)`.

### Step 8: I/O Mode Transition (internal, no wire data)

**Process:** Both sides

**Source:** `.upstream/main.c:1266` (daemon output), `.upstream/main.c:1318-1325` (client).

**What happens:**
- Daemon: `io_start_multiplex_out(f_out)` at `.upstream/main.c:1266` (proto ≥ 23)
- Daemon: `io_start_multiplex_in(f_in)` at `.upstream/main.c:1273` for proto ≥ 30 (because `need_messages_from_generator` is always 1), or `io_start_buffering_in(f_in)` at `.upstream/main.c:1275` for proto < 30
- Client sender: `io_start_multiplex_out(f_out)` at `.upstream/main.c:1319` (proto ≥ 30)
- Client sender: `io_start_multiplex_in(f_in)` at `.upstream/main.c:1323` (proto ≥ 31 or proto ≥ 23 without filesfrom_host)
- Client receiver (pre-fork): `io_start_multiplex_in(f_in)` at `.upstream/main.c:1361` (proto ≥ 23)

**After this point, the daemon→client channel (Channel 2) flows through the multiplexed I/O layer.**  The generator→daemon channel (Channel 1) remains buffered: the generator writes selectors as raw bytes (buffered output), and the daemon reads them as raw bytes (buffered input for proto < 30, multiplexed input for proto >= 30 because `need_messages_from_generator` is set).

### Step 9: Filter List Transfer (binary)

**Direction:** Client → Daemon

**Wire format:** Mux-wrapped for proto ≥ 30, raw bytes for proto < 30 (filter rules in rsync internal format)

**Process:** Client sender → Daemon

**Source:** `.upstream/main.c:1326` (`send_filter_list(f_out)`), `.upstream/main.c:1276` (`recv_filter_list(f_in)`).

**Details:**
- Sent AFTER mux output is started on the client side (`.upstream/main.c:1319`, proto ≥ 30), so wrapped in MSG_DATA frames for proto ≥ 30.  For proto < 30, client uses buffered output (`.upstream/main.c:1321`) and the filter list is raw bytes.
- Daemon reads via buffered input (raw bytes) for proto < 30, or multiplexed input (transparently unwraps MSG_DATA) for proto ≥ 30 -- the daemon's input mode is set independently of the client's output mode

### Step 10: File List Transfer (binary, mux-wrapped)

**Direction:** Daemon → Client (server sends the file list to the client)

**Wire format:** Mux-wrapped raw bytes (file list entries with delta-encoded xflags)

**Process:** Daemon sender → Client receiver

**Source:** `.upstream/main.c:968` (`send_file_list(f_out, argc, argv)` on server sender side via `do_server_sender()`), `.upstream/main.c:1379` (`recv_file_list(f_in, -1)` on client).

**Wire layout per entry:**
```
xflags  : varint (when CF_VARINT_FLIST_FLAGS) or byte/shortint
[name prefix_len] : uint8 (if XMIT_SAME_NAME)
name_suffix_len   : uint8 (or varint if XMIT_LONG_NAME)
name_suffix       : raw[name_suffix_len]
[file_size]       : varlong30(3) (proto ≥ 30) or longint (older)
[mtime]           : varlong(4) (proto ≥ 30) or uint32 LE (older)
[mod_nsec]        : varint (if XMIT_MOD_NSEC, proto ≥ 31)
[mode]            : int32 LE (if !XMIT_SAME_MODE)
[atime]           : varlong(4) (if atimes enabled, !XMIT_SAME_ATIME)
[uid]             : varint (if preserve_uid, !XMIT_SAME_UID)
[gid]             : varint (if preserve_gid, !XMIT_SAME_GID)
[checksum]        : raw[csum_len] (if always_checksum, regular file)
```

**End-of-list:** `NDX_DONE` sent as compressed NDX (1 byte `0x00` for proto ≥ 30).

### Step 11: Phase Exchange (binary)

**Direction:** Bidirectional

**Wire format:**
```
Generator → Daemon: 0x00  (NDX_DONE as compressed NDX, 1 byte)
Daemon → Generator: 0x00  (NDX_DONE as compressed NDX, 1 byte, echoed by sender)
```

**Process:** Generator ↔ Daemon

**Source:** `.upstream/generator.c:2375-2450` (generator phase exchange in `generate_files()`), `.upstream/sender.c:236-260` (daemon sender phase exchange in `send_files()`).

**Details for proto ≥ 29 (max_phase = 2):**
1. Generator reads NDX_DONE from daemon (file list complete) → phase=1, writes NDX_DONE to daemon.
2. Daemon reads generator's NDX_DONE → phase=1, writes NDX_DONE to generator.
3. Generator reads daemon's NDX_DONE → phase=2, writes NDX_DONE to daemon.
4. Daemon reads generator's NDX_DONE → phase=2, writes NDX_DONE to generator.
5. Generator reads daemon's NDX_DONE → phase=3 > max_phase=2, exits loop.
6. Daemon then reads selectors (or NDX_DONE if no files to transfer).

**Critical distinction:** Generator uses `write_ndx(f_out, NDX_DONE)` which produces 1 byte `0x00` on the daemon socket (compressed NDX, buffered output).  Receiver uses `write_int(f_out, NDX_DONE)` which produces 4 bytes `0xFFFFFFFF` on the internal pipe (fixed-width int, multiplexed output).

### Step 12: Selector Loop (binary)

**Direction:** Generator → Daemon (selectors), Daemon → Receiver (data)

**Wire format (generator → daemon, buffered):**
```
ndx       : compressed NDX (proto ≥ 30) or int32 LE (older)
iflags    : uint16 LE (proto ≥ 29)
[type]    : uint8 (if ITEM_BASIS_TYPE_FOLLOWS)
[xname]   : vstring (if ITEM_XNAME_FOLLOWS)
```

**Wire format (daemon → receiver, mux-wrapped):**
```
MSG_DATA: [echoed selector bytes]
MSG_DATA: [sum_head: count, blength, s2length, remainder -- all int32 LE]
MSG_DATA: [block checksums: sum1[4] + sum2[s2length] per block]
MSG_DATA: [delta fill data: raw bytes for mismatched regions]
MSG_SUCCESS: [ndx as int32 LE]
```

**Process:** Generator → Daemon (Channel 1, buffered), Daemon → Receiver (Channel 2, multiplexed)

**Source:** `.upstream/generator.c:586-591` (generator sends selectors via `sock_f_out`), `.upstream/sender.c:236-360` (daemon reads selectors via `read_ndx_and_attrs` and sends data).

**Details:**
- Generator sends selectors as **raw bytes** (buffered output, no mux wrapping).
- Daemon reads selectors via **buffered input** (raw bytes) for proto < 30, or **multiplexed input** (transparently unwraps MSG_DATA) for proto >= 30 (because `need_messages_from_generator` is always 1 at `.upstream/compat.c:777`).
- Daemon echoes each selector back to the client via **multiplexed output** (MSG_DATA frames).
- Receiver reads echoed selectors via **multiplexed input** (transparently unwraps MSG_DATA).
- For TRANSFER selectors, daemon sends sum_head + block checksums + delta fill data.
- For non-TRANSFER selectors, daemon just echoes the selector.

### Step 13: Final Goodbye (binary)

**Direction:** Bidirectional

**Wire format:**
```
Generator → Daemon: 0x00  (NDX_DONE as compressed NDX)
Daemon → Generator: 0x00  (NDX_DONE as compressed NDX, echoed)
[proto ≥ 31: Generator → Daemon: 0x00, Daemon → Generator: 0x00]
```

**Process:** Generator ↔ Daemon

**Source:** `.upstream/main.c:893-924` (`read_final_goodbye()`), `.upstream/generator.c:2375-2450` (`generate_files()` phase loop).

**Details for proto ≥ 31:**
- Extra round-trip: after first NDX_DONE exchange, server writes another NDX_DONE and reads another from client.
- For proto < 29: server reads `read_int(f_in)` and verifies `NDX_DONE`.
- For proto 29-30: server reads via `read_ndx_and_attrs()` and verifies `NDX_DONE`.

### Step 14: Stats Exchange (binary, mux-wrapped)

**Direction:** Daemon → Client

**Wire format:**
```
varlong30(total_read)
varlong30(total_written)
varlong30(total_size)
[proto ≥ 29: varlong30(flist_buildtime), varlong30(flist_xfertime)]
```

**Process:** Daemon sender → Client receiver

**Source:** `.upstream/main.c:325-385` (`handle_stats()`).

**Details:**
- Only sent by the server when `am_sender` is set.
- Sent AFTER the selector loop and final goodbye.
- Client reads via multiplexed input.

## I/O Mode Resolution

### How `write_buf()`/`read_buf()` resolve to different wire formats

The upstream `iobuf` system has two modes: **multiplexed** and **buffered**.  The mode is set per-file-descriptor by calling `io_start_multiplex_*()` or `io_start_buffering_*()`.

### Multiplexed output (`io_start_multiplex_out`)

**Source:** `.upstream/io.c:2447-2463`

When multiplexed output is enabled:
1. `iobuf.out_empty_len` is set to 4, which makes `OUT_MULTIPLEXED` true.
2. `io_start_buffering_out(fd)` is called internally -- so buffered output is the underlying mechanism.
3. `iobuf.raw_data_header_pos` is set to reserve space for the first 4-byte mux header.

When `write_buf()` (`.upstream/io.c:2255`) is called:
1. Bytes are accumulated in `iobuf.out` circular buffer.
2. On flush (via `perform_io()` at `.upstream/io.c:562`), a 4-byte MSG_DATA header is prepended: `SIVAL(hdr, 0, ((MPLEX_BASE + (int)MSG_DATA)<<24) + len)`.
3. Multiple `write_buf()` calls are batched into a single MSG_DATA frame.
4. A new mux header is reserved for the next batch.

**Wire format:** `header : uint32 LE ((7 + 0) << 24 | payload_len)` + `payload : raw[payload_len]`

### Multiplexed input (`io_start_multiplex_in`)

**Source:** `.upstream/io.c:2466-2472`

When multiplexed input is enabled:
1. `iobuf.in_multiplexed` is set to 1, which makes `IN_MULTIPLEXED` true.
2. `io_start_buffering_in(fd)` is called internally.

When `read_buf()` is called:
1. If `IN_MULTIPLEXED` and buffer is empty, `read_a_msg()` (`.upstream/io.c:1495`) is called.
2. `read_a_msg()` reads a 4-byte header via `raw_read_int()`, extracts msg code and length.
3. For MSG_DATA: `iobuf.raw_input_ends_before` marks where the payload ends.
4. `read_buf()` reads from the transparent byte stream, fetching more MSG_DATA frames as needed.
5. Non-DATA messages (MSG_SUCCESS, MSG_ERROR, etc.) are dispatched to handlers in `read_a_msg()`.

**Wire format:** Reader transparently unwraps MSG_DATA frames.  Application code sees a raw byte stream.

### Buffered output (`io_start_buffering_out`)

**Source:** `.upstream/io.c:1369-1386`

When buffered output is enabled:
1. `iobuf.out_fd` is set to the fd.
2. `iobuf.out_empty_len` remains 0, so `OUT_MULTIPLEXED` is false.

When `write_buf()` is called:
1. Bytes are accumulated in `iobuf.out` circular buffer.
2. On flush, bytes are written directly to the socket -- **no mux header added**.

**Wire format:** Raw bytes, no framing.

### Buffered input (`io_start_buffering_in`)

**Source:** `.upstream/io.c:1388-1404`

When buffered input is enabled:
1. `iobuf.in_fd` is set to the fd.
2. `iobuf.in_multiplexed` remains 0, so `IN_MULTIPLEXED` is false.

When `read_buf()` is called:
1. Reads directly from the socket via the buffered input path.
2. No mux unwrapping -- raw bytes.

**Wire format:** Raw bytes, no framing.

### Key implication for `write_ndx()`

`write_ndx()` (`.upstream/io.c:2318`) uses `write_buf()` internally.  The wire format depends on the I/O mode of the target fd:

- **Buffered output:** `write_ndx()` writes compressed NDX directly as raw bytes.  NDX_DONE (-1) = 1 byte `0x00` for proto ≥ 30 (or 4 bytes `0xFFFFFFFF` if `read_batch` is set, since `write_ndx()` falls back to `write_int()` for batch mode).
- **Multiplexed output:** `write_ndx()` writes compressed NDX to the iobuf buffer, which is later flushed as a MSG_DATA frame.  NDX_DONE (-1) = 1 byte `0x00` inside a MSG_DATA frame (same `read_batch` exception applies).

The compressed NDX encoding is the same in both cases; only the framing differs.

### Key implication for `write_int()`

`write_int()` (`.upstream/io.c:2157`) always writes 4 bytes LE.  The wire format depends on the I/O mode:

- **Buffered output:** 4 raw bytes on the wire.
- **Multiplexed output:** 4 bytes inside a MSG_DATA frame.

## Protocol Version I/O Mode Matrix

### Client Side (sender path, `client_run()`)

**Source:** `.upstream/main.c:1318-1325`

| Direction | Proto 27 | Proto 30 | Proto 31 | Proto 32 |
|-----------|----------|----------|----------|----------|
| Output → Daemon | buffered | multiplexed | multiplexed | multiplexed |
| Input ← Daemon | buffered* | multiplexed* | multiplexed | multiplexed |

\* Input is buffered when `filesfrom_host` is set and proto < 31.  Otherwise multiplexed for proto ≥ 23.

**Source lines:** Output: `.upstream/main.c:1318-1321`.  Input: `.upstream/main.c:1322-1325`.

### Client Side (receiver path, pre-fork in `client_run()`)

**Source:** `.upstream/main.c:1360-1365`

| Direction | Proto 27 | Proto 30 | Proto 31 | Proto 32 |
|-----------|----------|----------|----------|----------|
| Output → Daemon | buffered | buffered | buffered* | buffered* |
| Input ← Daemon | multiplexed | multiplexed | multiplexed | multiplexed |

\* Output is multiplexed only when `need_messages_from_generator` is set (e.g., `remove_source_files`).  For the receiver path, `need_messages_from_generator` is typically 0 (it's set by compat.c:777 only for `am_sender`), so output is normally buffered.

**Source lines:** Input: `.upstream/main.c:1360-1361` (`if (protocol_version >= 23) io_start_multiplex_in(f_in);`).  Output: `.upstream/main.c:1362-1365` (`if (need_messages_from_generator) io_start_multiplex_out(f_out); else io_start_buffering_out(f_out);`).

### Client Side (generator, post-fork in `do_recv()`)

**Source:** `.upstream/main.c:1121-1122` (all protocol versions, no version gate)

| Direction | All Protos |
|-----------|------------|
| Output → Daemon (`sock_f_out`) | buffered |
| Input ← Receiver pipe (`f_in`) | multiplexed |

**Source lines:** Output: `.upstream/main.c:1121`.  Input: `.upstream/main.c:1122`.

### Client Side (receiver, post-fork in `do_recv()`)

**Source:** `.upstream/main.c:1071-1072` (all protocol versions, no version gate)

| Direction | All Protos |
|-----------|------------|
| Input ← Daemon (`f_in`) | multiplexed (inherited from pre-fork) |
| Output → Generator pipe (`f_out`) | multiplexed |

**Source lines:** Input: inherited from `.upstream/main.c:1361` (`io_start_multiplex_in` in `client_run()`).  Overridden to buffered by `.upstream/main.c:1071` if `read_batch`.  Output: `.upstream/main.c:1072`.

### Server Side (daemon, `start_server()`)

**Source:** `.upstream/main.c:1265-1275`

| Direction | Proto 27 | Proto 30 | Proto 31 | Proto 32 |
|-----------|----------|----------|----------|----------|
| Output → Client | multiplexed | multiplexed | multiplexed | multiplexed |
| Input ← Client | buffered | multiplexed | multiplexed | multiplexed |

For `am_sender`: input is buffered for proto < 30, multiplexed for proto >= 30 (because `need_messages_from_generator` is set unconditionally at `.upstream/compat.c:777` for all proto >= 30 sender connections).  For `am_receiver` (push), `do_server_recv()` sets its own I/O mode separately (see table below).

**Source lines:** Output: `.upstream/main.c:1265-1266`.  Input: `.upstream/main.c:1270-1275` (`if (need_messages_from_generator) io_start_multiplex_in(f_in); else io_start_buffering_in(f_in);`).

### Server Side (daemon recv path, `do_server_recv()`)

**Source:** `.upstream/main.c:1185-1188`

| Direction | Proto 27 | Proto 30 | Proto 31 | Proto 32 |
|-----------|----------|----------|----------|----------|
| Input ← Client | buffered | multiplexed | multiplexed | multiplexed |

**Source lines:** `.upstream/main.c:1185-1188`.

## Known Pitfalls

### 1. Generator `write_ndx()` vs Receiver `write_int()` for NDX_DONE

Same semantic meaning (NDX_DONE = -1), but different wire format and different channel.

- **Generator** (`generate_files()`, `.upstream/generator.c:2390`): `write_ndx(f_out, NDX_DONE)` → compressed NDX → 1 byte `0x00` on the daemon socket (buffered output, raw bytes).  Here `f_out` is the daemon socket fd.
- **Receiver** (`recv_files()`, `.upstream/receiver.c:696`): `write_int(f_out, NDX_DONE)` → 4-byte LE int32 → 4 bytes `0xFF 0xFF 0xFF 0xFF` on the internal pipe (multiplexed output, inside MSG_DATA frame).

The generator talks to the daemon (Channel 1, buffered) and the receiver talks to the generator (Channel 3, multiplexed).  The receiver always uses `write_int()` because the generator's `wait_for_receiver()` (`.upstream/io.c:1749`) reads via `read_int(iobuf.in_fd)` (`.upstream/io.c:1756`).

### 2. `read_ndx_and_attrs()` reads from one fd and echoes to another

The echo may use a different I/O mode than the read.  Source: `.upstream/rsync.c:322-433`.

`read_ndx_and_attrs(f_in, f_out, ...)` reads a selector from `f_in` and the caller (e.g., `write_ndx_and_attrs()` in `.upstream/sender.c:184-199`) echoes it to `f_out`.  If `f_in` is buffered and `f_out` is multiplexed, the selector arrives as raw bytes but is echoed as MSG_DATA frames.

**Example:** Daemon reads selector from generator (Channel 1, buffered/raw) and echoes to client receiver (Channel 2, multiplexed/MSG_DATA).  The selector bytes are the same, but the wire framing differs.

### 3. Mux frame headers are transparent to application code but visible on the wire

A raw-byte reader will see mux headers as data.  When I/O mode is multiplexed, `write_buf()`/`read_buf()` transparently wrap and unwrap MSG_DATA frames.  Application code that calls `write_ndx()` or `read_ndx()` never sees the 4-byte mux header.  However, a packet capture or raw socket reader will see:
```
[4-byte header: (7 << 24) | payload_len] [payload bytes]
```

If you read the socket as raw bytes when mux is enabled, you'll get garbled data (mux headers mixed with protocol bytes).

### 4. `sock_f_out` vs `f_out` -- generator fd redirection

Generator redirects `f_in` to the internal pipe but `f_out` and `sock_f_out` both still point to the daemon socket.  Source: `.upstream/main.c:1119` (generator redirects `f_in` to `error_pipe[0]`), `.upstream/main.c:1121` (generator sets buffered output on `f_out` which is the daemon socket at this point).

After the fork:
- Generator: `f_in` = internal pipe (from receiver), `f_out` = daemon socket, `sock_f_out` = daemon socket.
- Receiver: `f_in` = daemon socket, `f_out` = internal pipe (to generator), `sock_f_out` = -1.

The generator sends selectors to `sock_f_out` (daemon socket) via `write_ndx(sock_f_out, ndx)` (`.upstream/generator.c:586`).  In `generate_files(f_out, local_name)`, the `f_out` parameter is the daemon socket fd (NOT the internal pipe).  The generator writes NDX_DONE and selectors to the daemon socket via `write_ndx(f_out, ndx)` (`.upstream/generator.c:2390`) and reads status messages, NDX_DONE, and file list data (inc_recurse) from the receiver via `wait_for_receiver()` which reads from `iobuf.in_fd` (the internal pipe).

### 5. `io_start_buffering_out(f_out)` in generator overrides earlier mux setup

The generator's output to the daemon socket is buffered, even though `client_run()` may have set it to multiplexed before the fork.  Source: `.upstream/main.c:1319` (client_run sets `io_start_multiplex_out(f_out)` for proto ≥ 30), `.upstream/main.c:1121` (generator overrides with `io_start_buffering_out(f_out)`).

Before the fork, `client_run()` sets up multiplexed output on the daemon socket.  After the fork, the generator calls `io_start_buffering_out(f_out)` which resets the output mode to buffered.  This is intentional -- the generator sends selectors as raw bytes, not mux-wrapped.

### 6. `write_ndx_and_attrs()` in sender.c echoes selectors

The echo happens in a separate function call, not inside `read_ndx_and_attrs()`.  Source: `.upstream/sender.c:184-199` (`write_ndx_and_attrs()`), `.upstream/sender.c:294,349,442` (callers).

When the daemon processes a selector, it calls `read_ndx_and_attrs(f_in, f_out, ...)` to read it, then `write_ndx_and_attrs(f_out, ...)` to echo it.  The echo uses `write_ndx()` (compressed NDX) on the multiplexed output channel, so the echoed selector appears as a MSG_DATA frame on the wire.

### 7. Phase exchange uses different functions on client vs server

Generator uses `write_ndx()` (compressed NDX), receiver uses `write_int()` (4-byte LE).  Source: Generator: `.upstream/generator.c:2390` (`write_ndx(f_out, NDX_DONE)`).  Receiver: `.upstream/receiver.c:696` (`write_int(f_out, NDX_DONE)`).

The daemon sender also uses `write_ndx()` for the phase exchange (`.upstream/sender.c:260`), matching the generator's format.  The receiver uses `write_int()` because the generator's `wait_for_receiver()` (`.upstream/io.c:1749`) expects 4-byte LE ints via `read_int(iobuf.in_fd)`.

### 8. Stats exchange uses `handle_stats()` with different behavior per process

`handle_stats(f)` behaves differently depending on whether `f` is -1, the process role, and whether it's a daemon.  Source: `.upstream/main.c:325-385`.

- Generator: `handle_stats(-1)` -- does nothing (returns early at `.upstream/main.c:340`, `if (am_generator)` check).
- Receiver: `handle_stats(f_in)` -- reads stats from daemon socket (`.upstream/main.c:367-374`).
- Daemon sender: `handle_stats(f_out)` -- writes stats to client socket (`.upstream/main.c:349-355`).
- Daemon receiver: `handle_stats(f_out)` -- returns early (`.upstream/main.c:343-345`, `if (am_daemon)` && `!am_sender`).

## Integer Encoding Formats

### Fixed-Width Integers (`write_int` / `read_int`)

4 bytes, **little-endian**, signed int32.

**Source:** `.upstream/io.c:2157-2163` (`write_int`), `.upstream/io.c:1795-1811` (`read_int`).

### Variable-Length Integers (`varint`, protocol ≥ 30)

Compact encoding for signed int32.  Uses a lookup table (`int_byte_extra[]`) indexed by `first_byte / 4` to determine the number of extra bytes.

**Source:** `.upstream/io.c:2164-2184` (`write_varint`), `.upstream/io.c:1816-1846` (`read_varint`).

**Lookup table (`int_byte_extra`):** 64 entries indexed by `first_byte / 4`.

| Index range | Byte range | Extra bytes |
|-------------|------------|-------------|
| 0x00-0x1F | 0x00-0x7F | 0 |
| 0x20-0x2F | 0x80-0xBF | 1 |
| 0x30-0x37 | 0xC0-0xDF | 2 |
| 0x38-0x3B | 0xE0-0xE7 | 3 |
| 0x3C-0x3D | 0xE8-0xEB | 4 |
| 0x3E-0x3F | 0xEC-0xED | 5 (overflow) |

### Variable-Length Long Integers (`varlong`, protocol ≥ 30)

Similar to varint but for int64 with configurable minimum byte count.

**Source:** `.upstream/io.c:2186-2220` (`write_varlong`), `.upstream/io.c:1848-1887` (`read_varlong`).

Common uses: file sizes (`write_varlong30(f, size, 3)`), timestamps (`write_varlong(f, time, 4)`).

### Legacy Long Integers (`longint`, protocol < 30)

For values in `[0, 0x7FFFFFFF]`: `write_int(value)` -- 4 bytes LE.
For larger values (or negative): sentinel `0xFFFFFFFF` (4 bytes) followed by full 8-byte LE int64.  Total: 12 bytes.

### Short Integers (`write_shortint` / `read_shortint`)

2 bytes, **little-endian**, unsigned uint16.

**Source:** `.upstream/io.c:2149-2155` (`write_shortint`), `.upstream/io.c:1788-1793` (`read_shortint`).

Used for: extended xflags (when `XMIT_EXTENDED_FLAGS` is set, proto ≥ 28), item flags in selector protocol (proto ≥ 29).

### Compressed NDX (protocol ≥ 30)

Stateful delta encoding.  Initial state: `prev_positive = -1`, `prev_negative = 1`.

**Source:** `.upstream/io.c:2318-2363` (`write_ndx`), `.upstream/io.c:2365-2400` (`read_ndx`).

**Writing:**
1. `ndx == NDX_DONE (-1)`: single byte `0x00` (no side effects).
2. `ndx >= 0`: `diff = ndx - prev_positive`, update `prev_positive = ndx`.
3. `ndx < 0` (not NDX_DONE): prefix `0xFF`, treat as positive `ndx = -ndx`, `diff = ndx - prev_negative`, update `prev_negative = ndx`.

Diff encoding (cases 2-3):
- `1 <= diff <= 253`: single byte = diff.
- `diff == 0` or `254 <= diff <= 32767`: `0xFE` + 2-byte big-endian diff.
- `diff < 0` or `diff > 32767`: `0xFE` + 4 bytes encoding absolute index: `(absNdx >> 24) | 0x80`, `absNdx & 0xFF`, `(absNdx >> 8) & 0xFF`, `(absNdx >> 16) & 0xFF`.

**Reading:**
1. `0x00` → `NDX_DONE (-1)`, no state change.
2. `0xFF` → read next byte, use `prev_negative` tracker, negate result.
3. `0x01-0xFD` → `result = byte + prev_positive`, update tracker (1-byte diff).
4. `0xFE` → read next byte:
   - High bit set: 4-byte form, absolute index (not diff).
   - High bit clear: 2-byte form, big-endian diff added to tracker.

### vstring

**Source:** `.upstream/io.c:2297-2316` (`write_vstring`), `.upstream/io.c:2004-2021` (`read_vstring`).

Format: `length : uint8` (or 2 bytes if high bit set) + `data : raw[length]`.

If `len & 0x80`: actual length = `(len & 0x7F) * 256 + next_byte`.

## Multiplexed I/O Layer

### Frame Format

**Source:** `.upstream/rsync.h:203` (`MPLEX_BASE = 7`), `.upstream/io.c:688` (header construction), `.upstream/io.c:1506-1510` (header parsing).

Every frame: 4-byte header + payload.  Header is a **little-endian uint32**:
```
header = ((MPLEX_BASE + msgCode) << 24) | length
where MPLEX_BASE = 7, length in bits [0..23] (max ~16MB per frame)
```

### Message Codes (`enum msgcode`)

**Source:** `.upstream/rsync.h:286-302`.

| Code | Name | Description | Payload | Direction |
|------|------|-------------|---------|-----------|
| 0 | `MSG_DATA` | Raw protocol data | Variable | Both |
| 1 | `MSG_ERROR_XFER` | Transfer error | Text | Server→Client |
| 2 | `MSG_INFO` | Info log | Text | Server→Client |
| 3 | `MSG_ERROR` | Error (proto ≥ 30) | Text | Both |
| 4 | `MSG_WARNING` | Warning (proto ≥ 30) | Text | Both |
| 5 | `MSG_ERROR_SOCKET` | Socket error | Text | Receiver→Generator |
| 6 | `MSG_LOG` | Log message | Text | Receiver→Generator |
| 7 | `MSG_CLIENT` | Client log | Text | -- |
| 8 | `MSG_ERROR_UTF8` | UTF-8 error | Text | Receiver→Generator |
| 9 | `MSG_REDO` | Reprocess flist index | int32 LE | Sender→Receiver |
| 10 | `MSG_STATS` | Transfer stats | int64 total_read | Receiver→Sender |
| 22 | `MSG_IO_ERROR` | I/O error flags | int32 bitmask | Both |
| 33 | `MSG_IO_TIMEOUT` | Timeout value | int32 seconds | Server→Client |
| 42 | `MSG_NOOP` | Keep-alive (proto 30 only) | 0 bytes | Sender→Receiver |
| 86 | `MSG_ERROR_EXIT` | Error exit signal | 0 or int32 | Both |
| 100 | `MSG_SUCCESS` | Transfer complete | int32 ndx | Sender→Receiver |
| 101 | `MSG_DELETED` | File deleted | Text filename | Receiver→Sender |
| 102 | `MSG_NO_SEND` | Failed to open file | int32 ndx | Both |

### iobuf Buffering Model

**Source:** `.upstream/io.c` -- `iobuf.out` (data buffer), `iobuf.msg` (message buffer), `iobuf.in` (input buffer).

Upstream uses two separate output paths with circular buffers (32KB default, `IO_BUFFER_SIZE`):

1. **`iobuf.out`** -- for `MSG_DATA`.  Application code calls `write_buf()`/`write_int()`/`write_byte()` which accumulate raw bytes.  On flush, bytes are wrapped in a `MSG_DATA` frame.  **Multiple small writes are batched into larger frames.**

2. **`iobuf.msg`** -- for non-DATA messages.  Application code calls `send_msg()` which buffers with its own header.  On flush, **any pending `iobuf.out` data is flushed first** (ensuring MSG_DATA frames precede control messages), then message frames are sent.

**Input:** `iobuf.in` -- transparent byte stream from incoming `MSG_DATA` frames.  Application code calls `read_buf()`/`read_int()`/`read_byte()` which read from this buffer.  When empty, the iobuf layer reads the next `MSG_DATA` frame and refills.  **Application never sees mux headers.**

## File List Wire Format

### Xmit Flags Encoding

**Source:** `.upstream/flist.c` -- `send_file_list()`, `write_xmit_flags()`.

**Protocol ≥ 30 with `CF_VARINT_FLIST_FLAGS`:** Varint encoding.  If xflags is zero, send varint(`XMIT_EXTENDED_FLAGS`) as sentinel (zero signals end-of-list).

**Protocol 28-31:**
1. If `xflags == 0` and not a directory: inject `XMIT_TOP_DIR`.
2. If `(xflags & 0xFF00) || xflags == 0`: set `XMIT_EXTENDED_FLAGS` bit, `write_shortint(xflags)` (2 bytes LE).
3. Otherwise: `write_byte(xflags)`.

**Protocol < 28:** If no flags set: add harmless flag (`XMIT_LONG_NAME` for dirs, `XMIT_TOP_DIR` for files).  Then `write_byte(xflags)`.

### Xmit Flag Bits

```
XMIT_TOP_DIR          = 1 << 0
XMIT_SAME_MODE        = 1 << 1
XMIT_EXTENDED_FLAGS   = 1 << 2
XMIT_SAME_UID         = 1 << 3
XMIT_SAME_GID         = 1 << 4
XMIT_SAME_NAME        = 1 << 5
XMIT_LONG_NAME        = 1 << 6
XMIT_SAME_TIME        = 1 << 7
XMIT_SAME_RDEV_MAJOR  = 1 << 8  (proto ≥ 28)
XMIT_HLINKED          = 1 << 9  (proto ≥ 28)
XMIT_USER_NAME_FOLLOWS= 1 << 10 (proto ≥ 30)
XMIT_GROUP_NAME_FOLLOWS=1 << 11 (proto ≥ 30)
XMIT_HLINK_FIRST      = 1 << 12 (proto ≥ 30)
XMIT_MOD_NSEC         = 1 << 13 (proto ≥ 31)
XMIT_SAME_ATIME       = 1 << 14
XMIT_CRTIME_EQ_MTIME  = 1 << 17 (varint xflags)
```

### File Entry Wire Layout

```
1. [if XMIT_SAME_NAME] prefix_length : uint8
2. name_suffix_length : uint8 (or varint if XMIT_LONG_NAME)
3. name_suffix : raw[name_suffix_length]
4. [if XMIT_HLINKED, !XMIT_HLINK_FIRST, proto ≥ 30] hlink_ndx : varint
5. file_size : varlong30(3) (proto ≥ 30) or longint (older)
6. [if !XMIT_SAME_TIME] mtime : varlong(4) (proto ≥ 30) or uint32 LE (older)
7. [if XMIT_MOD_NSEC, proto ≥ 31] mod_nsec : varint
8. [if crtimes, !XMIT_CRTIME_EQ_MTIME] crtime : varlong(4)
9. [if !XMIT_SAME_MODE] mode : int32 LE
10. [if atimes, not dir, !XMIT_SAME_ATIME] atime : varlong(4)
11. [if preserve_uid, !XMIT_SAME_UID] uid : varint (proto ≥ 30) or int32 LE (older)
    [if XMIT_USER_NAME_FOLLOWS] username : vstring
12. [if preserve_gid, !XMIT_SAME_GID] gid : varint (proto ≥ 30) or int32 LE (older)
    [if XMIT_GROUP_NAME_FOLLOWS] groupname : vstring
13. [if device, !XMIT_SAME_RDEV_MAJOR] rdev_major : varint30/byte
    [device minor] : varies by protocol version
14. [if symlink] symlink_target : vstring-like (length + data)
15. [if always_checksum, regular file] checksum[flist_csum_len]
```

### End-of-List Markers

| Value | Meaning | Encoding (proto ≥ 30) |
|-------|---------|----------------------|
| `NDX_DONE` (-1) | All file lists complete | Compressed NDX (`0x00`) |
| `NDX_FLIST_EOF` (-2) | End of sub-list (inc_recurse) | Compressed NDX (prefix `0xFF`) |
| Positive | Next subdirectory index | Compressed NDX |

## Checksum & Delta Transfer Protocol

### SumHead (`write_sum_head` / `read_sum_head`)

**Source:** `.upstream/io.c:2025-2085` (`read_sum_head` at line 2025, `write_sum_head` at line 2072).

All fields are **int32 LE**:
```
count     : int32  // block count (0 = empty file)
blength   : int32  // block size
s2length  : int32  // strong hash length (only if proto ≥ 27)
remainder : int32  // final partial block size
```

### Block Checksums

For each block `i` in `[0..count-1]`:
```
sum1[i] : raw[csum_length]  // rolling checksum (always 4 bytes)
sum2[i] : raw[s2length]     // strong hash (MD4/MD5 = 16, SHA-256 = 32, etc)
```

### Checksum Algorithms

**Checksum1 (rolling):** Adler-32-inspired.  **Source:** `.upstream/checksum.c` -- `get_checksum1()`.

**Checksum2 (strong):** Depends on negotiated digest.  Seed is 4 bytes LE.

| Algorithm | Digest length | Seed order |
|-----------|--------------|------------|
| MD4 | 16 | `MD4(data + seed)` |
| MD5 (non-OpenSSL, `proper_seed_order`) | 16 | `MD5(seed + data)` |
| MD5 (non-OpenSSL, legacy) | 16 | `MD5(data + seed)` |
| MD5 (OpenSSL) | 16 | `MD5(seed + data)` |
| SHA-1/256/512 | 20/32/64 | `hash(seed + data)` |
| XXH64/XXH3 | 8/16 | Seed as hash parameter |

**`proper_seed_order`** is set via `CF_CHKSUM_SEED_FIX` compat flag (`.upstream/compat.c:748`).

## Selector Protocol (Phase 13)

### Selector Wire Format

**Source:** `.upstream/generator.c:586-591` (generator sends), `.upstream/sender.c:184-199` (daemon echoes).

```
ndx       : compressed NDX (proto ≥ 30) or int32 LE (older)
iflags    : uint16 LE (proto ≥ 29)
[type]    : uint8 (if ITEM_BASIS_TYPE_FOLLOWS)
[xname]   : vstring (if ITEM_XNAME_FOLLOWS)
```

**On the daemon socket (generator → daemon):** Raw bytes (buffered output).
**Echoed back (daemon → receiver):** MSG_DATA frames (multiplexed output).

### Item Flags

**Source:** `.upstream/rsync.h` -- `ITEM_*` constants.

| Flag | Bit | Meaning |
|------|-----|---------|
| `ITEM_REPORT_ATIME` | 0 | Report access time changes |
| `ITEM_REPORT_CHANGE` | 1 | Report any changes |
| `ITEM_REPORT_SIZE` | 2 | Report size changes |
| `ITEM_REPORT_TIME` | 3 | Report time changes |
| `ITEM_REPORT_PERMS` | 4 | Report permission changes |
| `ITEM_REPORT_OWNER` | 5 | Report owner changes |
| `ITEM_REPORT_GROUP` | 6 | Report group changes |
| `ITEM_REPORT_ACL` | 7 | Report ACL changes |
| `ITEM_REPORT_XATTR` | 8 | Report xattr changes |
| `ITEM_REPORT_CRTIME` | 10 | Report create time changes |
| `ITEM_BASIS_TYPE_FOLLOWS` | 11 | fnamecmp_type byte follows |
| `ITEM_XNAME_FOLLOWS` | 12 | Alternate filename follows |
| `ITEM_IS_NEW` | 13 | File is new on receiver |
| `ITEM_LOCAL_CHANGE` | 14 | Local change on receiver |
| `ITEM_TRANSFER` | 15 | Request file data transfer |
| `ITEM_MISSING_DATA` | 16 | Client has no local copy |
| `ITEM_DELETED` | 17 | File was deleted |
| `ITEM_MATCHED` | 18 | File matched |

For proto < 29: `iflags` not sent, defaults to `ITEM_TRANSFER | ITEM_MISSING_DATA`.

## Verification Answers

1. **Generator selector output I/O mode:** Buffered (raw bytes).  Source: `.upstream/main.c:1121` (`io_start_buffering_out(f_out)`).  The generator calls `write_ndx(sock_f_out, ndx)` (`.upstream/generator.c:586`) which writes compressed NDX as raw bytes.

2. **Daemon file data output I/O mode:** Multiplexed (MSG_DATA frames).  Source: `.upstream/main.c:1266` (`io_start_multiplex_out(f_out)` for proto ≥ 23).

3. **Receiver daemon socket input I/O mode:** Multiplexed for proto ≥ 23.  Source: `.upstream/main.c:1360-1361` (`if (protocol_version >= 23) io_start_multiplex_in(f_in);` in `client_run()` receiver path).

4. **NDX_DONE wire format:**
   - Daemon socket (generator → daemon): 1 byte `0x00` (compressed NDX, buffered) for proto ≥ 30 && !read_batch.  Falls back to 4 bytes `0xFFFFFFFF` (`write_int()`) in batch mode.  Source: `.upstream/io.c:2324-2337` (`write_ndx()`).
   - Internal pipe (receiver → generator): 4 bytes `0xFF 0xFF 0xFF 0xFF` (int32 LE of -1, multiplexed).  Source: `.upstream/receiver.c:696` (`write_int(f_out, NDX_DONE)`).

5. **Daemon input I/O mode for proto 32 pull:** Multiplexed (MSG_DATA frames).  Source: `.upstream/main.c:1272-1273` (`if (need_messages_from_generator) io_start_multiplex_in(f_in);`).  For proto >= 30 sender connections, `need_messages_from_generator` is always 1 (`.upstream/compat.c:777`).
