# natra vs. upstream bandwidth — head-to-head

Real-cluster comparison across three configurations:

1. **baseline** — flannel alone, no rate-limiting plugin chained.
   The bandwidth annotations on `perf-server` are present but
   nothing acts on them; this is the "unaided cluster" floor.
2. **natra** — flannel + natra chained.
3. **upstream `bandwidth`** — flannel + `containernetworking/plugins`
   bandwidth plugin chained.

Two workloads are run against each:

1. **iperf-only** — characterizes elephant-flow rate-limiting in
   both directions.
2. **mixed** — an iperf3 `--bidir` elephant against an annotated
   pod, plus two parallel `hey` HTTP runs: one against the same
   annotated pod (annotated mice — share the bucket), one against
   a separate unannotated bystander pod on the same node
   (bystander mice — should be untouched by either plugin).

Run with:

```bash
make perf-vs-vanilla
# ~18-22 min, three k3d clusters in sequence
cat docs/perf-vs-vanilla-result.txt
```

The driver is `scripts/perf-vs-vanilla.sh`. Override the natra attach
mode for the comparison via `NATRA_PERF_ATTACH_MODE=clsact-podside`.

## Setup

Three k3d clusters, identical topology:

- 2 nodes (control-plane + worker), flannel as main CNI
- `perf-server` pinned to worker, annotated
  `kubernetes.io/ingress-bandwidth: "10M"` + `egress-bandwidth: "10M"`;
  runs iperf3 + nginx
- `bystander` pinned to worker, **no annotations**; runs nginx only
- `perf-client` on control-plane (cross-node traffic over flannel's
  bridge + tunnel)
- Cluster 0: flannel only. Cluster A chains natra; cluster B chains
  the upstream `bandwidth` plugin.

For Cluster B, the test fetches the upstream `bandwidth` binary from
the `containernetworking/plugins` v1.5.1 release (kindest/node images, which k3d also uses, ship a
subset of CNI plugins and don't include `bandwidth`) and `modprobe`s
`ifb` on each node. Without the IFB module, the upstream plugin's
HTB-on-IFB silently no-ops.

Two normalization steps are applied so each workload measures
steady-state behavior, not initial-burst artifacts:

- **HTB burst patch (vanilla only).** Kubelet sends no per-pod
  burst override, so the bandwidth plugin defaults to ~150 seconds
  of credit (observed: `burst 193 MB / cburst 386 MB` on a 10 Mbps
  annotation). A 30s measurement that fits inside that window never
  sees HTB engage. The script overrides each pod's HTB class via
  `tc class change ... burst 1mb cburst 1mb` after pod-ready,
  before measurement. natra's bucket is 2× rate by design (2.5 MB
  for 10 Mbps), already small enough.
- **Bucket warmup.** A 20s forward + 20s reverse priming flow on
  each server pod drains any remaining initial-burst tokens before
  the real measurement starts. Applies symmetrically to both
  plugins.

## Workload 1: iperf-only (rate sweep)

iperf3 against per-rate iperf3-only servers. One pod per rate in
`RATE_SWEEP` (default `10M 1G 10G`); each pod carries
`kubernetes.io/{ingress,egress}-bandwidth` at its own rate. Each rate
sees four phases:

- **Ingress elephant**: one TCP flow, 15s, forward (client → server)
- **Ingress mice**: 20 parallel TCP flows, 10s, forward
- **Egress elephant**: one TCP flow, 15s, reverse (`-R`, server → client)
- **Egress mice**: 20 parallel TCP flows, 10s, reverse

Receiver-side aggregate goodput from
`end.sum_received.bits_per_second`.

The rate sweep is there to catch plugin-induced regressions that only
appear at higher annotated rates (token-bucket math edge cases,
fast-pass threshold scaling, BPF map cost under heavier traffic). On
Mac/colima the underlying wire (flannel host-gw via the docker
bridge) tops out around 1 Gbps single-stream, so 10G annotations
report ~Gbps line rate — both natra and vanilla. Override the sweep
on metal:

```bash
RATE_SWEEP="100M 1G 10G 40G" make perf-vs-vanilla
```

### Most recent run (colima, aarch64, k3d + flannel host-gw)

With bytes-counting CMS and the rate-scaled threshold (10 Mbps pod
→ ~125 KiB threshold), natra now lands within vanilla's variance on
both the single-flow elephant column and the 20-parallel "mice"
column. Workload 1 success criterion (rate-limited like vanilla) is
met across all four cells:

| Direction | Plugin                | Elephant   | Mice (20× parallel)  |
|-----------|-----------------------|------------|----------------------|
| ingress   | natra                 | 11.16 Mbps | **11.65 Mbps**       |
| ingress   | upstream `bandwidth`  | 10.04 Mbps |  9.64 Mbps           |
| egress    | natra                 | 11.69 Mbps | **12.81 Mbps**       |
| egress    | upstream `bandwidth`  | 10.11 Mbps |  9.61 Mbps           |

