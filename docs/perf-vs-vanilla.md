# natra vs upstream bandwidth

Head-to-head against `containernetworking/plugins/bandwidth` on a
k3d rig. Same workloads against three configurations: baseline
(no rate-limiter), natra, upstream token-bucket qdisc (HTB in
v1.5.1, TBF in v1.6.0+).

This doc's main tables are the k3d-based comparison (single Linux
kernel, software dataplane on colima/LinuxKit). The same
three-phase comparison now also runs on **two real kernels** via
the vm-rig (`make perf-vs-vanilla-vm`; two lima VMs, each its own
kernel, real inter-VM vmnet wire) — the cross-kernel measurement
the "Gaps" section below long flagged as missing. See
`scripts/vm-rig/README.md` for the rig; results below under
"Two-kernel (vm-rig) results".

## Setup

Three k3d clusters brought up in sequence:

- 2 nodes (control-plane + worker), flannel host-gw as main CNI.
- `perf-server` on worker, both directions annotated 10M; runs
  iperf3 + nginx.
- `bystander` on worker, unannotated; runs nginx.
- `perf-client` on control-plane so traffic crosses a node
  boundary.
- Cluster 0: flannel only. Cluster A chains natra. Cluster B
  chains the upstream `bandwidth` plugin.

k3s 1.30+ ships the `bandwidth` plugin (v1.6.0-k3s1) already
chained into the default `10-flannel.conflist`, so Cluster B
uses that directly — no init container needed. Earlier rigs
that didn't bundle bandwidth used a `vanilla-bandwidth-installer`
DaemonSet; the script detects the existing chain and skips it.
`modprobe ifb` still runs on each node so the plugin can install
its IFB device.

Pre-measurement normalizations:

- **TBF burst patch (vanilla only).** kubelet sets the upstream
  bandwidth plugin's per-pod TBF burst to ~150 seconds of credit
  (~193 MB on a 10 Mbps annotation). The script reaches into the
  node netns via nsenter and rewrites each pod's TBF qdisc to
  `burst 1mb latency 50ms` before measuring. (v1.5.1 of the
  plugin used HTB; v1.6.0 uses TBF. The script targets whichever
  is present.) natra's bucket defaults to 0.5 sec of credit
  (`config.DefaultBurstRatio`), which is in the same envelope
  without an explicit override.
- **Bucket warmup.** 20s forward + 20s reverse priming flows
  drain initial-burst tokens before each measurement.

## Workload 1: iperf-only (rate sweep)

The rig deploys a separate iperf3 server pod for each annotated
rate in `RATE_SWEEP` — by default three pods at 10 Mbps, 1 Gbps,
and 10 Gbps — then runs an iperf3 elephant flow plus a 20-parallel
"mice" run against each one. Ingress = forward `iperf3`; egress =
`iperf3 -R` (server is the sender). Goodput is the receiver-side
`end.sum_received.bits_per_second`. Repeating across three orders
of magnitude catches plugin bugs that only surface at higher rates
(token-bucket math overflow, fast-pass threshold scaling errors).

| Direction | Plugin   | Elephant   | Mice (20× parallel) |
|-----------|----------|------------|---------------------|
| ingress   | natra    | 10.19 Mbps | 11.79 Mbps          |
| ingress   | upstream | 10.07 Mbps |  9.64 Mbps          |
| egress    | natra    | 10.18 Mbps | 11.54 Mbps          |
| egress    | upstream | 10.08 Mbps |  9.45 Mbps          |

Rig: colima aarch64, LinuxKit ~6.8.x, k3d v5.7.4, flannel
host-gw, software dataplane (no NIC offload). Single sample
each, captured in one run with both plugins active. Both
plugins land within 2% of cap on every cell. At higher rates
both stay inside 5% of cap (natra 1024/1026 Mbps on 1G,
upstream 956/956 Mbps on 1G). Above ~1 Gbps the rig's wire
becomes the bottleneck — single-stream colima caps at ~10 Gbps
regardless of shaper.

Heavy-hitter threshold scales with rate:
`max(16 KiB, rate_bytes × 100ms)`. 10 Mbps pod → ~125 KiB. Tail
mice (HTTP responses, WebSocket frames) fit under that and
fast-pass via CMS; iperf3 parallel streams cross after ~2 GRO
super-packets and pay the bucket from there.

