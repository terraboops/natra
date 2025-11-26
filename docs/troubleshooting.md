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
one of `tcx` (default) or `clsact-podside`. Either fix the manifest or
unset `NATRA_ATTACH_MODE` on the install init container.

## Attach mode

natra defaults to `tcx`. Use `clsact-podside` only on kernels < 6.6
or when investigating tcx-specific issues.

```bash
# DaemonSet
kubectl set env -n kube-system daemonset/natra-installer \
  NATRA_ATTACH_MODE=clsact-podside

# tests
NATRA_E2E_ATTACH_MODE=clsact-podside make test-e2e
```

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

iperf measured at the receiver should fall within +20% of the
annotation. If it's wildly higher (e.g. line-rate Gbps under a 10 Mbps
annotation), natra is fail-open: it loaded but the attach failed. See
"CNI ADD path" above to find the underlying error.

### Throughput is well below the annotation

Most likely cause: the heavy-hitter threshold is wrong for the
workload. natra rate-limits flows whose CMS estimate exceeds the
threshold (default 10). Below threshold, mice take the fast pass and
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

## L4 e2e on the local rig

The L4 test brings up a kind cluster and asserts iperf throughput is
within +20% of the annotated rate. Bringup logs are in the test's
output; on failure the test dumps:

- `kubectl describe pod` for the iperf pods
- The natra install container log
- The patched conflist on the worker node

`NATRA_E2E_KEEP=1 make test-e2e` leaves the kind cluster up after the
test for inspection; otherwise the AfterSuite tears it down.

## Verify kernel

```bash
./scripts/verify-kernel.sh
```

Reports the kernel version and whether tcx (BPF_TCX_INGRESS) is
supported. Present in 6.6+.
