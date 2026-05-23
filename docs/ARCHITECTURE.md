# Natra Architecture

## Overview

Natra is a CNI plugin that rate-limits Pod traffic in either direction.
It reads the standard `kubernetes.io/ingress-bandwidth` and
`kubernetes.io/egress-bandwidth` annotations, applies the limit only
to the heavy flows in each direction (so short-lived mice keep flowing
at line rate), chains behind whatever main CNI is in place (kindnet,
calico, etc.), and implements the rate-limit logic in BPF.

A pod with only one annotation gets only that direction's program
attached. A pod with neither annotation gets nothing — the BPF object
is never loaded, so the unannotated case costs zero.

## Components

### CNI plugin

`cmd/natra`, `pkg/cni/`

Invoked by kubelet during Pod network setup. Reads the CNI stdin
payload, pulls the bandwidth values out of `runtimeConfig.bandwidth`
(falling back to `runtimeConfig.podAnnotations` if absent), loads the
embedded BPF object, and attaches one program per direction with a
rate. Past stdin parsing every error path is fail-open — log to
stderr, return success, let the Pod start unrate-limited rather than
stuck in ContainerCreating.

### BPF programs

`bpf/natra.bpf.c` is what the plugin loads in production. It exports
two `SEC("tc")` entries — `natra_ingress` and `natra_egress` — that
share an inlined `natra_classify(skb, dir)` body. `bpf/vanilla.bpf.c`
is the matching token-bucket-on-every-packet emulator (`vanilla_ingress`
/ `vanilla_egress`) of the upstream `containernetworking/plugins/bandwidth`
plugin; only the L5 perf test loads it, for head-to-head comparison.

The per-direction state lives in direction-keyed map slots:

| Map               | Layout                                                                                |
|-------------------|---------------------------------------------------------------------------------------|
| `natra_config_map`| key = direction (0=ingress, 1=egress); 2 slots                                        |
| `natra_bucket_map`| key = direction; 2 slots (each: tokens, last_update_ns, next_release_ns + spin_lock)  |
| `natra_stats_map` | key = direction × 6 + slot (passed/throttled/hh_hits/ecn_marked/edt_delayed/dropped); 12 slots, per-CPU |
| `natra_cms_map`   | key = direction × 131072 + row × 32768 + col; 262144 cells × 16 bytes = 4 MiB         |

CMS halves are independent per-direction. Asymmetric workloads (HTTP
downloads, file uploads, streaming) make a flow heavy in one direction
and mice on the other (just ACKs); a shared CMS would falsely classify
ACK streams as heavy. Total CMS cost is 4 MiB per pod, sized to
cover ~50K-flow workloads without saturating.

The pipeline is two stages on the Pod-side veth:

1. **Count-Min Sketch.** 4-row × 32768-column sketch per direction
   (16 bytes per cell after alignment padding, 2 MiB per direction).
   The 5-tuple (src/dst IP, src/dst port, proto) hashes into one cell
   per row via FNV-1a
   mixed with per-row seeds. The estimator is `min` across the four
   rows. A flow is "heavy" when its estimate exceeds the
   `heavy_hitter_threshold`. Each cell carries a `last_decay_idx`
   so cells fade lazily on access — old elephants stop counting
   without a sweeper. CMS counters are non-atomic; lost increments
   give slightly conservative classification (the safe direction).

2. **Token bucket — heavy hitters only.** A per-direction bucket
   protected by `bpf_spin_lock`. Mice (below threshold) bypass the
   bucket entirely and return `TC_ACT_OK`. Heavy flows pay tokens
   proportional to `skb->len`; when the bucket is starved, the
   disposition helper picks the next action (EDT-pace on egress
   when available, else ECN-mark, else drop — see below).
   `bpf_ktime_get_ns()` is read *outside* the
   spin-locked region — helper calls inside `bpf_spin_lock` are
   verifier-rejected.

