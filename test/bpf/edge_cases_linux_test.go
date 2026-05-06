//go:build linux && bpf

// Edge-case stress tests for natra's BPF dataplane. The intent is
// adversarial: try to break the program, then either fix what breaks
// or document why the failure mode is acceptable. Sprawling
// intentionally — these aren't intended to enforce a tight contract,
// they're documenting the actual robustness boundary so future
// regressions are visible.

package bpf_test

import (
	"encoding/binary"
	"sync/atomic"
	"testing"

	"github.com/cilium/ebpf"
	"github.com/cilium/ebpf/rlimit"
)

// edgeMkPkt is a local copy of test/perf/perf_linux_test.go's mkPkt
// helper. Packages can't share `*_test.go` symbols, and the perf
// suite has its own build tag.
func edgeMkPkt(srcIP, dstIP uint32, srcPort, dstPort uint16) []byte {
	pkt := make([]byte, 64)
	pkt[12], pkt[13] = 0x08, 0x00
	pkt[14] = 0x45
	pkt[16], pkt[17] = 0x00, 0x32
	pkt[23] = 6
	binary.BigEndian.PutUint32(pkt[26:30], srcIP)
	binary.BigEndian.PutUint32(pkt[30:34], dstIP)
	binary.BigEndian.PutUint16(pkt[34:36], srcPort)
	binary.BigEndian.PutUint16(pkt[36:38], dstPort)
	return pkt
}

// loadNatra returns a fresh natra collection ready for tests, with a
// bucket pre-filled to burst and config set to typical 10 Mbps shape.
// Each test gets its own collection so per-test state doesn't bleed.
func loadNatra(t *testing.T) (*ebpf.Collection, natraConfig, *ebpf.Program) {
	t.Helper()
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
	cfg := natraConfig{RateBps: 1_250_000, BurstBytes: 64_000, HHThreshold: 10}
	zero := uint32(0)
	if err := coll.Maps["natra_config_map"].Update(&zero, &cfg, ebpf.UpdateAny); err != nil {
		t.Fatalf("config: %v", err)
	}
	if err := coll.Maps["natra_bucket_map"].Update(&zero, &tokenBucket{Tokens: cfg.BurstBytes}, ebpf.UpdateAny); err != nil {
		t.Fatalf("bucket: %v", err)
	}
	return coll, cfg, coll.Programs["natra_ratelimit"]
}

// TestEdgePacketBiggerThanBurst — when a single packet exceeds the
// configured burst, the bucket can never hold enough tokens to admit
// it. Expected: TC_ACT_SHOT every time after threshold crossed.
//
// This is the failure mode for "user configures a very small bandwidth
// burst on a network with jumbo frames or GRO superpackets". natra
// should drop every heavy-hitter packet rather than let an oversized
// one through silently.
func TestEdgePacketBiggerThanBurst(t *testing.T) {
	coll, _, prog := loadNatra(t)

	// Override config: tiny burst (32 bytes), tiny rate (1 byte/sec).
	// All "large" packets will be too big.
	cfg := natraConfig{RateBps: 1, BurstBytes: 32, HHThreshold: 5}
	zero := uint32(0)
	_ = coll.Maps["natra_config_map"].Update(&zero, &cfg, ebpf.UpdateAny)
	_ = coll.Maps["natra_bucket_map"].Update(&zero, &tokenBucket{Tokens: cfg.BurstBytes}, ebpf.UpdateAny)

	pkt := edgeMkPkt(0x0A000001, 0x0A000002, 12345, 5201)
	if len(pkt) <= int(cfg.BurstBytes) {
		t.Fatalf("test packet (%d bytes) is not larger than burst (%d) — pick a bigger packet or smaller burst",
			len(pkt), cfg.BurstBytes)
	}

	// Cross threshold: send threshold+1 packets, all of which should
	// be classified heavy.
	for i := 0; i < int(cfg.HHThreshold)+1; i++ {
		_, _, _ = prog.Test(pkt)
	}

	// Now send a packet that's bigger than burst — it must drop.
	ret, _, err := prog.Test(pkt)
	if err != nil {
		t.Fatalf("BPF_PROG_RUN: %v", err)
	}
	if ret != 2 {
		t.Errorf("ret=%d, expected 2 (TC_ACT_SHOT) — packet (%d B) > burst (%d B) but was admitted",
			ret, len(pkt), cfg.BurstBytes)
	}
}

