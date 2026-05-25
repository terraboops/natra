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

## Workloads

The ci profile (PERF_PROFILE=ci, the default) runs each of three
workloads once per phase: an iperf elephant sweep, the mixed
(elephant + annotated mice + bystander) workload, and the
three-comparable memory workload. Full profile (PERF_PROFILE=full)
extends each across rates and samples.

### iperfSweep — per-direction elephant throughput

iperf3 against the annotated perf-server, ingress (forward) +
egress (-R), receiver-side `end.sum_received.bits_per_second`.
Per-rate pods (perf-server-r10m, perf-server-r1g, perf-server-r10g)
deployed with the matching annotation; mean ± stddev across
Samples per (phase × rate) cell.

Full profile on k3d (3 samples, 3 rates):

| Phase    | rate | iperf ing Mbps  | iperf eg Mbps   |
|----------|------|-----------------|-----------------|
| baseline | 10M  | 57821 ± 342     | 51971 ± 446     |
| baseline | 1G   | 57284 ± 109     | 51955 ± 197     |
| baseline | 10G  | 58188 ± 405     | 52778 ± 294     |
| vanilla  | 10M  | **15.3 ± 5.0**  | **50.0 ± 50.3** |
| vanilla  | 1G   | 1140 ± 156      | 1230 ± 0        |
| vanilla  | 10G  | 9760 ± 134      | 9838 ± 0        |
| natra    | 10M  | **10.1 ± 0.1**  | **10.1 ± 0.0**  |
| natra    | 1G   | **1024 ± 8**    | **1047 ± 0.3**  |
| natra    | 10G  | **10078 ± 74**  | **10169 ± 21**  |

Rig: colima aarch64, LinuxKit ~6.12, k3d v5.7.4, flannel host-gw,
software dataplane (no NIC offload). The colima inter-container
wire caps single-stream around ~58 Gbps unshaped (the baseline
rows above); at 1G and 10G annotations both plugins are
wire-limited, not shaper-limited, so the "did it cap" question
only has a clean answer at 10M.

- **baseline** is the unshaped wire — same number at every rate
  because no shaper engages.
- **natra** holds 10M within 0.1%, with sub-0.1 Mbps stddev
  across samples. Tightest cap of any plugin × rate combination
  in the table.
- **vanilla at 10M** shows the burst-overshoot variance — 50 ± 50
  on egress means some samples land at ~10 Mbps and others at
  100+ Mbps. The upstream bandwidth plugin's TBF burst is
  patched to 1 MB in the node root netns, which reaches the
  host-side IFB qdisc but not the pod-eth0 egress TBF (which
  lives in the pod netns and keeps its default kubelet burst,
  ~150 s of credit). A pod-netns-aware patch was attempted and
  rejected — the nsenter approach broke on k3d's busybox-based
  rancher/k3s image and made overshoot worse. Documented as a
  k3d limitation.
- **At 1G and 10G** both plugins are wire-limited (~1.2 Gbps
  single-stream colima cap, ~10 Gbps multi-stream effective).
  Reads as "doesn't break under high annotation" rather than
  "caps accurately at the annotation."

### mixed — elephant + annotated mice + bystander mice

`iperf3 --bidir` against the annotated perf-server (drains both
buckets at once) while two `hey -c 50 -z 25s -disable-keepalive`
runs hit perf-server (annotated mice — CMS fast-pass story) and
`bystander` (unannotated pod on the same worker). Fresh-connection
requests at ~5-7 KB each, well under the 125 KiB heavy-hitter
threshold at 10 Mbps.

Numbers from the GH Actions `ci` profile run on commit
`f06c4dd`:

| Phase    | iperf ing | iperf eg | pod rps / p99 | bystander rps / p99 | mice total |
|----------|-----------|----------|---------------|---------------------|------------|
| baseline | 2894 Mbps | 6631 Mbps |  2068 /  69 ms |  2077 /  67 ms     | 4145       |
| vanilla  |   16 Mbps |    6 Mbps |    16 / 5584 ms |  4441 /  64 ms     | 4457       |
| natra    |   10 Mbps |   10 Mbps |  2644 /  42 ms |  2672 /  42 ms     | **5316**   |

The right way to read this is the **mice total** column — the
sum of annotated + bystander RPS — alongside the per-row split:

