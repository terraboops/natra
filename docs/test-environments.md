# Test environments — what each one actually validates

natra's correctness depends on three things being right: BPF logic
(token bucket math, CMS classification, hook composition), CNI
plumbing (binary install, conflist patch, runtime invocation), and
kernel/wire behavior (how the BPF program's decisions translate into
observed throughput at the receiver). The 5-layer test harness covers
the first two cleanly. The third is partial — what's measured today
is the behavior of the kernel and software wire we happen to be
running, which is not always the kernel and wire production cares
about.

This doc is the catalogue: what each environment covers, where the
coverage stops, and what the next environments would add.

## Current rig

### L1 — unit / fuzz / bench (`make test-unit`, `test-fuzz`, `test-bench`)

Pure Go. Tests the annotation parser, the install-cni-chain logic,
the config validator. No BPF, no kernel, no network. Catches regressions
in the parsing layer; doesn't say anything about runtime behavior.

### L2 — CNI integration (`make test-cni`)

Privileged Linux container with bpffs mounted. Drives `natra` from
Go test code through the CNI ADD/DEL contract, asserts the
expected BPF links / pin files exist after ADD and are gone after
DEL. Validates the CNI side end-to-end, including attach-mode
fallback, conflist patching, and pin cleanup.

What it doesn't test: actual traffic going through the BPF programs.

### L3 — BPF dataplane (`make test-bpf`)

`BPF_PROG_RUN` in a privileged Linux container. Feeds synthetic skbs
through the BPF programs and reads return codes + map state. Token
bucket math, CMS classification, ECN-set behavior, edge cases
(IPv4 options, oversize packets, ICMP). Validates the program in
isolation; doesn't say anything about TC hook composition or how
the kernel schedules the program against real traffic.

### L4 — e2e (`make test-e2e`)

`k3d` (k3s in Docker). Single LinuxKit VM (on Mac via colima) or
GH-runner kernel runs the full stack: kubelet, containerd, CNI
chain, the natra binary, the BPF programs, plus iperf3 client and
server pods. Asserts throughput is throttled within a calibrated
cap. Validates the integration up to "kubelet invokes natra and
traffic gets shaped."

Critical caveat: k3d's "nodes" are containers sharing one Linux
kernel. The k3d-natra-e2e-agent-0 "worker node" and the
k3d-natra-e2e-server-0 "control plane" are sibling containers on
the same docker bridge. Inter-node traffic crosses a software
bridge inside a single kernel; there is no wire, no remote NIC,
no real network-stack-to-network-stack handoff.

For natra this matters because:

- BPF attaches inside the pod netns at the pod veth, so the
  attachment itself is real. ✓
- The bucket and CMS execute on real packets with real skb->len,
  real skb->tstamp, real flow_keys. ✓
- The packets seen at the BPF hook are GRO-coalesced at whatever
  size the kernel chose, not at whatever the wire would have
  delivered. On a real NIC the kernel sees mtu-sized frames and
  GROs them locally; on a software bridge it sees whatever the
  sender put in. The bytes-counter doesn't care, but anything
  that reasons about packet count (legacy CMS, drop rate) is
  artificial.
- ECN-mark works (BPF sets CE in the IP header) and the receiver
  honors it, but there's no queue anywhere — software bridge
  doesn't drop on congestion. ECN-CE is observed by the receiver
  as a signal but it never corresponds to actual queueing.
- EDT pacing works (BPF stamps skb->tstamp, fq qdisc on pod-eth0
  honors it) and the receiver sees the paced arrival pattern.
  This is genuinely tested. ✓

### L5 — perf (`make test-perf` + `make perf-vs-vanilla`)

`BPF_PROG_RUN` with elephant + mice scenario traffic patterns (synthetic
in-kernel benchmarks), plus `scripts/perf-vs-vanilla.sh` which drives a
real iperf workload across three k3d clusters comparing baseline /
natra / upstream `bandwidth`. Same k3d kernel-sharing caveat as L4.

## What's not in the rig

The throughline of what's missing: anything that requires two
separate kernels. Specifically:

### Cross-kernel network stack

Two pods on two separate physical / virtual machines, with a real
network between them. Traffic exits one kernel through a real
egress NIC, traverses some kind of L2 / L3 fabric, enters the
other kernel via its NIC. This is what production looks like; it
is not what k3d looks like.

