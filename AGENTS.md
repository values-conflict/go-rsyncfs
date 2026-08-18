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

### `trash/` -- superseded implementation code

The `trash/` folder contains previously written code that was deleted because it didn't match the api-design.md spec (wrong package, unexported when it should be exported, duplicated logic, etc).  The code is not compiled (`go build ./...` will fail due to package name conflicts in `trash/` -- use `go build ./. ./protocol/...` or specific package paths instead).

When implementing tasks, read the relevant trash files for protocol knowledge (wire format byte sequences, edge cases, test expectations) but write fresh code that matches api-design.md.  Do not copy-paste from trash.

| Trash file | Plan task(s) | What to look for |
|---|---|---|
| `trash/wireint.go` | Task 3 | Varint/varlong/longint encoding logic; missing: WriteInt32, ReadInt32, WriteUint16, ReadUint16, NdxState |
| `trash/wireint_test.go` | Task 3 | Round-trip test values and edge cases |
| `trash/server.go` | Task 12 | NewServer construction, module map (missing: Greeting field on Server, AuthCallback on ServerModule) |
| `trash/server_test.go` | Task 12 | Module creation, duplicate rejection, error formatting |
| `trash/server-handshake.go` | Task 13 | Full handshake flow, greeting exchange, auth, args reading, compat flags, checksum negotiation, vstring helpers, readLine, readDelimitedArgs, extractClientInfo, resolveCompatFlags, negotiateChecksum |
| `trash/server-handshake_test.go` | Task 13 | Module list, unknown module, auth success/failure, client disconnect, mockRW pattern |
| `trash/server-flist.go` | Task 14 | File list wire encoding, xmit flags, delta encoding, walkFS, writeXflags, writeMode, writeID, writeSize, writeMtime, writeSymlinkTarget, writeEndMarker, writeNdxDone, commonPrefixLen |
| `trash/server-flist_test.go` | Task 14 | File list payload structure, delta encoding, name prefix reuse, protocol version differences, xflags fallback for proto < 28 |
| `trash/server-transfer.go` | Task 14 | checksum1 (rolling), checksum2 (strong), computeSumHead, sendFile, writeSumHead, sendBlockChecksums, readDeltaStream, parseDeltaStream, sendFileChecksum, selector/ndxState with compressed NDX read |
| `trash/server-transfer_test.go` | Task 14 | Checksum test vectors, sumHead math, delta stream parsing, full sendFile round-trip via net.Pipe |
| `trash/client.go` | Tasks 15 | Client struct, Session struct (with fields), Connect handshake, OpenRoot, sendArguments, readModuleList, doListRequest, rootDir/rootFileInfo types, vstring read/write |
| `trash/client_test.go` | Task 15 | startServer helper, Connect success/version/auth, OpenRoot module listing, PasswordAuth, computeAuthHash |
| `trash/client-open.go` | Task 16 | flistReader, fileListEntry, readFileList, writeNdx (compressed), phaseExchange, writeSelector, openModule, openFile, openRootMode, moduleFile, moduleDirFile, symlinkFile, fileInfo, findEntry, filterChildren |
| `trash/client-open_test.go` | Task 16 | File open, directory listing, subdirectory, symlinks, empty files, large files, root mode, flistReader parsing, writeNdx compressed encoding, writeSelector item flags |
| `trash/cross_test.go` | Task 18 | Full directory tree, subdirectory listing, symlinks, large files, fstest.TestFS, version negotiation, root mode, file content integrity, multi-file single connection |
| `trash/integration_test.go` | Task 19 | Real rsync daemon integration, rsync client pulling from our server, process management, Unix socket preference |
