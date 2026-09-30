#!/usr/bin/env bash
# CI benchmark gate. Benchmark results must never get worse.
#
# Runs an interleaved A/B (bench-ab.sh) and gates it (bench-gate.sh).
# If anything regresses, runs a second full A/B and fails only on
# benchmarks that regress in both runs: a real regression of any size
# reproduces, while code-alignment and runner noise mostly doesn't.
#
# Usage: scripts/bench-check.sh <base-dir> <head-dir> <out-dir>

set -euo pipefail

if [ "$#" -ne 3 ]; then
	echo "usage: $0 <base-dir> <head-dir> <out-dir>" >&2
	exit 64
fi
here="$(cd "$(dirname "$0")" && pwd)"
base_dir="$1"
head_dir="$2"
out="$3"
mkdir -p "$out"

"$here/bench-ab.sh" "$base_dir" "$head_dir" "$out/bench-main-1.txt" "$out/bench-head-1.txt"
if "$here/bench-gate.sh" "$out/bench-main-1.txt" "$out/bench-head-1.txt" "$out/regressed-1.txt"; then
	exit 0
fi

echo
echo "Re-running the A/B to check whether the regressions reproduce."
"$here/bench-ab.sh" "$base_dir" "$head_dir" "$out/bench-main-2.txt" "$out/bench-head-2.txt"
"$here/bench-gate.sh" "$out/bench-main-2.txt" "$out/bench-head-2.txt" "$out/regressed-2.txt" || true

confirmed="$(sort "$out/regressed-1.txt" | comm -12 - <(sort "$out/regressed-2.txt"))"
if [ -n "$confirmed" ]; then
	echo
	echo "Regressions that reproduced in both runs:"
	printf '%s\n' "$confirmed" | sed 's/^/  /'
	exit 1
fi
echo
echo "No regression reproduced across both runs."
