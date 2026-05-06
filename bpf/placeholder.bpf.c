// SPDX-License-Identifier: Apache-2.0
//
// Trivial pass-through BPF program. Used by the Layer 3 sanity test
// to prove the loader plumbing — clang→bytecode→cilium/ebpf→kernel
// →BPF_PROG_RUN — works end-to-end. Production never loads this; the
// real dataplane is bpf/natra.bpf.c.
//
// Compiles via `clang -target bpf` without kernel headers; loaded only
// on Linux.

#define TC_ACT_OK 0

// __section is the libbpf-compatible way to put the function into the
// "tc" section so it loads as a tcx/clsact-attachable program.
#define __section(s) __attribute__((section(s), used))

__section("tc")
int natra_placeholder(void *ctx)
{
	(void)ctx;
	return TC_ACT_OK;
}

char __license[] __section("license") = "Apache-2.0";
