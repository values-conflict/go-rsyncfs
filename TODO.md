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
    - replaced explicit-frame mux API with transparent buffered I/O matching upstream's iobuf model
    - fixed 4-byte compressed NDX form reading (was reading 4 bytes instead of 5, corrupting iflags)
    - fixed file size calculation for files where remainder == 0 (exactly N blocks)
    - fixed NDX_DONE handling (no iflags follow NDX_DONE, unlike regular selectors)
  - remaining:
    - checksum2 (block checksums) must include the seed: MD5(seed + data) when CF_CHKSUM_SEED_FIX is set
    - final file checksum must NOT include the seed (plain MD5(data))
    - may need to match upstream's exact wire format for file list entries (mode, uid, gid encoding)

- what's the shape of upstream's test suite?  could it be adapated or ported so that we can run our implementations against it directly?

- verify all comments match the appropriate/correct format

- should we summarize the relevant tianonfmt rules here somewhere so they're easier/quicker to reference?  maybe make the upstream tianonfmt docs themselves tighter somehow?

- create explicit `Example` functions that demonstrate how to create a TCP-based rsync `Server` and/or `Client`

- can we somehow get creative with the `net.Pipe` usage / tests to avoid the goroutines entirely?

- cross tests only cover proto 30/31/32; need to verify all supported versions (20–32) work end-to-end
  - proto < 30: NDX is plain int32 LE, not compressed
  - proto < 29: no item flags in selectors
  - proto < 27: sum_head lacks s2length field (12 bytes instead of 16)
  - phase exchange NDX_DONE format differs across versions
  - checksum seed read was gated behind varintFlistFlags but server always sends it

- rewrite `plan.md` as if the transparent buffering `mux` reader/writer was the plan all along (Task 13 shouldn't exist, or shouldn't happen so late)

- our `protocol/mux` implementation doesn't actually bound the size of the buffer (and flush when it's full) - it will fill up forever, which may or may not cause issues at some point?  maybe it's fine but I don't think so - I think we need to probably flush more often so that the remote end knows what our progress is (and because TCP packets are not unbounded in size and our files might get big enough to trigger something like `bytes.ErrTooLarge` on our `bytes.Buffer`)

- if `.upstream/rsync` exists, `integration_test.go` should prefer it (maybe even before consulting `PATH`? we should explore pros/cons of this)
