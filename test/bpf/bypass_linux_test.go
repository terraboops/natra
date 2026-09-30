//go:build linux && bpf

// Limit-bypass regressions: traffic shapes that used to reach the
// fast pass without ever touching the token bucket. Each test drives
// a single "elephant" that would be throttled if natra classified it
// as one flow, and asserts that it is.

package bpf_test

import (
	"encoding/binary"
	"testing"

	"github.com/cilium/ebpf"
)

// bypassCfg makes any traffic that reaches the bucket throttle: the
// rate is 1 byte/sec and the bucket holds a single byte. 640 bytes
// is ten 64-byte packets of one flow.
var bypassCfg = natraConfig{RateBps: 1, BurstBytes: 1, HHThreshold: 640}

type bypassCounts struct{ passed, hh, throttled uint64 }

// runBypass sends pkts (one BPF_PROG_RUN each) through every direction
// with cfg installed and returns per-direction stat deltas.
func runBypass(t *testing.T, cfg natraConfig, pkts func(i int) []byte, n int) map[string]bypassCounts {
	t.Helper()
	_, cfgMap, bucketMap, statsMap, progIngress, progEgress := loadNatraColl(t)
	out := map[string]bypassCounts{}
	for _, dc := range directionCases(progIngress, progEgress) {
		key := dc.mapKey
		if err := cfgMap.Update(&key, &cfg, ebpf.UpdateAny); err != nil {
			t.Fatalf("config: %v", err)
		}
		if err := bucketMap.Update(&key, &tokenBucket{}, ebpf.UpdateAny); err != nil {
			t.Fatalf("bucket: %v", err)
		}
		p0 := readPerCPUStat(t, statsMap, dc.statKey(statPassed))
		h0 := readPerCPUStat(t, statsMap, dc.statKey(statHHHits))
		t0 := readPerCPUStat(t, statsMap, dc.statKey(statThrottled))
		for i := 0; i < n; i++ {
			if _, _, err := dc.prog.Test(pkts(i)); err != nil {
				t.Fatalf("%s BPF_PROG_RUN #%d: %v", dc.name, i, err)
			}
		}
		out[dc.name] = bypassCounts{
			passed:    readPerCPUStat(t, statsMap, dc.statKey(statPassed)) - p0,
			hh:        readPerCPUStat(t, statsMap, dc.statKey(statHHHits)) - h0,
			throttled: readPerCPUStat(t, statsMap, dc.statKey(statThrottled)) - t0,
		}
	}
	return out
}

func requireThrottled(t *testing.T, got map[string]bypassCounts, what string) {
	t.Helper()
	for dir, c := range got {
		if c.hh == 0 || c.throttled == 0 {
			t.Errorf("%s: %s never reached the bucket (passed=%d hh_hits=%d throttled=%d)",
				dir, what, c.passed, c.hh, c.throttled)
		}
	}
}

// ipv6TCPPkt returns ETH + IPv6 + TCP (74 bytes) for one fixed flow.
func ipv6TCPPkt() []byte {
	pkt := make([]byte, 14+40+20)
	binary.BigEndian.PutUint16(pkt[12:14], 0x86dd)
	pkt[14] = 0x60
	binary.BigEndian.PutUint16(pkt[18:20], 20) // payload length
	pkt[20] = 6                                // next header TCP
	pkt[21] = 64
	copy(pkt[22:38], []byte{0xfd, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 1})
	copy(pkt[38:54], []byte{0xfd, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 2})
	binary.BigEndian.PutUint16(pkt[54:56], 40000)
	binary.BigEndian.PutUint16(pkt[56:58], 5201)
	return pkt
}

// TestBypassIPv6Elephant: IPv6 used to skip classification entirely.
func TestBypassIPv6Elephant(t *testing.T) {
	requireThrottled(t, runBypass(t, bypassCfg, func(int) []byte { return ipv6TCPPkt() }, 50),
		"single-flow IPv6 elephant")
}

// TestBypassIPv4Fragments: fragments of one datagram used to be keyed
// on whatever payload bytes sat where the ports would be, so each
// fragment looked like a new mouse.
func TestBypassIPv4Fragments(t *testing.T) {
	frag := func(i int) []byte {
		pkt := synthEthIPpktFromFlow(0x0A000001, 0x0A000002, uint16(i), uint16(i*7))
		binary.BigEndian.PutUint16(pkt[20:22], 0x2000|uint16(i)) // MF + offset
		return pkt
	}
	requireThrottled(t, runBypass(t, bypassCfg, frag, 50), "IPv4 fragments of one host pair")
}

// TestBypassNonIPFlood: non-IP frames used to pass unaccounted, so a
// pod with a raw socket could send any volume under an unused
// EtherType.
func TestBypassNonIPFlood(t *testing.T) {
	frame := func(int) []byte {
		pkt := make([]byte, 64)
		binary.BigEndian.PutUint16(pkt[12:14], 0x88b5) // local experimental
		return pkt
	}
	requireThrottled(t, runBypass(t, bypassCfg, frame, 50), "non-IP flood")
}

// TestBypassTruncatedL4: an IPv4 TCP packet too short for a TCP header
// used to fail parsing and pass unaccounted.
func TestBypassTruncatedL4(t *testing.T) {
	short := func(int) []byte {
		pkt := synthEthIPpkt()[:40] // ETH + IP + 6 bytes
		binary.BigEndian.PutUint16(pkt[16:18], 26)
		binary.BigEndian.PutUint32(pkt[26:30], 0x0A000001)
		binary.BigEndian.PutUint32(pkt[30:34], 0x0A000002)
		return pkt
	}
	requireThrottled(t, runBypass(t, bypassCfg, short, 50), "truncated-L4 IPv4")
}

// TestBypassRotatingMice: a pod opening a new 5-tuple per few packets
// keeps every flow under hh_threshold. The per-direction mouse budget
// (4 × threshold at this rate) caps how much of that passes.
func TestBypassRotatingMice(t *testing.T) {
	cfg := natraConfig{RateBps: 1, BurstBytes: 1, HHThreshold: 6400}
	const flows, perFlow, pktLen = 1000, 5, 64
	mice := func(i int) []byte {
		return synthEthIPpktFromFlow(0x0A000001+uint32(i/perFlow), 0x0A000002, 12345, 5201)
	}
	got := runBypass(t, cfg, mice, flows*perFlow)
	// Budget is 25600 bytes per ~134 ms window. The loop can straddle
	// one window boundary, so allow two budgets plus one packet.
	const maxPassed = (2*4*6400 + pktLen) / pktLen
	for dir, c := range got {
		if c.passed > maxPassed {
			t.Errorf("%s: %d mouse packets passed (%d bytes), want <= %d — rotating flows bypass the limit",
				dir, c.passed, c.passed*pktLen, maxPassed)
		}
		if c.throttled == 0 {
			t.Errorf("%s: nothing throttled after %d bytes of mice at 1 B/s", dir, flows*perFlow*pktLen)
		}
	}
}
