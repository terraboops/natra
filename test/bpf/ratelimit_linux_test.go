//go:build linux && bpf

// BPF-level tests for natra.bpf.o: load the program, populate config
// and bucket maps, run BPF_PROG_RUN with synthetic packets, assert the
// verdicts and stats. Catches BPF-level regressions independent of the
// Go loader and CNI integration.
//
// Headline scenarios run as sub-tests across both directions
// (natra_ingress and natra_egress) to assert per-direction
// independence. A separate cross-direction isolation test verifies
// that configuring one direction's rate doesn't bleed into the other.

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
	statPassed     = bpf.StatPassed
	statThrottled  = bpf.StatThrottled
	statHHHits     = bpf.StatHHHits
	statECNMarked  = bpf.StatECNMarked
	statEDTDelayed = bpf.StatEDTDelayed
	statDropped    = bpf.StatDropped
)

// throttleVerdict returns the TC verdict and matching disposition stat
// slot that natra produces for an above-rate non-ECN packet with
// cfg.EDTPacing == 0 (the default). Both directions drop in that case
// because EDT pacing is opt-in (requires fq qdisc downstream — without
// fq the EDT path silently breaks rate limiting). The
// TestNatraEDTPacingOnEgress test covers the EDT-enabled path
// separately.
func throttleVerdict(_ bpf.Direction) (verdict uint32, stat uint32) {
	return 2, statDropped // TC_ACT_SHOT
}

// directionCases lists the per-direction setup the headline tests
// iterate over. Each case binds together a program FD, the matching
// userspace map key for config/bucket, and the matching stats key
// helper — keeps individual tests free of direction arithmetic.
type directionCase struct {
	name    string
	dir     bpf.Direction
	prog    *ebpf.Program
	mapKey  uint32
	statKey func(slot uint32) uint32
}

func directionCases(progIngress, progEgress *ebpf.Program) []directionCase {
	return []directionCase{
		{
			name:    "ingress",
			dir:     bpf.DirectionIngress,
			prog:    progIngress,
			mapKey:  uint32(bpf.DirectionIngress),
			statKey: func(slot uint32) uint32 { return bpf.StatKey(bpf.DirectionIngress, slot) },
		},
		{
			name:    "egress",
			dir:     bpf.DirectionEgress,
			prog:    progEgress,
			mapKey:  uint32(bpf.DirectionEgress),
			statKey: func(slot uint32) uint32 { return bpf.StatKey(bpf.DirectionEgress, slot) },
		},
	}
}

// loadNatraColl loads and instantiates bpf/natra.bpf.o. Returns the
// collection (caller closes), the config map, the bucket map, the
// stats map, and both per-direction programs.
func loadNatraColl(t *testing.T) (*ebpf.Collection, *ebpf.Map, *ebpf.Map, *ebpf.Map, *ebpf.Program, *ebpf.Program) {
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
	progIngress, ok := coll.Programs["natra_ingress"]
	if !ok {
		t.Fatalf("program natra_ingress missing")
	}
	progEgress, ok := coll.Programs["natra_egress"]
	if !ok {
		t.Fatalf("program natra_egress missing")
	}
	return coll, cfgMap, bucketMap, statsMap, progIngress, progEgress
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
	_, _, _, statsMap, progIngress, progEgress := loadNatraColl(t)
	for _, dc := range directionCases(progIngress, progEgress) {
		t.Run(dc.name, func(t *testing.T) {
			pkt := synthEthIPpkt()
			ret, _, err := dc.prog.Test(pkt)
			if err != nil {
				t.Fatalf("BPF_PROG_RUN: %v", err)
			}
			// rate_bps == 0 → fail-open path, packet passes.
			if ret != 0 { // TC_ACT_OK
				t.Errorf("ret = %d, want 0 (TC_ACT_OK)", ret)
			}
			if got := readPerCPUStat(t, statsMap, dc.statKey(statPassed)); got == 0 {
				t.Errorf("STAT_PASSED for %s = 0, want >= 1", dc.name)
			}
		})
	}
}

