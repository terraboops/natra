//go:build linux && bpf

// Layer 3 chaos — BPF failure modes.
//
// Phase 1 lands TestVerifierRejection — a real test that loads an
// intentionally-invalid BPF program and asserts the verifier rejects
// it with a clear, multi-line *ebpf.VerifierError. The remaining
// scenarios are still stubbed for future iterations.

package bpf_test

import (
	"errors"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"

	"github.com/cilium/ebpf"
	"github.com/cilium/ebpf/rlimit"
)

// invalidBPFPath returns the absolute path to a chaos-testdata BPF
// object built by `make build-bpf`. Each .bpf.c under bpf/testdata/
// compiles to a .bpf.o that the verifier MUST reject.
func invalidBPFPath(name string) string {
	_, thisFile, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(thisFile), "..", "..", "bpf", "testdata", name)
}

func TestVerifierRejection(t *testing.T) {
	if err := rlimit.RemoveMemlock(); err != nil {
		t.Fatalf("remove memlock: %v", err)
	}

	path := invalidBPFPath("invalid_oob_packet_access.bpf.o")
	spec, err := ebpf.LoadCollectionSpec(path)
	if err != nil {
		t.Fatalf("load spec %s: %v", path, err)
	}

	coll, err := ebpf.NewCollection(spec)
	if err == nil {
		coll.Close()
		t.Fatalf("expected verifier rejection for %s, got nil error (collection loaded successfully)", path)
	}

	// The kernel verifier returns rich, multi-line error messages.
	// cilium/ebpf wraps them in *ebpf.VerifierError so callers can
	// detect the class of failure reliably; natra's pkg/bpf.Load()
	// uses the same `errors.As` shape.
	var verr *ebpf.VerifierError
	if !errors.As(err, &verr) {
		t.Fatalf("expected *ebpf.VerifierError, got %T: %v", err, err)
	}

	// The OOB packet read should produce a message about packet
	// access. Kernel wording varies ("invalid access to packet",
	// "R1 invalid mem access"), so we match the family rather than
	// an exact string.
	msg := verr.Error()
	if !strings.Contains(msg, "packet") && !strings.Contains(msg, "access") {
		t.Errorf("verifier message doesn't mention packet/access: %q", msg)
	}
	t.Logf("verifier rejected %s as expected (excerpt): %s",
		filepath.Base(path), firstLines(msg, 3))
}

// firstLines returns up to n lines of s, joined with " / " — keeps
// log output readable without losing the headline error.
func firstLines(s string, n int) string {
	parts := strings.SplitN(s, "\n", n+1)
	if len(parts) > n {
		parts = parts[:n]
	}
	return strings.Join(parts, " / ")
}

// TestMalformedPackets feeds the natra BPF program packets that real
// network paths produce but rate-limit logic can stumble on:
//   - non-IP (ARP-shaped EtherType)
//   - IPv6 (we ignore it; verifier-runtime should not trip)
//   - truncated IP header (skb shorter than ihl declares)
//   - bogus IP version
//   - zero-length payload
//
// Asserts that natra returns TC_ACT_OK (pass-through) for each — non-IP
// is supposed to flow free — and never produces a kernel verifier
// runtime error. Bonus: STAT_THROTTLED stays 0 (we never drop on
// malformed data; the upstream main CNI is responsible for that).
func TestMalformedPackets(t *testing.T) {
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

	// Even with a real config (not the fail-open path), malformed
	// packets must pass through.
	cfg := struct {
		R, B, H uint64
	}{R: 1_250_000, B: 64_000, H: 10}
	zero := uint32(0)
	if err := coll.Maps["natra_config_map"].Update(&zero, &cfg, ebpf.UpdateAny); err != nil {
		t.Fatalf("config: %v", err)
	}
	if err := coll.Maps["natra_bucket_map"].Update(&zero, &struct {
		_lock, _pad        uint32
		Tokens, LastUpdate uint64
	}{Tokens: cfg.B}, ebpf.UpdateAny); err != nil {
		t.Fatalf("bucket: %v", err)
	}
	prog := coll.Programs["natra_ratelimit"]

	// Cases below are all >= 14 bytes (ETH_HLEN). Below that the
	// BPF_PROG_RUN syscall itself returns EINVAL — kernel-side, not
	// natra's responsibility, and the network stack drops sub-eth
	// runts at L1/L2 long before any TC ingress hook fires.
	cases := []struct {
		name string
		pkt  []byte
	}{
		{"non-ip ARP-like", arpLike()},
		{"ipv6 (TC pass-through expected)", ipv6Pkt()},
		{"truncated_ip_header", truncatedIP()},
		{"bogus IP version (5)", bogusIPVersion()},
		{"eth-only no payload (14 bytes)", make([]byte, 14)},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ret, _, err := prog.Test(tc.pkt)
			if err != nil {
				t.Fatalf("BPF_PROG_RUN: %v", err)
			}
			// All malformed inputs should TC_ACT_OK (pass-through);
			// natra explicitly chooses fail-open on parse failure.
			if ret != 0 {
				t.Errorf("ret=%d, want 0 (TC_ACT_OK) for malformed input %q", ret, tc.name)
			}
		})
	}

	throttled := readPerCPUStatLocal(t, coll.Maps["natra_stats_map"], 1)
	if throttled != 0 {
		t.Errorf("STAT_THROTTLED=%d after malformed-only packets, want 0", throttled)
	}
}

