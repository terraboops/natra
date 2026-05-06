//go:build linux && bpf

// BPF-level tests for natra.bpf.o: load the program, populate config
// and bucket maps, run BPF_PROG_RUN with synthetic packets, assert the
// verdicts and stats. Catches BPF-level regressions independent of the
// Go loader and CNI integration.

package bpf_test

import (
	"encoding/binary"
	"testing"

	"github.com/cilium/ebpf"
	"github.com/cilium/ebpf/rlimit"

	"github.com/terraboops/natra/pkg/bpf"
)

// Tests use the canonical struct shapes from pkg/bpf so the BPF ABI
// has one definition, not several drifting copies across test files.
type (
	natraConfig = bpf.Config
	tokenBucket = bpf.TokenBucket
)

const (
	statPassed    = bpf.StatPassed
	statThrottled = bpf.StatThrottled
	statHHHits    = bpf.StatHHHits
)

// loadNatraColl loads and instantiates bpf/natra.bpf.o. Returns the
// collection (caller closes), the config map, the bucket map, and the
// stats map for assertions.
func loadNatraColl(t *testing.T) (*ebpf.Collection, *ebpf.Map, *ebpf.Map, *ebpf.Map, *ebpf.Program) {
	t.Helper()
	if err := rlimit.RemoveMemlock(); err != nil {
		t.Fatalf("remove memlock: %v", err)
	}
	spec, err := ebpf.LoadCollectionSpec(bpfObjectPath("natra.bpf.o"))
	if err != nil {
		t.Fatalf("load spec: %v", err)
	}
	coll, err := ebpf.NewCollection(spec)
	if err != nil {
		t.Fatalf("instantiate collection: %v", err)
	}
	t.Cleanup(coll.Close)

	cfgMap, ok := coll.Maps["natra_config_map"]
	if !ok {
		t.Fatalf("config map missing; have: %v", mapNames(coll.Maps))
	}
	bucketMap, ok := coll.Maps["natra_bucket_map"]
	if !ok {
		t.Fatalf("bucket map missing")
	}
	statsMap, ok := coll.Maps["natra_stats_map"]
	if !ok {
		t.Fatalf("stats map missing")
	}
	prog, ok := coll.Programs["natra_ratelimit"]
	if !ok {
		t.Fatalf("program missing")
	}
	return coll, cfgMap, bucketMap, statsMap, prog
}

func mapNames(m map[string]*ebpf.Map) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// readPerCPUStat sums a per-CPU stats counter across all CPUs.
func readPerCPUStat(t *testing.T, statsMap *ebpf.Map, idx uint32) uint64 {
	t.Helper()
	var values []uint64
	if err := statsMap.Lookup(&idx, &values); err != nil {
		t.Fatalf("stats lookup idx=%d: %v", idx, err)
	}
	var sum uint64
	for _, v := range values {
		sum += v
	}
	return sum
}

// synthEthIPpkt returns a 64-byte ETH+IPv4 packet, valid enough that
// the BPF flow parser accepts it. The 5-tuple fields aren't filled in;
// callers that care about flow identity use synthEthIPpktFromFlow.
func synthEthIPpkt() []byte {
	pkt := make([]byte, 64)
	// EtherType IPv4
	binary.BigEndian.PutUint16(pkt[12:14], 0x0800)
	// IP version=4, IHL=5 → 0x45
	pkt[14] = 0x45
	// IP total length = 50 (rest of pkt after eth)
	binary.BigEndian.PutUint16(pkt[16:18], 50)
	// Protocol = TCP (6)
	pkt[23] = 6
	return pkt
}

func TestNatraNoConfigPasses(t *testing.T) {
	_, _, _, statsMap, prog := loadNatraColl(t)
	pkt := synthEthIPpkt()

	ret, _, err := prog.Test(pkt)
	if err != nil {
		t.Fatalf("BPF_PROG_RUN: %v", err)
	}
	// rate_bps == 0 → fail-open path, packet passes.
	if ret != 0 { // TC_ACT_OK
		t.Errorf("ret = %d, want 0 (TC_ACT_OK)", ret)
	}
	if got := readPerCPUStat(t, statsMap, statPassed); got != 1 {
		t.Errorf("STAT_PASSED = %d, want 1", got)
	}
}

