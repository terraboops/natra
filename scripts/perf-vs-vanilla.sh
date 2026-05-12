#!/usr/bin/env bash
# Real-cluster head-to-head: natra vs upstream containernetworking/plugins/bandwidth.
#
# Spins up two kind clusters in sequence (one with natra chained behind
# kindnet, one with the upstream bandwidth plugin chained behind
# kindnet). Runs two workloads per cluster:
#
#   1. iperf-only (legacy). Four phases: ingress/egress × elephant/mice.
#      The "mice" here are iperf3 -P 20 — 20 parallel long-lived TCP
#      flows. Useful to characterize bucket behavior under parallel
#      elephants, but doesn't exercise natra's CMS fast-pass (every
#      flow eventually crosses the heavy-hitter threshold).
#
#   2. realistic mixed (CMS fast-pass demo). One phase: iperf3 --bidir
#      elephant (drains both direction buckets) plus concurrent `hey`
#      HTTP load against an nginx in the same server pod
#      (-disable-keepalive so each request is a brand-new TCP flow,
#      well under threshold → natra fast-passes; vanilla HTB queues
#      everything in the same bucket). Measures hey RPS and p99
#      latency. Under natra hey should run at near-line-rate; under
#      vanilla the elephant starves it.
#
# Output: docs/perf-vs-vanilla-result.txt with the raw numbers.
#
# Run time: ~12-15 minutes. Docker required on macOS (colima or Docker
# Desktop).

set -euo pipefail

REPO_ROOT="$(cd "$(dirname "$0")/.." && pwd)"
RESULT_FILE="${REPO_ROOT}/docs/perf-vs-vanilla-result.txt"

NATRA_CLUSTER="natra-vs-vanilla-natra"
VANILLA_CLUSTER="natra-vs-vanilla-vanilla"
NATRA_IMAGE="ghcr.io/terraboops/natra:vsperf"
PERFCLIENT_IMAGE="ghcr.io/terraboops/natra-perfclient:vsperf"

ELEPHANT_DURATION=15
MICE_PARALLEL=20
MICE_DURATION=10
RATE="10M"

# Mixed-workload phase tuning. The iperf3 --bidir flow runs for
# MIXED_IPERF_DURATION; hey runs for MIXED_HEY_DURATION starting after
# MIXED_HEY_LAG so the bucket is fully drained before hey is measured.
# MIXED_HEY_DURATION + MIXED_HEY_LAG must be ≤ MIXED_IPERF_DURATION so
# iperf is still in flight while hey samples.
MIXED_IPERF_DURATION=30
MIXED_HEY_LAG=5
MIXED_HEY_DURATION=20
MIXED_HEY_CONCURRENCY=50

cleanup() {
    kind delete cluster --name "$NATRA_CLUSTER" 2>/dev/null || true
    kind delete cluster --name "$VANILLA_CLUSTER" 2>/dev/null || true
}
trap cleanup EXIT

require() {
    command -v "$1" >/dev/null 2>&1 || { echo "missing: $1"; exit 1; }
}

require docker
require kind
require kubectl
require jq

# Render iperf manifests with the cluster-specific node names. kind
# names nodes <cluster>-{control-plane,worker}; the source manifests
# hardcode natra-e2e-* names, so we sed them at runtime. The bidi
# manifest carries both ingress and egress annotations so the same
# server pod can be exercised in either direction without a redeploy.
render_manifests() {
    local cluster_name="$1" outdir="$2"
    # Rename the pod/service from iperf-server-bidi → iperf-server so
    # the run_workload commands below can use the canonical name and
    # match the L4 e2e shape. The bidi YAML carries both annotations,
    # which is what we want for this two-phase per-direction workload.
    sed -e "s|natra-e2e-worker|${cluster_name}-worker|" \
        -e "s|natra-e2e-control-plane|${cluster_name}-control-plane|" \
        -e "s|iperf-server-bidi|iperf-server|g" \
        "${REPO_ROOT}/test/e2e/manifests/iperf-server-bidi.yaml" \
        > "$outdir/iperf-server.yaml"
    sed -e "s|natra-e2e-worker|${cluster_name}-worker|" \
        -e "s|natra-e2e-control-plane|${cluster_name}-control-plane|" \
        "${REPO_ROOT}/test/e2e/manifests/iperf-client.yaml" \
        > "$outdir/iperf-client.yaml"
    cp "${REPO_ROOT}/test/e2e/manifests/namespace.yaml" "$outdir/namespace.yaml"
}

