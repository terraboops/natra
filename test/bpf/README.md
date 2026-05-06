# Layer 3 — BPF dataplane tests

Loads `bpf/natra.bpf.o`, drives it via `BPF_PROG_RUN`, and asserts on
the verdicts and stats. The original plan was a kernel matrix via lvh;
the upstream lvh image registry has been unstable, so for now each run
executes against a single host kernel (Docker Desktop on Mac, the runner
kernel in CI). See `TODO_LINUX.md` for restoring the matrix.

## Quick reference

```bash
make test-bpf            # one run, one kernel
```

## Files

- `prog_linux_test.go` — placeholder load + sanity.
- `ratelimit_linux_test.go` — token-bucket and CMS classification.
- `chaos_linux_test.go` — verifier rejection of invalid programs,
  malformed packets, concurrent map updates, CMS saturation.
- `edge_cases_linux_test.go` — packet-bigger-than-burst, ICMP, IPv4
  options, zero burst, rapid config change, jumbo packets, counter
  overflow.
- `prog_stub_test.go` — non-Linux skip.
- `testdata/` — intentionally-invalid BPF programs for the verifier
  rejection test.

## Prerequisites and full context

See `TODO_LINUX.md` at the repo root.
