# natra vs. upstream bandwidth — head-to-head

Real-cluster comparison between natra and the upstream
`containernetworking/plugins/bandwidth` plugin, both directions. Run
with:

```bash
make perf-vs-vanilla
# ~10-12 min, two kind clusters in sequence, both directions per cluster
cat docs/perf-vs-vanilla-result.txt
```

The driver is `scripts/perf-vs-vanilla.sh`. Override the natra attach
mode for the comparison via `NATRA_PERF_ATTACH_MODE=clsact-podside`.

## Setup

Two kind clusters, identical config:

- 2 nodes (control-plane + worker), kindnet as main CNI
- iperf-server pinned to the worker with **both**
  `kubernetes.io/ingress-bandwidth: "10M"` and
  `kubernetes.io/egress-bandwidth: "10M"`; iperf-client on
  control-plane (cross-node traffic over kindnet's bridge + tunnel)
- Cluster A chains natra after kindnet; Cluster B chains the upstream
  `bandwidth` plugin after kindnet

For Cluster B, the test fetches the upstream `bandwidth` binary from
the `containernetworking/plugins` v1.5.1 release (kind nodes ship a
subset of CNI plugins and don't include `bandwidth`) and `modprobe`s
`ifb` on each node. Without the IFB module, the upstream plugin's
HTB-on-IFB silently no-ops and doesn't rate-limit.

## Workload

iperf3 against the same server, four phases per cluster:

- **Ingress elephant**: one TCP flow, 15 seconds, forward (client → server).
- **Ingress mice**: 20 parallel TCP flows, 10 seconds, forward.
- **Egress elephant**: one TCP flow, 15 seconds, reverse (`-R`, server → client).
- **Egress mice**: 20 parallel TCP flows, 10 seconds, reverse.

Receiver-side aggregate goodput is read from
`end.sum_received.bits_per_second`.

## Most recent run (colima 6.8.0-64-generic, aarch64)

| Direction | Plugin                | Elephant     | Mice (20× parallel)  |
|-----------|-----------------------|--------------|----------------------|
| ingress   | natra                 | 11.27 Mbps   | 23.74 Mbps           |
| ingress   | upstream `bandwidth`  | 97.21 Mbps   |  9.59 Mbps           |
| egress    | natra                 | 11.29 Mbps   | 14.54 Mbps           |
| egress    | upstream `bandwidth`  | 108.16 Mbps  |  9.61 Mbps           |

Read these as separate signals:

- **natra**: rate-limits the elephant cleanly to within +13% of the
  10 Mbps annotation in both directions. The mice numbers run
  meaningfully above 10 Mbps because, against a 20-parallel workload,
  natra's CMS classifies most of the per-flow streams as mice (each
  individually under threshold) and they fast-pass the bucket. That's
  the point of natra; it's not a "violation" of the annotation, it's
  the design.
- **upstream bandwidth, mice phase**: lands near 10 Mbps in both
  directions. HTB-on-IFB engaging as expected.
- **upstream bandwidth, elephant phase**: doesn't engage (~97 Mbps
  ingress, ~108 Mbps egress under a 10 Mbps annotation). HTB engages
  by the second phase but didn't during the first 15s. This is an
  unresolved test-rig issue (kind/HTB initialization timing or IFB
  redirect quirks), not natra winning.

## What this run does and doesn't tell you

It tells you natra rate-limits as designed in a real kind cluster
with kindnet's chain.

It doesn't tell you natra's CMS fast-pass produces a measurable
benefit on this workload — it doesn't, by design. A workload of many
brief TCP connections (HTTP-style: wrk, ab, hey) would exercise the
fast pass; iperf with `-P` doesn't.

## Synthetic vs real

The synthetic L5 test (`TestScenarioMixedVsVanilla` in
`test/perf/perf_linux_test.go`) runs the same packet sequence through
`natra.bpf.o` and an in-tree token-bucket-on-every-packet emulator
(`bpf/vanilla.bpf.c`) under `BPF_PROG_RUN`, with mice flows of 5
packets each (well under the threshold). It runs on every push and
catches algorithmic regressions in natra without depending on a real
cluster.

The synthetic test reports a ~98pp gap on its specific worst-case
mice-after-elephant scenario. The real-cluster comparison documented
here is for "does it actually work end-to-end against real
upstream-bandwidth plumbing." Different questions.

## Reproduce

```bash
make perf-vs-vanilla
```

To compare with `clsact-podside` instead of the default `tcx`:

```bash
NATRA_PERF_ATTACH_MODE=clsact-podside make perf-vs-vanilla
```
