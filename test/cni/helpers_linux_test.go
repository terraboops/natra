//go:build linux && integration

// Helpers for Layer 2 CNI protocol tests. Network-namespace lifecycle,
// natra-binary location, CNI env-var construction. Pulled out so the
// test files (cni_linux_test.go, chaos_linux_test.go) stay declarative.

package cni_test

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"

	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
	"golang.org/x/sys/unix"
)

// natraBinary returns the absolute path to the natra binary built by
// `make build`. Tests fail loudly if it isn't there — that's a setup bug,
// not a per-test failure.
func natraBinary() (string, error) {
	_, thisFile, _, _ := runtime.Caller(0)
	repoRoot := filepath.Join(filepath.Dir(thisFile), "..", "..")
	bin := filepath.Join(repoRoot, "bin", "natra")
	if _, err := os.Stat(bin); err != nil {
		return "", fmt.Errorf("natra binary not found at %s: run `make build` first: %w", bin, err)
	}
	return bin, nil
}

// newTestNetns creates a fresh network namespace and returns its handle plus
// a cleanup func. The caller must defer cleanup() to avoid leaking netns.
//
// netns.New() switches the calling OS thread into the new namespace as a
// side effect. We immediately switch back to origin so child processes
// (the natra binary we exec) inherit the original namespace — otherwise
// CNI_NETNS would point at the plugin's *own* netns and CNI's skel
// framework rejects that as code 8 ("plugin's netns and netns from
// CNI_NETNS should not be the same").
func newTestNetns() (netns.NsHandle, func(), error) {
	origin, err := netns.Get()
	if err != nil {
		return 0, nil, fmt.Errorf("get current netns: %w", err)
	}
	newNs, err := netns.New()
	if err != nil {
		_ = origin.Close()
		return 0, nil, fmt.Errorf("new netns: %w", err)
	}
	if err := netns.Set(origin); err != nil {
		_ = newNs.Close()
		_ = origin.Close()
		return 0, nil, fmt.Errorf("restore origin netns: %w", err)
	}
	cleanup := func() {
		_ = newNs.Close()
		_ = origin.Close()
	}
	return newNs, cleanup, nil
}

// netnsPath returns the /proc-style path that CNI plugins expect in
// CNI_NETNS, given a netns handle.
func netnsPath(h netns.NsHandle) string {
	return fmt.Sprintf("/proc/%d/fd/%d", os.Getpid(), int(h))
}

// newTestNetnsWithVeth creates a fresh netns and pulls a veth pair end
// into it under the requested name, so a CNI plugin entering the netns
// finds an interface to attach BPF to. Cleanup deletes the host end
// (which auto-deletes the peer in the netns).
//
// Both ends are created with unique tags in the host netns (avoids
// colliding with the container's existing eth0), then the peer is
// moved into the pod netns and renamed to ifName via a netlink handle
// scoped to that netns. No thread-level netns switching needed.
func newTestNetnsWithVeth(ifName string) (netns.NsHandle, func(), error) {
	ns, baseCleanup, err := newTestNetns()
	if err != nil {
		return 0, nil, err
	}
	tag := fmt.Sprintf("%d-%d", os.Getpid(), int(ns))
	hostEnd := "h" + tag
	tmpPeer := "p" + tag
	if len(hostEnd) > 15 {
		hostEnd = hostEnd[:15]
	}
	if len(tmpPeer) > 15 {
		tmpPeer = tmpPeer[:15]
	}

	veth := &netlink.Veth{
		LinkAttrs: netlink.LinkAttrs{Name: hostEnd},
		PeerName:  tmpPeer,
	}
	if err := netlink.LinkAdd(veth); err != nil {
		baseCleanup()
		return 0, nil, fmt.Errorf("netlink add veth %s/%s: %w", hostEnd, tmpPeer, err)
	}
	cleanup := func() {
		_ = netlink.LinkDel(veth)
		baseCleanup()
	}

	peer, err := netlink.LinkByName(tmpPeer)
	if err != nil {
		cleanup()
		return 0, nil, fmt.Errorf("find peer %s in host netns: %w", tmpPeer, err)
	}
	if err := netlink.LinkSetNsFd(peer, int(ns)); err != nil {
		cleanup()
		return 0, nil, fmt.Errorf("move %s into netns: %w", tmpPeer, err)
	}

	// Operate on the pod netns via a scoped netlink handle — no thread
	// migration. Rename the moved-in peer to the requested name (e.g.
	// "eth0") and bring it up so the BPF program can attach.
	nlh, err := netlink.NewHandleAt(ns)
	if err != nil {
		cleanup()
		return 0, nil, fmt.Errorf("netlink handle for pod netns: %w", err)
	}
	defer nlh.Close()
	movedPeer, err := nlh.LinkByName(tmpPeer)
	if err != nil {
		cleanup()
		return 0, nil, fmt.Errorf("find peer %s in pod netns: %w", tmpPeer, err)
	}
	if err := nlh.LinkSetName(movedPeer, ifName); err != nil {
		cleanup()
		return 0, nil, fmt.Errorf("rename %s to %s in pod netns: %w", tmpPeer, ifName, err)
	}
	if err := nlh.LinkSetUp(movedPeer); err != nil {
		cleanup()
		return 0, nil, fmt.Errorf("set %s up in pod netns: %w", ifName, err)
	}
	return ns, cleanup, nil
}