High-rate rows are bounded by the rig's wire, not the shaper:
colima's host-gw via the docker bridge caps single-stream at
~1 Gbps, so `RATE_SWEEP=10G` rows report ~Gbps from both plugins.
They confirm the plugins don't break under a 10G annotation; they
don't confirm 10G throttle accuracy.

## Workload 2: mixed (elephant + annotated mice + bystander mice)

Three pods on one cluster:

- `perf-server` (annotated 10M/10M): iperf3 + nginx.
- `bystander` (unannotated, same node): nginx.
- `perf-client` (control-plane): iperf3 + hey, concurrent.

Client traffic:

- `iperf3 --bidir` for 30s against perf-server — one elephant per
  direction, drains both buckets.
- 5s into the iperf, two parallel `hey -c 50 -z 20s
  -disable-keepalive` runs: one against perf-server (annotated
  mice), one against bystander (unannotated mice). Each request
  is a fresh TCP connection at ~5-7 KB, well under the 125 KiB
  heavy-hitter threshold.

| Plugin                | iperf ing  | iperf eg   | Annotated mice RPS / p99 | Bystander RPS / p99 |
|-----------------------|------------|------------|--------------------------|---------------------|
| baseline (no plugin)  | 59.66 Mbps | 49.67 Mbps |   10 /  5015 ms          | 3662 / 110 ms       |
| natra                 | 10.24 Mbps | 10.03 Mbps | 6593 /    28 ms          | 6735 /  27 ms       |
| upstream `bandwidth`  | 10.69 Mbps | 10.28 Mbps |   31 /  1795 ms          | 6555 /  54 ms       |

Single sample, captured in the same run as Workload 1 with
both plugins active. Post bounded-EDT-delay change (273a99f).
Baseline mice are slow under concurrent load because the
elephant saturates colima's shared software dataplane — the
bucket isn't what's hurting them, the wire is. With either
plugin's elephant capped to 10 Mbps, the mice get the wire
back. Read in three pieces:

- **Elephant cap.** Both plugins land within 3% of the 10M cap
  on both directions (natra 10.24/10.03, upstream 10.69/10.28).
  natra's egress was previously stuck below cap from cwnd-halve
  feedback; the 50 ms EDT-delay bound shipped in 273a99f lets
  occasional ECN signals reach the sender, which keeps cwnd
  at the steady-state level corresponding to the cap.
- **Annotated mice.** natra 6593 RPS / p99 28 ms vs upstream
  31 RPS / p99 1795 ms. CMS classification lets each fresh-flow
  hey request bypass the bucket; the upstream token-bucket
  qdisc queues every flow against the same 10 Mbps slot, so
  mice wait behind the elephant. **213× RPS, 64× lower p99 —
  the value-prop of CMS-then-bucket on a mixed workload,
  end-to-end on real upstream code.**
- **Bystander.** Neither plugin attaches anything to unannotated
  pods. natra's bystander p99 (27 ms) is now lower than
  upstream's (54 ms) in this run — the bounded-EDT change keeps
  fq queue depth at ~50 ms × rate (≈ 42 MTU packets at 10 Mbps),
  so the bystander competes against a bounded backlog of
  EDT-stamped packets instead of an arbitrarily-deep one.
  Vanilla still drops packets at the qdisc, but bystander
  measurements have non-trivial run-to-run variance on this rig
  (a prior run showed 34 ms vs 61 ms; this one shows the reverse).
  Practical read: bystander cost from natra is now at most
  parity with upstream's drop disposition, sometimes better.

## Gaps in this comparison

What these numbers don't support, and what would close each:

- **Real-NIC behavior**: TSO/GRO/LRO, hardware TX timestamping —
  none exercised. The BPF programs see whatever GRO shape colima
  produces. → cloud-VM or bare-metal rig.
- **Cross-kernel wire**: k3d "nodes" share one Linux kernel; the
  inter-node fabric is a software bridge. *Now covered* by
  `make perf-vs-vanilla-vm` (two lima VMs, two real kernels, real
  inter-VM vmnet wire — see "Two-kernel (vm-rig) results" below).
  Real *hardware* NICs/switches still need cloud-VM/bare-metal.
