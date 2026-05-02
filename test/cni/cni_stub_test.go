//go:build !linux

// Stub for non-Linux platforms. The real Layer 2 tests live in
// cni_linux_test.go and require Linux network namespaces (CAP_NET_ADMIN).
// On macOS, run them via `make test-cni`, which invokes
// scripts/run-in-docker.sh to provide a Linux kernel.

package cni_test

import "testing"

func TestCNILayerSkipped(t *testing.T) {
	t.Skip("Layer 2 CNI tests require Linux — run `make test-cni` on macOS to invoke them via Docker")
}
