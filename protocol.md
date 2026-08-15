# Rsync Daemon Protocol Reference

Definitive reference for implementing go-rsyncfs.  Every claim is verified against the upstream rsync source tree in `.upstream/`.

### Reference format

Every `.upstream/` citation includes a verifiable snippet in backticks:

```
(prose, `.upstream/file.c:line`, `snippet-from-that-li`)
```

- `prose` is optional -- (`.upstream/file.c:line`, `snipp`) is valid.
- The snippet is a literal string that **must** appear on the cited line.  Run `./upstreams.gawk protocol.md` to verify.
- For ranges, the line ref uses `-` and the snippet uses `...` to separate start/end excerpts:
  (`.upstream/main.c:1356-1364`, `if (protocol_version >= 30) ... send_filter_list(f_out`)
- File-only refs (no line number) are allowed for high-level pointers.

## Byte order

All multi-byte integers on the wire are little-endian.  This includes mux frame headers, fixed-width ints (`write_int`/`read_int`), varint, and varlong.  Confirmed via `SIVAL()`/`SIVAL64()` macros in `.upstream/byteorder.h`.

## 1. Transport modes

Rsync supports two fundamentally different transport modes, which differ in greeting exchange, version negotiation, module selection, and authentication.

### 1.1 Daemon socket transport (port 873)

The daemon socket transport uses a text-based greeting exchange followed by module selection and optional digest-based authentication.  Informal documentation is in `.upstream/csprotocol.txt`.

#### Greeting exchange

Both sides send a greeting simultaneously (simultaneous write, then read).  Source: `.upstream/compat.c:853`, `io_printf(f_out, "@RSYNCD: %d.%d %s\n", protocol_version, our_sub, tmpbuf);` (client, inside `output_daemon_greeting()`), `.upstream/compat.c:844`, `void output_daemon_greeting(int f_out, int am_client)`, `.upstream/clientserver.c:209`, `if (sscanf(buf, "@RSYNCD: %d.%d", &remote_protocol, &remote_sub) < 1) {` (server).

```
@RSYNCD: <version>.<sub> <digest1> <digest2> ...
```

- `<version>` is the protocol version number (eg, 32).
- `<sub>` is the subprotocol version (0 for final releases, nonzero for pre-releases).  Source: `.upstream/compat.c:896-903`, `int get_subprotocol_version() ... }`.
- The digest list is space-separated.  Modern rsync always includes it in the greeting (`.upstream/compat.c:851-853` calls `get_default_nno_list()`).  If omitted on protocol 32+, it is a fatal error (`.upstream/clientserver.c:234-24, `get_default_nno_list(&valid_auth_checksums, tmpbuf, MAX_NSTR_STRLEN, '\0'); ... io_printf(f_out, "@RSYNCD: %d.%d %s\n", protocol_version, our_sub, tmpbuf);`0`, gate `remote_protocol > 31`).  If omitted on protocol 30-31, the receiver assumes `md5`; on protocol < 30, it assumes `md4` (`daemon_auth_choices` stays unset, `.upstream/clientserver.c:228-233`, `daemon_auth_choices = strchr(buf + 9, ' '); ... *cp = '\0';`; the default algorithm comes from `.upstream/compat.c:555-556`, `else ... len = strlcpy(tmpbuf, protocol_version >= 30 ? "md5" : "md4", MAX_NSTR_STRLEN);`).

Both sides parse the other's greeting and negotiate down to the lower version.  Subprotocol mismatch causes a version downgrade.  Source: `.upstream/clientserver.c:242-253`, `if (protocol_version > remote_protocol) { ... protocol_version--;`.

`remote_protocol` is set during this greeting exchange (via `sscanf`), so it is nonzero after the exchange.

#### Module selection

After the greeting, the client sends a single line (text, newline-terminated) which is either a module name or a special command.  Source: `.upstream/clientserver.c:1414`, `int start_daemon(int f_in, int f_out)`, `.upstream/clientserver.c:1538`, `if (!read_line_old(f_in, line, sizeof line, 0))` (reads the line via `read_line_old()`).  Before the module line, the client may send `#early_input=<len>` (see below).

The server processes the line in this order (`.upstream/clientserver.c:1541-1572`, `if (strncmp(line, EARLY_INPUT_CMD, EARLY_INPUT_CMDLEN) == 0) { ... }`):

1. **`#early_input=<len>`** -- if the line starts with `#early_input=`, the server reads `len` raw bytes of binary data, then reads the actual module line again (`.upstream/clientserver.c:1541-1552`, `if (strncmp(line, EARLY_INPUT_CMD, EARLY_INPUT_CMDLEN) == 0) { ... }`).  Used for `--early-input` on the client side (`.upstream/options.c:855`, `{"early-input",      0,  POPT_ARG_STRING, &early_input_file, 0, 0, 0 },`, sending code at `.upstream/clientserver.c:299-338`, `if (early_input_file) { ... }`).  If `len` is invalid or exceeds `BIGPATHBUFLEN`, the server sends `@ERROR: invalid early_input length` and closes.  This is an implementation detail for passing binary data before module selection; most clients never use it.

2. **`#list` or empty line** -- if the line is exactly `#list` or empty (`!\*line || strcmp(line, "#list") == 0`), the server sends a module listing and closes the connection (`.upstream/clientserver.c:1554-1559`, `if (!*line || strcmp(line, "#list") == 0) { ... }`).  Listing format per module (`.upstream/clientserver.c:1374-1386`):, `static void send_listing(int fd) ... }`
   ```
   %-15s\t%s\n
   ```
   where the first field is the module name (left-justified, 15 chars) and the second is the module comment, separated by a tab.  Only modules with `list = true` in rsyncd.conf are included (`.upstream/clientserver.c:1380`).  After the listing, if `protocol_version >= 25`, the server sends `@RSYNCD: EXIT\n` (`.upstream/clientserver.c:1385`).  For proto < 25, the server just closes the connection (no EXIT marker) and the client uses EOF as t, `if (lp_list(i))`he terminator (`.upstream/clientserver.c:399`).  The client reads lines until `@RSYNCD: EX, `kluge_around_eof = list_only && protocol_version < 25 ? 1 : 0;`IT` or EOF and then exits cleanly (`.upstream/clientserver.c:401-437`, `while (1) { ... kluge_around_eof = 0;`, EXIT handling at `.upstream/clientserver.c:416-421`, `if (strcmp(line,"@RSYNCD: EXIT") == 0) { ... exit(0);`).

3. **Unknown `#` command** -- if the line starts with `#` but is not `#list` or `#early_input=`, the server sends `@ERROR: Unknown command '<line>'\n` and closes (`.upstream/clientserver.c:1561-1565`, `if (*line == '#') { ... }`).

4. **Normal module name** -- the server looks up the module by name via `lp_number(line)` (`.upstream/clientserver.c:1567-1572`, `if ((i = lp_number(line)) < 0) { ... }`).  If not found, sends `@ERROR: Unknown module '<name>'\n` and closes.  On success, proceeds to authentication and argument transmission.

The server may also send a message-of-the-day (free-format text) after authentication but before the arguments phase.

#### Authentication (optional)

If the module requires authentication, the server sends:

```
@RSYNCD: AUTHREQD <base64-challenge>
```

The client responds:

```
<username> <base64-digest>
```

The server responds:

```
@RSYNCD: OK
```

Source: `.upstream/clientserver.c:408-414`, `if (strncmp(line,"@RSYNCD: AUTHREQD ",18) == 0) { ... break;` (auth challenge/response), `.upstream/clientserver.c:809`, `auth_user = auth_server(f_in, f_out, i, host, addr, "@RSYNCD: AUTHREQD ");` (`auth_server()` sends challenge).

#### Argument transmission

After authentication, the client sends rsync command-line arguments.  Source: `.upstream/clientserver.c:263-464`, `int start_inband_exchange(int f_in, int f_out, const char *user, int argc, ch... ... }` (`start_inband_exchange()`; args sent at `.upstream/clientserver.c:439-451`, `if (rl_nulls) { ... }`), `.upstream/io.c:1454-1525`, `void read_args(int f_in, char *mod_name, char *buf, size_t bufsiz, int rl_nulls, ... }` (`read_args()`, invoked at `.upstream/clientserver.c:1154`, `read_args(f_in, name, line, sizeof line, rl_nulls, 1, &argv, &argc, &request);`).

- **Protocol ≥ 30:** Null-terminated (`.upstream/clientserver.c:258`)., `rl_nulls = 1;`
- **Protocol < 30:** Newline-terminated.

First arg is always `"."`.  Double null (`\x00\x00`) or double newline (`\n\n`) terminates the list.

The `e` flag argument contains `client_info` feature flags (letters like `i`, `L`, `s`, `f`, `x`, `C`, `I`, `v`, `u`) that are parsed on the server side to set compat flags.  Source: `.upstream/compat.c:724-740`, `compat_flags = allow_inc_recurse ? CF_INC_RECURSE : 0; ... compat_flags |= CF_ID0_NAMES;`.

#### Binary version exchange is skipped

The binary version exchange at `.upstream/compat.c:602-609`, `if (remote_protocol == 0) { ... protocol_version = remote_protocol;` is **skipped** for daemon connections because `remote_protocol` is already set during the greeting exchange.  The guard `if (remote_protocol == 0)` at `.upstream/compat.c:602`, `if (remote_protocol == 0) {` is false for daemon connections.

### 1.2 SSH/rsh transport

No greeting exchange -- the rsync binary is invoked remotely via shell.  Binary version exchange replaces the text greeting.

#### Binary version exchange

Source: `.upstream/compat.c:602-609`, `if (remote_protocol == 0) { ... protocol_version = remote_protocol;`.

```c
if (remote_protocol == 0) {
    if (am_server && !local_server)
        check_sub_protocol();
    if (!read_batch)
        write_int(f_out, protocol_version);
    remote_protocol = read_int(f_in);
    if (protocol_version > remote_protocol)
        protocol_version = remote_protocol;
}
```

- Sends `protocol_version` as 4-byte LE int via `write_int()` (`.upstream/compat.c:606`, `write_int(f_out, protocol_version);`).
- Reads `remote_protocol` as 4-byte LE int via `read_int()` (`.upstream/compat.c:607`, `remote_protocol = read_int(f_in);`).
- Negotiates down to the lower version (`.upstream/compat.c:608-609`, `if (protocol_version > remote_protocol) ... protocol_version = remote_protocol;`).
- This exchange **only** happens when `remote_protocol == 0` (the initial value for SSH/rsh connections, `.upstream/compat.c:75`, `int remote_protocol = 0;`).

#### No module selection or authentication

Arguments are passed as command-line args to the remote rsync process.  No text-based module selection or digest-based authentication.

### 1.3 Key differences

| Aspect | Daemon socket | SSH/rsh |
|--------|---------------|---------|
| Greeting | `@RSYNCD: version.sub digests` (text) | None |
| Version exchange | During greeting (parsed from text) | Binary `write_int`/`read_int` (`.upstream/compat.c:606-607`, `write_int(f_out, protocol_version); ... remote_protocol = read_int(f_in);`) |
| Module selection | Yes (text) | No |
| Module listing (`#list`) | Yes (daemon-socket-only) | No |
| Authentication | Yes (digest-based, optional) | No (delegated to SSH/rsh) |
| Argument format | Text (newline or null-terminated) | Command-line args |
| `remote_protocol` initial value | Set by greeting parse | 0 (triggers binary exchange) |
| `daemon_connection` value | 1 (via shell) or -1 (direct socket) | 0 |

## 2. Protocol version reference (20--32)

### 2.1 Version constants

Source: `.upstream/rsync.h:113-149`, `* SUBPROTOCOL_VERSION if it is not a final (official) release. */ ... #define MAX_PROTOCOL_VERSION 40`.

| Constant | Value | Notes |
|----------|-------|-------|
| `MIN_PROTOCOL_VERSION` | 20 | Oldest still supported |
| `OLD_PROTOCOL_VERSION` | 25 | Threshold for "very old" warning |
| `PROTOCOL_VERSION` | 32 | Current latest |
| `MAX_PROTOCOL_VERSION` | 40 | Upper bound for compatibility |
| `SUBPROTOCOL_VERSION` | 0 | 0 = final release, nonzero = pre-release |

### 2.2 Version-by-version changes

#### Protocol 20 (rsync 2.3.0, 1999)

`MIN_PROTOCOL_VERSION` -- oldest still supported.  No special features; baseline protocol.

#### Protocol 21

Checksum algorithm change.  Source: `.upstream/checksum.c:145`, `else if (protocol_version >= 21)` (`if (protocol_version >= 21)`).  The checksum2 (strong) seed ordering was changed.

#### Protocol 22

File list and argument handling changes.  Source: `.upstream/clientserver.c:456-457`, `if (protocol_version < 23) { ... if (protocol_version == 22 || !am_sender)` (`if (protocol_version == 22 || !am_sender)`), `.upstream/clientserver.c:1226`, `if (protocol_version < 23 && (protocol_version == 22 || am_sender))`.

#### Protocol 23 -- multiplexed I/O layer introduced

Major change: the multiplexed I/O layer was introduced, allowing MSG_DATA frames to be mixed with control messages (MSG_SUCCESS, MSG_ERROR, etc).  This is the version where `io_start_multiplex_in/out` gates appear.

Source: `.upstream/main.c:1293`, `io_start_multiplex_out(f_out);` (`if (protocol_version >= 23) io_start_multiplex_out(f_out)`), `.upstream/main.c:1399`, `io_start_multiplex_in(f_in);` (`if (protocol_version >= 23) io_start_multiplex_in(f_in)`).

Before protocol 23, all I/O was buffered (raw bytes).  From protocol 23 onward, the daemon→client channel (file data) uses multiplexed I/O.

#### Protocol 24 -- final goodbye message

Added the final goodbye message exchange.  Source: `.upstream/main.c:1154-1157`, `if (protocol_version >= 24) { ... }` (`if (protocol_version >= 24) write_ndx(f_out, NDX_DONE)`), `.upstream/main.c:996-997`, `if (protocol_version >= 24) ... read_final_goodbye(f_in, f_out);` (`if (protocol_version >= 24) read_final_goodbye(f_in, f_out)`), `.upstream/main.c:1384-1385`, `if (protocol_version >= 24) ... read_final_goodbye(f_in, f_out);` (`if (protocol_version >= 24) read_final_goodbye(f_in, f_out)`).

#### Protocol 25 (rsync 2.5.0, 2001)

`@RSYNCD: EXIT` command, `OLD_PROTOCOL_VERSION` threshold.  Source: `.upstream/clientserver.c:1384`, `if (protocol_version >= 25)` (`if (protocol_version >= 25)` for `@RSYNCD: EXIT`), `.upstream/clientserver.c:399`, `kluge_around_eof = list_only && protocol_version < 25 ? 1 : 0;` (`kluge_around_eof = list_only && protocol_version < 25 ? 1 : 0`).

#### Protocol 26 (rsync 2.4.6pre1)

Device number encoding changes.  Source: `.upstream/flist.c:744`, `if (protocol_version < 26) {` (send side), `.upstream/flist.c:1346`, `if (protocol_version < 26) {` (receive side; `if (protocol_version < 26)` -- 32-bit dev_t/ino_t for proto < 26, 64-bit for proto ≥ 26).

#### Protocol 27 (rsync 2.6.0, 2004)

Per-file strong checksum length (`s2length` in sum_head).  Source: `.upstream/io.c:2240`, `sum->s2length = protocol_version < 27 ? csum_length : (int)read_int(f);` (`sum->s2length = protocol_version < 27 ? csum_length : (int)read_int(f)`), `.upstream/io.c:2267`, `write_int(f, sum->s2length);` (`if (protocol_version >= 27) write_int(f, sum->s2length)`), `.upstream/checksum.c:143`, `if (protocol_version >= 27)`.

Before protocol 27, the strong checksum length was fixed to `csum_length` (MD4 = 16 bytes).  From protocol 27, it is sent as a separate int32 in the sum_head.

#### Protocol 28 (rsync 2.6.1, 2004)

