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

Numbers from the latest run live in `docs/perf-vs-vanilla-result.txt`;
this doc explains what each column is measuring rather than carrying
a snapshot that drifts on every rerun.

For Workload 1, expect roughly:

- baseline: elephant and 20-parallel mice both at kindnet line rate
  (hundreds of Mbps; whatever the runner allows)
- vanilla: elephant ~10 Mbps, mice ~10 Mbps (HTB shares the bucket)
- natra: elephant ~12 Mbps, mice ~30-40 Mbps (CMS fast-passes new
  streams until each one's per-flow count crosses threshold)

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

Numbers from the latest run are in `docs/perf-vs-vanilla-result.txt`.
The qualitative shape to expect:

- Elephant: ~line rate under baseline, ~10 Mbps under natra and
  vanilla.
- Annotated mice: ~line rate under baseline, very high (thousands
  of RPS, sub-second p99) under natra, very low (single-digit RPS,
  multi-second p99) under vanilla.
- Bystander mice: ~line rate under all three.

The mixed iperf throughput under natra comes in below 10 Mbps
because when hey *does* hit the bucket (CMS collisions, occasional
above-threshold bursts on a connection), it consumes tokens iperf
would otherwise have. The headline guarantee is "annotated rate is
the ceiling," not "the annotated rate is always reached."

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
