# test/bpf/testdata

Holds intentionally-invalid BPF C sources used by the verifier-rejection
chaos tests. Each file is named for the failure mode it provokes:

- `invalid_unbounded_loop.bpf.c` — verifier rejects unbounded loops on
  pre-5.3 kernels and bounded ones on later kernels with the wrong shape.
- `invalid_oob_map_access.bpf.c` — out-of-bounds map index access.
- `invalid_uninit_read.bpf.c` — reading from uninitialized map values.

These files are **not** built by the default `make build-bpf` target;
they're built by the Layer 3 chaos suite on demand when running inside
lvh. Phase 0: empty placeholder; Phase 1 fills these in alongside real
natra BPF code so the chaos tests have something to point at.
