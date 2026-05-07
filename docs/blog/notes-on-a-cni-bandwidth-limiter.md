# Notes on building a Kubernetes CNI bandwidth limiter

A few days ago I started natra, a CNI plugin for Kubernetes that
rate-limits ingress traffic to a Pod. The standard
`containernetworking/plugins/bandwidth` plugin already does this — the
question I had was whether a smarter shape was reachable in a small
amount of code, specifically whether running a Count-Min Sketch in BPF
before the rate-limit bucket would meaningfully improve fairness on
mixed workloads.

The short answer is yes, in synthetic head-to-head. The longer answer
involves some kernel weirdness that I'm still chasing.

This is mostly a journey post, not a launch post. natra is days old,
has zero production users, and I'm publishing it as much to document
the gotchas as to share the artifact.

## The design idea

The upstream bandwidth plugin uses HTB on IFB. Every packet on the way
into a Pod gets charged against one token bucket; once the bucket is
empty, packets drop. That's perfectly reasonable for a single steady
flow, but it has a bias I don't like: when an elephant flow and a bunch
of mice share a Pod's annotation, the elephant drains the bucket and
the mice arrive empty-handed. They drop too. The Pod is annotated
"10 Mbps" but in practice every flow loses, not just the loud one.

The fix is conceptually simple: classify flows before charging the
bucket, and only charge the heavy ones. CMS in BPF gives you that for
~16 KB per Pod, regardless of flow cardinality.

```
   skb -> parse 5-tuple -> CMS atomic add x4 rows -> min across rows
                                                      |
                                       count <= threshold? -> TC_ACT_OK
                                                      |
                                       count >  threshold? -> bucket
                                                                |
                                                          tokens? -> OK
                                                          empty?  -> SHOT
```

That's the whole shape. Two BPF maps, a few hundred lines of C, a
small Go loader.

## The five things that broke before I got an iperf throttle

The BPF code is the easy part. Wiring it through kubelet, kubelet
through containerd, containerd through kindnet, kindnet through to a
real veth — that's where the time went.

I had a kind cluster running natra-as-CNI, a Pod with
`kubernetes.io/ingress-bandwidth: "10M"`, and iperf3 measuring the
unbounded 95–110 Gbps loopback throughput. The annotation was being
ignored. Five separate fixes later it worked:

1. **`version.ParsePrevResult` was never called.** The CNI library has
   a typed `PrevResult` and a generic `RawPrevResult`. JSON unmarshal
   fills in the latter; the typed field is only populated when you
   explicitly ask the version framework to walk it. Without that call,
   passthrough was emitting a minimal Result missing the IP addresses
   that kindnet had assigned, and containerd v2 then failed sandbox
   creation with the famous "failed to find network info for sandbox"
   error. Pods stuck in `ContainerCreating` until I figured this out.

2. **Patching kindnet's conflist in-place was racy.** Containerd v2's
   CNI watcher reloads on every conflist write, and rapid rewrites
   raced with sandbox creation. Switched to writing a sibling
   `00-natra-<original>.conflist` that sorts ahead alphabetically;
   with `maxConfNum: 1` containerd picks the chained version and
   leaves the original alone. Kindnet can keep rewriting it on its own
   schedule.

3. **Reading the annotation from `runtimeConfig.podAnnotations` was
   wrong.** Kubelet doesn't pass arbitrary pod annotations through CNI
   — it requires the plugin to declare `capabilities.bandwidth: true`
   in the conflist entry, then kubelet populates
   `runtimeConfig.bandwidth.{ingressRate,ingressBurst}` from the
   `kubernetes.io/{ingress,egress}-bandwidth` annotations. Both pieces
   needed adding. The capabilities mechanism is the explicit opt-in
   protocol; without it, the annotation is invisible to the plugin.

4. **`BPF_OBJ_PIN` returned EPERM on tcx-link pins.** Even with
   `cap_bpf` and `cap_net_admin` set on the binary via `setcap`,
   pinning a TCX_INGRESS link to bpffs failed with EPERM in the kind
   container. I ended up routing this through `clsact-podside` and
   shipping that as the working path. (I later reverted to tcx as the
   default — that turns out to be a separate kernel quirk; see below.)

5. **The bandwidth runtimeConfig is in bits/sec, not bytes/sec.**
   Kubelet follows the upstream-bandwidth convention. Without dividing
   by 8, a "10M" annotation throttled to 80 Mbps. Burst defaults to
   `MaxUint32` (~4 GB) when the annotation doesn't specify one, which
   would let any flow saturate for ~30 seconds before the bucket
   caught up. Clamped burst to 2× rate to match the upstream
   heuristic.

