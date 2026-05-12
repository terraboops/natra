# Development

## Prerequisites

- Go 1.25+ (matches `go.mod`)
- Docker (colima or Docker Desktop on macOS) — provides the Linux
  kernel for L2/L4/L5 test runs and for the production-image build
- LLVM clang with the `bpf` target. Apple clang lacks it; on macOS,
  `brew install llvm` and the Makefile picks up `/opt/homebrew/opt/llvm/bin/clang`.
- `kubectl` and `kind` for L4
- `k3d` (optional) — only needed for `scripts/soak-test.sh`, which
  uses a different cluster base than the rest of the test rig to
  exercise node-membership changes (`brew install k3d`).

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
make test-e2e        # L4: kind end-to-end with iperf throughput assertion
make test-perf       # L5: BPF_PROG_RUN perf scenarios + synthetic vs-vanilla
make ci              # All of the above + lint + license scan
```

L2/L3/L4/L5 all run on macOS via Docker. None of them require lvh.

A real-cluster head-to-head against the upstream
`containernetworking/plugins/bandwidth` plugin is available
on-demand:

```bash
make perf-vs-vanilla   # ~6 min; two kind clusters, real iperf
```

See `docs/perf-vs-vanilla.md` for what it measures.

## Code quality

```bash
make fmt   # go fmt ./...
make vet   # go vet ./...
make lint  # golangci-lint v2.5.0 (auto-installed under bin/)
make check # fmt + vet + lint
```

## Local deploy

```bash
kind create cluster --name natra-dev
make docker-build
kind load docker-image ghcr.io/terraboops/natra:latest --name natra-dev
kubectl apply -f deploy/cni-installer.yaml
kubectl get pods -n kube-system -l app=natra
```

To exercise the opt-in fallback attach path:

```bash
NATRA_E2E_ATTACH_MODE=clsact-podside make test-e2e   # or tcx-podside, clsact-hostside
```

## Layout

```
cmd/natra/             CNI plugin entry point + install + dump-stats
pkg/bpf/               Go loader; embeds bpf/natra.bpf.o
pkg/cni/config/        Bandwidth annotation parser
bpf/                   BPF C source (natra.bpf.c, vanilla.bpf.c, placeholder.bpf.c)
deploy/                DaemonSet manifest, Dockerfile
test/cni/              L2 CNI protocol tests
test/bpf/              L3 BPF dataplane + chaos + edge-case tests
test/e2e/              L4 kind end-to-end + chaos
test/perf/             L5 perf scenarios; test/perf/realworld for the
                       on-demand vs-vanilla cluster comparison
docs/                  Architecture, CNI spec, this guide, blog
scripts/               run-in-docker wrapper, license-scan, perf-vs-vanilla
TODO_LINUX.md          Linux-only test layer details
```

## Debugging the CNI plugin

The natra binary writes to `/var/log/natra-cni.log` on the node on
every CNI invocation; the DaemonSet host-mounts the path so a
single `tail -f /var/log/natra-cni.log` shows everything across
pods.

To inspect attached programs and pinned objects on a kind node:

```bash
docker exec <node> bpftool link list
docker exec <node> ls /sys/fs/bpf/natra/
docker exec <node> cat /etc/cni/net.d/00-natra-*.conflist
```

To read live counters from a running attachment:

```bash
# inside a node, with bpftool installed:
natra dump-stats <containerID>
```