func TestNatraTokenBucketUnderRate(t *testing.T) {
	_, cfgMap, bucketMap, statsMap, prog := loadNatraColl(t)

	// 100 Mbps rate, large burst so a single 64-byte packet always fits.
	cfg := natraConfig{
		RateBps:     100_000_000 / 8, // 12.5 MB/s
		BurstBytes:  1 << 20,         // 1 MB
		HHThreshold: 0,               // not exercised here
	}
	zero := uint32(0)
	if err := cfgMap.Update(&zero, &cfg, ebpf.UpdateAny); err != nil {
		t.Fatalf("config update: %v", err)
	}
	// Pre-fill bucket to burst so the first packet has tokens.
	tb := tokenBucket{Tokens: cfg.BurstBytes}
	if err := bucketMap.Update(&zero, &tb, ebpf.UpdateAny); err != nil {
		t.Fatalf("bucket update: %v", err)
	}

	pkt := synthEthIPpkt()
	for i := 0; i < 100; i++ {
		ret, _, err := prog.Test(pkt)
		if err != nil {
			t.Fatalf("BPF_PROG_RUN i=%d: %v", i, err)
		}
		if ret != 0 {
			t.Fatalf("packet %d throttled (ret=%d), expected to pass under burst capacity", i, ret)
		}
	}
	if got := readPerCPUStat(t, statsMap, statPassed); got != 100 {
		t.Errorf("STAT_PASSED = %d, want 100", got)
	}
	if got := readPerCPUStat(t, statsMap, statThrottled); got != 0 {
		t.Errorf("STAT_THROTTLED = %d, want 0", got)
	}
}

// synthEthIPpktFromFlow returns a 64-byte ETH+IPv4 packet whose 5-tuple
// hashes to a different CMS bucket than synthEthIPpkt's. Used to drive
// many distinct flows from a single test goroutine.
func synthEthIPpktFromFlow(srcIP, dstIP uint32, srcPort, dstPort uint16) []byte {
	pkt := synthEthIPpkt()
	binary.BigEndian.PutUint32(pkt[26:30], srcIP)
	binary.BigEndian.PutUint32(pkt[30:34], dstIP)
	// Packet is 64 bytes: ETH(14) + IP(20) = 34, with 30 trailing bytes.
	// TCP header would start at offset 34 if proto==6; we set proto=6 and
	// the L4 ports occupy 34..38.
	binary.BigEndian.PutUint16(pkt[34:36], srcPort)
	binary.BigEndian.PutUint16(pkt[36:38], dstPort)
	return pkt
}

func TestNatraCMSMiceFlowsBypassTokenBucket(t *testing.T) {
	_, cfgMap, bucketMap, statsMap, prog := loadNatraColl(t)

	// Threshold = 100 means the first 100 packets of any single flow
	// pass for free; only the 101st onward go through the token bucket.
	cfg := natraConfig{
		RateBps:     1, // crippled rate — if any mouse traffic hits the
		BurstBytes:  1, // bucket, it would be throttled. None should.
		HHThreshold: 100,
	}
	zero := uint32(0)
	if err := cfgMap.Update(&zero, &cfg, ebpf.UpdateAny); err != nil {
		t.Fatalf("config: %v", err)
	}
	tb := tokenBucket{}
	if err := bucketMap.Update(&zero, &tb, ebpf.UpdateAny); err != nil {
		t.Fatalf("bucket: %v", err)
	}

	const flows = 50
	const perFlow = 5 // far below the 100-packet threshold
	for i := 0; i < flows; i++ {
		pkt := synthEthIPpktFromFlow(0x0A000001+uint32(i), 0x0A000002, 12345, 5201)
		for j := 0; j < perFlow; j++ {
			ret, _, err := prog.Test(pkt)
			if err != nil {
				t.Fatalf("BPF_PROG_RUN flow=%d j=%d: %v", i, j, err)
			}
			if ret != 0 {
				t.Fatalf("flow=%d j=%d throttled (ret=%d) — mouse flows must pass", i, j, ret)
			}
		}
	}
	if got := readPerCPUStat(t, statsMap, statHHHits); got != 0 {
		t.Errorf("STAT_HH_HITS = %d, want 0 — mice should never reach the bucket", got)
	}
	if got := readPerCPUStat(t, statsMap, statThrottled); got != 0 {
		t.Errorf("STAT_THROTTLED = %d, want 0", got)
	}
	if got := readPerCPUStat(t, statsMap, statPassed); got != flows*perFlow {
		t.Errorf("STAT_PASSED = %d, want %d", got, flows*perFlow)
	}
}

