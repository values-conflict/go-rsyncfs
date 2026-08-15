#!/usr/bin/env -S gawk -f

# usage: ./upstreams.gawk protocol.md

n = patsplit($0, matches, /`[.]upstream[/][^`]+`/) {
	printf "\n%s:%s:%s\n\n", FILENAME, FNR, $0
	for (i = 1; i <= n; i++) {
		printf "=== %s ===\n", matches[i]
		gsub(/^`|`$/, "", matches[i])
		c = split(matches[i], up, ":")
		if (c == 1) {
			# "head" the file
			system("grep -HnvE '^\\s*(/[*/]|[*]|$)' " up[1] " | head -3")
		} else if (c == 2) {
			c = split(up[2], lines, "-")
			if (c == 1) {
				# print one line (with context)
				start = lines[1] - 1
				stop  = lines[1] + 1
			} else if (c == 2) {
				# print range of lines
				if (lines[2] - lines[1] >= 10) {
					# print "large" range verbatim (no context)
					start = lines[1]
					stop = lines[2]
				} else {
					# add a tiny smidge of context to "short" ranges
					start = lines[1] - 1
					stop  = lines[2] + 1
				}
			} else {
				printf "\nerror: unknown upstream ref (%d lines): %s\n", c, matches[i] > /dev/stderr
				exit(1)
			}
			system("gawk 'FNR >= " start " && FNR <= " stop " { printf \"%s:%s:%s\\n\", FILENAME, FNR, $0 } FNR == " stop " { exit }' " up[1])
		} else {
			printf "\nerror: unknown upstream ref (%d splits): %s\n", c, matches[i] > /dev/stderr
			exit(1)
		}
	}
	printf "\n"
}
