<!-- remove from file when complete; keep a double space between TODO entries so they're more readable / digestible -->
<!-- sub-bullets (2-space indent, `-`, cuddled -- no blank line between parent and sub-bullets, nor between sibling sub-bullets) are for related side notes subordinate to the main item but distinct enough to stand alone -- use a semicolon continuation for the same thought, a sub-bullet for a related angle, and a new top-level entry for a separate concern -->

- upstream rsync interop: real `rsync` client gets "connection unexpectedly closed (61 bytes)" when connecting to our server
  - resolved issues (internal client ↔ server tests pass):
    - `extractClientInfo` was looking for standalone `-e` argument, but upstream embeds `e` flags in combined short-options
    - checksum negotiation vstrings were null-separated but upstream uses space-separated
    - added checksum negotiation step (vstring exchange when CF_VARINT_FLIST_FLAGS is set)
    - added checksum seed exchange (4-byte LE after checksum negotiation)
    - file list xflags: first entry must always send uid/gid (no XMIT_SAME_UID/GID for first entry)
    - file list xflags: avoid zero xflags (signals end-of-list) by setting XMIT_TOP_DIR when otherwise zero
  - current state: handshake through file list works, client receives file list, but connection closes before phase exchange completes
  - remaining issues (for full upstream interop):
    - server returns after selector loop (NDX_DONE), but upstream expects phase-based NDX_DONE exchange
    - client's send_files() has multiple phases (maxPhase=2 for proto >= 29) with NDX_DONE round-trips
    - server needs to send NDX_DONE after file list, read client's NDX_DONE, repeat for each phase
    - final goodbye protocol (read_final_goodbye) for proto >= 24/31
    - server needs to handle file selectors and send file data (currently works for internal client but not upstream)

- what's the shape of upstream's test suite?  could it be adapated or ported so that we can run our implementations against it directly?

- verify all comments match the appropriate/correct format

- should we summarize the relevant tianonfmt rules here somewhere so they're easier/quicker to reference?  maybe make the upstream tianonfmt docs themselves tighter somehow?

- create explicit `Example` functions that demonstrate how to create a TCP-based rsync `Server` and/or `Client`

- can we somehow get creative with the `net.Pipe` usage / tests to avoid the goroutines entirely?
