package rsyncfs

// Skipped port: upstream testsuite test proto-cleared-ndx_test.py (upstream rsync @ 471e17dc, "Preparing for release of 3.5.0").
//
// The upstream test: a peer can send duplicate file-list entries; the receiver's flist_sort_and_clean() clear_file()s the second slot (name and mode zeroed) but leaves it indexable.  A transfer-phase ndx for that cleared slot used to resolve to the dead file_struct and NULL-deref in the name path.  The fix refuses the inactive entry at ndx resolution ("refusing transfer of cleared file index N").
//
// Why it does not port: the "cleared slot" is a C lifecycle artifact -- clear_file() zeroes a pooled file_struct in place while the slot stays addressable, and the guard is an F_IS_ACTIVE check on that pool.  Go's reader builds fresh struct values into a slice with no inactive state: duplicate entries simply parse as two ordinary entries, and there is no cleared slot for a transfer ndx to target.  The attack's precondition (an active/inactive distinction on flist slots) does not exist here, so there is no refusal to pin.
