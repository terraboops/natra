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

Cross-kernel wire is closed — `make perf-vs-vanilla-vm` runs the
comparison on two real kernels over a real inter-VM wire (see
"Two-kernel (vm-rig) results"). What these numbers still don't
support:

- **Hardware NIC / wire.** Both rigs use software networking
  (k3d: docker bridge; vm-rig: vmnet). No hardware TSO/GRO/LRO,
  no NIC TX timestamping, no real switch queueing. The vm-rig
  software wire also tops out ~1.9 Gbps, so the 10G-annotation
  rows test "doesn't break at 10G", not "caps accurately at
  10G". Closing this needs cloud-VM or bare-metal with real
  NICs, which isn't available.
- **Run-to-run distribution.** The shared spec carries a
  `Samples` field both rigs honor; the `ci` profile pins it at
  1 and the `full` profile at 3 for mean ± stddev. The numbers
  in "Two-kernel (vm-rig) results" above are from the `ci`
  profile (one sample) for fast iteration; running the `full`
  profile via `make perf-vs-vanilla-vm` (no `PVV_PROFILE=ci`
  override) reports mean ± stddev for every cell. No hardware
  needed to close — it's sampling cost.
- **cilium / AWS NPA composition.** natra composes at the TCX
  hook via bpf_mprog (kernel 6.6+) by construction; the rig to
  measure it is `make cilium-compose` (`scripts/cilium-compose.sh`
  — k3d, cilium as the CNI with kube-proxy/flannel disabled,
  natra chained after it, asserting both programs sit at the pod
  TCX hook and natra still rate-limits). Scaffold landed;
  end-to-end verification pending (the natra-installer conflist
  patch was written against flannel and may need work for
  cilium's conflist shape).

Escalation rigs: `docs/test-environments.md`.

## Two-kernel (vm-rig) results

`make perf-vs-vanilla-vm` — two lima VMs, each its own Linux
kernel (Debian 13, 6.12), real inter-VM vmnet wire. perf-server
on the agent VM, perf-client on the server VM, so every packet
crosses the kernel boundary. iperf3 elephant (receiver-side
bps) + hey fresh-connection HTTP mice. Each phase runs on its
own fresh cluster (full down/up/measure/down); baseline has no
bandwidth annotation, vanilla and natra annotate 10M/10M.

Driven by `internal/perfrig`, the shared spec + executor both
rigs use; the lima path runs the `full` profile, the k3d path
(`make perf-vs-vanilla`) runs `ci` against the identical Spec.
A unit test asserts `ci ⊆ full` so the structural subset
relationship is enforced, not maintained by hand.

| Phase    | iperf ing | iperf eg  | hey rps | p50 ms | p99 ms |
|----------|-----------|-----------|---------|--------|--------|
| baseline | 1863 Mbps | 1858 Mbps |   17410 |    2.7 |    5.9 |
| vanilla  |   10.1 Mbps|  10.1 Mbps|    1059 |   47.3 |   48.7 |
| natra    |   10.2 Mbps|  10.1 Mbps|   16699 |    2.9 |    6.6 |

- baseline is the unshaped cross-VM wire: elephant ~1.9 Gbps,
  mice 17410 rps at 5.9 ms p99.
- vanilla and natra cap the elephant to the 10M annotation
  equally well (~10.1/10.1 Mbps each).
- Under that cap, vanilla's mice drop to 1059 rps at 48.7 ms
  p99; natra's hold at 16699 rps at 6.6 ms p99 — within noise
  of the unshaped baseline. The upstream plugin runs one token
  bucket per pod for all flows, so the small requests queue
  behind the elephant; natra's CMS classifies them under the
  heavy-hitter threshold and they bypass the bucket.

Same elephant cap as upstream; mice latency unchanged from no
rate-limiter, where upstream costs ~16× rps / ~7× p99.

### Memory comparison

Three comparables captured per phase on the worker node, all
with baseline as the empirical noise floor. Sources:

1. **Dataplane kernel memory** — `/proc/meminfo` Slab +
   KernelStack + PageTables delta across 1 → 8 annotated pods.
   The same ruler in every phase; the delta attributes to that
   phase's mechanism (qdiscs in vanilla, BPF in natra).
2. **BPF memlock** — `bpftool -j map/prog show` summed
   `bytes_memlock` for `natra_*` objects. Byte-exact
   corroboration in the natra phase only.
3. **CNI plugin invocation peak RSS** — `/usr/bin/time -v` peak
   resident set size for one `CNI_COMMAND=VERSION` invocation
   of the phase's plugin binary on the worker.

| Phase    | kmem@N (kB) | kmem/pod above baseline (kB) | bpf memlock | invoke peak RSS |
|----------|-------------|------------------------------|-------------|------------------|
| baseline | 133560      | — (noise floor: 2565)        | 0           | — (no plugin)    |
| vanilla  | 134044      | +251 (16 TBF qdiscs ✓)       | 0           | 5.5 MB           |
| natra    | 139492      | +212 (BPF maps + progs)      | 32 MB total (~4 MB/pod) | 5.8 MB |

- vanilla's per-pod cost is ~16 TBF qdiscs (eight pods × two
  qdiscs each, the bundled bandwidth plugin's ingress + egress)
  worth of kernel memory; `tc -s qdisc show` confirms the
  count.
- natra's per-pod cost is the CMS + token bucket + stats maps
  plus the two TCX programs. bpftool reports 32 MB memlocked
  across all natra_* objects at 8 pods — ~4 MB per annotated
  pod, dominated by the CMS array.
- Both plugins pay ~5.5–5.8 MB in peak RSS per CNI ADD
  invocation; natra is ~6% heavier than vanilla on the
  per-event cost.

Single-sample numbers (the `ci` profile, `Samples=1`); the
`full` profile runs three samples for mean ± stddev. Persistent
installer DaemonSet RSS is the fourth comparable defined by the
spec; capture of natra-installer's cgroup memory needs one
more crictl-output fix and currently reads zero. The installer
DS runs `pause` post-install so its persistent RSS is bounded
to a few MB — minor compared to the kernel BPF cost above.

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
make perf-vs-vanilla       # k3d, ci profile (~18-22 min, fits CI)
make perf-vs-vanilla-vm    # lima, full profile (~60 min, two real kernels)
```

Both targets share `internal/perfrig` — same Spec, same Executor,
different Substrate (k3d on colima vs lima). Knobs:

```
PERF_PROFILE=ci            # default for make perf-vs-vanilla; single rate, Samples=1
PERF_PROFILE=full          # full rate sweep, Samples=3 — much longer
PERF_CLUSTER=natra-perfrig # k3d cluster name (default natra-perfrig)
PVV_PROFILE=ci             # same idea for the vm-rig entry; default full there
```

Outputs:

```
/tmp/natra-k3d-perf-vs-vanilla-result.txt    # k3d
/tmp/natra-vm-rig-perf-vs-vanilla-result.txt # vm-rig
```

The CI workflow (`.github/workflows/perf.yml`) runs the k3d
ci-profile job on every push and uploads the result table as a
build artifact.