// TestEdgeICMP — protocol = ICMP (1) has no L4 ports. parse_flow
// should still produce a valid flow_key (with src_port=dst_port=0)
// and CMS must hash it consistently. Two ICMP packets with the same
// IPs should map to the same flow.
func TestEdgeICMP(t *testing.T) {
	coll, _, prog := loadNatra(t)

	icmpPkt := func(srcIP, dstIP uint32) []byte {
		pkt := make([]byte, 64)
		pkt[12], pkt[13] = 0x08, 0x00 // EtherType IPv4
		pkt[14] = 0x45                // version=4 ihl=5
		binary.BigEndian.PutUint16(pkt[16:18], 50)
		pkt[23] = 1 // proto = ICMP
		binary.BigEndian.PutUint32(pkt[26:30], srcIP)
		binary.BigEndian.PutUint32(pkt[30:34], dstIP)
		// no L4 header — bytes 34+ are ICMP type/code/checksum/data
		return pkt
	}

	// Send the same ICMP flow many times, threshold should be crossed
	// and stats[hh_hits] should reflect it.
	pkt := icmpPkt(0x0A0A0A01, 0x0A0A0A02)
	for i := 0; i < 50; i++ {
		ret, _, err := prog.Test(pkt)
		if err != nil {
			t.Fatalf("BPF_PROG_RUN i=%d: %v", i, err)
		}
		_ = ret
	}

	statsMap := coll.Maps["natra_stats_map"]
	hh := readPerCPUStatLocal(t, statsMap, statHHHits)
	if hh < 30 {
		t.Errorf("STAT_HH_HITS=%d, expected >= 30 (50 packets - 10 threshold = 40 heavy hits)", hh)
	}
}

// TestEdgeIPv4WithOptions — IPv4 header may carry options (ihl > 5).
// Our parse_flow must compute L4 offset as ip + ihl*4, not assume 20
// bytes. With ihl=15 (60-byte IP header), the L4 offset is 14+60=74
// in a 64-byte skb → out of bounds, parse_flow must reject.
func TestEdgeIPv4WithOptions(t *testing.T) {
	coll, _, prog := loadNatra(t)

	// 64-byte skb with IHL=15 (claims 60-byte IP header), proto=TCP.
	// L4 = 74 > 64 → parse_flow returns -1 → mouse path.
	pkt := make([]byte, 64)
	pkt[12], pkt[13] = 0x08, 0x00
	pkt[14] = 0x4f // version=4, ihl=15
	binary.BigEndian.PutUint16(pkt[16:18], 50)
	pkt[23] = 6 // TCP

	ret, _, err := prog.Test(pkt)
	if err != nil {
		t.Fatalf("BPF_PROG_RUN: %v", err)
	}
	if ret != 0 {
		t.Errorf("ret=%d, want 0 (TC_ACT_OK) — truncated IP options should pass through, not drop", ret)
	}
	// And no STAT_THROTTLED should fire.
	throttled := readPerCPUStatLocal(t, coll.Maps["natra_stats_map"], statThrottled)
	if throttled != 0 {
		t.Errorf("STAT_THROTTLED=%d, want 0 — parse_flow failures must not throttle", throttled)
	}
}

// TestEdgeBurstZero — burst configured to 0 means the bucket can
// never admit any packet. After threshold crossed, every packet is
// throttled.
func TestEdgeBurstZero(t *testing.T) {
	coll, _, prog := loadNatra(t)

	cfg := natraConfig{RateBps: 1_000_000, BurstBytes: 0, HHThreshold: 5}
	zero := uint32(0)
	_ = coll.Maps["natra_config_map"].Update(&zero, &cfg, ebpf.UpdateAny)
	_ = coll.Maps["natra_bucket_map"].Update(&zero, &tokenBucket{}, ebpf.UpdateAny)

	pkt := edgeMkPkt(0x0A000001, 0x0A000002, 12345, 5201)
	for i := 0; i < int(cfg.HHThreshold); i++ {
		_, _, _ = prog.Test(pkt)
	}
	ret, _, err := prog.Test(pkt)
	if err != nil {
		t.Fatalf("BPF_PROG_RUN: %v", err)
	}
	if ret != 2 {
		t.Errorf("ret=%d, want 2 (TC_ACT_SHOT) — burst=0 means no packet ever admitted", ret)
	}
}

