# vm-rig

A two-VM Kubernetes cluster that gives natra a real kernel-to-kernel
test environment. Unlike the k3d-based L4 rig — where "nodes" are
containers in one shared Linux kernel — each VM here runs its own
Linux kernel, and inter-pod traffic between annotated pods crosses
a real virtual NIC pair via the lima shared network.

What this catches that k3d doesn't:

- Cross-kernel BPF behavior. The server VM and agent VM each load
  natra's BPF program into their own kernel; map state is per-VM.
- Real network-stack handoff. iperf packets leave one VM's NIC,
  cross the host-side bridge, enter the other VM's NIC, get
  GRO-coalesced by *that* kernel — exactly the shape a production
  cross-node packet takes.
- Kernel-version drift. Each VM can run a different kernel image
  (override `images:` in `lima-server.yaml` / `lima-agent.yaml`)
  to validate the attach-mode fallback chain end-to-end.

What it still doesn't cover: real NICs (the underlying transport
is software vmnet / KVM bridged, not hardware), real switch
queueing, real cross-AZ latency. See `docs/test-environments.md`
for what cloud-VM / metal would add on top.

## Prerequisites

- `limactl` (lima 1.0+). Mac: `brew install lima`. Linux:
  distro package or build from source.
- `socket_vmnet` on macOS for VM-to-VM networking:
  `brew install socket_vmnet` then `sudo brew services start
  socket_vmnet`. Lima will refuse to start the `shared` network
  without it. Linux doesn't need this — lima uses libvirt/KVM
  bridged networks directly.
- `kubectl`, `docker`, `jq` on PATH.
- ~6 GiB free disk per VM (Ubuntu 24.04 cloud image + k3s state +
  the natra container image).
- ~30 GiB free disk overall is comfortable headroom.

## Run

```bash
make test-vm
```

End-to-end: brings up both VMs (~2-3 min on first run, faster after
the cloud image is cached), joins them into a k3s cluster, builds
the natra image, imports it into each VM, applies the installer
DaemonSet, runs an iperf3 throttle assertion across the VM
boundary, and tears down.

Leave the VMs up for inspection:

```bash
NATRA_VM_KEEP=1 make test-vm
# ...inspect...
export KUBECONFIG=/tmp/natra-vm-rig.kubeconfig
kubectl get pods -A
bash scripts/vm-rig/down.sh   # when done
```

## Layout

- `lima-server.yaml` — VM template for the k3s server (control-plane).
- `lima-agent.yaml` — VM template for the k3s agent (worker).
- `up.sh` — start both VMs, join them, export kubeconfig.
- `install-natra.sh` — build natra image, import into both VMs,
  apply installer DaemonSet.
- `run-tests.sh` — run the ingress-throttle topology against the
  cluster.
- `down.sh` — stop + delete both VMs.
- `all.sh` — wire `up → install → run-tests` together with cleanup
  on exit (the Makefile entry point invokes this).

## Pinning a kernel version

The default base image is Ubuntu 24.04 LTS (kernel 6.8.x). To
exercise an older kernel — say, the clsact fallback path on 5.x —
replace the `images:` block in one of the YAMLs with a 22.04 or
20.04 cloud image, or any custom kernel image lima understands.
The agent and server can run *different* kernels independently.

## Known limits

- First-time VM bring-up downloads the Ubuntu cloud image (~600 MB
  per arch). Cached after that.
- On Apple Silicon with lima's `vmType: vz` (the default), the
  shared network still needs `socket_vmnet` — vz's own NAT doesn't
  expose VM-to-VM L2.
- Tests run from the host's kubectl against the agent VM's k3s
  API. If the lima shared network drops (rare), the test will see
  the API as unreachable; rerun `up.sh`.
