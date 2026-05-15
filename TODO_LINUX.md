# Linux test layer details

Reference for the Linux-only parts of natra's test harness. The
top-level `docs/development.md` covers the day-to-day commands; this
document is the deeper "what each layer is, what it asserts, and what
to know when it goes red" reference.

## Layers

| Layer | Purpose                                       | Build tag    |
|------:|:----------------------------------------------|:-------------|
|     1 | Unit + Go-native fuzz + benchmarks            | _none_       |
|     2 | CNI protocol — exec the binary in a netns     | `integration`|
|     3 | BPF dataplane — `BPF_PROG_RUN` + verifier     | `bpf`        |
|     4 | k3d end-to-end with iperf assertions          | `e2e`        |
|     5 | Perf scenarios + synthetic vs-vanilla         | `perf`       |

Plain `go test ./...` runs only L1.

Every layer runs from `make ci` on macOS (via Docker) and on Linux
(natively or via Docker). The vs-vanilla cluster comparison is
on-demand via `make perf-vs-vanilla`, not part of `make ci`.

## Layer 2 — CNI protocol

Files under `test/cni/`:
- `cni_linux_test.go` — happy-path ADD/DEL/CHECK + the four explicit
  attach modes (tcx-hostside, tcx-podside, clsact-hostside,
  clsact-podside) + per-direction specs (ingress only, egress only,
  both, neither). The `auto` default isn't exercised here because
  every mode is tested explicitly; the auto strategy logic lives in
  `resolveAttachStrategy` and is covered by unit tests.
- `chaos_linux_test.go` — malformed stdin, annotation injection on
  both ingress and egress channels, bad CNI env vars.
- `helpers_linux_test.go` — netns lifecycle, exec, env-var
  construction, direction-aware `linkPinExists`, `remainingPinsFor`.
- `cni_stub_test.go` — non-Linux skip stub.

Run:
```bash
sudo make test-cni    # native Linux
make test-cni         # macOS (wrapped in scripts/run-in-docker.sh)
```

Prerequisites:
- Linux kernel 5.x+ (6.6+ to exercise the tcx attach happy path).
- `sudo` for CAP_NET_ADMIN.
- bpffs at `/sys/fs/bpf` — the test's BeforeSuite mounts it via
  `unix.Mount` if not already; idempotent.

Tests `runtime.LockOSThread()` in BeforeSuite so netns operations
don't migrate goroutines mid-test. The CNI_NETNS path uses
`/proc/<pid>/fd/<fd>` rather than `/var/run/netns/<name>` — fine for
tests; some real CNI runtimes use named netns.

The L2 test exec's the natra binary as a subprocess (matching
kubelet's invocation pattern), so arg-parsing and stdin-handling bugs
surface here. CNI errors come back as JSON on stdout, not stderr.

## Layer 3 — BPF dataplane

Files under `test/bpf/`:
- `prog_linux_test.go` — placeholder load + sanity.
- `ratelimit_linux_test.go` — token-bucket and CMS classification,
  table-driven across `natra_ingress` and `natra_egress`. Includes
  `TestCrossDirectionIsolation` — configures one direction tight and
  the other wide-open, asserts no bleed.
- `chaos_linux_test.go` — verifier rejection of intentionally
  invalid programs, malformed packets, concurrent map updates, CMS
  saturation.
- `edge_cases_linux_test.go` — packet > burst, ICMP without L4 ports,
  IPv4 options, zero burst, rapid config change, jumbo packets,
  counter overflow. Direction-agnostic; runs against
  `natra_ingress` only (the egress program shares the same code).
- `prog_stub_test.go` — non-Linux skip.
- `testdata/invalid_oob_packet_access.bpf.c` — verifier-rejection
  fixture.

Run:
```bash
make test-bpf
```

Prerequisites:
- Linux kernel 5.10+ (`BPF_PROG_RUN` with skb).
- LLVM clang with the `bpf` target. The Makefile sets
  `BPF_CLANG=/opt/homebrew/opt/llvm/bin/clang` on macOS automatically
  if Homebrew's LLVM is installed.

Constraints to be aware of when extending L3 tests:
- `BPF_PROG_RUN` with skb caps the input packet size at roughly
  `PAGE_SIZE - sizeof(struct skb_shared_info)` (~3,772 B on x86_64).
  4 KB+ inputs return EINVAL. See `TestEdgeJumboPacket` for the
  documented constraint.
- BPF programs with atomic adds (CMS uses `__sync_add_and_fetch`)
  need `-mcpu=v3` or newer. The Makefile sets it.
- Helper calls (`bpf_ktime_get_ns`, etc.) are verifier-rejected
  inside `bpf_spin_lock`-protected regions. Read the timestamp first,
  then take the lock.

## Layer 4 — k3d end-to-end

