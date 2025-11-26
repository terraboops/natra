# Natra Architecture

## Overview

Natra is a CNI plugin that rate-limits ingress traffic to a Pod, applying
the limit only to the heavy flows so short-lived mice keep flowing at line
rate. It uses the standard `kubernetes.io/ingress-bandwidth` annotation,
chains behind whatever main CNI is in place (kindnet, calico, etc.), and
implements the rate-limit logic in BPF.

## Components

### CNI plugin

`cmd/natra`, `pkg/cni/`

Invoked by kubelet during Pod network setup. Reads the CNI stdin
payload, pulls the bandwidth value out of `runtimeConfig.bandwidth`
(falling back to `runtimeConfig.podAnnotations` if absent), loads the
embedded BPF object, and attaches it to the Pod's veth ingress. Past
stdin parsing every error path is fail-open — log to stderr, return
success, let the Pod start unrate-limited rather than stuck in
ContainerCreating.

### BPF programs

`bpf/natra.bpf.c` is what the plugin loads in production. `bpf/vanilla.bpf.c`
is a token-bucket-on-every-packet emulator that mimics what the upstream
`containernetworking/plugins/bandwidth` plugin does; only the L5 perf test
loads it, for head-to-head comparison.

The pipeline is two stages on the Pod-side veth ingress:

1. **Count-Min Sketch.** 4-row × 1024-column sketch of `u32` counters
   (~16 KB regardless of flow cardinality). The 5-tuple (src/dst IP, src/dst
   port, proto) hashes into one cell per row via FNV-1a mixed with per-row
   seeds. Each cell is `__sync_add_and_fetch`-ed atomically (compiled with
   `clang -mcpu=v3`). The estimator is `min` across the four rows. A flow
   is "heavy" when its estimate exceeds `heavy_hitter_threshold`.

2. **Token bucket — heavy hitters only.** A single per-Pod bucket protected
   by `bpf_spin_lock`. Mice (below threshold) bypass the bucket entirely
   and return `TC_ACT_OK`. Heavy flows pay tokens proportional to `skb->len`;
   when the bucket is starved, `TC_ACT_SHOT`. `bpf_ktime_get_ns()` is read
   *outside* the spin-locked region — helper calls inside `bpf_spin_lock`
   are verifier-rejected.

The upstream bandwidth plugin (HTB qdisc on IFB) charges every packet, so
one elephant flow eats every token and mice starve. The CMS-then-bucket
shape lets mice fast-pass, which is what the L5 test
(`TestScenarioMixedVsVanilla`) measures.

The default heavy-hitter threshold is 10. GRO/GSO superpackets routinely
produce 27 KB "packets" at the BPF layer; a higher threshold lets short
flows through ~tens of MB before any throttling kicks in.

### DaemonSet installer

`deploy/cni-installer.yaml`. An init container copies the natra binary to
`/opt/cni/bin/`, runs `setcap cap_bpf,cap_net_admin,cap_perfmon,cap_sys_resource+ep`
on it, mounts bpffs, and patches every existing `*.conflist` in
`/etc/cni/net.d/` to add `{"type":"natra","capabilities":{"bandwidth":true}}`
as the last plugin in the chain. Pod readiness is gated on init completion
(no marker file). A `pause` sidecar keeps the Pod present so the DaemonSet
controller is happy.

The conflist patch is written to a `00-natra-<original>.conflist` sibling
file rather than in-place — containerd's CNI watcher races with sandbox
creation when the source conflist is rewritten, and the sibling sorts
ahead alphabetically so containerd picks it.

## Plugin flow

```
kubelet → CNI ADD on natra
         stdin = NetConf with prevResult + runtimeConfig.bandwidth
   ↓
parse stdin → resolve rate/burst from runtimeConfig
   ↓
enter pod netns → load embedded BPF object → configure maps
   ↓
attach via clsact + tc filter on the pod-side veth ingress
   ↓
pin maps under /sys/fs/bpf/natra/ for `natra dump-stats <containerID>`
   ↓
print prevResult unchanged → return success
```

CNI DEL removes the per-container pins (the tcx-link pin for tcx
mode, and the per-container map pins). For clsact-podside there's no
link pin; the kernel auto-detaches the filter when the chained CNI's
DEL deletes the underlying veth.

## Fail-open

Past stdin parsing, every error path falls through to passthrough
with a note on stderr and to `/var/log/natra-cni.log`:

- BPF load / verifier rejection → passthrough
- Kernel too old / missing helpers → passthrough
- Attach failure (`BPF_LINK_CREATE` or `tc filter add`) → passthrough
- Pin failure (`BPF_OBJ_PIN`) → passthrough; the link or filter is
  closed, the BPF program detaches when natra exits
- Annotation malformed → passthrough

## Annotation forms

Standard simple form:
```yaml
apiVersion: v1
kind: Pod
metadata:
  annotations:
    kubernetes.io/ingress-bandwidth: "10M"
```

Extended JSON form (overrides the parsed defaults):
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

## Kernel requirements

- Default tcx mode: kernel 6.6+ (uses `bpf_mprog`).
- Fallback `clsact-podside` mode: kernel 5.x+.
- EKS: AL2023 or recent Bottlerocket for the `cap_bpf` ambient cap.

## Attachment

natra supports two attach modes, controlled by the `attachMode` field on
the conflist entry (or the `NATRA_ATTACH_MODE` env var passed to the
install init container).

### `tcx` (default)

`pkg/bpf/loader.go::attachTCX` calls `link.AttachTCX(...)` and pins
the returned link to bpffs at `/sys/fs/bpf/natra/<containerID>-<ifName>-link`.
The kernel holds the program reference via the link until `cmdDel`
removes the pin. Composes via `bpf_mprog` with anything else attaching
at the same hook (cilium-agent, aws-network-policy-agent).

Prerequisites:

- Kernel 6.6+ (when tcx attachment was added).
- bpffs mounted at `/sys/fs/bpf` before natra runs. The DaemonSet's
  install init container handles this with `mountPropagation:
  Bidirectional` so kubelet's later CNI invocations see the mount.
- File caps: `setcap cap_bpf,cap_net_admin,cap_perfmon,cap_sys_resource+ep`
  on `/opt/cni/bin/natra`. Kubelet doesn't pass these through ambient,
  so the binary needs them via file caps.

### `clsact-podside` (opt-in)

`pkg/bpf/loader.go::attachClsactPodside` adds a `clsact` qdisc
(idempotent) and calls `netlink.FilterReplace` to install the program
on the ingress hook with `DirectAction: true`. Because `attachBPF`
enters the pod netns before calling, this attaches to the pod-side
end of the veth pair — host-side AWS VPC CNI clsact filters live in
the host netns and don't see this. The kernel's qdisc tree holds
the program reference for the lifetime of the veth.

Tradeoffs vs tcx: collides with anything else attaching clsact in
the same pod's netns. Doesn't compose; whoever attached last wins.
Useful only on kernels that lack tcx (< 6.6).

## Open ends

- IPv6: `parse_flow` returns -1 for non-IPv4, so IPv6 flows pass through
  unrate-limited.
- UDP: parsed for the 5-tuple, same fast path as TCP.
- CO-RE: BPF program currently uses fixed kernel headers; CO-RE would help
  on heterogeneous kernels.
- ebpf_exporter integration for the per-pod stats map.