- **Elephant cap.** natra holds the --bidir elephant at 10/10
  Mbps; vanilla shows the iperfSweep overshoot pattern (the host-
  side IFB TBF is patched, the pod-netns egress TBF isn't, so the
  cap is loose).

- **Annotated mice.** natra 2644 rps / p99 42 ms ≈ baseline (2068
  / 69 ms). vanilla collapses to 16 rps / p99 5584 ms — **130×
  worse than baseline** on RPS, p99 from 69 ms to 5.6 seconds.
  The upstream token bucket queues every annotated-pod flow
  against the same 10 Mbps slot; natra's CMS fast-passes
  fresh-flow HTTP under the heavy-hitter threshold so it bypasses
  the bucket.

- **Bystander vs. mice total.** Neither plugin attaches anything
  to the unannotated bystander, so absolute bystander RPS isn't
  a "plugin cost" — it's how much of the freed worker capacity
  (CPU, software dataplane, conntrack) the bystander gets in
  contention with the annotated mice.

  Capping the elephant frees roughly the same spare worker
  capacity in vanilla and natra. **Vanilla's bystander column
  (4441) looks higher than natra's (2672) only because vanilla
  collapses annotated mice (16 rps) and hands their share to the
  bystander.** natra honors the annotated mice fairly, so the
  spare capacity splits ~evenly between annotated (2644) and
  bystander (2672). The right comparison is the *total*
  mice-class throughput — and natra delivers 5316 rps, **+19%
  more total request work than vanilla's 4457** and **+28% more
  than baseline's 4145** (baseline's elephant dominates worker
  resources).

  Read the single bystander column alone and natra looks worse to
  the neighbor; read the row as a whole and natra is delivering
  more useful request work *and* respecting the annotated bucket
  the user actually asked for. The per-column reading is the
  trap; the row-level reading is the story.

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
- **AWS NPA composition.** The vm-rig now runs cilium as its
  CNI (TCX dataplane, kube-proxy replacement), so natra-with-
  cilium coexistence at the pod TCX hook is exercised on every
  `make perf-vs-vanilla-vm` run. AWS NPA (which also attaches
  at TCX via bpf_mprog) is the remaining unmeasured composition
  case — it doesn't run locally; closing it needs an EKS cluster
  with NPA enabled, which isn't available.

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

The vm-rig **uses cilium as its CNI** (TCX dataplane, kube-proxy
replacement, helm-installed on first server boot). natra chains
after cilium in the conflist. This raises the fidelity of the
two-real-kernel measurement: it's not just two real kernels, it's
two real kernels running the same dataplane (cilium) a production
cluster usually runs.

**Cilium is a proxy for the class** of BPF-based network-policy
CNIs that attach via `tc clsact` (and increasingly TCX) on
host-side veths. The most production-relevant member of that
class for natra is **AWS NPA** (`aws-network-policy-agent`),
which runs on EKS and isn't reachable from a local rig; cilium
on the vm-rig stands in for it. The composition story natra
needs to support — "another BPF dataplane is already on the
veth; coexist cleanly via `bpf_mprog`, see traffic, enforce" —
is the same regardless of which CNI is at the other end of the
hook. What cilium tests, AWS NPA inherits.

Full-profile flannel-host-gw vm-rig numbers (3 samples per
(phase × rate), commit `bosfo9o35`):

**iperfSweep:**

| Phase    | rate | iperf ing Mbps | iperf eg Mbps  |
|----------|------|----------------|----------------|
| baseline | 10M  | 716 ± 5        | 728 ± 8        |
| baseline | 1G   | 719 ± 11       | 718 ± 9        |
| baseline | 10G  | 696 ± 7        | 699 ± 6        |
| vanilla  | 10M  | **9.9 ± 0.3**  | **10.1 ± 0.0** |
| vanilla  | 1G   | 725 ± 21       | 713 ± 16       |
| vanilla  | 10G  | 704 ± 3        | 706 ± 3        |
| natra    | 10M  | **10.1 ± 0.1** | **10.2 ± 0.0** |
| natra    | 1G   | **1005 ± 13**  | **1030 ± 1**   |
| natra    | 10G  | **1687 ± 13**  | **1671 ± 28**  |

**mixed (with mice-total column):**

