#!/usr/bin/env bash
# Compare two `go test -bench` outputs and fail on any statistically
# significant regression. Benchmark results must never get worse.
#
# benchstat prints a signed delta in its "vs base" column only when
# the difference is significant (p < 0.05); otherwise "~". Every
# metric natra's benchmarks report (sec/op, B/op, allocs/op,
# bpf-ns/op) is lower-is-better, so any "+" is a regression. Run each
# side with -count=10 or more so benchstat can detect a difference.
#
# Usage: scripts/bench-gate.sh <base.txt> <head.txt>

set -euo pipefail

if [ "$#" -ne 2 ]; then
	echo "usage: $0 <base.txt> <head.txt>" >&2
	exit 64
fi
base="$1"
head="$2"

benchstat "$base" "$head"

# CSV sections open with ",<base>,,<head>,..." then ",<unit>,CI,...".
regressions="$(benchstat -format csv "$base" "$head" 2>/dev/null | awk -F, '
	/^,/ { unit = $2; next }
	$1 == "" || $1 == "geomean" { next }
	$6 ~ /^\+/ { printf "  %s %s %s (%s)\n", $1, unit, $6, $7 }
')"

if [ -n "$regressions" ]; then
	echo
	echo "Benchmark regressions vs base (p < 0.05):"
	echo "$regressions"
	exit 1
fi
echo
echo "No significant benchmark regressions."
