# CNI behavior

natra is a chained CNI plugin. It runs after the main CNI (kindnet,
calico, AWS VPC CNI, etc.) has set up Pod networking, attaches its BPF
program to the Pod's veth in either or both directions (per the
annotations present on the Pod), and prints the upstream `prevResult`
unchanged so the chain continues working.

Negotiated CNI version: 1.0.0 (with 0.3.0/0.3.1/0.4.0 also accepted).

## Verbs

### ADD

1. Read NetConf from stdin.
2. Resolve the bandwidth per direction:
   - Ingress: `runtimeConfig.bandwidth.{ingressRate,ingressBurst}` first,
     falling back to `runtimeConfig.podAnnotations["kubernetes.io/ingress-bandwidth"]`.
   - Egress: `runtimeConfig.bandwidth.{egressRate,egressBurst}` first,
     falling back to `runtimeConfig.podAnnotations["kubernetes.io/egress-bandwidth"]`.

   Kubelet populates the runtimeConfig fields from the matching
   annotations when the conflist entry declares
   `capabilities.bandwidth: true`.
3. If neither direction has a rate, print `prevResult` and return —
   no BPF is loaded.
4. Resolve attach strategy from the top-level `attachMode` field and
   the resolved EDT mode in `defaults.edtPacing`. `attachMode: auto`
   (default) expands to an ordered fallback chain; explicit values
   (`tcx-hostside` etc.) produce a single-attempt list. See
   [ARCHITECTURE.md § Attachment](ARCHITECTURE.md#attachment) for
   the chain order.
5. Resolve the target ifindex for each candidate side — pod-side
   enters the pod netns and reads eth0; host-side enters briefly to
   read eth0's veth peer ifindex, then attaches in the host netns.
6. For pod-side candidates when EDT is `on` or `auto`: install `fq`
   as the root qdisc on pod-eth0 so `skb->tstamp` is honored on
   egress. Set `cfg.edt_pacing = 1` in the BPF config slot on
   success. On failure, `edtPacing: on` returns an error from the
   attempt; `edtPacing: auto` continues with `cfg.edt_pacing = 0`
   and the BPF falls back to drop after ECN-mark.
7. Load the BPF object, and for each direction that has a rate:
   configure that direction's slot, attach the matching program
   (`natra_ingress` or `natra_egress`) to the matching hook. The
   loader flips hook-direction to opposite the pod-direction on
   host-side attach.
8. Print `prevResult` and return.

If any step in an attempt fails, the partial state is torn down
(link closed, pin removed) and the next attempt in the strategy
runs. The plugin returns success once any attempt completes; if
every attempt fails it falls through to passthrough (fail-open).

Past stdin parsing every error path is fail-open: log to stderr and
to `/var/log/natra-cni.log`, return success. A Pod stuck in
ContainerCreating because the rate limiter couldn't load is worse
than a Pod running unrate-limited.

### DEL

Remove the per-container pins under `/sys/fs/bpf/natra/`:

- `<containerID>-<side>-ingress-link` — the ingress tcx-link pin
  (only present in tcx modes; `<side>` is `hostside` or `podside`).
- `<containerID>-<side>-egress-link` — the egress tcx-link pin
  (only present in tcx modes).
- `<containerID>-{config,bucket,stats,cms}-map` — the per-pod map
  pins (used by `natra dump-stats`; one set shared across
  directions, written for both tcx and clsact modes).

For clsact-* attachments there are no link pins. The kernel
auto-detaches the filters when the next chained plugin's DEL deletes
the underlying veth.

`cmdDel` walks the pin dir and removes everything with the
`<containerID>-` prefix, so partial state from a half-applied ADD or
from either direction being absent is cleaned up uniformly. DEL is
idempotent: missing pins are not errors.

### CHECK

No-op success. Re-entering the pod netns to verify each direction's
attachment would require listing tc filters or tcx links per ifindex,
and kubelet uses CHECK as a liveness hint where a false positive
isn't worse than the fail-open path elsewhere.

### VERSION

Returns the CNI versions natra accepts (handled by `skel.PluginMainFuncs`
with `version.All`).

## NetConf shape

```jsonc
{
  "cniVersion": "1.0.0",
  "name": "kindnet",          // matches the upstream conflist
  "type": "natra",
  "attachMode": "auto",       // optional; one of: auto (default), tcx-hostside, tcx-podside, clsact-hostside, clsact-podside
  "defaults": {               // optional cluster-wide knobs
    "hhThreshold": 50,        // CMS count above which a flow is "heavy"
    "burstRatio": 0.5,        // bucket size = rate * burstRatio (seconds)
    "edtPacing": "auto"       // one of: auto (default), on, off
  },
  "capabilities": {
    "bandwidth": true         // tells kubelet to populate runtimeConfig.bandwidth
  },
  "runtimeConfig": {
    "bandwidth": {            // populated by kubelet
      "ingressRate":  10000000,
      "ingressBurst": 20000000,
      "egressRate":    5000000,
      "egressBurst":  10000000
    }
  },
  "prevResult": { ... }       // upstream main-CNI result, passed through
}
```

Either direction's `Rate`/`Burst` may be absent — kubelet only
includes a direction when the matching annotation is present.

`runtimeConfig.bandwidth.{ingress,egress}Rate` is in **bits per
second** (matching kubelet's convention); natra divides by 8 to get
bytes/sec for the BPF program. Each direction's burst is clamped to
`rate × defaults.burstRatio` (compiled default 0.5 — half a second
of credit at the configured rate), floored at 64 KiB so one
max-GRO super-packet always fits. Without the clamp, kubelet's
default `MaxUint32` (~4 GB) burst would let any flow saturate the
link for tens of seconds before the bucket caught up.

## Annotation forms

Simple (the standard form), one or both directions:

```yaml
metadata:
  annotations:
    kubernetes.io/ingress-bandwidth: "10M"
    kubernetes.io/egress-bandwidth: "5M"
```

Extended JSON, for overriding the heavy-hitter threshold. The same
fields and semantics apply on either annotation; each is parsed
independently:

```yaml
metadata:
  annotations:
    kubernetes.io/ingress-bandwidth: |
      {"rate":"10M","burst":"20M","heavyHitterThreshold":131072}
    kubernetes.io/egress-bandwidth: |
      {"rate":"5M","heavyHitterThreshold":65536}
```

`rate` and `burst` follow the k8s annotation convention — bits per
second with SI/IEC suffixes ("M" = Mbit, not MB). The parser divides
by 8 to populate the bytes/sec Config the BPF dataplane needs.

`heavyHitterThreshold` is a raw byte count (CMS unit). 131072 is
128 KiB, 65536 is 64 KiB. When unset on a per-pod basis, the
per-pod default is rate-scaled:
`max(16 KiB, rate_bytes × FASTPASS_TIME_CONSTANT_MS / 1000)`. With
the default 100 ms time constant, a 10 Mbps pod gets ~125 KiB
threshold and a 1 Gbps pod gets ~12.5 MiB.

## Chained conflist

A typical kindnet + natra chain produced by `natra install-cni-chain`:

```jsonc
{
  "cniVersion": "0.3.1",
  "name": "kindnet",
  "plugins": [
    { "type": "ptp", ... },
    { "type": "portmap", "capabilities": { "portMappings": true } },
    { "type": "natra", "capabilities": { "bandwidth": true } }
  ]
}
```

natra is intentionally last: kindnet sets up the veth pair and IPAM,
natra attaches BPF to the resulting interface.

## CNI environment variables

Standard set, used per spec:
`CNI_COMMAND`, `CNI_CONTAINERID`, `CNI_NETNS`, `CNI_IFNAME`,
`CNI_PATH`, `CNI_ARGS`.

natra enters the netns at `CNI_NETNS` with `runtime.LockOSThread()`
and a deferred restore — without restoring, CNI's skel framework
fails post-flight with "code 8: plugin's netns and netns from
CNI_NETNS should not be the same".

## Error responses

CNI-spec error JSON (returned only for stdin parsing or unknown
attachMode):

```json
{
  "cniVersion": "1.0.0",
  "code": 7,
  "msg": "natra: parse config: ..."
}
```

For everything else (BPF load failure, attach failure on either
direction, kernel feature missing, malformed annotation), natra logs
and prints `prevResult`.

## References

- [CNI Specification](https://www.cni.dev/docs/spec/)
- [containernetworking/cni](https://github.com/containernetworking/cni)
- [containernetworking/plugins/bandwidth](https://github.com/containernetworking/plugins/tree/main/plugins/meta/bandwidth)
  — the upstream token-bucket-on-IFB plugin natra coexists with or replaces
