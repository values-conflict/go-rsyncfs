package rsyncfs

// Shared helpers for the upstream-<name>_test.go files (ports of upstream
// testsuite tests; see each file's header for its source and oracle).
//
// The naming convention: `sed 's/^upstream-//;s/_test\.go$//' upstream-foo_test.go` round-trips to the upstream test name `foo`.  Tests that upstream has but we deliberately do not port exist as stub files with the same name and a comment explaining the skip.

import (
	"testing"
	"time"
)

// upstreamTestTimeout is the watchdog for the goroutine-driven port tests: any upstream test that expects the daemon to refuse/crash expects the connection to end, so a live goroutine after this deadline is a hang, not a slow test.
func upstreamTestTimeout(t *testing.T) <-chan time.Time {
	t.Helper()
	return time.After(10 * time.Second)
}