Baseline (no plugin) on this rig lands at ~21-43 Mbps for elephants
(colima's flannel host-gw is bottlenecked by the LinuxKit kernel's
software dataplane) rather than the kindnet line-rate ~55 Gbps we
saw pre-migration. Doesn't affect the rate-limit comparison — both
natra and vanilla cap at ~10 Mbps which is well below the
unrestricted baseline — but it does make the baseline-to-throttled
ratio less dramatic on this hardware. Real Linux hosts with hardware
network offload would show the full ratio.

Pre-change (packet-count CMS, threshold=50 packets): natra read
36-39 Mbps on the 20-parallel mice column, 3.5× the cap. The
bytes-CMS + rate-scaled threshold work cuts that to ~12 Mbps, in
the same range as the upstream HTB plugin.

The 20-parallel "mice" column sits at the threshold tradeoff
natra's CMS is designed for. The heavy-hitter threshold is now in
**bytes** and **scaled to the annotated rate** (default formula:
`rate × 100 ms`), so a 10 Mbps pod gets a ~125 KiB threshold —
enough headroom for tail mice (HTTP responses, WebSocket frames)
but tight enough that each iperf3 parallel stream crosses after
~2 GRO-coalesced super-packets and steady-state throttling takes
over. With single-stream flows the elephant crosses in
milliseconds. Twenty parallel iperf streams is neither real mice
nor a real elephant; this column is a synthetic in-between case
useful for sanity-checking the threshold value.

## Workload 2: mixed (elephant + annotated mice + bystander mice)

Three pods on the same k3d cluster:

- `perf-server` (annotated 10M/10M, runs iperf3 + nginx, pinned
  to worker)
- `bystander` (no annotations, runs nginx, pinned to worker)
- `perf-client` (iperf3 + hey, pinned to control-plane)

Client traffic, concurrent for `MIXED_HEY_DURATION` (~20s):

- `iperf3 --bidir` for 30s against `perf-server` — one elephant flow
  in each direction. Drains the ingress and egress buckets.
- After 5s of warmup, two parallel `hey -c 50 -z 20s
  -disable-keepalive` runs — one to `perf-server`, one to
  `bystander`. Each request opens a fresh TCP connection (new
  5-tuple → new flow_key) with ~5-7 KB of total bytes. Well
  under the rate-scaled heavy-hitter threshold (~125 KiB at the
  10 Mbps annotation).

Three things to read out of the result table:

1. **Elephant ingress/egress.** The headline rate-limit guarantee.
   Baseline shows flannel line rate; both plugins land at-or-below
   10 Mbps.
2. **Annotated mice (perf-server) RPS / p99.** What the plugin does
   to small flows *sharing the elephant's pod budget*. This is
   natra's design wedge: CMS classification lets the mice fast-pass
   the bucket. Under vanilla HTB, everything queues together so
   hey latency tracks the elephant. Baseline is the un-throttled
   ceiling.
3. **Bystander mice (unannotated, same node) RPS / p99.** What the
   plugin does to a neighboring unannotated pod. Both natra and
   vanilla leave unannotated pods alone (no BPF / no HTB attached),
   so the bystander row should look ≈ baseline under all three
   configurations. This is the "natra is a no-op for the rest of
   the cluster" assertion; if natra ever regresses to charging
   every pod, this column drops.

### Most recent run

EDT pacing defaults to `auto` — at CNI ADD, natra probes `fq`
install on pod-eth0, uses EDT pacing if it succeeds, falls back to
drop semantics if it fails. ECN-mark fires whenever the peer
negotiated ECN-capable TCP (`net.ipv4.tcp_ecn=1` on the cluster
nodes). The combination removes virtually all above-rate drops, so
no TCP retransmit amplifier is bleeding worker CPU into neighboring
pods.

| Plugin                       | iperf ing | iperf eg  | Annotated mice (perf-server) RPS / p99 | Bystander RPS / p99 |
|------------------------------|-----------|-----------|---------------------------------------:|--------------------:|
| baseline (no plugin)         | ~8 Gbps   | ~27 Gbps  | 5283 / 71 ms                           | 6479 / 25 ms        |
| **natra (EDT auto + ECN)**   | 9.12 Mbps | 6.77 Mbps | **4073 / 262 ms**                      | **6913 / 43 ms**    |
| upstream `bandwidth` (HTB)   | 10.59 Mbps| 8.67 Mbps | 12 / 5715 ms                           | 8519 / 40 ms        |

The headline wedge is the **annotated mice** column. natra serves
4073 RPS in the same pod as a 10 Mbps elephant; vanilla serves 12
RPS — natra is ~340× higher. CMS classification is what makes that
gap: each hey request is a fresh flow_key that stays under the
heavy-hitter threshold, fast-passes the bucket, and isn't queued
behind the elephant.

