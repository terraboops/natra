# Layer 3 — BPF dataplane tests

Loads `bpf/natra.bpf.o`, drives it via `BPF_PROG_RUN`, asserts on the
verdicts and stats. Single host kernel per run; see `TODO_LINUX.md`
for restoring the kernel matrix.

```bash
make test-bpf
```

## Files

- `prog_linux_test.go` — placeholder load + sanity.
- `ratelimit_linux_test.go` — token-bucket + CMS classification.
- `chaos_linux_test.go` — verifier rejection, malformed packets,
  concurrent map updates, CMS saturation.
- `edge_cases_linux_test.go` — packet > burst, ICMP, IPv4 options,
  zero burst, rapid config change, jumbo packets, counter overflow.
- `prog_stub_test.go` — non-Linux skip.
- `testdata/` — intentionally-invalid BPF programs for the verifier
  rejection test.

Prerequisites and details: `TODO_LINUX.md`.
