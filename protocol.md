# Rsync Daemon Protocol Reference

Focused reference for implementing go-rsyncfs. All details verified against upstream rsync source tree (sibling directory).

## Byte Order

**All multi-byte integers on the wire are little-endian.** This includes mux frame headers, fixed-width ints (write_int/read_int), varint, and varlong. Confirmed via `SIVAL()`/`SIVAL64()` macros in upstream `byteorder.h`.

---

## Phase 1: Greeting Exchange (text)

### Server → Client
```
@RSYNCD: <version>.<subprotocol> <digests...>\n
```
- `<version>` is the server's protocol version number (integer, e.g. `32`)
- `<subprotocol>` is always `0` for official releases; non-zero means pre-release and must match exactly or both sides fall back one major version
- `<digests...>`: space-separated list of supported auth digest algorithms in preference order (e.g. `md5 md4`). Required for protocol ≥ 32, optional but recommended for 31. For older protocols without this field: assume `"md5"` if proto ≥ 30, otherwise `"md4"`.

**Examples:**
```
@RSYNCD: 32.0 md5 md4\n
@RSYNCD: 30.0 md5\n
```

### Client → Server (response)
Same format as server greeting. Both sides parse each other's version and negotiate down to the lower of the two. If subprotocol versions differ, both fall back one major protocol level.

**Negotiation rules:**
```
if client_version > server_version:
    negotiated = server_version
elif subprotocol_mismatch:
    negotiated = min(client, server) - 1
else:
    negotiated = min(client, server)
```

### Message of the Day (optional, after greeting exchange)
If configured, server sends raw text lines followed by an empty line. Not parsed — purely informational.

---

## Phase 2: Module Selection & Authentication (text)

### Client → Server: Module Request
- `#list\n` — request module listing
- `<module_name>\n` — bind to named module

### Module Listing Response (`#list`)
Server sends tab-separated lines, then terminator:
```
<name>           <comment>\n    // name left-padded to 15 chars + tab + comment
...
@RSYNCD: EXIT\n          // protocol ≥ 25; EOF for older protocols
```

### Authentication Challenge (if module requires auth)
Server → Client: `@RSYNCD: AUTHREQD <challenge>\n` where `<challenge>` is base64-encoded random data.

Client → Server: `<username> <response_hash>\n`
- Hash = base64(digest(password + challenge)) using the most preferred common digest algorithm

Server response:
- `@RSYNCD: OK\n` — success, proceed to Phase 3
- `@ERROR: <message>\n` — fatal error, server closes connection immediately

### Error Format (all phases)
```
@ERROR: <message>\n
```
Always fatal. Server closes socket after sending.

---

## Phase 3: Argument Transmission (text → binary transition point)

After `@RSYNCD: OK`, client sends rsync command-line arguments to the server. These control transfer behavior (flags like `-a`, `--delete`, etc.).

### Protocol ≥ 30 (null-terminated)
```
<arg1>\x00<arg2>\x00...<argN>\x00\x00
```
Each argument null-terminated, final extra `\x00` signals end. First arg is always `"."`.

### Protocol < 30 (newline-terminated)
```
<arg1>\n<arg2>\n...<argN>\n\n
```
Double newline terminates. Special characters backslash-escaped via `safe_arg()` encoding.

---

## Phase 4: Data Transfer (binary multiplexed I/O)

### Protocol Version Exchange (binary, before file lists)
Server → Client: `write_int(protocol_version)` — 4 bytes little-endian int32
Client reads this as `remote_protocol`. Negotiated version = min(local, remote).

**Note:** If `remote_protocol` is already set (daemon connection), this step may be skipped. For our implementation we always send/receive it to keep things simple.

---

## Multiplexed I/O Layer

Every frame: 4-byte header + payload. Header is a **little-endian uint32**:
```
header = ((MPLEX_BASE + msgCode) << 24) | length
where MPLEX_BASE = 7, length in bits [0..23] (max ~16MB per frame)
```

### Message Codes (`enum msgcode`)

