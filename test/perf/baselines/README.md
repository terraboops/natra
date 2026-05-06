# Layer 5 perf baselines

One JSON file per kernel target. The L5 perf test compares the current
run's `bpf_prog_run_ns_per_op` against the recorded ceiling and fails
on regression. Baselines:

- `local.json` — Docker (Mac) / local Linux, `KERNEL` unset
- `5.15.json`, `6.6.json`, `bpf-next.json` — when the kernel matrix is
  back (lvh registry currently unusable; see TODO_LINUX.md)

To regenerate after an intentional perf-affecting change:

```bash
make perf-baseline KERNEL=6.6
# inspect the diff and commit in a dedicated PR
```

Don't regenerate to silence a regression — investigate first.
