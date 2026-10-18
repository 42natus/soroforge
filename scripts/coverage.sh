#!/usr/bin/env bash
# Summarise a Go coverage profile per package, optionally against a baseline.
#
#   scripts/coverage.sh coverage.out                 # per-package table
#   scripts/coverage.sh coverage.out base.out        # table with change vs base
#
# Prints a Markdown table, so CI can append it to $GITHUB_STEP_SUMMARY.
# With a baseline, exits 1 when total coverage has dropped by more than
# COVERAGE_MAX_DROP percentage points (default 1.0).
#
# Test helpers that are compiled into the package but exercised only by other
# tests (internal/stellar/mock.go) are excluded, or the figure is misleading.
set -euo pipefail

EXCLUDE='/internal/stellar/mock\.go:'
MAX_DROP="${COVERAGE_MAX_DROP:-1.0}"

usage() {
	echo "usage: $0 <coverage.out> [baseline.out]" >&2
	exit 2
}

[ $# -ge 1 ] && [ $# -le 2 ] || usage
for f in "$@"; do
	[ -f "$f" ] || { echo "$0: no such profile: $f" >&2; exit 2; }
done

# Prints "<package> <statements> <covered>" per package, then "TOTAL ...".
# A block can appear more than once in a ./... profile; count it once, as
# covered if any appearance was hit.
summarise() {
	awk -v exclude="$EXCLUDE" '
		NR == 1 && /^mode:/ { next }
		$0 ~ exclude { next }
		NF == 3 {
			key = $1
			if (!(key in stmts)) {
				file = key
				sub(/:[^:]*$/, "", file)
				pkg = file
				sub(/\/[^\/]*$/, "", pkg)
				stmts[key] = $2
				pkgof[key] = pkg
			}
			if ($3 > 0) hit[key] = 1
		}
		END {
			for (k in stmts) {
				p = pkgof[k]
				s[p] += stmts[k]
				if (k in hit) c[p] += stmts[k]
				ts += stmts[k]
				if (k in hit) tc += stmts[k]
			}
			for (p in s) printf "%s %d %d\n", p, s[p], c[p] + 0
			printf "TOTAL %d %d\n", ts + 0, tc + 0
		}
	' "$1"
}

head_sum=$(summarise "$1")
base_sum=""
[ $# -eq 2 ] && base_sum=$(summarise "$2")

# Join head and base on package and render the table. Packages that exist on
# only one side show "–" for the other.
awk -v max_drop="$MAX_DROP" -v has_base="$([ -n "$base_sum" ] && echo 1 || echo 0)" '
	function pct(c, s) { return s > 0 ? 100 * c / s : 0 }
	function cell(c, s) { return s > 0 ? sprintf("%.1f%%", pct(c, s)) : "–" }
	function short(p) { sub(/^github\.com\/[^\/]+\/[^\/]+\/?/, "", p); return p == "" ? "." : p }
	NF == 0 { next }
	FNR == NR { hs[$1] = $2; hc[$1] = $3; seen[$1] = 1; next }
	{ bs[$1] = $2; bc[$1] = $3; seen[$1] = 1 }
	END {
		n = 0
		for (p in seen) if (p != "TOTAL") names[++n] = p
		# Insertion sort: portable across awk implementations.
		for (i = 2; i <= n; i++) {
			v = names[i]
			for (j = i - 1; j > 0 && names[j] > v; j--) names[j + 1] = names[j]
			names[j + 1] = v
		}

		ht = pct(hc["TOTAL"], hs["TOTAL"])
		print "### Coverage"
		print ""
		if (has_base) {
			bt = pct(bc["TOTAL"], bs["TOTAL"])
			printf "Total: **%.1f%%** (main: %.1f%%, change: %+.1f pp; fails below -%.1f pp)\n\n", ht, bt, ht - bt, max_drop
			print "| Package | main | This change | Δ |"
			print "|---|---:|---:|---:|"
		} else {
			printf "Total: **%.1f%%**\n\n", ht
			print "| Package | Coverage |"
			print "|---|---:|"
		}
		for (i = 1; i <= n; i++) {
			p = names[i]
			if (has_base) {
				d = (hs[p] > 0 && bs[p] > 0) ? sprintf("%+.1f", pct(hc[p], hs[p]) - pct(bc[p], bs[p])) : "–"
				printf "| `%s` | %s | %s | %s |\n", short(p), cell(bc[p], bs[p]), cell(hc[p], hs[p]), d
			} else {
				printf "| `%s` | %s |\n", short(p), cell(hc[p], hs[p])
			}
		}
		print ""
		print "_Excludes test helpers (`internal/stellar/mock.go`)._"

		if (has_base && bt - ht > max_drop + 1e-9) {
			printf "\n**Coverage dropped by %.1f pp, more than the allowed %.1f pp.**\n", bt - ht, max_drop
			exit 1
		}
	}
' <(echo "$head_sum") <(echo "${base_sum:-}")
