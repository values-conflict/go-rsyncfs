<!-- remove from file when complete; keep a double space between TODO entries so they're more readable / digestible -->
<!-- sub-bullets (2-space indent, `-`, cuddled -- no blank line between parent and sub-bullets, nor between sibling sub-bullets) are for related side notes subordinate to the main item but distinct enough to stand alone -- use a semicolon continuation for the same thought, a sub-bullet for a related angle, and a new top-level entry for a separate concern -->

- verify all comments match the appropriate/correct format

- should we summarize the relevant tianonfmt rules here somewhere so they're easier/quicker to reference?  maybe make the upstream tianonfmt docs themselves tighter somehow?

- can we somehow get creative with the `net.Pipe` usage / tests to avoid the goroutines entirely?

- when we're not using mux (old protocol, etc) do we still have a buffer like upstream does?

- are any of our tests slow enough to be worth the overhead that `t.Parallel()` adds?  are they safe/ready for that?
