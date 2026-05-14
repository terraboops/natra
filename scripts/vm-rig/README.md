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

### Cross-VM pod traffic blocker (current)

On Debian 13 under lima's `shared` network, systemd-networkd's
DHCPv4 client doesn't complete on `lima0` — networkd has the
interface set to `DHCP=ipv4` but the client never logs an
attempt. The current workaround (`scripts/vm-rig/lima-*.yaml`
provision scripts) assigns static IPs `192.168.105.10` (server)
and `192.168.105.11` (agent).

The k3s control-plane join works fine with statics — it routes
via lima-usernet NAT, which doesn't depend on vmnet's ARP table.
But pod-to-pod traffic via flannel `host-gw` fails: macOS
`socket_vmnet` only learns ARP entries for IPs it assigned via
DHCP, so cross-VM ARP for the static addresses gets dropped, and
host-gw's "route pod CIDR via the other node's lima0 IP" never
resolves a next-hop.

Symptom: the `cmd/vm-rig/test.go` connectivity gate
(`waitForIperfConnect`) fails loudly instead of producing a
silent 0-bps PASS. The cluster is up, kubectl works, single-VM
pod traffic works — only cross-VM pod traffic is dead.

Paths to unblock (rough order of likelihood):

1. **Different distro.** Ubuntu 24.04's cloud-init + netplan
   stack DHCPs cleanly under lima where Debian/networkd
   doesn't. Simplest swap — change the `images:` block, drop the
   static-IP provisioning.
2. **flannel-VXLAN with explicit MTU.** Tunnels pod traffic over
   UDP, so cross-VM ARP for pod CIDRs becomes irrelevant. A
   previous attempt failed at "1/2 nodes Ready," almost certainly
   an MTU mismatch (VXLAN adds 50 bytes of overhead; pod MTU
   needs to be 1450 on a 1500-byte underlay).
3. **Run vm-rig on Linux, not macOS.** lima's macOS networking
   quirks evaporate — libvirt/KVM bridged gives real L2. A
   small Linux VM accessed via SSH ends up simpler than fighting
   socket_vmnet on the developer's Mac.

## Planned direction

The vm-rig is the long-term shape for `make perf-vs-vanilla` on
developer machines. The current `scripts/perf-vs-vanilla.sh` uses
k3d (containers as nodes, one shared colima kernel) and will stay
the path for CI runs. Once cross-VM connectivity is unblocked,
`cmd/vm-rig` will gain a `perf-vs-vanilla` subcommand that drives
the same three-phase (baseline / natra / vanilla) comparison
across real two-kernel pods, and `make perf-vs-vanilla` will
dispatch to vm-rig on macOS / k3d in CI.