# Patch the upstream bandwidth plugin's HTB classes to use a sane
# burst. Kubelet provides no per-pod burst override, so kubelet sends
# (and the plugin uses) a huge default — observed as
# burst=193MB / cburst=386MB on a 10 Mbps annotation. That's
# ~150 seconds of credit, enough that a 30s measurement runs entirely
# inside the burst window without rate-shaping engaging. Apply 1 MB
# burst / 1 MB cburst directly via tc so the measured rate reflects
# the configured rate, not the initial burst.
#
# Idempotent: skips classes that don't exist or already match. Run
# AFTER pods are Ready (so the plugin has installed the qdiscs).
fix_vanilla_htb_burst() {
    local cluster="$1"
    for node in $(kind get nodes --name "$cluster"); do
        docker exec "$node" bash -c '
            for dev in $(tc qdisc show 2>/dev/null | awk "/htb/ {print \$5}" | sort -u); do
                # class 1:30 (hex) = ShapedClassMinorID 48 in the
                # bandwidth plugin. The unshaped class 1:1 has rate
                # 800Gbit and is untouched.
                tc class change dev "$dev" classid 1:30 htb \
                    rate 10mbit ceil 20mbit burst 1mb cburst 1mb \
                    2>/dev/null || true
            done
        '
    done
}

# Render the mixed-workload manifests (perf-server with iperf3+nginx,
# perf-client with iperf3+hey).
render_mixed_manifests() {
    local cluster_name="$1" outdir="$2"
    sed -e "s|PERF_WORKER_NODE|${cluster_name}-worker|" \
        "${REPO_ROOT}/test/perf/realworld/perf-server.yaml" \
        > "$outdir/perf-server.yaml"
    sed -e "s|PERF_CONTROL_NODE|${cluster_name}-control-plane|" \
        "${REPO_ROOT}/test/perf/realworld/perf-client.yaml" \
        > "$outdir/perf-client.yaml"
}

# warmup_pod drains a freshly-attached qdisc's initial burst-token
# allowance before measurements. Both rate-limiters under test have a
# burst window — natra's is 2× rate (2.5 MB for a 10 Mbps annotation),
# upstream bandwidth's HTB picks up kubelet's default IngressBurst,
# which kubelet sets to a huge value (~193 MB observed on kind nodes,
# ~150 seconds of credit at 10 Mbps). Without a warmup, the first
# elephant flow consumes that burst at line rate before the rate
# limiter actually engages, inflating measurements by ~10× under
# vanilla. After ~12s of forward + 12s of reverse traffic, both
# plugins are at steady-state and subsequent measurements reflect the
# configured rate.
warmup_pod() {
    local namespace="natra-e2e" server="$1" client="$2"
    # 20s × ~100 Mbps line rate ≈ 250 MB transferred — enough to
    # fully drain vanilla's 193 MB HTB burst and natra's 2.5 MB
    # token bucket. -P 4 parallel streams to maximize throughput
    # during the drain since kindnet vxlan single-stream peaks below
    # line rate.
    kubectl exec -n "$namespace" "$client" -- \
        iperf3 -c "$server" -t 20 -P 4 >/dev/null 2>&1 || true
    kubectl exec -n "$namespace" "$client" -- \
        iperf3 -c "$server" -t 20 -P 4 -R >/dev/null 2>&1 || true
    # No post-warmup sleep: the next measurement starts with an
    # empty bucket so it can't borrow any unused-burst headroom.
    # The first measured second of the test will dip slightly
    # below rate, but the rest is true steady-state.
}