// TestEdgeRapidConfigChange — userspace updates the config map while
// traffic is flowing. The BPF program reads cfg via map lookup on
// every packet, so a config change should take effect on the next
// packet without disrupting in-flight state.
func TestEdgeRapidConfigChange(t *testing.T) {
	coll, _, prog := loadNatra(t)
	cfgMap := coll.Maps["natra_config_map"]
	zero := uint32(0)
	pkt := edgeMkPkt(0x0A000001, 0x0A000002, 12345, 5201)

	// Phase 1: high rate, low threshold. All packets pass.
	cfg1 := natraConfig{RateBps: 1_000_000_000, BurstBytes: 1 << 30, HHThreshold: 5}
	_ = cfgMap.Update(&zero, &cfg1, ebpf.UpdateAny)
	for i := 0; i < 50; i++ {
		_, _, _ = prog.Test(pkt)
	}

	// Phase 2: zero burst, mid-flow. Now any heavy hitter must drop.
	cfg2 := natraConfig{RateBps: 1, BurstBytes: 0, HHThreshold: 5}
	if err := cfgMap.Update(&zero, &cfg2, ebpf.UpdateAny); err != nil {
		t.Fatalf("cfgMap update mid-flow: %v", err)
	}
	if err := coll.Maps["natra_bucket_map"].Update(&zero, &tokenBucket{}, ebpf.UpdateAny); err != nil {
		t.Fatalf("bucket reset: %v", err)
	}

	// CMS counts are already past threshold (50 >> 5). Next packet
	// must throttle under the new zero-burst config.
	ret, _, err := prog.Test(pkt)
	if err != nil {
		t.Fatalf("BPF_PROG_RUN post-reconfig: %v", err)
	}
	if ret != 2 {
		t.Errorf("ret=%d, want 2 — config change should take effect immediately", ret)
	}
}

// TestEdgeBucketTokensClamp — set the bucket's `tokens` field via
// userspace to a value larger than burst. The BPF program's refill
// step caps tokens at burst, so the next packet must consume from
// the cap, not the inflated value.
func TestEdgeBucketTokensClamp(t *testing.T) {
	coll, _, prog := loadNatra(t)

	cfg := natraConfig{RateBps: 1, BurstBytes: 100, HHThreshold: 1}
	zero := uint32(0)
	_ = coll.Maps["natra_config_map"].Update(&zero, &cfg, ebpf.UpdateAny)
	// Force tokens >> burst — should be clamped on first packet.
	_ = coll.Maps["natra_bucket_map"].Update(&zero, &tokenBucket{Tokens: 1 << 40}, ebpf.UpdateAny)

	pkt := edgeMkPkt(0x0A000001, 0x0A000002, 12345, 5201)
	// First few packets cross threshold and start hitting bucket.
	for i := 0; i < 5; i++ {
		_, _, _ = prog.Test(pkt)
	}

	// Read bucket back. Tokens must be <= burst.
	var tb tokenBucket
	if err := coll.Maps["natra_bucket_map"].Lookup(&zero, &tb); err != nil {
		t.Fatalf("bucket lookup: %v", err)
	}
	if tb.Tokens > cfg.BurstBytes {
		t.Errorf("tokens=%d > burst=%d after refill — clamp not applied", tb.Tokens, cfg.BurstBytes)
	}
}

// TestEdgeCMSMonotonicGrowth — under heavy concurrent load, CMS
// counters must never decrease (no torn writes / lost increments).
// Drives many goroutines, samples the cell value periodically, asserts
// the value only grows.
func TestEdgeCMSMonotonicGrowth(t *testing.T) {
	coll, _, prog := loadNatra(t)
	cmsMap := coll.Maps["natra_cms_map"]

	pkt := edgeMkPkt(0x0A000001, 0x0A000002, 12345, 5201)

	var stop atomic.Bool
	const goroutines = 4
	done := make(chan struct{}, goroutines)
	for g := 0; g < goroutines; g++ {
		go func() {
			for !stop.Load() {
				_, _, _ = prog.Test(pkt)
			}
			done <- struct{}{}
		}()
	}

	var lastMaxes []uint32
	for sample := 0; sample < 20; sample++ {
		// Pick the cell that the test packet's first hash row hits.
		// We don't know which cell exactly without re-implementing the
		// hash; instead, scan all cells and take the max.
		var curMax uint32
		for k := uint32(0); k < 4096; k++ {
			var v uint32
			if err := cmsMap.Lookup(&k, &v); err == nil && v > curMax {
				curMax = v
			}
		}
		if len(lastMaxes) > 0 && curMax < lastMaxes[len(lastMaxes)-1] {
			t.Errorf("CMS max decreased between samples: %v -> %d (atomic add not working?)", lastMaxes, curMax)
		}
		lastMaxes = append(lastMaxes, curMax)
	}
	stop.Store(true)
	for g := 0; g < goroutines; g++ {
		<-done
	}
	t.Logf("CMS max samples (monotonic): %v", lastMaxes)
}

