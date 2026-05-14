#!/usr/bin/env bash
# Build the natra container image on the host, push it into both
# lima VMs' k3s-containerd, then apply the installer DaemonSet.
# Assumes up.sh ran first and $KUBECONFIG_OUT is populated.

set -euo pipefail

REPO_ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
KUBECONFIG_OUT="${KUBECONFIG_OUT:-/tmp/natra-vm-rig.kubeconfig}"
NATRA_IMAGE="${NATRA_IMAGE:-ghcr.io/terraboops/natra:vm-rig}"
TARFILE="/tmp/natra-vm-rig.tar"

if [ ! -f "$KUBECONFIG_OUT" ]; then
    echo "natra vm-rig: $KUBECONFIG_OUT not found — run up.sh first" >&2
    exit 1
fi

echo "==> building natra image ($NATRA_IMAGE)"
docker build -q -t "$NATRA_IMAGE" \
    -f "${REPO_ROOT}/deploy/docker/Dockerfile.cni" "$REPO_ROOT" >/dev/null

echo "==> exporting image tarball"
docker save -o "$TARFILE" "$NATRA_IMAGE"

# Push to both VMs and import into k3s's embedded containerd. The
# k3s image store lives at /var/lib/rancher/k3s/agent/containerd/...
# but `k3s ctr` handles the namespace ("k8s.io") detail for us.
for vm in natra-server natra-agent; do
    echo "==> copying image to $vm"
    limactl copy "$TARFILE" "${vm}:/tmp/natra-vm-rig.tar"
    echo "==> importing image in $vm"
    limactl shell "$vm" -- sudo k3s ctr -n k8s.io images import /tmp/natra-vm-rig.tar
done

rm -f "$TARFILE"

# Apply the installer manifest. The shipped deploy/cni-installer.yaml
# points at ghcr.io/terraboops/natra:latest with imagePullPolicy:
# IfNotPresent; sed the image reference to the local tag and pin
# the policy to Never so k3s uses the imported copy instead of
# trying to pull. k3s paths for CNI bin / conflist replace the
# default kind-shaped ones.
echo "==> applying installer DaemonSet"
sed \
    -e "s|ghcr.io/terraboops/natra:latest|${NATRA_IMAGE}|" \
    -e "s|path: /etc/cni/net.d|path: /var/lib/rancher/k3s/agent/etc/cni/net.d|" \
    "${REPO_ROOT}/deploy/cni-installer.yaml" \
    | awk -v img="$NATRA_IMAGE" '
        $0 ~ "image: " img { print; getline; if ($1 == "imagePullPolicy:") sub(/IfNotPresent/, "Never"); print; next }
        { print }
    ' \
    | KUBECONFIG="$KUBECONFIG_OUT" kubectl apply -f -

echo "==> waiting for installer DaemonSet rollout"
KUBECONFIG="$KUBECONFIG_OUT" kubectl rollout status \
    daemonset/natra-installer -n kube-system --timeout=120s

echo "natra installed on the vm-rig cluster."