Extended xmit flags (`XMIT_EXTENDED_FLAGS`), device major/minor 32-bit accuracy, hard link support in file list.  Source: `.upstream/rsync.h:50`, `#define XMIT_EXTENDED_FLAGS (1<<2)	/* protocols 28 - now */` (`XMIT_EXTENDED_FLAGS` at bit 2, replacing `XMIT_SAME_RDEV_pre28`), `.upstream/flist.c:531`, `if (protocol_version < 28) {` (`if (protocol_version < 28)`), `.upstream/flist.c:1025`, `if (protocol_version < 28) {` (device encoding changes).

Key changes:
- Xmit flags use `write_shortint()` (2 bytes LE) when `XMIT_EXTENDED_FLAGS` is set, instead of `write_byte()` (1 byte).
- Device major is sent as varint30, minor as byte (if ≤ 255) or int32.
- New xmit flags: `XMIT_SAME_RDEV_MAJOR` (bit 8), `XMIT_HLINKED` (bit 9), `XMIT_SAME_DEV_pre30` (bit 10), `XMIT_RDEV_MINOR_8_pre30` (bit 11).
- `always_checksum` applies only to regular files (not other types).  Source: `.upstream/flist.c:1365`, `if (always_checksum && (real_ISREG_entry || protocol_version < 28)) {` (`always_checksum && (real_ISREG_entry || protocol_version < 28)`).

#### Protocol 29 (rsync 2.6.4, 2005)

Major protocol restructuring: phase exchange changes, iflags in selectors, keep-alive, filter rule improvements.  Source: `.upstream/compat.c:690`, `if (protocol_version < 29) {` (feature requirement gates), `.upstream/sender.c:498`, `int phase = 0, max_phase = protocol_version >= 29 ? 2 : 1;` (`max_phase = protocol_version >= 29 ? 2 : 1`), `.upstream/receiver.c:809`, `int max_phase = protocol_version >= 29 ? 2 : 1;` (same gate).

Key changes:
- Phase exchange: `max_phase = 2` (was 1).  The sender and receiver now have a two-phase handshaking protocol.  Source: `.upstream/sender.c:498`., `int phase = 0, max_phase = protocol_version >= 29 ? 2 : 1;`
- Item flags (iflags): selectors include a 2-byte LE iflags field sent via `write_shortint()`.  Source: `.upstream/rsync.c:384-385`, `iflags = protocol_version >= 29 ? read_shortint(f_in) ... : ITEM_TRANSFER | ITEM_MISSING_DATA;` (`iflags = protocol_version >= 29 ? read_shortint(f_in) : ITEM_TRANSFER | ITEM_MISSING_DATA`), `.upstream/generator.c:588-597`, `if ((iflags & (SIGNIFICANT_ITEM_FLAGS|ITEM_REPORT_XATTR) || INFO_GTE(NAME, 2) ... write_vstring(sock_f_out, xname, strlen(xname));` (`itemize()`, `if (protocol_version >= 29)` sends iflags via `write_shortint()`).
- For proto < 29, iflags defaults to `ITEM_TRANSFER | ITEM_MISSING_DATA` (both set).
- Keep-alive: the sender can send keep-alive messages via `maybe_send_keepalive()` (`.upstream/io.c:1613`, `void maybe_send_keepalive(time_t now, int flags)`).  Modern rsync sends an empty `MSG_DATA` frame as keep-alive.  Older rsync versions used `MSG_NOOP` for proto 30 (`.upstream/io.c:1732-1733`, `case MSG_NOOP: ... /* Support protocol-30 keep-alive method. */`, comment).  The proto < 31 vs ≥ 31 distinction affects the files-from forwarding path (`.upstream/io.c:1372-1374`), not the keep-alive itself., `void start_filesfrom_forwarding(int fd) ... if (protocol_version < 31 && OUT_MULTIPLEXED) {`
- Filter rules: full filter-rule support with delete modes.  Source: `.upstream/exclude.c:1947`., `|| (delete_mode && (!delete_excluded || protocol_version >= 29));`
- Delete phases: `delete_before`, `delete_during`, `delete_after` support.  Source: `.upstream/compat.c:684-688`, `if (protocol_version < 30) ... }`.
- Features requiring proto ≥ 29: `--fuzzy`, `--inplace` with `--link-dest`, multiple `--link-dest`, `--prune-empty-dirs`.  Source: `.upstream/compat.c:691-719`, `if (fuzzy_basis) { ... protocol_version);`.

#### Protocol 30 (rsync 3.0.0, 2008)

Major overhaul: compressed NDX, varint xmit flags, compat flags exchange, subprotocol version, null-terminated args, MD5 checksums.  Source: `.upstream/compat.c:722`, `} else if (protocol_version >= 30) {`, `.upstream/io.c:2510`, `write_int(f, ndx);` (`if (protocol_version < 30 || read_batch)`), `.upstream/io.c:2557`, `if (protocol_version < 30)`.

Key changes:
- **Compressed NDX:** `write_ndx()`/`read_ndx()` use delta-encoded single-byte format instead of 4-byte LE int.  NDX_DONE is 1 byte `0x00`.  Source: `.upstream/io.c:2503-2547`, `void write_ndx(int f, int32 ndx) ... }`, `.upstream/io.c:2550-2592`, `int32 read_ndx(int f) ... }`.  A peer-supplied index that overflows a signed int32 is rejected (`MAX_INT32` gate, `.upstream/io.c:2582-2586`, `if (unum > (uint32)MAX_INT32) { ... }`).
- **Varint xmit flags:** When `CF_VARINT_FLIST_FLAGS` is set (via `v` in client_info), xmit flags use varint encoding instead of byte/shortint.  Source: `.upstream/flist.c:644`, `if (xfer_flags_as_varint)`.
- **Compat flags exchange:** Server sends compat flags as varint; client reads and sets features accordingly.  Source: `.upstream/compat.c:722-788`, `} else if (protocol_version >= 30) { ... need_messages_from_generator = 1;`.
- **`need_messages_from_generator` is always 1** for proto ≥ 30.  Source: `.upstream/compat.c:788`, `need_messages_from_generator = 1;` (`need_messages_from_generator = 1`), set unconditionally inside the `} else if (protocol_version >= 30) {` block.  This is set for ALL processes (not just senders).  For the daemon sender, this causes `start_server()` to set multiplexed input initially (for the filter list phase), but `do_server_sender()` switches to buffered input before reading selectors.
- **Null-terminated args:** `rl_nulls = 1` for proto ≥ 30.  Source: `.upstream/clientserver.c:258`, `rl_nulls = 1;`.
- **MD5 checksums:** Default checksum algorithm is MD5 instead of MD4.  Source: `.upstream/compat.c:415`, `env_str = ntype == NSTR_COMPRESS ? "zlib" : protocol_version >= 30 ? "md5" : ...` (`protocol_version >= 30 ? "md5" : "md4"`).
- **Subprotocol version:** Greeting includes subprotocol version number.  Source: `.upstream/compat.c:844`, `void output_daemon_greeting(int f_out, int am_client)` (`io_printf(f_out, "@RSYNCD: %d.%d %s\n", ...)`).
- **ACL and xattr support:** `--acls` and `--xattrs` require proto ≥ 30.  Source: `.upstream/compat.c:664-681`, `if (protocol_version < 30) { ... }`.
- **Hard links:** New encoding with `XMIT_HLINK_FIRST` (bit 12), `XMIT_USER_NAME_FOLLOWS` (bit 10), `XMIT_GROUP_NAME_FOLLOWS` (bit 11).  Source: `.upstream/flist.c:1230`, `if (protocol_version >= 30) {` (`if (protocol_version >= 30 && BITS_SETnUNSET(xflags, XMIT_HLINKED, XMIT_HLINK_FIRST))`).
- **Max block size:** The checksum block length (blength) is now capped at `MAX_BLOCK_SIZE` (131072, 128 KiB) for protocol ≥ 30; older protocols allowed blength up to `OLD_MAX_BLOCK_SIZE` (536870912, 512 MiB).  Source: `.upstream/io.c:2197`, `int32 max_blength = protocol_version < 30 ? OLD_MAX_BLOCK_SIZE : MAX_BLOCK_SIZE;` (receive: `int32 max_blength = protocol_version < 30 ? OLD_MAX_BLOCK_SIZE : MAX_BLOCK_SIZE`), `.upstream/generator.c:725`, `int32 max_blength = protocol_version < 30 ? OLD_MAX_BLOCK_SIZE : MAX_BLOCK_SIZE;` (send: same gate).  Values: `MAX_BLOCK_SIZE = 1<<17` (131072), `OLD_MAX_BLOCK_SIZE = 1<<29` (536870912), `.upstream/rsync.h:161,164`.
- **SumHead validation:** Zero block length with nonzero count is rejected (`.upstream/io.c:2225-2229`, `if (sum->count && sum->blength == 0) { ... }`).  On 32-bit OFF_T, `count * blength` overflow is rejected (`.upstream/io.c:2234-2238`, `if (sum->blength > 0 && sum->count > MAX_INT32 / sum->blength) { ... }`).

#### Protocol 31 (rsync 3.1.0, 2013)

Nanosecond timestamps, client keep-alive, delete phase.  Source: `.upstream/rsync.h:66`, `#define XMIT_MOD_NSEC (1<<13)		/* protocols 31 - now */` (`XMIT_MOD_NSEC` at bit 13, proto ≥ 31).

Key changes:
- **Nanosecond timestamps:** `XMIT_MOD_NSEC` flag in xmit flags; `mod_nsec` sent as varint.  Source: `.upstream/flist.c:582-583`, `if (NSEC_BUMP(file) && protocol_version >= 31) ... xflags |= XMIT_MOD_NSEC;` (`if (NSEC_BUMP(file) && protocol_version >= 31) xflags |= XMIT_MOD_NSEC`), `.upstream/flist.c:1581`, `if (nsec && protocol_version >= 31)` (receive side).
- **Client keep-alive:** Client can send keep-alive messages via `maybe_send_keepalive()` (`.upstream/io.c:1613`, `void maybe_send_keepalive(time_t now, int flags)`).  The proto < 31 vs ≥ 31 distinction affects the files-from forwarding path (`.upstream/io.c:1372-1374`): for proto < 31, files-from data is sent without multiplexing., `void start_filesfrom_forwarding(int fd) ... if (protocol_version < 31 && OUT_MULTIPLEXED) {`
- **Delete phase:** Extra NDX_DONE exchange in final goodbye for delete operations.  Source: `.upstream/main.c:920-931`, `if (protocol_version >= 31 && i == NDX_DONE) { ... }` (`if (protocol_version >= 31 && i == NDX_DONE)`), `.upstream/generator.c:2867-2872`, `if (protocol_version >= 31 && EARLY_DELETE_DONE_MSG()) { ... }` (`if (protocol_version >= 31 && EARLY_DELETE_DONE_MSG())`).
- **`MSG_ERROR_EXIT`:** Synchronize error exit between siblings.  Source: `.upstream/rsync.h:305`, `MSG_ERROR_EXIT=86, /* synchronize an error exit (siblings and protocol >= 31) */` (`MSG_ERROR_EXIT=86`).
- **`MSG_DELETED`:** File deletion notification.  Source: `.upstream/rsync.h:307`, `MSG_DELETED=101,/* successfully deleted a file on receiving side */` (`MSG_DELETED=101`).
- **Data-duplicating bug fix:** Token ring checksum fix.  Source: `.upstream/token.c:485`, `if (protocol_version >= 31) /* Newer protocols avoid a data-duplicating bug */` (`if (protocol_version >= 31)`), `.upstream/token.c:713`, `if (protocol_version >= 31) /* Newer protocols avoid a data-duplicating bug */` (same gate).
- **Xattr optimization:** `want_xattr_optim` set for proto ≥ 31 unless `CF_AVOID_XATTR_OPTIM`.  Source: `.upstream/compat.c:758`, `want_xattr_optim = protocol_version >= 31 && !(compat_flags & CF_AVOID_XATTR_...`.
- **Safe incremental file list:** `use_safe_inc_flist` set for proto ≥ 31.  Source: `.upstream/compat.c:787`, `use_safe_inc_flist = (compat_flags & CF_SAFE_FLIST) || protocol_version >= 31;`.
- **IO timeout message:** Daemon can send `MSG_IO_TIMEOUT` to client.  Source: `.upstream/main.c:1294-1295`, `if (am_daemon && io_timeout && protocol_version >= 31) ... send_msg_int(MSG_IO_TIMEOUT, io_timeout);` (`if (am_daemon && io_timeout && protocol_version >= 31) send_msg_int(MSG_IO_TIMEOUT, io_timeout)`).

#### Protocol 32 (rsync 3.2.7, 2024)

Security fix version number bump, digest name list on greeting becomes mandatory.  Source: `.upstream/clientserver.c:234-240`, `} else if (remote_protocol > 31) { ... }` (fatal error if omitted on proto > 31).

Key changes:
- **Digest name list mandatory:** The digest list has always been included in the greeting by modern rsync (`.upstream/compat.c:851-853` calls `get_default_nno_list()`).  For protocol 32+, omitting it is a fatal error (`.upstream/clientserver.c:234-240`, gate `remote_prot, `get_default_nno_list(&valid_auth_checksums, tmpbuf, MAX_NSTR_STRLEN, '\0'); ... io_printf(f_out, "@RSYNCD: %d.%d %s\n", protocol_version, our_sub, tmpbuf);`ocol > 31`).  For proto 30-31, it was optional (defaults to `md5`); for proto < 30, defaults to `md4`.
- **Security fixes:** Protocol version bumped for security reasons (CVE-related).

### Protocol 32 (rsync 3.5.0, 2026) -- security hardening

The protocol version remains 32, but rsync 3.5.0 adds significant wire-level security hardening:

- **IOERR_VALID_MASK** (`.upstream/rsync.h:199`, `* code: cleanup.c maps only these defined bits onto RERR_* values.) */`): Peer-supplied `MSG_IO_ERROR` values are masked to the defined `IOERR_*` bits (`IOERR_GENERAL | IOERR_VANISHED | IOERR_DEL_LIMIT`), preventing a malicious peer from setting arbitrary undefined bits in the local `io_error`.  Applied on receipt in `read_a_msg()` (`.upstream/io.c:1702-1707`, `case MSG_IO_ERROR: ... io_error |= val;`).
- **Daemon handshake timeout** (`.upstream/io.c:115-118`): A separate deadline spans the pre-transfer handshake , `/* Absolute wall-clock bound for peer-controlled daemon handshake reads. ... static time_t daemon_handshake_deadline;`(greeting, module selection, auth, argument reading), preventing an unauthenticated peer from holding a connection slot open indefinitely.  Default 60 seconds, configurable via module `timeout`.
- **MAX_DAEMON_ARGS** (`.upstream/io.c:1452`, `#define MAX_DAEMON_ARGS (MAX_ARGS * 16)`): The daemon argument count is bounded at `MAX_ARGS * 16` during `read_args()`, preventing unbounded memory growth from a peer trickling arguments.
- **Compressed NDX overflow protection** (`.upstream/io.c:2582-2586`, `if (unum > (uint32)MAX_INT32) { ... }`): A peer-supplied index that overflows signed int32 is rejected with RERR_PROTOCOL.
- **SumHead validation** (`.upstream/io.c:2195-2252`, `void read_sum_head(int f, struct sum_struct *sum) ... }`): Zero block length with nonzero count is rejected; on 32-bit OFF_T, `count * blength` overflow is rejected.
- **MSG_IO_TIMEOUT validation** (`.upstream/io.c:1712-1731`, `case MSG_IO_TIMEOUT: ... break;`, cap at `.upstream/io.c:1724-1725`, `if (val <= 0 || val > 86400) ... break;`): The client caps the received value at 86400 seconds and rejects non-positive values, preventing a malicious server from disabling the client's timeout or overflowing signed arithmetic.
- **Negotiation string fix** (`.upstream/compat.c:333-361`): Each side now picks its own most-preferred algorithm th, `static int parse_negotiate_str(struct name_num_obj *nno, char *tmpbuf) ... return 1;`at also appears in the peer's list (was: server stopped at first acceptable client choice).  Honest peers converge on the strongest mutual choice; a peer that front-loads a weaker name only desyncs itself.
- **iobuf.in_multiplexed placement** (`.upstream/io.c:1655-1900`): The `iobuf.in_multiplexed = 1` flag is now set AFTER message , `static void read_a_msg(void) ... }`processing (not before), ensuring the message handler runs in a clean state.
- **uid_ndx/gid_ndx timing** (`.upstream/compat.c:641-651`, `if (read_batch) ... xattrs_ndx = ++file_extra_cnt;`): The `uid_ndx`/`gid_ndx`/`acls_ndx`/`xattrs_ndx` assignments are now done AFTER `check_batch_flags()`, preventing a batch file's stream-flags from flipping preserve_uid/gid/acls/xattrs on with an uninitialized ndx slot.

