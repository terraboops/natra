//go:build linux && perf

// Layer 5 — head-to-head perf vs upstream containernetworking/plugins/bandwidth.
//
// The project's pitch is "smarter than vanilla." This layer makes that
// claim falsifiable on every push. Each scenario runs against both
// plugins; results are diffed against baselines/<kernel>.json and the
// test fails if natra regresses or stops winning the mixed scenario.
//
// Phase 0: a single end-to-end micro-scenario runs (BPF_PROG_RUN
// throughput against the placeholder program) and tries to compare
// against a baseline. Without a baseline file it fails — surfacing the
// "no baseline yet" state rather than silently passing. Phase 1 fills
// in the real elephant/mice scenarios alongside natra's BPF dataplane.

package perf_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/cilium/ebpf"
	"github.com/cilium/ebpf/rlimit"
)

func bpfObjectPath(name string) string {
	_, thisFile, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(thisFile), "..", "..", "bpf", name)
}

func baselinePath(kernel string) string {
	_, thisFile, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(thisFile), "baselines", kernel+".json")
}

// kernelTag returns a coarse kernel identifier for baseline lookup. On lvh
// it's set via env (KERNEL=5.15 / 6.6 / bpf-next); local Mac dev runs
// against whatever kernel colima's VM provides, which we tag "local".
func kernelTag() string {
	if k := os.Getenv("KERNEL"); k != "" {
		return k
	}
	return "local"
}

type baseline struct {
	Kernel               string  `json:"kernel"`
	BPFProgRunNsPerOpMax float64 `json:"bpf_prog_run_ns_per_op_max"`
}

// loadBPFRunBaseline reads the kernel-specific perf baseline. Returns nil
// (with no error) if the file doesn't exist — the caller decides how to
// react. Decoding errors are propagated so a corrupt baseline isn't silently
// treated as "no baseline."
func loadBaseline(kernel string) (*baseline, error) {
	path := baselinePath(kernel)
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var b baseline
	if err := json.Unmarshal(data, &b); err != nil {
		return nil, err
	}
	return &b, nil
}

// TestBPFProgRunThroughput is the Phase 0 scenario: run BPF_PROG_RUN
// against the placeholder program for a fixed budget and report ns/op.
// Compares to the per-kernel baseline; fails if there's no baseline (so
// the absence is loud, not silent) or if we regress past the threshold.
func TestBPFProgRunThroughput(t *testing.T) {
	if err := rlimit.RemoveMemlock(); err != nil {
		t.Fatalf("remove memlock: %v", err)
	}

	spec, err := ebpf.LoadCollectionSpec(bpfObjectPath("placeholder.bpf.o"))
	if err != nil {
		t.Fatalf("load spec: %v", err)
	}
	coll, err := ebpf.NewCollection(spec)
	if err != nil {
		t.Fatalf("instantiate: %v", err)
	}
	t.Cleanup(coll.Close)

	prog, ok := coll.Programs["natra_placeholder"]
	if !ok {
		t.Fatalf("program not found")
	}

	pkt := make([]byte, 64)
	const iterations = 100_000

	start := time.Now()
	for i := 0; i < iterations; i++ {
		if _, _, err := prog.Test(pkt); err != nil {
			t.Fatalf("BPF_PROG_RUN i=%d: %v", i, err)
		}
	}
	elapsed := time.Since(start)
	nsPerOp := float64(elapsed.Nanoseconds()) / float64(iterations)

	t.Logf("BPF_PROG_RUN: %d iterations in %v (%.1f ns/op)", iterations, elapsed, nsPerOp)

	kernel := kernelTag()
	bl, err := loadBaseline(kernel)
	if err != nil {
		t.Fatalf("load baseline %s: %v", kernel, err)
	}
	if bl == nil {
		t.Fatalf("no baseline for kernel %q (expected at %s) — record one with `make perf-baseline KERNEL=%s`",
			kernel, baselinePath(kernel), kernel)
	}

	if nsPerOp > bl.BPFProgRunNsPerOpMax {
		t.Errorf("regression: %.1f ns/op > baseline %.1f ns/op (kernel=%s)",
			nsPerOp, bl.BPFProgRunNsPerOpMax, kernel)
	}
}

func TestScenarioOneElephant(t *testing.T) {
	t.Skip("Phase 1: 1 Gbps source rate-limited to 10 Mbps; assert both plugins converge near 10 Mbps; record per-packet BPF cost")
}

func TestScenarioThousandMice(t *testing.T) {
	t.Skip("Phase 1: 1000 short-lived TCP flows; record aggregate goodput, p99 connect time, BPF program CPU")
}

func TestScenarioMixed(t *testing.T) {
	t.Skip("Phase 1: 1 elephant + 100 mice for 60s — natra's hero scenario; assert mice goodput >= 80% line rate, vanilla mice <= 50% (the elephant starves them)")
}
