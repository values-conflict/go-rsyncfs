# Rsync Daemon Line Protocol Specification (`rsync://`)

## Overview

The rsync daemon protocol is a text-based, request-response line protocol that runs over TCP (default port **873**). It consists of four distinct phases:

1. **Greeting Exchange** — Version negotiation and capability advertisement
2. **Module Selection & Authentication** — Module binding with optional challenge-response auth
3. **Argument Transmission** — Client sends rsync command-line arguments to the server
4. **Data Transfer** — Binary multiplexed protocol for file lists, checksums, and deltas

The first three phases use newline-terminated text lines (or null-terminated strings in newer protocols). Phase 4 switches to a binary multiplexed I/O layer that carries both raw data and control messages over the same socket.

---

## Table of Contents

1. [Connection Establishment](#1-connection-establishment)
2. [Proxy Protocol Header (Optional)](#2-proxy-protocol-header-optional)
3. [Phase 1: Greeting Exchange](#3-phase-1-greeting-exchange)
4. [Phase 2: Module Selection & Authentication](#4-phase-2-module-selection--authentication)
5. [Phase 3: Argument Transmission](#5-phase-3-argument-transmission)
6. [Phase 4: Data Transfer Protocol](#6-phase-4-data-transfer-protocol)
7. [Multiplexed I/O Layer](#7-multiplexed-io-layer)
8. [File List Wire Format](#8-file-list-wire-format)
9. [Checksum & Delta Transfer](#9-checksum--delta-transfer)
10. [Protocol Version History](#10-protocol-version-history)

---

## 1. Connection Establishment

The client opens a TCP connection to the server on port **873** (configurable via `rsync_port` in rsyncd.conf). No TLS is built into the protocol itself; encryption must be provided externally (e.g., stunnel, SSH tunneling).

```
Client                          Server
  |                               |
  |-- TCP SYN ------------------>|
  |<-- SYN-ACK -------------------|
  |-- ACK ----------------------->|
  |                               |
  |== Phase 1: Greeting Exchange ==|
  |== Phase 2: Module Selection    |
  |== Phase 3: Arguments           |
  |== Phase 4: Data Transfer       |
```

---

## 2. Proxy Protocol Header (Optional)

If the daemon is configured with `proxy protocol = yes`, it expects a [HAProxy PROXY protocol](https://www.haproxy.org/download/1.8/doc/proxy-protocol.txt) header as the very first bytes on the connection, before any rsync greeting. Both V1 (text) and V2 (binary) headers are supported.

### Proxy Protocol V2 Binary Header Format

```
+---------------------+--------+---------+----------+------+-------+
| 5-byte magic sig    | ver/cmd| family  | len      | addr | ...   |
| "PROXY\x0A\x00\x01"          |         | (big-end) | data |       |
+---------------------+--------+---------+----------+------+-------+

Magic signature: 0x50 0x52 0x4F 0x58 0x59 0x0A 0x00 0x01 0x04 0x00 0x00
```

The daemon reads the proxy header to determine the client's real IP address for ACL checks and logging. If no valid proxy header is detected, the connection proceeds normally (the first bytes are treated as part of the greeting).

---

## 3. Phase 1: Greeting Exchange

### Server → Client: Daemon Greeting

Immediately after accepting a TCP connection, the server sends a newline-terminated greeting line:

```
@RSYNCD: <version>.<subprotocol> <digest1> [<digest2> ...]\n
```

| Field | Description |
|-------|-------------|
| `version` | Numeric protocol version (e.g., 30, 31, 32). Current maximum is **PROTOCOL_VERSION = 32**. Valid range: MIN_PROTOCOL_VERSION (**20**) to MAX_PROTOCOL_VERSION (**40**). |
| `subprotocol` | Sub-protocol number. Always **0** for final/official releases. Non-zero values indicate pre-release/in-development protocol variants that must match exactly between client and server. |
| `digestN` | Space-separated list of supported authentication digest algorithms, in order of preference (e.g., `md5 md4`). Required for protocols ≥ 32; optional but recommended for 31. For older protocols without this list: assume `"md5"` if protocol ≥ 30, otherwise `"md4"`. |

**Examples:**
```
@RSYNCD: 32.0 md5 md4\n
@RSYNCD: 31.0 sha256 sha1 md5\n
@RSYNCD: 30.0 md5\n
```

### Client → Server: Greeting Response

The client responds with its own greeting line in the same format:

```
@RSYNCD: <version>.<subprotocol> <digest1> [<digest2> ...]\n
```

Both sides parse each other's version and negotiate down to the lower of the two. If sub-protocol versions differ, both fall back one major protocol level (e.g., from 32 → 31).

### Server → Client: Message of the Day (Optional)

After receiving the client greeting, if a `motd file` is configured in rsyncd.conf, the server sends its contents as raw text lines followed by an empty line. This is free-format and not parsed by the protocol — it's purely informational for human readers.

### Protocol Negotiation Rules

```
if (client_version > server_version):
    negotiated = server_version
elif (subprotocol_mismatch):
    negotiated = min(client, server) - 1
else:
    negotiated = min(client, server)
```

For protocol ≥ 30, the `rl_nulls` flag is set to **true**, meaning subsequent argument lines are null-terminated instead of newline-terminated.

---

## 4. Phase 2: Module Selection & Authentication

### Client → Server: Early Input (Optional)

If the client has an early input file (`--early-input-file`), it sends a special command before module selection:

```
#early_input=<length>\n<binary_data>
```

Where `<length>` is the byte count and `<binary_data>` follows immediately. The server reads exactly that many bytes, then expects the next line to be either `#list`, a module name, or another early input command.

### Client → Server: Module Request

The client sends one of:

| Line | Meaning |
|------|---------|
| `#list\n` | Request listing of available modules |
| `<module_name>\n` | Bind to the named module and proceed with transfer setup |

#### Module Listing Response (Server → Client)

For a `#list` request, the server sends tab-separated lines:

```
<name>           <comment>\n
...
@RSYNCD: EXIT\n
```

Each line contains the module name left-padded to 15 characters, a tab, and the comment string. The listing is terminated by `@RSYNCD: EXIT` (protocol ≥ 25) or EOF for older protocols. After sending this, the server closes the connection.

#### Module Binding Response

For a valid module name, the server checks access controls (`hosts allow/deny`, max connections). Then one of three responses follows:

### Server → Client: Authentication Challenge (If Required)

```
@RSYNCD: AUTHREQD <challenge>\n
```

Where `<challenge>` is a base64-encoded random string. The challenge is generated from the client's IP address, current timestamp, and server PID using the negotiated digest algorithm.

### Client → Server: Authentication Response

```
<username> <response_hash>\n
```

| Field | Description |
|-------|-------------|
| `username` | The claimed username (matched against module's `auth users`) |
| `response_hash` | Base64-encoded digest of `<password><challenge>` concatenated together. Uses the most preferred digest algorithm supported by both sides. |

**Hash computation:**
```
hash = base64(digest(password + challenge))
```

The server looks up the password from its secrets file (`secrets file`) and computes the same hash to verify. The `auth users` list supports wildcards (fnmatch) and group prefixes (`@groupname`). Individual user entries can have suffixes: `:deny`, `:ro`, or `:rw`.

### Server → Client: Authentication Result

| Response | Meaning |
|----------|---------|
| `@RSYNCD: OK\n` | Authentication successful (or anonymous access granted). Proceed to argument transmission. |
| `@ERROR: auth failed on module <name>\n` | Authentication failed. Connection closed. |
| `@ERROR: Unknown module '<name>'\n` | Module not found or listing disabled (`list = false`). Connection closed. |
| `@ERROR: access denied to <module> from <host> (<addr>)\n` | ACL check failed. Connection closed. |
| `@ERROR: max connections (N) reached -- try again later\n` | Module connection limit exceeded. Connection closed. |

### Error Response Format

All error responses follow the pattern:

```
@ERROR: <message>\n
```

These are always fatal — upon sending an @ERROR, the server closes the socket immediately.

---

## 5. Phase 3: Argument Transmission

After receiving `@RSYNCD: OK`, the client sends rsync command-line arguments to the daemon. These are parsed by the server as if it had been invoked with them directly on the command line.

### Protocol ≥ 30 (Null-Terminated)

```
<arg1>\x00<arg2>\x00...<argN>\x00\x00
```

Each argument is null-terminated, and a final extra `\x00` signals end of arguments. The first argument after `@RSYNCD: OK` is always `"."`. Arguments are sent as raw bytes — no escaping or encoding beyond the null terminators.

### Protocol < 30 (Newline-Terminated)

```
<arg1>\n<arg2>\n...<argN>\n\n
```

Each argument on its own line, terminated by a double newline (`\n\n`). Arguments containing special characters are backslash-escaped using `safe_arg()` encoding. The server applies `unbackslash_arg()` to reverse this.

### Protected Arguments (Optional)

If the client sends with `protect_args` enabled (protocol ≥ 30), after the initial argument block, it may send a second set of arguments that are not backslash-escaped. This is used when the remote shell would normally unescape args but the daemon protocol doesn't go through a shell.

### Argument Processing on Server Side

The server:
1. Parses all standard rsync options (`--archive`, `-z`, `--delete`, etc.)
2. Applies module-level configuration overrides (e.g., `read only` from config takes precedence over client flags)
3. Sets up include/exclude filters, combining daemon-side and client-side rules

---

## 6. Phase 4: Data Transfer Protocol

After argument parsing succeeds, both sides switch to the **multiplexed I/O layer**. This is a binary protocol that multiplexes raw data (file contents, file lists) with control messages over a single socket.

### Transition from Text to Binary Mode

The transition happens at different points depending on direction and protocol version:

| Protocol | Sender Side | Receiver Side |
|----------|-------------|---------------|
| < 23 | `io_start_multiplex_out()` after args | Plain socket until data transfer begins |
| ≥ 23, < 30 | Buffered I/O with multiplexing for messages only | Multiplexed input enabled early |
| ≥ 30 | Full multiplexed I/O from start of data phase | Full multiplexed I/O from start of data phase |

### Protocol Version Exchange (Binary)

Before file list transfer begins, the server and client exchange protocol versions as binary integers:

```
Server → Client: write_int(protocol_version)    // 4 bytes, big-endian
Client → Server: read_int()                     // stores in remote_protocol
```

If `remote_protocol` is already set (daemon connection), this step is skipped. The negotiated version is the minimum of local and remote versions.

---

## 7. Multiplexed I/O Layer

The multiplexing layer allows two types of data to share a single socket: **raw data** (`MSG_DATA`) for file lists, checksums, and deltas; and **control messages** (various `MSG_*` codes) for errors, status updates, keep-alives, etc.

### Message Frame Format

Every multiplexed frame is 4 bytes of header followed by payload:

```
+--------+--------+--------+--------+----------+
| byte0  | byte1  | byte2  | byte3  | payload  |
| (big-endian uint32)       |          |
+--------+--------+--------+--------+----------+

header = ((MPLEX_BASE + msgcode) << 24) | payload_length

Where: MPLEX_BASE = 7
```

- **Bits 31–24**: `msgcode` (message type, offset by MPLEX_BASE=7)
- **Bits 23–0**: Payload length in bytes

### Message Codes (`enum msgcode`)

| Code | Name | Description | Size | Direction |
|------|------|-------------|------|-----------|
| 0 | `MSG_DATA` | Raw data (file lists, checksums, deltas) | Variable | Both |
| 1 | `MSG_ERROR_XFER` | Transfer error message | Text | Server→Client |
| 2 | `MSG_INFO` | Informational log message | Text | Server→Client |
| 3 | `MSG_ERROR` | Error message (protocol ≥ 30) | Text | Both |
| 4 | `MSG_WARNING` | Warning message (protocol ≥ 30) | Text | Both |
| 5 | `MSG_ERROR_SOCKET` | Socket error (sibling logging only) | Text | Receiver→Generator |
| 6 | `MSG_LOG` | Log-only message (not sent to client) | Text | Receiver→Generator |
| 7 | `MSG_CLIENT` | Client-side log message | Text | — |
| 8 | `MSG_ERROR_UTF8` | UTF-8 conversion error | Text | Receiver→Generator |
| 9 | `MSG_REDO` | Request to reprocess a file list index | int32 | Sender→Receiver |
| 10 | `MSG_STATS` | Statistics data for generator | int64 (total_read) | Receiver→Sender |
| 22 | `MSG_IO_ERROR` | I/O error flags from sender | int32 bitmask | Both |
| 33 | `MSG_IO_TIMEOUT` | Timeout value notification | int32 seconds | Server→Client |
| 42 | `MSG_NOOP` | Keep-alive (legacy protocol-30 only) | 0 bytes | Sender→Receiver |
| 86 | `MSG_ERROR_EXIT` | Synchronized error exit signal | 0 or int32 | Both |
| 100 | `MSG_SUCCESS` | File transfer completed successfully | int32 ndx (or 4+8+8 with dev/ino) | Sender→Receiver |
| 101 | `MSG_DELETED` | File deleted on receiving side | Text filename | Receiver→Sender |
| 102 | `MSG_NO_SEND` | Sender failed to open requested file | int32 ndx | Both |

### I/O Error Bitmask (`MSG_IO_ERROR`)

```c
IOERR_GENERAL   = (1<<0)  // General I/O error
IOERR_VANISHED  = (1<<1)  // File vanished during transfer
IOERR_DEL_LIMIT = (1<<2)  // Deletion limit reached (--max-delete)
```

### Keep-Alive Mechanism

When `--timeout` is set, idle connections send keep-alive messages:
- **Protocol ≥ 30**: Empty `MSG_DATA` frame (`send_msg(MSG_DATA, "", 0, 0)`)
- **Protocol 29 and earlier**: Raw-data-based keep-alives or `MSG_NOOP`

The timeout check fires when no I/O has occurred for `io_timeout` seconds. The allowed lull before sending a keep-alive is `(io_timeout + 1) / 2`. Default select timeout is 60 seconds if no explicit timeout is set.

### Flushing Priority

When writing to the socket, data is flushed in this priority order:
1. Complete any in-progress `MSG_DATA` sequence from output buffer
2. Flush all pending messages from message buffer (`iobuf.msg`)
3. Write raw data from main output buffer (`iobuf.out`), filling in multiplexed headers

---

## 8. File List Wire Format

File lists are sent as a series of file entries, each encoding metadata about one filesystem object. The format uses delta-encoding to minimize wire size by reusing values from the previous entry when possible.

### File Entry Header: Xmit Flags Byte(s)

Each file entry starts with an **xflags** field that indicates which fields are present and how they're encoded:

| Flag | Value | Meaning |
|------|-------|---------|
| `XMIT_SAME_MODE` | 0x01 | Mode is same as previous entry (skip mode field) |
| `XMIT_SAME_UID` | 0x02 | UID is same as previous entry (skip uid fields) |
| `XMIT_SAME_GID` | 0x04 | GID is same as previous entry (skip gid fields) |
| `XMIT_SAME_TIME` | 0x08 | Modification time is same as previous (skip mtime field) |
| `XMIT_SAME_NAME` | 0x10 | Filename shares prefix with previous name |
| `XMIT_LONG_NAME` | 0x20 | Filename suffix length > 255 bytes |
| `XMIT_TOP_DIR` | 0x40 | This is a top-level directory (for recursive transfers) |
| `XMIT_EXTENDED_FLAGS` | 0x80 | Extended flags follow (16-bit xflags instead of 8-bit) |

**Protocol ≥ 32**: Xmit flags are sent as varint. If zero, send `XMIT_EXTENDED_FLAGS` sentinel instead.  
**Protocol 28–31**: Single byte if value fits in 0x00–0xFF; otherwise `0x80` + second byte for extended flags.  
**Protocol < 28**: Always single byte with fallback bits to avoid zero values.

### Extended Flags (protocol ≥ 28, when XMIT_EXTENDED_FLAGS is set)

| Flag | Value | Meaning |
|------|-------|---------|
| `XMIT_SAME_RDEV_pre28` / `XMIT_SAME_DEV_pre30` | Various | Device number same as previous |
| `XMIT_HLINKED` | 0x100 | File is part of a hard-link group |
| `XMIT_HLINK_FIRST` | 0x200 (protocol ≥ 30) | First entry in hard-link group; reference index follows |
| `XMIT_SAME_RDEV_MAJOR` | 0x400 | Device major number same as previous |
| `XMIT_MOD_NSEC` | 0x800 (protocol ≥ 31) | Nanosecond mtime component follows |
| `XMIT_CRTIME_EQ_MTIME` | 0x2000 (protocol ≥ 31, crtimes enabled) | Creation time equals modification time |
| `XMIT_SAME_ATIME` | 0x4000 | Access time same as previous entry |
| `XMIT_USER_NAME_FOLLOWS` | 0x8000 (inc_recurse mode) | Username string follows uid field |
| `XMIT_GROUP_NAME_FOLLOWS` | 0x10000 (inc_recurse mode) | Group name string follows gid field |
| `XMIT_NO_CONTENT_DIR` | 0x40 (protocol ≥ 30, dirs only) | Directory has no content to transfer |

### File Entry Wire Layout

After the xflags byte(s), fields are sent in this order:

```
1. [if XMIT_SAME_NAME] prefix_length : uint8_t   // bytes shared with previous filename
2. name_suffix_length : uint8_t (or varint if XMIT_LONG_NAME)
3. name_suffix : raw[name_suffix_length]          // only the differing suffix of the path
4. [protocol ≥ 30, hard-link first hlink_ndx] reference_index : varint   // index into file list
5. file_size : varlong(3)                         // minimum 3 bytes for int64 size
6. [if !XMIT_SAME_TIME] mtime : varlong(4) (proto ≥ 30) or int32 (proto < 30)
7. [if XMIT_MOD_NSEC] mod_nsec : varint           // nanosecond component of mtime
8. [if crtimes enabled and !XMIT_CRTIME_EQ_MTIME] crtime : varlong(4)
9. [if !XMIT_SAME_MODE] mode : int32              // wire-mode encoding (see below)
10. [if atimes enabled, not dir, !XMIT_SAME_ATIME] atime : varlong(4)
11. [if preserve_uid and !XMIT_SAME_UID] uid : varint (proto ≥ 30) or int32 (proto < 30)
    [if XMIT_USER_NAME_FOLLOWS] username_len : uint8_t, username : raw[username_len]
12. [if preserve_gid and !XMIT_SAME_GID] gid : varint (proto ≥ 30) or int32 (proto < 30)
    [if XMIT_GROUP_NAME_FOLLOWS] groupname_len : uint8_t, groupname : raw[groupname_len]
13. [if device/special and !XMIT_SAME_RDEV_MAJOR] rdev_major : varint30
    [device minor number]: varint (proto ≥ 30) or byte/int depending on flags
14. [if symlink] symlink_target_length : varint30, symlink_data : raw[symlink_len]
15. [protocol < 30, hard-link] dev+1 : longint, ino : longint   // 64-bit (proto ≥ 26) or int32×2 (proto < 26)
16. [if always_checksum and regular file] checksum[flist_csum_len]    // full-file hash for skip-check
```

### Wire Mode Encoding (`to_wire_mode()`)

The mode field is encoded as a packed integer:
- Bits 0–8: Standard Unix permission bits (mode & 07777)
- Bit 9+: File type indicator from the high byte of `st_mode`

### End-of-List Marker

A file list entry with **xflags = 0** signals end of that directory's entries. The sender then sends a special index marker:

```
ndx : int32 (or compressed ndx for protocol ≥ 30)
```

| Value | Meaning |
|-------|---------|
| `NDX_DONE` (-1) | All file lists complete; transfer phase begins |
| `NDX_FLIST_EOF` (-2) | End of current directory's sub-lists (protocol ≥ 30, inc_recurse mode) |
| Positive value | Index into the parent file list for next subdirectory to process |

### Compressed NDX Encoding (Protocol ≥ 30)

File list indices use a byte-reduction scheme:

```
First byte = 0x00 → NDX_DONE (-1), no side effects
First byte = 0xFF → negative number follows; read next byte as sign indicator
First byte = 0xFE → extended encoding follows (2 or 4 more bytes)
First byte = 0x01–0xFD → delta from previous positive index: ndx = prev + value

Extended format after 0xFE:
  If high bit of second byte is set: full 32-bit unsigned number follows (big-endian, high bit cleared)
  Otherwise: 16-bit big-endian delta added to previous index
```

---

## 9. Checksum & Delta Transfer

### File-Level Checksum Header (`write_sum_head` / `read_sum_head`)

Before transferring a file's data or checksums, the sender transmits a **checksum header**:

```
count     : int32    // number of blocks (0 if no transfer needed)
blength   : int32    // block size in bytes (max MAX_BLOCK_SIZE = 131072 for proto ≥ 30; OLD_MAX_BLOCK_SIZE = 536870912 for older)
s2length  : int32    // length of second checksum hash (only if protocol ≥ 27; defaults to csum_length otherwise)
remainder : int32    // size of final partial block (0 ≤ remainder ≤ blength)
```

If `count = -1`, it signals an error condition. If `count = 0` and the file is up-to-date, no further data follows for that file.

### Block Checksums

After the header, if count > 0:

```
for each block i in [0..count-1]:
    sum1[i] : raw[csum_length]     // rolling checksum ( Adler-like)
    sum2[i] : raw[s2length]        // strong hash (MD4/MD5/SHA, depending on build config)
```

### Delta Algorithm Communication

The receiver reads the file locally and computes matching block checksums. It then sends a **delta map** to the sender indicating which blocks match and where gaps exist:

- Matching blocks are skipped by both sides using seek operations
- Gaps (mismatched regions) trigger data transmission from sender to receiver
- The delta uses rsync's rolling checksum algorithm for efficient mismatch detection

### File Transfer Completion Signals

After each file transfer completes, the sender sends a completion message via multiplex:

```
MSG_SUCCESS : int32 ndx              // protocol < 31 or non-local-server
MSG_SUCCESS : [ndx(4) + dev(8) + ino(8)]   // local server mode (for --remove-source-files verification)
```

If the file couldn't be opened: `MSG_NO_SEND : int32 ndx`  
If reprocessing is needed: `MSG_REDO : int32 ndx`

---

## 10. Protocol Version History

| Version | Release Date | Key Changes |
|---------|-------------|-------------|
| **20** | — | Minimum supported version (MIN_PROTOCOL_VERSION) |
| **25** | 2001-08-20 (2.4.7pre2) | `@RSYNCD: EXIT` terminator for module listing; explicit end-of-listing signal replaces EOF detection |
| **26** | — | 64-bit dev_t and ino_t support (`longint` encoding); hard-link tracking improvements |
| **27** | — | Explicit `s2length` field in checksum header (was hardcoded to csum_length) |
| **28** | — | Extended xflags (16-bit); device major/minor split; improved rdev handling; special file support |
| **30** | 2007-10-04 (3.0.0pre1) | Subprotocol version field in greeting (`@RSYNCD: V.S`); varint/varlong encoding for integers; null-terminated arguments (`rl_nulls=1`); compressed NDX encoding; improved hard-link tracking with `XMIT_HLINK_FIRST`; 64-bit mtime via varlong(4) |
| **31** | 2013-09-28 (3.1.0) | Nanosecond timestamp support (`XMIT_MOD_NSEC`, crtimes); digest name list in greeting; `MSG_ERROR_EXIT` with exit code; improved timeout handling via `MSG_IO_TIMEOUT`; files-from forwarding without multiplexing for older protocols |
| **32** | Current (development) | Xmit flags as varint; additional security hardening; proxy protocol V1/V2 support |

### Subprotocol Version Rules

- **0**: Final/official release. Any client with matching major version can interoperate.
- **Non-zero**: Pre-release/in-development. Both sides must have identical subprotocol values for the highest common major version, otherwise fall back one level.

---

## Appendix A: Integer Encoding Formats

### Fixed-Width Integers (All Protocols)

| Function | Size | Endianness | Signed? |
|----------|------|-----------|---------|
| `write_int` / `read_int` | 4 bytes | Big-endian | Yes (int32, sign-extended if platform int > 32 bits) |
| `write_shortint` / `read_shortint` | 2 bytes | Little-endian | No (uint16) |

### Variable-Length Integers (`varint`, protocol ≥ 30)

A compact encoding for signed integers. The first byte indicates how many additional bytes follow:

```
First byte high nibble determines extra bytes:
  0x00–0xBF → 0 extra bytes (value in low bits of first byte, sign-extended)
  0xC0–0xDF → 1 extra byte
  0xE0–0xEF → 2 extra bytes  
  0xF0–0xF7 → 3 extra bytes
  0xF8–0xFB → 4 extra bytes (full int32 range)

The low bits of the first byte are masked out based on how many extra bytes follow.
```

### Variable-Length Long Integers (`varlong`, protocol ≥ 30)

Similar to varint but for int64 values with a configurable minimum byte count:

```
write_varlong(f, value, min_bytes=3 or 4)
```

The encoding ensures at least `min_bytes` are always written. Common uses: file sizes (min=3), timestamps (min=4).

### Legacy Long Integers (`longint`, protocol < 30)

For values that fit in signed int32 range [−2³¹, 2³¹−1]:
```
write_int(value)    // 4 bytes only
```

For larger values:
```
write_buf(0xFFFFFFFFFFFFFFFF)   // 8 bytes of 0xFF as sentinel
write_int64(value)              // 8 bytes big-endian int64
Total: 12 bytes
```

### Variable-Length Strings (`vstring`)

Used for filenames and other text data:

```
if (len <= 0x7F):
    write_byte(len & 0x7F)       // single byte, high bit clear
else:
    write_byte((len >> 8) | 0x80)   // first byte with high bit set
    write_byte(len & 0xFF)          // second byte (max len = 0x7FFF)

write_buf(data, len)               // raw string bytes follow length prefix
```

---

## Appendix B: Socket Options and Behavior

### Default Socket Configuration

- **Port**: 873 (`RSYNC_PORT`)
- **Keepalive**: `SO_KEEPALIVE` set when running as standalone daemon (`am_daemon > 0`)
- **Non-blocking**: Set on input fd for standalone daemon mode (enables select-based I/O)
- **Buffer sizes**: IO_BUFFER_SIZE = 32 KiB; output buffer is double that

### Timeout Behavior

- Default `select()` timeout: 60 seconds (`SELECT_TIMEOUT`)
- With `--timeout=N`: fires after N seconds of no bidirectional I/O
- Keep-alive sent at `(N+1)/2` second intervals when idle
- Module-level `timeout = N` in rsyncd.conf overrides client setting if lower

### Connection Lifecycle States

```
CONNECTED → GREETING_EXCHANGED → MODULE_SELECTED → AUTHENTICATED 
    → ARGUMENTS_SENT → DATA_TRANSFER → COMPLETE/ERROR
```

At each state transition, errors result in an `@ERROR:` line (text phase) or socket closure. During data transfer, errors are communicated via multiplexed messages and the connection terminates with `MSG_ERROR_EXIT`.

---

## Appendix C: Security Considerations

1. **No built-in encryption**: The protocol transmits passwords as base64-encoded hashes over plaintext TCP. Use stunnel/SSH for encrypted transport.
2. **Challenge-response auth**: Prevents replay attacks (challenge includes timestamp and PID), but does not provide forward secrecy.
3. **Module isolation**: Each module can have its own `path`, `uid`/`gid`, ACL rules, and chroot jail. The daemon drops privileges before processing transfers.
4. **Symlink security**: Non-chrooted daemons enable `use_secure_symlinks` to prevent TOCTOU symlink race attacks via `secure_relative_open()` wrappers.
5. **Path sanitization**: Module paths are normalized with `normalize_path()` to prevent directory traversal outside the module root.
