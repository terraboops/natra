#!/usr/bin/env bash
# tune-from-soak.sh — analyze a soak-test result directory and
# surface signals for tuning natra's defaults.
#
# Usage:
#   scripts/tune-from-soak.sh <soak-result-dir>
#
# Prints:
#   - Per-mode mean/p50/p99 of key metrics across the run
#   - Trend signal: are hey RPS / latency drifting over the run?
#     (DROP over time = aging isn't keeping up; constant = good)
#   - CMS fill curve (from bpf/snapshots-*.jsonl)
#   - BPF program ns/op trend (regression detector)
#   - Concrete tuning recommendation if a signal exceeds threshold

set -euo pipefail

dir="${1:-}"
if [ -z "$dir" ] || [ ! -d "$dir" ]; then
    echo "usage: $0 <soak-result-dir>" >&2
    echo "example: $0 /tmp/soak-natra-2h30m" >&2
    exit 1
fi

tsv="$dir/metrics.tsv"
if [ ! -s "$tsv" ]; then
    echo "no metrics.tsv in $dir" >&2
    exit 1
fi

echo "=== $dir ==="
n=$(tail -n +2 "$tsv" | wc -l | tr -d ' ')
duration_s=$(tail -n +2 "$tsv" | awk -F'\t' 'NR==1{first=$1} {last=$1} END{print last-first}')
duration_h=$(awk -v s="$duration_s" 'BEGIN{printf "%.2f", s/3600}')
echo "samples: $n"
echo "duration: ${duration_s}s (${duration_h}h)"

echo
echo "=== central tendency (across the whole run) ==="
tail -n +2 "$tsv" | awk -F'\t' '
{
    ing_sum += $2; eg_sum += $3; rps_sum += $4
    p50_sum += $5; p99_sum += $6
    gp_failed += $8
    n++
}
END {
    if (n == 0) exit
    printf "iperf_ingress: mean %.2f Mbps\n", ing_sum / n / 1e6
    printf "iperf_egress:  mean %.2f Mbps\n", eg_sum / n / 1e6
    printf "hey_rps:       mean %.0f req/s\n", rps_sum / n
    printf "hey_p50:       mean %.1f ms\n", p50_sum / n * 1000
    printf "hey_p99:       mean %.1f ms\n", p99_sum / n * 1000
    printf "goldpinger failed probes (total): %d\n", gp_failed
}'

echo
echo "=== trend (first quartile vs last quartile) ==="
# Split rows into 4 quartiles, compare Q1 average to Q4 average.
# A DECLINING hey_rps over the run = aging isn't keeping CMS clean.
tail -n +2 "$tsv" | awk -F'\t' -v n="$n" '
{ rows[NR]=$0 }
END {
    q = int(n / 4)
    if (q < 1) { print "(too few samples for trend)"; exit }
    for (i = 1; i <= q; i++) {
        split(rows[i], a, "\t")
        q1_rps += a[4]; q1_p99 += a[6]; q1_ing += a[2]
    }
    for (i = n - q + 1; i <= n; i++) {
        split(rows[i], a, "\t")
        q4_rps += a[4]; q4_p99 += a[6]; q4_ing += a[2]
    }
    q1_rps /= q; q4_rps /= q; q1_p99 /= q; q4_p99 /= q
    q1_ing /= q; q4_ing /= q
    printf "hey_rps:       Q1=%.0f → Q4=%.0f  (%+.1f%%)\n", q1_rps, q4_rps, (q4_rps - q1_rps) / q1_rps * 100
    printf "hey_p99 (ms):  Q1=%.1f → Q4=%.1f  (%+.1f%%)\n", q1_p99 * 1000, q4_p99 * 1000, (q4_p99 - q1_p99) / q1_p99 * 100
    printf "iperf_ing(Mbps): Q1=%.2f → Q4=%.2f  (%+.1f%%)\n", q1_ing / 1e6, q4_ing / 1e6, (q4_ing - q1_ing) / q1_ing * 100
}'

echo
echo "=== BPF snapshot drift (if available) ==="
if ls "$dir/bpf/snapshots-"*.jsonl >/dev/null 2>&1; then
    for f in "$dir/bpf/snapshots-"*.jsonl; do
        slot=$(basename "$f" .jsonl | sed 's/snapshots-//')
        cms_total=$(jq -s 'first.pods[0].cms_total_count // 0' "$f" 2>/dev/null)
        cms_fill=$(jq -s 'first.pods[0] | (.cms_nonzero / (.cms_zeros + .cms_nonzero) * 100) | floor' "$f" 2>/dev/null)
        ns_per_op=$(jq -s '[first.programs[] | select(.run_count > 1000) | (.runtime_ns / .run_count)] | (add / length // 0) | floor' "$f" 2>/dev/null)
        printf "  slot %s: cms_fill=%s%% cms_total=%s ns/op=%s\n" "$slot" "$cms_fill" "$cms_total" "$ns_per_op"
    done
else
    echo "  no bpf snapshots (--deep-interval was 0?)"
fi

echo
echo "=== tuning recommendations ==="
# Compute trend deltas in awk again, then suggest based on direction.
tail -n +2 "$tsv" | awk -F'\t' -v n="$n" '
{ rows[NR]=$0 }
END {
    q = int(n / 4)
    if (q < 1) { print "(insufficient samples for recommendations)"; exit }
    for (i = 1; i <= q; i++) { split(rows[i], a, "\t"); q1_rps += a[4]; q1_p99 += a[6] }
    for (i = n-q+1; i <= n; i++) { split(rows[i], a, "\t"); q4_rps += a[4]; q4_p99 += a[6] }
    q1_rps /= q; q4_rps /= q; q1_p99 /= q; q4_p99 /= q
    drop_rps = (q1_rps - q4_rps) / q1_rps * 100
    rise_p99 = (q4_p99 - q1_p99) / q1_p99 * 100

    if (drop_rps > 20) {
        printf "  ⚠ hey RPS dropped %.0f%% Q1→Q4 — CMS is saturating despite aging.\n", drop_rps
        printf "    Tightening recommendations: shorten CMS_DECAY_INTERVAL_NS\n"
        printf "    (currently 60s → try 30s), or bump CMS_WIDTH again.\n"
    } else if (drop_rps < -10) {
        printf "  hey RPS improved %.0f%% over the run — workload settled\n", -drop_rps
        printf "    or aging effect is helping. No tightening needed.\n"
    } else {
        printf "  hey RPS stable within ±20%% across quartiles — defaults look right.\n"
    }
    if (rise_p99 > 50) {
        printf "  ⚠ hey p99 rose %.0f%% — tail latency creeping; consider lower\n", rise_p99
        printf "    threshold so the elephant gets bucket-gated faster.\n"
    }
}'

# Heap pprof presence
heap_count=$(ls "$dir/heap" 2>/dev/null | wc -l | tr -d ' ')
echo
echo "heap pprof snapshots: $heap_count (use \`go tool pprof <file>\` for detail)"
