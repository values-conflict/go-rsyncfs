<!-- remove from file when complete; keep a double space between TODO entries so they're more readable / digestible -->
<!-- sub-bullets (2-space indent, `-`, cuddled -- no blank line between parent and sub-bullets, nor between sibling sub-bullets) are for related side notes subordinate to the main item but distinct enough to stand alone -- use a semicolon continuation for the same thought, a sub-bullet for a related angle, and a new top-level entry for a separate concern -->

- upstream rsync interop: real `rsync` client fails with "connection unexpectedly closed" when connecting to our server
  - resolved issues:
    - `extractClientInfo` was looking for standalone `-e` argument, but upstream embeds `e` flags in combined short-options
    - checksum negotiation vstrings were null-separated but upstream uses space-separated
    - added checksum negotiation step (vstring exchange when CF_VARINT_FLIST_FLAGS is set)
    - added checksum seed exchange (4-byte LE after checksum negotiation)
    - file list xflags: first entry must always send uid/gid (no XMIT_SAME_UID/GID for first entry)
    - file list xflags: avoid zero xflags (signals end-of-list) by setting XMIT_TOP_DIR when otherwise zero
    - added phase exchange (NDX_DONE round-trips) after file list
    - added streaming reads from batched mux frames (upstream batches multiple writes into single frame)
    - added final goodbye protocol (read_final_goodbye) for proto >= 24/31
    - server handles directory selectors (skips non-ITEM_TRANSFER and directories)
  - current state (2025-08-09 investigation):
    - handshake through file list works, phase exchange works
    - client sends selectors (confirmed via hex trace)
    - file transfer fails: server reads wrong Generator sum_head due to mux frame batching
  - root cause analysis:
    - upstream rsync uses a multi-process architecture (Receiver + Generator connected via pipes)
    - the Generator writes selectors AND sum_heads to the socket (via iobuf mux layer)
    - the iobuf layer batches multiple writes into single mux frames
    - when batching occurs, a selector for file N may be batched with sum_head for file N+1
    - our server reads the batched frame, parses the selector, then reads the "leftover" bytes as the Generator's sum_head
    - but those leftover bytes are the sum_head for a DIFFERENT file, causing protocol desync
    - additionally, some selectors arrive without ITEM_TRANSFER flag (e.g., iflags=0x0018 for report-only)
    - the server correctly skips non-TRANSFER selectors but then blocks waiting for more selectors that never arrive
  - remaining issues (for full upstream interop):
    - must handle Generator sum_head reading correctly when selector + sum_head are batched in one mux frame
    - the Generator's sum_head must be read from a SEPARATE mux frame, not from leftover selector bytes
    - need to understand upstream's iobuf batching boundaries to know when selector ends and sum_head begins
    - checksum2 (block checksums) must include the seed: MD5(seed + data) when CF_CHKSUM_SEED_FIX is set
    - final file checksum must NOT include the seed (plain MD5(data))
    - may need to match upstream's exact wire format for file list entries (mode, uid, gid encoding)

- what's the shape of upstream's test suite?  could it be adapated or ported so that we can run our implementations against it directly?

- verify all comments match the appropriate/correct format

- should we summarize the relevant tianonfmt rules here somewhere so they're easier/quicker to reference?  maybe make the upstream tianonfmt docs themselves tighter somehow?

- create explicit `Example` functions that demonstrate how to create a TCP-based rsync `Server` and/or `Client`

- can we somehow get creative with the `net.Pipe` usage / tests to avoid the goroutines entirely?
