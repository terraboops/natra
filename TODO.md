# TODO

Active open items. Grouped by what unblocks what; each has the
file or area to start in. For the per-test-layer constraints (lvh,
arm64, multi-kernel matrix) see also `TODO_LINUX.md`; for the
test-environment escalation ladder see `docs/test-environments.md`.

## vm-rig validation

- [ ] **Install `socket_vmnet` and run `make test-vm` end-to-end.**
  The Go rig (`cmd/vm-rig/`) compiles, gofmt/vet/lint clean, and
  the hey CSV parser has unit-test coverage — but the full
  bring-up + bidi-iperf + hey assertions have never been exercised
  on a real machine. macOS prereq:
  `brew install socket_vmnet && sudo brew services start socket_vmnet`.
  First-run likely tripwires: socket_vmnet path symlink, k3s
  `--flannel-backend=host-gw` interaction with lima's shared
  network, image-pull timing exceeding the 3-min provision budget
  in `cmd/vm-rig/up.go::waitForFile`.

- [ ] **vm-rig kernel-version matrix.** Both VM templates
  (`scripts/vm-rig/lima-{server,agent}.yaml`) hardcode Ubuntu 24.04
  (kernel 6.8.x). Document or scriptify a "server on 6.8, agent on
  5.x" path so the tcx→clsact fallback gets exercised under live
  traffic across kernel versions in the same cluster.

## vm-rig coverage gaps

- [ ] **L4 topologies beyond C in vm-rig.** The rig currently
  covers Topology C (bidi sequential iperf) + hey HTTP-mice
  fast-pass. The L4 e2e suite has A (ingress-only), B (egress-only),
  D (mixed: some pods annotated, some not), E (no annotations),
  F (no-plugin regression delta), G (proxy-like simultaneous bidir).
  When porting beyond C, factor out shared helpers (`runIperf`,
  `logThrottled`, calibration math) from `test/e2e/e2e_test.go`
  into an importable package so the L4 tests and the vm-rig stop
  duplicating logic.

## Coverage gaps not reachable from any local rig

- [ ] **cilium + AWS-NPA + natra composition validated under
  `bpf_mprog`.** Currently a claim derived from the bpf_mprog
  contract, not measured. Needs a rig with cilium installed
  alongside natra; closest practical path is the cloud-VM
  escalation step (#2 in `docs/test-environments.md`).

- [ ] **Cloud-VM rig.** 2-3 small VMs joined into a k8s cluster
  with real (cloud-virtual) NICs. Catches NIC-offload skew and
  switch-side queueing the vm-rig's software vmnet doesn't reach.
  See `docs/test-environments.md` for the design notes.

- [ ] **Bare-metal rig.** 2-3 hosts on a real switch. Ground
  truth for hardware-offload behavior. Heavy setup; probably one
  canonical reference rig per major release rather than per-PR.

- [ ] **arm64 in CI.** CI runs amd64; local Mac dev runs arm64.
  The BPF program uses byte-order-sensitive code (FNV-1a hash,
  IP-header parsing) — a per-arch CI matrix would catch silent
  endianness or alignment regressions. All workflows under
  `.github/workflows/*.yml` are amd64-only.

## Code-level open ends

From `docs/ARCHITECTURE.md§Open ends`:

- [ ] **IPv6 classification.** `parse_flow` in `bpf/natra.bpf.c`
  returns -1 for non-IPv4, so IPv6 flows pass through unrate-limited
  in both directions. Add an IPv6 header path that produces the
  same flow-key shape so the CMS treats IPv4 and IPv6 traffic
  symmetrically.

- [ ] **CO-RE in the BPF program.** Currently uses fixed kernel
  headers via `linux/*.h` includes. CO-RE (via `vmlinux.h` +
  `bpf_core_read`) lets the same `.bpf.o` load against different
  kernel versions without recompilation — valuable on
  heterogeneous clusters where node kernels drift.

- [ ] **ebpf_exporter integration** for the per-direction stats.
  `natra_stats_map` already exposes
  `passed/throttled/hh_hits/ecn_marked/edt_delayed/dropped` per
  direction; an ebpf_exporter config can scrape them straight into
  Prometheus without natra growing its own metrics server.

## Tooling polish

- [ ] **hey-as-library** in a custom Go loadgen image, replacing
  the shell-out to the `hey` binary. `github.com/rakyll/hey`'s
  `requester` package exposes `Work` + `Run()`, but extracting
  results requires either stdout capture or a small fork to expose
  the private results channel. Alternative: switch to vegeta
  (`github.com/tsenart/vegeta/v12/lib`) which has a stable
  library API with marshalable `Metrics`. Worth doing when we
  want richer per-percentile output than `hey -o csv` produces.

- [ ] **vm-rig egress-only topology (Topology B).** Same shape as
  the current bidi-iperf path but with a server pod annotated only
  on egress. Smallest possible expansion of vm-rig coverage and
  validates the egress-only attach path in cross-kernel mode.

## Deferred (not on the roadmap right now)

These exist as `PIt` specs in `test/e2e/chaos_test.go` and stay
deferred for the reasons captured at the spec sites:

- Node drain: requires multi-node scheduling not exercised by the
  current single-worker e2e.
- CNI binary corruption: covered semi-implicitly by fail-open
  elsewhere; explicit corruption-path test is low-value vs effort.
- Annotation update on a running pod: Kubernetes itself doesn't
  propagate annotation changes to running pods, so the test
  characterizes a kubelet behavior, not a natra behavior.
