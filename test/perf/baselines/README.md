# Layer 5 perf baselines

The L5 perf test compares each run's `bpf_prog_run_ns_per_op` against
a recorded ceiling and fails on regression. The kernel tag comes from
the `KERNEL` env var (set to `local` in the GH Actions workflow and
absent in `make test-perf` defaults; the test falls back to `local`).

Currently the only baseline file is `local.json` — generous ceiling
sized for the GH Actions runner.

To regenerate after an intentional perf-affecting change, run the
test, take the reported `ns/op`, write it (with headroom) into
`local.json`, and commit. Don't refresh to silence a regression —
investigate first.
