#!/usr/bin/env bash
# Tear down the vm-rig: stop and delete both lima VMs. Idempotent —
# missing VMs are not errors. Does NOT remove the cached cloud
# images (those persist in lima's image cache for fast re-up).

set -euo pipefail

for vm in natra-agent natra-server; do
    if limactl list "$vm" --format '{{.Name}}' 2>/dev/null | grep -qx "$vm"; then
        echo "==> stopping $vm"
        limactl stop --force "$vm" 2>/dev/null || true
        echo "==> deleting $vm"
        limactl delete --force "$vm"
    else
        echo "==> $vm not present, skipping"
    fi
done

rm -f /tmp/natra-vm-rig.kubeconfig
echo "vm-rig torn down."
