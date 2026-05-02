//go:build linux

package main

import (
	"fmt"

	"github.com/vishvananda/netns"
)

// enterNetns moves the calling OS thread into the namespace at `path`
// and returns a `restore` function that switches the thread back to its
// origin namespace. Caller must runtime.LockOSThread() first; the
// namespace switch is per-thread and goroutine migration would
// otherwise corrupt state.
//
// The restore is critical: CNI's skel framework runs a post-flight
// check that exits 1 if the plugin's final netns equals CNI_NETNS. If
// we entered the pod netns and never came back, every CNI ADD that
// touches BPF would fail at the protocol layer, regardless of stdout.
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