3. **Throttle disposition.** When the bucket can't admit a packet,
   natra picks the disposition in this order:

   - **EDT pacing** (egress only, when `cfg.edt_pacing != 0`) —
     advance the bucket's `next_release_ns` by
     `bytes * 8e9 / rate_bps`, stamp `skb->tstamp = next_release_ns`,
     and pass. The `fq` qdisc on pod-eth0 holds the packet until
     that time. Drop becomes "delay", with no congestion signal back
     to the sender — TCP keeps cwnd, just gets paced.
   - `bpf_skb_ecn_set_ce` — set CE on ECN-capable packets and pass
     (TC_ACT_OK). Receiver's TCP backs off without a retransmit.
     Used on ingress and on egress when EDT is disabled. Requires
     the peer to have negotiated ECN (`tcp_ecn=1` on either end of
     the connection).
   - **Drop** (`TC_ACT_SHOT`) — fallback for traffic neither EDT
     nor ECN-mark could handle (ingress non-ECN; egress non-ECN
     with EDT disabled).

   EDT precedes ECN on egress-with-EDT because ECN-mark halves cwnd
   on every above-rate packet, which under-throttles below the
   annotated rate (cwnd drops faster than the bucket refills). EDT
   handles the typical above-rate packet without a retransmit
   amplifier.

   EDT delay is bounded at 50 ms — above that, the disposition
   falls through to ECN-mark. The bound caps how many in-flight
   packets fq holds (rate × 50 ms ≈ 42 MTU-sized packets at 10
   Mbps) so a sustained over-rate flow can't accumulate arbitrary
   queue depth and starve same-node neighbors of softirq time.
   The bucket's debt counter still advances on every packet, so
   rate enforcement is preserved regardless of which disposition
   delivers the packet. Steady state under sustained over-rate
   is "most packets EDT'd within the 50 ms window, occasional
   ECN signal when the queue target would overshoot."

   Per-direction stats break the throttled-packet count down into
   `STAT_ECN_MARKED`, `STAT_EDT_DELAYED`, `STAT_DROPPED` (their sum
   equals `STAT_THROTTLED`).

The upstream bandwidth plugin (single per-pod token-bucket qdisc on
IFB — HTB in v1.5.1, TBF in v1.6.0+) charges every packet, so one
elephant flow eats every token and mice starve. The
CMS-then-bucket shape lets mice fast-pass, which is what the L5 test
(`TestScenarioMixedVsVanilla` in both directions) measures.

The default heavy-hitter threshold is rate-scaled: each pod gets
`threshold = max(16 KiB, rate_bps × 100 ms / 1000)`, computed at
CNI ADD from the pod's annotated rate. A 10 Mbps pod gets ~125 KiB,
a 1 Gbps pod gets ~12.5 MiB. The CMS counts bytes (not packets),
so this is invariant of GRO super-packet coalescing. Operators can
override per-pod via the JSON annotation form's
`heavyHitterThreshold` field or cluster-wide via
`NATRA_DEFAULT_HH_THRESHOLD` / `NATRA_FASTPASS_TIME_CONSTANT_MS`.

### DaemonSet installer

`deploy/cni-installer.yaml`. An init container copies the natra binary
to `/opt/cni/bin/`, runs `setcap cap_bpf,cap_net_admin,cap_perfmon,cap_sys_resource+ep`
on it, mounts bpffs, and patches every existing `*.conflist` in
`/etc/cni/net.d/` to add `{"type":"natra","capabilities":{"bandwidth":true}}`
as the last plugin in the chain. Pod readiness is gated on init
completion (no marker file). A `pause` sidecar keeps the Pod present
so the DaemonSet controller is happy.

`capabilities.bandwidth: true` causes kubelet to populate
`runtimeConfig.bandwidth.{ingressRate,ingressBurst,egressRate,egressBurst}`
on every Pod with either annotation. natra reads each direction's
fields independently.

The conflist patch is written to a `00-natra-<original>.conflist`
sibling file rather than in-place — containerd's CNI watcher races
with sandbox creation when the source conflist is rewritten, and the
sibling sorts ahead alphabetically so containerd picks it.

## Plugin flow

```
kubelet → CNI ADD on natra
         stdin = NetConf with prevResult + runtimeConfig.bandwidth
   ↓
parse stdin → resolve rate/burst per direction (ingress, egress)
   ↓
both directions absent? → print prevResult, no BPF load
   ↓
enter pod netns → load embedded BPF object
   ↓
for each direction with a rate:
   configure that direction's config + bucket slot
   attach the matching program (natra_ingress or natra_egress)
   ↓
pin maps under /sys/fs/bpf/natra/ for `natra dump-stats <containerID>`
   ↓
print prevResult unchanged → return success
```

