#!/usr/bin/env bash
# Real-cluster head-to-head: natra vs upstream containernetworking/plugins/bandwidth.
#
# Spins up two kind clusters in sequence (one with natra chained behind
# kindnet, one with the upstream bandwidth plugin chained behind
# kindnet), runs the same iperf elephant+mice workload in each
# direction, prints a comparison.
#
# Each cluster is exercised twice:
#   - ingress phase: server has kubernetes.io/ingress-bandwidth=10M;
#     iperf3 forward (client → server) measures the throttle on the
#     server's inbound traffic.
#   - egress phase: server has kubernetes.io/egress-bandwidth=10M;
#     iperf3 reverse (server → client via -R) measures the throttle on
#     the server's outbound traffic.
#
# Output: docs/perf-vs-vanilla-result.txt with the raw numbers.
#
# Run time: ~10-12 minutes. Docker required on macOS (colima or Docker
# Desktop).

set -euo pipefail

REPO_ROOT="$(cd "$(dirname "$0")/.." && pwd)"
RESULT_FILE="${REPO_ROOT}/docs/perf-vs-vanilla-result.txt"

NATRA_CLUSTER="natra-vs-vanilla-natra"
VANILLA_CLUSTER="natra-vs-vanilla-vanilla"
NATRA_IMAGE="ghcr.io/terraboops/natra:vsperf"

ELEPHANT_DURATION=15
MICE_PARALLEL=20
MICE_DURATION=10
RATE="10M"

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

# run_workload prints four integers on stdout:
#   ingress_elephant_bps ingress_mice_bps egress_elephant_bps egress_mice_bps
# Forward iperf3 measures throughput INTO the server (ingress); reverse
# (-R) measures throughput OUT of the server (egress).
run_workload() {
    local namespace="natra-e2e"

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

TMPDIR=$(mktemp -d)
trap 'rm -rf "$TMPDIR"; cleanup' EXIT

# Build the natra image once.
echo "==> building natra image: $NATRA_IMAGE"
docker build -q -t "$NATRA_IMAGE" -f "${REPO_ROOT}/deploy/docker/Dockerfile.cni" "$REPO_ROOT" >/dev/null

# ---- Phase A: natra ----
echo
echo "===================================================================="
echo "Phase A: natra"
echo "===================================================================="
mkdir -p "$TMPDIR/natra"
render_manifests "$NATRA_CLUSTER" "$TMPDIR/natra"

kind create cluster --name "$NATRA_CLUSTER" \
    --config "${REPO_ROOT}/test/e2e/kind-config.yaml" --wait 120s
kind load docker-image "$NATRA_IMAGE" --name "$NATRA_CLUSTER"

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
kubectl wait --for=condition=Ready pod/iperf-server pod/iperf-client \
    -n natra-e2e --timeout=120s

echo "==> running workload (phase A)"
read -r natra_ing_elephant natra_ing_mice natra_eg_elephant natra_eg_mice < <(run_workload)
echo "  natra ingress elephant=$natra_ing_elephant bps  mice=$natra_ing_mice bps"
echo "  natra egress  elephant=$natra_eg_elephant bps  mice=$natra_eg_mice bps"

kind delete cluster --name "$NATRA_CLUSTER"

# ---- Phase B: upstream bandwidth plugin ----
echo
echo "===================================================================="
echo "Phase B: upstream containernetworking/plugins/bandwidth"
echo "===================================================================="
mkdir -p "$TMPDIR/vanilla"
render_manifests "$VANILLA_CLUSTER" "$TMPDIR/vanilla"

kind create cluster --name "$VANILLA_CLUSTER" \
    --config "${REPO_ROOT}/test/e2e/kind-config.yaml" --wait 120s

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
kubectl wait --for=condition=Ready pod/iperf-server pod/iperf-client \
    -n natra-e2e --timeout=120s

echo "==> running workload (phase B)"
read -r vanilla_ing_elephant vanilla_ing_mice vanilla_eg_elephant vanilla_eg_mice < <(run_workload)
echo "  vanilla ingress elephant=$vanilla_ing_elephant bps  mice=$vanilla_ing_mice bps"
echo "  vanilla egress  elephant=$vanilla_eg_elephant bps  mice=$vanilla_eg_mice bps"

kind delete cluster --name "$VANILLA_CLUSTER"

# Format human-readable output.
fmt_bps() {
    awk -v b="$1" 'BEGIN {
        if (b == 0) { print "0.00 Mbps"; exit }
        printf "%.2f Mbps", b/1e6
    }'
}

cat <<EOF | tee "$RESULT_FILE"
natra vs upstream containernetworking/plugins/bandwidth — kind cluster head-to-head
====================================================================================
Workload: iperf3 over kindnet, ${RATE} bandwidth annotation in each direction
  - ingress: forward iperf3 (client → server, charged to server's ingress)
  - egress:  reverse iperf3 -R (server → client, charged to server's egress)
  - Phase 1: ${ELEPHANT_DURATION}s single elephant flow
  - Phase 2: ${MICE_DURATION}s × ${MICE_PARALLEL} parallel mice flows
Iperf goodput, receiver-side aggregate (sum_received.bits_per_second)

Direction  Plugin                          Elephant            Mice (${MICE_PARALLEL}× parallel)
-------------------------------------------------------------------------------------------
ingress    natra                           $(fmt_bps "$natra_ing_elephant")        $(fmt_bps "$natra_ing_mice")
ingress    upstream bandwidth              $(fmt_bps "$vanilla_ing_elephant")        $(fmt_bps "$vanilla_ing_mice")
egress     natra                           $(fmt_bps "$natra_eg_elephant")        $(fmt_bps "$natra_eg_mice")
egress     upstream bandwidth              $(fmt_bps "$vanilla_eg_elephant")        $(fmt_bps "$vanilla_eg_mice")

Raw numbers (bps):
  natra_ingress_elephant=$natra_ing_elephant
  natra_ingress_mice=$natra_ing_mice
  natra_egress_elephant=$natra_eg_elephant
  natra_egress_mice=$natra_eg_mice
  vanilla_ingress_elephant=$vanilla_ing_elephant
  vanilla_ingress_mice=$vanilla_ing_mice
  vanilla_egress_elephant=$vanilla_eg_elephant
  vanilla_egress_mice=$vanilla_eg_mice

Generated by scripts/perf-vs-vanilla.sh on $(date -u +%Y-%m-%dT%H:%M:%SZ)
EOF
