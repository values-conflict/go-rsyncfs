- what's the shape of upstream's test suite?  could it be adapated or ported so that we can run our implementations against it directly?

- verify implementation of Task 9 is completely correct

- verify that all code / tasks have appropriate tests
  - for example, `PasswordAuth` does not
  - a task is not complete until all the code has meaningful tests

- verify all comments match the appropriate/correct format

- should we summarize the relevant tianonfmt rules here somewhere so they're easier/quicker to reference?  maybe make the upstream tianonfmt docs themselves tighter somehow?

- create explicit `Example` functions that demonstrate how to create a TCP-based rsync `Server` and/or `Client`

- can we somehow get creative with the `net.Pipe` usage / tests to avoid the goroutines entirely?
