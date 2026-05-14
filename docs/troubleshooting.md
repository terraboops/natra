# Troubleshooting

## CNI ADD path

### Pod stuck in ContainerCreating

Check kubelet for the CNI error:

```bash
journalctl -u kubelet | grep -i cni | tail
```

The natra binary logs every invocation to `/var/log/natra-cni.log` on
the node. The DaemonSet host-mounts this path, so it accumulates across
pods on the node:

```bash
docker exec <node> tail -50 /var/log/natra-cni.log
```

Check the install init container log to confirm the conflist patch
landed:

```bash
kubectl logs -n kube-system -l app=natra -c install --tail=50
```

The patched conflist should be at
`/etc/cni/net.d/00-natra-<original>.conflist` on each node. Containerd's
CNI loader picks the alphabetically-first conflist with `maxConfNum:1`.

### "BPF attach failed" + fail-open

natra's CNI ADD is fail-open past stdin parsing: if the BPF attach
fails, it logs the reason to stderr and to `/var/log/natra-cni.log`,
then returns success so the Pod can start. Look in the log for
`attachBPF FAILED:` and the underlying error.

Common attach errors:

- `pin tcx link to /sys/fs/bpf/natra/...: operation not permitted`
  — the pin path contains a `.` somewhere. Bpffs forbids dots in
  user-mounted subdirectory names. Verify natra's pin name scheme
  (`<containerID>-<ifName>-link`, no extension).
- `attach BPF to eth0 ingress: ... no such device` — the CNI chain
  ran natra before the main CNI created the veth. Check the conflist
  order: natra must be after kindnet/calico/etc.

### `unknown attachMode "..."`

The natra entry in the conflist has an `attachMode` value that isn't
one of `auto` (default), `tcx-hostside`, `tcx-podside`,
`clsact-hostside`, or `clsact-podside`. Either fix the manifest or
unset `NATRA_ATTACH_MODE` on the install init container.

### `edtPacing=on requires pod-side attach mode but strategy includes ...`

`NATRA_EDT_PACING=on` was set while `NATRA_ATTACH_MODE` was pinned to
a host-side variant. EDT needs `fq` downstream of the BPF program,
which only works for pod-side attach. Either switch `NATRA_EDT_PACING`
to `auto` (the default — host-side attach silently disables EDT) or
unpin the attach mode.

## Attach mode

natra defaults to `auto` — tries `tcx-pod` → `clsact-pod` →
`tcx-host` → `clsact-host` (when EDT is auto, the standard default;
the order flips host-first when EDT is explicitly off). First
attempt that succeeds wins. Explicit modes pin a single attempt with
no fallback.

```bash
# DaemonSet
kubectl set env -n kube-system daemonset/natra-installer \
  NATRA_ATTACH_MODE=clsact-podside

# tests
NATRA_E2E_ATTACH_MODE=clsact-podside make test-e2e
```

## EDT pacing

natra's default is `edtPacing: auto`. At CNI ADD, the plugin tries
`tc qdisc replace dev eth0 root fq` inside the pod netns; on success
the BPF program EDT-stamps above-rate egress packets, on failure it
falls back to drop after ECN-mark.

```bash
# Force on (fail attach if fq install fails)
kubectl set env -n kube-system daemonset/natra-installer \
  NATRA_EDT_PACING=on

# Force off (never install fq, always drop after ECN-mark)
kubectl set env -n kube-system daemonset/natra-installer \
  NATRA_EDT_PACING=off
```

Inspect the active disposition on a pod via `dump-stats`:

```bash
docker exec <node> /opt/cni/bin/natra dump-stats <containerID>
```

`ecn_marked`, `edt_delayed`, and `dropped` slots break down what
natra did with each above-rate packet. Their sum equals `throttled`.

## bpffs and pin paths

`/sys/fs/bpf` must be mounted as a `bpf` filesystem before natra
attempts a pin. The DaemonSet's install init container does this with
`mountPropagation: Bidirectional` so the mount escapes to the host's
mount namespace and is visible to kubelet's CNI invocations.

To check on a node:

```bash
docker exec <node> mount | grep '/sys/fs/bpf'
docker exec <node> ls /sys/fs/bpf/natra/
```

Pin names must not contain `.`. The kernel's `bpf_lookup` returns
`EPERM` on any path component containing a dot under a user-mounted
bpffs subdirectory; those names are reserved for `populate_bpffs`'s
internal special files. natra uses dotless `-link` and `-map`
suffixes.

## Throughput

### Throughput exceeds the annotation

iperf measured at the receiver should fall within ~30% of the
annotation. If it's wildly higher (e.g. line-rate Gbps under a 10 Mbps
annotation), natra is fail-open: it loaded but the attach failed. See
"CNI ADD path" above to find the underlying error.

### Throughput is well below the annotation

Most likely cause: the heavy-hitter threshold is wrong for the
workload. natra rate-limits flows whose CMS byte count exceeds the
threshold. The per-pod default is rate-scaled
(`max(16 KiB, rate × 100 ms / 1000)`); a 10 Mbps pod gets ~125 KiB,
a 1 Gbps pod ~12.5 MiB. Below threshold, mice take the fast pass and
ignore the bucket. Above threshold, every packet pays tokens.

A workload of many sustained TCP flows (each crossing threshold within
a few ms) ends up rate-limited as a group, not as individuals. This is
the design.

To see the live classification on a pod:

```bash
docker exec <node> /opt/cni/bin/natra dump-stats <containerID>
```

Watch `passed`, `throttled`, `hh_hits`. If `hh_hits` is most of the
traffic, every flow is classified heavy. Drop the threshold to see the
fast pass take effect; raise it to give more flows the fast pass.

Two cluster-wide knobs on the installer DaemonSet adjust the
threshold behavior:

```bash
# Pin every pod to the same threshold (bytes), ignoring rate-scaling
kubectl set env -n kube-system daemonset/natra-installer \
  NATRA_DEFAULT_HH_THRESHOLD=262144

# Raise the per-pod time constant (ms) — more honeymoon before a
# flow crosses to heavy. Defaults to 100.
kubectl set env -n kube-system daemonset/natra-installer \
  NATRA_FASTPASS_TIME_CONSTANT_MS=250
```

For per-pod tuning use the JSON annotation form
(`{"rate":"10M","heavyHitterThreshold":131072}`) instead.

## L4 e2e on the local rig

The L4 test brings up a k3d cluster and asserts iperf throughput is
within a calibrated cap (μ + 2σ + 5% margin, floored at 1.30× rate).
The runner's measured jitter sets the effective cap; the floor only
binds on low-jitter runners. Bringup logs are in the test's output;
on failure the test dumps:

- `kubectl describe pod` for the iperf pods
- The natra install container log
- The patched conflist on the worker node

`NATRA_E2E_KEEP=1 make test-e2e` leaves the k3d cluster up after the
test for inspection; otherwise the AfterSuite tears it down.

## Verify kernel

```bash
./scripts/verify-kernel.sh
```

Reports the kernel version and whether tcx (BPF_TCX_INGRESS) is
supported. Present in 6.6+.
