# CNI behavior

natra is a chained CNI plugin. It runs after the main CNI (kindnet,
calico, AWS VPC CNI, etc.) has set up Pod networking, attaches its BPF
program to the Pod's veth ingress, and prints the upstream `prevResult`
unchanged so the chain continues working.

Negotiated CNI version: 1.0.0 (with 0.3.0/0.3.1/0.4.0 also accepted).

## Verbs

### ADD

1. Read NetConf from stdin.
2. Resolve the bandwidth from `runtimeConfig.bandwidth.{ingressRate,
   ingressBurst}` (kubelet populates this from
   `kubernetes.io/ingress-bandwidth` when the conflist entry declares
   `capabilities.bandwidth: true`). Fall back to
   `runtimeConfig.podAnnotations["kubernetes.io/ingress-bandwidth"]`
   if the bandwidth field is absent.
3. If no rate, print `prevResult` and return.
4. Resolve attach mode from the top-level `attachMode` field
   (`tcx` default, `clsact-podside` opt-in).
5. Enter the pod netns, load the BPF object, configure rate/burst,
   attach to the veth ingress.
6. Print `prevResult` and return.

Past stdin parsing every error path is fail-open: log to stderr and
to `/var/log/natra-cni.log`, return success. A Pod stuck in
ContainerCreating because the rate limiter couldn't load is worse than
a Pod running unrate-limited.

### DEL

Remove the per-container pins under `/sys/fs/bpf/natra/`:

- `<containerID>-<ifName>-link` — the tcx-link pin (only present in
  tcx mode; removing it detaches the program).
- `<containerID>-{config,bucket,stats,cms}-map` — the per-pod map
  pins (used by `natra dump-stats`).

For clsact-podside attachments there's no link pin. The kernel
auto-detaches the filter when the next chained plugin's DEL deletes
the underlying veth.

DEL is idempotent: missing pins are not errors.

### CHECK

No-op success. Re-entering the pod netns to verify the attachment
would require listing tc filters or tcx links per ifindex, and
kubelet uses CHECK as a liveness hint where a false positive isn't
worse than the fail-open path elsewhere.

### VERSION

Returns the CNI versions natra accepts (handled by `skel.PluginMainFuncs`
with `version.All`).

## NetConf shape

```jsonc
{
  "cniVersion": "1.0.0",
  "name": "kindnet",          // matches the upstream conflist
  "type": "natra",
  "attachMode": "tcx",        // optional; "tcx" (default) or "clsact-podside"
  "capabilities": {
    "bandwidth": true         // tells kubelet to populate runtimeConfig.bandwidth
  },
  "runtimeConfig": {
    "bandwidth": {            // populated by kubelet
      "ingressRate": 10000000,
      "ingressBurst": 20000000
    }
  },
  "prevResult": { ... }       // upstream main-CNI result, passed through
}
```

`runtimeConfig.bandwidth.ingressRate` is in **bits per second**
(matching kubelet's convention); natra divides by 8 to get bytes/sec
for the BPF program. `ingressBurst` is clamped to 2× rate when
unspecified or larger; without that clamp, the kubelet default of
`MaxUint32` (~4 GB) would let any flow saturate the link for ~30s
before the bucket caught up.

## Annotation forms

Simple (the standard form):

```yaml
metadata:
  annotations:
    kubernetes.io/ingress-bandwidth: "10M"
```

Extended JSON, for overriding the heavy-hitter threshold:

```yaml
metadata:
  annotations:
    kubernetes.io/ingress-bandwidth: |
      {"rate":"10M","burst":"20M","heavyHitterThreshold":50}
```

The default threshold is 10 (lowered from earlier project defaults to
work under realistic GRO/GSO).

## Chained conflist

A typical kindnet + natra chain produced by
`natra install-cni-chain`:

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

For everything else (BPF load failure, attach failure, kernel feature
missing, malformed annotation), natra logs and prints `prevResult`.

## References

- [CNI Specification](https://www.cni.dev/docs/spec/)
- [containernetworking/cni](https://github.com/containernetworking/cni)
- [containernetworking/plugins/bandwidth](https://github.com/containernetworking/plugins/tree/main/plugins/meta/bandwidth)
  — the upstream HTB-on-IFB plugin natra coexists with or replaces
