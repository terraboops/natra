//go:build !linux

// Stub for non-Linux platforms. The real Layer 3 BPF dataplane tests live
// in prog_linux_test.go and require a Linux kernel + KVM via lvh.
// On macOS, see TODO_LINUX.md §"Layer 3 local on Mac" (lima/orbstack
// escape hatch). GH Actions runs the full kernel matrix on every push.

package bpf_test

import "testing"

func TestBPFLayerSkipped(t *testing.T) {
	t.Skip("Layer 3 BPF tests require Linux + lvh; see TODO_LINUX.md")
}
