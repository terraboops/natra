//go:build linux

// Package bpf loads the embedded natra BPF object and attaches it to a
// pod-side veth ingress. Two attach modes:
//
//   - AttachTCX (default): cilium/ebpf link.AttachTCX, pin the resulting
//     link to bpffs so it survives the CNI plugin's process exit. Uses
//     bpf_mprog under the hood and composes cleanly with other
//     pod-side BPF (cilium-agent, aws-network-policy-agent).
//   - AttachClsactPodside (opt-in): clsact qdisc + tc filter on the
//     pod-side veth from inside the pod netns. No userspace handle to
//     track; the kernel garbage-collects the filter when the veth is
//     deleted. Useful on kernels < 6.6 where tcx isn't available, but
//     can collide with other pod-side clsact users.
//
// The .bpf.o is embedded at build time so the natra binary is
// self-contained — install is just "copy the binary to /opt/cni/bin".
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

// AttachMode selects how AttachIngress wires the BPF program to a veth.
type AttachMode int

const (
	// AttachTCX is the default. Requires kernel 6.6+ and a bpffs mount
	// at /sys/fs/bpf (or wherever pinPath lives). Composes cleanly with
	// other pod-side BPF.
	AttachTCX AttachMode = iota
	// AttachClsactPodside is the opt-in fallback for older kernels.
	// Works on 5.x+, no bpffs requirement, but collides with anything
	// else attaching clsact filters in the pod's netns.
	AttachClsactPodside
)

// Config mirrors `struct natra_config` in bpf/natra.bpf.c. Field
// order and types must match the C side exactly.
type Config struct {
	RateBps     uint64
	BurstBytes  uint64
	HHThreshold uint64
}

// TokenBucket mirrors `struct token_bucket`. The first u32 is the
// kernel's bpf_spin_lock; the second is alignment padding before the
// 8-byte fields. Both are kernel-managed — userspace must include
// them in the layout but never reads them.
type TokenBucket struct {
	_            uint32 // bpf_spin_lock
	_            uint32 // alignment
	Tokens       uint64
	LastUpdateNs uint64
}

// Stat slot indices match the enum in bpf/natra.bpf.c.
const (
	StatPassed    uint32 = 0
	StatThrottled uint32 = 1
	StatHHHits    uint32 = 2
)

// Program holds a loaded natra BPF program plus its maps. One Program
// per attached veth — maps are not shared across pods because each pod
// has its own rate limit, so concurrent CNI ADDs each instantiate their
// own Program/Collection.
type Program struct {
	coll      *ebpf.Collection
	prog      *ebpf.Program
	configMap *ebpf.Map
	bucketMap *ebpf.Map
	statsMap  *ebpf.Map
	cmsMap    *ebpf.Map // CMS counters; nil with the placeholder program
	tcxLink   link.Link // set on AttachTCX, nil on AttachClsactPodside
}

// Load instantiates the embedded BPF object. Verification failures
// surface as `*ebpf.VerifierError`, which has a multi-line `.Error()`
// showing the rejected instruction. Caller closes via Close().
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
	tb := TokenBucket{Tokens: cfg.BurstBytes}
	if err := p.bucketMap.Update(&zero, &tb, ebpf.UpdateAny); err != nil {
		return fmt.Errorf("bucket map update: %w", err)
	}
	return nil
}

// PinMaps pins the program's maps under `dir`/<containerID>-<name>-map
// so `natra dump-stats` can read them out-of-band. Best-effort.
//
// No `.map` extension: bpffs forbids dots in pin file names —
// kernel/bpf/inode.c::bpf_lookup returns EPERM on any path component
// containing `.` when the parent dir has any S_IALLUGO bit set, since
// those names are reserved for populate_bpffs's internal special
// files.
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
		path := dir + "/" + containerID + "-" + name + "-map"
		if err := m.Pin(path); err != nil {
			return fmt.Errorf("pin %s: %w", path, err)
		}
	}
	return nil
}

// AttachIngress binds the program to ifIndex's ingress hook (packets
// arriving INTO the pod — the direction kubernetes.io/ingress-bandwidth
// describes). Caller should already be inside the pod's netns when the
// AttachClsactPodside mode is used; for AttachTCX the netns of the
// caller is what determines which veth the link binds to.
//
// pinPath is the bpffs path the tcx link gets pinned to so it survives
// the plugin's process exit. Ignored for AttachClsactPodside.
func (p *Program) AttachIngress(ifIndex int, mode AttachMode, pinPath string) error {
	switch mode {
	case AttachTCX:
		return p.attachTCX(ifIndex, pinPath)
	case AttachClsactPodside:
		return p.attachClsactPodside(ifIndex)
	default:
		return fmt.Errorf("unknown AttachMode %d", mode)
	}
}

func (p *Program) attachTCX(ifIndex int, pinPath string) error {
	l, err := link.AttachTCX(link.TCXOptions{
		Interface: ifIndex,
		Program:   p.prog,
		Attach:    ebpf.AttachTCXIngress,
	})
	if err != nil {
		return fmt.Errorf("AttachTCX ingress on ifindex %d: %w", ifIndex, err)
	}
	if pinPath != "" {
		if err := l.Pin(pinPath); err != nil {
			_ = l.Close()
			return fmt.Errorf("pin tcx link to %s: %w", pinPath, err)
		}
	}
	p.tcxLink = l
	return nil
}

// attachClsactPodside is the opt-in fallback. Adds a clsact qdisc to
// the link (idempotent), then attaches the BPF program via a tc filter
// on the ingress hook. Because the caller has already entered the pod's
// netns, this attaches to the pod-side end of the veth pair — host-side
// AWS VPC CNI clsact filters live in the host netns and don't see this.
func (p *Program) attachClsactPodside(ifIndex int) error {
	// Local name `nl` (not `link`) so we don't shadow the cilium/ebpf/link
	// import.
	nl, err := netlink.LinkByIndex(ifIndex)
	if err != nil {
		return fmt.Errorf("netlink LinkByIndex(%d): %w", ifIndex, err)
	}

	clsact := &netlink.GenericQdisc{
		QdiscAttrs: netlink.QdiscAttrs{
			LinkIndex: nl.Attrs().Index,
			Handle:    netlink.MakeHandle(0xffff, 0),
			Parent:    netlink.HANDLE_CLSACT,
		},
		QdiscType: "clsact",
	}
	if err := netlink.QdiscAdd(clsact); err != nil && !errors.Is(err, unix.EEXIST) {
		return fmt.Errorf("add clsact qdisc to ifindex %d: %w", ifIndex, err)
	}

	filter := &netlink.BpfFilter{
		FilterAttrs: netlink.FilterAttrs{
			LinkIndex: nl.Attrs().Index,
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
	return nil
}

// Close releases the userspace BPF references. For tcx, the kernel
// keeps the program loaded and attached as long as the pinned link
// exists, so packets keep flowing after Close. For clsact-podside the
// kernel similarly holds the program via the qdisc tree.
func (p *Program) Close() error {
	if p.tcxLink != nil {
		_ = p.tcxLink.Close()
		p.tcxLink = nil
	}
	if p.coll != nil {
		p.coll.Close()
		p.coll = nil
	}
	return nil
}
