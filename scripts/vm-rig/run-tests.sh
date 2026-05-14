#!/usr/bin/env bash
# Drive the standard ingress-throttle topology against the vm-rig
# cluster — same shape as the L4 e2e suite's Topology A, but here
# the iperf client and server land on separate VMs (separate
# kernels), so the traffic crosses a real virtual NIC pair, not a
# shared bridge in one kernel.
#
# Assertion: receiver-side throughput stays within +30% of the
# annotated rate. Matches the L4 e2e throttledCap floor and
# absorbs lima/vmnet jitter.

set -euo pipefail

REPO_ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
KUBECONFIG_OUT="${KUBECONFIG_OUT:-/tmp/natra-vm-rig.kubeconfig}"

export KUBECONFIG="$KUBECONFIG_OUT"

if [ ! -f "$KUBECONFIG_OUT" ]; then
    echo "natra vm-rig: $KUBECONFIG_OUT not found — run up.sh first" >&2
    exit 1
fi

# Namespace + manifest paths. Reuse the L4 e2e manifests so the
# annotation shape and pod definitions stay in lockstep.
NAMESPACE="natra-vm-rig"

# Pin iperf-server to the agent node, iperf-client to the server
# node, so traffic crosses the inter-VM virtual NIC. The L4 e2e
# manifests hardcode k3d node names — rewrite them to the lima VM
# hostnames at apply time.
SERVER_NODE="lima-natra-server"
WORKER_NODE="lima-natra-agent"

kubectl create namespace "$NAMESPACE" --dry-run=client -o yaml | kubectl apply -f -

echo "==> deploying iperf client + server"
for m in iperf-client.yaml iperf-server.yaml; do
    sed \
        -e "s|k3d-natra-e2e-agent-0|${WORKER_NODE}|" \
        -e "s|k3d-natra-e2e-server-0|${SERVER_NODE}|" \
        -e "s|namespace: natra-e2e|namespace: ${NAMESPACE}|" \
        "${REPO_ROOT}/test/e2e/manifests/$m" \
        | kubectl apply -f -
done

echo "==> waiting for iperf pods Ready"
kubectl wait --for=condition=Ready \
    pod/iperf-client pod/iperf-server \
    -n "$NAMESPACE" --timeout=120s

# Drain initial burst tokens. natra's bucket is 2× rate (2.5 MB
# at 10 Mbps); without a warmup the first measured second runs
# at line rate from the burst before the rate-limiter engages.
echo "==> warming up iperf-server (draining bucket)"
kubectl exec -n "$NAMESPACE" iperf-client -- \
    iperf3 -c iperf-server -t 20 -P 4 >/dev/null 2>&1 || true

# Measurement run. 15 seconds is enough to amortize the 2s burst
# below the +30% cap (rate × (1 + 2/15) ≈ 1.133× ≈ 11.3 Mbps for a
# 10 Mbps annotation).
echo "==> measuring throttled throughput"
RESULT_JSON=$(kubectl exec -n "$NAMESPACE" iperf-client -- \
    iperf3 -c iperf-server -t 15 -J 2>/dev/null)

BPS=$(echo "$RESULT_JSON" | jq '.end.sum_received.bits_per_second // 0')
MBPS=$(awk -v b="$BPS" 'BEGIN { printf "%.2f", b/1e6 }')

# 10 Mbps annotation × 1.30 slack = 13.00 Mbps cap. Same envelope
# the L4 throttle assertions use after the 2026-05 calibration
# work — see test/e2e/e2e_test.go.
RATE_BPS=10000000
CAP_BPS=$(awk -v r="$RATE_BPS" 'BEGIN { printf "%.0f", r * 1.30 }')
CAP_MBPS=$(awk -v c="$CAP_BPS" 'BEGIN { printf "%.2f", c/1e6 }')

echo
printf 'iperf-server (annotated 10 Mbps) on %s ← iperf-client on %s\n' "$WORKER_NODE" "$SERVER_NODE"
printf '  measured: %s Mbps\n' "$MBPS"
printf '  cap:      %s Mbps (rate × 1.30 slack)\n' "$CAP_MBPS"

if [ "$(awk -v b="$BPS" -v c="$CAP_BPS" 'BEGIN { print (b <= c) }')" = 1 ]; then
    echo "PASS: ingress throttled within cap on a real two-kernel cluster."
else
    echo "FAIL: measured throughput exceeds cap." >&2
    exit 1
fi
