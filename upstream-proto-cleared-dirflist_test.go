package rsyncfs

// Skipped port: upstream testsuite test proto-cleared-dirflist_test.py (upstream rsync @ 471e17dc, "Preparing for release of 3.5.0").
//
// The upstream test: under inc_recurse, the receiver appends every received directory entry to dir_flist before flist_sort_and_clean() runs; two top-level directories with the SAME name both land there, the dedup pass clear_file()s one, and a sub-flist marker tagged with the cleared dir index used to pass the used-bounds check and dereference the cleared entry's NULL f_name() in the dirname strcmp.  The fix refuses an inactive (!F_IS_ACTIVE) dir slot up front ("refusing flist for cleared dir_ndx N").
//
// Why it does not port: the whole attack rides the inc_recurse sub-flist machinery -- NDX_FLIST_OFFSET markers, per-directory sub-flists, a receiver-maintained dir_flist -- none of which exists in this library (the FlistReader reads a single flat file list, and the client never negotiates CF_INC_RECURSE).  The "cleared slot" the test targets is also a C artifact: clear_file() zeroes a file_struct in place and leaves the slot indexable, a lifecycle that Go's slice-of-structs reader has no counterpart for.  There is no dir slot to clear and no sub-flist marker to validate, so there is no guard to pin.