# run_workload prints four integers on stdout:
#   ingress_elephant_bps ingress_mice_bps egress_elephant_bps egress_mice_bps
# Forward iperf3 measures throughput INTO the server (ingress); reverse
# (-R) measures throughput OUT of the server (egress).
run_workload() {
    local namespace="natra-e2e"

    echo "==> warming up iperf-server (draining initial burst)" >&2
    warmup_pod iperf-server iperf-client

    local ing_elephant ing_mice eg_elephant eg_mice

    ing_elephant=$(kubectl exec -n "$namespace" iperf-client -- \
        iperf3 -c iperf-server -t "$ELEPHANT_DURATION" -J 2>/dev/null \
        | jq '.end.sum_received.bits_per_second // 0')

    ing_mice=$(kubectl exec -n "$namespace" iperf-client -- \
        iperf3 -c iperf-server -t "$MICE_DURATION" -P "$MICE_PARALLEL" -J 2>/dev/null \
        | jq '.end.sum_received.bits_per_second // 0')

    eg_elephant=$(kubectl exec -n "$namespace" iperf-client -- \
        iperf3 -c iperf-server -t "$ELEPHANT_DURATION" -R -J 2>/dev/null \
        | jq '.end.sum_received.bits_per_second // 0')

    eg_mice=$(kubectl exec -n "$namespace" iperf-client -- \
        iperf3 -c iperf-server -t "$MICE_DURATION" -P "$MICE_PARALLEL" -R -J 2>/dev/null \
        | jq '.end.sum_received.bits_per_second // 0')

    echo "$ing_elephant $ing_mice $eg_elephant $eg_mice"
}

# run_mixed_workload runs an iperf3 --bidir elephant concurrently with
# hey HTTP load, both targeting the perf-server pod. Prints six values:
#   iperf_ing_bps iperf_eg_bps hey_rps hey_p50_secs hey_p99_secs hey_total
# hey_p50_secs and hey_p99_secs are floats (seconds). Empty string for
# any value that couldn't be parsed (e.g., hey crashed) — caller
# decides what to do.
run_mixed_workload() {
    local namespace="natra-e2e" tag="$1"
    local iperf_log="$TMPDIR/iperf-mixed-${tag}.json"
    local hey_log="$TMPDIR/hey-mixed-${tag}.txt"

    echo "==> warming up perf-server (draining initial burst)" >&2
    warmup_pod perf-server perf-client

    # Start the elephant in the background. --bidir drains both
    # ingress and egress buckets simultaneously, so hey traffic
    # (request: ingress, response: egress) finds both buckets empty
    # under vanilla. Under natra, hey is classified as mice and skips
    # the bucket entirely.
    kubectl exec -n "$namespace" perf-client -- \
        iperf3 -c perf-server -t "$MIXED_IPERF_DURATION" --bidir -J \
        > "$iperf_log" 2>/dev/null &
    local iperf_pid=$!

    # Give the elephant time to fully drain the bucket before we
    # start measuring hey. Without this, hey's first few requests
    # would see a partially-full bucket and inflate the natra-vs-
    # vanilla gap artificially.
    sleep "$MIXED_HEY_LAG"

    # -disable-keepalive: each request opens a fresh TCP connection.
    # Each connection has its own 5-tuple (new source port) → its own
    # CMS flow_key → stays well under heavy-hitter threshold → natra
    # fast-passes it around the bucket. That's the design natra is
    # built around; this measures how much it's worth in practice.
    kubectl exec -n "$namespace" perf-client -- \
        hey -z "${MIXED_HEY_DURATION}s" -c "$MIXED_HEY_CONCURRENCY" \
        -disable-keepalive \
        http://perf-server/ \
        > "$hey_log" 2>&1 || true

    wait "$iperf_pid" || true

    # Parse iperf3 --bidir output. With --bidir there are TWO streams:
    # streams[0] is the forward (client→server, server's ingress);
    # streams[1] is the reverse (server→client, server's egress).
    # NOTE: .end.sum_sent and .end.sum_received in --bidir mode
    # aggregate ACROSS both streams, so they reflect total combined
    # throughput — not per-direction. We pull each stream's
    # .sender.bits_per_second instead.
    local iperf_ing iperf_eg
    iperf_ing=$(jq '.end.streams[0].sender.bits_per_second // 0' "$iperf_log" 2>/dev/null || echo 0)
    iperf_eg=$(jq '.end.streams[1].sender.bits_per_second // 0' "$iperf_log" 2>/dev/null || echo 0)

    # Parse hey output. hey prints a human-readable summary; we grep
    # the lines we care about. "Requests/sec:" gives RPS, the latency
    # distribution block has "50%% in <X> secs" / "99%% in <X> secs"
    # — hey emits a literal "%%" (double percent) in its latency
    # distribution lines, so the regex matches that.
    local rps p50 p99 total
    rps=$(awk -F: '/Requests\/sec:/ {gsub(/^ */, "", $2); print $2; exit}' "$hey_log")
    p50=$(awk '/50%% in/ {print $3; exit}' "$hey_log")
    p99=$(awk '/99%% in/ {print $3; exit}' "$hey_log")
    total=$(awk '/Total:/ {gsub(/^ */, "", $2); print $2; exit}' "$hey_log")

    : "${rps:=0}"
    : "${p50:=0}"
    : "${p99:=0}"
    : "${total:=0}"

    echo "$iperf_ing $iperf_eg $rps $p50 $p99 $total"
}

