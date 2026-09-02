package rsyncfs

// Skipped port: upstream testsuite test readonly-partial-abort-mode-regression_test.py (upstream rsync @ 471e17dc, "Preparing for release of 3.5.0").
//
// The upstream test: a proposed-3.5 receiver retries an EACCES output open by chmod'ing an existing output or in-place partial file to 0600; this test's fake remote sender closes after the receiver has requested the file and before sending any file-data token, and the assertion is that the aborted transfer fails without relaxing the file's existing 0444 mode.
//
// Why it does not port: the behavior under test is receiver-side in-place update with a partial-file cache (the CF_INPLACE_PARTIAL_DIR feature plus the EACCES-retry open path) -- neither of which exists in this library.  The Client only reads files over the wire, the Server only sends them, and there is no chmod-on-retry logic to regress.  Port it when the writable-filesystem / partial-transfer phases land.
