//go:build linux

// Package bpf loads the embedded natra BPF object and attaches it to a
// veth in either or both directions. The attach is parameterized by
// two orthogonal choices:
//
//   - Hook: TCX (kernel 6.6+, bpf_mprog) or Clsact (5.x+, classic
//     filter on a clsact qdisc).
//   - Side: HostSide (peer of pod's eth0, in the host netns) or
//     PodSide (eth0 inside the pod netns).
//
// The four combinations are independently selectable. natra defaults
// to attachMode=auto, which expands into an ordered fallback chain
// whose head depends on the resolved EDT mode — pod-side first when
// EDT is enabled, host-side first when EDT is off (host-side matches
// how Cilium and the AWS network-policy-agent attach). Pod-side
// modes are useful when the host netns is locked down or when
// host-side attach would collide with another BPF stack. See
// cmd/natra/main.go::resolveAttachStrategy for the full chains.
//
// The natra BPF program is symmetric per direction (`natra_ingress`
// processes packets in the pod-ingress direction regardless of which
// veth-half the program sits on). On the host side the hook direction
// is the opposite of the pod direction: a packet leaving the pod
// arrives at the host-side veth's ingress hook (TCX_INGRESS or
// HANDLE_MIN_INGRESS), and vice versa. The loader handles that flip
// internally so the caller specifies one pod-direction.
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

// Hook selects the kernel hook surface used for attach.
type Hook int

const (
	// HookTCX uses cilium/ebpf link.AttachTCX. Requires kernel 6.6+
	// and a bpffs mount. Multiple TCX programs can coexist on the
	// same hook in a defined order — composes cleanly with other
	// BPF stacks (cilium, aws-network-policy-agent in newer versions).
	HookTCX Hook = iota
	// HookClsact uses a clsact qdisc plus a tc bpf filter. Works on
	// 5.x+ and has no bpffs requirement. Multiple programs on the
	// same hook need priority discipline.
	HookClsact
)

func (h Hook) String() string {
	switch h {
	case HookTCX:
		return "tcx"
	case HookClsact:
		return "clsact"
	default:
		return fmt.Sprintf("hook(%d)", int(h))
	}
}

// Side selects which half of the veth pair to attach to.
type Side int

const (
	// SideHost is the host-side veth (peer of pod's eth0, lives in
	// the host netns). Same attach point Cilium and NPA use.
	SideHost Side = iota
	// SidePod is the pod-side veth (eth0 inside the pod netns).
	SidePod
)

func (s Side) String() string {
	switch s {
	case SideHost:
		return "hostside"
	case SidePod:
		return "podside"
	default:
		return fmt.Sprintf("side(%d)", int(s))
	}
}

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
// order and types must match the C side exactly. EDTPacing is the
// runtime opt-in: 0 = ECN-mark-or-drop (safe everywhere), non-zero
// = EDT pacing first on egress, else ECN-mark on ECN-capable, else
// drop. EDT requires fq qdisc downstream of natra's attach point;
// without it packets pass at line rate and the rate limit silently
// breaks. Operators opt in per-cluster via NATRA_EDT_PACING=1 on
// the installer DS.
type Config struct {
	RateBps     uint64
	BurstBytes  uint64
	HHThreshold uint64
	EDTPacing   uint64
}

// TokenBucket mirrors `struct token_bucket`. The first u32 is the
// kernel's bpf_spin_lock; the second is alignment padding before the
// 8-byte fields. Both are kernel-managed — userspace must include
// them in the layout but never reads them. NextReleaseNs is the EDT
// pacing cursor; the BPF program advances it past now+packet-delay
// each time it stamps skb->tstamp for an above-rate egress packet.
type TokenBucket struct {
	_             uint32 // bpf_spin_lock
	_             uint32 // alignment
	Tokens        uint64
	LastUpdateNs  uint64
	NextReleaseNs uint64
}

// Stat slot indices match the per-direction enum in bpf/natra.bpf.c.
// Userspace key into natra_stats_map = direction * StatPerDir + slot.
//
// StatThrottled is the cardinality of all bucket-overflow events;
// the three disposition slots break it down by what natra actually
// did with each overflow packet (ECN-mark, EDT-delay, drop) — their
// sum equals StatThrottled.
const (
	StatPassed     uint32 = 0
	StatThrottled  uint32 = 1
	StatHHHits     uint32 = 2
	StatECNMarked  uint32 = 3
	StatEDTDelayed uint32 = 4
	StatDropped    uint32 = 5
	StatPerDir     uint32 = 6
)