Each one of these failed in essentially the same way: pods stuck or
throughput unbounded. It was hard to localize without reading kubelet
source and watching containerd's inotify behavior. The CNI plugin
ecosystem is one of those places where the spec is precise but the
real-world behavior accumulates a lot of unwritten assumptions about
what kubelet, containerd, and the main CNI plugin will do for you.

## The clsact ↔ tcx pivot

Originally I wrote tcx attachment, hit the `BPF_OBJ_PIN` EPERM, gave up
and switched to clsact-podside (`tc filter add` on the pod-side veth
ingress). This worked end-to-end. I then convinced myself the AWS VPC
CNI compatibility story would be fine because *pod-side* clsact filters
live in the pod's netns and don't see *host-side* AWS clsact filters.

That was the wrong place to land. AWS's network-policy-agent — a
separate BPF-using component that ships alongside AWS VPC CNI — also
attaches BPF inside the pod's netns. So pod-side clsact does collide
with policy enforcement, just not with the basic VPC CNI itself. The
clean path is tcx, which uses `bpf_mprog` and composes with anything
attaching at the same hook point regardless of mechanism.

So I reverted to tcx as the default, kept clsact-podside as an opt-in
fallback for old kernels (`attachMode: clsact-podside` in the conflist
or `NATRA_ATTACH_MODE` env on the install init container). Now I have
two attach modes, one composes cleanly, one works on ancient kernels.

And then I ran the test suite and tcx pin failed with EPERM again,
just like before.

## The bpffs dot rule

So I'd shipped clsact-podside as a workaround, written it up as
"unresolved kernel quirk", pushed, and was about to call it a night
when I went back to verify one more time. Full caps in a privileged
container. No AppArmor (`/proc/self/attr/current` says unconfined).
No seccomp. `unprivileged_bpf_disabled=2` but that only matters for
non-root. `bpftool prog pin` works fine on the same bpffs. But
`BPF_OBJ_PIN` of a TCX_INGRESS link kept returning EPERM, every
time:

```
bpf(BPF_LINK_CREATE, {link_create={prog_fd=13, target_fd=3,
    attach_type=BPF_TCX_INGRESS, flags=0}}, 64) = 8
bpf(BPF_OBJ_PIN, {pathname="/sys/fs/bpf/natra/t1-eth0.link",
    bpf_fd=8, file_flags=0, path_fd=0}, 24) = -1 EPERM
```

I tried legacy pathname, `BPF_F_PATH_FD` with an open dirfd, raw
syscall bypassing cilium/ebpf entirely. EPERM, EPERM, EPERM. ftrace
showed `bpf_link_get_from_fd` returning a kernel pointer (success) and
then `bpf_obj_pin_user` returning -1 about 100 bytes later. So
something specific in the link-pin code path. I tried pinning a
*non-tcx* link via `bpftool link pin` for a stray `tracing` link the
kernel had — that worked. So it was tcx-link-pin specifically, not
links-in-general.

This was the moment I should have just read the kernel source instead
of grasping at lockdown / userns / token theories. So I read it.
Linus's tree at `v6.8`, `kernel/bpf/inode.c`, `bpf_obj_do_pin`. Line
449:

```c
dir = d_inode(path.dentry);
if (dir->i_op != &bpf_dir_iops) {
    ret = -EPERM;
    goto out;
}
```

Parent inode ops must be `bpf_dir_iops`. Which they were — the parent
was `/sys/fs/bpf/natra`, a bpffs subdir.

Skim further. Up at line 374:

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
`.link` extension tripped this every time. Which also explained why
my `os.Stat` calls on missing pin files returned EPERM instead of
ENOENT in tests: even *looking up* a name with a dot fails.

`bpftool prog pin` worked because programs there were named
`natra_placeholder` (no dot). My L5 BPF_PROG_RUN tests never pinned
anything (synthetic packets only). My L4 e2e tests with
clsact-podside passed because clsact doesn't pin anything either —
the kernel's qdisc tree owns the filter.

I'd been in the dark for hours about a problem the kernel's own
code review comment had announced ahead of time. The fix was a
two-character change: `.link` → `-link`. Same for the per-container
map pins (`.map` → `-map`), which had been silently failing the
whole time too — `PinMaps` is best-effort and logs without
returning, so I never noticed.

After that, tcx attach + pin worked clean on colima. L4 e2e green
with the production-default tcx mode in 99 seconds. clsact-podside
stays as the documented opt-in fallback for kernels < 6.6 only, not
as a Docker-Desktop workaround.

