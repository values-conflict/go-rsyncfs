<!-- remove from file when complete; keep a double space between TODO entries so they're more readable / digestible -->
<!-- sub-bullets (2-space indent, `-`, cuddled -- no blank line between parent and sub-bullets, nor between sibling sub-bullets) are for related side notes subordinate to the main item but distinct enough to stand alone -- use a semicolon continuation for the same thought, a sub-bullet for a related angle, and a new top-level entry for a separate concern -->

- we've updated plan.md with details from our new api-design.md rewrite - for every item in the plan that we claim is done, let's verify that it really *is* done -- let's also delete any code that doesn't serve the updated plan

- what's the shape of upstream's test suite?  could it be adapated or ported so that we can run our implementations against it directly?

- verify all comments match the appropriate/correct format

- should we summarize the relevant tianonfmt rules here somewhere so they're easier/quicker to reference?  maybe make the upstream tianonfmt docs themselves tighter somehow?

- update the plan to create explicit `ExampleXXX` Go testing functions that demonstrate how to create a TCP-based rsync `Server` and/or `Client`

- can we somehow get creative with the `net.Pipe` usage / tests to avoid the goroutines entirely?

- our `protocol/mux` implementation doesn't actually bound the size of the buffer (and flush when it's full) - it will fill up forever, which may or may not cause issues at some point?  maybe it's fine but I don't think so - I think we need to probably flush more often so that the remote end knows what our progress is (and because TCP packets are not unbounded in size and our files might get big enough to trigger something like `bytes.ErrTooLarge` on our `bytes.Buffer`)

- if `.upstream/rsync` exists, `integration_test.go` should prefer it (maybe even before consulting `PATH`? we should explore pros/cons of this)
