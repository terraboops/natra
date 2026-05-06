// Package scenarios holds shared types for perf-comparison runs. The
// actual perf scenarios live inline in perf_linux_test.go (using
// BPF_PROG_RUN), so this package is currently unused by tests; it
// stays as the place to put a Result/Config schema if/when we add
// real-veth scenarios that need richer outputs.
package scenarios

import "time"

// Result is what each scenario emits. perf_linux_test.go diffs these
// against baselines/<kernel>.json for regression detection.
type Result struct {
	Scenario              string  `json:"scenario"`
	Plugin                string  `json:"plugin"` // "natra" or "vanilla"
	Kernel                string  `json:"kernel"`
	ElephantThroughputBps int64   `json:"elephant_throughput_bps"`
	MiceGoodputBps        int64   `json:"mice_goodput_bps"`
	BPFRunNanosPerPacket  float64 `json:"bpf_run_ns_per_packet"`
	P99ConnectMillis      float64 `json:"p99_connect_ms"`
	DurationSeconds       int     `json:"duration_seconds"`
}

// Config controls how a scenario runs. Defaults are tuned to fit a
// CI-runtime budget while still producing stable numbers.
type Config struct {
	Duration   time.Duration
	Seed       int64
	TargetBps  int64 // bandwidth annotation under test (per pod)
	MiceCount  int   // number of concurrent mice flows in mixed scenario
	ElephantOn bool  // include the elephant flow
}

// DefaultConfig returns the canonical run shape used by CI.
func DefaultConfig() Config {
	return Config{
		Duration:   30 * time.Second,
		Seed:       1,
		TargetBps:  10_000_000, // 10 Mbps
		MiceCount:  100,
		ElephantOn: true,
	}
}