// readPerCPUStatLocal duplicates readPerCPUStat from
// ratelimit_linux_test.go — both files are in the same _test package,
// so we keep the helper named distinctly to avoid name collision while
// staying self-contained.
func readPerCPUStatLocal(t *testing.T, m *ebpf.Map, idx uint32) uint64 {
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

// Test inputs constructed by hand to keep the test independent of any
// IP/ETH-formatting helper that could itself contain bugs.

func arpLike() []byte {
	pkt := make([]byte, 64)
	pkt[12], pkt[13] = 0x08, 0x06 // ETH_P_ARP
	return pkt
}

func ipv6Pkt() []byte {
	pkt := make([]byte, 64)
	pkt[12], pkt[13] = 0x86, 0xdd // ETH_P_IPV6
	pkt[14] = 0x60                // version 6, traffic class 0
	return pkt
}

func truncatedIP() []byte {
	// Eth header (14 bytes) declaring IPv4, but no IP body follows.
	pkt := make([]byte, 14)
	pkt[12], pkt[13] = 0x08, 0x00
	return pkt
}

func bogusIPVersion() []byte {
	pkt := make([]byte, 64)
	pkt[12], pkt[13] = 0x08, 0x00
	pkt[14] = 0x55 // version 5, ihl=5 — version=5 is not a valid IP version
	return pkt
}

// TestConcurrentMapUpdates exercises CMS atomicity: many goroutines
// drive Program.Test against natra concurrently; CMS counters must
// converge to the total packet count seen for that flow within the
// CMS approximation tolerance (estimator is min across rows; should
// equal the exact count for a single flow without collisions).
func TestConcurrentMapUpdates(t *testing.T) {
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

	// Configure with high threshold so all packets are mice (no token
	// bucket contention — we're stress-testing CMS atomic adds).
	cfg := struct{ R, B, H uint64 }{R: 1_000_000_000, B: 1 << 30, H: 1 << 30}
	zero := uint32(0)
	_ = coll.Maps["natra_config_map"].Update(&zero, &cfg, ebpf.UpdateAny)
	_ = coll.Maps["natra_bucket_map"].Update(&zero, &struct {
		_lock, _pad        uint32
		Tokens, LastUpdate uint64
	}{Tokens: cfg.B}, ebpf.UpdateAny)

	prog := coll.Programs["natra_ratelimit"]
	pkt := elephantPkt()

	const goroutines = 8
	const perGoroutine = 1000
	expectedTotal := uint64(goroutines * perGoroutine)

	var wg sync.WaitGroup
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			for j := 0; j < perGoroutine; j++ {
				if _, _, err := prog.Test(pkt); err != nil {
					// Can't t.Fatalf from a goroutine pre-Go 1.21
					t.Errorf("BPF_PROG_RUN: %v", err)
					return
				}
			}
		}()
	}
	wg.Wait()

	// Sum of STAT_PASSED across CPUs should equal the exact packet
	// count we sent — the only path is mouse-pass with HHThreshold so
	// high it's never crossed.
	passed := readPerCPUStatLocal(t, coll.Maps["natra_stats_map"], 0)
	if passed != expectedTotal {
		t.Errorf("STAT_PASSED=%d, want %d (concurrent atomic adds lost packets?)", passed, expectedTotal)
	}
}

