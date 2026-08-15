#!/usr/bin/env -S gawk -f

# usage: ./upstreams.gawk protocol.md
#
# Reference format (enforced by this script):
#   (prose, `.upstream/file.c:start[-end]`, `snippet`)
#
# - prose is optional: (`.upstream/file.c:line`, `snippet`) is valid
# - snippet is a backtick-quoted string that must appear on the cited line(s)
# - for ranges (start-end), snippet uses "..." to separate start/end excerpts:
#     `if (proto >= 30) ... io_start_buffering_in(f_in);`
# - the script verifies that the snippet text is present at the cited line(s)
# - exits 0 if all refs verify, 1 if any errors found

function verify_snippet(file, line, snippet,    cmd, actual, found, truncated) {
	# Skip placeholder refs like file.c:line
	if (line == "line" || file == ".upstream/file.c") return 1
	cmd = "sed -n " line "p " file
	cmd | getline actual
	close(cmd)
	if (actual == "") {
		printf "  STALE: line %s of %s is empty\n", line, file > "/dev/stderr"
		return 0
	}
	# Handle truncated snippets (ending with ...)
	truncated = 0
	if (substr(snippet, length(snippet)-2) == "...") {
		truncated = 1
		snippet = substr(snippet, 1, length(snippet) - 3)
	}
	# Handle range end snippets (starting with ... )
	if (substr(snippet, 1, 4) == "... ") {
		snippet = substr(snippet, 5)
	}
	found = index(actual, snippet)
	if (!found) {
		printf "  MISMATCH: snippet \"%s%s\" not found at %s:%s\n", snippet, truncated ? "..." : "", file, line > "/dev/stderr"
		printf "    actual: %s\n", actual > "/dev/stderr"
		return 0
	}
	return 1
}

# Extract snippet that immediately follows a ref at position ref_end in the original line
function get_snippet_at(orig_line, ref_end,    rest, snippet, pos) {
	rest = substr(orig_line, ref_end)
	# Skip whitespace after ref
	gsub(/^[[:space:]]+/, "", rest)
	# Check for `, ` pattern (backtick-comma-space-backtick)
	if (substr(rest, 1, 3) != ", `") return ""
	# Find the closing backtick of the snippet
	rest = substr(rest, 4)  # skip `, `
	pos = index(rest, "`")
	if (pos == 0) return ""
	snippet = substr(rest, 1, pos - 1)
	return snippet
}

function show_context(file, start, stop) {
	system("gawk 'FNR >= " start " && FNR <= " stop " { printf \"%s:%d:%s\\n\", FILENAME, FNR, $0 } FNR == " stop " { exit }' " file)
}

BEGIN { errors = 0 }

n = patsplit($0, matches, /`[.]upstream[/][^`]+`/) {
	for (i = 1; i <= n; i++) {
		ref = matches[i]
		gsub(/^`|`$/, "", ref)
		c = split(ref, up, ":")
		if (c == 1) {
			# file-only ref, no verification needed
			continue
		} else if (c == 2) {
			# Find this ref in the original line and extract snippet after it
			snippet = ""
			pos = index($0, matches[i])
			if (pos > 0) {
				ref_end = pos + length(matches[i])
				snippet = get_snippet_at($0, ref_end)
			}

			c2 = split(up[2], lines, "-")
			if (c2 == 1) {
				# single line
				if (snippet) {
					if (!verify_snippet(up[1], lines[1], snippet)) {
						errors++
						printf "%s:%d: %s\n", FILENAME, FNR, matches[i] > "/dev/stderr"
						show_context(up[1], lines[1]-1, lines[1]+1)
					}
				}
			} else if (c2 == 2) {
				# range
				if (snippet) {
					split(snippet, parts, " \\.{3} ")
					ok = 1
					if (parts[1] && !verify_snippet(up[1], lines[1], parts[1])) ok = 0
					if (parts[2] && !verify_snippet(up[1], lines[2], parts[2])) ok = 0
					if (!ok) {
						errors++
						printf "%s:%d: %s\n", FILENAME, FNR, matches[i] > "/dev/stderr"
						if (lines[2] - lines[1] >= 10) {
							show_context(up[1], lines[1], lines[2])
						} else {
							show_context(up[1], lines[1]-1, lines[2]+1)
						}
					}
				}
			} else {
				printf "\nerror: unknown upstream ref (%d lines): %s\n", c2, matches[i] > "/dev/stderr"
				exit(1)
			}
		} else {
			printf "\nerror: unknown upstream ref (%d splits): %s\n", c, matches[i] > "/dev/stderr"
			exit(1)
		}
	}
}

END {
	if (errors > 0) {
		printf "\n%d verification error(s) found.\n", errors > "/dev/stderr"
		exit(1)
	}
}
