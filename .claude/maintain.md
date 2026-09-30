# Maintenance facts

- Repo: `terraboops/natra`, default branch `main`. Maintainer: @terraboops.
- History is linear, one commit per change. Merge PRs with squash and
  delete the branch.
- Hooks: `make hooks-install` points git at `.githooks/`
  (pre-commit = `make pre-commit`, pre-push = `make pre-push`).

## Tests

- Fast: `make pre-push` (fmt-check, vet, golangci-lint, L1 unit, 30s fuzz).
- Linux layers on macOS run in Docker via `scripts/run-in-docker.sh`
  (colima is the backend):
  - `make test-cni` (L2, builds `bin/natra` first)
  - `make test-bpf` (L3)
  - `make test-e2e` (L4, k3d, ~5 min)
  - `make test-perf` (L5)
- `make ci` runs every layer and prints a per-layer summary.
- Benchmark results must never get worse. CI's "Bench (compare to
  main)" job runs `scripts/bench-check.sh`: interleaved main/HEAD
  rounds (`bench-ab.sh`, each side on its own go.mod toolchain; Go
  benchmarks + BPF hot-path `BenchmarkBPF`), gated by `bench-gate.sh`.
  Any significant regression triggers a second A/B; it fails if the
  same benchmark regresses again.
- Sequential local microbenchmarks are unreliable on a laptop (the same
  binary measured 60% apart an hour later). Compare interleaved, and
  measure CNI invoke RSS with many interleaved `/usr/bin/time -v` runs
  rather than the k3d rig's 3-sample poll.
  For dataplane changes also compare `PERF_PROFILE=full make
  perf-vs-vanilla` on main vs the branch before merging.
- Head-to-head numbers: `make perf-vs-vanilla` (k3d, ~20 min) and the
  `perf-vs-vanilla-vm*` targets (lima, 40-50 min). Only run when a
  change touches the dataplane or the perf rig.

## Running it

natra is a CNI plugin, so "running the app" means L4: `make test-e2e`
creates a k3d cluster, installs natra, pushes traffic through shaped
pods, and tears the cluster down.

## Gotchas

- colima shares only `$HOME` with its VM. Worktrees for Docker-backed
  layers must live under `$HOME`, not `/tmp`.
- golangci-lint must be built with a Go at least as new as go.mod's
  directive and must understand that Go's export data. A Go minor bump
  usually needs a golangci-lint bump in the Makefile.
- The Go directive in `go.mod` is the only patch-level pin; the Docker
  builder images use the minor tag (`golang:1.NN`).
- `cilium/ebpf` and `x/sys` touch the BPF loader and netlink paths: run
  `make test-bpf` and `make test-e2e` after bumping them, not just L1.
