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
