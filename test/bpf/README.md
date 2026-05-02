# Layer 3 — BPF dataplane tests

Loads natra's BPF program inside an lvh qemu VM and exercises it across
kernels 5.15, 6.6, and bpf-next. This is the only layer that validates
kernel-version compatibility — every other layer runs against one kernel.

## Quick reference

```bash
# Local (Linux + KVM):
make test-bpf KERNEL=6.6
make test-bpf-all

# CI: runs the full matrix on every push (.github/workflows/bpf.yml).
```

## Files

- `lvh-config.yaml` — kernel matrix.
- `run-in-vm.sh` — driver script (host side).
- `in-vm-runner.sh` — entry point inside the VM.
- `prog_linux_test.go` — happy-path BPF load + `BPF_PROG_RUN`.
- `chaos_linux_test.go` — verifier rejection, map OOM, malformed packets,
  detach race, kernel-feature fallback (Phase 1).
- `prog_stub_test.go` — non-Linux skip stub.
- `testdata/` — intentionally-invalid BPF programs for chaos tests.

## Prerequisites and full context

See `TODO_LINUX.md` at the repo root.
