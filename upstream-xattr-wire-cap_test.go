package rsyncfs

// Skipped port: upstream testsuite test xattr-wire-cap_test.py (upstream rsync @ 471e17dc, "Preparing for release of 3.5.0").
//
// The upstream test: a malicious daemon (acting as the sender) puts an oversized per-value xattr datum_len in the file list it streams to a pulling client; the receiver's receive_xattr() (xattrs.c) must bound-check the length before reading the value ("xattr datum_len exceeds per-value limit").
//
// Why it does not port: the xattr wire format is not part of this library's protocol surface.  Neither protocol.FlistEntry nor the FlistReader/FlistWriter pair carries xattr blocks (upstream send_xattr()/receive_xattr() ride on the same per-entry stream under -X, and neither our client requests nor our server negotiates them -- xattr support is an explicit post-v1 phase in goals.md).  There is no datum length to bound-check, so there is no guard to pin; the moment the xattr wire format lands, this test should be ported as a FlistReader bounds-check test alongside the other upstream-... ports.
