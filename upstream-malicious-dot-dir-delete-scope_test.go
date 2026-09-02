package rsyncfs

// Skipped port: upstream testsuite test malicious-dot-dir-delete-scope_test.py (upstream rsync @ 471e17dc, "Preparing for release of 3.5.0").
//
// The upstream test: a malicious pull server encodes the synthetic "." entry as a top-level content directory, which makes an --delete generator run delete_in_dir(".") and sweep receiver-owned siblings outside the requested scope.  The defense lives in the receiver's delete scoping (delete.c / generator.c).
//
// Why it does not port: this library has no --delete handling at all.  The Server is read-only (it sends file lists and answers delta requests; nothing in the transfer phase deletes local state), and the Client is an io/fs.FS with no delete operations -- the attack's precondition (a generator that runs delete_in_dir on the transfer root) has no code path to protect.  When write/delete support arrives, the delete scope rules need porting, and this is the test that should come with them.
