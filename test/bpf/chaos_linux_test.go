//go:build linux && bpf

// Layer 3 chaos — BPF failure modes.
//
// Phase 0 stubs out scenarios that need real natra BPF code (verifier
// rejection of intentionally-bad programs, map OOM, malformed packets,
// concurrent map updates, kernel-feature fallback, detach race). Each
// is a t.Skip with a one-line note so the test names appear in CI output
// and the work to flesh them out is discoverable.

package bpf_test

import "testing"

func TestVerifierRejection(t *testing.T) {
	t.Skip("Phase 1: load test/bpf/testdata/invalid_*.bpf.o, assert verifier error reported clearly")
}

func TestMapCapacityOOM(t *testing.T) {
	t.Skip("Phase 1: fill cms_map and decision_map past max_entries, assert graceful behavior (CMS approximate, LRU evict)")
}

func TestMalformedPackets(t *testing.T) {
	t.Skip("Phase 1: BPF_PROG_RUN with truncated IP, bad checksums, IPv6 next-header chains, fragmented, multicast-mac-with-unicast-dst, zero-length payloads")
}

func TestConcurrentMapUpdates(t *testing.T) {
	t.Skip("Phase 1: Program.Run from multiple goroutines, verify CMS counter convergence within tolerance")
}

func TestKernelFeatureFallback(t *testing.T) {
	t.Skip("Phase 1: on 5.15, assert tcx unavailable -> clsact fallback; on 6.6+, assert tcx selected")
}

func TestDetachRace(t *testing.T) {
	t.Skip("Phase 1: attach/detach in tight loop while injecting traffic; assert bpftool prog list and /sys/fs/bpf clean after suite")
}