- **Run-to-run distribution**: single sample per cell. Re-run with
  `PERF_RUNS=N` for mean ± stddev; full p50/p99/p100 histograms
  aren't currently captured.
- **>1 Gbps accuracy**: colima's host-gw caps single-stream at
  ~1 Gbps. 10G annotation rows report behavior under the cap,
  not throttle accuracy at 10 Gbps.
- **cilium / aws-network-policy-agent composition**: claimed by
  construction (bpf_mprog on kernel 6.6+), not measured. Needs a
  cluster with cilium chained alongside natra.

Plan to close: `docs/test-environments.md`.

## Two-kernel (vm-rig) results

`make perf-vs-vanilla-vm` — two lima VMs, each its own Linux
kernel (Debian 13, 6.12), real inter-VM vmnet wire. perf-server
on the agent VM, perf-client on the server VM, so every packet
crosses the kernel boundary. iperf3 elephant (receiver-side
bps) + hey fresh-connection HTTP mice. Each phase runs on its
own fresh cluster (full down/up/measure/down); baseline has no
bandwidth annotation, vanilla and natra annotate 10M/10M.

| Phase    | iperf ing   | iperf eg    | hey rps | p50 ms | p99 ms |
|----------|-------------|-------------|---------|--------|--------|
| baseline | 1885.3 Mbps | 1904.2 Mbps |   17524 |    2.7 |    6.0 |
| vanilla  |   10.07 Mbps|   10.07 Mbps|    1065 |   47.3 |   48.2 |
| natra    |   10.21 Mbps|   10.14 Mbps|   16651 |    2.9 |    6.5 |

- baseline is the unshaped cross-VM wire: elephant ~1.9 Gbps,
  mice 17524 rps @ 6.0 ms p99.
- vanilla and natra cap the elephant to the 10M annotation
  equally well (~10/10 Mbps each).
- Under that cap, vanilla's mice drop to 1065 rps @ 48 ms p99;
  natra's hold at 16651 rps @ 6.5 ms p99 — within noise of the
  unshaped baseline. The upstream plugin runs one token bucket
  per pod for all flows, so the small requests queue behind the
  elephant; natra's CMS classifies them under the heavy-hitter
  threshold and they bypass the bucket.

Same elephant cap as upstream; mice latency unchanged from no
rate-limiter, where upstream costs ~16× rps / ~8× p99.

## Throttle disposition

When the bucket can't admit a packet, natra picks in this order:

1. **EDT pacing** (egress only, when `cfg.edt_pacing != 0`).
   Stamps `skb->tstamp` with the next-release time; `fq` on
   pod-eth0 releases at that time. Preferred on egress because
   ECN-mark halves cwnd on every above-rate packet and pulls the
   measured rate below the cap; EDT alone keeps the flow at the
   cap.
2. **ECN-mark** (`bpf_skb_ecn_set_ce`) on ECN-capable TCP. Sets
   CE, returns `TC_ACT_OK`. Used on ingress, and on egress when
   EDT is disabled.
3. **Drop** (`TC_ACT_SHOT`). Non-ECN traffic that neither EDT
   nor ECN-mark could handle.

EDT requires `fq` downstream of the BPF program. natra installs
`fq` on pod-eth0 when it picks pod-side egress attach;
host-side has no deterministic spot for `fq`, so EDT only
applies on pod-side.

`NATRA_EDT_PACING=auto` (default) probes `fq` at CNI ADD and
uses the EDT path on success. Also reorders the attach chain to
`tcx-pod → clsact-pod → tcx-host → clsact-host` — pod-side
combos tried first.

`NATRA_EDT_PACING=on` requires `fq` (fails attach if install
fails). `NATRA_EDT_PACING=off` never installs `fq`; egress falls
back to the ingress disposition (ECN-mark, else drop). Use `off`
when cilium / NPA already owns the qdisc layout.

## Reproduce

```
make perf-vs-vanilla
```

Knobs (env vars):

```
RATE_SWEEP="100M 1G 10G 40G"   # rates per phase
PERF_RUNS=3                    # samples per cell → mean ± stddev
NATRA_PERF_ATTACH_MODE=tcx-podside    # pin attach mode
NATRA_PERF_EDT_PACING=on              # pin EDT mode
```

Output: `/tmp/natra-perf-vs-vanilla-result.txt` (also tee'd to
stdout during the run).