// elephantPkt is a single-flow packet shared by the concurrency test
// and the OOM test below. Keeps fields stable so each invocation
// hashes to the same CMS cells.
func elephantPkt() []byte {
	pkt := make([]byte, 64)
	pkt[12], pkt[13] = 0x08, 0x00
	pkt[14] = 0x45
	pkt[16], pkt[17] = 0x00, 0x32
	pkt[23] = 6
	pkt[26], pkt[27], pkt[28], pkt[29] = 10, 0, 0, 1
	pkt[30], pkt[31], pkt[32], pkt[33] = 10, 0, 0, 2
	pkt[34], pkt[35] = 0x30, 0x39
	pkt[36], pkt[37] = 0x14, 0x51
	return pkt
}

// TestMapCapacityOOM drives many distinct flows past CMS capacity. CMS
// is approximate by design — collisions are expected and the estimator
// degrades gracefully. The assertion is *not* "no collisions" but
// "the program doesn't crash, kernel doesn't panic, and STAT_PASSED
// continues counting".
//
// We send 8192 distinct flows × 1 packet each. CMS_WIDTH × CMS_DEPTH
// is 1024 × 4 = 4096 cells, so this is 2× capacity — every flow's
// counters must collide with at least one other flow.
func TestMapCapacityOOM(t *testing.T) {
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

	// All packets are mice (high threshold). The point is to load CMS
	// to overflow without engaging the bucket.
	cfg := struct{ R, B, H uint64 }{R: 1, B: 1, H: 1 << 30}
	zero := uint32(0)
	_ = coll.Maps["natra_config_map"].Update(&zero, &cfg, ebpf.UpdateAny)
	_ = coll.Maps["natra_bucket_map"].Update(&zero, &struct {
		_lock, _pad        uint32
		Tokens, LastUpdate uint64
	}{}, ebpf.UpdateAny)

	prog := coll.Programs["natra_ratelimit"]
	const flows = 8192 // 2× CMS capacity
	for i := 0; i < flows; i++ {
		pkt := mkUniqueFlowPkt(uint32(i))
		if _, _, err := prog.Test(pkt); err != nil {
			t.Fatalf("BPF_PROG_RUN flow=%d: %v", i, err)
		}
	}

	passed := readPerCPUStatLocal(t, coll.Maps["natra_stats_map"], 0)
	if passed != flows {
		t.Errorf("STAT_PASSED=%d, want %d (program crashed mid-overflow?)", passed, flows)
	}

	// Sanity: the CMS map is still queryable (no kernel-side corruption)
	// and contains values within u32 range (no overflow).
	cmsMap := coll.Maps["natra_cms_map"]
	var maxV uint32
	for i := uint32(0); i < 4096; i++ {
		var v uint32
		if err := cmsMap.Lookup(&i, &v); err != nil {
			t.Fatalf("cms[%d]: %v", i, err)
		}
		if v > maxV {
			maxV = v
		}
	}
	if maxV == 0 {
		t.Errorf("CMS max=0 after %d packets — increments aren't landing", flows)
	}
	t.Logf("OOM scenario: %d flows ran, CMS max bucket=%d (collisions expected at >1)",
		flows, maxV)
}

// mkUniqueFlowPkt produces a packet whose 5-tuple varies with `idx`,
// guaranteeing CMS hashes are spread across all dimensions of input.
func mkUniqueFlowPkt(idx uint32) []byte {
	pkt := make([]byte, 64)
	pkt[12], pkt[13] = 0x08, 0x00
	pkt[14] = 0x45
	pkt[16], pkt[17] = 0x00, 0x32
	pkt[23] = 6
	pkt[26], pkt[27], pkt[28], pkt[29] = 10, byte(idx>>16), byte(idx>>8), byte(idx)
	pkt[30], pkt[31], pkt[32], pkt[33] = 10, 0xFF, 0, 1
	pkt[34], pkt[35] = byte(idx>>8), byte(idx)
	pkt[36], pkt[37] = 0x14, 0x51
	return pkt
}

func TestKernelFeatureFallback(t *testing.T) {
	t.Skip("natra currently uses clsact uniformly (see pkg/bpf/loader.go AttachIngress for rationale); revisit if/when tcx-link pinning works in our deployment environments")
}

func TestDetachRace(t *testing.T) {
	t.Skip("requires real veth + concurrent CNI ADD/DEL drivers; deferred until L4 chaos lands TestPodChurnDuringTraffic which exercises the same kernel state")
}
