Project goals can be found in `goals.md`
API design (exported surface) is in `api-design.md`
Implementation plan (tracked tasks) is in `plan.md`
The plan should be *grounded* by re-reading the goals and api-design.md before every task / session

This repo follows [tianonfmt](https://github.com/values-conflict/tianonfmt) in Cute/Lenient mode throughout -- read the tianonfmt docs before writing any file.

Project-specific Go coding standards are in `coding-standards-go.md`

Upstream (pinned to the most recent commit checked for upstream compatibility) is in a submodule at `.upstream` (suitable for `git -C .upstream grep ...`, etc) -- inside `.upstream/old_versions` there are a bunch of upstream-maintained static builds of rsync versions targeting different protocol versions (see `.upstream/old_versions/README.md` for which binary prefers which protocol version)

There is a definitive protocol reference in `protocol.md`, verified against the upstream source tree -- when ambiguous or too light on details, fall back on reading the upstream sources, and proactively update `protocol.md` so it remains a complete and functional reference

`protocol.md` should always read as a current, up-to-date reference for the upstream protocol -- no implementation status notes, no "current scope", no "TODO" callouts.  It describes the protocol, not our progress.

`plan.md` should always read as a current, up-to-date plan -- written as if the plan had always been this way.  The only indicator of progress is crossed-off task/phase titles.  Avoid language like "Implementation note", "changed from plan", "replaced by", "added", "rearchitecture", "Current status", or "Currently" that reads as a retroactive correction.

For now, we don't care about backwards compatibility for consumers of our API/library -- if an API, description, documentation, etc needs to change, it should just change.  We care about upstream compatibility/correctness above all.