func TestNatraTokenBucketUnderRate(t *testing.T) {
	_, cfgMap, bucketMap, statsMap, progIngress, progEgress := loadNatraColl(t)
	for _, dc := range directionCases(progIngress, progEgress) {
		t.Run(dc.name, func(t *testing.T) {
			// 100 Mbps rate, large burst so a single 64-byte packet always fits.
			cfg := natraConfig{
				RateBps:     100_000_000 / 8, // 12.5 MB/s
				BurstBytes:  1 << 20,         // 1 MB
				HHThreshold: 0,               // not exercised here
			}
			key := dc.mapKey
			if err := cfgMap.Update(&key, &cfg, ebpf.UpdateAny); err != nil {
				t.Fatalf("config update: %v", err)
			}
			tb := tokenBucket{Tokens: cfg.BurstBytes}
			if err := bucketMap.Update(&key, &tb, ebpf.UpdateAny); err != nil {
				t.Fatalf("bucket update: %v", err)
			}

			before := readPerCPUStat(t, statsMap, dc.statKey(statPassed))
			pkt := synthEthIPpkt()
			for i := 0; i < 100; i++ {
				ret, _, err := dc.prog.Test(pkt)
				if err != nil {
					t.Fatalf("BPF_PROG_RUN i=%d: %v", i, err)
				}
				if ret != 0 {
					t.Fatalf("packet %d throttled (ret=%d), expected to pass under burst capacity", i, ret)
				}
			}
			if got := readPerCPUStat(t, statsMap, dc.statKey(statPassed)) - before; got != 100 {
				t.Errorf("STAT_PASSED delta = %d, want 100", got)
			}
			if got := readPerCPUStat(t, statsMap, dc.statKey(statThrottled)); got != 0 {
				t.Errorf("STAT_THROTTLED = %d, want 0", got)
			}
		})
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

// withECT marks the packet as ECN-capable (ECT(0) — TOS bits = 10).
// bpf_skb_ecn_set_ce only sets CE on packets that already have ECT;
// non-ECT packets go down the EDT-or-drop path.
func withECT(pkt []byte) []byte {
	pkt[15] = 0x02
	return pkt
}

// TestNatraEDTPacingOnEgress confirms that with cfg.EDTPacing != 0,
// above-rate egress packets return TC_ACT_OK with STAT_EDT_DELAYED
// bumped and skb->tstamp advanced. Ingress still drops because
// transmission-side qdiscs can't pace incoming packets — natra
// zeroes EDTPacing in the ingress slot at attach time, and this
// test mirrors that contract.
func TestNatraEDTPacingOnEgress(t *testing.T) {
	_, cfgMap, bucketMap, statsMap, progIngress, progEgress := loadNatraColl(t)
	for _, dc := range directionCases(progIngress, progEgress) {
		t.Run(dc.name, func(t *testing.T) {
			cfg := natraConfig{
				RateBps:     1,
				BurstBytes:  64,
				HHThreshold: 0, // every packet is heavy
			}
			// EDT is per-direction in cfg; mirror attachBPF's
			// "ingress always zero" rule here.
			if dc.dir == bpf.DirectionEgress {
				cfg.EDTPacing = 1
			}
			key := dc.mapKey
			if err := cfgMap.Update(&key, &cfg, ebpf.UpdateAny); err != nil {
				t.Fatalf("config: %v", err)
			}
			tb := tokenBucket{Tokens: 64}
			if err := bucketMap.Update(&key, &tb, ebpf.UpdateAny); err != nil {
				t.Fatalf("bucket: %v", err)
			}

			pkt := synthEthIPpkt() // non-ECN

			// Packet 1: bucket admits, passes normally.
			if ret, _, err := dc.prog.Test(pkt); err != nil {
				t.Fatalf("packet 1: %v", err)
			} else if ret != 0 {
				t.Fatalf("packet 1 ret=%d, want 0", ret)
			}

			beforeEDT := readPerCPUStat(t, statsMap, dc.statKey(statEDTDelayed))
			beforeDrop := readPerCPUStat(t, statsMap, dc.statKey(statDropped))

			// Packet 2: bucket empty, non-ECN. Egress with EDTPacing=1
			// → EDT-delayed (TC_ACT_OK, STAT_EDT_DELAYED). Ingress
			// (EDTPacing=0) → dropped (TC_ACT_SHOT, STAT_DROPPED).
			ret, _, err := dc.prog.Test(pkt)
			if err != nil {
				t.Fatalf("packet 2: %v", err)
			}

			if dc.dir == bpf.DirectionEgress {
				if ret != 0 {
					t.Errorf("egress ret=%d, want 0 (EDT-delayed)", ret)
				}
				if got := readPerCPUStat(t, statsMap, dc.statKey(statEDTDelayed)) - beforeEDT; got != 1 {
					t.Errorf("egress STAT_EDT_DELAYED delta=%d, want 1", got)
				}
				if got := readPerCPUStat(t, statsMap, dc.statKey(statDropped)) - beforeDrop; got != 0 {
					t.Errorf("egress STAT_DROPPED delta=%d, want 0 (EDT preferred over drop)", got)
				}
			} else {
				if ret != 2 {
					t.Errorf("ingress ret=%d, want 2 (drop; EDT not set)", ret)
				}
				if got := readPerCPUStat(t, statsMap, dc.statKey(statDropped)) - beforeDrop; got != 1 {
					t.Errorf("ingress STAT_DROPPED delta=%d, want 1", got)
				}
				if got := readPerCPUStat(t, statsMap, dc.statKey(statEDTDelayed)) - beforeEDT; got != 0 {
					t.Errorf("ingress STAT_EDT_DELAYED delta=%d, want 0", got)
				}
			}
		})
	}
}

// TestNatraECNMarkOnAboveRate confirms that an ECN-capable above-rate
// packet returns TC_ACT_OK with STAT_ECN_MARKED bumped instead of
// being dropped or EDT-delayed. The bucket stays empty (no token
// deduction in the disposition path); the contract is "marked CE,
// peer's TCP will back off".
func TestNatraECNMarkOnAboveRate(t *testing.T) {
	_, cfgMap, bucketMap, statsMap, progIngress, progEgress := loadNatraColl(t)
	for _, dc := range directionCases(progIngress, progEgress) {
		t.Run(dc.name, func(t *testing.T) {
			cfg := natraConfig{
				RateBps:     1,
				BurstBytes:  64, // one packet's worth
				HHThreshold: 0,  // every packet is "heavy"
			}
			key := dc.mapKey
			if err := cfgMap.Update(&key, &cfg, ebpf.UpdateAny); err != nil {
				t.Fatalf("config: %v", err)
			}
			tb := tokenBucket{Tokens: 64} // exactly one packet of credit
			if err := bucketMap.Update(&key, &tb, ebpf.UpdateAny); err != nil {
				t.Fatalf("bucket: %v", err)
			}

			beforeECN := readPerCPUStat(t, statsMap, dc.statKey(statECNMarked))
			beforeDrop := readPerCPUStat(t, statsMap, dc.statKey(statDropped))
			beforeEDT := readPerCPUStat(t, statsMap, dc.statKey(statEDTDelayed))

			pkt := withECT(synthEthIPpkt())

			// Packet 1: bucket has tokens, passes via the normal path.
			ret, _, err := dc.prog.Test(pkt)
			if err != nil {
				t.Fatalf("packet 1: %v", err)
			}
			if ret != 0 {
				t.Fatalf("packet 1 ret=%d, want 0 (bucket admits one packet)", ret)
			}

			// Packet 2: bucket empty, ECN-capable → mark CE, pass.
			ret, _, err = dc.prog.Test(pkt)
			if err != nil {
				t.Fatalf("packet 2: %v", err)
			}
			if ret != 0 {
				t.Errorf("packet 2 ret=%d, want 0 (ECN-marked, not dropped)", ret)
			}
			if got := readPerCPUStat(t, statsMap, dc.statKey(statECNMarked)) - beforeECN; got != 1 {
				t.Errorf("STAT_ECN_MARKED delta=%d, want 1", got)
			}
			if got := readPerCPUStat(t, statsMap, dc.statKey(statDropped)) - beforeDrop; got != 0 {
				t.Errorf("STAT_DROPPED delta=%d, want 0 (ECN preferred over drop)", got)
			}
			if got := readPerCPUStat(t, statsMap, dc.statKey(statEDTDelayed)) - beforeEDT; got != 0 {
				t.Errorf("STAT_EDT_DELAYED delta=%d, want 0 (ECN preferred over EDT)", got)
			}
		})
	}
}

func TestNatraCMSMiceFlowsBypassTokenBucket(t *testing.T) {
	_, cfgMap, bucketMap, statsMap, progIngress, progEgress := loadNatraColl(t)
	for _, dc := range directionCases(progIngress, progEgress) {
		t.Run(dc.name, func(t *testing.T) {
			// Threshold = 100 means the first 100 packets of any single flow
			// pass for free; only the 101st onward go through the token bucket.
			cfg := natraConfig{
				RateBps:     1, // crippled rate — if any mouse traffic hits the
				BurstBytes:  1, // bucket, it would be throttled. None should.
				HHThreshold: 100,
			}
			key := dc.mapKey
			if err := cfgMap.Update(&key, &cfg, ebpf.UpdateAny); err != nil {
				t.Fatalf("config: %v", err)
			}
			tb := tokenBucket{}
			if err := bucketMap.Update(&key, &tb, ebpf.UpdateAny); err != nil {
				t.Fatalf("bucket: %v", err)
			}

			beforeHH := readPerCPUStat(t, statsMap, dc.statKey(statHHHits))
			beforeThrottled := readPerCPUStat(t, statsMap, dc.statKey(statThrottled))
			beforePassed := readPerCPUStat(t, statsMap, dc.statKey(statPassed))

			const flows = 50
			const perFlow = 5 // far below the 100-packet threshold
			for i := 0; i < flows; i++ {
				pkt := synthEthIPpktFromFlow(0x0A000001+uint32(i), 0x0A000002, 12345, 5201)
				for j := 0; j < perFlow; j++ {
					ret, _, err := dc.prog.Test(pkt)
					if err != nil {
						t.Fatalf("BPF_PROG_RUN flow=%d j=%d: %v", i, j, err)
					}
					if ret != 0 {
						t.Fatalf("flow=%d j=%d throttled (ret=%d) — mouse flows must pass", i, j, ret)
					}
				}
			}
			if got := readPerCPUStat(t, statsMap, dc.statKey(statHHHits)) - beforeHH; got != 0 {
				t.Errorf("STAT_HH_HITS delta = %d, want 0 — mice should never reach the bucket", got)
			}
			if got := readPerCPUStat(t, statsMap, dc.statKey(statThrottled)) - beforeThrottled; got != 0 {
				t.Errorf("STAT_THROTTLED delta = %d, want 0", got)
			}
			if got := readPerCPUStat(t, statsMap, dc.statKey(statPassed)) - beforePassed; got != flows*perFlow {
				t.Errorf("STAT_PASSED delta = %d, want %d", got, flows*perFlow)
			}
		})
	}
}

func TestNatraCMSElephantHitsBucket(t *testing.T) {
	_, cfgMap, bucketMap, statsMap, progIngress, progEgress := loadNatraColl(t)
	for _, dc := range directionCases(progIngress, progEgress) {
		t.Run(dc.name, func(t *testing.T) {
			cfg := natraConfig{
				RateBps:     1,
				BurstBytes:  64, // exactly one packet's worth
				HHThreshold: 10, // 11th packet onward is "heavy"
			}
			key := dc.mapKey
			if err := cfgMap.Update(&key, &cfg, ebpf.UpdateAny); err != nil {
				t.Fatalf("config: %v", err)
			}
			tb := tokenBucket{Tokens: 64}
			if err := bucketMap.Update(&key, &tb, ebpf.UpdateAny); err != nil {
				t.Fatalf("bucket: %v", err)
			}

			beforeHH := readPerCPUStat(t, statsMap, dc.statKey(statHHHits))
			beforeThrottled := readPerCPUStat(t, statsMap, dc.statKey(statThrottled))

			pkt := synthEthIPpktFromFlow(0x0A000001, 0x0A000002, 12345, 5201)
			// First 10 packets: count goes 1..10, none > threshold(10) → mice.
			for i := 0; i < 10; i++ {
				ret, _, err := dc.prog.Test(pkt)
				if err != nil {
					t.Fatalf("packet %d: %v", i, err)
				}
				if ret != 0 {
					t.Fatalf("packet %d throttled at count<=threshold (ret=%d)", i, ret)
				}
			}
			if got := readPerCPUStat(t, statsMap, dc.statKey(statHHHits)) - beforeHH; got != 0 {
				t.Errorf("after first 10 packets STAT_HH_HITS delta=%d, want 0", got)
			}

			// 11th packet (count=11 > 10): heavy. Burst=64 admits this packet,
			// so it's logged as a heavy-hitter PASS.
			ret, _, err := dc.prog.Test(pkt)
			if err != nil {
				t.Fatalf("packet 11: %v", err)
			}
			if ret != 0 {
				t.Errorf("11th packet ret=%d, want 0 (TC_ACT_OK; bucket has 64 tokens)", ret)
			}

			// 12th packet (count=12, still heavy): bucket empty (one packet
			// drained it), rate is 1 byte/sec so micro-second elapsed adds
			// nothing. Over-rate verdict differs per direction — egress
			// paces via EDT, ingress drops.
			wantRet, wantStat := throttleVerdict(dc.dir)
			beforeDisp := readPerCPUStat(t, statsMap, dc.statKey(wantStat))

			ret, _, err = dc.prog.Test(pkt)
			if err != nil {
				t.Fatalf("packet 12: %v", err)
			}
			if uint32(ret) != wantRet {
				t.Errorf("12th packet ret=%d, want %d (direction-specific throttle verdict)", ret, wantRet)
			}

			if got := readPerCPUStat(t, statsMap, dc.statKey(statHHHits)) - beforeHH; got != 2 {
				t.Errorf("STAT_HH_HITS delta=%d, want 2 (packets 11 and 12)", got)
			}
			if got := readPerCPUStat(t, statsMap, dc.statKey(statThrottled)) - beforeThrottled; got != 1 {
				t.Errorf("STAT_THROTTLED delta=%d, want 1", got)
			}
			if got := readPerCPUStat(t, statsMap, dc.statKey(wantStat)) - beforeDisp; got != 1 {
				t.Errorf("disposition stat delta=%d, want 1", got)
			}
		})
	}
}

func TestNatraTokenBucketThrottlesOnceBurstSpent(t *testing.T) {
	_, cfgMap, bucketMap, statsMap, progIngress, progEgress := loadNatraColl(t)
	for _, dc := range directionCases(progIngress, progEgress) {
		t.Run(dc.name, func(t *testing.T) {
			// Tiny rate so back-to-back BPF_PROG_RUN calls don't refill
			// meaningfully between iterations. Burst sized to admit exactly
			// one packet, so the second call must be throttled.
			cfg := natraConfig{
				RateBps:    1,  // effectively zero refill in the test window
				BurstBytes: 64, // exactly one synthetic packet
			}
			key := dc.mapKey
			if err := cfgMap.Update(&key, &cfg, ebpf.UpdateAny); err != nil {
				t.Fatalf("config update: %v", err)
			}
			tb := tokenBucket{Tokens: 64}
			if err := bucketMap.Update(&key, &tb, ebpf.UpdateAny); err != nil {
				t.Fatalf("bucket update: %v", err)
			}

			beforePassed := readPerCPUStat(t, statsMap, dc.statKey(statPassed))
			beforeThrottled := readPerCPUStat(t, statsMap, dc.statKey(statThrottled))

			pkt := synthEthIPpkt()

			// First packet drains the bucket and updates last_update_ns to
			// "now", anchoring the refill clock.
			ret, _, err := dc.prog.Test(pkt)
			if err != nil {
				t.Fatalf("BPF_PROG_RUN #1: %v", err)
			}
			if ret != 0 {
				t.Fatalf("first packet ret=%d, want 0 (TC_ACT_OK) — bucket should admit one full packet", ret)
			}

			// Second packet: bucket is empty, rate is 1 byte/sec, microseconds
			// since the first call → no refill → must throttle. Over-rate
			// verdict differs per direction (EDT-paced on egress, dropped
			// on ingress).
			wantRet, wantStat := throttleVerdict(dc.dir)
			beforeDisp := readPerCPUStat(t, statsMap, dc.statKey(wantStat))

			ret, _, err = dc.prog.Test(pkt)
			if err != nil {
				t.Fatalf("BPF_PROG_RUN #2: %v", err)
			}
			if uint32(ret) != wantRet {
				t.Errorf("second packet ret=%d, want %d (direction-specific throttle verdict)", ret, wantRet)
			}

			if got := readPerCPUStat(t, statsMap, dc.statKey(wantStat)) - beforeDisp; got != 1 {
				t.Errorf("disposition stat delta=%d, want 1", got)
			}
			if got := readPerCPUStat(t, statsMap, dc.statKey(statPassed)) - beforePassed; got != 1 {
				t.Errorf("STAT_PASSED delta = %d, want 1", got)
			}
			if got := readPerCPUStat(t, statsMap, dc.statKey(statThrottled)) - beforeThrottled; got != 1 {
				t.Errorf("STAT_THROTTLED delta = %d, want 1", got)
			}
		})
	}
}

// TestCrossDirectionIsolation pins the per-direction state contract:
// configuring ingress to throttle and egress to pass-through means
// packets through the ingress program drop while the same packets
// through the egress program flow. Catches any accidental shared
// state (one map slot, swapped keys, missing direction parameter)
// that would let the two directions affect each other.
func TestCrossDirectionIsolation(t *testing.T) {
	_, cfgMap, bucketMap, statsMap, progIngress, progEgress := loadNatraColl(t)

	// Ingress: tight bucket, low threshold. Will throttle quickly.
	ingressKey := uint32(bpf.DirectionIngress)
	if err := cfgMap.Update(&ingressKey,
		&natraConfig{RateBps: 1, BurstBytes: 64, HHThreshold: 1},
		ebpf.UpdateAny); err != nil {
		t.Fatalf("ingress cfg: %v", err)
	}
	if err := bucketMap.Update(&ingressKey,
		&tokenBucket{Tokens: 64},
		ebpf.UpdateAny); err != nil {
		t.Fatalf("ingress bucket: %v", err)
	}

	// Egress: high rate, infinite burst. Should never throttle.
	egressKey := uint32(bpf.DirectionEgress)
	if err := cfgMap.Update(&egressKey,
		&natraConfig{RateBps: 1_000_000_000, BurstBytes: 1 << 30, HHThreshold: 1 << 30},
		ebpf.UpdateAny); err != nil {
		t.Fatalf("egress cfg: %v", err)
	}
	if err := bucketMap.Update(&egressKey,
		&tokenBucket{Tokens: 1 << 30},
		ebpf.UpdateAny); err != nil {
		t.Fatalf("egress bucket: %v", err)
	}

	pkt := synthEthIPpktFromFlow(0x0A000001, 0x0A000002, 12345, 5201)

	// Drive 20 packets through both programs. Ingress should drop
	// after the bucket drains; egress should pass everything.
	for i := 0; i < 20; i++ {
		if _, _, err := progIngress.Test(pkt); err != nil {
			t.Fatalf("ingress BPF_PROG_RUN i=%d: %v", i, err)
		}
		ret, _, err := progEgress.Test(pkt)
		if err != nil {
			t.Fatalf("egress BPF_PROG_RUN i=%d: %v", i, err)
		}
		if ret != 0 {
			t.Errorf("egress packet %d ret=%d, want 0 — egress should pass under wide-open config", i, ret)
		}
	}

	ingressThrottled := readPerCPUStat(t, statsMap, bpf.StatKey(bpf.DirectionIngress, statThrottled))
	egressThrottled := readPerCPUStat(t, statsMap, bpf.StatKey(bpf.DirectionEgress, statThrottled))
	egressPassed := readPerCPUStat(t, statsMap, bpf.StatKey(bpf.DirectionEgress, statPassed))

	if ingressThrottled == 0 {
		t.Errorf("ingress throttled = 0, expected > 0 — tight ingress config didn't drop anything")
	}
	if egressThrottled != 0 {
		t.Errorf("egress throttled = %d, want 0 — egress config was wide open and shouldn't drop", egressThrottled)
	}
	if egressPassed < 20 {
		t.Errorf("egress passed = %d, want >= 20 — every egress packet should pass", egressPassed)
	}
}