### 2.3 Version gate summary

All `protocol_version` gates found via `grep -rn "protocol_version" .upstream/*.c`:

| File | Line | Gate | Effect |
|------|------|------|--------|
| checksum.c | 137 | `>= 30` | MD5 default |
| checksum.c | 143 | `>= 27` | s2length in sum_head |
| checksum.c | 145 | `>= 21` | checksum algorithm change |
| compat.c | 602 | `== 0` | binary version exchange gate |
| compat.c | 653 | `<= 28` | msgs2stderr default |
| compat.c | 664 | `< 30` | --acls/--xattrs require proto 30 |
| compat.c | 684 | `< 30` | delete_before default |
| compat.c | 690 | `< 29` | feature requirement errors |
| compat.c | 722 | `>= 30` | compat flags exchange |
| compat.c | 758 | `>= 31` | xattr optimization |
| compat.c | 787 | `>= 31` | safe inc_flist |
| compat.c | 788 | `>= 30` | need_messages_from_generator |
| compat.c | 805 | `>= 30` | partial-dir filter perishable |
| clientserver.c | 257 | `>= 30` | null-terminated args |
| clientserver.c | 399 | `< 25` | kluge_around_eof |
| clientserver.c | 456-457 | `< 23`, `== 22` | arg reading |
| clientserver.c | 1226 | `< 23` | sender arg reading |
| clientserver.c | 1384 | `>= 25` | @RSYNCD EXIT |
| flist.c | 506 | `>= 30` | dir xflags |
| flist.c | 531 | `< 28` | pre-28 device encoding |
| flist.c | 542 | `< 30` | minor 8-bit gate |
| flist.c | 545 | `< 31` | special file rdev |
| flist.c | 548-556 | `< 28`, `< 30` | special file encoding |
| flist.c | 582 | `>= 31` | XMIT_MOD_NSEC |
| flist.c | 600 | `>= 30` | hlink encoding |
| flist.c | 620 | `>= 28` | hlink pre-30 |
| flist.c | 644-658 | varint/`>= 28`/`< 28` | xflags encoding |
| flist.c | 677 | `>= 30` | mtime as varlong |
| flist.c | 693, 705 | `< 30` | uid/gid int32 (else varint) |
| flist.c | 717 | `< 31` | special-file rdev |
| flist.c | 718-724 | `< 28`, `>= 30` | device encoding |
| flist.c | 741 | `< 30` | hlink dev number |
| flist.c | 744 | `< 26` | 32-bit dev_t |
| flist.c | 757 | `< 28` | always_checksum scope |
| flist.c | 874 | `>= 30` | hlink first |
| flist.c | 932 | `>= 30` | receive hlink |
| flist.c | 1000, 1011 | `< 30` | receive uid/gid |
| flist.c | 1025-1032 | `< 28`, `>= 30` | receive device |
| flist.c | 1098 | `< 28` | receive checksum |
| flist.c | 1230 | `>= 30` | hlink on receive |
| flist.c | 1336 | `>= 30` | hlink ndx on receive |
| flist.c | 1346 | `< 26` | 32-bit dev on receive |
| flist.c | 1365 | `< 28` | checksum on receive |
| flist.c | 1460 | `< 28` | log code |
| flist.c | 1581 | `>= 31` | nsec on receive |
| flist.c | 1629 | `>= 28` | xflags on receive |
| flist.c | 1656 | `>= 31` | nsec on receive |
| flist.c | 2529 | `>= 30` | relative_paths |
| flist.c | 2533 | `>= 30` | hlink init |
| flist.c | 2553 | `< 31` | hlink pre-31 |
| flist.c | 2792 | `>= 30` | hlink inc_recurse |
| flist.c | 2824 | `< 30` | hlink dev on receive |
| flist.c | 2957 | `>= 28` | extended flags on receive |
| flist.c | 3067 | `< 30` | dev number on receive |
| flist.c | 3164 | `< 29` | fnamecmp on receive |
| flist.c | 3354 | `>= 29` | fnamecmp dir |
| flist.c | 3560 | `>= 29` | fnamecmp type |
| generator.c | 590 | `>= 29` | iflags in selector |
| generator.c | 725 | `< 30` | max block size |
| generator.c | 742 | `< 27` | s2length gate |
| generator.c | 2725 | `>= 29` | itemizing |
| generator.c | 2747 | `>= 30` | TIMEFAIL flag |
| generator.c | 2748 | `< 30` | implied_dirs |
| generator.c | 2864 | `>= 29` | early delay done |
| generator.c | 2867 | `>= 31` | early delete done |
| generator.c | 2882 | `>= 29` | phase gate |
| generator.c | 2888 | `>= 31` | delete done in phase |
| generator.c | 2911 | `>= 31` | delete phase |
| io.c | 1374 | `< 31` | filesfrom mux switch |
| io.c | 1875 | `>= 31` | ERROR_EXIT |
| io.c | 2197 | `< 30` | max block size |
| io.c | 2240 | `< 27` | s2length default |
| io.c | 2266 | `>= 27` | s2length write |
| io.c | 2509 | `< 30` | write_ndx fallback |
| io.c | 2557 | `< 30` | read_ndx fallback |
| io.c | 2717 | `>= 30` | batch compat flags |
| main.c | 356, 374, 384 | `>= 29` | stats fields |
| main.c | 434 | `>= 29` | stats write |
| main.c | 436 | `>= 31` | stats write |
| main.c | 916 | `< 29` | read_final_goodbye |
| main.c | 920 | `>= 31` | delete phase in goodbye |
| main.c | 996 | `>= 24` | final goodbye |
| main.c | 1104 | `>= 29` | receiver final |
| main.c | 1154 | `>= 24` | generator goodbye |
| main.c | 1172 | `< 31` | filesfrom negation |
| main.c | 1202 | `>= 30` | server recv mux |
| main.c | 1292 | `>= 23` | server mux output |
| main.c | 1294 | `>= 31` | io timeout |
| main.c | 1356 | `>= 30` | client sender mux out |
| main.c | 1360 | `>= 31`, `>= 23` | client sender mux in |
| main.c | 1377 | `< 31`, `>= 23` | filesfrom mux |
| main.c | 1384 | `>= 24` | sender goodbye |
| main.c | 1398 | `>= 23` | client recv mux in |
| rsync.c | 384 | `>= 29` | iflags read |
| rsync.c | 388 | `< 30` | keepalive selector |
| sender.c | 340 | `>= 31` | lull mod |
| sender.c | 473 | `< 29` | iflags write |
| sender.c | 498 | `>= 29` | max_phase |
| sender.c | 668, 722 | `>= 30` | MSG_NO_SEND |
| sender.c | 809 | `>= 30` | io_error |
| token.c | 485, 713 | `>= 31` | data-dup bug fix |
## 3. Process architecture reference

Rsync uses **three cooperating processes** during a data transfer.  Understanding which process owns which file descriptor and I/O mode is essential for correct implementation.

### 3.1 Generator (client-side)

Parent process after `do_recv()` forks on the client side.  Purpose: receives the file list, sends selectors (file transfer requests) to the daemon, and reads status/completion messages from the receiver.  Source: `.upstream/main.c:1124`, `am_generator = 1;` (parent becomes generator after fork, `am_generator = 1`; `do_recv()` forks at `.upstream/main.c:1065`, `if ((pid = do_fork()) == -1) {`).

#### File descriptors (after fork)

- `sock_f_out` -- daemon socket (write).  Set by `io_set_sock_fds()` in `client_run()` (`.upstream/main.c:1335`, `io_set_sock_fds(f_in, f_out);`).  Remains open through the generator's lifetime.  Used to send selectors and NDX_DONE to the daemon.
- `f_in` -- internal pipe from receiver.  Redirected from the daemon socket to `error_pipe[0]` after fork (`.upstream/main.c:1135-1136`, `sock_f_in = -1; ... f_in = error_pipe[0];`).  Used to read status messages (MSG_STATS, MSG_SUCCESS), NDX_DONE, and file list data (inc_recurse forwarding via `start_flist_forward()`) from the receiver.
- `f_out` -- daemon socket (write).  Unchanged from pre-fork; remains the daemon socket.  `io_start_buffering_out(f_out)` at `.upstream/main.c:1138`, `io_start_buffering_out(f_out);` sets buffered output on the daemon socket.
- `sock_f_in` -- set to -1 after fork (`.upstream/main.c:1135`, `sock_f_in = -1;`).

#### I/O mode (all protocol versions, no version gate)

Source: `.upstream/main.c:1138-1139`, `io_start_buffering_out(f_out); ... io_start_multiplex_in(f_in);`.

| Direction | Mode | Source |
|-----------|------|--------|
| Output (`f_out` → daemon socket) | **buffered** (raw bytes) | `.upstream/main.c:1138`, `io_start_buffering_out(f_out);` (`io_start_buffering_out(f_out)`) |
| Input (`f_in` ← receiver pipe) | **multiplexed** (MSG_DATA frames) | `.upstream/main.c:1139`, `io_start_multiplex_in(f_in);` (`io_start_multiplex_in(f_in)`) |

**Key detail:** The generator's output to the daemon socket is buffered, even though `client_run()` may have set it to multiplexed before the fork.  `io_start_buffering_out(f_out)` at `.upstream/main.c:1138`, `io_start_buffering_out(f_out);` overrides any earlier `io_start_multiplex_out(f_out)` from `.upstream/main.c:1357`, `io_start_multiplex_out(f_out);`.

In `generate_files(f_out, local_name)`, the `f_out` parameter is the daemon socket fd (not the internal pipe).  The generator writes selectors and NDX_DONE to the daemon socket via `write_ndx(f_out, ndx)` (selectors at `.upstream/generator.c:2376`, `write_ndx(f_out, ndx);`, NDX_DONE at `.upstream/generator.c:2861`, `write_ndx(f_out, NDX_DONE);`).  The generator reads status messages, NDX_DONE, and file list data (inc_recurse) from the receiver via `wait_for_receiver()` (`.upstream/io.c:1920`, `void wait_for_receiver(void)`), which reads from `iobuf.in_fd` (the internal pipe, set up as `f_in`).

**Source:** `.upstream/main.c:1124-1163`, `am_generator = 1; ... }` (generator side of fork), `.upstream/generator.c:2716-2929`, `void generate_files(int f_out, const char *local_name) ... }`.

### 3.2 Receiver (client-side)

Child process after `do_recv()` forks on the client side.  Purpose: reads file data from the daemon socket, writes files to disk, and sends completion status to the generator.  Source: `.upstream/main.c:1071`, `am_receiver = 1;` (child becomes receiver after fork, `am_receiver = 1`; `do_recv()` forks at `.upstream/main.c:1065`, `if ((pid = do_fork()) == -1) {`).

#### File descriptors

- `f_in` -- daemon socket (read).  Inherits the daemon socket from the parent.  Used to read echoed selectors, file data, and checksums from the daemon.
- `f_out` -- internal pipe to generator.  Set to `error_pipe[1]` (`.upstream/main.c:1080`, `sock_f_out = -1;`).  Used to send MSG_SUCCESS, NDX_DONE, and file list data (inc_recurse forwarding) to the generator.
- `sock_f_out` -- set to -1 after fork (`.upstream/main.c:1079`, `close(f_out);`).

#### I/O mode

Source: `.upstream/main.c:1086-1087`, `io_start_buffering_in(f_in); ... io_start_multiplex_out(f_out);`.

| Direction | Mode | Source |
|-----------|------|--------|
| Input (`f_in` ← daemon socket) | **multiplexed** (MSG_DATA frames) | Inherited from `.upstream/main.c:1399`, `io_start_multiplex_in(f_in);` (`io_start_multiplex_in(f_in)` for proto ≥ 23).  Overridden to buffered by `.upstream/main.c:1086`, `io_start_buffering_in(f_in);` if `read_batch`. |
| Output (`f_out` → generator pipe) | **multiplexed** (MSG_DATA frames) | `.upstream/main.c:1087`, `io_start_multiplex_out(f_out);` (`io_start_multiplex_out(f_out)`) |

**Key detail:** The receiver reads from the daemon socket using multiplexed input (transparently unwraps MSG_DATA frames) and writes to the generator pipe using multiplexed output (wraps in MSG_DATA frames).  When the receiver sends `write_int(f_out, NDX_DONE)` it writes 4 bytes (`0xFFFFFFFF`) to the generator pipe, wrapped in a MSG_DATA frame.

**Source:** `.upstream/main.c:1070-1122`, `if (pid == 0) { ... }` (receiver side of fork), `.upstream/receiver.c:795-1377`, `int recv_files(int f_in, int f_out, char *local_name) ... rprintf(FINFO,"recv_files finished\n");`.

### 3.3 Daemon (server-side)

Serves file data to clients.  Runs as either a sender (pull -- client requests files) or receiver (push -- client sends files).  Activated when `start_server()` is called on the server side.  Source: `.upstream/main.c:1284`, `void start_server(int f_in, int f_out, int argc, char *argv[])`.

#### File descriptors

- `f_in` -- daemon socket (read).  Reads selectors from the client generator.
- `f_out` -- daemon socket (write).  Sends file data, echoed selectors, and checksums to the client.

#### I/O mode

Source: `.upstream/main.c:1292-1313`, `if (protocol_version >= 23) ... io_start_buffering_in(f_in);`.

| Direction | Mode | Proto gate | Source |
|-----------|------|------------|--------|
| Output (`f_out` → client socket) | **multiplexed** (MSG_DATA frames) | proto ≥ 23 | `.upstream/main.c:1293`, `io_start_multiplex_out(f_out);` (`io_start_multiplex_out(f_out)`) |
| Input (`f_in` ← client socket) | **multiplexed** if `need_messages_from_generator`, else **buffered** (initially); switched to **buffered** for selector reading | see below | `.upstream/main.c:1310-1313` |, `if (need_messages_from_generator) ... io_start_buffering_in(f_in);`

`need_messages_from_generator` is set when:

- `protocol_version >= 30` (`.upstream/compat.c:788`, `need_messages_from_generator = 1;`) -- set unconditionally inside the `} else if (protocol_version >= 30) {` block in `setup_protocol()`.  This is set for ALL processes (both client and server, sender and receiver), not just senders.  The `if (am_sender)` guard is in `start_server()` (`.upstream/main.c:1297`, `if (am_sender) {`), which only checks the flag for the sender path.
- `remove_source_files` (`--remove-source-files`) is set (`.upstream/options.c:2360`, `if (remove_source_files) {`).

For a standard pull (`rsync -av host::mod/ ./`) with proto ≥ 30, `need_messages_from_generator` is 1 (set by compat.c:788), so the daemon sets multiplexed input initially (for the filter list phase).  However, `do_server_sender()` switches to buffered input before reading selectors (`.upstream/main.c:991`, `io_start_buffering_in(f_in);`).  For proto < 30, `need_messages_from_generator` remains 0 and daemon input is buffered throughout.

#### Server recv path (`do_server_recv()`)

Source: `.upstream/main.c:1200-1205`, `} ... io_start_buffering_in(f_in);`.