Files under `test/e2e/`:
- `e2e_test.go::BeforeSuite` — creates a 2-node k3d cluster (1 server +
  1 agent) with flannel host-gw forced (VXLAN is ~30 Mbps on colima's
  LinuxKit kernel, below the rate-limit caps under test).
- `manifests/iperf-server.yaml` — server with
  `kubernetes.io/ingress-bandwidth: "10M"` (Topology A).
- `manifests/iperf-server-egress.yaml` — egress only (Topology B).
- `manifests/iperf-server-bidi.yaml` — both annotations at 10M
  (Topologies C and G).
- `manifests/iperf-server-mixed-{a,b,c}.yaml` — three pods on the
  worker; only mixed-a is annotated (Topology D, also reused by E).
- `manifests/iperf-server-noplugin.yaml` — unannotated, used by the
  no-plugin regression test (Topology F).
- `manifests/iperf-client.yaml` — client on the control-plane.
- `e2e_test.go` — Topologies A through G, plus a connectivity smoke
  in Topology A.
- `chaos_test.go` — DaemonSet restart preserves rate-limiting on
  ingress and egress pods, pod churn, three pending characterization
  specs (PIt).

Topologies asserted:

| Topology | What it pins                                                                 |
|----------|------------------------------------------------------------------------------|
| A        | ingress annotation throttles forward iperf3                                  |
| B        | egress annotation throttles reverse iperf3 (`-R`)                            |
| C        | both annotations throttle forward then reverse, sequential                   |
| D        | mixed: only annotated pods throttled; unannotated pods on the same node free |
| E        | no-annotation case: natra in path, no throttling                             |
| F        | no-plugin regression: with-natra delta vs. no-natra baseline < 20%           |
| G        | proxy-like: both directions throttle independently under concurrent traffic  |

Run:
```bash
make test-e2e                                 # default attach=auto, edt=auto
NATRA_E2E_ATTACH_MODE=tcx-podside make test-e2e
NATRA_E2E_ATTACH_MODE=clsact-hostside make test-e2e
NATRA_E2E_ATTACH_MODE=clsact-podside make test-e2e
```

Prerequisites:
- Docker (colima or Docker Desktop on macOS, dockerd on Linux).
- `k3d` v5.7.4+, `kubectl`.
- `iperf3` (in-pod, image `networkstatic/iperf3:latest`).

Failure-mode dumps: on iperf-Ready timeout, BeforeSuite emits
`kubectl describe pod`, the install init-container log, and the
patched conflist. `NATRA_E2E_KEEP=1 make test-e2e` leaves the k3d
cluster up after the test.

## Layer 5 — Perf

Files under `test/perf/`:
- `perf_linux_test.go` —
  - `TestBPFProgRunThroughput` — placeholder ns/op vs baseline.
  - `TestScenarioOneElephant{,Egress}` — single elephant per
    direction, expect throttling.
  - `TestScenarioThousandMice` — 1000 short flows on ingress, expect
    zero `hh_hits`.
  - `TestScenarioMixed` — elephant + mice on ingress, mice survive.
  - `TestScenarioMixedVsVanilla{,Egress}` — head-to-head vs
    `bpf/vanilla.bpf.o`, both directions.
- `perf_stub_test.go` — non-Linux skip.
- `baselines/local.json` — ns/op ceiling for the synthetic
  BPF_PROG_RUN tests; the test fails on regression past the recorded
  value.
- `realworld/vanilla-installer.yaml` — DaemonSet that fetches the
  upstream `bandwidth` plugin and chains it after flannel (k3d's
  default CNI), used by `make perf-vs-vanilla` for both ingress
  and egress phases.

Run:
```bash
make test-perf            # synthetic, in-process, BPF_PROG_RUN
make perf-vs-vanilla      # real-cluster, ~18-22 min, three k3d phases
```

The mixed scenario is elephant-first by design: the elephant
pre-drains the bucket, then mice arrive into the depleted bucket.
Interleaved sequences let the bucket refill between elephant packets
and trivially pass under both implementations.

## Attach modes for tests

natra picks an attach mode from an orthogonal cross of
`{tcx, clsact}` × `{hostside, podside}`, plus an `auto` mode that
expands to an ordered fallback chain:

| Mode             | Hook    | Veth half      | Notes                                       |
|------------------|---------|----------------|---------------------------------------------|
| `auto`           | —       | —              | Default. Tries each combination in order.   |
| `tcx-hostside`   | TCX     | host           | Same shape as Cilium / NPA.                 |
| `tcx-podside`    | TCX     | pod (eth0)     | Lives inside the pod netns. EDT-friendly.   |
| `clsact-hostside`| clsact  | host           | TC filter on the host-side veth.            |
| `clsact-podside` | clsact  | pod (eth0)     | Fallback for kernels < 6.6 / no bpffs.      |

`auto` expansion depends on EDT pacing mode (`defaults.edtPacing`):

