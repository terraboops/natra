//go:build linux && perf

package perf_test

import (
	"testing"

	"github.com/cilium/ebpf"
	"github.com/cilium/ebpf/rlimit"

	"github.com/terraboops/natra/pkg/bpf"
)

// BenchmarkBPF times natra.bpf.o per packet on its three hot paths,
// both directions. The time comes from the kernel's BPF_PROG_TEST_RUN
// repeat loop (no syscall per packet), reported as bpf-ns/op next to
// Go's wall-clock ns/op. CI compares these against main with benchstat
// and fails on any significant regression (scripts/bench-gate.sh).
func BenchmarkBPF(b *testing.B) {
	if err := rlimit.RemoveMemlock(); err != nil {
		b.Fatalf("remove memlock: %v", err)
	}
	cases := []struct {
		name string
		cfg  natraConfig
	}{
		// Threshold far above anything the run sends: every packet is
		// a mouse and takes the fast pass.
		{"mouse", natraConfig{RateBps: 1_250_000_000, BurstBytes: 1 << 30, HHThreshold: 1 << 40}},
		// Threshold 0 makes the flow heavy on the first packet; the
		// huge rate keeps the bucket from ever running dry.
		{"elephant", natraConfig{RateBps: 1 << 40, BurstBytes: 1 << 40}},
		// Heavy and starved: every packet takes the throttle path.
		{"throttled", natraConfig{RateBps: 1, BurstBytes: 1}},
	}
	pkt := mkPkt(0x0A000001, 0x0A000002, 40000, 5201)
	for _, dir := range []bpf.Direction{bpf.DirectionIngress, bpf.DirectionEgress} {
		for _, c := range cases {
			b.Run(dir.String()+"/"+c.name, func(b *testing.B) {
				spec, err := ebpf.LoadCollectionSpec(bpfObjectPath("natra.bpf.o"))
				if err != nil {
					b.Fatalf("load: %v", err)
				}
				coll, err := ebpf.NewCollection(spec)
				if err != nil {
					b.Fatalf("instantiate: %v", err)
				}
				defer coll.Close()
				key := uint32(dir)
				if err := coll.Maps["natra_config_map"].Update(&key, &c.cfg, ebpf.UpdateAny); err != nil {
					b.Fatalf("config: %v", err)
				}
				if err := coll.Maps["natra_bucket_map"].Update(&key, &tokenBucket{Tokens: c.cfg.BurstBytes}, ebpf.UpdateAny); err != nil {
					b.Fatalf("bucket: %v", err)
				}
				prog := coll.Programs[natraProgFor(dir)]
				b.ResetTimer()
				_, perRun, err := prog.Benchmark(pkt, b.N, b.ResetTimer)
				if err != nil {
					b.Fatalf("benchmark: %v", err)
				}
				b.ReportMetric(float64(perRun.Nanoseconds()), "bpf-ns/op")
			})
		}
	}
}
