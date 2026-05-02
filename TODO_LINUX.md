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
|     4 | kind end-to-end with iperf assertions         | `e2e`        |
|     5 | Perf scenarios + synthetic vs-vanilla         | `perf`       |

Plain `go test ./...` runs only L1.

Every layer runs from `make ci` on macOS (via Docker) and on Linux
(natively or via Docker). The vs-vanilla cluster comparison is
on-demand via `make perf-vs-vanilla`, not part of `make ci`.

## Layer 2 — CNI protocol

Files under `test/cni/`:
- `cni_linux_test.go` — happy-path ADD/DEL/CHECK + the two attach
  modes (tcx, clsact-podside).
- `chaos_linux_test.go` — malformed stdin, annotation injection, bad
  CNI env vars.
- `helpers_linux_test.go` — netns lifecycle, exec, env-var
  construction, `linkPinExists` and `remainingPinsFor`.
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
- `ratelimit_linux_test.go` — token-bucket and CMS classification.
- `chaos_linux_test.go` — verifier rejection of intentionally invalid
  programs, malformed packets, concurrent map updates, CMS
  saturation.
- `edge_cases_linux_test.go` — packet > burst, ICMP without L4 ports,
  IPv4 options, zero burst, rapid config change, jumbo packets,
  counter overflow.
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

## Layer 4 — kind end-to-end

Files under `test/e2e/`:
- `kind-config.yaml` — 2-node kind cluster, kindnet as main CNI.
- `manifests/{namespace,iperf-server,iperf-client}.yaml` — server
  on the worker with the `kubernetes.io/ingress-bandwidth: "10M"`
  annotation, client on the control-plane with the standard
  control-plane toleration so traffic crosses the inter-node fabric.
- `e2e_test.go` — connectivity smoke + bandwidth-enforcement
  assertion (within +20% of the annotated rate).
- `chaos_test.go` — DaemonSet restart mid-traffic, pod churn, plus
  three pending characterization specs (PIt).

Run:
```bash
make test-e2e                                 # default tcx
NATRA_E2E_ATTACH_MODE=clsact-podside make test-e2e
```

Prerequisites:
- Docker (colima or Docker Desktop on macOS, dockerd on Linux).
- `kind`, `kubectl`.
- `iperf3` (in-pod, image `networkstatic/iperf3:latest`).

Failure-mode dumps: on iperf-Ready timeout, BeforeSuite emits
`kubectl describe pod`, the install init-container log, and the
patched conflist. `NATRA_E2E_KEEP=1 make test-e2e` leaves the kind
cluster up after the test.

## Layer 5 — Perf

Files under `test/perf/`:
- `perf_linux_test.go` —
  - `TestBPFProgRunThroughput` — placeholder ns/op vs baseline.
  - `TestScenarioOneElephant` — single elephant, expect throttling.
  - `TestScenarioThousandMice` — 1000 short flows, expect zero
    `hh_hits`.
  - `TestScenarioMixed` — elephant + mice, mice survive.
  - `TestScenarioMixedVsVanilla` — head-to-head vs `bpf/vanilla.bpf.o`.
- `perf_stub_test.go` — non-Linux skip.
- `baselines/{local,5.15,6.6,bpf-next}.json` — per-kernel ns/op
  ceilings; the script compares the current run against the matching
  file.
- `realworld/vanilla-installer.yaml` — DaemonSet that fetches the
  upstream `bandwidth` plugin and chains it after kindnet, used by
  `make perf-vs-vanilla`.
- `scenarios/scenarios.go` — shared types; not currently exercised.

Run:
```bash
make test-perf            # synthetic, in-process, BPF_PROG_RUN
make perf-vs-vanilla      # real-cluster, ~6 min
```

The mixed scenario is elephant-first by design: the elephant
pre-drains the bucket, then mice arrive into the depleted bucket.
Interleaved sequences let the bucket refill between elephant packets
and trivially pass under both implementations.

## Attach modes for tests

natra's default is `tcx` (kernel 6.6+). The opt-in fallback
`clsact-podside` is for older kernels. Selected via the conflist
`attachMode` field at the plugin level, or via `NATRA_ATTACH_MODE`
on the install init container, or via `NATRA_E2E_ATTACH_MODE` /
`NATRA_PERF_ATTACH_MODE` on the test rig.

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
| `e2e.yml`     | 4 (kind + chaos)             | <8m |
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
- **AWS VPC CNI coexistence.** natra and AWS's network-policy-agent
  both operate at the same hook layer; tcx-mode coexistence is the
  intended path. Validation needs a real EKS cluster.
- **`linux/arm64` in CI.** Local Mac dev runs arm64; CI runs amd64.
