#!/usr/bin/env bash
# soak-test.sh — multi-hour cluster-from-hell experiment platform.
#
# Goal: measure natra's behavior on production-shaped chaos over time
# scales that the perf-vs-vanilla rig can't reach. The perf rig runs
# 30 seconds and the CMS aging interval is 60 seconds — so it never
# sees decay fire, never sees long-term saturation, never sees node
# churn, and never exercises coexistence with another BPF stack
# attaching to the same pod veth.
#
# What this rig adds:
#
#   - Multi-hour duration (default 4h). Long enough that CMS aging
#     fires (~240 decay intervals in a 4h run at 60s cadence) and
#     long-term drift / saturation curves become visible.
#
#   - Node churn. k3d's `node create / delete` lets us add and remove
#     workers while the cluster keeps running. kind doesn't — its
#     node count is fixed at create time. Cluster autoscalers, spot
#     reclaims, and AZ failures all look like node churn from a CNI's
#     perspective; this rig surfaces install-path bugs that only
#     show up when natra meets a fresh node mid-life.
#
#   - Calico CNI in eBPF mode. Calico-eBPF attaches TC programs to
#     the host-side veth via clsact, mirroring how the AWS network-
#     policy-agent + VPC CNI attach in EKS. natra layers on top.
#     Mirrors the natra-on-EKS-with-NPA coexistence shape.
#
#   - Goldpinger DaemonSet. All-pairs connectivity probe scraping
#     /metrics every second. If any plugin (natra or vanilla or
#     Calico itself) silently breaks routing on some pod combo, the
#     RTT / timeout shows up in goldpinger long before our explicit
#     iperf measurement would catch it.
#
#   - Three modes (--mode flag):
#         natra      → Calico + natra chained on top
#         vanilla    → Calico + upstream bandwidth plugin chained on top
#         baseline   → Calico alone, no rate-limiting plugin
#     Run each separately, get three TSVs, compare post-hoc. The
#     baseline tells you what natra is "paying" in throttle costs
#     when it's doing the right thing; the vanilla tells you what
#     prior art gives you with the same plumbing.
#
# Output (per run, under --output):
#
#     metrics.tsv           one row per measurement (every 60s),
#                           columns: timestamp, iperf_ing_mbps,
#                           iperf_eg_mbps, hey_rps, hey_p50_ms,
#                           hey_p99_ms, gp_failed_probes, gp_avg_rtt_ms,
#                           cms_max_nonzero, cms_max_count,
#                           bpf_ingress_ns_per_op, bpf_egress_ns_per_op
#
#     heap/heap-NNNNN.pprof bounded ring buffer (keep latest 20).
#     bpf/snapshots-MMM.jsonl  bounded ring buffer (keep latest 20).
#     events.log            node churn events, pod churn events,
#                           any anomalies (goldpinger failed probes,
#                           BPF stat regressions).
#
# Run time: as long as --duration. Default 4h. Operator picks per run.
#
# WORK IN PROGRESS — this is the skeleton. Sections marked TODO will
# be filled out in follow-up commits as the harness comes together.

set -euo pipefail

REPO_ROOT="$(cd "$(dirname "$0")/.." && pwd)"

# Defaults — override via flags.
MODE="natra"
DURATION="4h"
OUTPUT_DIR="${REPO_ROOT}/results/soak-$(date -u +%Y%m%dT%H%M%SZ)"
INITIAL_NODES=3
NODE_CHURN_INTERVAL_S=600    # add/remove a worker every 10 min
SAMPLE_INTERVAL_S=60         # light measurement cadence
DEEP_INTERVAL_S=1800         # heap pprof + bpf stats snapshot cadence
RETAIN_HEAP=20               # keep latest N heap pprof files
RETAIN_BPF=20                # keep latest N bpf snapshot files

