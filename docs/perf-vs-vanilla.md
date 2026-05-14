# natra vs upstream bandwidth

Head-to-head against `containernetworking/plugins/bandwidth` on a
k3d rig. Same workloads against three configurations: baseline
(no rate-limiter), natra, upstream HTB.

This doc covers the k3d-based comparison (single Linux kernel,
software dataplane on colima/LinuxKit). For a real-kernel-isolated
measurement of natra alone — two lima VMs, each running its own
kernel, joined into one k3s cluster — see
`scripts/vm-rig/README.md`. The vm-rig runs the bidi-iperf and
hey-HTTP-mice assertions against the cross-VM virtual NIC pair;
it's been built and unit-tested but not yet captured here as a
table (blocked on `socket_vmnet` install on the operator's Mac;
once that's in, `make test-vm` produces the numbers).

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

Cluster B's init container fetches the `bandwidth` plugin from
`containernetworking/plugins` v1.5.1 (k3d's base image doesn't
ship it) and `modprobe ifb` on each node so HTB-on-IFB can
install.

Pre-measurement normalizations:

- **HTB burst patch (vanilla only).** kubelet sets HTB burst to
  ~150 seconds of credit (~193 MB on a 10 Mbps annotation). The
  script overrides each pod's HTB class to `burst 1mb cburst 1mb`
  before measuring. natra's bucket defaults to 0.5 sec of credit
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
| ingress   | natra    | 10.29 Mbps | 11.49 Mbps          |
| ingress   | upstream | 10.04 Mbps |  9.64 Mbps          |
| egress    | natra    |  8.88 Mbps | 12.79 Mbps          |
| egress    | upstream | 10.11 Mbps |  9.61 Mbps          |

Rig: colima aarch64, LinuxKit ~6.8.x, k3d v5.7.4, flannel
host-gw, software dataplane (no NIC offload). Single sample.
natra rows from the latest run (post EDT-first egress reorder,
post 0.5× burst-default); the upstream rows are carried forward
from an earlier run when colima's LinuxKit kernel still shipped
the `ifb` module (the script needs it for HTB-on-IFB egress
shaping, and current colima images don't have it).

The 10M egress elephant at 8.88 Mbps (≈11% under cap) is
single-sample noise on this rig — the same path under
concurrent traffic in Workload 2 measures 9.70 Mbps (3% under),
and the L4 e2e GH runner measures `mean=10.37Mbps stddev=0.17`
on the same 10M annotation. At higher rates the elephants land
within 5% of cap (1.05 Gbps on 1G, 10.19 Gbps on 10G).

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

| Plugin                | iperf ing  | iperf eg  | Annotated mice RPS / p99 | Bystander RPS / p99 |
|-----------------------|------------|-----------|--------------------------|---------------------|
| baseline (no plugin)  | ~60 Mbps   | ~57 Mbps  |   43 / 4884 ms           | 8162 / 41 ms        |
| natra                 | 10.17 Mbps | 9.70 Mbps | 6776 /   61 ms           | 7848 / 48 ms        |
| upstream `bandwidth`  | 10.59 Mbps | 8.67 Mbps |   12 / 5715 ms           | 8519 / 40 ms        |

Single sample. natra and baseline rows from the latest run;
upstream row carried forward from the previous run (colima
LinuxKit missing `ifb`). Baseline mice are slow under concurrent
load because the elephant saturates colima's shared software
dataplane — the bucket isn't what's hurting them, the wire is.
With either plugin's elephant capped at 10 Mbps, the mice get
the wire back. Read in three pieces:

- **Elephant cap.** natra ingress 10.17 Mbps and egress 9.70 Mbps
  — both inside 5% of the 10M cap. The egress number is the
  post-reorder behavior; previously this row was 6.77 Mbps
  because ECN-mark fired first on every above-rate egress packet.
- **Annotated mice.** natra 6776 RPS / p99 61 ms vs vanilla 12
  RPS / p99 5715 ms. CMS classification lets each fresh-flow
  hey request bypass the bucket; HTB queues everything against
  the same 10 Mbps slot, so mice wait behind the elephant. The
  61 ms p99 is itself a post-reorder improvement (was 262 ms
  pre-fix because ECN-cwnd-collapse held elephant tokens longer
  than necessary).
- **Bystander.** Neither plugin attaches anything to unannotated
  pods. The bystander p99 sits at 48 ms under natra vs 40 ms
  under vanilla — structural cost from a paced elephant sharing
  the node (softirq time, NIC ring contention, cache pressure),
  not from natra touching the bystander.

## Gaps in this comparison

What these numbers don't support, and what would close each:

- **Real-NIC behavior**: TSO/GRO/LRO, hardware TX timestamping —
  none exercised. The BPF programs see whatever GRO shape colima
  produces. → cloud-VM or bare-metal rig.
- **Cross-kernel wire**: k3d "nodes" share one Linux kernel; the
  inter-node fabric is a software bridge, not a switch. ECN-CE
  fires but no router queues. → `make test-vm` (lima two-VM rig,
  partial coverage), cloud-VM (full).
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
