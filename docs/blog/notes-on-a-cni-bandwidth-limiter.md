# Notes on building a Kubernetes CNI bandwidth limiter

A few days ago I started natra, a CNI plugin that rate-limits ingress
traffic to a Pod. The standard `containernetworking/plugins/bandwidth`
plugin already does this — the question I had was whether running a
Count-Min Sketch in BPF before the rate-limit bucket would meaningfully
improve fairness on mixed workloads.

natra is days old, has zero production users, and the rest of this
post is mostly the gotchas I hit on the way.

## The design idea

The upstream bandwidth plugin uses HTB on IFB. Every packet on the way
into a Pod gets charged against one token bucket; once the bucket is
empty, packets drop. That's reasonable for a single steady flow, but
when an elephant flow and a bunch of mice share a Pod's annotation,
the elephant drains the bucket and the mice arrive empty-handed. The
Pod is annotated "10 Mbps" but every flow loses, not just the loud one.

The fix is simple: classify before charging. CMS in BPF gives that for
~16 KB per Pod, regardless of flow cardinality.

```
   skb -> parse 5-tuple -> CMS atomic add x4 rows -> min across rows
                                                      |
                                       count <= threshold? -> TC_ACT_OK
                                       count >  threshold? -> bucket
                                                                |
                                                          tokens? -> OK
                                                          empty?  -> SHOT
```

Two BPF maps, a few hundred lines of C, a small Go loader.

## The five things that broke before I got an iperf throttle

The BPF code is the easy part. Wiring it through kubelet, kubelet
through containerd, containerd through kindnet, kindnet through to a
real veth — that's where the time went.

I had a kind cluster running natra-as-CNI, a Pod with
`kubernetes.io/ingress-bandwidth: "10M"`, and iperf3 measuring
unbounded 95–110 Gbps. The annotation was being ignored. Five fixes:

1. **`version.ParsePrevResult` was never called.** The CNI library has
   a typed `PrevResult` and a generic `RawPrevResult`. JSON unmarshal
   fills the latter; the typed field is only populated when the
   version framework walks it. Without that call, passthrough emitted
   a minimal Result missing the IPs kindnet had assigned, and
   containerd v2 then failed sandbox creation with "failed to find
   network info for sandbox". Pods stuck in `ContainerCreating`.

2. **Patching kindnet's conflist in-place was racy.** Containerd v2's
   CNI watcher reloads on every conflist write, and rapid rewrites
   raced with sandbox creation. Switched to writing a sibling
   `00-natra-<original>.conflist` that sorts ahead alphabetically;
   with `maxConfNum: 1` containerd picks the chained version and
   leaves the original alone.

3. **Reading the annotation from `runtimeConfig.podAnnotations` was
   wrong.** Kubelet doesn't pass arbitrary pod annotations through CNI
   — it requires the plugin to declare `capabilities.bandwidth: true`
   in the conflist entry, then kubelet populates
   `runtimeConfig.bandwidth.{ingressRate,ingressBurst}` from the
   `kubernetes.io/{ingress,egress}-bandwidth` annotations. Both pieces
   needed adding.

4. **`BPF_OBJ_PIN` returned EPERM on tcx-link pins.** Even with
   `cap_bpf` and `cap_net_admin` set on the binary via `setcap`. I
   routed around this by switching to a clsact attach, then later came
   back to it — see the bpffs dot rule below.

5. **The bandwidth runtimeConfig is in bits/sec, not bytes/sec.**
   Kubelet follows the upstream-bandwidth convention. Without dividing
   by 8, a "10M" annotation throttled to 80 Mbps. Burst defaults to
   `MaxUint32` (~4 GB) when the annotation doesn't specify one — would
   let any flow saturate for ~30 seconds before the bucket caught up.
   Clamped burst to 2× rate.

Each failed the same way: pods stuck, or throughput unbounded. The CNI
spec is precise; what the spec doesn't tell you is the unwritten
behavior accumulated across kubelet, containerd, and the main CNI
plugin.

## The bpffs dot rule

I'd shipped the clsact-podside workaround for the EPERM in #4 above
and was about to call it done. Then I went back. Full caps in a
privileged container. No AppArmor. No seccomp. `unprivileged_bpf_disabled=2`
but root is unaffected. `bpftool prog pin` works fine on the same
bpffs. But `BPF_OBJ_PIN` of a TCX_INGRESS link kept EPERM-ing:

```
bpf(BPF_LINK_CREATE, {link_create={prog_fd=13, target_fd=3,
    attach_type=BPF_TCX_INGRESS, flags=0}}, 64) = 8
bpf(BPF_OBJ_PIN, {pathname="/sys/fs/bpf/natra/t1-eth0.link",
    bpf_fd=8, file_flags=0, path_fd=0}, 24) = -1 EPERM
```

Tried legacy pathname, `BPF_F_PATH_FD` with an open dirfd, raw syscall
bypassing cilium/ebpf. EPERM, EPERM, EPERM. ftrace showed
`bpf_link_get_from_fd` returning success and `bpf_obj_pin_user`
returning -1 about 100 bytes later. `bpftool link pin` on a non-tcx
link (a stray `tracing` one in the kernel) worked. So tcx-link-pin
specifically.

This was where I should have read the kernel source instead of
chasing lockdown / userns / token theories. Linus's tree at `v6.8`,
`kernel/bpf/inode.c`, line 374:

