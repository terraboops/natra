#!/usr/bin/env bash
# analyze-profile.sh — quick read of a profile artifact dir.
#
# Run this on the output of `NATRA_PERF_ARTIFACT_DIR=... bash
# scripts/perf-vs-vanilla.sh` or on the equivalent soak-test output.
# Prints: BPF program ns/op, CMS fill state, stats counters, heap
# top allocations. Useful between perf-rig iterations to spot the
# next thing to tune without re-walking the JSONL by hand each time.
#
# Usage:
#   scripts/analyze-profile.sh <artifact-dir>
#   scripts/analyze-profile.sh /tmp/natra-artifacts/profile-natra-<ts>

set -euo pipefail

dir="${1:-}"
if [ -z "$dir" ] || [ ! -d "$dir" ]; then
    echo "usage: $0 <artifact-dir>" >&2
    exit 1
fi

jsonl="$dir/snapshots.jsonl"
if [ ! -s "$jsonl" ]; then
    echo "no snapshots.jsonl in $dir" >&2
    exit 1
fi

echo "=== $dir ==="
echo "snapshots: $(wc -l < "$jsonl" | tr -d ' ')"

echo
echo "=== BPF program ns/op (programs with run_count > 1000 only) ==="
tail -1 "$jsonl" | jq -r '
    .programs[]
    | select(.run_count > 1000)
    | "\(.name) run_count=\(.run_count) runtime_ns=\(.runtime_ns) ns_per_op=\((.runtime_ns / .run_count) | floor)"
'

echo
echo "=== CMS fill (per pod) ==="
tail -1 "$jsonl" | jq -r '
    .pods[]
    | "\(.container_id[0:12]) zeros=\(.cms_zeros) nonzero=\(.cms_nonzero) fill=\(((.cms_nonzero / (.cms_zeros + .cms_nonzero)) * 100) | floor)% max=\(.cms_max_count) total_incr=\(.cms_total_count)"
'

echo
echo "=== Per-direction stats (per pod) ==="
tail -1 "$jsonl" | jq -r '
    .pods[]
    | .container_id[0:12] as $cid
    | (.stats.ingress // {}) as $i
    | (.stats.egress // {}) as $e
    | "\($cid) ingress: passed=\($i.passed) throttled=\($i.throttled) hh_hits=\($i.hh_hits) (\(((($i.hh_hits // 0) / (($i.passed // 0) + ($i.throttled // 0) + 1)) * 100) | floor)% heavy)\n\($cid) egress:  passed=\($e.passed) throttled=\($e.throttled) hh_hits=\($e.hh_hits) (\(((($e.hh_hits // 0) / (($e.passed // 0) + ($e.throttled // 0) + 1)) * 100) | floor)% heavy)"
'

heap_count=$(ls "$dir/heap" 2>/dev/null | wc -l | tr -d ' ')
echo
echo "=== Heap pprof files: $heap_count ==="
if [ "$heap_count" -gt 0 ]; then
    last_heap=$(ls "$dir/heap" | sort | tail -1)
    echo "top-5 allocations in $last_heap:"
    go tool pprof -top -nodecount=5 "$dir/heap/$last_heap" 2>&1 | grep -E "^\s+[0-9]" | head -5 || true
fi