If one direction's attach fails after the other succeeded, the
successful side is rolled back (link closed, pin removed) and the
plugin falls through to passthrough. The fail-open contract avoids
half-applied state.

CNI DEL removes the per-container pins:

- For tcx modes: per-direction link pins
  (`<containerID>-<side>-{ingress,egress}-link`, where `<side>` is
  `hostside` or `podside`).
- For clsact modes: no link pins; the kernel auto-detaches the
  filters when the chained CNI's DEL deletes the underlying veth.
- For all modes: per-container map pins
  (`<containerID>-{config,bucket,stats,cms}-map`).

`cmdDel` walks the pin dir and removes everything with the
`<containerID>-` prefix, regardless of which directions were attached.

## Fail-open

Past stdin parsing, every error path falls through to passthrough
with a note on stderr and to `/var/log/natra-cni.log`:

- BPF load / verifier rejection → passthrough
- Kernel too old / missing helpers → passthrough
- Attach failure (`BPF_LINK_CREATE` or `tc filter add`) on either
  direction → passthrough; the other direction is rolled back so
  state stays consistent
- Pin failure (`BPF_OBJ_PIN`) on a link → passthrough
- Pin failure on a map → continue without debug pins; the BPF
  attachment is not torn down (PinMaps is best-effort)
- Annotation malformed → that direction is treated as absent

## Annotation forms

Standard simple form:
```yaml
apiVersion: v1
kind: Pod
metadata:
  annotations:
    kubernetes.io/ingress-bandwidth: "10M"
    kubernetes.io/egress-bandwidth: "5M"
```

Either annotation may be omitted; that direction stays unattached.

Extended JSON form (overrides the parsed defaults; can appear on
either annotation):

```yaml
apiVersion: v1
kind: Pod
metadata:
  annotations:
    kubernetes.io/ingress-bandwidth: |
      {
        "rate": "10M",
        "burst": "20M",
        "heavyHitterThreshold": 131072
      }
```

`rate` and `burst` follow the k8s annotation convention (bits/sec
with SI/IEC suffixes); the parser divides by 8 to populate the
bytes/sec Config. `heavyHitterThreshold` is a raw byte count in
CMS units — 131072 = 128 KiB.

The parser is direction-agnostic — same fields and semantics apply on
the egress annotation.

## Kernel requirements

- tcx-* modes: kernel 6.6+ (uses `bpf_mprog`).
- clsact-* modes: kernel 5.x+.
- EKS: AL2023 or recent Bottlerocket for the `cap_bpf` ambient cap.

## Attachment

natra picks an attach mode along two orthogonal axes — the kernel
hook surface (TCX or clsact) and the veth half (host-side or
pod-side). Four explicit modes: `tcx-hostside`, `tcx-podside`,
`clsact-hostside`, `clsact-podside`. The default is `auto`, which
expands to an ordered fallback chain — natra tries each attempt
in sequence and uses the first that succeeds.

The order depends on whether EDT pacing is active (see below):

- `attachMode: auto`, `edtPacing: off`: tcx-host → tcx-pod →
  clsact-host → clsact-pod. Host-side first matches Cilium / AWS
  NPA's coexistence shape.
- `attachMode: auto`, `edtPacing: auto` (default): tcx-pod →
  clsact-pod → tcx-host → clsact-host. Pod-side first so the `fq`
  install on pod-eth0 sits downstream of the BPF program. Host-side
  attempts at the tail of the chain skip EDT but still attach.
- `attachMode: auto`, `edtPacing: on`: tcx-pod → clsact-pod. EDT is
  required, so host-side attempts are dropped from the chain
  entirely.

Explicit `attachMode` values (e.g. `tcx-hostside`) produce a
single-attempt list with no fallback — the operator asked for that
exact mode, so honor it. `edtPacing: on` with a non-pod attachMode
errors at CNI ADD instead of silently downgrading.

Selected via the `attachMode` field on the conflist entry or the
`NATRA_ATTACH_MODE` env on the install init container. Both
directions use the same mode; the choice is deployment-wide.

