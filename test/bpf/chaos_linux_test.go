//go:build linux && bpf

// Layer 3 chaos — BPF failure modes.
//
// Phase 1 lands TestVerifierRejection — a real test that loads an
// intentionally-invalid BPF program and asserts the verifier rejects
// it with a clear, multi-line *ebpf.VerifierError. The remaining
// scenarios are still stubbed for future iterations.

package bpf_test

import (
	"errors"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/cilium/ebpf"
	"github.com/cilium/ebpf/rlimit"
)

// invalidBPFPath returns the absolute path to a chaos-testdata BPF
// object built by `make build-bpf`. Each .bpf.c under bpf/testdata/
// compiles to a .bpf.o that the verifier MUST reject.
func invalidBPFPath(name string) string {
	_, thisFile, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(thisFile), "..", "..", "bpf", "testdata", name)
}

func TestVerifierRejection(t *testing.T) {
	if err := rlimit.RemoveMemlock(); err != nil {
		t.Fatalf("remove memlock: %v", err)
	}

	path := invalidBPFPath("invalid_oob_packet_access.bpf.o")
	spec, err := ebpf.LoadCollectionSpec(path)
	if err != nil {
		t.Fatalf("load spec %s: %v", path, err)
	}

	coll, err := ebpf.NewCollection(spec)
	if err == nil {
		coll.Close()
		t.Fatalf("expected verifier rejection for %s, got nil error (collection loaded successfully)", path)
	}

	// The kernel verifier returns rich, multi-line error messages.
	// cilium/ebpf wraps them in *ebpf.VerifierError so callers can
	// detect the class of failure reliably; natra's pkg/bpf.Load()
	// uses the same `errors.As` shape.
	var verr *ebpf.VerifierError
	if !errors.As(err, &verr) {
		t.Fatalf("expected *ebpf.VerifierError, got %T: %v", err, err)
	}

	// The OOB packet read should produce a message about packet
	// access. Kernel wording varies ("invalid access to packet",
	// "R1 invalid mem access"), so we match the family rather than
	// an exact string.
	msg := verr.Error()
	if !strings.Contains(msg, "packet") && !strings.Contains(msg, "access") {
		t.Errorf("verifier message doesn't mention packet/access: %q", msg)
	}
	t.Logf("verifier rejected %s as expected (excerpt): %s",
		filepath.Base(path), firstLines(msg, 3))
}

// firstLines returns up to n lines of s, joined with " / " — keeps
// log output readable without losing the headline error.
func firstLines(s string, n int) string {
	parts := strings.SplitN(s, "\n", n+1)
	if len(parts) > n {
		parts = parts[:n]
	}
	return strings.Join(parts, " / ")
}

// Remaining scenarios are still stubbed; track via test names appearing
// in CI output as SKIP so the work is visible without TODOs in code.
//
// (The skips below are kept verbatim from Phase 0 scaffolding.)

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