- `edtPacing: off`: tcx-host → tcx-pod → clsact-host → clsact-pod
- `edtPacing: auto` (default): tcx-pod → clsact-pod → tcx-host →
  clsact-host (pod-side first so the `fq` install on pod-eth0 sits
  downstream of the BPF program)
- `edtPacing: on`: tcx-pod → clsact-pod (host-side dropped — EDT
  requires pod-side)

Selected via the conflist `attachMode` field at the plugin level, or
via `NATRA_ATTACH_MODE` on the install init container, or via
`NATRA_E2E_ATTACH_MODE` / `NATRA_PERF_ATTACH_MODE` on the test rig.

Each annotated direction adds one tcx link per pod (or one clsact
filter on the matching `HANDLE_MIN_*` parent). A bidi-annotated pod
in tcx mode therefore has two link pins under
`/sys/fs/bpf/natra/<containerID>-<side>-{ingress,egress}-link`.
The `<side>` field is `hostside` or `podside` matching the attach
mode the pod was started with.

Bpffs forbids `.` in pin path components — `kernel/bpf/inode.c::bpf_lookup`
returns EPERM on any name containing a dot when the parent has any
`S_IALLUGO` bits set. natra's pin paths use dotless `-link` and `-map`
suffixes accordingly. See `pkg/bpf/loader.go::PinMaps` and
`cmd/natra/main.go::pinPathFor`.

## CI workflows

Triggers: `push` to any branch + `pull_request`. No path filters, no
schedule gating. Concurrency-cancel in-progress runs per ref.

| Workflow      | Layer                        | Duration target |
|---------------|------------------------------|-----------------|
| `unit.yml`    | 1 (unit + fuzz + bench)      | <30s unit, <2m fuzz, <2m bench |
| `cni.yml`     | 2 (CNI + chaos)              | <3m |
| `bpf.yml`     | 3 (BPF, single kernel)       | <5m |
| `e2e.yml`     | 4 (k3d + chaos)              | <8m |
| `perf.yml`    | 5 (perf, single kernel)      | <5m |
| `license.yml` | go-licenses + scancode       | <2m |
| `ci.yml`      | aggregator (`needs:`)        | reads other jobs |

The aggregator gives branch protection a single status to read.

## `make ci` local mirror

Runs every layer + lint + license-scan in sequence, keeps going past
failures, prints a per-layer pass/fail summary, exits non-zero if any
failed. macOS without Docker skips L2-L5 with a clear message; Linux
runs everything.

## Fuzzing

- New crashing inputs land in
  `pkg/cni/config/testdata/fuzz/<FuzzName>/<sha>` and are committed
  to the repo so CI replays them on every push.
- Reproduce a crash:
  `go test -run=FuzzParseBandwidthAnnotation/<sha> ./pkg/cni/config/...`
- The default `-fuzztime=30s` is for the agent feedback loop. For
  release validation, raise it: `go test -fuzz=Fuzz... -fuzztime=1h`.
- The fuzz job has `-test.timeout=2m` to give the GH runner wind-down
  headroom — without it, slow runners hit "context deadline exceeded"
  on the last in-flight iteration after fuzztime fires.

## Constraints not yet covered

- **Multi-kernel matrix.** The lvh image registry was unreliable
  (`manifest unknown` on the kernel tags). L3 and L5 currently run
  against the runner's host kernel only.
- **Real-veth in L3.** `BPF_PROG_RUN`'s ~3,772 B input cap rules out
  jumbo behavior at the BPF unit level. Real-veth coverage is
  currently in L4 only.
- **Single-kernel L4/L5 topology (k3d).** k3d's "nodes" are
  containers sharing one Linux kernel and one docker bridge. The
  cross-kernel signal lives in the lima-based vm-rig
  (`make test-vm` / `scripts/vm-rig/`) — two VMs, two kernels, one
  k3s cluster joined over the lima shared network — which is
  invoked manually and runs alongside the k3d-based L4 e2e. Real
  NICs, hardware queueing, and real wire latency still aren't
  reached; see `docs/test-environments.md` for the cloud-VM and
  bare-metal escalation steps.
- **Cilium / AWS NPA coexistence.** natra composes via bpf_mprog at
  the TCX hook by construction; no end-to-end rig with a loaded
  cilium / NPA cluster has been run yet. Validation needs a real
  EKS-or-similar cluster.
- **`linux/arm64` in CI.** Local Mac dev runs arm64; CI runs amd64.
- ~~**Bystander cost from EDT preservation.**~~ Resolved by
  273a99f — bounded EDT delay at 50 ms, fall through to ECN-mark
  above. Measured on perf-vs-vanilla Workload 2: bystander p99
  61 → 27 ms, annotated mice p99 69 → 28 ms, egress 9.16 → 10.03
  Mbps (closer to cap, not under). Egress stays in the 5%
  envelope.