TMPDIR=$(mktemp -d)
trap 'rm -rf "$TMPDIR"; cleanup' EXIT

# Build images once, up front. Both clusters reuse them via kind load.
echo "==> building natra image: $NATRA_IMAGE"
docker build -q -t "$NATRA_IMAGE" -f "${REPO_ROOT}/deploy/docker/Dockerfile.cni" "$REPO_ROOT" >/dev/null

echo "==> building perfclient image: $PERFCLIENT_IMAGE"
docker build -q -t "$PERFCLIENT_IMAGE" -f "${REPO_ROOT}/deploy/docker/Dockerfile.perfclient" "$REPO_ROOT" >/dev/null

# ---- Phase A: natra ----
echo
echo "===================================================================="
echo "Phase A: natra"
echo "===================================================================="
mkdir -p "$TMPDIR/natra"
render_manifests "$NATRA_CLUSTER" "$TMPDIR/natra"
render_mixed_manifests "$NATRA_CLUSTER" "$TMPDIR/natra"

kind create cluster --name "$NATRA_CLUSTER" \
    --config "${REPO_ROOT}/test/e2e/kind-config.yaml" --wait 120s
kind load docker-image "$NATRA_IMAGE" "$PERFCLIENT_IMAGE" --name "$NATRA_CLUSTER"

kubectl apply -f "$TMPDIR/natra/namespace.yaml"
# NATRA_PERF_ATTACH_MODE picks the attach path. Default is tcx
# (production default). Set clsact-podside to exercise the fallback.
ATTACH_MODE="${NATRA_PERF_ATTACH_MODE:-}"
if [ "$ATTACH_MODE" = "tcx" ]; then ATTACH_MODE=""; fi
sed -e "s|ghcr.io/terraboops/natra:latest|${NATRA_IMAGE}|" \
    -e "s|imagePullPolicy: IfNotPresent|imagePullPolicy: Never|" \
    -e "s|value: \"\"$|value: \"${ATTACH_MODE}\"|" \
    "${REPO_ROOT}/deploy/cni-installer.yaml" | kubectl apply -f -
kubectl rollout status daemonset/natra-installer -n kube-system --timeout=120s

kubectl apply -f "$TMPDIR/natra/iperf-server.yaml"
kubectl apply -f "$TMPDIR/natra/iperf-client.yaml"
kubectl apply -f "$TMPDIR/natra/perf-server.yaml"
kubectl apply -f "$TMPDIR/natra/perf-client.yaml"
kubectl wait --for=condition=Ready \
    pod/iperf-server pod/iperf-client pod/perf-server pod/perf-client \
    -n natra-e2e --timeout=180s

echo "==> running iperf-only workload (phase A)"
read -r natra_ing_elephant natra_ing_mice natra_eg_elephant natra_eg_mice < <(run_workload)
echo "  natra ingress elephant=$natra_ing_elephant bps  mice=$natra_ing_mice bps"
echo "  natra egress  elephant=$natra_eg_elephant bps  mice=$natra_eg_mice bps"

echo "==> running mixed workload (phase A)"
read -r natra_mixed_iperf_ing natra_mixed_iperf_eg \
        natra_mixed_rps natra_mixed_p50 natra_mixed_p99 natra_mixed_total \
    < <(run_mixed_workload natra)
