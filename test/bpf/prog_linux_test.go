//go:build linux && bpf

// Layer 3 — BPF dataplane sanity test. Loads bpf/placeholder.bpf.o
// and runs BPF_PROG_RUN against it, just to confirm the loader path
// is wired up. The real natra BPF assertions are in
// ratelimit_linux_test.go and edge_cases_linux_test.go.

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

	// 64-byte synthetic Ethernet+IP packet, the minimum valid input
	// for an skb-typed BPF_PROG_RUN. Asserting only that the program
	// runs and returns TC_ACT_OK.
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
