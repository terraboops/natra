# test/bpf/testdata

Intentionally-invalid BPF C sources for the verifier-rejection chaos
tests. Each file provokes a specific verifier failure mode:

- `invalid_oob_packet_access.bpf.c` — out-of-bounds packet read; the
  verifier rejects with "invalid access to packet" or similar.

These compile via the standard `make build-bpf` target (the same flags
as the production programs) so the verifier-rejection assertions stay
honest — the test fails meaningfully when *clang* drift would otherwise
produce different bytecode.