// runPlugin executes the natra binary with the canonical CNI env vars and
// stdin, returning stdout, stderr, and any exec error.
func runPlugin(bin, command, containerID, netnsPath, ifName string, stdin []byte) (stdout, stderr []byte, err error) {
	cmd := exec.Command(bin)
	cmd.Env = append(os.Environ(),
		"CNI_COMMAND="+command,
		"CNI_CONTAINERID="+containerID,
		"CNI_NETNS="+netnsPath,
		"CNI_IFNAME="+ifName,
		"CNI_PATH=/opt/cni/bin",
		"CNI_ARGS=",
	)
	cmd.Stdin = bytes.NewReader(stdin)
	var out, errBuf bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errBuf
	err = cmd.Run()
	return out.Bytes(), errBuf.Bytes(), err
}

// requireRoot fails the test if not running as root (CAP_NET_ADMIN required
// for netns operations). Used in BeforeEach to give a clear error rather
// than a confusing EPERM later.
func requireRoot() error {
	if unix.Geteuid() != 0 {
		return fmt.Errorf("Layer 2 tests require root (need CAP_NET_ADMIN); current euid=%d", unix.Geteuid())
	}
	return nil
}

// remainingPinsFor returns the entries in /sys/fs/bpf/natra/ whose name
// starts with the given containerID. Used after a CNI DEL to assert
// that nothing leaked. Empty slice if the pin dir doesn't exist.
//
// Uses ReadDir rather than per-file stat because bpffs returns EPERM
// (not ENOENT) on stat of a non-existent pin file, which makes
// os.IsNotExist unreliable.
func remainingPinsFor(containerID string) []string {
	const pinDir = "/sys/fs/bpf/natra"
	entries, err := os.ReadDir(pinDir)
	if err != nil {
		return nil
	}
	prefix := containerID + "-"
	var out []string
	for _, e := range entries {
		if len(e.Name()) >= len(prefix) && e.Name()[:len(prefix)] == prefix {
			out = append(out, e.Name())
		}
	}
	return out
}

// linkPinExists reports whether /sys/fs/bpf/natra/<containerID>-<ifName>.link
// is present. Uses ReadDir + name match because bpffs returns EPERM
// (not ENOENT) on stat of a non-existent file.
func linkPinExists(containerID, ifName string) bool {
	target := containerID + "-" + ifName + ".link"
	for _, name := range remainingPinsFor(containerID) {
		if name == target {
			return true
		}
	}
	return false
}
