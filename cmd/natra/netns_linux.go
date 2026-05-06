//go:build linux

package main

import (
	"fmt"

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