usage() {
    cat <<'EOF'
Usage: soak-test.sh [flags]

  --mode {natra,vanilla,baseline}
        Which rate-limiting layer to chain after Calico. Default: natra.

  --duration <go-duration>
        How long to run (e.g. 4h, 30m, 1d). Default: 4h.

  --output <dir>
        Where to write metrics.tsv, heap/, bpf/, events.log. Default:
        results/soak-<UTC-timestamp>/

  --initial-nodes <N>
        Worker count at startup (k3d cluster create). Default: 3.

  --node-churn-interval <seconds>
        How often to add/remove a node. 0 disables churn. Default: 600.

  --sample-interval <seconds>
        Light-measurement cadence (iperf + hey + goldpinger scrape).
        Default: 60.

  --deep-interval <seconds>
        Heavy-snapshot cadence (heap pprof + bpf prog stats).
        Default: 1800.

Mode descriptions:
  natra     — Calico + natra. Default production-shaped setup.
  vanilla   — Calico + upstream containernetworking/plugins/bandwidth.
              Reference implementation, HTB-on-IFB rate limiting.
  baseline  — Calico alone, no rate limiter at all. The "what natra
              is paying for" reference point.
EOF
}

# Argument parsing.
while [ $# -gt 0 ]; do
    case "$1" in
        --mode)              MODE="$2"; shift 2 ;;
        --duration)          DURATION="$2"; shift 2 ;;
        --output)            OUTPUT_DIR="$2"; shift 2 ;;
        --initial-nodes)     INITIAL_NODES="$2"; shift 2 ;;
        --node-churn-interval) NODE_CHURN_INTERVAL_S="$2"; shift 2 ;;
        --sample-interval)   SAMPLE_INTERVAL_S="$2"; shift 2 ;;
        --deep-interval)     DEEP_INTERVAL_S="$2"; shift 2 ;;
        -h|--help)           usage; exit 0 ;;
        *)                   echo "unknown flag: $1"; usage; exit 1 ;;
    esac
done

case "$MODE" in
    natra|vanilla|baseline) ;;
    *) echo "--mode must be one of: natra, vanilla, baseline"; exit 1 ;;
esac

require() {
    command -v "$1" >/dev/null 2>&1 || { echo "missing: $1"; exit 1; }
}
require docker
require k3d
require kubectl
require jq

mkdir -p "$OUTPUT_DIR/heap" "$OUTPUT_DIR/bpf"

# TODO sections — filled in subsequent commits as the harness lands:
#
#   1. bootstrap_cluster
#        k3d cluster create with $INITIAL_NODES workers, install
#        Calico in eBPF mode, install goldpinger DaemonSet, install
#        the rate-limiting layer for $MODE.
#
#   2. deploy_workload
#        Deploy long-lived server pod with bidi bandwidth annotation
#        (server runs iperf3 + nginx). Deploy client pod with iperf3
#        + hey + curl.
#
#   3. node_churn_loop
#        Background goroutine-style loop: every
#        NODE_CHURN_INTERVAL_S, either add or remove a worker
#        (alternate). Log to events.log.
#
#   4. measurement_loop
#        Every SAMPLE_INTERVAL_S:
#          - Short iperf3 --bidir + concurrent hey burst
#          - Scrape goldpinger /metrics
#          - Append one row to metrics.tsv
#
#   5. snapshot_loop
#        Every DEEP_INTERVAL_S:
#          - Dump heap pprof from natra-installer's age-cms... wait,
#            natra-installer has no long-running natra process. We
#            need to spin up a one-off `natra profile` on a worker
#            for snapshot and tear it down. Sketch only — implement
#            once first three loops work.
#          - Save snapshots.jsonl + heap pprof, rotate to keep latest
#            RETAIN_HEAP / RETAIN_BPF files.
#
#   6. teardown
#        k3d cluster delete on exit (success or failure).
#
echo "soak-test skeleton — implementation in progress."
echo "  mode:                 $MODE"
echo "  duration:             $DURATION"
echo "  output:               $OUTPUT_DIR"
echo "  initial nodes:        $INITIAL_NODES"
echo "  node churn interval:  ${NODE_CHURN_INTERVAL_S}s"
echo "  sample interval:      ${SAMPLE_INTERVAL_S}s"
echo "  deep interval:        ${DEEP_INTERVAL_S}s"
exit 0
