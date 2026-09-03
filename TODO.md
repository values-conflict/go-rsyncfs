<!-- remove from file when complete; keep a double space between TODO entries so they're more readable / digestible -->
<!-- sub-bullets (2-space indent, `-`, cuddled -- no blank line between parent and sub-bullets, nor between sibling sub-bullets) are for related side notes subordinate to the main item but distinct enough to stand alone -- use a semicolon continuation for the same thought, a sub-bullet for a related angle, and a new top-level entry for a separate concern -->

- verify all comments match the appropriate/correct format

- should we summarize the relevant tianonfmt rules here somewhere so they're easier/quicker to reference?  maybe make the upstream tianonfmt docs themselves tighter somehow?

- can we somehow get creative with the `net.Pipe` usage / tests to avoid the goroutines entirely?

- are any of our tests slow enough to be worth the overhead that `t.Parallel()` adds?  are they safe/ready for that?

- deal with protocol-level logging somehow -- maybe a "logger" object that gets passed around so that protocol-level logs can go there instead of being dropped?  maybe just a callback?
  - `MSG_ERROR_XFER` is the one log code that means something beyond a log line: upstream's `rwrite()` sets `got_xfer_error`, which drives the final exit status (exit 23), and the daemon forwards its own transfer errors to the client as this code -- so a sink (or `Session` remembering the last one) could turn a bare EOF / checksum-mismatch error into the daemon's actual diagnostic