// StatKey returns the natra_stats_map key for (direction, slot).
func StatKey(dir Direction, slot uint32) uint32 {
	return uint32(dir)*StatPerDir + slot
}

// AttachOptions parameterizes a single per-direction attach.
type AttachOptions struct {
	// Direction is the pod-direction (ingress = packets going INTO
	// the pod). Selects which BPF program (natra_ingress or
	// natra_egress) and which map slot to use.
	Direction Direction
	// Side is the veth half to attach on. The loader maps pod
	// direction to kernel hook direction based on this.
	Side Side
	// Hook is the kernel attach API (TCX or clsact).
	Hook Hook
	// IfIndex is the kernel ifindex of the chosen veth side. The
	// caller resolves it — host-side via netlink.VethPeerIndex from
	// the pod-side, pod-side via net.InterfaceByName inside the pod
	// netns.
	IfIndex int
	// PinPath is the bpffs path the tcx link gets pinned to so it
	// outlives the CNI plugin process. Ignored for HookClsact (the
	// kernel keeps the program loaded via the qdisc tree).
	PinPath string
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
	ingressLink link.Link // set when DirectionIngress attaches via HookTCX
	egressLink  link.Link // set when DirectionEgress attaches via HookTCX
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

// Attach binds one program direction to the chosen (hook, side,
// ifindex). For host-side attach, the kernel hook direction is the
// opposite of opts.Direction (a pod-ingress packet shows up at the
// host-side veth's egress hook); the loader flips that internally.
//
// pinPath only matters for HookTCX. For HookClsact the kernel holds
// the program reference via the qdisc tree until the veth is deleted.
//
// Caller is responsible for being in the right netns: SidePod requires
// the pod netns, SideHost requires the host netns. The loader does
// not switch netns.
func (p *Program) Attach(opts AttachOptions) error {
	prog := p.programFor(opts.Direction)
	hookDir := opts.Direction
	if opts.Side == SideHost {
		// pod-ingress = host-side egress; pod-egress = host-side ingress
		if hookDir == DirectionIngress {
			hookDir = DirectionEgress
		} else {
			hookDir = DirectionIngress
		}
	}

	switch opts.Hook {
	case HookTCX:
		attach := ebpf.AttachTCXIngress
		if hookDir == DirectionEgress {
			attach = ebpf.AttachTCXEgress
		}
		l, err := p.attachTCX(opts.IfIndex, prog, attach, opts.PinPath, opts.Direction.String())
		if err != nil {
			return err
		}
		if opts.Direction == DirectionIngress {
			p.ingressLink = l
		} else {
			p.egressLink = l
		}
		return nil
	case HookClsact:
		parent := uint32(netlink.HANDLE_MIN_INGRESS)
		if hookDir == DirectionEgress {
			parent = uint32(netlink.HANDLE_MIN_EGRESS)
		}
		return p.attachClsact(opts.IfIndex, prog, parent, "natra_"+opts.Direction.String())
	default:
		return fmt.Errorf("unknown Hook %d", opts.Hook)
	}
}

func (p *Program) programFor(dir Direction) *ebpf.Program {
	if dir == DirectionEgress {
		return p.progEgress
	}
	return p.progIngress
}

func (p *Program) attachTCX(
	ifIndex int,
	prog *ebpf.Program,
	attach ebpf.AttachType,
	pinPath, label string,
) (link.Link, error) {
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

// attachClsact installs a clsact qdisc on the link (idempotent) and
// then attaches the BPF program via a tc filter on the requested
// parent (HANDLE_MIN_INGRESS or HANDLE_MIN_EGRESS).
func (p *Program) attachClsact(ifIndex int, prog *ebpf.Program, parent uint32, name string) error {
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

// Close releases the userspace BPF references. For HookTCX the kernel
// keeps each program loaded and attached as long as its pinned link
// exists, so packets keep flowing after Close. For HookClsact the
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
