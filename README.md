# Natra

[![CI](https://github.com/terraboops/natra/workflows/CI/badge.svg)](https://github.com/terraboops/natra/actions)
[![Go Report Card](https://goreportcard.com/badge/github.com/terraboops/natra)](https://goreportcard.com/report/github.com/terraboops/natra)
[![License](https://img.shields.io/badge/License-Apache%202.0-blue.svg)](LICENSE)

> **Status: experimental.** A few days old; no production users; tested
> on kind + colima. See [Limitations](#limitations) before deploying.

A chained CNI plugin that rate-limits Pod traffic in either direction.
Reads the standard `kubernetes.io/ingress-bandwidth` and
`kubernetes.io/egress-bandwidth` annotations, runs after an existing
main CNI (kindnet, calico, etc.), attaches a BPF program to the Pod's
veth — one program per direction, only the directions you annotate.

Two stages on the BPF dataplane: a Count-Min Sketch classifies each
flow, then heavy flows pay against a per-Pod token bucket while flows
under the threshold take a fast pass. The upstream
`containernetworking/plugins/bandwidth` plugin charges every packet
against one HTB bucket, so an elephant flow drains the bucket and
short-lived flows arrive empty-handed. natra's CMS-then-bucket
arrangement targets that asymmetry; whether the difference matters on
real workloads depends on the workload's flow-length distribution.

Above-rate packets aren't dropped by default. natra tries
`bpf_skb_ecn_set_ce` first (ECN-mark on ECN-capable flows, no
retransmits), then EDT pacing on egress (`skb->tstamp` honored by an
`fq` qdisc on pod-eth0), and only drops as a last resort for ingress
non-ECN traffic. Both attach mode and EDT are auto-detected per pod
at CNI ADD; operators can pin either if they need to. See
[docs/perf-vs-vanilla.md](docs/perf-vs-vanilla.md) for measured
results.

## Quick start

```bash
# Deploy
kubectl apply -f deploy/cni-installer.yaml

# Annotate a Pod (one or both directions)
kubectl run test --image=nginx \
  --annotations="kubernetes.io/ingress-bandwidth=10M,kubernetes.io/egress-bandwidth=10M"
```

The DaemonSet is one of three supported install paths — see
[docs/install.md](docs/install.md) if you'd rather bake natra into
your node image or install manually.

## Build

```bash
make build         # CNI binary, with the BPF object embedded
make docker-build  # container image for the DaemonSet
make test          # Layer 1 unit/fuzz/bench
make ci            # full matrix (lint, licenses, L1-L5)
```

## Requirements

- Linux kernel 6.6+ for tcx attach modes; 5.x+ for the clsact
  fallback modes.
- Go 1.25+ (matches `go.mod`).
- LLVM clang with the `bpf` target. Apple clang lacks it; on macOS
  `brew install llvm` and the Makefile picks it up.
- Docker (colima or Docker Desktop on macOS) for the container image
  build and any test layer that needs a Linux kernel.

## Limitations

- No production users. The code is days old.
- Tested on kind + colima. Not yet exercised on EKS, GKE, AKS, or a
  real bare-metal cluster.
- Default attach mode is `auto` — tries TCX (kernel 6.6+) then
  clsact, host-side then pod-side, taking the first that works.
  EDT pacing on egress also defaults to `auto`: natra probes `fq`
  install on pod-eth0 and uses EDT when the probe succeeds. Pin
  attach mode via `attachMode` in the conflist or
  `NATRA_ATTACH_MODE` env; pin EDT via `edtPacing` or
  `NATRA_EDT_PACING`. ECN-mark is always-on.
- CI runs against a single host kernel. There's no kernel matrix
  (the lvh image registry has been unreliable).
- L5 perf scenarios use `BPF_PROG_RUN` against synthetic packets,
  which has different timing characteristics than packets flowing
  through a NIC. The real-cluster head-to-head in
  [docs/perf-vs-vanilla.md](docs/perf-vs-vanilla.md) uses real iperf
  traffic in a kind cluster, but kind is not bare metal either.
- IPv6 is not classified. `parse_flow` returns -1 for non-IPv4, so
  IPv6 flows pass through unrate-limited.
- The CMS sketch is fixed at compile time at 32768 × 4 cells per
  direction (262144 cells total per pod, ~2 MiB). Past saturation,
  every flow's estimate collides with at least one other flow's;
  classification accuracy degrades silently. The chaos test confirms
  the program survives the condition, not that the classification
  stays meaningful.

## Docs

- [Architecture](docs/ARCHITECTURE.md) — components and data flow
- [CNI behavior](docs/cni-spec.md) — verbs, NetConf shape, attach modes
- [Performance vs. vanilla](docs/perf-vs-vanilla.md) — head-to-head
  with the upstream bandwidth plugin
- [Development](docs/development.md) — build, test, debug
- [Troubleshooting](docs/troubleshooting.md) — common failure modes
- [TODO_LINUX.md](TODO_LINUX.md) — per-layer test details

## License

Apache 2.0. See [LICENSE](LICENSE).