| Direction | Mode | Proto gate | Source |
|-----------|------|------------|--------|
| Input (`f_in` ← client socket) | **multiplexed** | proto ≥ 30 | `.upstream/main.c:1203`, `io_start_multiplex_in(f_in);` (`io_start_multiplex_in(f_in)`) |
| Input (`f_in` ← client socket) | **buffered** | proto < 30 | `.upstream/main.c:1205`, `io_start_buffering_in(f_in);` (`io_start_buffering_in(f_in)`) |

**Source:** `.upstream/main.c:1284-1317`, `void start_server(int f_in, int f_out, int argc, char *argv[]) ... do_server_recv(f_in, f_out, argc, argv);`.

## 4. Communication channel map

### 4.1 Channel 1: Generator → Daemon (selectors)

| Field | Value |
|-------|-------|
| Transport | Daemon socket |
| Output mode (generator) | Buffered (raw bytes) |
| Input mode (daemon) | Buffered (raw bytes) -- switched by `io_start_buffering_in(f_in)` in `do_server_sender()` |
| Wire format | Raw bytes from generator -- `write_ndx()` produces compressed NDX, `write_shortint()` produces 2-byte LE.  Daemon reads raw bytes. |
| Proto gate | All versions |
| Data | Selectors (NDX + iflags + optional attrs), NDX_DONE |

**Source:** Generator output: `.upstream/main.c:1138`, `io_start_buffering_out(f_out);`.  Daemon input: `.upstream/main.c:991`, `io_start_buffering_in(f_in);` (`io_start_buffering_in(f_in)` in `do_server_sender()`).  The daemon initially sets multiplexed input for proto ≥ 30 (`.upstream/main.c:1311`, `io_start_multiplex_in(f_in);`), but `do_server_sender()` switches to buffered input before reading selectors (`.upstream/main.c:991`, `io_start_buffering_in(f_in);`).  The filter list is read via multiplexed input (proto ≥ 30) before this switch.

### 4.2 Channel 2: Daemon → Receiver (file data, echoed selectors)

| Field | Value |
|-------|-------|
| Transport | Daemon socket |
| Output mode (daemon) | Multiplexed (MSG_DATA frames) |
| Input mode (receiver) | Multiplexed (MSG_DATA frames) |
| Wire format | MSG_DATA frames wrapping raw protocol bytes |
| Proto gate | Proto ≥ 23 |
| Data | Echoed selectors, sum_head, block checksums, delta fill data, MSG_SUCCESS, MSG_REDO, MSG_NO_SEND |

**Source:** Daemon output: `.upstream/main.c:1293`, `io_start_multiplex_out(f_out);`.  Receiver input: inherited from `.upstream/main.c:1399`, `io_start_multiplex_in(f_in);` (`io_start_multiplex_in` in `client_run()` receiver path, proto ≥ 23).

### 4.3 Channel 3: Receiver → Generator (status, file list forwarding)

| Field | Value |
|-------|-------|
| Transport | Internal pipe (`error_pipe`) |
| Output mode (receiver) | Multiplexed (MSG_DATA frames) |
| Input mode (generator) | Multiplexed (MSG_DATA frames) |
| Wire format | MSG_DATA frames wrapping raw protocol bytes |
| Proto gate | All versions |
| Data | `write_int(f_out, NDX_DONE)` (4-byte LE), MSG_STATS, MSG_SUCCESS, file list data (inc_recurse forwarding via `start_flist_forward()`) |

**Source:** Receiver output: `.upstream/main.c:1087`, `io_start_multiplex_out(f_out);`.  Generator input: `.upstream/main.c:1139`, `io_start_multiplex_in(f_in);`.

### 4.4 Channel 4: Daemon → Generator (non-selector messages, conditional)

| Field | Value |
|-------|-------|
| Transport | Daemon socket |
| Output mode (daemon) | Multiplexed (MSG_DATA frames) |
| Input mode (generator) | N/A -- generator does not read from daemon socket after fork |
| Wire format | MSG_* frames (MSG_ERROR, MSG_WARNING, MSG_INFO, etc) |
| Proto gate | Proto ≥ 23 (for mux), always for stderr messages |
| Data | Error messages, warnings, info messages |

**Note:** The generator does NOT read from the daemon socket after the fork.  Non-DATA messages sent by the daemon (MSG_ERROR, MSG_WARNING, etc) are read by the receiver via its multiplexed input and forwarded to the generator if needed.  The generator reads only from the internal pipe (Channel 3).

### 4.5 Mux frame visibility

When I/O mode is multiplexed, `write_buf()`/`read_buf()` transparently wrap and unwrap MSG_DATA frames, so application code never sees mux headers.  A raw socket capture will show 4-byte mux headers before each MSG_DATA payload.  In buffered mode, there are no mux headers and raw bytes go directly on the wire.

## 5. Wire protocol step-by-step (daemon socket)

Trace for `rsync -av host::mod/ ./` (pull, proto 32).  Focus on the daemon socket only.

### Step 1: Greeting exchange (text)

**Direction:** Simultaneous (both sides send, then read)

**Wire format:**
```
@RSYNCD: 32.0 md5 md4\n
```

**Process:** Client (pre-fork) ↔ Daemon

**Source:** `.upstream/compat.c:853`, `io_printf(f_out, "@RSYNCD: %d.%d %s\n", protocol_version, our_sub, tmpbuf);` (greeting sent via `io_printf(f_out, "@RSYNCD: %d.%d %s\n", ...)`), `.upstream/clientserver.c:209`, `if (sscanf(buf, "@RSYNCD: %d.%d", &remote_protocol, &remote_sub) < 1) {` (greeting parsed via `sscanf(buf, "@RSYNCD: %d.%d", ...)`).

**Details:**
- Both sides send their greeting simultaneously (simultaneous write, then read).
- Parse version, subprotocol, and digest list.
- Negotiate down to the lower version; subprotocol mismatch causes version downgrade.
- Digest list: space-separated, client preference wins.
- Subprotocol value is always included in the greeting format string (`.upstream/compat.c:853`, `io_printf(f_out, "@RSYNCD: %d.%d %s\n", protocol_version, our_sub, tmpbuf);`, format `@RSYNCD: %d.%d %s\n`).  If the server's `sscanf` parse yields `remote_sub < 0` (parse failure), it is a fatal error for proto >= 30 (`.upstream/clientserver.c:217-226`, `if (remote_sub < 0) { ... }`) and defaults to 0 for proto < 30.
- Digest list is always sent by modern rsync (`.upstream/compat.c:851-853` calls `get_default_nno_list()`).  For proto > 31 (proto 32+), omitting it, `get_default_nno_list(&valid_auth_checksums, tmpbuf, MAX_NSTR_STRLEN, '\0'); ... io_printf(f_out, "@RSYNCD: %d.%d %s\n", protocol_version, our_sub, tmpbuf);` is a fatal error (`.upstream/clientserver.c:234-240`, `} else if (remote_protocol > 31) { ... }`).  For proto 30-31, the receiver assumes `md5`; for proto < 30, it assumes `md4` (`.upstream/compat.c:555-556`, `else ... len = strlcpy(tmpbuf, protocol_version >= 30 ? "md5" : "md4", MAX_NSTR_STRLEN);`).

### Step 2: Module selection (text)

**Direction:** Client → Daemon

**Wire format:**
```
mod/\n
```

**Process:** Client (pre-fork) → Daemon

**Source:** `.upstream/clientserver.c:395`, `io_printf(f_out, "%.*s\n", modlen, modname);` (`io_printf(f_out, "%.*s\n", modlen, modname)` in `start_inband_exchange()`, `.upstream/clientserver.c:263-464`, `int start_inband_exchange(int f_in, int f_out, const char *user, int argc, ch... ... }`).

### Step 3: Authentication (text, optional)

**Direction:** Daemon → Client (challenge), Client → Daemon (response)

**Wire format:**
```
@RSYNCD: AUTHREQD <base64-challenge>\n
<username> <base64-digest>\n
@RSYNCD: OK\n
```

**Process:** Daemon ↔ Client (pre-fork)

**Source:** `.upstream/clientserver.c:408-414`, `if (strncmp(line,"@RSYNCD: AUTHREQD ",18) == 0) { ... break;` (auth challenge/response parsed inline), `.upstream/clientserver.c:809`, `auth_user = auth_server(f_in, f_out, i, host, addr, "@RSYNCD: AUTHREQD ");` (`auth_server()` sends challenge).

### Step 4: Argument transmission (text → binary transition)

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

**Source:** `.upstream/clientserver.c:439-451`, `if (rl_nulls) { ... }` (`start_inband_exchange()` sends arguments), `.upstream/clientserver.c:1154`, `read_args(f_in, name, line, sizeof line, rl_nulls, 1, &argv, &argc, &request);` (`read_args()` invoked; defined at `.upstream/io.c:1454-1525`, `void read_args(int f_in, char *mod_name, char *buf, size_t bufsiz, int rl_nulls, ... }`).  Null-terminated for proto ≥ 30 (`rl_nulls` set at `.upstream/clientserver.c:257`, `if (protocol_version >= 30)`), newline-terminated otherwise.

**Details:**
- First arg is always `"."` (current directory).
- The `e` flag: everything after `e` in the combined short-options arg is the `client_info` string (feature flags like `i`, `L`, `s`, `f`, `x`, `C`, `I`, `v`, `u`).
- Double null (`\x00\x00`) or double newline (`\n\n`) terminates.

(The binary protocol version exchange at `.upstream/compat.c:602-609`, `if (remote_protocol == 0) { ... protocol_version = remote_protocol;` is **skipped** for daemon connections because `remote_protocol` is already set during the greeting exchange.  The guard `if (remote_protocol == 0)` at `.upstream/compat.c:602`, `if (remote_protocol == 0) {` is false for daemon connections.  This exchange only happens for SSH/rsh transport.)

### Step 5: Compat flags exchange (binary, proto ≥ 30)

**Direction:** Daemon → Client

**Wire format:**
```
Server → Client: varint(compat_flags)
```

**Process:** Daemon → Client (pre-fork)

**Source:** `.upstream/compat.c:722-755`, `} else if (protocol_version >= 30) { ... }` (compat flags setup and exchange in `setup_protocol()`).

**Details:**
- Server builds `compat_flags` based on compile-time capabilities and client's advertised feature flags (from `-e` argument).  Source: `.upstream/compat.c:723-750`, `if (am_server) { ... write_varint(f_out, compat_flags);`.
- If client sent `V` flag (legacy superseded pre-release flag): sent as `write_byte()` (`.upstream/compat.c:747-749`, `compat_flags |= CF_VARINT_FLIST_FLAGS; ... } else`).  The `V` flag was from an old pre-release that got superseded; it forces `CF_VARINT_FLIST_FLAGS` and uses `write_byte()` instead of `write_varint()` for backward compatibility.
- Otherwise: sent as `write_varint()` (`.upstream/compat.c:750`, `write_varint(f_out, compat_flags);`).
- Client reads as `read_varint()` -- compatible with both `write_byte()` and `write_varint()` when the 0x80 bit is not set (`.upstream/compat.c:751-752`, `} else { /* read_varint() is compatible with the older write_byte() when the ... ... compat_flags = read_varint(f_in);`).
- If `CF_VARINT_FLIST_FLAGS` (`v` flag) is set, xmit flags use varint encoding.

**Compat flag bits:**

| Bit | Name | Meaning |
|-----|------|---------|
| 0 | `CF_INC_RECURSE` | Incremental file list |
| 1 | `CF_SYMLINK_TIMES` | Receiver can set symlink times |
| 2 | `CF_SYMLINK_ICONV` | Sender converts symlink content |
| 3 | `CF_SAFE_FLIST` | Safe incremental file list |
| 4 | `CF_AVOID_XATTR_OPTIM` | Avoid xattr optimization |
| 5 | `CF_CHKSUM_SEED_FIX` | Proper seed order (seed + data) |
| 6 | `CF_INPLACE_PARTIAL_DIR` | Inplace partial dir support |
| 7 | `CF_VARINT_FLIST_FLAGS` | Varint xmit flags |
| 8 | `CF_ID0_NAMES` | Send id0 names |

Source: `.upstream/compat.c:118-126`, `#define CF_INC_RECURSE	 (1<<0) ... #define CF_ID0_NAMES (1<<8)`.

### Step 6: Checksum/compression negotiation (binary)

**Direction:** Bidirectional (vstring exchange only when `do_negotiated_strings` is set)

**Wire format (when `do_negotiated_strings` is 1, proto ≥ 30 with `v` flag):**
```
Server → Client: vstring("md5 md4")       -- checksum list (if server has non-default checksums)
Server → Client: vstring("zlib")          -- compression list (if compression enabled)
Client → Server: vstring("md5 md4")       -- checksum list (if client has non-default checksums)
Client → Server: vstring("zlib")          -- compression list (if server sent one)
```

**Wire format (when `do_negotiated_strings` is 0):** No vstring data exchanged on the wire.  Each side silently validates that the default algorithm is acceptable.

**Process:** Client (pre-fork) ↔ Daemon

**Source:** `.upstream/compat.c:538-574`, `static void negotiate_the_strings(int f_in, int f_out) ... }`, called from `.upstream/compat.c:820`, `negotiate_the_strings(f_in, f_out);` in `setup_protocol()`.

**Details:**
- A **vstring** is: `length : uint8` (or 2 bytes if high bit set) followed by `data : raw[length]`.
- `negotiate_the_strings()` is called unconditionally for all protocol versions (`.upstream/compat.c:820`, `negotiate_the_strings(f_in, f_out);`).  However, `send_negotiate_str()` (`.upstream/compat.c:506-535`, `static void send_negotiate_str(int f_out, struct name_num_obj *nno, int ntype) ... write_vstring(f_out, tmpbuf, len);`) only calls `write_vstring()` when `do_negotiated_strings` is 1 (`.upstream/compat.c:533-534`, `* choice; a peer that front-loads a weaker name only desyncs itself. */ ... if (do_negotiated_strings)`).
- `do_negotiated_strings` is set to 1 when the compat flags exchange (step 5) includes `CF_VARINT_FLIST_FLAGS` (`.upstream/compat.c:742`, `do_negotiated_strings = 1;` server side, `.upstream/compat.c:752-754`, `compat_flags = read_varint(f_in); ... do_negotiated_strings = 1;` client side).  This requires proto ≥ 30 and the `v` flag in `client_info`.
- When `do_negotiated_strings` is 0: `send_negotiate_str()` sends nothing (`.upstream/compat.c:533-534`, `* choice; a peer that front-loads a weaker name only desyncs itself. */ ... if (do_negotiated_strings)`), and `recv_negotiate_str()` just validates that the default algorithm (`"md5"` for proto ≥ 30, `"md4"` otherwise) is acceptable (`.upstream/compat.c:555-558`, `else ... }`).  No vstring data is exchanged.
- When `do_negotiated_strings` is 1: each side sends its list *before* reading the other's list to avoid deadlock (`.upstream/compat.c:539`, `{`), and each side then picks its own most-preferred name that also appears in the peer's list (`.upstream/compat.c:333-361` comment)., `static int parse_negotiate_str(struct name_num_obj *nno, char *tmpbuf) ... return 1;`
- Compression negotiation only happens if `do_compression` is set (`.upstream/compat.c:547-548`, `if (do_compression && !compress_choice) ... send_negotiate_str(f_out, &valid_compressions, NSTR_COMPRESS);`).
- If the other side is too old to negotiate, `negotiate_the_strings` just ensures the environment didn't disallow the old algorithm (`.upstream/compat.c:572-574`, `if (!do_negotiated_strings) ... }`).

**When `CF_VARINT_FLIST_FLAGS` is set:** The full negotiation exchange happens, and the chosen algorithm is stored in `valid_checksums.negotiated_nni` / `valid_compressions.negotiated_nni`.  When not set, these are NULL and the defaults are used.

### Step 7: Checksum seed exchange (binary)

**Direction:** Daemon → Client

**Wire format:**
```
Server → Client: 0x9F 0x3A 0x01 0x00  (checksum_seed as int32 LE, example value)
```

**Process:** Daemon → Client (pre-fork)