Symptoms that only appear here:

- NIC offloads (TSO, GRO, LRO, hardware TX timestamping). The BPF
  egress program sees GRO-coalesced super-packets on one side and
  the receiver's BPF ingress program sees segments at whatever
  size the NIC's LRO produced. The two sides have different
  packet shapes for the same byte stream.
- Real queueing. Switches drop on congestion, routers drop on
  TTL/MTU. ECN-CE means something. Drops re-amplify into TCP
  retransmits. Bystander cost from softirq contention on real
  hardware is measurable; on a software bridge inside one kernel
  it's the same shared softirq either way.
- Real latency. Bridge hops are sub-µs. Real wire latency on a
  rack switch is ~10s of µs; cross-region cloud is ms. Bucket
  refill math, EDT pacing scheduler precision, and HH-threshold
  fast-pass behavior are all sensitive to RTT.
- Kernel-version skew. Two nodes on different kernels (legitimate
  in a rolling-upgrade cluster) expose BPF feature drift —
  kernel A supports tcx, kernel B falls back to clsact. The rig
  uses one kernel; this code path is exercised by L2's
  table-driven attach-mode tests but not under live traffic.

### Real CPU contention under load

The bystander column in the mixed workload measures the cost natra
imposes on an unannotated pod sharing a node with an annotated
elephant. Real-world bystander cost includes:

- NIC interrupt coalescing competing for the same CPU as
  ksoftirqd processing the elephant's packets
- L2/L3 cache eviction from BPF map state plus kernel network
  state plus userspace nginx workset
- Scheduler fairness under genuine CPU pressure

