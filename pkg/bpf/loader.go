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
	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
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
//
// Attachment goes via clsact qdisc + tc filter (see AttachIngress for
// the rationale). The kernel's qdisc tree owns the attachment after
// FilterReplace returns, so we don't track a userspace link handle.
type Program struct {
	coll      *ebpf.Collection
	prog      *ebpf.Program
	configMap *ebpf.Map
	bucketMap *ebpf.Map
	statsMap  *ebpf.Map
	cmsMap    *ebpf.Map // CMS counters; nil when running with the placeholder program
	tcxLink   link.Link // unused for clsact path; reserved for future tcx-link path
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
	if cms, ok := coll.Maps["natra_cms_map"]; ok {
		p.cmsMap = cms
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

// PinMaps pins the program's maps under `dir`/<containerID>-<map> so a
// debug subcommand can read stats and CMS counters out-of-band. The
// program itself doesn't need pinned maps to function (clsact filter
// holds the references), but pinning is the easiest way to expose
// runtime state to a separate `natra dump` invocation.
func (p *Program) PinMaps(dir, containerID string) error {
	if dir == "" || containerID == "" {
		return nil
	}
	for name, m := range map[string]*ebpf.Map{
		"config": p.configMap,
		"bucket": p.bucketMap,
		"stats":  p.statsMap,
		"cms":    p.cmsMap,
	} {
		if m == nil {
			continue
		}
		path := dir + "/" + containerID + "-" + name + ".map"
		if err := m.Pin(path); err != nil {
			return fmt.Errorf("pin %s: %w", path, err)
		}
	}
	return nil
}

// AttachIngress binds the program to `ifIndex`'s INGRESS hook (packets
// arriving at the interface). For natra this is the pod-side veth's
// eth0 ingress — i.e., packets arriving INTO the pod, which is the
// direction `kubernetes.io/ingress-bandwidth` describes.
//
// Uses the legacy `clsact` + tc-filter path rather than tcx links,
// because tcx links require BPF_OBJ_PIN to persist past the CNI
// plugin process exit, and that syscall returned EPERM in our test
// environment (likely an LSM/apparmor restriction on tcx-link pins
// even with CAP_BPF). clsact attachment lives in the kernel's qdisc
// tree — once `tc filter add` succeeds, the BPF program is referenced
// by the filter and stays attached until the veth is deleted, with
// no userspace fd / link / pin required. Tradeoff: clsact is older
// (kernel 5.x+) but works on every kernel natra targets, including
// 6.x where tcx is also available. Revisit when we have a confirmed
// configuration where tcx-link pinning works.
func (p *Program) AttachIngress(ifIndex int, _ string) error {
	link, err := netlink.LinkByIndex(ifIndex)
	if err != nil {
		return fmt.Errorf("netlink LinkByIndex(%d): %w", ifIndex, err)
	}

	// 1. Make sure clsact qdisc exists on the link. Idempotent — if
	// another tc filter (e.g. kindnet's, or a previous natra ADD on
	// the same veth before recreate) already added clsact, we get
	// EEXIST and ignore.
	clsact := &netlink.GenericQdisc{
		QdiscAttrs: netlink.QdiscAttrs{
			LinkIndex: link.Attrs().Index,
			Handle:    netlink.MakeHandle(0xffff, 0),
			Parent:    netlink.HANDLE_CLSACT,
		},
		QdiscType: "clsact",
	}
	if err := netlink.QdiscAdd(clsact); err != nil && !errors.Is(err, unix.EEXIST) {
		return fmt.Errorf("add clsact qdisc to ifindex %d: %w", ifIndex, err)
	}

	// 2. Attach the BPF program via a tc filter on the ingress hook.
	// `Parent: HANDLE_MIN_INGRESS` selects the clsact ingress branch.
	// `Priority: 1, Protocol: ETH_P_ALL` is the conventional default.
	// `DirectAction: true` lets the BPF program return TC_ACT_* verdicts
	// directly without a follow-on policer.
	filter := &netlink.BpfFilter{
		FilterAttrs: netlink.FilterAttrs{
			LinkIndex: link.Attrs().Index,
			Parent:    netlink.HANDLE_MIN_INGRESS,
			Handle:    netlink.MakeHandle(0, 1),
			Protocol:  unix.ETH_P_ALL,
			Priority:  1,
		},
		Fd:           p.prog.FD(),
		Name:         "natra_ratelimit",
		DirectAction: true,
	}
	if err := netlink.FilterReplace(filter); err != nil {
		return fmt.Errorf("replace tc bpf filter on ifindex %d: %w", ifIndex, err)
	}

	// p.link stays nil — clsact attachment is anchored in the kernel's
	// qdisc tree, no userspace handle to track. Cleanup happens when
	// the underlying veth is deleted (which the next chained CNI plugin
	// does at pod teardown), so cmdDel is a no-op too.
	return nil
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

// Close releases the userspace BPF references. The kernel keeps the
// program loaded and attached as long as the clsact filter holds a
// reference, so closing here is safe — packets continue flowing
// through the rate limiter after this call.
func (p *Program) Close() error {
	var firstErr error
	if p.tcxLink != nil {
		if err := p.tcxLink.Close(); err != nil {
			firstErr = err
		}
		p.tcxLink = nil
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
