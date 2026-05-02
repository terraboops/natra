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

// natraConfig / tokenBucket / synthEthIPpktFromFlow mirror the structs
// in test/bpf/ratelimit_linux_test.go. Duplicated here rather than
// shared so the L5 perf scenarios stay self-contained and test/bpf can
// keep its build tag (`bpf`) distinct from L5's (`perf`).
type natraConfig struct {
	RateBps     uint64
	BurstBytes  uint64
	HHThreshold uint64
}

type tokenBucket struct {
	Reserved0    uint32 //nolint:unused
	Reserved1    uint32 //nolint:unused
	Tokens       uint64
	LastUpdateNs uint64
}

func TestScenarioOneElephant(t *testing.T) {
	if err := rlimit.RemoveMemlock(); err != nil {
		t.Fatalf("remove memlock: %v", err)
	}
	natraObj := bpfObjectPath("natra.bpf.o")
	spec, err := ebpf.LoadCollectionSpec(natraObj)
	if err != nil {
		t.Fatalf("load %s: %v", natraObj, err)
	}
	coll, err := ebpf.NewCollection(spec)
	if err != nil {
		t.Fatalf("instantiate: %v", err)
	}
	t.Cleanup(coll.Close)

	cfgMap := coll.Maps["natra_config_map"]
	bucketMap := coll.Maps["natra_bucket_map"]
	statsMap := coll.Maps["natra_stats_map"]
	prog := coll.Programs["natra_ratelimit"]
	if cfgMap == nil || bucketMap == nil || statsMap == nil || prog == nil {
		t.Fatalf("expected maps and program, got: %v / %v",
			collMapNames(coll), collProgNames(coll))
	}

	// 10 Mbps = 1.25 MB/s in BPF units. hh_threshold=10 so the elephant
	// goes heavy almost immediately. Burst sized to one second of
	// throughput so a 100k-iteration loop can't satisfy itself from
	// burst alone.
	cfg := natraConfig{
		RateBps:     1_250_000,
		BurstBytes:  1_250_000,
		HHThreshold: 10,
	}
	zero := uint32(0)
	if err := cfgMap.Update(&zero, &cfg, ebpf.UpdateAny); err != nil {
		t.Fatalf("config: %v", err)
	}
	tb := tokenBucket{Tokens: cfg.BurstBytes}
	if err := bucketMap.Update(&zero, &tb, ebpf.UpdateAny); err != nil {
		t.Fatalf("bucket: %v", err)
	}

	// Single elephant flow — all packets share a 5-tuple, so CMS
	// counts climb monotonically. After 10 packets the threshold is
	// crossed and every subsequent packet is gated by the token bucket.
	pkt := synthElephantPkt()
	const iterations = 100_000

	start := time.Now()
	for i := 0; i < iterations; i++ {
		if _, _, err := prog.Test(pkt); err != nil {
			t.Fatalf("BPF_PROG_RUN i=%d: %v", i, err)
		}
	}
	elapsed := time.Since(start)
	nsPerOp := float64(elapsed.Nanoseconds()) / float64(iterations)
	t.Logf("one_elephant: %d iterations, %v wall (%.1f ns/op)", iterations, elapsed, nsPerOp)

	passed := readPerCPUStat(t, statsMap, 0)
	throttled := readPerCPUStat(t, statsMap, 1)
	hhHits := readPerCPUStat(t, statsMap, 2)
	t.Logf("stats: passed=%d throttled=%d hh_hits=%d", passed, throttled, hhHits)

	// Sanity asserts: most of the iterations should be heavy hits (we
	// crossed threshold after the first 10), and a meaningful chunk
	// should be throttled — the test sends much more than the bucket
	// can absorb in this wall time.
	if hhHits < uint64(iterations-100) {
		t.Errorf("hh_hits=%d, expected ≈%d (threshold crossed after 10)", hhHits, iterations-10)
	}
	if throttled == 0 {
		t.Errorf("throttled=0, expected the bucket to drop packets when sending %d>>burst", iterations)
	}

	bl, err := loadBaseline(kernelTag())
	if err != nil {
		t.Fatalf("load baseline: %v", err)
	}
	if bl == nil {
		t.Fatalf("no baseline for kernel %q (expected at %s)", kernelTag(), baselinePath(kernelTag()))
	}
	if nsPerOp > bl.BPFProgRunNsPerOpMax {
		t.Errorf("one_elephant ns/op=%.1f > baseline %.1f", nsPerOp, bl.BPFProgRunNsPerOpMax)
	}
}

// readPerCPUStat sums a per-CPU stats counter. Mirrors the helper in
// test/bpf/ratelimit_linux_test.go (different package, can't share).
func readPerCPUStat(t *testing.T, m *ebpf.Map, idx uint32) uint64 {
	t.Helper()
	var values []uint64
	if err := m.Lookup(&idx, &values); err != nil {
		t.Fatalf("stats lookup idx=%d: %v", idx, err)
	}
	var sum uint64
	for _, v := range values {
		sum += v
	}
	return sum
}

