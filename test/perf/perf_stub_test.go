//go:build !linux

// Stub for non-Linux platforms. The real Layer 5 perf scenarios live
// in perf_linux_test.go and need a Linux kernel for BPF_PROG_RUN.
// On macOS, run them via `make test-perf`, which invokes
// scripts/run-in-docker.sh to provide one.

package perf_test

import "testing"

func TestPerfLayerSkipped(t *testing.T) {
	t.Skip("Layer 5 perf tests require Linux — run `make test-perf` on macOS to invoke them via Docker")
}