| Phase    | iperf ing       | iperf eg       | annotated mice rps/p99 | bystander rps/p99 | mice total    |
|----------|-----------------|----------------|------------------------|-------------------|---------------|
| baseline | 396 ± 22 Mbps   | 330 ± 46 Mbps  |  332 ± 29 / 221 ± 17 ms | 333 ± 28 / 225 ± 15 ms | 665 ± 57  |
| vanilla  | 10.0 ± 0.5 Mbps | 10.0 ± 0.2 Mbps |    58 ± 15 / 1829 ± 113 ms | 5519 ± 341 / 23 ± 3 ms | 5576 ± 345 |
| natra    | 10.0 ± 0.1 Mbps | 10.0 ± 0.0 Mbps | **5922 ± 149 / 21.8 ± 0.4 ms** | 6155 ± 143 / 21.5 ± 0.3 ms | **12077 ± 127** |

natra at `auto`-resolved `tcx-podside` on both directions
(BPF memlock 32 MB byte-exact, every sample). Read the row,
not the column:

- **Elephant cap.** vanilla and natra both hold 10M within
  ~3% across every sample; vanilla's wider sample variance
  (10.0 ± 0.5 vs natra's 10.0 ± 0.1) is the burst-overshoot
  story showing through. Higher rates (1G, 10G) are wire-
  limited on the lima inter-VM software wire (~720 Mbps
  single-stream), so 1G/10G annotations read as "doesn't
  break" rather than "caps accurately"; natra at 1G measures
  1005 Mbps and at 10G measures 1687 Mbps, which sits above
  the baseline wire — an unresolved measurement puzzle
  documented as the natra-faster-than-baseline follow-on.
- **Annotated mice.** natra delivers **5922 rps / p99 22 ms
  with stddev 149 rps** (extremely consistent); vanilla
  collapses to **58 rps / p99 1829 ms** — natra is ~100× more
  rps and ~85× better p99 at the same cap. The CMS fast-pass
  routes fresh HTTP requests under the heavy-hitter
  threshold, around the bucket; vanilla queues every flow
  against the same 10M slot.
- **Mice total.** natra's row sum is **12077 ± 127 rps —
  +117% over vanilla's 5576 ± 345 and 18× baseline's 665 ±
  57**. The cap frees worker capacity that baseline's elephant
  was hogging; natra splits it fairly (annotated 5922 ≈
  bystander 6155), vanilla collapses annotated and hands
  their share to the bystander (58 + 5519). natra's mice-
  total stddev is ~1% (127/12077); vanilla's is ~6% (345/
  5576) — natra's data is 6× cleaner across samples.

### BPF-NPA composition — measured, working

Cilium as CNI on the vm-rig comes up cleanly, the
natra-installer DS rolls out and writes
`00-natra-05-cilium.conflist` (natra chained after cilium),
both natra BPF programs attach at `tcx-podside`, and the natra
phase shapes traffic to the annotated 10 Mbps rate.

Result on a clean cilium-as-CNI / kube-proxy-handling-Services
run (lima vm-rig, `ci` profile, two real Linux 6.12 kernels):

| Phase    | iperf elephant         | annotated mice          | bystander            | mice total |
|----------|------------------------|-------------------------|----------------------|------------|
| baseline | 1500 / 1538 Mbps       | 773 rps  p99 124 ms     | 774 rps p99 119 ms   | 1547       |
| natra    | **10.2 / 10.2 Mbps**   | **2423 rps p99 74 ms**  | 2461 rps p99 77 ms   | **4884**   |

natra honors the 10M annotation within 2%, keeps annotated
mice fast (CMS fast-pass under cilium is the same fast-pass
that works under flannel), bystander unaffected, mice total
**3.2× baseline** — the cap frees worker capacity that
baseline's elephant was hogging, fairly split between annotated
and bystander. `bpftool` on the worker reports 32 MB of natra
BPF memlocked, byte-exact corroboration that the programs are
loaded and running.

Because cilium proxies for the broader BPF-NPA class, this is
the **AWS NPA composition story** too, ahead of any actual EKS
run: a BPF policy enforcer that owns the host-side veth and a
TCX-attached natra coexist via `bpf_mprog` at the pod-eth0
hook, no traffic redirection between them.

#### The one cilium setting that matters

Only one cilium helm flag is load-bearing for natra coexistence:

- **`cni.exclusive=false`** — cilium's CNI installer defaults
  to exclusive mode, which actively **renames any other
  conflist** in `/etc/cni/net.d/` with a `.cilium_bak` suffix.
  natra's chained `00-natra-05-cilium.conflist` was being
  moved aside as fast as the installer wrote it, so containerd
  never saw natra in the chain at all. (The first sign was
  `/var/log/natra-cni.log` staying empty — the binary was
  never invoked by CNI ADD.) Setting `cni.exclusive=false`
  tells cilium to coexist with sibling conflists.