```c
static struct dentry *
bpf_lookup(struct inode *dir, struct dentry *dentry, unsigned flags)
{
    /* Dots in names (e.g. "/sys/fs/bpf/foo.bar") are reserved for future
     * extensions. That allows popoulate_bpffs() create special files.
     */
    if ((dir->i_mode & S_IALLUGO) &&
        strchr(dentry->d_name.name, '.'))
        return ERR_PTR(-EPERM);

    return simple_lookup(dir, dentry, flags);
}
```

**Bpffs forbids dots in path component names** under user-mounted
subdirectories. They're reserved for `populate_bpffs`'s internal
special files. My pin path was `<containerID>-eth0.link` — the
`.link` extension tripped this. Which also explained why my `os.Stat`
calls on missing pin files returned EPERM instead of ENOENT in tests:
even *looking up* a name with a dot fails.

`bpftool prog pin` worked because programs were named
`natra_placeholder` (no dot). The L4 e2e tests with clsact-podside
passed because clsact doesn't pin anything — the kernel's qdisc tree
owns the filter.

I'd been in the dark for hours about a problem the kernel's own code
review comment had announced ahead of time. The fix was a
two-character change: `.link` → `-link`. Same for the per-container
map pins (`.map` → `-map`), which had been silently failing the whole
time too — `PinMaps` is best-effort and just logs.

After that, tcx attach + pin worked clean. L4 e2e green with the
production-default tcx mode in 99 seconds. clsact-podside stays as
the documented opt-in fallback for kernels < 6.6.

Read the kernel comments.

## Some smaller things

The default heavy-hitter threshold I shipped was 1000, lifted from an
early architecture doc. Wrong. With GRO/LRO active (most real network
paths), a single skb seen by BPF can carry 30+ TCP segments, so a
1000-packet threshold lets ~27 MB through before any throttling kicks
in. Lowered to 10. Brief HTTP requests / DNS lookups still fast-pass;
sustained flows cross threshold in < 100 ms. The synthetic
BPF_PROG_RUN tests passed with the higher threshold — only the iperf
e2e exposed it.

`BPF_PROG_RUN` with skb caps the input size at roughly
`PAGE_SIZE - sizeof(struct skb_shared_info)` (~3,772 B on x86_64). A
4 KB jumbo-packet test always returned EINVAL. Real fix: 3 KB buffer.

Helpers can't be called inside a `bpf_spin_lock` region. The verifier
rejects with "function calls are not allowed while holding a lock."
Read `bpf_ktime_get_ns()` first, then take the lock.

Atomic-fetch BPF instructions need `-mcpu=v3`. Without it, clang
generates a plain XADD which doesn't return a value, and
`__sync_add_and_fetch` becomes "increment but discard the result" —
the verifier accepts it but the CMS counts come out wrong.

## The numbers, with caveats

Two numbers for the same question.

The synthetic head-to-head, in `test/perf/perf_linux_test.go::TestScenarioMixedVsVanilla` —
both natra and an in-tree emulator of the upstream HTB-on-IFB algorithm
(`bpf/vanilla.bpf.c`) running the same packet sequence through
`BPF_PROG_RUN`, with mice flows of 5 packets each (under threshold) —
shows **natra mice goodput 100%, vanilla mice goodput 1.6%** under an
elephant-pre-drains-the-bucket workload. Contrived worst case for
vanilla.

The real-cluster head-to-head, two kind clusters with identical
iperf3 workload, run via `make perf-vs-vanilla`:

| Plugin                | Elephant     | Mice (20× sustained)  |
|-----------------------|--------------|------------------------|
| natra                 | 11.26 Mbps   | 10.95 Mbps             |
| upstream `bandwidth`  | 97.86 Mbps*  | 9.59 Mbps              |

natra rate-limits cleanly to within +13% of the 10 Mbps annotation.

The iperf3 `-P 20 -t 10` workload doesn't actually exercise natra's
fast-pass: 20 sustained TCP flows for 10s all cross threshold in
milliseconds and get classified heavy. The fast-pass only fires for
short-lived flows below threshold. A wrk- or ab-style HTTP workload
with many brief connections would model that; iperf3's parallelism
flag doesn't.

The asterisk on vanilla's elephant number: 97 Mbps under a 10 Mbps
annotation says HTB isn't engaging on that pod's veth in the elephant
phase. The mice phase (9.59 Mbps) suggests it does engage eventually;
could be HTB initialization timing, IFB redirect quirks in kindnet,
or a kindnet-bandwidth interaction. I haven't traced it; not making
a claim based on it.

## What this isn't

natra is days old, has zero production users, lives entirely in kind
clusters on a colima daemon. There are no real-hardware benchmarks.
IPv6 isn't classified — `parse_flow` returns -1 on non-IPv4 and the
program passes the packet through unrate-limited. CMS saturation past
4,096 cells (1024 × 4) silently degrades classification; the chaos
test confirms the program survives, not that the answer it returns is
meaningful. CI runs against a single host kernel because the lvh image
registry has been returning "manifest unknown".

## Code

[github.com/terraboops/natra](https://github.com/terraboops/natra),
Apache-2.0. [ARCHITECTURE](../ARCHITECTURE.md) walks through the
components. [TODO_LINUX.md](../../TODO_LINUX.md) has the test-rig
details.
