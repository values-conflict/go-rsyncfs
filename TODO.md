<!-- remove from file when complete; keep a double space between TODO entries so they're more readable / digestible -->
<!-- sub-bullets (2-space indent, `-`, cuddled -- no blank line between parent and sub-bullets, nor between sibling sub-bullets) are for related side notes subordinate to the main item but distinct enough to stand alone -- use a semicolon continuation for the same thought, a sub-bullet for a related angle, and a new top-level entry for a separate concern -->

- upstream rsync interop: real `rsync` client gets "File-list index 188" error when connecting to our server
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
  - current state: handshake through file list works, phase exchange works, but file transfer fails with "File-list index 188" error
  - remaining issues (for full upstream interop):
    - file list wire format may have encoding differences (client reports "File-list index 188 not in -1 - 1")
    - file transfer protocol: server sends sum_head/file data, but client's receiver rejects with protocol error
    - may need to match upstream's exact wire format for file list entries (mode, uid, gid encoding)

- what's the shape of upstream's test suite?  could it be adapated or ported so that we can run our implementations against it directly?

- verify all comments match the appropriate/correct format

- should we summarize the relevant tianonfmt rules here somewhere so they're easier/quicker to reference?  maybe make the upstream tianonfmt docs themselves tighter somehow?

- create explicit `Example` functions that demonstrate how to create a TCP-based rsync `Server` and/or `Client`

- can we somehow get creative with the `net.Pipe` usage / tests to avoid the goroutines entirely?
