//go:build !linux

// Stub for non-Linux platforms. The real Layer 3 BPF dataplane tests
// live in prog_linux_test.go and need a Linux kernel for BPF_PROG_RUN.
// On macOS, run them via `make test-bpf`, which invokes
// scripts/run-in-docker.sh to provide one.

package bpf_test

import "testing"

func TestBPFLayerSkipped(t *testing.T) {
	t.Skip("Layer 3 BPF tests require Linux — run `make test-bpf` on macOS to invoke them via Docker")
}
