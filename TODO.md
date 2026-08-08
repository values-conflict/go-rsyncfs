<!-- remove from file when complete; keep a double space between TODO entries so they're more readable / digestible -->
<!-- sub-bullets (2-space indent, `-`, cuddled -- no blank line between parent and sub-bullets, nor between sibling sub-bullets) are for related side notes subordinate to the main item but distinct enough to stand alone -- use a semicolon continuation for the same thought, a sub-bullet for a related angle, and a new top-level entry for a separate concern -->

- upstream rsync interop: real `rsync` client gets "unexpected tag -7 [Receiver]" when connecting to our server
  - handshake works fine (greeting, module selection, auth, arguments, compat flags all exchange correctly)
  - failure happens when rsync client tries to read the file list as a mux frame after compat flags
  - our client ↔ our server works perfectly (cross_test.go, integration ClientSelfTest)
  - debugging showed server sends: compat flags varint (e.g. `80 80` for 0x80), then mux frame header `28 00 00 07` (MSG_DATA, 40 bytes), then file list payload
  - possible angles to investigate:
    - does upstream use buffered I/O that reads ahead past the compat flags varint into the mux frame?
    - does `io_flush(FULL_FLUSH)` in `io_start_multiplex_out()` matter for separating the raw compat flags from the mux frame?
    - are there additional arguments or protocol steps we're missing (e.g., `send_protected_args`)?
    - check if the file list wire format matches byte-for-byte with upstream (xflags, name encoding, etc.)
  - use `strace` or a TCP proxy to capture exact bytes from real rsync client for comparison

- what's the shape of upstream's test suite?  could it be adapated or ported so that we can run our implementations against it directly?

- verify all comments match the appropriate/correct format

- should we summarize the relevant tianonfmt rules here somewhere so they're easier/quicker to reference?  maybe make the upstream tianonfmt docs themselves tighter somehow?

- create explicit `Example` functions that demonstrate how to create a TCP-based rsync `Server` and/or `Client`

- can we somehow get creative with the `net.Pipe` usage / tests to avoid the goroutines entirely?
