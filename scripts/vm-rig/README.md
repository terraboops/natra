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
  `brew install socket_vmnet`. **Do NOT** `brew services start
  socket_vmnet` — lima 2.x starts and manages its own instance
  (via the sudoers entry `limactl` writes); a second
  brew-services instance is a pointless duplicate shared-mode
  vmnet network on the same gateway. (The rig uses static lima0
  IPs and no longer depends on vmnet's DHCP responder, so a
  stray duplicate is no longer the catastrophe it was in the
  DHCP era — see "Network architecture" below — but a single
  lima-managed instance is still the only supported config.)
  Older lima docs told you to `brew services start` it; that
  predates lima managing socket_vmnet itself. Linux doesn't need
  socket_vmnet — lima uses libvirt/KVM bridged networks directly.
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
  up             bring up the two-VM k3s cluster
  install        build and import the natra image, apply installer
  test           iperf throttle + hey HTTP-mice fast-pass assertions
  perfvsvanilla  baseline/natra/upstream-bandwidth comparison;
                 owns the VM lifecycle (fresh cluster per phase)
  down           tear down both VMs
  all            up + install + test (down on exit unless -keep)

Environment:
  NATRA_VM_KUBECONFIG   kubeconfig output path (default /tmp/natra-vm-rig.kubeconfig)
  NATRA_VM_IMAGE        natra image tag to build/use (default ghcr.io/terraboops/natra:vm-rig)
  NATRA_VM_KEEP=1       used by `all` to skip teardown on exit
```

`perfvsvanilla` is the local-developer counterpart of the k3d
`make perf-vs-vanilla` — same baseline/natra/upstream-bandwidth
comparison, but on two real kernels instead of containers sharing
one. It **owns the VM lifecycle**: each phase runs on its own
pristine cluster (full down → up → stage → measure → down), so
do not `vm-rig up` first.

Independent clusters per phase is a deliberate cost/correctness
trade. An earlier design swapped the shaper in place on one
shared cluster — cheaper, but it leaked warm page/containerd
cache, accumulated kernel networking state, and natra's
persistent BPF into later phases, so the last (warm) phase was
unfairly faster than the first (cold). That's fatal for the
per-phase *latency* numbers. Fresh-cluster-per-phase removes
every cross-phase confound (and dissolves the phase-ordering
constraint — phases are now independent) at the price of ~3x
bring-up (~40 min total on the static-IP architecture).

```
make perf-vs-vanilla-vm                 # owns lifecycle, no `up` first
# equivalently:
go run ./cmd/vm-rig perfvsvanilla
```

Output: a comparison table on stdout and at
`/tmp/natra-vm-rig-perf-vs-vanilla-result.txt`.

## Pinning a kernel version

The default base image is Debian 13 trixie genericcloud (kernel
6.12). To exercise an older kernel — say, the clsact fallback
path on 5.x — replace the `images:` block in one of the YAMLs
with any other cloud image lima understands (Debian 12, Ubuntu
20.04/22.04/24.04, Fedora cloud, etc.). The agent and server can
run *different* kernels independently.

The image was switched from Ubuntu to Debian because
`cloud-images.ubuntu.com` is DNS-blocked on some constrained
networks; `cloud.debian.org` is more widely mirrored.

## Known limits

- First-time VM bring-up downloads the cloud image (~600 MB per
  arch). Cached after that. lima 2.0+ requires `qemu-img` for
  qcow2→raw conversion under `vmType: vz` (`brew install qemu`).
- On Apple Silicon with lima's `vmType: vz` (the default), the
  shared network still needs `socket_vmnet` — vz's own NAT doesn't
  expose VM-to-VM L2.
- Tests run from the host's kubectl against the server VM's k3s
  API. If the lima shared network drops (rare), the test will see
  the API as unreachable; rerun `go run ./cmd/vm-rig up`.

### Network architecture (static lima0, no vmnet DHCP)

lima0 (the shared-network interface) is given a **static IP** —
server `192.168.105.10`, agent `192.168.105.11` — by a
networkd drop-in `/etc/systemd/network/05-natra-lima0.network`
(`DHCP=no`), written in the provision script. The `05-` prefix
sorts before lima's netplan-generated `10-netplan-lima0.network`,
so networkd owns lima0 statically from the start and never runs
a competing DHCP client on it. `--node-ip` is the static
address; k3s gets `--flannel-iface=lima0`.

Two pieces are load-bearing and non-obvious:

- **`--flannel-iface=lima0`** is the actual fix for cross-VM
  pod traffic. Without it flannel auto-selects its inter-node
  interface from the default route — which is correctly eth0
  (lima's per-VM-isolated user-mode NAT; its `192.168.5.15` is
  identical on every VM and goes nowhere cross-VM). flannel then
  installs `<other pod-cidr> via 192.168.5.15 dev eth0`, a black
  hole. Pinning lima0 makes host-gw use the real inter-VM wire.
- **Static, not DHCP.** lima's shared-network vmnet DHCP server
  is wildly unreliable on this macOS stack (observed convergence
  0 s → never across runs). Static is deterministic; it also
  installs no default route, so internet egress stays on eth0
  with zero extra work.

Two earlier theories were both disproved by direct test and are
recorded here so they aren't re-tried: (1) "macOS vmnet won't
ARP statically-assigned addresses, so cross-VM pod traffic
fails" — false; with static .10/.11, server↔agent ping is 0%
loss and ARP resolves both ways. (2) "DHCP is required for
cross-VM" — false; the real blocker was always the flannel
interface selection above, invisible until the cluster reliably
reached the flannel-routing stage. An intermediate fix that
made vmnet DHCP work (removing a stray brew-services
socket_vmnet that broke vmnet's DHCP responder) was superseded:
removing the DHCP dependency entirely is simpler and
deterministic.

## Two perf-vs-vanilla rigs

Both exist on purpose, different cost/fidelity trade-offs:

- **`make perf-vs-vanilla`** (`scripts/perf-vs-vanilla.sh`, k3d):
  containers as nodes, one shared colima kernel. Cheap, fast,
  runs in CI. Single-kernel — no real cross-kernel wire.
- **`make perf-vs-vanilla-vm`** (`cmd/vm-rig perfvsvanilla`):
  two lima VMs, two real kernels, real inter-VM vmnet wire, a
  fresh cluster per phase (~40 min). The high-fidelity local-dev
  rig. This is the cross-kernel measurement the k3d "Gaps in this comparison"
  note (`docs/perf-vs-vanilla.md`) called out as missing.

The k3d path stays the CI path (no nested virt on GH runners).
The vm-rig path is the developer-machine high-fidelity check.
