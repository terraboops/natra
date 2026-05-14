# vm-rig

A two-VM Kubernetes cluster that gives natra a real kernel-to-kernel
test environment. Unlike the k3d-based L4 rig — where "nodes" are
containers in one shared Linux kernel — each VM here runs its own
Linux kernel, and inter-pod traffic between annotated pods crosses
a real virtual NIC pair via the lima shared network.

The orchestration lives in Go at `cmd/vm-rig/` (subcommands:
`up`, `install`, `test`, `down`, `all`). The two files in this
directory are config templates the Go binary reads at runtime —
keeping them as YAML lets you tweak kernel image / CPU / memory /
networking without recompiling.

What this catches that k3d doesn't:

- Cross-kernel BPF behavior. The server VM and agent VM each load
  natra's BPF program into their own kernel; map state is per-VM.
- Real network-stack handoff. iperf packets leave one VM's NIC,
  cross the host-side bridge, enter the other VM's NIC, get
  GRO-coalesced by *that* kernel — exactly the shape a production
  cross-node packet takes.
- Kernel-version drift. Each VM can run a different kernel image
  (swap the `images:` block in either YAML) to validate the
  attach-mode fallback chain end-to-end.

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
- `kubectl`, `docker` on PATH.
- Go 1.25+ (to run `cmd/vm-rig`; the binary is built on demand by
  `go run`, no separate install step).
- ~6 GiB free disk per VM; ~30 GiB headroom overall is comfortable.

## Run

```bash
make test-vm
```

End-to-end: brings up both VMs (~2-3 min on first run, faster after
the cloud image is cached), joins them into a k3s cluster, builds
the natra + perfclient images, imports them into each VM, applies
the installer DaemonSet, then runs two assertions back-to-back
across the VM boundary:

- **iperf3 throttle (bidi)**: against a server annotated with both
  ingress and egress at 10 Mbps, two 15-second TCP elephants run
  back-to-back — forward (`iperf3 -c`) for ingress and reverse
  (`iperf3 -R`) for egress. Asserts the receiver-side bps stays
  within the 1.30× slack cap on *each* direction. Same shape as
  Topology C in the L4 e2e suite.
- **iperf3 throttle (egress-only)**: a second server pod annotated
  with `kubernetes.io/egress-bandwidth: "10M"` and no ingress
  annotation. iperf3 `-R` against it; asserts the egress direction
  throttles to within the slack cap. Catches a regression that
  attaches both programs regardless of which annotations are
  present (Topology B in the L4 e2e suite).
- **hey HTTP fast-pass**: 15 seconds of hey at -c 50
  -disable-keepalive against an annotated nginx target (same 10
  Mbps cap as the iperf elephant). Asserts RPS clears a generous
  floor (200 RPS) — natra's CMS classifier should let each fresh
  TCP connection fast-pass the bucket because its byte volume
  stays well under the heavy-hitter threshold.

Then tears the rig down.

Leave the VMs up for inspection:

```bash
NATRA_VM_KEEP=1 make test-vm
# ...or run the subcommands directly:
go run ./cmd/vm-rig up
export KUBECONFIG=/tmp/natra-vm-rig.kubeconfig
kubectl get pods -A
go run ./cmd/vm-rig down   # when done
```

## Files in this directory

- `lima-server.yaml` — VM template for the k3s server (control-plane).
  Cloud-init provisions k3s on first boot and writes the join token
  to `/etc/natra-node-token`.
- `lima-agent.yaml` — VM template for the k3s agent (worker). Reads
  `NATRA_K3S_URL` + `NATRA_K3S_TOKEN` from its env block; `cmd/vm-rig`
  renders a copy with those values inlined before `limactl create`.

## CLI

```text
vm-rig — natra kernel-isolated test rig (lima + k3s)

Subcommands:
  up        bring up the two-VM k3s cluster
  install   build and import the natra image, apply installer
  test      iperf throttle + hey HTTP-mice fast-pass assertions
  down      tear down both VMs
  all       up + install + test (down on exit unless -keep)

Environment:
  NATRA_VM_KUBECONFIG   kubeconfig output path (default /tmp/natra-vm-rig.kubeconfig)
  NATRA_VM_IMAGE        natra image tag to build/use (default ghcr.io/terraboops/natra:vm-rig)
  NATRA_VM_KEEP=1       used by `all` to skip teardown on exit
```

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
- Tests run from the host's kubectl against the server VM's k3s
  API. If the lima shared network drops (rare), the test will see
  the API as unreachable; rerun `go run ./cmd/vm-rig up`.
