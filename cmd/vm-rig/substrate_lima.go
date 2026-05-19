package main

import (
	"bytes"
	"context"
	"os/exec"

	"github.com/terraboops/natra/internal/perfrig"
)

// limaSubstrate is the lima/k3s impl of perfrig.Substrate. Wraps
// the rig's existing primitives (cmdUp / cmdDown / cmdInstall /
// importImage) so the shared executor can drive a real two-VM
// cross-kernel cluster.
type limaSubstrate struct {
	c *Config
}

// Compile-time interface check — a future drift between the
// interface and this impl fails the build right here.
var _ perfrig.Substrate = (*limaSubstrate)(nil)

func newLimaSubstrate(c *Config) *limaSubstrate { return &limaSubstrate{c: c} }

func (l *limaSubstrate) Name() string { return "vm-rig" }

func (l *limaSubstrate) KubeconfigPath() string { return l.c.KubeconfigPath }

func (l *limaSubstrate) Nodes() (string, string) {
	// lima sets the in-VM hostname to lima-<name>; k3s registers
	// nodes by hostname, so node names track the lima VM names.
	return "lima-" + l.c.ServerName, "lima-" + l.c.AgentName
}

func (l *limaSubstrate) Up(_ context.Context) error   { return cmdUp(l.c) }
func (l *limaSubstrate) Down(_ context.Context) error { return cmdDown(l.c) }

func (l *limaSubstrate) InstallNatra(_ context.Context) error { return cmdInstall(l.c) }

func (l *limaSubstrate) ImportImage(_ context.Context, image, dockerfile string) error {
	return importImage(l.c, image, dockerfile)
}

// NodeShell runs `limactl shell <vm> -- sudo sh -c <script>`,
// capturing combined stdout+stderr. The substrate-level shell is
// always rooted; per-script `sudo -u` switching is the script's
// problem.
func (l *limaSubstrate) NodeShell(ctx context.Context, node, script string) ([]byte, error) {
	// Node names are lima-<vm>; strip the prefix to get the lima
	// instance name. If it doesn't have the prefix (caller passed
	// the raw lima name), use it as-is.
	vm := node
	if len(vm) > 5 && vm[:5] == "lima-" {
		vm = vm[5:]
	}
	cmd := exec.CommandContext(ctx, "limactl", "shell", vm, "--", "sudo", "sh", "-c", script)
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	err := cmd.Run()
	return buf.Bytes(), err
}

// EnsureBpftool is a no-op on lima: the Debian trixie image used
// by the vm-rig already has `bpftool` in /usr/sbin via the
// linux-tools-common package the cloud-init drag-installs. If a
// future lima image drops it, this method becomes the install
// hook.
func (l *limaSubstrate) EnsureBpftool(_ context.Context, _ string) error { return nil }
