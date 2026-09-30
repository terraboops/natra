#!/usr/bin/env bash
# Interleaved A/B benchmark run between two checkouts. Rounds alternate
# base → head so machine drift lands on both sides equally; with the
# defaults each side gets 10 samples per benchmark, enough for
# benchstat to call a difference.
#
# Runs the Go benchmarks under ./pkg/... and the BPF hot-path
# benchmarks in test/perf (Linux only; BPF_PROG_TEST_RUN needs root,
# so they go through sudo when not already root). Each checkout needs
# its bpf/natra.bpf.o built and copied to pkg/bpf/ first.
#
# Usage: scripts/bench-ab.sh <base-dir> <head-dir> <base-out> <head-out>
# Env:   BENCH_ROUNDS (default 5), BENCH_COUNT per round (default 2)

set -euo pipefail

if [ "$#" -ne 4 ]; then
	echo "usage: $0 <base-dir> <head-dir> <base-out> <head-out>" >&2
	exit 64
fi
base_dir="$1"
head_dir="$2"
base_out="$(realpath "$3" 2>/dev/null || echo "$PWD/$3")"
head_out="$(realpath "$4" 2>/dev/null || echo "$PWD/$4")"
rounds="${BENCH_ROUNDS:-5}"
count="${BENCH_COUNT:-2}"

sudo_cmd=()
if [ "$(id -u)" -ne 0 ]; then
	sudo_cmd=(sudo)
fi

# The BPF benchmarks are compiled once per side as the invoking user
# and only the test binary runs under sudo, so root never writes into
# the shared Go build cache.
bin_dir="$(mktemp -d)"
trap 'rm -rf "$bin_dir"' EXIT
bpf_bench=0
if [ "$(uname -s)" = Linux ]; then
	bpf_bench=1
	(cd "$base_dir" && go test -c -tags=perf -o "$bin_dir/base-perf.test" ./test/perf/)
	(cd "$head_dir" && go test -c -tags=perf -o "$bin_dir/head-perf.test" ./test/perf/)
fi

: >"$base_out"
: >"$head_out"

run_side() {
	local side="$1" dir="$2" out="$3"
	(cd "$dir" && go test -run='^$' -bench=. -benchmem -count="$count" ./pkg/... >>"$out")
	if [ "$bpf_bench" = 1 ]; then
		(cd "$dir/test/perf" && "${sudo_cmd[@]}" "$bin_dir/$side-perf.test" \
			-test.run='^$' -test.bench=BPF -test.count="$count" >>"$out")
	fi
}

for i in $(seq "$rounds"); do
	echo "round $i/$rounds" >&2
	run_side base "$base_dir" "$base_out"
	run_side head "$head_dir" "$head_out"
done