On a single-kernel rig everything shares one ksoftirqd context but
also one set of caches and no real NIC. The numbers are directional
("does natra add cost?") but not absolute ("is 43ms p99 acceptable
production behavior?").

### Above-1Gbps rates

The host-gw flannel topology on Mac/colima is software-bridged; on
GH runners it's whatever the runner kernel's bridge does without
hardware offload. Single-stream iperf3 peaks around 1 Gbps on this
class of setup. The new `RATE_SWEEP=10M 1G 10G` exercise plugs in
the annotations but doesn't actually push 10G traffic. Validation
of "natra correctly throttles a 10G annotated pod" requires real
metal capable of producing >1 Gbps single-stream.

### Real CNI composition

natra coexists with cilium, aws-network-policy-agent
(`aws-network-policy-agent`, aka AWS NPA), and other tc/TCX-hook
users via `bpf_mprog` on kernel 6.6+. The vm-rig
(`make perf-vs-vanilla-vm`) now runs cilium as its CNI on every
run, exercising the composition path end-to-end on two real
kernels.

**Cilium proxies for the BPF-NPA class.** AWS NPA is the
production case we can't run locally (needs EKS); cilium with
its BPF dataplane stands in for it, because the host-side
veth + `bpf_mprog` coexistence story is the same regardless of
which CNI is the other side of the hook. What the vm-rig's
cilium run shows (current finding: host-side bypass with
kube-proxy-replacement, fixed by `NATRA_ATTACH_MODE=tcx-hostside`
— see `docs/perf-vs-vanilla.md`) inherits to AWS NPA without
further measurement.

What the local rig still doesn't measure: NPA-specific policy
ordering semantics, EKS-CNI's secondary-ENI interactions, and
any cilium-or-NPA configuration we don't have a flag for in the
vm-rig. Those land at "real cloud cluster" cost.

## What we'd add, ranked

Roughly in order of incremental value vs. setup pain:

### 1. Multi-VM cluster on a single host — **vm-rig (lima)**

Same physical machine, but each k8s node is its own VM with its
own kernel and its own (virtual) NIC. Validates kernel-to-kernel
plumbing without committing to multi-host infra. Catches: NIC
offload skew, real veth/bridge handoff, cross-kernel BPF version
mismatch, real network namespaces, MTU.

**Status: built and working, end-to-end.** Orchestration lives
in `cmd/vm-rig/` (Go, subcommands
`up / install / test / down / all / perfvsvanilla`); lima VM
templates live in `scripts/vm-rig/*.yaml`. The rig brings up two
VMs — server (control-plane) and agent (worker), each on its own
Linux kernel — joined over the lima `shared` (socket_vmnet)
network. `make test-vm` runs the natra throttle + CMS fast-pass
assertions across the kernel boundary; `make perf-vs-vanilla-vm`
runs the baseline/natra/upstream-bandwidth comparison (fresh
cluster per phase). Both pass.

The cross-VM path took a substantial debugging cascade to get
right; the resolution is **static lima0 IPs via a networkd
drop-in (no vmnet DHCP) plus `--flannel-iface=lima0`** — see
`scripts/vm-rig/README.md` "Network architecture". The earlier
"cross-VM ARP for static IPs is dropped by vmnet" diagnosis was
disproved; ARP/ping always worked, flannel was selecting the
wrong interface.

Invoke with `make test-vm`. macOS prerequisite:
`brew install socket_vmnet` for VM-to-VM L2 reachability; Linux
uses libvirt/KVM bridged networks directly.

See `scripts/vm-rig/README.md` for the layout, kernel-version
override path (swap the `images:` block in either
`lima-server.yaml` / `lima-agent.yaml` to test e.g. clsact on 5.x
against tcx on 6.6+), and known limits (the underlying transport
is software vmnet, not hardware NICs).

### 2. Multi-host cluster on cloud VMs

Two or three EC2 / GCE / Hetzner VMs joined into a k8s cluster.
Each VM has its own kernel and its own real (cloud-virtual) NIC;
traffic crosses the cloud provider's underlay. Adds: real wire
latency (single-digit ms within an AZ), cloud-NIC TSO/GRO
behavior, real queueing at the underlay layer (rare but happens
under congestion), real cross-AZ behavior if you span AZs.

Cost: a small standing pool (3 × t3.medium) is ~$30/month. Tear
down between runs to control cost; the bring-up is ~5 min via
kubeadm or k3sup.

Where this catches things L4 can't: cilium + natra interop on
real kernels, cilium's chained-mode behavior with natra's
mprog-style attach, multi-tenant elephant-isolates-bystander on
nodes where the bystander has its own NIC interrupts.

The vm-rig (`make perf-vs-vanilla-vm`) closes most of these on
two real kernels locally — including the cilium interop case,
which itself proxies for the broader BPF-NPA class (AWS NPA in
particular). Cloud VMs add real underlay + real NIC, but the
BPF-coexistence shape is the vm-rig's signal.

### 3. Bare metal

Two or three real Linux machines on a real switch. This is the
ground truth for natra. NIC offloads behave like the real world,
the switch can actually queue, ECN-CE means something, hardware
TX timestamping is available, single-stream throughput is
limited by the actual hardware (10G NICs are commodity).

Setup pain is high: physical infra, OOB management, switch
config. Probably worth one canonical reference rig per major
release rather than a per-PR check.

### 4. Production-shaped clusters

Real EKS / GKE / AKS clusters with cilium installed, natra
chained, real customer-shaped workloads (not iperf). This is
where we'd discover the failure modes that don't show up in any
synthetic test — pod density issues, churn behavior under
deploy rollouts, real CPU sharing under HPA.

Probably ad-hoc for now (run on a staging cluster when needed)
rather than automated.

## Open questions

A few I'd want the user's read on before sketching a plan:

1. **Local vs CI split.** Is the goal that local dev still runs k3d
   (fast, no infra) and a separate "validation" pass runs on
   real infra periodically, or that we move the full rig onto
   real infra and accept the slowdown? I'd lean toward the former
   — k3d catches most regressions, the validation pass catches
   the rest — but it means caring which environment caught what.
2. **Multi-VM-on-host vs cloud-VMs.** Stepping up from k3d, what's
   easier to get to in the next month: vagrant+libvirt on a
   Linux dev box, or a small cloud quota? Multi-VM-on-host is
   cheaper-per-run; cloud-VMs are closer to production.
3. **Failure mode targeting.** Which of the missing-coverage
   categories above is the one we're actually worried about
   breaking? "Above-1Gbps rates" is exciting but maybe not load-
   bearing for the workloads we care about; "real NIC offload
   skew" is unglamorous but is the one that would silently
   break in prod first.
4. **Metal timing.** "Eventually I'd like to get real hardware
   but that takes time" — meaning months, or meaning "next time
   I have a spare weekend"? Affects whether bare metal is on the
   roadmap as a sprint goal or a longer-arc thing.