**Source:** `.upstream/compat.c:822-828`, `if (am_server) { ... }` (checksum seed exchange in `setup_protocol()`).

**Details:**
- Server generates seed as `time(NULL) ^ (getpid() << 6)` if not already set.
- Sent as `write_int(f_out, checksum_seed)` (4 bytes LE).
- Client reads as `read_int(f_in)`.

### Step 8: I/O mode transition (internal, no wire data)

**Process:** Both sides

**Source:** `.upstream/main.c:1293`, `io_start_multiplex_out(f_out);` (daemon output), `.upstream/main.c:1356-1364`, `if (protocol_version >= 30) ... send_filter_list(f_out);` (client).

**What happens:**
- Daemon: `io_start_multiplex_out(f_out)` at `.upstream/main.c:1293`, `io_start_multiplex_out(f_out);` (proto ≥ 23).
- Daemon: `io_start_multiplex_in(f_in)` at `.upstream/main.c:1311`, `io_start_multiplex_in(f_in);` for proto ≥ 30 (because `need_messages_from_generator` is always 1), or `io_start_buffering_in(f_in)` at `.upstream/main.c:1313`, `io_start_buffering_in(f_in);` for proto < 30.
- Client sender: `io_start_multiplex_out(f_out)` at `.upstream/main.c:1357`, `io_start_multiplex_out(f_out);` (proto ≥ 30).
- Client sender: `io_start_multiplex_in(f_in)` at `.upstream/main.c:1361`, `io_start_multiplex_in(f_in);` (proto ≥ 31 or proto ≥ 23 without filesfrom_host, gate at `.upstream/main.c:1360`, `if (protocol_version >= 31 || (!filesfrom_host && protocol_version >= 23))`).
- Client receiver (pre-fork): `io_start_multiplex_in(f_in)` at `.upstream/main.c:1399`, `io_start_multiplex_in(f_in);` (proto ≥ 23).

**After this point, the daemon→client channel (Channel 2) flows through the multiplexed I/O layer.**  The generator→daemon channel (Channel 1) uses buffered I/O for selector reading: the generator writes selectors as raw bytes (buffered output), and the daemon reads them as raw bytes (buffered input, switched by `io_start_buffering_in(f_in)` in `do_server_sender()` at `.upstream/main.c:991`, `io_start_buffering_in(f_in);`).

### Step 9: Filter list transfer (binary)

**Direction:** Client → Daemon

**Wire format:** Mux-wrapped for proto ≥ 30, raw bytes for proto < 30 (filter rules in rsync internal format).

**Process:** Client sender → Daemon

**Source:** `.upstream/main.c:1364`, `send_filter_list(f_out);` (`send_filter_list(f_out)`), `.upstream/main.c:1314`, `recv_filter_list(f_in);` (`recv_filter_list(f_in)`).

**Details:**
- Sent AFTER mux output is started on the client side (`.upstream/main.c:1357`, `io_start_multiplex_out(f_out);`, proto ≥ 30), so wrapped in MSG_DATA frames for proto ≥ 30.  For proto < 30, client uses buffered output (`.upstream/main.c:1359`, `io_start_buffering_out(f_out);`) and the filter list is raw bytes.
- Daemon reads via buffered input (raw bytes) for proto < 30, or multiplexed input (transparently unwraps MSG_DATA) for proto ≥ 30 -- the daemon's input mode is set independently of the client's output mode.

### Step 10: File list transfer (binary, mux-wrapped)

**Direction:** Daemon → Client (server sends the file list to the client)

**Wire format:** Mux-wrapped raw bytes (file list entries with delta-encoded xflags).

**Process:** Daemon sender → Client receiver

**Source:** `.upstream/main.c:983`, `flist = send_file_list(f_out,argc,argv);` (`send_file_list(f_out, argc, argv)` on server sender side via `do_server_sender()`), `.upstream/main.c:1417`, `flist = recv_file_list(f_in, -1);` (`recv_file_list(f_in, -1)` on client).

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
[uid]             : varint (proto ≥ 30) or int32 LE (older) (if preserve_uid, !XMIT_SAME_UID)
    [username]    : vstring (if XMIT_USER_NAME_FOLLOWS)
[gid]             : varint (proto ≥ 30) or int32 LE (older) (if preserve_gid, !XMIT_SAME_GID)
    [groupname]   : vstring (if XMIT_GROUP_NAME_FOLLOWS)
