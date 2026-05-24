# Development

## Prerequisites

- Go 1.25+ (matches `go.mod`)
- Docker (colima or Docker Desktop on macOS) — provides the Linux
  kernel for L2/L4/L5 test runs and for the production-image build
- LLVM clang with the `bpf` target. Apple clang lacks it; on macOS,
  `brew install llvm` and the Makefile picks up `/opt/homebrew/opt/llvm/bin/clang`.
- `kubectl` and `k3d` for L4 e2e + `scripts/soak-test.sh`
  (`brew install k3d`)

## Git hooks

Once per clone:

```bash
make hooks-install
```

That points `core.hooksPath` at `./.githooks/`, which contains:

- **pre-commit** → `make pre-commit` (fmt-check, vet, lint, build).
  ~20s. Catches what would otherwise burn a CI cycle.
- **pre-push** → `make pre-push` (pre-commit + L1 unit tests + 10s
  fuzz). ~1-2 min.

CI's lint job invokes `make pre-commit` too — same target, same
binary, same config. Local and CI can't drift.

Bypass with `--no-verify` if you really need to. To disable the
hooks entirely: `make hooks-uninstall`.

## Build

```bash
make build         # natra binary, Linux ELF, BPF object embedded
make docker-build  # container image for the install DaemonSet
make build-bpf     # compile bpf/*.bpf.c → *.bpf.o (used by Layer 3)
```

`make build` cross-compiles cleanly on macOS by running the Linux
toolchain inside a `golang:1.25` container; the natra binary is always
a Linux ELF.

## Tests

The harness has five layers, all gated by Go build tags so plain
`go test ./...` only runs L1.

```bash
make test-unit       # L1a: Ginkgo unit tests
make test-fuzz       # L1b: 30s fuzz against the parser
make test-bench      # L1c: hot-path Go benchmarks
make test-cni        # L2: CNI protocol (privileged container)
make test-bpf        # L3: BPF dataplane via BPF_PROG_RUN
make test-e2e        # L4: k3d end-to-end with iperf throughput assertion
make test-perf       # L5: BPF_PROG_RUN perf scenarios + synthetic vs-vanilla
make ci              # All of the above + lint + license scan
```

L2/L3/L4/L5 all run on macOS via Docker. All four share one Linux
kernel (colima's LinuxKit VM on Mac; the runner kernel on GH).

For real kernel-to-kernel coverage there's a separate on-demand
two-VM lima rig:

```bash
make test-vm                  # two-VM k3s cluster under lima, real
                              # cross-kernel pod traffic: natra
                              # throttle + CMS fast-pass.
make perf-vs-vanilla-vm       # baseline/natra/upstream comparison on
                              # two real kernels, flannel host-gw CNI
                              # (default), fresh cluster per phase
                              # (~40 min). macOS prereq: brew install
                              # socket_vmnet (do NOT `brew services
                              # start` it). See scripts/vm-rig/README.md.
make perf-vs-vanilla-vm-cilium # Same as above but with cilium as the
                              # CNI (proxies for AWS NPA; exercises
                              # the bpf_mprog coexistence path at pod
                              # TCX). VMRIG_CNI=cilium under the hood.
```

For what each layer actually validates — and the wire-level
behaviors none of these reach (real NICs, switch queueing, etc.) —
see `docs/test-environments.md`.

The cluster-level head-to-head against the upstream
`containernetworking/plugins/bandwidth` plugin runs locally and in
CI via the shared `internal/perfrig` executor:

```bash
make perf-vs-vanilla    # k3d substrate, ci profile (~18-22 min,
                        # also runs per-push in GH Actions).
```

Both rigs share the same Spec/Executor; the only difference is the
Substrate impl. A unit test asserts `ci ⊆ full` so the k3d profile
is structurally a subset of what the lima rig runs. See
`docs/perf-vs-vanilla.md` for what either measures.

## Code quality

```bash
make fmt   # go fmt ./...
make vet   # go vet ./...
make lint  # golangci-lint v2.5.0 (auto-installed under bin/)
make check # fmt + vet + lint
```

## Local deploy

```bash
k3d cluster create natra-dev --agents 1
make docker-build
k3d image import ghcr.io/terraboops/natra:latest --cluster natra-dev
kubectl apply -f deploy/cni-installer.yaml
kubectl get pods -n kube-system -l app=natra
```

To pin an explicit attach mode (default is `auto`, which auto-detects
TCX vs clsact and host vs pod):

```bash
NATRA_E2E_ATTACH_MODE=clsact-podside make test-e2e   # or tcx-podside, clsact-hostside
```

## Layout

```
cmd/natra/             CNI plugin entry point + install + dump-stats
cmd/perfrig/           k3d substrate frontend; invoked by make
                       perf-vs-vanilla and the GH CI perf-vs-vanilla
                       job. Lima vm-rig has its own entry under
                       cmd/vm-rig perfvsvanilla.
cmd/vm-rig/            Lima two-VM rig lifecycle: up/down/install/test/
                       perfvsvanilla. Reads VMRIG_CNI={flannel,cilium}.
internal/perfrig/      Shared Spec + Executor + Substrate interface that
                       both rigs run through.
pkg/bpf/               Go loader; embeds bpf/natra.bpf.o
pkg/cni/config/        Bandwidth annotation parser
bpf/                   BPF C source (natra.bpf.c, vanilla.bpf.c, placeholder.bpf.c)
deploy/                DaemonSet manifest, Dockerfile
test/cni/              L2 CNI protocol tests
test/bpf/              L3 BPF dataplane + chaos + edge-case tests
test/e2e/              L4 k3d end-to-end + chaos
test/perf/             L5 perf scenarios; test/perf/realworld holds the
                       perf-server / perf-client / bystander manifests
                       both perfrig substrates consume.
docs/                  Architecture, CNI spec, this guide, blog
scripts/               run-in-docker wrapper, license-scan, vm-rig/
                       (lima-server-{flannel,cilium}.yaml +
                       lima-agent-*.yaml), perf-vs-vanilla.sh
                       (thin k3d-bootstrap shim → cmd/perfrig)
TODO_LINUX.md          Linux-only test layer details
```

## Debugging the CNI plugin

The natra binary writes to `/var/log/natra-cni.log` on the node on
every CNI invocation; the DaemonSet host-mounts the path so a
single `tail -f /var/log/natra-cni.log` shows everything across
pods.

To inspect attached programs and pinned objects on a k3d node:

```bash
docker exec <node> bpftool link list
docker exec <node> ls /sys/fs/bpf/natra/
docker exec <node> cat /var/lib/rancher/k3s/agent/etc/cni/net.d/00-natra-*.conflist
```

To read live counters from a running attachment:

```bash
# inside a node, with bpftool installed:
natra dump-stats <pod-sandbox-id>
```

The argument is the pod *sandbox* ID (what containerd passes via
CNI_CONTAINERID), not the kubectl-visible app container ID. See
`docs/troubleshooting.md` for how to find it via crictl.
