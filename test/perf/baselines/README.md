# Layer 5 perf baselines

One JSON file per kernel matrix cell. CI compares the current run's
metrics against these and fails on regression > 10% or natra losing to
vanilla on the mixed scenario.

Files (Phase 1):
- `5.15.json`
- `6.6.json`
- `bpf-next.json`

To regenerate after an intentional perf-affecting change:

```bash
make perf-baseline KERNEL=6.6
# inspect the diff, commit in a dedicated PR titled
# "perf: refresh baselines for kernel 6.6"
```

**Don't** regenerate baselines to silence a regression. Investigate
first; only refresh after the regression has been understood and
accepted.
