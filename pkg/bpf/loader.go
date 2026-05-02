//go:build linux

// Package bpf loads natra's compiled BPF object, attaches it to a
// network interface as a tcx (kernel ≥ 6.6) or clsact (older) program,
// and exposes typed accessors for the config / bucket / stats maps.
//
// The .bpf.o is embedded at build time so the natra binary is
// self-contained — no /etc/natra/natra.bpf.o, no DaemonSet shipping
// the bytecode separately. Embedding adds ~10 KB to the binary; in
// exchange the install path is just "copy this binary to /opt/cni/bin
// and you're done."
package bpf

import (
	"bytes"
	_ "embed"
	"errors"
	"fmt"

	"github.com/cilium/ebpf"
	"github.com/cilium/ebpf/link"
	"github.com/cilium/ebpf/rlimit"
)

//go:embed natra.bpf.o
var natraBPF []byte

// Config mirrors `struct natra_config` in bpf/natra.bpf.c. Field order
// and types must match exactly — cilium/ebpf uses BTF to validate, but
// only the C-side BTF; mismatched Go layout would silently corrupt.
type Config struct {
	RateBps     uint64
	BurstBytes  uint64
	HHThreshold uint64
}

// TokenBucket mirrors `struct token_bucket`. Field order matters; the
// 4-byte spin lock field at the front + 4 bytes of pad align the 64-bit
// fields to 8 bytes (BPF spin lock fields are exactly u32-sized).
type TokenBucket struct {
	_lock        uint32 // bpf_spin_lock — kernel writes; userspace zero-fills.
	_pad         uint32
	Tokens       uint64
	LastUpdateNs uint64
}

// Stats slot indices match the BPF program's enum.
const (
	StatPassed    uint32 = 0
	StatThrottled uint32 = 1
	StatHHHits    uint32 = 2
)

// Program holds a loaded natra BPF program plus its maps. One Program
// per attached veth — the maps are not shared across pods because each
// pod has its own rate limit, so concurrent CNI ADDs each instantiate
// their own Program/Collection.
type Program struct {
	coll      *ebpf.Collection
	prog      *ebpf.Program
	configMap *ebpf.Map
	bucketMap *ebpf.Map
	statsMap  *ebpf.Map
	link      link.Link // tcx attachment; nil before Attach()
}

// Load instantiates the embedded BPF object. The kernel verifies it on
// NewCollection; verification failures surface here as
// `*ebpf.VerifierError`, which has a multi-line `.Error()` showing the
// rejected instruction. Caller closes via Close().
func Load() (*Program, error) {
	if err := rlimit.RemoveMemlock(); err != nil {
		return nil, fmt.Errorf("remove memlock: %w", err)
	}

	spec, err := ebpf.LoadCollectionSpecFromReader(bytes.NewReader(natraBPF))
	if err != nil {
		return nil, fmt.Errorf("load embedded spec: %w", err)
	}

	coll, err := ebpf.NewCollection(spec)
	if err != nil {
		// Verifier errors are usually multi-line; preserve them.
		var verr *ebpf.VerifierError
		if errors.As(err, &verr) {
			return nil, fmt.Errorf("BPF verifier rejected program:\n%+v", verr)
		}
		return nil, fmt.Errorf("instantiate collection: %w", err)
	}

	p := &Program{coll: coll}
	for name, dst := range map[string]**ebpf.Map{
		"natra_config_map": &p.configMap,
		"natra_bucket_map": &p.bucketMap,
		"natra_stats_map":  &p.statsMap,
	} {
		m, ok := coll.Maps[name]
		if !ok {
			coll.Close()
			return nil, fmt.Errorf("map %q missing from BPF object (recompile?)", name)
		}
		*dst = m
	}

	prog, ok := coll.Programs["natra_ratelimit"]
	if !ok {
		coll.Close()
		return nil, fmt.Errorf("program 'natra_ratelimit' missing from BPF object")
	}
	p.prog = prog
	return p, nil
}

// Configure writes the per-pod rate-limit configuration. Must be called
// AFTER Load and BEFORE the program starts seeing traffic — otherwise
// the BPF code's fail-open path (rate_bps == 0 → TC_ACT_OK) lets traffic
// through unrate-limited until config arrives.
func (p *Program) Configure(cfg Config) error {
	zero := uint32(0)
	if err := p.configMap.Update(&zero, &cfg, ebpf.UpdateAny); err != nil {
		return fmt.Errorf("config map update: %w", err)
	}
	// Pre-fill the bucket to burst capacity so the first packet doesn't
	// have to wait on the refill clock.
	tb := TokenBucket{Tokens: cfg.BurstBytes}
	if err := p.bucketMap.Update(&zero, &tb, ebpf.UpdateAny); err != nil {
		return fmt.Errorf("bucket map update: %w", err)
	}
	return nil
}

// AttachIngress binds the program to `ifIndex`'s INGRESS hook (packets
// arriving at the interface). For natra this is the pod-side veth's
// eth0 ingress — i.e., packets arriving INTO the pod, which is the
// direction `kubernetes.io/ingress-bandwidth` describes.
//
// Tries tcx first (kernel ≥ 6.6, qdisc-less); clsact fallback for
// older kernels is TODO (P1.5+).
func (p *Program) AttachIngress(ifIndex int) error {
	tcx, err := link.AttachTCX(link.TCXOptions{
		Interface: ifIndex,
		Program:   p.prog,
		Attach:    ebpf.AttachTCXIngress,
	})
	if err == nil {
		p.link = tcx
		return nil
	}
	return fmt.Errorf("AttachTCX(ingress) failed and clsact fallback not yet implemented: %w (TODO: P1.5+ adds clsact)", err)
}

// Stats returns the accumulated counter values across all CPUs.
type Stats struct {
	Passed    uint64
	Throttled uint64
	HHHits    uint64
}

// Stats reads the per-CPU stats map and returns the aggregate. Cheap
// (no syscall per CPU; cilium/ebpf does one BPF_MAP_LOOKUP that returns
// all per-CPU values).
func (p *Program) Stats() (Stats, error) {
	var s Stats
	if err := p.readStat(StatPassed, &s.Passed); err != nil {
		return s, err
	}
	if err := p.readStat(StatThrottled, &s.Throttled); err != nil {
		return s, err
	}
	if err := p.readStat(StatHHHits, &s.HHHits); err != nil {
		return s, err
	}
	return s, nil
}

func (p *Program) readStat(idx uint32, sum *uint64) error {
	var values []uint64
	if err := p.statsMap.Lookup(&idx, &values); err != nil {
		return fmt.Errorf("stats lookup idx=%d: %w", idx, err)
	}
	var total uint64
	for _, v := range values {
		total += v
	}
	*sum = total
	return nil
}

// Close detaches the program (if attached) and frees the collection.
// Safe to call multiple times.
func (p *Program) Close() error {
	var firstErr error
	if p.link != nil {
		if err := p.link.Close(); err != nil {
			firstErr = err
		}
		p.link = nil
	}
	if p.coll != nil {
		p.coll.Close()
		p.coll = nil
	}
	return firstErr
}

// Program returns the underlying *ebpf.Program. Exposed for test code
// that wants to drive BPF_PROG_RUN directly without attaching.
func (p *Program) Program() *ebpf.Program {
	return p.prog
}
