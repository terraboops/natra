//go:build linux && bpf

// Layer 3 — BPF dataplane happy-path tests.
//
// Loads the natra BPF object via cilium/ebpf, exercises it with synthetic
// packets via BPF_PROG_RUN, and verifies map state. Runs inside an lvh
// qemu VM at the kernel requested in CI matrix (5.15 / 6.6 / bpf-next).
//
// Phase 0 placeholder: only loads bpf/placeholder.bpf.o and verifies the
// program can be instantiated. The real heavy-hitter / token-bucket
// assertions land alongside Phase 1 BPF code.

package bpf_test

import (
	"path/filepath"
	"runtime"
	"testing"

	"github.com/cilium/ebpf"
	"github.com/cilium/ebpf/rlimit"
)

func bpfObjectPath(name string) string {
	_, thisFile, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(thisFile), "..", "..", "bpf", name)
}

func TestPlaceholderLoads(t *testing.T) {
	if err := rlimit.RemoveMemlock(); err != nil {
		t.Fatalf("remove memlock: %v", err)
	}

	path := bpfObjectPath("placeholder.bpf.o")
	spec, err := ebpf.LoadCollectionSpec(path)
	if err != nil {
		t.Fatalf("load spec %s: %v", path, err)
	}

	coll, err := ebpf.NewCollection(spec)
	if err != nil {
		t.Fatalf("instantiate collection: %v", err)
	}
	t.Cleanup(coll.Close)

	prog, ok := coll.Programs["natra_placeholder"]
	if !ok {
		t.Fatalf("program 'natra_placeholder' not found in collection; got: %v", maps(coll.Programs))
	}

	// 64-byte synthetic Ethernet+IP packet — minimal valid input for skb.
	// Phase 0 only checks the program runs and returns TC_ACT_OK (= 0).
	pkt := make([]byte, 64)
	ret, _, err := prog.Test(pkt)
	if err != nil {
		t.Fatalf("BPF_PROG_RUN: %v", err)
	}
	if ret != 0 {
		t.Errorf("placeholder returned %d, want 0 (TC_ACT_OK)", ret)
	}
}

func maps[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return keys
}