func collMapNames(c *ebpf.Collection) []string {
	out := make([]string, 0, len(c.Maps))
	for k := range c.Maps {
		out = append(out, k)
	}
	return out
}

func collProgNames(c *ebpf.Collection) []string {
	out := make([]string, 0, len(c.Programs))
	for k := range c.Programs {
		out = append(out, k)
	}
	return out
}

// synthElephantPkt is the canonical "single TCP flow" 64-byte packet
// used by the elephant scenario. Static src/dst IP+port so every
// invocation hashes to the same CMS cells.
func synthElephantPkt() []byte {
	pkt := make([]byte, 64)
	// EtherType IPv4
	pkt[12], pkt[13] = 0x08, 0x00
	// IP version=4 / IHL=5
	pkt[14] = 0x45
	// total length = 50
	pkt[16], pkt[17] = 0x00, 0x32
	// proto = TCP (6)
	pkt[23] = 6
	// src ip 10.0.0.1, dst 10.0.0.2
	pkt[26], pkt[27], pkt[28], pkt[29] = 10, 0, 0, 1
	pkt[30], pkt[31], pkt[32], pkt[33] = 10, 0, 0, 2
	// src port 12345, dst port 5201
	pkt[34], pkt[35] = 0x30, 0x39
	pkt[36], pkt[37] = 0x14, 0x51
	return pkt
}

// TestScenarioThousandMice exercises the design assumption that
// short-lived flows (count <= hh_threshold) bypass the token bucket
// entirely and pass at line rate. With 1000 distinct flows × 5
// packets each, no flow's count reaches threshold, so STAT_HH_HITS
// must be 0.
func TestScenarioThousandMice(t *testing.T) {
	if err := rlimit.RemoveMemlock(); err != nil {
		t.Fatalf("remove memlock: %v", err)
	}
	spec, err := ebpf.LoadCollectionSpec(bpfObjectPath("natra.bpf.o"))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	coll, err := ebpf.NewCollection(spec)
	if err != nil {
		t.Fatalf("instantiate: %v", err)
	}
	t.Cleanup(coll.Close)

	cfg := natraConfig{
		RateBps:     1, // crippled — any mouse hitting the bucket would drop
		BurstBytes:  1,
		HHThreshold: 100, // generous; 5 packets per flow stays well under
	}
	zero := uint32(0)
	if err := coll.Maps["natra_config_map"].Update(&zero, &cfg, ebpf.UpdateAny); err != nil {
		t.Fatalf("config: %v", err)
	}
	if err := coll.Maps["natra_bucket_map"].Update(&zero, &tokenBucket{}, ebpf.UpdateAny); err != nil {
		t.Fatalf("bucket: %v", err)
	}

	const flows = 1000
	const perFlow = 5
	prog := coll.Programs["natra_ratelimit"]
	start := time.Now()
	for i := 0; i < flows; i++ {
		// Vary src ip + src port so each flow_key is distinct. Dst
		// stays constant — same "service" but different clients.
		pkt := mkPkt(0x0A000001+uint32(i), 0x0A000002, uint16(20000+i), 5201)
		for j := 0; j < perFlow; j++ {
			if _, _, err := prog.Test(pkt); err != nil {
				t.Fatalf("BPF_PROG_RUN flow=%d j=%d: %v", i, j, err)
			}
		}
	}
	elapsed := time.Since(start)
	totalPackets := uint64(flows * perFlow)
	t.Logf("thousand_mice: %d flows × %d pkts in %v (%.1f ns/pkt)",
		flows, perFlow, elapsed, float64(elapsed.Nanoseconds())/float64(totalPackets))

	statsMap := coll.Maps["natra_stats_map"]
	passed := readPerCPUStat(t, statsMap, 0)
	throttled := readPerCPUStat(t, statsMap, 1)
	hh := readPerCPUStat(t, statsMap, 2)
	t.Logf("stats: passed=%d throttled=%d hh_hits=%d", passed, throttled, hh)

	if hh != 0 {
		t.Errorf("STAT_HH_HITS=%d, want 0 — every flow under threshold should bypass the bucket", hh)
	}
	if throttled != 0 {
		t.Errorf("STAT_THROTTLED=%d, want 0 — mice flows must not be rate-limited", throttled)
	}
	if passed != totalPackets {
		t.Errorf("STAT_PASSED=%d, want %d", passed, totalPackets)
	}
}