echo "  natra mixed iperf ingress=$natra_mixed_iperf_ing bps  egress=$natra_mixed_iperf_eg bps"
echo "  natra mixed hey rps=$natra_mixed_rps  p50=$natra_mixed_p50  p99=$natra_mixed_p99  total=$natra_mixed_total"

kind delete cluster --name "$NATRA_CLUSTER"

# ---- Phase B: upstream bandwidth plugin ----
echo
echo "===================================================================="
echo "Phase B: upstream containernetworking/plugins/bandwidth"
echo "===================================================================="
mkdir -p "$TMPDIR/vanilla"
render_manifests "$VANILLA_CLUSTER" "$TMPDIR/vanilla"
render_mixed_manifests "$VANILLA_CLUSTER" "$TMPDIR/vanilla"

kind create cluster --name "$VANILLA_CLUSTER" \
    --config "${REPO_ROOT}/test/e2e/kind-config.yaml" --wait 120s
kind load docker-image "$PERFCLIENT_IMAGE" --name "$VANILLA_CLUSTER"

# Load ifb on each kind node — the upstream bandwidth plugin uses
# HTB on an IFB device, and the kind base image ships the module but
# doesn't auto-load it. Doing this before the DaemonSet's install
# container patches the conflist guarantees the bandwidth plugin can
# create the IFB device when kubelet first invokes it.
for node in $(kind get nodes --name "$VANILLA_CLUSTER"); do
    docker exec "$node" modprobe ifb || \
        echo "warn: modprobe ifb on $node failed (continuing)"
done

kubectl apply -f "$TMPDIR/vanilla/namespace.yaml"
kubectl apply -f "${REPO_ROOT}/test/perf/realworld/vanilla-installer.yaml"
kubectl rollout status daemonset/vanilla-bandwidth-installer -n kube-system --timeout=120s

kubectl apply -f "$TMPDIR/vanilla/iperf-server.yaml"
kubectl apply -f "$TMPDIR/vanilla/iperf-client.yaml"
kubectl apply -f "$TMPDIR/vanilla/perf-server.yaml"
kubectl apply -f "$TMPDIR/vanilla/perf-client.yaml"
kubectl wait --for=condition=Ready \
    pod/iperf-server pod/iperf-client pod/perf-server pod/perf-client \
    -n natra-e2e --timeout=180s

# Patch HTB burst on every veth/ifb the bandwidth plugin created.
# Without this, kubelet's default-huge burst lets the first ~150s of
# traffic flow unshaped — measurements that fit inside that window
# never see HTB engage.
fix_vanilla_htb_burst "$VANILLA_CLUSTER"

echo "==> running iperf-only workload (phase B)"
read -r vanilla_ing_elephant vanilla_ing_mice vanilla_eg_elephant vanilla_eg_mice < <(run_workload)
echo "  vanilla ingress elephant=$vanilla_ing_elephant bps  mice=$vanilla_ing_mice bps"
echo "  vanilla egress  elephant=$vanilla_eg_elephant bps  mice=$vanilla_eg_mice bps"

echo "==> running mixed workload (phase B)"
read -r vanilla_mixed_iperf_ing vanilla_mixed_iperf_eg \
        vanilla_mixed_rps vanilla_mixed_p50 vanilla_mixed_p99 vanilla_mixed_total \
    < <(run_mixed_workload vanilla)
echo "  vanilla mixed iperf ingress=$vanilla_mixed_iperf_ing bps  egress=$vanilla_mixed_iperf_eg bps"
echo "  vanilla mixed hey rps=$vanilla_mixed_rps  p50=$vanilla_mixed_p50  p99=$vanilla_mixed_p99  total=$vanilla_mixed_total"

kind delete cluster --name "$VANILLA_CLUSTER"

# Format human-readable output.
fmt_bps() {
    awk -v b="$1" 'BEGIN {
        if (b == 0) { print "0.00 Mbps"; exit }
        printf "%.2f Mbps", b/1e6
    }'
}

fmt_rps() {
    awk -v r="$1" 'BEGIN {
        if (r+0 == 0) { print "0 req/s"; exit }
        printf "%.0f req/s", r
    }'
}

fmt_secs() {
    awk -v s="$1" 'BEGIN {
        if (s+0 == 0) { print "  -    "; exit }
        printf "%6.1f ms", s*1000
    }'
}

