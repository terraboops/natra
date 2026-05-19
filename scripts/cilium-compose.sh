#!/usr/bin/env bash
# cilium-compose: prove natra composes with cilium at the TCX hook.
#
# The "Gaps in this comparison" section of docs/perf-vs-vanilla.md
# claims natra coexists with cilium / AWS-NPA by construction —
# both attach at the TCX hook via bpf_mprog (kernel >= 6.6), so the
# kernel multiplexes them. This is asserted, not measured. This
# script measures it.
#
# Substrate: k3d on colima. "Composes at TCX via bpf_mprog" is a
# kernel + attach-mechanism property, NOT a cross-kernel-wire one,
# so the two-real-kernel vm-rig is unnecessary here — k3d's colima
# LinuxKit kernel (~6.12, >= the 6.6 bpf_mprog-TCX threshold) is
# sufficient and far cheaper. (Same reasoning the vm-rig arc
# taught: match the rig to the property under test.)
#
# Shape: one k3d cluster with k3s's flannel + kube-proxy disabled
# so cilium is the sole CNI/dataplane; install cilium (TCX mode);
# chain natra after it; then assert four things:
#
#   1. cilium is healthy and is the dataplane (cilium status).
#   2. natra's BPF actually attached on an annotated pod
#      (natra dump-stats shows a non-zero config slot).
#   3. BOTH programs are present at the pod's TCX hook
#      (bpftool net / tcx link list shows cilium AND natra) —
#      the literal bpf_mprog-coexistence proof.
#   4. natra still rate-limits with cilium present: an annotated
#      iperf3 elephant caps near 10 Mbps while fresh-connection
#      hey mice keep high RPS (CMS fast-pass still works).
#
# STATUS: scaffold — NOT yet verified end-to-end. The unknown is
# step "chain natra": natra's install-cni-chain was written
# against flannel's conflist; cilium's conflist shape differs and
# may need handling. That only shakes out against the real
# substrate; this file is the structured first attempt, mirrored
# on scripts/perf-vs-vanilla.sh's idioms, to iterate from.
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "$0")/.." && pwd)"
CLUSTER="natra-cilium-compose"
NATRA_IMAGE="ghcr.io/terraboops/natra:cilium-compose"
NS="natra-cilium"

cleanup() { k3d cluster delete "$CLUSTER" 2>/dev/null || true; }
trap cleanup EXIT

require() {
  for b in "$@"; do
    command -v "$b" >/dev/null 2>&1 || { echo "missing required tool: $b" >&2; exit 1; }
  done
}
require k3d kubectl docker helm

# --- 1. cluster with k3s flannel + kube-proxy + netpol disabled
# so cilium owns the dataplane. Nodes stay NotReady until cilium
# is up — expected; we wait for cilium, then nodes.
echo "==> creating k3d cluster (no flannel, no kube-proxy)"
k3d cluster create "$CLUSTER" \
  --agents 1 --no-lb \
  --k3s-arg "--disable=traefik,servicelb@server:*" \
  --k3s-arg "--flannel-backend=none@server:*" \
  --k3s-arg "--disable-network-policy@server:*" \
  --k3s-arg "--disable-kube-proxy@server:*" \
  --wait

# --- 2. install cilium as the CNI, TCX dataplane.
# cilium >= 1.16 on kernel >= 6.6 uses TCX for its tc programs;
# pin bpf.tcx=true so the coexistence assertion is meaningful even
# if the default ever changes. kube-proxy replacement because we
# disabled kube-proxy above.
echo "==> installing cilium (helm, TCX, kube-proxy replacement)"
helm repo add cilium https://helm.cilium.io >/dev/null 2>&1 || true
helm repo update >/dev/null
KUBE_API="$(kubectl config view -o jsonpath='{.clusters[0].cluster.server}' | sed -E 's#https?://##; s#:.*##')"
helm install cilium cilium/cilium --namespace kube-system \
  --set bpf.tcx=true \
  --set kubeProxyReplacement=true \
  --set k8sServiceHost="$KUBE_API" \
  --set k8sServicePort=6443 \
  --set operator.replicas=1
echo "==> waiting for cilium rollout"
kubectl -n kube-system rollout status ds/cilium --timeout=240s
kubectl wait --for=condition=Ready nodes --all --timeout=180s

# --- 3. chain natra after cilium.
# natra's installer appends its conflist entry to whatever main
# CNI conflist is present (here cilium's). This is the step most
# likely to need iteration — the patcher's flannel assumptions
# vs cilium's conflist shape.
echo "==> building + importing natra image"
docker build -q -t "$NATRA_IMAGE" \
  -f "${REPO_ROOT}/deploy/docker/Dockerfile.cni" "$REPO_ROOT" >/dev/null
k3d image import "$NATRA_IMAGE" --cluster "$CLUSTER"
echo "==> applying natra installer (chained after cilium)"
sed "s#ghcr.io/terraboops/natra:latest#${NATRA_IMAGE}#; \
     s#imagePullPolicy: IfNotPresent#imagePullPolicy: Never#" \
  "${REPO_ROOT}/deploy/cni-installer.yaml" \
  | kubectl apply -f -
kubectl -n kube-system rollout status ds/natra-installer --timeout=180s

# --- 4. workload + the four assertions
kubectl create namespace "$NS" --dry-run=client -o yaml | kubectl apply -f -
# (workload manifests + assertions filled in during verification —
#  reuse test/perf/realworld/perf-server.yaml + perf-client.yaml,
#  the natra dump-stats sandbox-id recipe from docs/troubleshooting,
#  and `kubectl exec ... bpftool net` for the TCX coexistence check.)
echo "SCAFFOLD: cluster + cilium + natra stood up; assertions pending verification."
echo "Next: deploy annotated perf-server/perf-client, assert (1) cilium status,"
echo "(2) natra dump-stats non-zero, (3) bpftool net shows cilium+natra at TCX,"
echo "(4) iperf elephant ~10M while hey mice keep RPS."