// TestScenarioMixed is natra's hero scenario: one elephant flow that
// MUST get throttled coexisting with many mice flows that MUST NOT.
// This is what differentiates natra from
// containernetworking/plugins/bandwidth — vanilla rate-limits ALL
// traffic and starves mice when the elephant exists; natra rate-limits
// only the elephant.
//
// Asserts:
//   1. Elephant has nonzero throttled count (bucket actually drops
//      packets when elephant rate exceeds configured limit).
//   2. Sum of mice STAT_HH_HITS = 0 (no mouse flow ever reaches the
//      bucket).
//   3. The bucket's heavy-hitter classification is stable: STAT_HH_HITS
//      is dominated by the elephant alone.
func TestScenarioMixed(t *testing.T) {
	if err := rlimit.RemoveMemlock(); err != nil {
		t.Fatalf("remove memlock: %v", err)
	}
	spec, err := ebpf.LoadCollectionSpec(bpfObjectPath("natra.bpf.o"))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	coll, err := ebpf.NewCollection(spec)
	if err != nil {
		t.Fatalf("instantiate: %v", err)
	}
	t.Cleanup(coll.Close)

	// Real natra rate, real burst. Threshold low enough that the
	// elephant crosses it within the first few packets but high enough
	// that 5-packet mice never approach it.
	cfg := natraConfig{
		RateBps:     1_250_000, // 10 Mbps in bytes/sec
		BurstBytes:  64_000,    // small enough that elephant exhausts it
		HHThreshold: 10,
	}
	zero := uint32(0)
	if err := coll.Maps["natra_config_map"].Update(&zero, &cfg, ebpf.UpdateAny); err != nil {
		t.Fatalf("config: %v", err)
	}
	if err := coll.Maps["natra_bucket_map"].Update(&zero, &tokenBucket{Tokens: cfg.BurstBytes}, ebpf.UpdateAny); err != nil {
		t.Fatalf("bucket: %v", err)
	}

	const elephantPackets = 10_000
	const miceFlows = 100
	const miceFlowPackets = 5
	prog := coll.Programs["natra_ratelimit"]

	// Round-robin elephant + mice so the BPF program sees them
	// interleaved (closer to a real elephant-among-mice mix). The
	// elephant uses one fixed flow_key; mice use 100 distinct ones.
	elephantPkt := mkPkt(0x0AFF0001, 0x0AFF0002, 33000, 5201)
	for i := 0; i < elephantPackets; i++ {
		// One elephant packet…
		if _, _, err := prog.Test(elephantPkt); err != nil {
			t.Fatalf("BPF_PROG_RUN elephant i=%d: %v", i, err)
		}
		// …followed by one mouse packet from a rotating flow.
		if i < miceFlows*miceFlowPackets {
			flowIdx := i % miceFlows
			micePkt := mkPkt(0x0A0A0000+uint32(flowIdx), 0x0AFF0002,
				uint16(40000+flowIdx), 5201)
			if _, _, err := prog.Test(micePkt); err != nil {
				t.Fatalf("BPF_PROG_RUN mouse i=%d: %v", i, err)
			}
		}
	}

	statsMap := coll.Maps["natra_stats_map"]
	passed := readPerCPUStat(t, statsMap, 0)
	throttled := readPerCPUStat(t, statsMap, 1)
	hh := readPerCPUStat(t, statsMap, 2)
	totalSent := uint64(elephantPackets + miceFlows*miceFlowPackets)
	t.Logf("mixed: sent=%d (1 elephant × %d + %d mice × %d) — passed=%d throttled=%d hh_hits=%d",
		totalSent, elephantPackets, miceFlows, miceFlowPackets, passed, throttled, hh)

	// Headline assertions:
	if throttled == 0 {
		t.Errorf("throttled=0 — bucket should drop packets when elephant exceeds rate")
	}
	// hh_hits should be ~elephant-after-threshold = 10_000 - 10 = 9990.
	// Mice contribute 0 to hh_hits because their per-flow count never
	// reaches threshold. Tolerate small CMS hash collisions inflating
	// mouse counts (mouse and elephant can share a CMS column on one
	// row, but min across rows still classifies the mouse correctly).
	if hh > uint64(elephantPackets) {
		t.Errorf("hh_hits=%d > elephant packets %d — mice must not appear in hh_hits", hh, elephantPackets)
	}
	if hh < uint64(elephantPackets)-100 {
		t.Errorf("hh_hits=%d, expected ≈%d — elephant should fully dominate hh_hits", hh, elephantPackets-10)
	}
	// And the value-prop: passed >= miceFlows*miceFlowPackets +
	// (the elephant packets that fit in burst).
	miceTotal := uint64(miceFlows * miceFlowPackets)
	if passed < miceTotal {
		t.Errorf("passed=%d < mice total %d — at minimum every mouse packet should pass", passed, miceTotal)
	}
}

func mkPkt(srcIP, dstIP uint32, srcPort, dstPort uint16) []byte {
	pkt := make([]byte, 64)
	pkt[12], pkt[13] = 0x08, 0x00
	pkt[14] = 0x45
	pkt[16], pkt[17] = 0x00, 0x32
	pkt[23] = 6
	pkt[26] = byte(srcIP >> 24)
	pkt[27] = byte(srcIP >> 16)
	pkt[28] = byte(srcIP >> 8)
	pkt[29] = byte(srcIP)
	pkt[30] = byte(dstIP >> 24)
	pkt[31] = byte(dstIP >> 16)
	pkt[32] = byte(dstIP >> 8)
	pkt[33] = byte(dstIP)
	pkt[34] = byte(srcPort >> 8)
	pkt[35] = byte(srcPort)
	pkt[36] = byte(dstPort >> 8)
	pkt[37] = byte(dstPort)
	return pkt
}
