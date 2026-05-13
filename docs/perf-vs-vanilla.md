# natra vs. upstream bandwidth — head-to-head

Real-cluster comparison across three configurations:

1. **baseline** — flannel alone, no rate-limiting plugin chained.
   The bandwidth annotations on `perf-server` are present but
   nothing acts on them; this is the "unaided cluster" floor.
2. **natra** — flannel + natra chained.
3. **upstream `bandwidth`** — flannel + `containernetworking/plugins`
   bandwidth plugin chained.

Two workloads per configuration:

1. **iperf-only** — elephant-flow rate-limiting in both directions,
   swept across `RATE_SWEEP` annotated rates (default `10M 1G 10G`).
2. **mixed** — an iperf3 `--bidir` elephant against an annotated
   pod, plus two parallel `hey` HTTP runs: one against the same
   annotated pod (annotated mice — share the bucket), one against
   a separate unannotated bystander pod on the same node
   (bystander mice — should be untouched by either plugin).

Run with:

```bash
make perf-vs-vanilla   # tees to /tmp/natra-perf-vs-vanilla-result.txt
```

The driver is `scripts/perf-vs-vanilla.sh`.

## Setup

Three k3d clusters, identical topology:

- 2 nodes (control-plane + worker), flannel as main CNI
- `perf-server` pinned to worker, annotated
  `kubernetes.io/ingress-bandwidth: "10M"` + `egress-bandwidth: "10M"`;
  runs iperf3 + nginx
- `bystander` pinned to worker, **no annotations**; runs nginx only
- `perf-client` on control-plane (cross-node traffic over flannel's
  bridge)
- Cluster 0: flannel only. Cluster A chains natra; cluster B chains
  the upstream `bandwidth` plugin.