| Code | Name | Description | Payload Size | Direction |
|------|------|-------------|--------------|-----------|
| 0 | `MSG_DATA` | Raw data (file lists, checksums, deltas) | Variable | Both |
| 1 | `MSG_ERROR_XFER` | Transfer error message | Text | Server→Client |
| 2 | `MSG_INFO` | Informational log message | Text | Server→Client |
| 3 | `MSG_ERROR` | Error (proto ≥ 30) | Text | Both |
| 4 | `MSG_WARNING` | Warning (proto ≥ 30) | Text | Both |
| 5 | `MSG_ERROR_SOCKET` | Socket error (sibling logging only) | Text | Receiver→Generator |
| 6 | `MSG_LOG` | Log-only message | Text | Receiver→Generator |
| 7 | `MSG_CLIENT` | Client-side log message | Text | — |
| 8 | `MSG_ERROR_UTF8` | UTF-8 conversion error | Text | Receiver→Generator |
| 9 | `MSG_REDO` | Reprocess file list index | int32 LE | Sender→Receiver |
| 10 | `MSG_STATS` | Statistics for generator | int64 (total_read) | Receiver→Sender |
| 22 | `MSG_IO_ERROR` | I/O error flags from sender | int32 bitmask | Both |
| 33 | `MSG_IO_TIMEOUT` | Timeout value notification | int32 seconds | Server→Client |
| 42 | `MSG_NOOP` | Keep-alive (legacy proto-30 only) | 0 bytes | Sender→Receiver |
| 86 | `MSG_ERROR_EXIT` | Synchronized error exit signal | 0 or int32 | Both |
| 100 | `MSG_SUCCESS` | File transfer completed successfully | See below | Sender→Receiver |
| 101 | `MSG_DELETED` | File deleted on receiving side | Text filename | Receiver→Sender |
| 102 | `MSG_NO_SEND` | Failed to open requested file | int32 ndx | Both |

**MSG_SUCCESS payload:**
- Protocol < 31 or non-local-server: just `int32 ndx` (file index)
- Local server mode: `[ndx(4) + dev(8) + ino(8)]` for `--remove-source-files` verification

---

## File List Wire Format

Sent as a series of file entries with delta-encoding to minimize wire size. Each entry starts with **xflags** (extended transmit flags).

### Xmit Flags Encoding by Protocol Version

**Protocol ≥ 32 (when `CF_VARINT_FLIST_FLAGS` is negotiated):**
Varint encoding. If xflags value is zero, send varint(`XMIT_EXTENDED_FLAGS`) as a sentinel instead of actual zero (zero signals end-of-list).

**Protocol 28–31:**
If extended flags needed (`xflags & 0xFF00 != 0`) or `xflags == 0` and not a directory: set XMIT_EXTENDED_FLAGS bit, then write as `write_shortint(xflags)` (2 bytes LE). Otherwise just `write_byte(xflags)`.

**Protocol < 28:**
If no flags set (`!(xflags & 0xFF)`): add harmless flag (`XMIT_LONG_NAME` for non-dirs or `XMIT_TOP_DIR` for dirs) to avoid sending zero. Then `write_byte(xflags)`.

### Xmit Flag Bits (basic, always present)

```go
XMIT_TOP_DIR          = 1 << 0   // top-level directory (for recursive transfers)
XMIT_SAME_MODE        = 1 << 1   // mode same as previous entry → skip mode field
XMIT_EXTENDED_FLAGS   = 1 << 2   // extended flags follow (proto ≥ 28)
XMIT_SAME_UID         = 1 << 3   // uid same as previous → skip uid fields
XMIT_SAME_GID         = 1 << 4   // gid same as previous → skip gid fields
XMIT_SAME_NAME        = 1 << 5   // filename shares prefix with previous name
XMIT_LONG_NAME        = 1 << 6   // filename suffix length > 255 bytes
XMIT_SAME_TIME        = 1 << 7   // mtime same as previous → skip mtime field
```

### Extended Flags (when XMIT_EXTENDED_FLAGS is set, proto ≥ 28)

| Flag | Value | Protocol Requirement | Meaning |
| `XMIT_SAME_RDEV_MAJOR` / `XMIT_NO_CONTENT_DIR` | `1 << 8` | proto ≥ 28 / ≥ 30 | Device major same (devices) / not a content dir (dirs) |
| `XMIT_HLINKED` | `1 << 9` | proto ≥ 28 | File is part of a hard-link group |
| `XMIT_SAME_DEV_pre30` / `XMIT_USER_NAME_FOLLOWS` | `1 << 10` | proto 28-29 / ≥ 30 | Same device (pre-30) / username follows (≥ 30) |
| `XMIT_RDEV_MINOR_8_pre30` / `XMIT_GROUP_NAME_FOLLOWS` | `1 << 11` | proto 28-29 / ≥ 30 | 8-bit minor (pre-30) / groupname follows (≥ 30) |
| `XMIT_HLINK_FIRST` / `XMIT_IO_ERROR_ENDLIST` | `1 << 12` | proto ≥ 30 / ≥ 31 | First in hard-link group / I/O error endlist |
| `XMIT_MOD_NSEC` | `1 << 13` | proto ≥ 31 | Nanosecond mtime component follows |
| `XMIT_SAME_ATIME` | `1 << 14` | any proto | Access time same as previous entry |
| `XMIT_CRTIME_EQ_MTIME` | `1 << 17` | varint xflags | Creation time equals modification time |

