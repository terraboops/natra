//go:build linux && bpf

// Incremental BPF tests for natra.bpf.o. Exercises the token-bucket
// behavior at the BPF level — loading the program, populating config
// and bucket maps, running BPF_PROG_RUN with synthetic packets, and
// asserting the verdicts and stats. These tests catch BPF-level
// regressions independent of the Go loader and CNI integration; they
// are the first thing to fail when the BPF program itself breaks.

package bpf_test

import (
	"encoding/binary"
	"testing"

	"github.com/cilium/ebpf"
	"github.com/cilium/ebpf/rlimit"
)

// natraConfig mirrors `struct natra_config` in bpf/natra.bpf.c.
type natraConfig struct {
	RateBps     uint64
	BurstBytes  uint64
	HHThreshold uint64
}

// tokenBucket mirrors `struct token_bucket`. The first field is the
// bpf_spin_lock; cilium/ebpf zeroes it for us when we Update().
type tokenBucket struct {
	Lock         struct{ _ uint32 } // 4-byte spin lock placeholder
	_            uint32             // 4-byte alignment pad before 8-byte fields
	Tokens       uint64
	LastUpdateNs uint64
}

const (
	statPassed    = 0
	statThrottled = 1
	statHHHits    = 2
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
// future flow-parsing code (CMS step) won't reject it but minimal for
// step 1's "ignore content, just count length" token bucket.
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
		HHThreshold: 0,               // unused in step 1
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
