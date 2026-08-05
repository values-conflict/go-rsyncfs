- what's the shape of upstream's test suite?  could it be adapated or ported so that we can run our implementations against it directly?

- verify implementation of Task 9 is completely correct

- verify that all code / tasks have appropriate tests

- close the "Known Gaps" in our implementation, remove them from the plan, and expressly forbid leaving "known gaps" in the future

- verify all comments match the appropriate/correct format

- should we summarize the relevant tianonfmt rules here somewhere so they're easier/quicker to reference?  maybe make the upstream tianonfmt docs themselves tighter somehow?

- check whether any tests ended up using actual TCP when they could be using `net.Pipe`