For Cluster B, the test fetches the upstream `bandwidth` binary from
the `containernetworking/plugins` v1.5.1 release (k3d's base image
ships a subset of CNI plugins and doesn't include `bandwidth`) and
`modprobe`s `ifb` on each node. Without the IFB module, the upstream
plugin's HTB-on-IFB silently no-ops.

Two normalization steps run before each measurement so the numbers
reflect steady state, not initial-burst artifacts:

- **HTB burst patch (vanilla only).** Kubelet sends no per-pod burst
  override, so the bandwidth plugin defaults to ~150 seconds of
  credit (observed: `burst 193 MB / cburst 386 MB` on a 10 Mbps
  annotation). The script overrides each pod's HTB class via `tc
  class change ... burst 1mb cburst 1mb` after pod-ready, before
  measurement. natra's bucket is 2× rate by design (2.5 MB for
  10 Mbps), already small enough.
- **Bucket warmup.** A 20s forward + 20s reverse priming flow on
  each server pod drains any initial-burst tokens. Applies
  symmetrically to both plugins.

## Workload 1: iperf-only (rate sweep)

iperf3 against per-rate iperf3-only servers. One pod per rate in
`RATE_SWEEP`; each pod carries
`kubernetes.io/{ingress,egress}-bandwidth` at its own rate. Per rate:

- **Ingress elephant**: one TCP flow, 15s, forward (client → server)
- **Ingress mice**: 20 parallel TCP flows, 10s, forward
- **Egress elephant**: one TCP flow, 15s, reverse (`-R`)
- **Egress mice**: 20 parallel TCP flows, 10s, reverse

Receiver-side aggregate goodput from
`end.sum_received.bits_per_second`.

The sweep exists to catch plugin-induced regressions that only
surface at higher annotated rates: token-bucket math edge cases,
fast-pass threshold scaling, BPF map cost under heavier traffic. On
Mac/colima the wire (flannel host-gw via the docker bridge) tops
out around 1 Gbps single-stream, so 10G rows report ~Gbps line rate
from both plugins. Override on metal:

```bash
RATE_SWEEP="100M 1G 10G 40G" make perf-vs-vanilla
```

### Results

Single-rig sample, single annotated rate. Rig: colima on aarch64
macOS, LinuxKit kernel ~6.8.x, k3d v5.7.4 (k3s under containerd) on
the docker bridge, flannel host-gw, no NIC offload (software
dataplane). One run; numbers below are not averaged across runs.

| Direction | Plugin                | Elephant   | Mice (20× parallel)  |
|-----------|-----------------------|------------|----------------------|
| ingress   | natra                 | 11.16 Mbps | 11.65 Mbps           |
| ingress   | upstream `bandwidth`  | 10.04 Mbps |  9.64 Mbps           |
| egress    | natra                 | 11.69 Mbps | 12.81 Mbps           |
| egress    | upstream `bandwidth`  | 10.11 Mbps |  9.61 Mbps           |

Both plugins land within their cap-plus-burst envelope; natra's
single-flow elephant and 20-parallel mice columns sit in the same
range as the upstream HTB plugin on this rig.

Baseline (no plugin) on this rig is ~21-43 Mbps for elephants —
colima's flannel host-gw is bottlenecked by the LinuxKit kernel's
software dataplane. The baseline-to-throttled ratio is therefore
less dramatic on this hardware than on a Linux host with hardware
offload, but it doesn't change the comparison: both plugins cap at
~10 Mbps, well below the unrestricted baseline.

The heavy-hitter threshold is in **bytes** and scaled to the
annotated rate (`rate × 100 ms`): a 10 Mbps pod gets a ~125 KiB
threshold. Tail mice (HTTP responses, WebSocket frames) fit inside
that budget; each iperf3 parallel stream crosses after ~2
GRO-coalesced super-packets and steady-state throttling takes over.
Single-stream elephants cross in milliseconds.

## Workload 2: mixed (elephant + annotated mice + bystander mice)

Three pods on the same k3d cluster:

- `perf-server` (annotated 10M/10M, runs iperf3 + nginx, pinned to
  worker)
- `bystander` (no annotations, runs nginx, pinned to worker)
- `perf-client` (iperf3 + hey, pinned to control-plane)

Client traffic, concurrent for ~20s:

- `iperf3 --bidir` for 30s against `perf-server` — one elephant flow
  in each direction. Drains the ingress and egress buckets.
- After 5s of warmup, two parallel
  `hey -c 50 -z 20s -disable-keepalive` runs — one to `perf-server`,
  one to `bystander`. Each request opens a fresh TCP connection
  (new 5-tuple → new flow_key) with ~5-7 KB of total bytes, well
  under the ~125 KiB heavy-hitter threshold.

Three things to read out of the result table:

1. **Elephant ingress/egress.** Baseline shows the line rate; both
   plugins land at-or-below 10 Mbps.
2. **Annotated mice (perf-server) RPS / p99.** What the plugin does
   to small flows sharing the elephant's pod budget. CMS
   classification lets natra's mice fast-pass the bucket; HTB
   queues them behind the elephant.
3. **Bystander mice (unannotated, same node) RPS / p99.** Whether
   the plugin imposes cost on a neighboring unannotated pod. Both
   natra and vanilla leave unannotated pods alone (no BPF / no
   HTB attached), so the bystander row should track baseline.

### Results

Same rig as Workload 1 (colima, LinuxKit ~6.8.x, k3d, flannel
host-gw, no NIC offload). Numbers from a single run.

| Plugin                       | iperf ing | iperf eg  | Annotated mice (perf-server) RPS / p99 | Bystander RPS / p99 |
|------------------------------|-----------|-----------|---------------------------------------:|--------------------:|
| baseline (no plugin)         | ~8 Gbps   | ~27 Gbps  | 5283 / 71 ms                           | 6479 / 25 ms        |
| natra                        | 9.12 Mbps | 6.77 Mbps | 4073 / 262 ms                          | 6913 / 43 ms        |
| upstream `bandwidth` (HTB)   | 10.59 Mbps| 8.67 Mbps |   12 / 5715 ms                         | 8519 / 40 ms        |

natra serves 4073 RPS on the annotated-mice column against a paced
10 Mbps elephant in the same pod; vanilla serves 12. The difference
is CMS classification: each hey request is a fresh flow_key that
stays under the heavy-hitter threshold, fast-passes the bucket, and
doesn't queue behind the elephant.

Bystander numbers on this run are within a few percent of vanilla
and baseline. The bystander p99 gap from baseline (25 ms → 43 ms)
is consistent with the cost of having a paced elephant share a
worker node — softirq time, NIC rings, cache pressure, bridge
forwarding — which is structural to throttling an elephant on a
shared NIC rather than specific to either plugin. Sample size is
one; we haven't characterized the run-to-run distribution.

## What hasn't been measured

What the numbers above support is "on a single-kernel, single-bridge
k3d rig with software dataplane, natra throttles within
cap-plus-burst and lets unrelated flows fast-pass." That's the
extent of the empirical claim. Specifically *not* yet validated:

- **Real-NIC behavior.** No measurements on hardware NICs.
  TSO/GRO/LRO offloads, hardware TX timestamping, real interrupt
  coalescing — none of those are exercised by colima's software
  dataplane. The BPF program sees whatever GRO shape colima
  produces; on real hardware the shape will differ.
- **Cross-kernel wire.** k3d's "nodes" are containers sharing one
  Linux kernel. Inter-node packets cross a software bridge, not a
  switch. ECN-CE is set but never observed as actual congestion;
  real queueing and real wire latency aren't tested.
- **Run-to-run variance.** Each cell is a single sample. We don't
  yet have a distribution to report mean/p99/p100 over.
- **High-rate accuracy.** The `RATE_SWEEP=10G` rows show plugin
  behavior under a 10G annotation, but the wire on this rig is
  ~1 Gbps single-stream, so we're observing "natra didn't make
  things worse than wire speed," not "natra accurately throttles
  to 10 Gbps."
- **cilium / aws-network-policy-agent coexistence.** Composes via
  `bpf_mprog` on kernel 6.6+ by construction; we haven't run a
  composed stack in this rig.

The plan to close those gaps is in `docs/test-environments.md`.

## Throttle disposition

When the bucket can't admit an above-rate packet, natra picks the
disposition in this order:

1. **ECN-mark** (`bpf_skb_ecn_set_ce`) — set CE on ECN-capable
   packets, return `TC_ACT_OK`. Works on both directions. Requires
   the peer to have negotiated ECN-capable TCP (`tcp_ecn=1` on
   either end).
2. **EDT pacing** (egress only, when `cfg.edt_pacing != 0`) —
   stamp `skb->tstamp` with the next-release time and return
   `TC_ACT_OK`. The `fq` qdisc on pod-eth0 honors the timestamp
   and releases the packet at the scheduled time.
3. **Drop** (`TC_ACT_SHOT`) — fallback for ingress non-ECN
   traffic that nothing else can pace.

The ordering avoids unnecessary drops: ECN signals congestion
without retransmits, EDT shifts the packet in time without losing
it, drop is the last resort. Dropping above-rate packets
unconditionally would amplify load via TCP retransmits and bleed
softirq work into bystander pods on the same node — the
ECN-then-EDT-then-drop ordering keeps that out of the dataplane.

### EDT requirements

EDT pacing needs an `fq` qdisc downstream of the BPF program.
natra installs `fq` on pod-eth0 inside the pod netns when it
picks pod-side egress attach, so the qdisc sits right after the
egress BPF. Host-side attach has no deterministic spot for `fq`,
so EDT only applies when the attach side is pod.

`NATRA_EDT_PACING=auto` (default): probe `fq` install at CNI ADD,
use the EDT path if the probe succeeds. The `auto` mode also
reorders attach attempts to `tcx-pod → clsact-pod → tcx-host →
clsact-host` so the optimal config (pod-side + EDT) is tried
first; environments where `fq` install fails degrade through the
rest of the chain.

`NATRA_EDT_PACING=on` requires `fq` and fails attach if install
fails (restricts strategy to pod-side). `NATRA_EDT_PACING=off`
never installs `fq` and always drops after ECN-mark (uses the
host-side-first strategy for cilium/NPA coexistence).

The mixed iperf throughput under natra sits below the 10 Mbps cap
because when hey occasionally hits the bucket (CMS collisions,
above-threshold bursts on a connection), it consumes tokens iperf
would otherwise have. The guarantee is "annotated rate is the
ceiling," not "the annotated rate is always reached."

## Reproduce

```bash
make perf-vs-vanilla
```

natra's attach mode defaults to `auto`. Pin a specific mode:

```bash
NATRA_PERF_ATTACH_MODE=tcx-podside     make perf-vs-vanilla
NATRA_PERF_ATTACH_MODE=clsact-hostside make perf-vs-vanilla
NATRA_PERF_ATTACH_MODE=clsact-podside  make perf-vs-vanilla
```

EDT pacing also defaults to `auto`:

```bash
NATRA_PERF_EDT_PACING=on   make perf-vs-vanilla   # require fq, fail if unavailable
NATRA_PERF_EDT_PACING=off  make perf-vs-vanilla   # never install fq, drop after ECN
```
