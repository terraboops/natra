// SPDX-License-Identifier: Apache-2.0
//
// Placeholder BPF program. Lets Layer 3 (lvh) prove its plumbing — load,
// attach, BPF_PROG_RUN — works end-to-end before any real natra BPF code
// exists. Phase 1 will replace this with the actual heavy-hitter / token
// bucket dataplane.
//
// Compiled on macOS via `clang -target bpf` (no kernel headers needed for
// this trivial program). Loaded only on Linux (Layer 3 in lvh VMs).

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