KPR (kube-proxy replacement) and BPF host-routing
(`bpf_redirect_peer` / `bpf_redirect_neigh`) turned out to be
**orthogonal** to natra coexistence. An earlier write-up of
this section theorized that those redirect helpers would
bypass natra's `tcx-podside` attach by short-circuiting between
pod-eth0 and host-veth without traversing pod-eth0's TC chain;
that theory was wrong. With `cni.exclusive=false`, natra's
chained conflist is in place, kubelet walks the chain on every
CNI ADD, the BPF programs attach, and TCX runs on traffic
regardless of cilium's redirect choices.

Both configurations are validated on the vm-rig and have their
own opt-in target:

- **KPR-off cilium** (`VMRIG_CNI=cilium`,
  `make perf-vs-vanilla-vm-cilium`) — cilium as the CNI +
  policy enforcer, kube-proxy handling Services via iptables.
  Default cilium variant. More faithful AWS NPA proxy (NPA is
  a pure policy enforcer; doesn't replace kube-proxy).
- **KPR-on cilium** (`VMRIG_CNI=cilium-kpr`,
  `make perf-vs-vanilla-vm-cilium-kpr`) — cilium replaces
  kube-proxy with socketLB + host-routing fast-path. cilium's
  full production configuration.

natra holds the 10M cap in both:

| Variant   | iperf elephant (natra phase) | annotated mice (natra phase) |
|-----------|------------------------------|------------------------------|
| KPR off   | 10.2 / 10.2 Mbps             | 2423 rps p99 74 ms           |
| KPR on    |  9.9 / 10.2 Mbps             | 2449 rps p99 80 ms           |

#### Production guidance

If you run cilium alongside natra, install cilium with:

    helm install cilium cilium/cilium ... \
      --set cni.exclusive=false

That's the only essential override. KPR / host-routing /
socketLB can stay at whatever you'd normally run for your
cluster — natra at `tcx-podside` engages either way.

To reproduce locally:

    make perf-vs-vanilla-vm-cilium       # KPR-off cilium
    make perf-vs-vanilla-vm-cilium-kpr   # KPR-on cilium

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
make perf-vs-vanilla            # k3d (flannel host-gw), ci profile
                                # (~18-22 min, fits CI)
make perf-vs-vanilla-vm         # lima, flannel host-gw CNI (default).
                                # Canonical two-kernel headline.
make perf-vs-vanilla-vm-cilium  # lima, cilium as CNI (cni.exclusive=
                                # false, KPR off). Proxies AWS NPA;
                                # exercises bpf_mprog coexistence at
                                # pod TCX.
```

Both substrates share `internal/perfrig` — same Spec, same
Executor, different Substrate (k3d on colima vs lima). The
vm-rig's CNI choice is independent and toggled via `VMRIG_CNI`:

```
VMRIG_CNI=flannel          # default; flannel host-gw (lima-server-
                           # flannel.yaml + lima-agent-flannel.yaml)
VMRIG_CNI=cilium           # cilium as CNI (lima-server-cilium.yaml
                           # + lima-agent-cilium.yaml). Set by
                           # perf-vs-vanilla-vm-cilium implicitly.
```

Other knobs:

```
PERF_PROFILE=ci            # default for make perf-vs-vanilla; single rate, Samples=1
PERF_PROFILE=full          # full rate sweep, Samples=3 — much longer
PERF_CLUSTER=natra-perfrig # k3d cluster name (default natra-perfrig)
PVV_PROFILE=ci             # same idea for the vm-rig entry; default full there
NATRA_ATTACH_MODE=…        # override the installer DS attach mode
                           # (auto by default; valid: tcx-podside,
                           # tcx-hostside, clsact-podside, clsact-
                           # hostside). Logged at run start so
                           # post-mortems are unambiguous.
```

Outputs:

```
/tmp/natra-k3d-perf-vs-vanilla-result.txt    # k3d
/tmp/natra-vm-rig-perf-vs-vanilla-result.txt # vm-rig
```

The CI workflow (`.github/workflows/perf.yml`) runs the k3d
ci-profile job on every push and uploads the result table as a
build artifact.
