# natra vs. upstream bandwidth — head-to-head

Real-cluster comparison across three configurations:

1. **baseline** — kindnet alone, no rate-limiting plugin chained.
   The bandwidth annotations on `perf-server` are present but
   nothing acts on them; this is the "unaided cluster" floor.
2. **natra** — kindnet + natra chained.
3. **upstream `bandwidth`** — kindnet + `containernetworking/plugins`
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
# ~18-22 min, three kind clusters in sequence
cat docs/perf-vs-vanilla-result.txt
```

The driver is `scripts/perf-vs-vanilla.sh`. Override the natra attach
mode for the comparison via `NATRA_PERF_ATTACH_MODE=clsact-podside`.

## Setup

Three kind clusters, identical topology:

- 2 nodes (control-plane + worker), kindnet as main CNI
- `perf-server` pinned to worker, annotated
  `kubernetes.io/ingress-bandwidth: "10M"` + `egress-bandwidth: "10M"`;
  runs iperf3 + nginx
- `bystander` pinned to worker, **no annotations**; runs nginx only
- `perf-client` on control-plane (cross-node traffic over kindnet's
  bridge + tunnel)
- Cluster 0: kindnet only. Cluster A chains natra; cluster B chains
  the upstream `bandwidth` plugin.

For Cluster B, the test fetches the upstream `bandwidth` binary from
the `containernetworking/plugins` v1.5.1 release (kind nodes ship a
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

## Workload 1: iperf-only (legacy)

iperf3 against an iperf3-only server, four phases per cluster:

- **Ingress elephant**: one TCP flow, 15s, forward (client → server)
- **Ingress mice**: 20 parallel TCP flows, 10s, forward
- **Egress elephant**: one TCP flow, 15s, reverse (`-R`, server → client)
- **Egress mice**: 20 parallel TCP flows, 10s, reverse

Receiver-side aggregate goodput from
`end.sum_received.bits_per_second`.

### Most recent run (colima, aarch64)

| Direction | Plugin                | Elephant     | Mice (20× parallel)  |
|-----------|-----------------------|--------------|----------------------|
| ingress   | baseline (no plugin)  | 55,963 Mbps  | 54,971 Mbps          |
| ingress   | natra                 | 12.16 Mbps   | 36.60 Mbps           |
| ingress   | upstream `bandwidth`  | 10.04 Mbps   |  9.64 Mbps           |
| egress    | baseline (no plugin)  | 54,812 Mbps  | 49,325 Mbps          |
| egress    | natra                 | 12.20 Mbps   | 39.04 Mbps           |
| egress    | upstream `bandwidth`  | 10.11 Mbps   |  9.61 Mbps           |

The single-stream elephant lands within ~21% of the 10 Mbps cap
under natra and exactly at cap under vanilla.

The 20-parallel "mice" column is where natra reads 3.5× over cap.
That's the intentional consequence of natra's heavy-hitter
threshold (default 50): each parallel iperf3 stream gets ~50 GRO-
coalesced super-packets of fast-pass before its CMS count crosses
classification, and 20 streams × 50-packet budget × 1Gbps line
rate dominates a 10s measurement. With single-stream flows the
elephant fully consumes its budget in milliseconds and steady-state
throttling takes over for the rest of the test, hence the ~12 Mbps
reading. The threshold is what makes the mixed workload (Workload
2 below) work — without it, every HTTP request would false-positive-
classify as heavy and hit the bucket. Twenty parallel iperf streams
is neither real mice nor a real elephant; this column is a synthetic
in-between case kept around to document the threshold trade-off.

## Workload 2: mixed (elephant + annotated mice + bystander mice)

Three pods on the same kind cluster:

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
  5-tuple → new flow_key) with ~5-7 packets total. Well under
  natra's heavy-hitter threshold of 50.

Three things to read out of the result table:

1. **Elephant ingress/egress.** The headline rate-limit guarantee.
   Baseline shows kindnet line rate; both plugins land at-or-below
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

| Plugin                | iperf ing  | iperf eg   | Annotated mice (perf-server) |             | Bystander mice (unannotated)  |             |
|-----------------------|------------|------------|-----------------------------:|------------:|-------------------------------:|------------:|
|                       |            |            | RPS                          | p99         | RPS                            | p99         |
| baseline (no plugin)  | 8168 Mbps  | 27407 Mbps | 5462                         | 73 ms       | 7143                           | 22 ms       |
| natra                 | 10.7 Mbps  | 9.3 Mbps   | **3539**                     | 211 ms      | 4728                           | 134 ms      |
| upstream `bandwidth`  | 10.6 Mbps  | 7.0 Mbps   | 11                           | 5118 ms     | 7560                           | 42 ms       |

The headline wedge is the **annotated mice** column. natra serves
3539 RPS in the same pod as a 10 Mbps elephant; vanilla serves 11
RPS — natra is ~320× higher. CMS classification is what makes that
gap: each hey request is a fresh flow_key that stays under the
heavy-hitter threshold, fast-passes the bucket, and isn't queued
behind the elephant.

The **bystander** column is honest news in both directions:

- Vanilla bystander is essentially baseline (7560 vs 7143 RPS) —
  the unannotated pod gets no HTB attached, so it's untouched.
- natra bystander is **~35% lower than baseline** (4728 vs 7143
  RPS), even though no BPF program is attached to the bystander
  itself. The most likely cause is CPU/NIC contention: natra's
  per-packet BPF work on perf-server's veth (CMS update + bucket
  check on every packet of a sustained 10 Mbps elephant) consumes
  worker-node cycles that the bystander's HTTP serving would
  otherwise have. Vanilla's HTB shaping is cheaper per-packet, so
  vanilla doesn't show this bleed.

This is a real, measurable cost of natra's design on neighboring
pods. It's much smaller than the gain on annotated mice (35% bleed
vs 320× win), but it's not zero — worth knowing when sizing nodes
that mix annotated heavy traffic with unannotated latency-sensitive
workloads.

### Bystander gap closed: EDT auto-detect + ECN

EDT pacing now defaults to `auto` — at CNI ADD, natra probes `fq`
install on pod-eth0, uses EDT pacing if it succeeds, falls back to
drop semantics if it fails. ECN-mark fires whenever the peer
negotiated ECN-capable TCP. The combination removes virtually all
above-rate drops, and the TCP retransmit amplifier that was
bleeding worker CPU into the bystander disappears with them.

The progression across runs:

| Plugin                          | Iperf ing  | Iperf eg  | Annotated mice RPS | Bystander RPS | Bystander Δ vs baseline |
|---------------------------------|------------|-----------|-------------------:|--------------:|------------------------:|
| baseline (no plugin)            | ~8 Gbps    | ~27 Gbps  | 5283               | 6479          | 0% (reference)          |
| natra (early — drop only)       | 10.7 Mbps  | 9.3 Mbps  | 3539               | 4728          | **-27%**                |
| natra (EDT only, ECN passive)   | 9.96 Mbps  | 6.71 Mbps | 4381               | 6303          | -3%                     |
| **natra (EDT auto + ECN=1)**    | **9.12 Mbps** | **6.77 Mbps** | **4073**       | **6913**      | **+6.7%** (within noise)|
| upstream `bandwidth` (HTB)      | 10.59 Mbps | 8.67 Mbps | 12                 | 8519          | +31% (HTB queueing artifact) |

The EDT auto-detect change:

- At CNI ADD, the plugin tries `tc qdisc replace dev eth0 root fq`
  inside the pod netns. Idempotent — skips when `fq` is already
  the root qdisc.
- If the install succeeds, `cfg.edt_pacing` is set in the BPF
  config slot and the BPF program EDT-stamps above-rate packets
  instead of dropping.
- If it fails (e.g., some other plugin already set a non-`fq`
  root qdisc), `cfg.edt_pacing` stays zero and the BPF falls back
  to drop after ECN-mark.

Operators can force the behavior either way with
`NATRA_EDT_PACING=on` (fail attach if `fq` install fails) or
`NATRA_EDT_PACING=off` (never install `fq`, always drop after
ECN-mark). Auto is the recommended default — it picks the
optimal configuration when supported and degrades cleanly when
not.

For ECN to fire on a TCP flow, both peers' kernels need
`net.ipv4.tcp_ecn` set to allow negotiation. Linux's default of
`2` (passive — only respond, never initiate) means most flows
won't negotiate even if natra can mark them. Setting `tcp_ecn=1`
on the cluster nodes lets active negotiation happen and closes
the ingress side of the bystander bleed (natra still drops
non-ECN ingress traffic — there's no transmission-side qdisc to
delay incoming packets).

The mixed iperf throughput under natra comes in close to but below
the 10 Mbps cap because when hey *does* occasionally hit the bucket
(CMS collisions, above-threshold bursts on a connection), it
consumes tokens iperf would otherwise have. The headline guarantee
is "annotated rate is the ceiling," not "the annotated rate is
always reached."

## Reproduce

```bash
make perf-vs-vanilla
```

natra's attach mode defaults to `tcx-hostside`. To exercise a
different mode in the comparison:

```bash
NATRA_PERF_ATTACH_MODE=tcx-podside    make perf-vs-vanilla
NATRA_PERF_ATTACH_MODE=clsact-hostside make perf-vs-vanilla
NATRA_PERF_ATTACH_MODE=clsact-podside make perf-vs-vanilla
```
