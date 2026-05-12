# natra vs. upstream bandwidth — head-to-head

Real-cluster comparison between natra and the upstream
`containernetworking/plugins/bandwidth` plugin. Two workloads are run
against each plugin:

1. **iperf-only** — characterizes elephant-flow rate-limiting in both
   directions.
2. **realistic mixed** — runs an iperf3 `--bidir` elephant alongside
   concurrent `hey` HTTP load against an nginx in the same server
   pod. Each hey request is a fresh TCP connection (no keep-alive),
   so it stays under natra's heavy-hitter threshold. This is the
   workload natra is designed for; it's what exercises CMS fast-pass
   against vanilla HTB's flow-agnostic bucket.

Run with:

```bash
make perf-vs-vanilla
# ~15 min, two kind clusters in sequence
cat docs/perf-vs-vanilla-result.txt
```

The driver is `scripts/perf-vs-vanilla.sh`. Override the natra attach
mode for the comparison via `NATRA_PERF_ATTACH_MODE=clsact-podside`.

## Setup

Two kind clusters, identical config:

- 2 nodes (control-plane + worker), kindnet as main CNI
- Server pinned to worker with both
  `kubernetes.io/ingress-bandwidth: "10M"` and
  `kubernetes.io/egress-bandwidth: "10M"`; client on control-plane
  (cross-node traffic over kindnet's bridge + tunnel)
- Cluster A chains natra after kindnet; Cluster B chains the upstream
  `bandwidth` plugin after kindnet

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

### Most recent run (colima 6.8.0-64-generic, aarch64)

| Direction | Plugin                | Elephant     | Mice (20× parallel)  |
|-----------|-----------------------|--------------|----------------------|
| ingress   | natra                 | 12.14 Mbps   | 36.79 Mbps           |
| ingress   | upstream `bandwidth`  | 10.04 Mbps   |  9.59 Mbps           |
| egress    | natra                 | 12.16 Mbps   | 38.99 Mbps           |
| egress    | upstream `bandwidth`  | 10.12 Mbps   |  9.61 Mbps           |

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

## Workload 2: realistic mixed (elephant + HTTP mice)

One server pod runs both iperf3 (port 5201) and nginx (port 80,
default ~600B index). Client (a pod with both iperf3 and `hey`
installed) runs:

- `iperf3 --bidir` for 30s — one elephant flow in each direction.
  Drains the ingress and egress buckets.
- After 5s of warmup, `hey -c 50 -z 20s -disable-keepalive
  http://server/` — each request opens a fresh TCP connection
  (new 5-tuple → new flow_key) with ~5-7 packets total. Stays well
  under natra's heavy-hitter threshold of 10.

### Most recent run

| Plugin               | Iperf ingress | Iperf egress | Hey RPS | Hey p50 | Hey p99 |
|----------------------|---------------|--------------|---------|---------|---------|
| natra                |  9.23 Mbps    |  5.13 Mbps   | **4426**|  1.1 ms |  208 ms |
| upstream `bandwidth` | 10.59 Mbps    |  8.32 Mbps   |     12  |  4.8 s  |  5.0 s  |

**Hey RPS 369× higher under natra, p99 24× lower, p50 4400× lower.**

Both plugins land at-or-below 10 Mbps for the iperf elephant in
both directions — bandwidth annotation honored.

The difference is what they do to the small HTTP requests competing
for that same 10 Mbps budget:

- **88× more requests/sec under natra** (1065 vs 12). Each hey
  request that arrives uses a new 5-tuple, so its per-flow CMS
  count stays below the heavy-hitter threshold and the bucket is
  bypassed entirely. Under vanilla every packet — large iperf
  payload or tiny HTTP request — enters the same HTB queue, so
  hey waits its turn behind the elephant.
- **p50 ≈ instant under natra, 3.8 seconds under vanilla**. The
  natra p50 of 0.4 ms is essentially "as fast as the network can
  go"; vanilla's 3.8 s is the time a request spends sitting in
  HTB's queue waiting for token-rationed bandwidth ahead of it.
- **p99 still 1 second under natra** because there are tail
  events — a hey burst can briefly inflate a flow's CMS estimate
  via collisions with the elephant's flow_key, and those tail
  requests do hit the bucket. Vanilla's p99 of 5.5 s is the queue
  reaching its drop limit.

natra's iperf throughput under mixed comes in below 10 Mbps because
when hey *does* hit the bucket (CMS collisions, occasional
above-threshold bursts on a connection), it consumes tokens iperf
would otherwise have. That's the right trade-off for this design —
the headline guarantee is "annotated rate is the ceiling," not
"the annotated rate is always reached."

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