Read the kernel comments.

## Some smaller things I learned

The default heavy-hitter threshold I shipped was **1000**, lifted from
an early ARCHITECTURE doc. Wrong. With GRO/LRO active (which is most
real network paths), a single skb seen by BPF can carry 30+ TCP
segments, so a "1000 packet" threshold lets ~27 MB of a flow through
before any throttling kicks in. Lowered to **10**. Brief HTTP requests
and DNS lookups still fast-pass; sustained flows cross threshold in
less than 100 ms. This was the kind of thing that the synthetic
BPF_PROG_RUN tests pass with the higher threshold — only the iperf
e2e exposed it.

`BPF_PROG_RUN` with skb has a maximum input size of roughly
`PAGE_SIZE - sizeof(struct skb_shared_info)` (~3,772 B on x86_64). I
wrote a "jumbo packet" test using a 4 KB buffer; it always returned
EINVAL. The real fix was a 3 KB buffer plus a comment for the next
person.

Helpers cannot be called inside a `bpf_spin_lock` region. If you forget,
the verifier rejects with "function calls are not allowed while holding
a lock." Read your `bpf_ktime_get_ns()` first, then take the lock.

Atomic-fetch BPF instructions need `-mcpu=v3`. Without it, clang
generates a plain XADD which doesn't return a value, and your
`__sync_add_and_fetch` becomes "increment but discard the result" —
which the verifier still accepts but produces silently wrong CMS
counts.

## The numbers, with caveats

Two numbers for the same question.

The synthetic head-to-head — both natra and an in-tree emulator of the
upstream HTB-on-IFB algorithm (`bpf/vanilla.bpf.c`) running the same
packet sequence through `BPF_PROG_RUN`, with mice flows of 5 packets
each (well under natra's heavy-hitter threshold) — shows
**natra mice goodput 100%, vanilla mice goodput 1.6%** under an
elephant-pre-drains-the-bucket workload. This is a contrived worst
case for vanilla, designed to be vanilla's worst case, and it lives
in `test/perf/perf_linux_test.go::TestScenarioMixedVsVanilla` which
runs on every push.

The real-cluster head-to-head — two kind clusters, identical iperf3
workload, the actual upstream `bandwidth` plugin in one and natra in
the other, run via `make perf-vs-vanilla`:

| Plugin                | Elephant     | Mice (20× sustained)  |
|-----------------------|--------------|------------------------|
| natra                 | 11.26 Mbps   | 10.95 Mbps             |
| upstream `bandwidth`  | 97.86 Mbps*  | 9.59 Mbps              |

natra rate-limits cleanly to within +13% of the 10 Mbps annotation.

But: the iperf3 `-P 20 -t 10` workload doesn't actually exercise
natra's fast-pass. With 20 sustained TCP flows for 10 seconds, every
flow easily crosses the heavy-hitter threshold within milliseconds —
they all get classified heavy and rate-limited just like vanilla
would. The fast-pass only fires for *short-lived* flows that stay
below threshold. To actually see the natra advantage in a real
cluster, I'd need a wrk- or ab-style HTTP workload with many brief
connections; iperf3's parallelism flag doesn't model that.

The asterisk on vanilla's elephant number is a bigger deal. 97 Mbps
under a 10 Mbps annotation says HTB isn't engaging on that pod's
veth in the elephant phase. The mice phase (9.59 Mbps) suggests it
DOES engage eventually; could be HTB initialization timing, IFB
redirect quirks in kindnet, or a kindnet-bandwidth interaction
specific to my setup. I haven't traced it and I'm not making a claim
based on it.

What I can claim: natra's plumbing works end-to-end on a real kind
cluster. The CMS+bucket *design* showing up as a real performance
win on real workloads is still pending a test that exercises the
right shape of traffic.

## What this isn't

natra is days old, has zero production users, and lives entirely in
kind clusters on a colima daemon. There are no real-hardware
benchmarks. IPv6 isn't classified — `parse_flow` returns -1 on
non-IPv4 and the program passes the packet through unrate-limited.
CMS saturation past 4,096 cells (1024 × 4) silently degrades
classification; the chaos test only confirms the program survives
the condition, not that the answer it returns is meaningful. CI runs
against a single host kernel because the lvh image registry has been
returning "manifest unknown" on the kernel tags I tried.

## Code

[github.com/terraboops/natra](https://github.com/terraboops/natra),
Apache-2.0. [ARCHITECTURE](../ARCHITECTURE.md) walks through the
components. [TODO_LINUX.md](../../TODO_LINUX.md) has the test-rig
details.
