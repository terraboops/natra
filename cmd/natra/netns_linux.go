//go:build linux

package main

import (
	"fmt"
	"runtime"

	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
)

// enterNetns moves the calling OS thread into the namespace at path
// and returns a restore function that switches the thread back. The
// caller has to runtime.LockOSThread() first — the netns switch is
// per-thread, and a goroutine migration mid-flow would land in the
// wrong namespace.
//
// The restore matters: CNI's skel framework checks after the plugin
// returns and exits 1 if the plugin's final netns equals CNI_NETNS.
// Skipping the restore would fail every CNI ADD that touches BPF
// regardless of what stdout said.
func enterNetns(path string) (func(), error) {
	origin, err := netns.Get()
	if err != nil {
		return nil, fmt.Errorf("get origin netns: %w", err)
	}
	target, err := netns.GetFromPath(path)
	if err != nil {
		_ = origin.Close()
		return nil, fmt.Errorf("open netns %s: %w", path, err)
	}
	if err := netns.Set(target); err != nil {
		_ = target.Close()
		_ = origin.Close()
		return nil, fmt.Errorf("set netns %s: %w", path, err)
	}
	restore := func() {
		_ = netns.Set(origin)
		_ = target.Close()
		_ = origin.Close()
	}
	return restore, nil
}

// installFQ replaces the root qdisc on `ifIndex` with `fq` so EDT
// timestamps set by natra's BPF program are honored. Idempotent: if
// the root qdisc is already `fq`, no change. The kernel's default
// for veth is `noqueue`, which ignores skb->tstamp — natra has to
// install fq itself or EDT pacing silently breaks (packets transmit
// at line rate regardless of timestamp).
//
// Caller is responsible for being in the right netns. Pod-side
// attach: call inside the pod netns so this acts on pod-eth0.
func installFQ(ifIndex int) error {
	link, err := netlink.LinkByIndex(ifIndex)
	if err != nil {
		return fmt.Errorf("LinkByIndex(%d): %w", ifIndex, err)
	}

	// Skip the replace if fq is already there. QdiscReplace is
	// idempotent but the no-op still triggers a netlink round-trip
	// per CNI ADD; this short-circuit keeps the hot path fast for
	// pod churn.
	existing, err := netlink.QdiscList(link)
	if err == nil {
		for _, q := range existing {
			if q.Attrs().Parent == netlink.HANDLE_ROOT && q.Type() == "fq" {
				return nil
			}
		}
	}

	fq := &netlink.GenericQdisc{
		QdiscAttrs: netlink.QdiscAttrs{
			LinkIndex: ifIndex,
			Handle:    netlink.MakeHandle(0x1, 0),
			Parent:    netlink.HANDLE_ROOT,
		},
		QdiscType: "fq",
	}
	if err := netlink.QdiscReplace(fq); err != nil {
		return fmt.Errorf("QdiscReplace fq on ifindex %d: %w", ifIndex, err)
	}
	return nil
}

// hostsidePeerIfIndex finds the kernel ifindex of the host-side veth
// peer of the pod's ifName interface (typically eth0). Briefly enters
// the pod netns to read the peer index via netlink, then restores the
// thread's netns so subsequent code (the actual BPF attach) runs in
// the host netns.
//
// Same approach Cilium's generic-veth chaining mode uses: enter pod
// netns, look up eth0, read peer ifindex from the veth link, exit.
// Works regardless of how the base CNI named the host-side veth.
func hostsidePeerIfIndex(netnsPath, ifName string) (int, error) {
	// Caller is expected to have locked the OS thread. Lock again
	// defensively in case this function is called outside attachBPF's
	// flow; runtime.LockOSThread is idempotent on the same thread.
	runtime.LockOSThread()

	restore, err := enterNetns(netnsPath)
	if err != nil {
		return 0, fmt.Errorf("enter pod netns %s: %w", netnsPath, err)
	}
	defer restore()

	link, err := netlink.LinkByName(ifName)
	if err != nil {
		return 0, fmt.Errorf("netlink LinkByName(%s) in pod netns: %w", ifName, err)
	}
	veth, ok := link.(*netlink.Veth)
	if !ok {
		return 0, fmt.Errorf("link %s is not a veth (type=%s) — host-side attach needs a veth pair", ifName, link.Type())
	}
	peerIdx, err := netlink.VethPeerIndex(veth)
	if err != nil {
		return 0, fmt.Errorf("VethPeerIndex(%s): %w", ifName, err)
	}
	return peerIdx, nil
}