func TestNatraCMSElephantHitsBucket(t *testing.T) {
	_, cfgMap, bucketMap, statsMap, prog := loadNatraColl(t)

	cfg := natraConfig{
		RateBps:     1,
		BurstBytes:  64, // exactly one packet's worth
		HHThreshold: 10, // 11th packet onward is "heavy"
	}
	zero := uint32(0)
	if err := cfgMap.Update(&zero, &cfg, ebpf.UpdateAny); err != nil {
		t.Fatalf("config: %v", err)
	}
	tb := tokenBucket{Tokens: 64}
	if err := bucketMap.Update(&zero, &tb, ebpf.UpdateAny); err != nil {
		t.Fatalf("bucket: %v", err)
	}

	pkt := synthEthIPpktFromFlow(0x0A000001, 0x0A000002, 12345, 5201)
	// First 10 packets: count goes 1..10, none > threshold(10) → mice.
	for i := 0; i < 10; i++ {
		ret, _, err := prog.Test(pkt)
		if err != nil {
			t.Fatalf("packet %d: %v", i, err)
		}
		if ret != 0 {
			t.Fatalf("packet %d throttled at count<=threshold (ret=%d)", i, ret)
		}
	}
	if got := readPerCPUStat(t, statsMap, statHHHits); got != 0 {
		t.Errorf("after first 10 packets STAT_HH_HITS=%d, want 0", got)
	}

	// 11th packet (count=11 > 10): heavy. Burst=64 admits this packet,
	// so it's logged as a heavy-hitter PASS.
	ret, _, err := prog.Test(pkt)
	if err != nil {
		t.Fatalf("packet 11: %v", err)
	}
	if ret != 0 {
		t.Errorf("11th packet ret=%d, want 0 (TC_ACT_OK; bucket has 64 tokens)", ret)
	}

	// 12th packet (count=12, still heavy): bucket empty (one packet
	// drained it), rate is 1 byte/sec so micro-second elapsed adds
	// nothing. Should throttle.
	ret, _, err = prog.Test(pkt)
	if err != nil {
		t.Fatalf("packet 12: %v", err)
	}
	if ret != 2 {
		t.Errorf("12th packet ret=%d, want 2 (TC_ACT_SHOT; bucket empty)", ret)
	}

	if got := readPerCPUStat(t, statsMap, statHHHits); got != 2 {
		t.Errorf("STAT_HH_HITS=%d, want 2 (packets 11 and 12)", got)
	}
	if got := readPerCPUStat(t, statsMap, statThrottled); got != 1 {
		t.Errorf("STAT_THROTTLED=%d, want 1", got)
	}
}

func TestNatraTokenBucketThrottlesOnceBurstSpent(t *testing.T) {
	_, cfgMap, bucketMap, statsMap, prog := loadNatraColl(t)

	// Tiny rate so back-to-back BPF_PROG_RUN calls don't refill
	// meaningfully between iterations. Burst sized to admit exactly
	// one packet, so the second call must be throttled.
	cfg := natraConfig{
		RateBps:    1,  // effectively zero refill in the test window
		BurstBytes: 64, // exactly one synthetic packet
	}
	zero := uint32(0)
	if err := cfgMap.Update(&zero, &cfg, ebpf.UpdateAny); err != nil {
		t.Fatalf("config update: %v", err)
	}
	tb := tokenBucket{Tokens: 64}
	if err := bucketMap.Update(&zero, &tb, ebpf.UpdateAny); err != nil {
		t.Fatalf("bucket update: %v", err)
	}

	pkt := synthEthIPpkt()

	// First packet drains the bucket and updates last_update_ns to
	// "now", anchoring the refill clock.
	ret, _, err := prog.Test(pkt)
	if err != nil {
		t.Fatalf("BPF_PROG_RUN #1: %v", err)
	}
	if ret != 0 {
		t.Fatalf("first packet ret=%d, want 0 (TC_ACT_OK) — bucket should admit one full packet", ret)
	}

	// Second packet: bucket is empty, rate is 1 byte/sec, microseconds
	// since the first call → no refill → must throttle.
	ret, _, err = prog.Test(pkt)
	if err != nil {
		t.Fatalf("BPF_PROG_RUN #2: %v", err)
	}
	if ret != 2 { // TC_ACT_SHOT
		t.Errorf("second packet ret=%d, want 2 (TC_ACT_SHOT) — bucket should be empty", ret)
	}

	if got := readPerCPUStat(t, statsMap, statPassed); got != 1 {
		t.Errorf("STAT_PASSED = %d, want 1", got)
	}
	if got := readPerCPUStat(t, statsMap, statThrottled); got != 1 {
		t.Errorf("STAT_THROTTLED = %d, want 1", got)
	}
}