cat <<EOF | tee "$RESULT_FILE"
natra vs upstream containernetworking/plugins/bandwidth — kind cluster head-to-head
====================================================================================
Workload 1: iperf-only (legacy). Same iperf3 client/server, ${RATE} annotation each direction.
  - ingress: forward iperf3 (client → server, charged to server's ingress)
  - egress:  reverse iperf3 -R (server → client, charged to server's egress)
  - Phase 1: ${ELEPHANT_DURATION}s single elephant flow
  - Phase 2: ${MICE_DURATION}s × ${MICE_PARALLEL} parallel long-lived TCP flows
Iperf goodput, receiver-side aggregate (sum_received.bits_per_second).

Direction  Plugin                          Elephant            Mice (${MICE_PARALLEL}× parallel)
-------------------------------------------------------------------------------------------
ingress    natra                           $(fmt_bps "$natra_ing_elephant")        $(fmt_bps "$natra_ing_mice")
ingress    upstream bandwidth              $(fmt_bps "$vanilla_ing_elephant")        $(fmt_bps "$vanilla_ing_mice")
egress     natra                           $(fmt_bps "$natra_eg_elephant")        $(fmt_bps "$natra_eg_mice")
egress     upstream bandwidth              $(fmt_bps "$vanilla_eg_elephant")        $(fmt_bps "$vanilla_eg_mice")


Workload 2: realistic mixed (elephant + hey HTTP mice). Same server pod runs iperf3 +
nginx; client runs iperf3 --bidir for ${MIXED_IPERF_DURATION}s and (starting ${MIXED_HEY_LAG}s later) hey -c
${MIXED_HEY_CONCURRENCY} -disable-keepalive -z ${MIXED_HEY_DURATION}s against the nginx. Each hey request is a
fresh TCP connection → distinct CMS flow_key → stays under heavy-hitter
threshold. Under natra, hey fast-passes the bucket; under vanilla HTB, hey
shares the bucket with the elephant.

Plugin                Elephant ingress    Elephant egress    Hey RPS         Hey p50     Hey p99
-------------------------------------------------------------------------------------------------
natra                 $(fmt_bps "$natra_mixed_iperf_ing")        $(fmt_bps "$natra_mixed_iperf_eg")       $(fmt_rps "$natra_mixed_rps")      $(fmt_secs "$natra_mixed_p50")  $(fmt_secs "$natra_mixed_p99")
upstream bandwidth    $(fmt_bps "$vanilla_mixed_iperf_ing")        $(fmt_bps "$vanilla_mixed_iperf_eg")       $(fmt_rps "$vanilla_mixed_rps")      $(fmt_secs "$vanilla_mixed_p50")  $(fmt_secs "$vanilla_mixed_p99")


Raw numbers:
  natra_ingress_elephant=$natra_ing_elephant
  natra_ingress_mice=$natra_ing_mice
  natra_egress_elephant=$natra_eg_elephant
  natra_egress_mice=$natra_eg_mice
  vanilla_ingress_elephant=$vanilla_ing_elephant
  vanilla_ingress_mice=$vanilla_ing_mice
  vanilla_egress_elephant=$vanilla_eg_elephant
  vanilla_egress_mice=$vanilla_eg_mice
  natra_mixed_iperf_ingress=$natra_mixed_iperf_ing
  natra_mixed_iperf_egress=$natra_mixed_iperf_eg
  natra_mixed_hey_rps=$natra_mixed_rps
  natra_mixed_hey_p50=$natra_mixed_p50
  natra_mixed_hey_p99=$natra_mixed_p99
  natra_mixed_hey_total=$natra_mixed_total
  vanilla_mixed_iperf_ingress=$vanilla_mixed_iperf_ing
  vanilla_mixed_iperf_egress=$vanilla_mixed_iperf_eg
  vanilla_mixed_hey_rps=$vanilla_mixed_rps
  vanilla_mixed_hey_p50=$vanilla_mixed_p50
  vanilla_mixed_hey_p99=$vanilla_mixed_p99
  vanilla_mixed_hey_total=$vanilla_mixed_total

Generated by scripts/perf-vs-vanilla.sh on $(date -u +%Y-%m-%dT%H:%M:%SZ)
EOF