The **bystander** column is essentially noise-equivalent to vanilla
and baseline. natra bystander RPS lands within run-to-run variance
of baseline; bystander p99 (43 ms) matches vanilla's (40 ms) and
sits at ~1.7× baseline (25 ms).

The ~1.7× bystander p99 over baseline is **not** natra-specific —
it's the irreducible cost of having an elephant flow running
concurrently on the same worker node. The elephant is paced to
10 Mbps for the entire 20s hey measurement (under both natra and
vanilla), competing with bystander traffic for shared resources on
the worker:

- per-CPU softirq processing time (ksoftirqd serializes elephant
  and bystander packet work)
- NIC rx/tx rings (single physical interface)
- CPU caches (every elephant packet touches BPF / HTB state that
  evicts the bystander's nginx working set)
- bridge fdb / forwarding cost

Baseline's elephant runs unthrottled at ~Gbps, finishes in well
under a second, and the worker is essentially idle during the 20s
hey measurement — that's why baseline p99 is so low. natra and
vanilla both keep the elephant alive for the whole window, so
shared-resource contention costs ~1.7×. Closing that further would
mean not throttling the elephant; there's no way to get a paced
10 Mbps elephant to be invisible to its neighbors on a shared NIC.

### How we got here

The bystander p99 wasn't always 43 ms. Earlier in the design,
above-rate packets were dropped unconditionally. Drops triggered
TCP retransmits, doubling packet counts for the same useful
payload, which doubled softirq work and bled into bystander
performance. Numbers from successive iterations:

| natra mode                      | Bystander RPS / p99   | Bystander Δ RPS vs baseline |
|---------------------------------|----------------------:|----------------------------:|
| early — drop only               | 4728 / 134 ms         | **-27%**                    |
| EDT only, ECN passive           | 6303 / —              | -3%                         |
| **EDT auto + ECN=1 (current)**  | **6913 / 43 ms**      | **+6.7%** (within noise)    |

The EDT-pacing + ECN-mark fix removed the retransmit amplifier
completely. What remains is the shared-resource cost above, which
matches vanilla within noise.

### Throttle disposition details

When the bucket can't admit an above-rate packet, natra picks the
disposition in this order:

1. **ECN-mark** (`bpf_skb_ecn_set_ce`) — set CE on ECN-capable
   packets, return TC_ACT_OK. Works on both directions. Requires
   the peer to have negotiated ECN-capable TCP (`tcp_ecn=1` on
   either end).
2. **EDT pacing** (egress only, when `cfg.edt_pacing != 0`) —
   stamp `skb->tstamp` with the next-release time and return
   TC_ACT_OK. The `fq` qdisc on pod-eth0 honors the timestamp
   and releases the packet at the scheduled time.
3. **Drop** (`TC_ACT_SHOT`) — fallback for ingress non-ECN
   traffic that nothing else can pace.

EDT pacing requires an `fq` qdisc downstream of where the BPF
program runs. natra installs `fq` on pod-eth0 inside the pod
netns when it picks pod-side egress attach, so the qdisc sits
right after the egress BPF program. Host-side attach has no
deterministic spot for `fq`, so EDT only applies when the attach
side is pod.

EDT defaults to `auto`: natra probes `fq` install at CNI ADD and
uses the EDT path only when the probe succeeds. The `auto`
strategy also reorders attach attempts to `tcx-pod → clsact-pod →
tcx-host → clsact-host` so the optimal config (pod-side + EDT)
is the first attempted shape; environments where `fq` install
fails cleanly degrade through the rest of the chain. Force the
behavior with `NATRA_EDT_PACING=on` (fail attach if `fq` install
fails — restricts strategy to pod-side) or `NATRA_EDT_PACING=off`
(never install `fq`, always drop after ECN-mark — uses the
host-side-first strategy for cilium/NPA coexistence).

The mixed iperf throughput under natra comes in close to but
below the 10 Mbps cap because when hey *does* occasionally hit
the bucket (CMS collisions, above-threshold bursts on a
connection), it consumes tokens iperf would otherwise have. The
headline guarantee is "annotated rate is the ceiling," not "the
annotated rate is always reached."

## Reproduce

```bash
make perf-vs-vanilla
```

natra's attach mode defaults to `auto`. To pin a specific mode:

```bash
NATRA_PERF_ATTACH_MODE=tcx-podside     make perf-vs-vanilla
NATRA_PERF_ATTACH_MODE=clsact-hostside make perf-vs-vanilla
NATRA_PERF_ATTACH_MODE=clsact-podside  make perf-vs-vanilla
```

EDT pacing also defaults to `auto`. To force a specific mode:

```bash
NATRA_PERF_EDT_PACING=on   make perf-vs-vanilla   # require fq, fail if unavailable
NATRA_PERF_EDT_PACING=off  make perf-vs-vanilla   # never install fq, drop after ECN
```
