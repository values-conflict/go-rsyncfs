package rsyncfs

// Skipped port: upstream testsuite test proto-subflist-freed_test.py (upstream rsync @ 471e17dc, "Preparing for release of 3.5.0").
//
// The upstream test: in inc_recurse mode the receiver frees each flist as it finishes; when the last goes, first_flist is set to NULL and the shared file pool is destroyed, but the global dir_flist still points into it.  A sub-flist marker sent at that point used to write FLAG_GOT_DIR_FLIST into the freed entry (a use-after-free).  The fix refuses the marker up front when first_flist is NULL ("refusing sub-flist after final flist was freed").
//
// Why it does not port: like proto-cleared-dirflist, the entire scenario is inc_recurse sub-flist lifecycle -- markers, per-flist free points, a pooled dir_flist that outlives its pool.  This library reads a single flat file list and has no sub-flist markers and no freed-pool state, so the "after the final flist is freed" window the test targets does not exist.  Use-after-free itself is a C memory-model failure with no Go counterpart; the portable residue (a phase rule about when sub-flist markers are legal) belongs to the inc_recurse work, not to this flat-list reader.