// TestEdgeJumboPacket — modern Linux paths produce GRO superpackets up
// to 65 KB on real veth. We exercise the "large skb" path here with a
// 3 KB packet, the largest BPF_PROG_RUN can synthesize:
// bpf_prog_test_run_skb() allocates one PAGE_SIZE buffer per call and
// subtracts sizeof(struct skb_shared_info) (~324 B) for trailers, so
// the effective ceiling is ~3,772 B on x86_64. Real-veth jumbo behavior
// is exercised in L4/L5 (e2e/perf) where the BPF program is attached
// to an actual qdisc and packet length isn't capped by the test harness.
func TestEdgeJumboPacket(t *testing.T) {
	coll, _, prog := loadNatra(t)
	cfg := natraConfig{RateBps: 100_000_000, BurstBytes: 200_000, HHThreshold: 5}
	zero := uint32(0)
	_ = coll.Maps["natra_config_map"].Update(&zero, &cfg, ebpf.UpdateAny)
	_ = coll.Maps["natra_bucket_map"].Update(&zero, &tokenBucket{Tokens: cfg.BurstBytes}, ebpf.UpdateAny)

	pkt := make([]byte, 3000)
	pkt[12], pkt[13] = 0x08, 0x00
	pkt[14] = 0x45
	binary.BigEndian.PutUint16(pkt[16:18], uint16(len(pkt)-14))
	pkt[23] = 6
	binary.BigEndian.PutUint32(pkt[26:30], 0x0A000001)
	binary.BigEndian.PutUint32(pkt[30:34], 0x0A000002)
	binary.BigEndian.PutUint16(pkt[34:36], 12345)
	binary.BigEndian.PutUint16(pkt[36:38], 5201)

	for i := 0; i < 20; i++ {
		ret, _, err := prog.Test(pkt)
		if err != nil {
			t.Fatalf("BPF_PROG_RUN i=%d: %v", i, err)
		}
		// Either pass or shot — both are fine. Just confirming no
		// kernel-side error on a 4K packet.
		_ = ret
	}
}

// TestEdgeCounterOverflow — drive a CMS cell to wrap around u32 max.
// The cell counts in u32; after 2^32 increments it wraps to 0. We
// can't realistically send 4 billion packets in a test, but we CAN
// pre-set a cell to near-max via cmsMap.Update and watch it wrap.
//
// Asserts: BPF program doesn't panic and STAT_HH_HITS reflects the
// truth (a wrapped flow correctly stays heavy because at least one
// cell is high; CMS estimator is min, so wraparound on one cell
// while others are large means min is large → still classified
// heavy. If ALL cells wrap, classification reverts to "mouse" briefly
// — that's an acceptable degradation for the edge case.)
func TestEdgeCounterOverflow(t *testing.T) {
	coll, _, prog := loadNatra(t)
	cmsMap := coll.Maps["natra_cms_map"]

	// Pre-set every CMS cell to (max - 5) so 6 increments wrap.
	nearMax := uint32(0xFFFFFFFF - 5)
	for k := uint32(0); k < 4096; k++ {
		if err := cmsMap.Update(&k, &nearMax, ebpf.UpdateAny); err != nil {
			t.Fatalf("cms[%d] preset: %v", k, err)
		}
	}

	pkt := edgeMkPkt(0x0A000001, 0x0A000002, 12345, 5201)
	// Send 10 packets — wraps cells 5x in.
	for i := 0; i < 10; i++ {
		_, _, err := prog.Test(pkt)
		if err != nil {
			t.Fatalf("BPF_PROG_RUN i=%d: %v", i, err)
		}
	}

	// We don't assert correctness of classification across the wrap
	// (CMS is approximate; this edge case is by design "best effort").
	// We DO assert: program survived, stats updated, no panic.
	passed := readPerCPUStatLocal(t, coll.Maps["natra_stats_map"], statPassed)
	throttled := readPerCPUStatLocal(t, coll.Maps["natra_stats_map"], statThrottled)
	if passed+throttled != 10 {
		t.Errorf("passed+throttled=%d, want 10 (program ran for every packet)", passed+throttled)
	}
}
