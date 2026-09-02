package rsyncfs

// Skipped port: upstream testsuite test malicious-dot-file-delete-scope_test.py (upstream rsync @ 471e17dc, "Preparing for release of 3.5.0").
//
// The upstream test: the same delete-scope attack as malicious-dot-dir-delete-scope, but with the synthetic "." entry encoded as a file rather than a directory.  The defense is identical in spirit: the receiver's --delete scoping must not let a synthetic "." entry enlarge the delete set.
//
// Why it does not port: for the same reason as its directory sibling -- this library has no --delete handling (read-only Server, no delete operations on the Client), so there is no delete scope to confine.  Port it together with the delete support, as a pair with upstream-malicious-dot-dir-delete-scope_test.go.
