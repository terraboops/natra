//go:build linux

// Package bpf loads the embedded natra BPF object and attaches it to a
// pod-side veth in either or both directions. Two attach modes per
// direction:
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

// AttachMode selects how AttachIngress/AttachEgress wires the BPF
// program to a veth.
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

// Direction selects which BPF program / map slot to operate on.
// Numeric values must match the C enum in bpf/natra.bpf.c.
type Direction uint32

const (
	DirectionIngress Direction = 0
	DirectionEgress  Direction = 1
)

func (d Direction) String() string {
	switch d {
	case DirectionIngress:
		return "ingress"
	case DirectionEgress:
		return "egress"
	default:
		return fmt.Sprintf("direction(%d)", uint32(d))
	}
}

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

// Stat slot indices match the per-direction enum in bpf/natra.bpf.c.
// Userspace key into natra_stats_map = direction * StatPerDir + slot.
const (
	StatPassed    uint32 = 0
	StatThrottled uint32 = 1
	StatHHHits    uint32 = 2
	StatPerDir    uint32 = 3
)

// StatKey returns the natra_stats_map key for (direction, slot).
func StatKey(dir Direction, slot uint32) uint32 {
	return uint32(dir)*StatPerDir + slot
}

// Program holds a loaded natra BPF collection plus per-direction
// program handles and links. One Program per attached veth — maps are
// not shared across pods because each pod has its own rate limit, so
// concurrent CNI ADDs each instantiate their own Collection.
type Program struct {
	coll        *ebpf.Collection
	progIngress *ebpf.Program
	progEgress  *ebpf.Program
	configMap   *ebpf.Map
	bucketMap   *ebpf.Map
	statsMap    *ebpf.Map
	cmsMap      *ebpf.Map
	ingressLink link.Link // set on AttachIngress with AttachTCX, nil otherwise
	egressLink  link.Link // set on AttachEgress with AttachTCX, nil otherwise
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
		"natra_cms_map":    &p.cmsMap,
	} {
		m, ok := coll.Maps[name]
		if !ok {
			coll.Close()
			return nil, fmt.Errorf("map %q missing from BPF object (recompile?)", name)
		}
		*dst = m
	}

	for name, dst := range map[string]**ebpf.Program{
		"natra_ingress": &p.progIngress,
		"natra_egress":  &p.progEgress,
	} {
		prog, ok := coll.Programs[name]
		if !ok {
			coll.Close()
			return nil, fmt.Errorf("program %q missing from BPF object", name)
		}
		*dst = prog
	}
	return p, nil
}

// Configure writes the per-direction rate-limit configuration. Must be
// called AFTER Load and BEFORE the matching direction's program starts
// seeing traffic — otherwise the BPF code's fail-open path
// (rate_bps == 0 → TC_ACT_OK) lets traffic through unrate-limited until
// config arrives.
func (p *Program) Configure(dir Direction, cfg Config) error {
	key := uint32(dir)
	if err := p.configMap.Update(&key, &cfg, ebpf.UpdateAny); err != nil {
		return fmt.Errorf("config map update (%s): %w", dir, err)
	}
	tb := TokenBucket{Tokens: cfg.BurstBytes}
	if err := p.bucketMap.Update(&key, &tb, ebpf.UpdateAny); err != nil {
		return fmt.Errorf("bucket map update (%s): %w", dir, err)
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
//
// Maps are shared across both directions (one set per pod, indexed by
// direction internally). Pin names don't include direction.
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

// AttachIngress binds the ingress program to ifIndex (packets arriving
// INTO the pod — the direction kubernetes.io/ingress-bandwidth
// describes). Caller should already be inside the pod's netns. pinPath
// is the bpffs path the tcx link gets pinned to so it survives the
// plugin's process exit. Ignored for AttachClsactPodside.
func (p *Program) AttachIngress(ifIndex int, mode AttachMode, pinPath string) error {
	switch mode {
	case AttachTCX:
		l, err := p.attachTCX(ifIndex, p.progIngress, ebpf.AttachTCXIngress, pinPath, "ingress")
		if err != nil {
			return err
		}
		p.ingressLink = l
		return nil
	case AttachClsactPodside:
		return p.attachClsactPodside(ifIndex, p.progIngress, netlink.HANDLE_MIN_INGRESS, "natra_ingress")
	default:
		return fmt.Errorf("unknown AttachMode %d", mode)
	}
}

// AttachEgress binds the egress program to ifIndex (packets leaving
// the pod — the direction kubernetes.io/egress-bandwidth describes).
// Same callsite contract and pinPath semantics as AttachIngress.
func (p *Program) AttachEgress(ifIndex int, mode AttachMode, pinPath string) error {
	switch mode {
	case AttachTCX:
		l, err := p.attachTCX(ifIndex, p.progEgress, ebpf.AttachTCXEgress, pinPath, "egress")
		if err != nil {
			return err
		}
		p.egressLink = l
		return nil
	case AttachClsactPodside:
		return p.attachClsactPodside(ifIndex, p.progEgress, netlink.HANDLE_MIN_EGRESS, "natra_egress")
	default:
		return fmt.Errorf("unknown AttachMode %d", mode)
	}
}

func (p *Program) attachTCX(ifIndex int, prog *ebpf.Program, attach ebpf.AttachType, pinPath, label string) (link.Link, error) {
	l, err := link.AttachTCX(link.TCXOptions{
		Interface: ifIndex,
		Program:   prog,
		Attach:    attach,
	})
	if err != nil {
		return nil, fmt.Errorf("AttachTCX %s on ifindex %d: %w", label, ifIndex, err)
	}
	if pinPath != "" {
		if err := l.Pin(pinPath); err != nil {
			_ = l.Close()
			return nil, fmt.Errorf("pin tcx %s link to %s: %w", label, pinPath, err)
		}
	}
	return l, nil
}

// attachClsactPodside is the opt-in fallback. Adds a clsact qdisc to
// the link (idempotent — the second call for the other direction is a
// no-op via EEXIST), then attaches the BPF program via a tc filter on
// the requested parent (HANDLE_MIN_INGRESS or HANDLE_MIN_EGRESS).
// Because the caller has already entered the pod's netns, this
// attaches to the pod-side end of the veth pair — host-side AWS VPC
// CNI clsact filters live in the host netns and don't see this.
func (p *Program) attachClsactPodside(ifIndex int, prog *ebpf.Program, parent uint32, name string) error {
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
			Parent:    parent,
			Handle:    netlink.MakeHandle(0, 1),
			Protocol:  unix.ETH_P_ALL,
			Priority:  1,
		},
		Fd:           prog.FD(),
		Name:         name,
		DirectAction: true,
	}
	if err := netlink.FilterReplace(filter); err != nil {
		return fmt.Errorf("replace tc bpf filter %s on ifindex %d: %w", name, ifIndex, err)
	}
	return nil
}

// Close releases the userspace BPF references. For tcx, the kernel
// keeps each program loaded and attached as long as its pinned link
// exists, so packets keep flowing after Close. For clsact-podside the
// kernel similarly holds the programs via the qdisc tree.
func (p *Program) Close() error {
	if p.ingressLink != nil {
		_ = p.ingressLink.Close()
		p.ingressLink = nil
	}
	if p.egressLink != nil {
		_ = p.egressLink.Close()
		p.egressLink = nil
	}
	if p.coll != nil {
		p.coll.Close()
		p.coll = nil
	}
	return nil
}