[rdev_major]      : varint30/byte (if device, !XMIT_SAME_RDEV_MAJOR)
[rdev_minor]      : varint (proto ≥ 30) or byte/int32 (older)
[symlink_target_len] : varint30 (if symlink)
[symlink_target]  : raw[len]
[hlink_ndx]       : varint (if XMIT_HLINKED, !XMIT_HLINK_FIRST, proto ≥ 30)
[dev]             : longint (if hlink, proto 28-29, !XMIT_SAME_DEV_pre30)
[ino]             : longint (if hlink, proto 28-29)
[dev]             : int32 LE (if hlink, proto < 26, 1-incremented)
[ino]             : int32 LE (if hlink, proto < 26)
[checksum]        : raw[csum_len] (if always_checksum, regular file)
```

**End-of-list:** `NDX_DONE` sent as compressed NDX (1 byte `0x00` for proto ≥ 30).

### Step 11: Phase exchange (binary)

**Direction:** Bidirectional (generator ↔ daemon on daemon socket, receiver ↔ generator on internal pipe)

**Process:** Generator ↔ Daemon

**Source:** `.upstream/generator.c:2831-2929`, `if (!inc_recurse) { ... }` (`generate_files()` phase loop), `.upstream/sender.c:498-560`, `int phase = 0, max_phase = protocol_version >= 29 ? 2 : 1; ... "rsync: refusing transfer of cleared file index %d\n",` (`send_files()` phase loop), `.upstream/io.c:1920-1956`, `void wait_for_receiver(void) ... }`.

**Overview:** The phase exchange coordinates the end of the selector loop between the generator and daemon.  It is intertwined with the receiver's progress -- the generator waits for the receiver to finish processing each phase before signaling the daemon.

**The receiver's role:** During the selector loop, the receiver processes file data and sends completion messages (MSG_SUCCESS, MSG_REDO, etc) to the generator via the internal pipe.  When the receiver finishes a phase, it writes `NDX_DONE` as a 4-byte LE int (`write_int(f_out, NDX_DONE)`) to the generator pipe.  The generator's `wait_for_receiver()` (`.upstream/io.c:1935`, `msgdone_cnt++;`) reads this and increments `msgdone_cnt`.

**Phase exchange for proto ≥ 29 (max_phase = 2):**

The generator communicates on two channels simultaneously:
- **Daemon socket** (`f_out` / `sock_f_out`): writes selectors and NDX_DONE to the daemon (buffered output, raw bytes)
- **Internal pipe** (`f_in` / `iobuf.in_fd`): reads completion messages from the receiver (multiplexed input, MSG_DATA frames)

The daemon reads selectors from the generator on the daemon socket (buffered input) and writes echoed selectors and file data to the receiver on the same socket (multiplexed output).

**Main transfer phase (phase 0 → 1):**
1. Generator sends all selectors to daemon via `recv_generator()` (called per-file at `.upstream/generator.c:2818`, `recv_generator(fbuf, file, ndx, itemizing, code, f_out);`).
2. Generator waits for receiver to finish: `while (!msgdone_cnt) wait_for_receiver()` (`.upstream/generator.c:2850-2855`, `while (1) { ... }`).  The receiver writes `write_int(f_out, NDX_DONE)` (4-byte LE, -1) to the generator pipe (`.upstream/receiver.c:860`, `write_int(f_out, NDX_DONE);`), and `wait_for_receiver()` increments `msgdone_cnt` (`.upstream/io.c:1935`, `msgdone_cnt++;`).
3. Generator writes `write_ndx(f_out, NDX_DONE)` (compressed, 1 byte `0x00`) to daemon socket (`.upstream/generator.c:2861`, `write_ndx(f_out, NDX_DONE);`).
4. Daemon reads NDX_DONE via `read_ndx_and_attrs()` (`.upstream/sender.c:520-521`, `ndx = read_ndx_and_attrs(f_in, f_out, &iflags, &fnamecmp_type, ... xname, &xlen);`), increments phase, writes `write_ndx(f_out, NDX_DONE)` to client socket (`.upstream/sender.c:544`, `write_ndx(f_out, NDX_DONE);`).

**Redo phase (phase 1 → 2, proto ≥ 29):**
5. Generator may send an early NDX_DONE if `EARLY_DELAY_DONE_MSG()` is true (no `--delay-updates`): `write_ndx(f_out, NDX_DONE)` (`.upstream/generator.c:2864-2865`, `if (protocol_version >= 29 && EARLY_DELAY_DONE_MSG()) ... write_ndx(f_out, NDX_DONE);`).
6. For proto ≥ 31, if `EARLY_DELETE_DONE_MSG()` is true (no `--delete` or `--delay-deletes`): sends delete stats and another NDX_DONE (`.upstream/generator.c:2867-2872`, `if (protocol_version >= 31 && EARLY_DELETE_DONE_MSG()) { ... }`).
7. Generator waits for redo completion: `while (msgdone_cnt <= 1) wait_for_receiver()` (`.upstream/generator.c:2875-2880`, `while (1) { ... }`).
8. For proto ≥ 29, if not early: sends delay-updates NDX_DONE (`.upstream/generator.c:2886-2889`, `if (!EARLY_DELAY_DONE_MSG()) { ... write_ndx(f_out, NDX_DONE);`).
9. Generator waits for delay-updates completion: `while (msgdone_cnt == 2) wait_for_receiver()` (`.upstream/generator.c:2892`, `while (msgdone_cnt == 2)`).

**Delete phase (proto ≥ 31):**
10. For proto ≥ 31, if not early: sends delete stats and delete NDX_DONE (`.upstream/generator.c:2912-2916`, `if (!EARLY_DELETE_DONE_MSG()) { ... }`).
11. Generator waits for delete completion: `while (msgdone_cnt == 3) wait_for_receiver()` (`.upstream/generator.c:2919-2920`, `while (msgdone_cnt == 3) ... wait_for_receiver();`).

**For proto < 29 (max_phase = 1):**
- No phase exchange -- the generator just sends selectors and NDX_DONE, and the daemon processes them in a single pass.
- If `!inc_recurse`, the generator sends `write_ndx(f_out, NDX_DONE)` after all selectors (`.upstream/generator.c:2831-2833`, `if (!inc_recurse) { ... break;`).

**Critical distinction:** Generator uses `write_ndx(f_out, NDX_DONE)` which produces 1 byte `0x00` on the daemon socket (compressed NDX, buffered output).  Receiver uses `write_int(f_out, NDX_DONE)` which produces 4 bytes `0xFFFFFFFF` on the internal pipe (fixed-width int, multiplexed output).

### Step 12: Selector loop (binary)

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

**Source:** `.upstream/generator.c:588-597`, `if ((iflags & (SIGNIFICANT_ITEM_FLAGS|ITEM_REPORT_XATTR) || INFO_GTE(NAME, 2) ... write_vstring(sock_f_out, xname, strlen(xname));` (generator sends selectors via `sock_f_out` in `itemize()`; the transfer ndx is written at `.upstream/generator.c:2376`, `write_ndx(f_out, ndx);`), `.upstream/sender.c:520`, `ndx = read_ndx_and_attrs(f_in, f_out, &iflags, &fnamecmp_type,` (daemon reads selectors via `read_ndx_and_attrs`), `.upstream/sender.c:766-781`, `write_ndx_and_attrs(f_out, ndx, iflags, fname, file, fnamecmp_type, xname, xl... ... end_progress(st.st_size);` (sends sum_head + delta data).

**Details:**
- Generator sends selectors via `recv_generator()` (`.upstream/generator.c:1608-2457`, `static void recv_generator(char *fname, struct file_struct *file, int ndx, ... }`), which writes the ndx via `write_ndx(f_out, ndx)` (`.upstream/generator.c:2376`, `write_ndx(f_out, ndx);`) and calls `itemize()` (`.upstream/generator.c:517-612`, `void itemize(const char *fnamecmp, struct file_struct *file, int ndx, int sta... ... }`), which writes `write_shortint(sock_f_out, iflags)` for proto ≥ 29 (`.upstream/generator.c:588-597`, `if ((iflags & (SIGNIFICANT_ITEM_FLAGS|ITEM_REPORT_XATTR) || INFO_GTE(NAME, 2) ... write_vstring(sock_f_out, xname, strlen(xname));`).  Output is buffered (raw bytes).
- Daemon reads selectors via `read_ndx_and_attrs(f_in, f_out, ...)` (`.upstream/rsync.c:323-434`, `int read_ndx_and_attrs(int f_in, int f_out, int *iflag_ptr, uchar *type_ptr, ... ... *len_ptr = len;`), which uses buffered input (raw bytes) -- switched by `io_start_buffering_in(f_in)` in `do_server_sender()` (`.upstream/main.c:991`, `io_start_buffering_in(f_in);`).  This applies to all protocol versions.
- Daemon echoes each selector to the client via `write_ndx_and_attrs(f_out, ...)` (`.upstream/sender.c:468-483`, `static void write_ndx_and_attrs(int f_out, int ndx, int iflags, ... send_xattr_request(fname, file, f_out);`), which uses multiplexed output (MSG_DATA frames).
- Receiver reads echoed selectors via **multiplexed input** (transparently unwraps MSG_DATA).
- For TRANSFER selectors, daemon sends sum_head + block checksums + delta fill data (`.upstream/sender.c:766-781`; `write_sum_head()` at `.upstream/sender.c:767`, `write_sum_head(f_xfer, s);`; `match_sums()` at `.upstream/send, `write_ndx_and_attrs(f_out, ndx, iflags, fname, file, fnamecmp_type, xname, xl... ... end_progress(st.st_size);`er.c:779`, `match_sums(f_xfer, s, mbuf, st.st_size);`).
- For non-TRANSFER selectors, daemon just echoes the selector (`.upstream/sender.c:583-601`, `if (!(iflags & ITEM_TRANSFER)) { ... continue;`, echo at `.upstream/sender.c:585`, `write_ndx_and_attrs(f_out, ndx, iflags, fname, file, fnamecmp_type, xname, xl...`).
- The generator interleaves selector sending with `check_for_finished_files()` and periodic `maybe_send_keepalive()` / `maybe_flush_socket()` to monitor the receiver's progress (`.upstream/generator.c:2818-2827`, `recv_generator(fbuf, file, ndx, itemizing, code, f_out); ... next_loopchk += loopchk_limit;`).  This allows the generator to pause if the receiver is behind.
- For `inc_recurse`, the generator waits for sub-file-lists to arrive before continuing (`.upstream/generator.c:2836-2841`, `while (1) { ... }`).

### Step 13: Final goodbye (binary)

**Direction:** Bidirectional

**Wire format:**
```
Generator → Daemon: 0x00  (NDX_DONE as compressed NDX)
Daemon → Generator: 0x00  (NDX_DONE as compressed NDX, echoed)
[proto ≥ 31: Generator → Daemon: 0x00, Daemon → Generator: 0x00]
```

**Process:** Generator ↔ Daemon

**Source:** `.upstream/main.c:908-939`, `static void read_final_goodbye(int f_in, int f_out) ... }`, `.upstream/main.c:1154-1157`, `if (protocol_version >= 24) { ... }` (generator sends `write_ndx(f_out, NDX_DONE)` after `generate_files()` returns), `.upstream/main.c:996-997`, `if (protocol_version >= 24) ... read_final_goodbye(f_in, f_out);` (server reads via `read_final_goodbye()`).

**Details for proto ≥ 31:**
- Extra round-trip: after first NDX_DONE exchange, server writes another NDX_DONE and reads another from client.
- For proto < 29: server reads `read_int(f_in)` and verifies `NDX_DONE`.
- For proto 29-30: server reads via `read_ndx_and_attrs()` and verifies `NDX_DONE`.

### Step 14: Stats exchange (binary, mux-wrapped)

**Direction:** Server sender → Client receiver (for a pull operation)

**Wire format:**
```
varlong30(total_read)
varlong30(total_written)
varlong30(total_size)
[proto ≥ 29: varlong30(flist_buildtime), varlong30(flist_xfertime)]
```

**Process:** Server sender (writes) → Client receiver (reads)

**Source:** `.upstream/main.c:329-389`, `static void handle_stats(int f) ... }`.

**Details:**
- `handle_stats(f)` behavior depends on process role:
  - **Generator:** `handle_stats(-1)` -- returns early at `.upstream/main.c:344`, `return;` (`if (am_generator)` check).
  - **Server sender:** `handle_stats(f_out)` -- writes stats to client socket (`.upstream/main.c:353-359`, `write_varlong30(f, total_read, 3); ... }`).
  - **Server receiver (push):** `handle_stats(f_out)` -- returns early (`.upstream/main.c:347-349` && `!am_sender`)., `if (f == -1 || !am_sender) ... }`
  - **Client receiver:** `handle_stats(f_in)` -- reads stats from server (`.upstream/main.c:371-378`, `total_written = read_varlong30(f, 3); ... } else if (write_batch) {`).  Note: the first two fields are read in opposite order (total_written, then total_read) because the meaning of read/write swaps when switching from sender to receiver.
  - **Client sender:** `handle_stats(-1)` -- does nothing when `!am_sender` and `f < 0` (`.upstream/main.c:367`, `;`).  For `write_batch`, stats are written to the batch file (`.upstream/main.c:379-387`, `/* The --read-batch process is going to be a client ... }`).
- Sent AFTER `send_files()` returns but BEFORE `read_final_goodbye()` (`.upstream/main.c:994-998`, `io_flush(FULL_FLUSH); ... io_flush(FULL_FLUSH);`).

### Module listing (`#list`) wire trace

Trace for `rsync --list-only host::` (module listing, proto 32).  This is an alternative to the normal module selection -- the client sends `#list` instead of a module name.

**Step 1: Greeting exchange** -- same as normal (§5 Step 1).

**Step 2: Module listing request**

**Direction:** Client → Daemon

**Wire format:**
```
#list\n
```

**Process:** Client (pre-fork) → Daemon

**Source:** `.upstream/clientserver.c:1554`, `if (!*line || strcmp(line, "#list") == 0) {` (`strcmp(line, "#list") == 0`).

**Step 3: Module listing response**

**Direction:** Daemon → Client

**Wire format (per module):**
```
%-15s\t%s\n
```

Example:
```
archive         Important files\n
public          Public HTML site\n
```

**Process:** Daemon → Client

**Source:** `.upstream/clientserver.c:1374-1386`, `static void send_listing(int fd) ... }`.  Only modules with `list = true` in rsyncd.conf are included (`.upstream/clientserver.c:1380`)., `if (lp_list(i))`

**Step 4: Termination**

**Direction:** Daemon → Client

**Wire format (proto ≥ 25):**
```
@RSYNCD: EXIT\n
```

**Wire format (proto < 25):** Connection closed (no terminator, client uses EOF).

**Source:** `.upstream/clientserver.c:1385`, `io_printf(fd,"@RSYNCD: EXIT\n");` (`if (protocol_version >= 25) io_printf(fd, "@RSYNCD: EXIT\n")`), `.upstream/clientserver.c:399`, `kluge_around_eof = list_only && protocol_version < 25 ? 1 : 0;` (`kluge_around_eof = list_only && protocol_version < 25 ? 1 : 0`).

**Client behavior:** Reads lines until `@RSYNCD: EXIT` or EOF, then exits cleanly (`.upstream/clientserver.c:401-437`, `while (1) { ... kluge_around_eof = 0;`, EXIT handling at `.upstream/clientserver.c:416-421`, `if (strcmp(line,"@RSYNCD: EXIT") == 0) { ... exit(0);`).

## 6. Wire protocol step-by-step (SSH/rsh transport)

Trace for `rsync -av user@host:/path/ ./` (pull, proto 32).

### Steps that differ from daemon socket

SSH/rsh transport **skips** the daemon-socket-specific steps: greeting exchange (§5.1), module selection (§5.2), authentication (§5.3), and argument transmission (§5.4).  Instead, it has a binary version exchange.  All other steps (compat flags, checksum negotiation, seed exchange, filter list, file list, phase exchange, selector loop, final goodbye, stats) are identical to the daemon socket protocol.

### Step 1: Remote process launch

No wire data -- shell exec.  The rsync binary is invoked remotely via SSH or rsh.  Arguments are passed as command-line args to the remote rsync process (not transmitted over the wire).

### Step 2: Binary version exchange

**Wire format:**
```
Client → Server: 0x20 0x00 0x00 0x00  (protocol_version = 32, int32 LE)
Server → Client: 0x20 0x00 0x00 0x00  (remote_protocol = 32, int32 LE)
```

**Source:** `.upstream/compat.c:602-609`, `if (remote_protocol == 0) { ... protocol_version = remote_protocol;`.

**Details:**
- Only happens when `remote_protocol == 0` (initial value for SSH/rsh, `.upstream/compat.c:75`, `int remote_protocol = 0;`).
- Server writes `protocol_version` as 4-byte LE int via `write_int()` (`.upstream/compat.c:606`, `write_int(f_out, protocol_version);`).
- Client reads `remote_protocol` as 4-byte LE int via `read_int()` (`.upstream/compat.c:607`, `remote_protocol = read_int(f_in);`).
- Negotiates down to the lower version (`.upstream/compat.c:608-609`, `if (protocol_version > remote_protocol) ... protocol_version = remote_protocol;`).
- If `remote_protocol < MIN_PROTOCOL_VERSION` or `> MAX_PROTOCOL_VERSION`, error: "protocol version mismatch -- is your shell clean?" (`.upstream/compat.c:621-625`, `if (remote_protocol < MIN_PROTOCOL_VERSION ... exit_cleanup(RERR_PROTOCOL);`).
- If `remote_protocol < OLD_PROTOCOL_VERSION`, a warning is logged (`.upstream/compat.c:627-630`, `if (remote_protocol < OLD_PROTOCOL_VERSION) { ... }`).

### Step 3: Compat flags exchange (proto ≥ 30)

Same as daemon socket §5.5.  Source: `.upstream/compat.c:722-788`, `} else if (protocol_version >= 30) { ... need_messages_from_generator = 1;`.

### Step 4: Checksum/compression negotiation (proto ≥ 30)

Same as daemon socket §5.6.  Source: `.upstream/compat.c:538-574`, `static void negotiate_the_strings(int f_in, int f_out) ... }`.

### Step 5: Checksum seed exchange

Same as daemon socket §5.7.  Source: `.upstream/compat.c:822-828`, `if (am_server) { ... }`.

### Step 6: Filter list transfer

Same as daemon socket §5.9.  Source: `.upstream/main.c:1364`, `send_filter_list(f_out);` (`send_filter_list(f_out)`).

### Step 7: File list transfer

Same as daemon socket §5.10.  Source: `.upstream/main.c:983`, `flist = send_file_list(f_out,argc,argv);` (`send_file_list()` on server), `.upstream/main.c:1417`, `flist = recv_file_list(f_in, -1);` (`recv_file_list()` on client).

### Step 8: Phase exchange

Same as daemon socket §5.11.  Source: `.upstream/generator.c:2831-2929`., `if (!inc_recurse) { ... }`

### Step 9: Selector loop

Same as daemon socket §5.12.  Source: `.upstream/generator.c:588-597`., `if ((iflags & (SIGNIFICANT_ITEM_FLAGS|ITEM_REPORT_XATTR) || INFO_GTE(NAME, 2) ... write_vstring(sock_f_out, xname, strlen(xname));`

### Step 10: Final goodbye

Same as daemon socket §5.13.  Source: `.upstream/main.c:908-939`, `static void read_final_goodbye(int f_in, int f_out) ... }`, `.upstream/generator.c:2861`, `write_ndx(f_out, NDX_DONE);` (`write_ndx(f_out, NDX_DONE)`).

### Step 11: Stats exchange

Same as daemon socket §5.14.  Source: `.upstream/main.c:329-389`, `static void handle_stats(int f) ... }`.

## 7. I/O mode resolution

### 7.1 How `write_buf()`/`read_buf()` resolve to different wire formats

The upstream `iobuf` system has two modes: **multiplexed** and **buffered**.  The mode is set per-file-descriptor by calling `io_start_multiplex_*()` or `io_start_buffering_*()`.

### 7.2 Multiplexed output (`io_start_multiplex_out`)

**Source:** `.upstream/io.c:2642-2658`, `void io_start_multiplex_out(int fd) ... }`.

When multiplexed output is enabled:
1. `iobuf.out_empty_len` is set to 4, which makes `OUT_MULTIPLEXED` true.
2. `io_start_buffering_out(fd)` is called internally -- so buffered output is the underlying mechanism.
3. `iobuf.raw_data_header_pos` is set to reserve space for the first 4-byte mux header.

When `write_buf()` (`.upstream/io.c:2440`, `void write_buf(int f, const char *buf, size_t len)`) is called:
1. Bytes are accumulated in `iobuf.out` circular buffer.
2. On flush (via `perform_io()` at `.upstream/io.c:629`, `static char *perform_io(size_t needed, int flags)`), a 4-byte MSG_DATA header is prepended: `SIVAL(hdr, 0, ((MPLEX_BASE + (int)MSG_DATA)<<24) + len)`.
3. Multiple `write_buf()` calls are batched into a single MSG_DATA frame.
4. A new mux header is reserved for the next batch.

**Wire format:** `header : uint32 LE ((7 + 0) << 24 | payload_len)` + `payload : raw[payload_len]`.

### 7.3 Multiplexed input (`io_start_multiplex_in`)

**Source:** `.upstream/io.c:2661-2667`, `void io_start_multiplex_in(int fd) ... io_start_buffering_in(fd);`.

When multiplexed input is enabled:
1. `iobuf.in_multiplexed` is set to 1, which makes `IN_MULTIPLEXED` true.
2. `io_start_buffering_in(fd)` is called internally.

When `read_buf()` is called:
1. If `IN_MULTIPLEXED` and buffer is empty, `read_a_msg()` (`.upstream/io.c:1655`, `static void read_a_msg(void)`) is called.
2. `read_a_msg()` reads a 4-byte header via `raw_read_int()`, extracts msg code and length.
3. For MSG_DATA: `iobuf.raw_input_ends_before` marks where the payload ends.
4. `read_buf()` reads from the transparent byte stream, fetching more MSG_DATA frames as needed.
5. Non-DATA messages (MSG_SUCCESS, MSG_ERROR, etc) are dispatched to handlers in `read_a_msg()`.

**Wire format:** Reader transparently unwraps MSG_DATA frames.  Application code sees a raw byte stream.

### 7.4 Buffered output (`io_start_buffering_out`)

**Source:** `.upstream/io.c:1527-1544`, `BOOL io_start_buffering_out(int f_out) ... return True;`.

When buffered output is enabled:
1. `iobuf.out_fd` is set to the fd.
2. `iobuf.out_empty_len` remains 0, so `OUT_MULTIPLEXED` is false.

When `write_buf()` is called:
1. Bytes are accumulated in `iobuf.out` circular buffer.
2. On flush, bytes are written directly to the socket -- **no mux header added**.

**Wire format:** Raw bytes, no framing.

### 7.5 Buffered input (`io_start_buffering_in`)

**Source:** `.upstream/io.c:1547-1563`, `BOOL io_start_buffering_in(int f_in)`.

When buffered input is enabled:
1. `iobuf.in_fd` is set to the fd.
2. `iobuf.in_multiplexed` remains 0, so `IN_MULTIPLEXED` is false.

When `read_buf()` is called:
1. Reads directly from the socket via the buffered input path.
2. No mux unwrapping -- raw bytes.

**Wire format:** Raw bytes, no framing.

### 7.6 Key implication for `write_ndx()`

`write_ndx()` (`.upstream/io.c:2503`, `void write_ndx(int f, int32 ndx)`) uses `write_buf()` internally.  The wire format depends on the I/O mode of the target fd:

- **Buffered output:** `write_ndx()` writes compressed NDX directly as raw bytes.  NDX_DONE (-1) = 1 byte `0x00` for proto ≥ 30 (or 4 bytes `0xFFFFFFFF` if `read_batch` is set, since `write_ndx()` falls back to `write_int()` for batch mode).
- **Multiplexed output:** `write_ndx()` writes compressed NDX to the iobuf buffer, which is later flushed as a MSG_DATA frame.  NDX_DONE (-1) = 1 byte `0x00` inside a MSG_DATA frame (same `read_batch` exception applies).

The compressed NDX encoding is the same in both cases; only the framing differs.

### 7.7 Key implication for `write_int()`

`write_int()` (`.upstream/io.c:2342`, `void write_int(int f, int32 x)`) always writes 4 bytes LE.  The wire format depends on the I/O mode:

- **Buffered output:** 4 raw bytes on the wire.
- **Multiplexed output:** 4 bytes inside a MSG_DATA frame.

## 8. Protocol version I/O mode matrix

### 8.1 Client side (sender path, `client_run()`)

**Source:** `.upstream/main.c:1356-1364`, `if (protocol_version >= 30) ... send_filter_list(f_out);`.

| Direction | Proto 27 | Proto 30 | Proto 31 | Proto 32 |
|-----------|----------|----------|----------|----------|
| Output → Daemon | buffered | multiplexed | multiplexed | multiplexed |
| Input ← Daemon | buffered* | multiplexed* | multiplexed | multiplexed |

\* Input is buffered when `filesfrom_host` is set and proto < 31.  Otherwise multiplexed for proto ≥ 23.

**Source lines:** Output: `.upstream/main.c:1356-1359`, `if (protocol_version >= 30) ... io_start_buffering_out(f_out);`.  Input: `.upstream/main.c:1360-1363`, `if (protocol_version >= 31 || (!filesfrom_host && protocol_version >= 23)) ... io_start_buffering_in(f_in);`.

### 8.2 Client side (receiver path, pre-fork in `client_run()`)

**Source:** `.upstream/main.c:1398-1403`, `if (protocol_version >= 23) ... io_start_buffering_out(f_out);`.

| Direction | Proto 27 | Proto 30 | Proto 31 | Proto 32 |
|-----------|----------|----------|----------|----------|
| Output → Daemon | buffered | multiplexed | multiplexed | multiplexed |
| Input ← Daemon | multiplexed | multiplexed | multiplexed | multiplexed |

**Details:** Output is multiplexed when `need_messages_from_generator` is set, which happens for proto ≥ 30 (set unconditionally for all processes at `.upstream/compat.c:788`, `need_messages_from_generator = 1;`) or when `remove_source_files` is set (`.upstream/options.c:2360`, `if (remove_source_files) {`).  For proto < 30 without `--remove-source-files`, output is buffered.  This pre-fork output mode is used for the filter list phase (`send_filter_list(f_out)`).  After the fork in `do_recv()`, the receiver's output is always multiplexed on the internal pipe (`.upstream/main.c:1087`, `io_start_multiplex_out(f_out);`).

**Source lines:** Input: `.upstream/main.c:1398-1399`, `if (protocol_version >= 23) ... io_start_multiplex_in(f_in);` (`if (protocol_version >= 23) io_start_multiplex_in(f_in);`).  Output: `.upstream/main.c:1400-1403`, `if (need_messages_from_generator) ... io_start_buffering_out(f_out);` (`if (need_messages_from_generator) io_start_multiplex_out(f_out); else io_start_buffering_out(f_out);`).

### 8.3 Client side (generator, post-fork in `do_recv()`)

**Source:** `.upstream/main.c:1138-1139`, `io_start_buffering_out(f_out); ... io_start_multiplex_in(f_in);` (all protocol versions, no version gate).

| Direction | All protos |
|-----------|------------|
| Output → Daemon (`sock_f_out`) | buffered |
| Input ← Receiver pipe (`f_in`) | multiplexed |

**Source lines:** Output: `.upstream/main.c:1138`, `io_start_buffering_out(f_out);`.  Input: `.upstream/main.c:1139`, `io_start_multiplex_in(f_in);`.

### 8.4 Client side (receiver, post-fork in `do_recv()`)

**Source:** `.upstream/main.c:1086-1087`, `io_start_buffering_in(f_in); ... io_start_multiplex_out(f_out);` (all protocol versions, no version gate).

| Direction | All protos |
|-----------|------------|
| Input ← Daemon (`f_in`) | multiplexed (inherited from pre-fork) |
| Output → Generator pipe (`f_out`) | multiplexed |

**Source lines:** Input: inherited from `.upstream/main.c:1399`, `io_start_multiplex_in(f_in);` (`io_start_multiplex_in` in `client_run()`).  Overridden to buffered by `.upstream/main.c:1086`, `io_start_buffering_in(f_in);` if `read_batch`.  Output: `.upstream/main.c:1087`, `io_start_multiplex_out(f_out);`.

### 8.5 Server side (daemon, `start_server()`)

**Source:** `.upstream/main.c:1292-1313`, `if (protocol_version >= 23) ... io_start_buffering_in(f_in);`.

| Direction | Proto 27 | Proto 30 | Proto 31 | Proto 32 |
|-----------|----------|----------|----------|----------|
| Output → Client | multiplexed | multiplexed | multiplexed | multiplexed |
| Input ← Client (initial) | buffered | multiplexed* | multiplexed* | multiplexed* |
| Input ← Client (selectors) | buffered | buffered | buffered | buffered |

\* For `am_sender`: initial input is buffered for proto < 30, multiplexed for proto ≥ 30 (because `need_messages_from_generator` is set unconditionally at `.upstream/compat.c:788`, `need_messages_from_generator = 1;`).  However, `do_server_sender()` calls `io_start_buffering_in(f_in)` (`.upstream/main.c:991`, `io_start_buffering_in(f_in);`) which switches to buffered input before reading selectors.  So selector reading always uses buffered input.  The multiplexed input is only used for the filter list phase.

For `am_receiver` (push), `do_server_recv()` sets its own I/O mode separately (see table below).

**Source lines:** Output: `.upstream/main.c:1292-1293`, `if (protocol_version >= 23) ... io_start_multiplex_out(f_out);`.  Input: `.upstream/main.c:1310-1313`, `if (need_messages_from_generator) ... io_start_buffering_in(f_in);` (`if (need_messages_from_generator) io_start_multiplex_in(f_in); else io_start_buffering_in(f_in);`).  Selector switch: `.upstream/main.c:991`, `io_start_buffering_in(f_in);` (`io_start_buffering_in(f_in)` in `do_server_sender()`).

### 8.6 Server side (daemon recv path, `do_server_recv()`)

**Source:** `.upstream/main.c:1200-1205`, `} ... io_start_buffering_in(f_in);`.

| Direction | Proto 27 | Proto 30 | Proto 31 | Proto 32 |
|-----------|----------|----------|----------|----------|
| Input ← Client | buffered | multiplexed | multiplexed | multiplexed |

**Source lines:** `.upstream/main.c:1200-1205`, `} ... io_start_buffering_in(f_in);`.

## 9. Integer encoding formats

### 9.1 Fixed-width integers (`write_int` / `read_int`)

4 bytes, **little-endian**, signed int32.

**Source:** `.upstream/io.c:2342-2347`, `void write_int(int f, int32 x) ... }`, `.upstream/io.c:1965-1977`, `int32 read_int(int f) ... }`.

### 9.2 Variable-length integers (`varint`, protocol ≥ 30)

Compact encoding for signed int32.  Uses a lookup table (`int_byte_extra[]`) indexed by `first_byte / 4` to determine the number of extra bytes.

**Source:** `.upstream/io.c:2349-2369`, `void write_varint(int f, int32 x) ... }`, `.upstream/io.c:1986-2016`, `int32 read_varint(int f) ... }`.

**Encoding algorithm:** The value is written as little-endian in bytes 1-4 of a 5-byte buffer.  Leading zero bytes are trimmed.  The first byte encodes the length via the `int_byte_extra[]` lookup table (`.upstream/io.c:167-172`, `static const char int_byte_extra[64] = { ... };`), indexed by `first_byte / 4`:

| First byte range | Extra bytes | Total bytes |
|-----------------|-------------|-------------|
| `0x00-0x7F` | 0 | 1 |
| `0x80-0xBF` | 1 | 2 |
| `0xC0-0xDF` | 2 | 3 |
| `0xE0-0xE7` | 3 | 4 |
| `0xE8-0xEF` | 4 | 5 |
| `0xF0-0xF7` | 5 | 6 |
| `0xF8-0xFF` | 6 | 7 (overflow) |

**Reading:** `.upstream/io.c:1986-2016`, `int32 read_varint(int f) ... }`.  Reads the first byte, looks up `int_byte_extra[ch / 4]` for the number of extra bytes, reads those bytes, and reconstructs the value.  For 4+ extra bytes, the high bit of the last extra byte is used as a sign extension bit.

### 9.3 Variable-length long integers (`varlong`, protocol ≥ 30)

Similar to varint but for int64 with configurable minimum byte count.

**Source:** `.upstream/io.c:2371-2401`, `void write_varlong(int f, int64 x, uchar min_bytes) ... }`, `.upstream/io.c:2018-2057`, `int64 read_varlong(int f, uchar min_bytes) ... }`.

Common uses: file sizes (`write_varlong30(f, size, 3)`), timestamps (`write_varlong(f, time, 4)`).

### 9.4 Legacy long integers (`longint`, protocol < 30)

For values in `[0, 0x7FFFFFFF]`: `write_int(value)` -- 4 bytes LE.
For larger values (or negative): sentinel `0xFFFFFFFF` (4 bytes) followed by full 8-byte LE int64.  Total: 12 bytes.

**Source:** `.upstream/io.c:2407-2425`, `void write_longint(int f, int64 x) ... }`.

### 9.5 Short integers (`write_shortint` / `read_shortint`)

2 bytes, **little-endian**, unsigned uint16.

**Source:** `.upstream/io.c:2334-2340`, `void write_shortint(int f, unsigned short x) ... }`, `.upstream/io.c:1958-1963`, `unsigned short read_shortint(int f) ... }`.

Used for: extended xflags (when `XMIT_EXTENDED_FLAGS` is set, proto ≥ 28), item flags in selector protocol (proto ≥ 29).

### 9.6 Compressed NDX (protocol ≥ 30)

Stateful delta encoding.  Initial state: `prev_positive = -1`, `prev_negative = 1`.

**Source:** `.upstream/io.c:2503-2547`, `void write_ndx(int f, int32 ndx) ... }`, `.upstream/io.c:2550-2592`, `int32 read_ndx(int f) ... }`.

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

**For proto < 30:** `write_ndx()` falls back to `write_int()` (4-byte LE).  `read_ndx()` falls back to `read_int()`.  Source: `.upstream/io.c:2509`, `if (protocol_version < 30 || read_batch) {` (`if (protocol_version < 30 || read_batch)`), `.upstream/io.c:2557`, `if (protocol_version < 30)`.

### 9.7 vstring

**Source:** `.upstream/io.c:2482-2501`, `void write_vstring(int f, const char *str, int len)`, `.upstream/io.c:2174-2191`, `int read_vstring(int f, char *buf, int bufsize) ... }`.

Format: `length : uint8` (or 2 bytes if high bit set) + `data : raw[length]`.

If `len & 0x80`: actual length = `(len & 0x7F) * 256 + next_byte`.

## 10. Multiplexed I/O layer

### 10.1 Frame format

**Source:** `.upstream/rsync.h:210`, `#define MPLEX_BASE 7` (`MPLEX_BASE = 7`), `.upstream/io.c:758`, `((MPLEX_BASE + (int)MSG_DATA)<<24) + iobuf.out.len - 4);` (MSG_DATA header construction), `.upstream/io.c:1155`, `SIVAL(hdr, 0, ((MPLEX_BASE + (int)code)<<24) + len);` (message header construction), `.upstream/io.c:1667-1670`, `tag = raw_read_int(); ... tag = (tag >> 24) - MPLEX_BASE;` (header parsing in `read_a_msg()`).

Every frame: 4-byte header + payload.  Header is a **little-endian uint32**:
```
header = ((MPLEX_BASE + msgCode) << 24) | length
where MPLEX_BASE = 7, length in bits [0..23] (max ~16MB per frame)
```

### 10.2 Message codes (`enum msgcode`)

**Source:** `.upstream/rsync.h:293-308`, `enum msgcode { ... MSG_NO_SEND=102,/* sender failed to open a file we wanted */`.

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

### 10.3 iobuf buffering model

**Source:** `.upstream/io.c` -- `iobuf.out` (data buffer), `iobuf.msg` (message buffer), `iobuf.in` (input buffer).

Upstream uses two separate output paths with circular buffers (32KB default, `IO_BUFFER_SIZE`):

1. **`iobuf.out`** -- for `MSG_DATA`.  Application code calls `write_buf()`/`write_int()`/`write_byte()` which accumulate raw bytes.  On flush, bytes are wrapped in a `MSG_DATA` frame.  **Multiple small writes are batched into larger frames.**

2. **`iobuf.msg`** -- for non-DATA messages.  Application code calls `send_msg()` which buffers with its own header.  On flush, **any pending `iobuf.out` data is flushed first** (ensuring MSG_DATA frames precede control messages), then message frames are sent.

**Input:** `iobuf.in` -- transparent byte stream from incoming `MSG_DATA` frames.  Application code calls `read_buf()`/`read_int()`/`read_byte()` which read from this buffer.  When empty, the iobuf layer reads the next `MSG_DATA` frame and refills.  **Application never sees mux headers.**

## 11. File list wire format

### 11.1 Xmit flags encoding

**Source:** `.upstream/flist.c:644-658`, `if (xfer_flags_as_varint) ... }` (send, in `send_file_entry()`), `.upstream/flist.c:2944-2974`, `struct file_struct *file; ... file = recv_file_entry(f, flist, flags);` (receive, in `recv_file_list()`), `.upstream/flist.c:2384-2394`, `static void write_end_of_flist(int f, int send_io_error) ... }`.

The xmit flags word is the first field of every file entry.  A zero word signals end-of-list, so the sender never transmits an actual zero.

**Varint encoding (`CF_VARINT_FLIST_FLAGS`, proto ≥ 30):** the full 16-bit xflags value is sent as a varint; if xflags is zero, `XMIT_EXTENDED_FLAGS` is sent as a stand-in (`.upstream/flist.c:644-645`, `if (xfer_flags_as_varint) ... write_varint(f, xflags ? xflags : XMIT_EXTENDED_FLAGS);`).  End-of-list: varint `0` followed by a varint io_error value (`.upstream/flist.c:2386-2388`, `if (xfer_flags_as_varint) { ... write_varint(f, send_io_error ? io_error : 0);`, read at `.upstream/flist.c:2946-2951`, `if (xfer_flags_as_varint) { ... break;`).

**Protocol 28-29:**
1. If `xflags == 0` and not a directory: inject `XMIT_TOP_DIR` (`.upstream/flist.c:647-648`, `if (!xflags && !S_ISDIR(mode)) ... xflags |= XMIT_TOP_DIR;`).
2. If `(xflags & 0xFF00) || xflags == 0`: set `XMIT_EXTENDED_FLAGS` bit, `write_shortint(xflags)` (2 bytes LE) (`.upstream/flist.c:649-651`, `if ((xflags & 0xFF00) || !xflags) { ... write_shortint(f, xflags);`).
3. Otherwise: `write_byte(xflags)` (`.upstream/flist.c:652-653`, `} else ... write_byte(f, xflags);`).

**Protocol < 28:** If no flags set: add harmless flag (`XMIT_LONG_NAME` for dirs, `XMIT_TOP_DIR` for files).  Then `write_byte(xflags)` (`.upstream/flist.c:654-657`, `} else { ... write_byte(f, xflags);`).

**End-of-list (byte path):** a single zero byte (`.upstream/flist.c:2393`, `write_byte(f, 0);`), or -- when the sender had an I/O error and the peer supports safe incremental flists -- `write_shortint(XMIT_EXTENDED_FLAGS|XMIT_IO_ERROR_ENDLIST)` followed by a varint io_error value (`.upstream/flist.c:2390-2391`, `write_shortint(f, XMIT_EXTENDED_FLAGS|XMIT_IO_ERROR_ENDLIST); ... write_varint(f, io_error);`, read at `.upstream/flist.c:2960-2966`, `if (flags == (XMIT_EXTENDED_FLAGS|XMIT_IO_ERROR_ENDLIST)) { ... err = read_varint(f);`).

### 11.2 Xmit flag bits

Source: `.upstream/rsync.h:47-73`, `#define XMIT_TOP_DIR (1<<0) ... #define XMIT_CRTIME_EQ_MTIME (1<<17)	/* any protocol - restricted by command-...`.

```
XMIT_TOP_DIR              = 1 << 0
XMIT_SAME_MODE            = 1 << 1
XMIT_SAME_RDEV_pre28      = 1 << 2   (proto 20-27 only)
XMIT_EXTENDED_FLAGS       = 1 << 2   (proto 28+, replaces SAME_RDEV_pre28)
XMIT_SAME_UID             = 1 << 3
XMIT_SAME_GID             = 1 << 4
XMIT_SAME_NAME            = 1 << 5
XMIT_LONG_NAME            = 1 << 6
XMIT_SAME_TIME            = 1 << 7
XMIT_SAME_RDEV_MAJOR      = 1 << 8   (proto 28+, devices only)
XMIT_NO_CONTENT_DIR       = 1 << 8   (proto 30+, dirs only)
XMIT_HLINKED              = 1 << 9   (proto 28+)
XMIT_SAME_DEV_pre30       = 1 << 10  (proto 28-29)
XMIT_USER_NAME_FOLLOWS    = 1 << 10  (proto 30+)
XMIT_RDEV_MINOR_8_pre30   = 1 << 11  (proto 28-29)
XMIT_GROUP_NAME_FOLLOWS   = 1 << 11  (proto 30+)
XMIT_HLINK_FIRST          = 1 << 12  (proto 30+)
XMIT_IO_ERROR_ENDLIST     = 1 << 12  (proto 31+ with EXTENDED_FLAGS)
XMIT_MOD_NSEC             = 1 << 13  (proto 31+)
XMIT_SAME_ATIME           = 1 << 14  (any proto, --atimes)
XMIT_UNUSED_15            = 1 << 15
XMIT_RESERVED_16          = 1 << 16
XMIT_CRTIME_EQ_MTIME      = 1 << 17  (any proto, --crtimes)
```

### 11.3 File entry wire layout

Source: `.upstream/flist.c:475-775`, `static void send_file_entry(int f, const char *fname, struct file_struct *file, ... }`, `.upstream/flist.c:777-1392`, `static struct file_struct *recv_file_entry(int f, struct file_list *flist, in... ... }`.

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
    [device minor] : varint (proto ≥ 30) or byte (if ≤ 255, proto 28-29) or int32 (proto 28-29)
14. [if symlink] symlink_target : varint30(len) + raw[len]
15. [if hlink, proto 28-29, !XMIT_SAME_DEV_pre30] dev : longint (1-incremented)
    [if hlink, proto 28-29] ino : longint
    [if hlink, proto < 26] dev : int32 LE (1-incremented)
    [if hlink, proto < 26] ino : int32 LE
16. [if always_checksum, regular file] checksum[flist_csum_len]
```

### 11.4 End-of-list markers

| Value | Meaning | Encoding (proto ≥ 30) |
|-------|---------|----------------------|
| `NDX_DONE` (-1) | All file lists complete | Compressed NDX (`0x00`) |
| `NDX_FLIST_EOF` (-2) | End of sub-list (inc_recurse) | Compressed NDX (prefix `0xFF`) |
| Positive | Next subdirectory index | Compressed NDX |

## 12. Checksum & delta transfer protocol

### 12.1 SumHead (`write_sum_head` / `read_sum_head`)

**Source:** `.upstream/io.c:2193-2270`, `/* Populate a sum_struct with values from the socket.  This is` (`read_sum_head` at line 2195, `write_sum_head` at line 2257).

All fields are **int32 LE**:
```
count     : int32  // block count (0 = empty file)
blength   : int32  // block size
s2length  : int32  // strong hash length (only if proto ≥ 27)
remainder : int32  // final partial block size
```

For proto < 27, `s2length` is not sent and defaults to `csum_length` (MD4 = 16 bytes).

### 12.2 Block checksums

For each block `i` in `[0..count-1]`:
```
sum1[i] : raw[csum_length]  // rolling checksum (always 4 bytes)
sum2[i] : raw[s2length]     // strong hash (MD4/MD5 = 16, SHA-256 = 32, etc)
```

### 12.3 Checksum algorithms

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

**`proper_seed_order`** is set via `CF_CHKSUM_SEED_FIX` compat flag (`.upstream/compat.c:759`, `proper_seed_order = compat_flags & CF_CHKSUM_SEED_FIX ? 1 : 0;`).

## 13. Selector protocol (phase 13)

### 13.1 Selector wire format

**Source:** `.upstream/generator.c:588-597`, `if ((iflags & (SIGNIFICANT_ITEM_FLAGS|ITEM_REPORT_XATTR) || INFO_GTE(NAME, 2) ... write_vstring(sock_f_out, xname, strlen(xname));` (generator sends in `itemize()`, transfer ndx at `.upstream/generator.c:2376`, `write_ndx(f_out, ndx);`), `.upstream/sender.c:468-483`, `static void write_ndx_and_attrs(int f_out, int ndx, int iflags, ... send_xattr_request(fname, file, f_out);` (daemon echoes).

```
ndx       : compressed NDX (proto ≥ 30) or int32 LE (older)
iflags    : uint16 LE (proto ≥ 29)
[type]    : uint8 (if ITEM_BASIS_TYPE_FOLLOWS)
[xname]   : vstring (if ITEM_XNAME_FOLLOWS)
```

**On the daemon socket (generator → daemon):** Raw bytes (buffered output).
**Echoed back (daemon → receiver):** MSG_DATA frames (multiplexed output).

### 13.2 Item flags

**Source:** `.upstream/rsync.h` -- `ITEM_*` constants.

| Flag | Bit | Meaning |
|------|-----|---------|
| `ITEM_REPORT_ATIME` | 0 | Report access time changes |
| `ITEM_REPORT_CHANGE` | 1 | Report any changes |
| `ITEM_REPORT_SIZE` | 2 | Report size changes (regular files) |
| `ITEM_REPORT_TIMEFAIL` | 2 | Report time failure (symlinks) |
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

## 14. Known pitfalls

### 14.1 Generator `write_ndx()` vs receiver `write_int()` for NDX_DONE

Same semantic meaning (NDX_DONE = -1), but different wire format and different channel.

- **Generator** (`generate_files()`, `.upstream/generator.c:2861`, `write_ndx(f_out, NDX_DONE);`): `write_ndx(f_out, NDX_DONE)` → compressed NDX → 1 byte `0x00` on the daemon socket (buffered output, raw bytes).  Here `f_out` is the daemon socket fd.
- **Receiver** (`recv_files()`, `.upstream/receiver.c:860`, `write_int(f_out, NDX_DONE);`): `write_int(f_out, NDX_DONE)` → 4-byte LE int32 → 4 bytes `0xFF 0xFF 0xFF 0xFF` on the internal pipe (multiplexed output, inside MSG_DATA frame).

The generator talks to the daemon (Channel 1, buffered) and the receiver talks to the generator (Channel 3, multiplexed).  The receiver always uses `write_int()` because the generator's `wait_for_receiver()` (`.upstream/io.c:1920`, `void wait_for_receiver(void)`) reads via `read_int(iobuf.in_fd)` (`.upstream/io.c:1926`, `int ndx = read_int(iobuf.in_fd);`).

### 14.2 `read_ndx_and_attrs()` reads from one fd and echoes to another

The echo may use a different I/O mode than the read.  Source: `.upstream/rsync.c:323-434`, `int read_ndx_and_attrs(int f_in, int f_out, int *iflag_ptr, uchar *type_ptr, ... ... *len_ptr = len;`.

`read_ndx_and_attrs(f_in, f_out, ...)` reads a selector from `f_in` and the caller (eg, `write_ndx_and_attrs()` in `.upstream/sender.c:468-483`, `static void write_ndx_and_attrs(int f_out, int ndx, int iflags, ... send_xattr_request(fname, file, f_out);`) echoes it to `f_out`.  If `f_in` is buffered and `f_out` is multiplexed, the selector arrives as raw bytes but is echoed as MSG_DATA frames.

**Example:** Daemon reads selector from generator (Channel 1, buffered/raw) and echoes to client receiver (Channel 2, multiplexed/MSG_DATA).  The selector bytes are the same, but the wire framing differs.

### 14.3 Mux frame headers are transparent to application code but visible on the wire

A raw-byte reader will see mux headers as data.  When I/O mode is multiplexed, `write_buf()`/`read_buf()` transparently wrap and unwrap MSG_DATA frames.  Application code that calls `write_ndx()` or `read_ndx()` never sees the 4-byte mux header.  However, a packet capture or raw socket reader will see:
```
[4-byte header: (7 << 24) | payload_len] [payload bytes]
```

If you read the socket as raw bytes when mux is enabled, you'll get garbled data (mux headers mixed with protocol bytes).

### 14.4 `sock_f_out` vs `f_out` -- generator fd redirection

Generator redirects `f_in` to the internal pipe but `f_out` and `sock_f_out` both still point to the daemon socket.  Source: `.upstream/main.c:1135-1136`, `sock_f_in = -1; ... f_in = error_pipe[0];` (generator redirects `f_in` to `error_pipe[0]`), `.upstream/main.c:1138`, `io_start_buffering_out(f_out);` (generator sets buffered output on `f_out` which is the daemon socket at this point).

After the fork:
- Generator: `f_in` = internal pipe (from receiver), `f_out` = daemon socket, `sock_f_out` = daemon socket.
- Receiver: `f_in` = daemon socket, `f_out` = internal pipe (to generator), `sock_f_out` = -1.

The generator sends selectors to `sock_f_out` (daemon socket) via `write_ndx(sock_f_out, ndx)` in `itemize()` (`.upstream/generator.c:591-592`, `if (ndx >= 0) ... write_ndx(sock_f_out, ndx);`).  In `generate_files(f_out, local_name)`, the `f_out` parameter is the daemon socket fd (NOT the internal pipe).  The generator writes NDX_DONE via `write_ndx(f_out, NDX_DONE)` (`.upstream/generator.c:2861`, `write_ndx(f_out, NDX_DONE);`) and selector ndx via `write_ndx(f_out, ndx)` (`.upstream/generator.c:2376`, `write_ndx(f_out, ndx);`), and reads status messages, NDX_DONE, and file list data (inc_recurse) from the receiver via `wait_for_receiver()` which reads from `iobuf.in_fd` (the internal pipe).

### 14.5 `io_start_buffering_out(f_out)` in generator overrides earlier mux setup

The generator's output to the daemon socket is buffered, even though `client_run()` may have set it to multiplexed before the fork.  Source: `.upstream/main.c:1357`, `io_start_multiplex_out(f_out);` (client_run sets `io_start_multiplex_out(f_out)` for proto ≥ 30), `.upstream/main.c:1138`, `io_start_buffering_out(f_out);` (generator overrides with `io_start_buffering_out(f_out)`).

Before the fork, `client_run()` sets up multiplexed output on the daemon socket.  After the fork, the generator calls `io_start_buffering_out(f_out)` which resets the output mode to buffered.  This is intentional -- the generator sends selectors as raw bytes, not mux-wrapped.

### 14.6 `write_ndx_and_attrs()` in sender.c echoes selectors

The echo happens in a separate function call, not inside `read_ndx_and_attrs()`.  Source: `.upstream/sender.c:468-483`, `static void write_ndx_and_attrs(int f_out, int ndx, int iflags, ... send_xattr_request(fname, file, f_out);`, `.upstream/sender.c:585,640,766` (callers).

When the daemon processes a selector, it calls `read_ndx_and_attrs(f_in, f_out, ...)` to read it, then `write_ndx_and_attrs(f_out, ...)` to echo it.  The echo uses `write_ndx()` (compressed NDX) on the multiplexed output channel, so the echoed selector appears as a MSG_DATA frame on the wire.

### 14.7 Phase exchange uses different functions on client vs server

Generator uses `write_ndx()` (compressed NDX), receiver uses `write_int()` (4-byte LE).  Source: Generator: `.upstream/generator.c:2861`, `write_ndx(f_out, NDX_DONE);` (`write_ndx(f_out, NDX_DONE)`).  Receiver: `.upstream/receiver.c:860`, `write_int(f_out, NDX_DONE);` (`write_int(f_out, NDX_DONE)`).

The daemon sender also uses `write_ndx()` for the phase exchange (`.upstream/sender.c:544`, `write_ndx(f_out, NDX_DONE);`), matching the generator's format.  The receiver uses `write_int()` because the generator's `wait_for_receiver()` (`.upstream/io.c:1926`, `int ndx = read_int(iobuf.in_fd);`) expects 4-byte LE ints via `read_int(iobuf.in_fd)`.

### 14.8 Stats exchange uses `handle_stats()` with different behavior per process

`handle_stats(f)` behaves differently depending on whether `f` is -1, the process role, and whether it is a daemon.  Source: `.upstream/main.c:329-389`, `static void handle_stats(int f) ... }`.

- Generator: `handle_stats(-1)` -- does nothing (returns early at `.upstream/main.c:344` check)., `return;`
- Receiver: `handle_stats(f_in)` -- reads stats from daemon socket (`.upstream/main.c:371-378`, `total_written = read_varlong30(f, 3); ... } else if (write_batch) {`).
- Daemon sender: `handle_stats(f_out)` -- writes stats to client socket (`.upstream/main.c:353-359`, `write_varlong30(f, total_read, 3); ... }`).
- Daemon receiver: `handle_stats(f_out)` -- returns early (`.upstream/main.c:347-349` && `!am_sender`)., `if (f == -1 || !am_sender) ... }`

### 14.9 `need_messages_from_generator` is set unconditionally for proto ≥ 30, but daemon switches to buffered for selectors

For proto ≥ 30, `need_messages_from_generator` is always 1 (set at `.upstream/compat.c:788`, `need_messages_from_generator = 1;` inside the `} else if (protocol_version >= 30) {` block).  This is set for ALL processes (both client and server, sender and receiver), not just senders.  The `if (am_sender)` guard is in `start_server()` (`.upstream/main.c:1297`, `if (am_sender) {`), which only checks the flag for the sender path.  This is NOT just for `inc_recurse` -- it is set unconditionally for all proto ≥ 30 connections.

In `start_server()`, this causes the daemon to set multiplexed input (`.upstream/main.c:1311`, `io_start_multiplex_in(f_in);`).  However, `do_server_sender()` calls `io_start_buffering_in(f_in)` (`.upstream/main.c:991`, `io_start_buffering_in(f_in);`) which switches to buffered input **before** `send_files()` reads selectors.  So the actual selector reading uses buffered input, matching the generator's buffered output.

The multiplexed input is only used for the filter list phase (read by `recv_filter_list()` in `start_server()` before `do_server_sender()` is called).  After the switch to buffered input, all selector reading and the final goodbye use buffered input.

### 14.10 Daemon socket protocol (text greeting) vs SSH/rsh protocol (binary version exchange)

The `remote_protocol == 0` gate (`.upstream/compat.c:602`, `if (remote_protocol == 0) {`) determines which path is taken:
- Daemon socket: `remote_protocol` is set by the greeting parse, so the binary exchange is skipped.
- SSH/rsh: `remote_protocol` starts at 0, so the binary `write_int`/`read_int` exchange happens.

## 15. Quick Reference & Common Pitfalls

1. **When the real rsync client (proto 32) connects to our server via daemon socket, what I/O mode does the generator use to send selectors?** Buffered (raw bytes).  Source: `.upstream/main.c:1138`, `io_start_buffering_out(f_out);` (`io_start_buffering_out(f_out)`).  The generator calls `write_ndx(sock_f_out, ndx)` in `itemize()` (`.upstream/generator.c:591-592`, `if (ndx >= 0) ... write_ndx(sock_f_out, ndx);`) which writes compressed NDX as raw bytes.

2. **What I/O mode does the daemon use to send file data to the receiver?** Multiplexed (MSG_DATA frames).  Source: `.upstream/main.c:1293`, `io_start_multiplex_out(f_out);` (`io_start_multiplex_out(f_out)` for proto ≥ 23).

3. **Does the receiver read from the daemon socket using mux or buffered input?** Multiplexed for proto ≥ 23.  Source: `.upstream/main.c:1398-1399`, `if (protocol_version >= 23) ... io_start_multiplex_in(f_in);` (`if (protocol_version >= 23) io_start_multiplex_in(f_in);` in `client_run()` receiver path).

4. **What wire format does NDX_DONE have on the daemon socket vs the internal pipe?** 1 byte `0x00` on socket for proto ≥ 30 (compressed NDX, buffered), 4 bytes `0xFFFFFFFF` on pipe (int32 LE of -1, multiplexed).  Source: Generator: `.upstream/io.c:2503-2547`, `void write_ndx(int f, int32 ndx) ... }` (`write_ndx()`; NDX_DONE branch at `.upstream/io.c:2519-2522`, `} else if (ndx == NDX_DONE) { ... return;`, proto < 30 / batch fallback at `.upstream/io.c:2509-2512`, `if (protocol_version < 30 || read_batch) { ... }`).  Receiver: `.upstream/receiver.c:860`, `write_int(f_out, NDX_DONE);` (`write_int(f_out, NDX_DONE)`).

5. **What I/O mode does the daemon use to read selectors from the generator on proto 32?** Buffered (raw bytes).  Source: `.upstream/main.c:991`, `io_start_buffering_in(f_in);` (`io_start_buffering_in(f_in)` in `do_server_sender()`).  Although `start_server()` sets multiplexed input for proto ≥ 30 (because `need_messages_from_generator` is always 1 at `.upstream/compat.c:788`, `need_messages_from_generator = 1;`, set for all processes), `do_server_sender()` switches to buffered input before reading selectors.  The multiplexed input is only used for the filter list phase.

6. **How does the SSH/rsh version exchange differ from the daemon socket greeting?** Binary `write_int`/`read_int` when `remote_protocol == 0` (`.upstream/compat.c:602-609`, `if (remote_protocol == 0) { ... protocol_version = remote_protocol;`), vs text `@RSYNCD:` parse (`.upstream/clientserver.c:209`, `if (sscanf(buf, "@RSYNCD: %d.%d", &remote_protocol, &remote_sub) < 1) {`).

7. **What changed at protocol version 23?** Multiplexed I/O layer introduced.  Source: `.upstream/main.c:1292`, `if (protocol_version >= 23)`.

8. **What changed at protocol version 30?** Compressed NDX (`.upstream/io.c:2509`, `if (protocol_version < 30 || read_batch) {`), varint xmit flags (`.upstream/flist.c:644`, `if (xfer_flags_as_varint)`), compat flags exchange (`.upstream/compat.c:722`, `} else if (protocol_version >= 30) {`), subprotocol version (`.upstream/compat.c:853`, `io_printf(f_out, "@RSYNCD: %d.%d %s\n", protocol_version, our_sub, tmpbuf);`), null-terminated args (`.upstream/clientserver.c:257`, `if (protocol_version >= 30)`), `need_messages_from_generator` always 1 (`.upstream/compat.c:788`, `need_messages_from_generator = 1;`), MD5 default checksums (`.upstream/compat.c:415`, `env_str = ntype == NSTR_COMPRESS ? "zlib" : protocol_version >= 30 ? "md5" : ...`), ACL/xattr support (`.upstream/compat.c:664`, `if (protocol_version < 30) {`).