### File Entry Wire Layout (field order)

```
1. [if XMIT_SAME_NAME] prefix_length : uint8     // bytes shared with previous filename
2. name_suffix_length : uint8 (or varint if XMIT_LONG_NAME, proto ≥ 30 uses vstring encoding)
3. name_suffix : raw[name_suffix_length]          // only the differing suffix of the path
4. [if XMIT_HLINK_FIRST and proto ≥ 30] hlink_ndx : varint   // index into file list for hard-link target
5. file_size : varlong(3)                         // minimum 3 bytes (proto ≥ 30); longint for older protocols
6. [if !XMIT_SAME_TIME] mtime : varlong(4)        // proto ≥ 30; int32 LE for older protocols
7. [if XMIT_MOD_NSEC and proto ≥ 31] mod_nsec : varint       // nanosecond component of mtime
8. [if crtimes enabled, !XMIT_CRTIME_EQ_MTIME] crtime : varlong(4)
9. [if !XMIT_SAME_MODE] mode : int32 LE           // wire-mode encoding (see below)
10. [if atimes enabled, not dir, !XMIT_SAME_ATIME] atime : varlong(4)
11. [if preserve_uid and !XMIT_SAME_UID] uid : varint (proto ≥ 30) or int32 LE (older)
    [if XMIT_USER_NAME_FOLLOWS] username_len : uint8, username : raw[username_len]
12. [if preserve_gid and !XMIT_SAME_GID] gid : varint (proto ≥ 30) or int32 LE (older)
    [if XMIT_GROUP_NAME_FOLLOWS] groupname_len : uint8, groupname : raw[groupname_len]
13. [if device/special and !XMIT_SAME_RDEV_MAJOR] rdev_major : varint30 / byte depending on proto
    [device minor number]: varies by protocol version
14. [if symlink] symlink_target_length : varint30, symlink_data : raw[symlink_len]
15. [proto < 30, hard-link] dev+1 : longint, ino : longint   // 64-bit (proto ≥ 26) or int32×2 (older)
16. [if always_checksum and regular file] checksum[flist_csum_len]    // full-file hash for skip-check
```

### Wire Mode Encoding (`to_wire_mode()`)
Mode is packed as: `(st_mode & 07777)` in low bits, with file type indicator from high byte of `st_mode`. The exact encoding preserves the standard Unix mode value.

**File type constants (from `/usr/include/bits/stat.h`):**
```go
S_IFMT   = 0o0170000 // mask for file type bits
S_IFDIR  = 0o0040000 // directory
S_IFCHR  = 0o0020000 // character device
S_IFBLK  = 0o0060000 // block device
S_IFREG  = 0o0100000 // regular file
S_IFIFO  = 0o0010000 // FIFO/pipe
S_IFLNK  = 0o0120000 // symbolic link
S_IFSOCK = 0o0140000 // socket
```

### End-of-List Marker
A file list entry with **xflags = 0** signals end of that directory's entries. Then a special index marker follows:

| Value | Meaning | Encoding (proto ≥ 30) |
|-------|---------|----------------------|
| `NDX_DONE` (-1) | All file lists complete; transfer phase begins | Compressed NDX |
| `NDX_FLIST_EOF` (-2) | End of current directory's sub-lists (inc_recurse mode, proto ≥ 30) | Compressed NDX |
| Positive value | Index into parent file list for next subdirectory to process | Compressed NDX |

### Compressed NDX Encoding (Protocol ≥ 30)
```
First byte = 0x00 → NDX_DONE (-1), no side effects
First byte = 0xFF → negative number follows; read next byte as sign indicator
First byte = 0xFE → extended encoding: if high bit of second byte set, full uint32 LE follows (high bit cleared); otherwise 16-bit big-endian delta added to previous index
First byte = 0x01–0xFD → delta from previous positive index: ndx = prev + value
```

---

## Checksum & Delta Transfer Protocol

### File-Level Checksum Header (`write_sum_head` / `read_sum_head`)
Sent before transferring a file's data or checksums. All fields are **int32 LE**:

