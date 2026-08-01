Project goals can be found in `goals.md`
Implementation plan (tracked tasks) is in `plan.md`
The plan should be *grounded* by re-reading the goals before every task / session

This repo follows [tianonfmt](https://github.com/values-conflict/tianonfmt) in Cute/Lenient mode throughout -- read the tianonfmt docs before writing any file.

Project-specific Go coding standards are in `coding-standards-go.md`

Upstream (pinned to the most recent commit checked for upstream compatibility) is in a submodule at `.upstream` (suitable for `git -C .upstream grep ...`, etc)

There is a rough summary of the upstream protocol in `protocol.md`, focused on details useful to implementing our tasks -- when ambiguous or too light on details, fall back on reading the upstream sources, and proactively update `protocol.md` so it remains a complete and functional reference
