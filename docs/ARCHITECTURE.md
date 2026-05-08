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

| Map               | Layout                                                    |
|-------------------|-----------------------------------------------------------|
| `natra_config_map`| key = direction (0=ingress, 1=egress); 2 slots             |
| `natra_bucket_map`| key = direction; 2 slots                                  |
| `natra_stats_map` | key = direction × 3 + slot (passed/throttled/hh_hits); 6 slots, per-CPU |
| `natra_cms_map`   | key = direction × 4096 + row × 1024 + col; 8192 cells     |

CMS halves are independent per-direction. Asymmetric workloads (HTTP
downloads, file uploads, streaming) make a flow heavy in one direction
and mice on the other (just ACKs); a shared CMS would falsely classify
ACK streams as heavy. Cost is +16 KB per pod (~32 KB total CMS).

The pipeline is two stages on the Pod-side veth:

1. **Count-Min Sketch.** 4-row × 1024-column sketch of `u32` counters
   per direction. The 5-tuple (src/dst IP, src/dst port, proto) hashes
   into one cell per row via FNV-1a mixed with per-row seeds. Each
   cell is `__sync_add_and_fetch`-ed atomically (compiled with
   `clang -mcpu=v3`). The estimator is `min` across the four rows. A
   flow is "heavy" when its estimate exceeds `heavy_hitter_threshold`.

2. **Token bucket — heavy hitters only.** A per-direction bucket
   protected by `bpf_spin_lock`. Mice (below threshold) bypass the
   bucket entirely and return `TC_ACT_OK`. Heavy flows pay tokens
   proportional to `skb->len`; when the bucket is starved, `TC_ACT_SHOT`.
   `bpf_ktime_get_ns()` is read *outside* the spin-locked region —
   helper calls inside `bpf_spin_lock` are verifier-rejected.

The upstream bandwidth plugin (HTB qdisc on IFB) charges every packet,
so one elephant flow eats every token and mice starve. The
CMS-then-bucket shape lets mice fast-pass, which is what the L5 test
(`TestScenarioMixedVsVanilla` in both directions) measures.

The default heavy-hitter threshold is 10. GRO/GSO superpackets
routinely produce 27 KB "packets" at the BPF layer; a higher threshold
lets short flows through ~tens of MB before any throttling kicks in.

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

- For tcx mode: per-direction link pins
  (`<containerID>-<ifName>-{ingress,egress}-link`).
- For clsact-podside: no link pins; the kernel auto-detaches the
  filters when the chained CNI's DEL deletes the underlying veth.
- For both modes: per-container map pins
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
        "heavyHitterThreshold": 50
      }
```

The parser is direction-agnostic — same fields and semantics apply on
the egress annotation.

## Kernel requirements

- Default tcx mode: kernel 6.6+ (uses `bpf_mprog`).
- Fallback `clsact-podside` mode: kernel 5.x+.
- EKS: AL2023 or recent Bottlerocket for the `cap_bpf` ambient cap.

## Attachment

natra supports two attach modes per direction, controlled by the
`attachMode` field on the conflist entry (or the `NATRA_ATTACH_MODE`
env var passed to the install init container). Both directions use
the same mode; the choice is a deployment-wide knob.

### `tcx` (default)

`pkg/bpf/loader.go::AttachIngress` and `AttachEgress` each call
`link.AttachTCX(...)` against the matching hook
(`ebpf.AttachTCXIngress` / `ebpf.AttachTCXEgress`) and pin the
returned link to bpffs at
`/sys/fs/bpf/natra/<containerID>-<ifName>-<direction>-link`. The
kernel holds each program reference via its link until `cmdDel`
removes the pin. Composes via `bpf_mprog` with anything else
attaching at the same hook (cilium-agent, aws-network-policy-agent).

Prerequisites:

- Kernel 6.6+ (when tcx attachment was added).
- bpffs mounted at `/sys/fs/bpf` before natra runs. The DaemonSet's
  install init container handles this with
  `mountPropagation: Bidirectional` so kubelet's later CNI invocations
  see the mount.
- File caps: `setcap cap_bpf,cap_net_admin,cap_perfmon,cap_sys_resource+ep`
  on `/opt/cni/bin/natra`. Kubelet doesn't pass these through ambient,
  so the binary needs them via file caps.

### `clsact-podside` (opt-in)

`pkg/bpf/loader.go::attachClsactPodside` adds a `clsact` qdisc
(idempotent — the second direction's call is a no-op via EEXIST) and
calls `netlink.FilterReplace` to install the program on the matching
parent (`HANDLE_MIN_INGRESS` or `HANDLE_MIN_EGRESS`) with
`DirectAction: true`. Because `attachBPF` enters the pod netns before
calling, this attaches to the pod-side end of the veth pair —
host-side AWS VPC CNI clsact filters live in the host netns and don't
see this. The kernel's qdisc tree holds the program references for
the lifetime of the veth.

Tradeoffs vs tcx: collides with anything else attaching clsact in the
same pod's netns. Doesn't compose; whoever attached last wins. Useful
only on kernels that lack tcx (< 6.6).

## Open ends

- IPv6: `parse_flow` returns -1 for non-IPv4, so IPv6 flows pass
  through unrate-limited in either direction.
- UDP: parsed for the 5-tuple, same fast path as TCP.
- CO-RE: BPF program currently uses fixed kernel headers; CO-RE would
  help on heterogeneous kernels.
- ebpf_exporter integration for the per-direction stats slots.