The natra BPF program is symmetric per direction: `natra_ingress`
processes packets in the pod-ingress direction regardless of which
veth-half it sits on. The loader handles the hook-direction flip
internally — on the host-side a pod-ingress packet arrives at the
host veth's egress hook, and vice versa.

### Hook: TCX (kernel 6.6+)

`pkg/bpf/loader.go::Attach` with `Hook: HookTCX` calls
`link.AttachTCX(...)` against the matching kernel hook
(`ebpf.AttachTCXIngress` / `ebpf.AttachTCXEgress`) and pins the
returned link to bpffs at
`/sys/fs/bpf/natra/<containerID>-<side>-<direction>-link`. The
kernel holds each program reference via its link until `cmdDel`
removes the pin. Composes via `bpf_mprog` with anything else
attaching at the same hook — that's the contract bpf_mprog
provides; the vm-rig (`make perf-vs-vanilla-vm`) runs cilium
as its CNI with natra chained after it, so a real composed
cilium + natra stack is exercised on every run.

### Hook: clsact (kernel 5.x+)

`pkg/bpf/loader.go::Attach` with `Hook: HookClsact` adds a `clsact`
qdisc (idempotent — the second direction's call is a no-op via
EEXIST) and calls `netlink.FilterReplace` to install the program on
the matching parent (`HANDLE_MIN_INGRESS` or `HANDLE_MIN_EGRESS`)
with `DirectAction: true`. The kernel's qdisc tree holds the
program references for the lifetime of the veth. Collides with
anything else attaching clsact on the same hook; whoever attached
last wins.

### Side: hostside (default)

`cmd/natra/main.go::resolveIfIndex` briefly enters the pod netns to
read the pod-eth0's veth peer ifindex via netlink, then returns to
the host netns for the actual attach. Same approach Cilium's
`generic-veth` chaining mode uses. Survives pod-side restrictions on
network admin caps, and is the default attach point Cilium and the
AWS network-policy-agent target — natra coexists with them on the
opposite veth half regardless.

### Side: podside

`resolveIfIndex` enters the pod netns and attaches inline against
`eth0` (typically). Kernel auto-cleans on netns teardown when the
pod terminates, with no host-side bpffs state to GC. Useful when the
host netns is locked down or already crowded with another BPF stack.

### Common prerequisites

- bpffs mounted at `/sys/fs/bpf` before natra runs. The DaemonSet's
  install init container handles this with
  `mountPropagation: Bidirectional` so kubelet's later CNI
  invocations see the mount.
- File caps:
  `setcap cap_bpf,cap_net_admin,cap_perfmon,cap_sys_resource+ep`
  on `/opt/cni/bin/natra`. Kubelet doesn't pass these through
  ambient, so the binary needs them via file caps.

## Open ends

Code-level gaps:

- IPv6: `parse_flow` returns -1 for non-IPv4, so IPv6 flows pass
  through unrate-limited in either direction.
- UDP: parsed for the 5-tuple, same fast path as TCP.
- CO-RE: BPF program currently uses fixed kernel headers; CO-RE would
  help on heterogeneous kernels.
- ebpf_exporter integration for the per-direction stats slots.

Properties that are claimed by construction but not yet validated
on a real rig (this list is what the test environments in
`docs/test-environments.md` would close):

- **aws-network-policy-agent coexistence.** The vm-rig now runs
  cilium as its CNI on every `make perf-vs-vanilla-vm` so the
  cilium+natra TCX coexistence is exercised end-to-end. AWS NPA
  (which also attaches at TCX via bpf_mprog) is the remaining
  unmeasured case — needs an EKS cluster with NPA, not local.
- **Real-NIC offload behavior.** TSO, GRO, LRO, hardware TX
  timestamping all reshape what the BPF program sees vs. what a
  software bridge produces. The L3-L5 rig is software-only.
- **CMS classification under saturated workloads.** Past ~50K
  concurrent flows per direction, collisions dominate and every
  flow looks heavy. The chaos test confirms the program survives
  the condition, not that the classification stays meaningful.
- **Above-1Gbps annotated rates on the wire.** The token-bucket
  math is rate-agnostic, but we don't have a rig that can produce
  >1 Gbps single-stream to confirm in-the-wild behavior.
