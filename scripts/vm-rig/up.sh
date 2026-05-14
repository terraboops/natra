#!/usr/bin/env bash
# Bring up the two-VM k3s cluster: separate Linux kernels for the
# server (control-plane) and the agent (worker), joined into one
# k3s cluster over the lima `shared` network. Idempotent: re-running
# against existing VMs is a no-op modulo the "already running"
# message lima prints.
#
# Required tooling: limactl (lima 1.0+), kubectl. On macOS the
# shared network needs socket_vmnet (`brew install socket_vmnet`)
# unless you've set up vz-bridged differently.

set -euo pipefail

REPO_ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
RIG_DIR="${REPO_ROOT}/scripts/vm-rig"

SERVER_NAME="natra-server"
AGENT_NAME="natra-agent"

KUBECONFIG_OUT="${KUBECONFIG_OUT:-/tmp/natra-vm-rig.kubeconfig}"

# socket_vmnet check on macOS — without it the lima shared network
# won't actually let the two VMs see each other, and the agent's
# k3s join will time out. Skip the check on Linux (lima handles
# bridged networking through libvirt/QEMU directly).
if [ "$(uname -s)" = "Darwin" ]; then
    if ! [ -e "/opt/homebrew/var/run/socket_vmnet" ] && ! [ -e "/usr/local/var/run/socket_vmnet" ]; then
        cat <<'EOF' >&2
natra vm-rig: socket_vmnet is required for VM-to-VM networking on macOS.
Install: brew install socket_vmnet
Then enable: sudo brew services start socket_vmnet
(Or follow https://lima-vm.io/docs/config/network/vmnet/ for manual setup.)
EOF
        exit 1
    fi
fi

require() {
    if ! command -v "$1" >/dev/null 2>&1; then
        echo "natra vm-rig: $1 not on PATH" >&2
        exit 1
    fi
}

require limactl
require kubectl

# Stage 1: server VM. lima caches the cloud image on first run.
echo "==> bringing up $SERVER_NAME"
if ! limactl list "$SERVER_NAME" --format '{{.Name}}' 2>/dev/null | grep -qx "$SERVER_NAME"; then
    limactl create --name "$SERVER_NAME" "$RIG_DIR/lima-server.yaml"
fi
limactl start "$SERVER_NAME"

# Wait for the k3s server's node-token to land — provision-script
# completion isn't strictly observable from outside the VM, but the
# token file is the canonical marker that the install script
# finished.
echo "==> waiting for k3s server to finish provisioning"
for i in $(seq 1 90); do
    if limactl shell "$SERVER_NAME" -- test -f /etc/natra-node-token 2>/dev/null; then
        break
    fi
    sleep 2
    if [ "$i" = 90 ]; then
        echo "natra vm-rig: k3s server provisioning timed out" >&2
        limactl shell "$SERVER_NAME" -- journalctl -u k3s --no-pager | tail -30 >&2 || true
        exit 1
    fi
done

NATRA_K3S_TOKEN="$(limactl shell "$SERVER_NAME" -- cat /etc/natra-node-token)"
NATRA_SERVER_IP="$(limactl shell "$SERVER_NAME" -- cat /etc/natra-server-ip)"
echo "==> server up at ${NATRA_SERVER_IP}, token captured (${#NATRA_K3S_TOKEN} chars)"

# Stage 2: agent VM. Pass the join URL + token via lima's --set
# (overrides .env block in the template); the agent's provision
# script consumes them.
echo "==> bringing up $AGENT_NAME"
if ! limactl list "$AGENT_NAME" --format '{{.Name}}' 2>/dev/null | grep -qx "$AGENT_NAME"; then
    limactl create --name "$AGENT_NAME" \
        --set ".env.NATRA_K3S_URL = \"https://${NATRA_SERVER_IP}:6443\"" \
        --set ".env.NATRA_K3S_TOKEN = \"${NATRA_K3S_TOKEN}\"" \
        "$RIG_DIR/lima-agent.yaml"
fi
limactl start "$AGENT_NAME"

# Stage 3: export kubeconfig for the host. k3s writes one at
# /etc/rancher/k3s/k3s.yaml inside the server VM with server URL
# 127.0.0.1:6443 — rewrite to the lima-shared IP so host kubectl
# can reach the API.
echo "==> exporting kubeconfig to $KUBECONFIG_OUT"
limactl shell "$SERVER_NAME" -- sudo cat /etc/rancher/k3s/k3s.yaml \
    | sed "s|server: https://127.0.0.1:6443|server: https://${NATRA_SERVER_IP}:6443|" \
    > "$KUBECONFIG_OUT"

# Stage 4: wait for the agent to register as a Node.
echo "==> waiting for both nodes Ready"
for i in $(seq 1 60); do
    ready=$(KUBECONFIG="$KUBECONFIG_OUT" kubectl get nodes --no-headers 2>/dev/null \
        | awk '$2=="Ready"{n++} END{print n+0}')
    if [ "$ready" -ge 2 ]; then
        break
    fi
    sleep 2
    if [ "$i" = 60 ]; then
        echo "natra vm-rig: agent failed to register as Ready" >&2
        KUBECONFIG="$KUBECONFIG_OUT" kubectl get nodes >&2 || true
        exit 1
    fi
done

KUBECONFIG="$KUBECONFIG_OUT" kubectl get nodes -o wide
echo
echo "vm-rig up. Use it with: export KUBECONFIG=$KUBECONFIG_OUT"