```
count     : int32    // number of blocks (0 = no transfer needed, -1 = error)
blength   : int32    // block size in bytes (max 1<<17 for proto ≥ 30; max 1<<29 for older)
s2length  : int32    // length of second checksum hash (only if protocol ≥ 27; defaults to csum_length otherwise)
remainder : int32    // size of final partial block (0 ≤ remainder ≤ blength)
```

### Checksum Algorithms

**Checksum1 (rolling):** Always the Adler-like rolling checksum. Returns uint32.
The buffer is treated as signed chars (`schar`), but `CHAR_OFFSET = 0` in modern rsync so this has no practical effect — just add byte values directly.

**Checksum2 (strong hash):** Depends on negotiated digest algorithm:
- **MD4**: Default for most implementations. Produces 16-byte digest. Computed as: MD4(data) then append seed bytes if `checksum_seed` is set.
- **MD5**: Also produces 16-byte digest, used when both sides advertise it in greeting.
- Protocol ≥ 32 supports additional algorithms (SHA variants) via the digest list in the greeting exchange. The most preferred common algorithm from each side's advertised list wins.

**Digest negotiation:** During Phase 1 greeting exchange, each side advertises supported digests. Both sides pick the first algorithm that appears in both lists. If no overlap: MD5 for proto ≥ 30, MD4 otherwise.

### Block Checksums (after header, only if count > 0)
For each block `i` in `[0..count-1]`:
```
sum1[i] : raw[csum_length]     // rolling checksum (Adler-like), always 4 bytes
sum2[i] : raw[s2length]        // strong hash digest length (MD4/MD5 = 16, SHA-256 = 32, etc.)
```

### Delta Algorithm Flow
1. **Server sends** SumHead + block checksums for the file it has
2. **Client reads** its local copy of the file and computes matching block checksums
3. **Client sends delta map:** a stream indicating which blocks match (skip via seek) and where gaps exist
4. **For each gap,** server transmits raw data bytes to fill in mismatched regions

### File Transfer Completion Signals
- `MSG_SUCCESS : int32 ndx` — file transfer completed successfully
- `MSG_NO_SEND : int32 ndx` — sender failed to open requested file
- `MSG_REDO : int32 ndx` — request to reprocess a file list index

---

## Integer Encoding Formats (all little-endian)

### Fixed-Width Integers (`write_int` / `read_int`)
4 bytes, **little-endian**, signed int32.

### Variable-Length Integers (`varint`, protocol ≥ 30)
Compact encoding for signed integers:
```
First byte high nibble determines extra bytes following:
  0x00–0xBF → 0 extra bytes (value encoded in low bits of first byte, sign-extended to int32)
  0xC0–0xDF → 1 extra byte follows
  0xE0–0xEF → 2 extra bytes follow
  0xF0–0xF7 → 3 extra bytes follow
  0xF8–0xFB → 4 extra bytes follow (full int32 range)

Extra bytes are stored little-endian after the first byte. The low bits of the first byte are masked out based on how many extra bytes follow.
```

### Variable-Length Long Integers (`varlong`, protocol ≥ 30)
Similar to varint but for int64 with configurable minimum byte count:
```
write_varlong(f, value, min_bytes=3 or 4)
```
Encoding ensures at least `min_bytes` are always written. Common uses: file sizes (min=3), timestamps (min=4).

### Legacy Long Integers (`longint`, protocol < 30)
For values fitting in signed int32 range `[−2³¹, 2³¹−1]`: just `write_int(value)` — 4 bytes LE.
For larger values: sentinel `0xFFFFFFFFFFFFFFFF` (8 bytes of 0xFF) followed by 8-byte big-endian int64. Total: 12 bytes.

### Short Integers (`write_shortint`)
2 bytes, **little-endian**, unsigned uint16. Used for name suffix lengths in some contexts.

---

## What We Need For Each Task

| Task | Protocol Sections Needed |
|------|------------------------|
| 1 — Mux layer | Multiplexed I/O Layer (frame format + message codes) |
| 2 — Wire ints | Integer Encoding Formats (varint, varlong, legacy longint) |
| 3 — Greeting | Phase 1: Greeting Exchange |
| 4 — Server struct | N/A (structural only) |
| 5 — Handshake | Phases 1–3 (greeting + module selection + auth + arguments) |
| 6 — File list | File List Wire Format (xflags, entry layout, end-of-list markers) |
| 7 — Data transfer | Checksum & Delta Transfer Protocol |
| 8+9 — Client FS | All of the above (reverse direction) |
