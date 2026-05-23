package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/terraboops/natra/internal/perfrig"
)

// k3dSubstrate runs the shared executor against a k3d cluster on
// the local docker daemon (colima on Mac, native on Linux). The
// k3d nodes are docker containers built from rancher/k3s images;
// NodeShell goes through `docker exec`, which is enough for every
// command the executor sends — provided PATH is widened to find
// k3s-bundled tools (tc, crictl) under
// /var/lib/rancher/k3s/data/current/bin/.
//
// Lifecycle equivalence:
//
//	limaSubstrate.Up           = limactl start (server) → start (agent)
//	k3dSubstrate.Up            = k3d cluster create --agents 1
//	limaSubstrate.NodeShell    = limactl shell <vm> -- sudo sh -c
//	k3dSubstrate.NodeShell     = docker exec <node> sh -c
//	limaSubstrate.EnsureBpftool = no-op (provision apt-installs it)
//	k3dSubstrate.EnsureBpftool = docker cp a static binary into the node
type k3dSubstrate struct {
	cluster         string
	natraImage      string
	perfclientImage string
	repoRoot        string

	// bpftoolHost is set by EnsureBpftool's first call: the path to
	// a cached static bpftool binary on disk. Reused across
	// per-phase EnsureBpftool calls so the download only happens
	// once per `perfrig` invocation.
	bpftoolHost string
}

var _ perfrig.Substrate = (*k3dSubstrate)(nil)

func newK3dSubstrate(cluster, natraImage, perfclientImage, repoRoot string) *k3dSubstrate {
	return &k3dSubstrate{
		cluster:         cluster,
		natraImage:      natraImage,
		perfclientImage: perfclientImage,
		repoRoot:        repoRoot,
	}
}

func (k *k3dSubstrate) Name() string { return "k3d" }

func (k *k3dSubstrate) KubeconfigPath() string {
	// k3d kubeconfig write returns the path; we mirror the bash
	// rig's convention so the file is stable across phases.
	return fmt.Sprintf("/tmp/natra-perfrig-%s.kubeconfig", k.cluster)
}

func (k *k3dSubstrate) Nodes() (server, worker string) {
	return "k3d-" + k.cluster + "-server-0", "k3d-" + k.cluster + "-agent-0"
}

// Up creates the cluster, writes its kubeconfig, and enables ECN
// on every node so iperf3 + nginx can negotiate ECN-capable TCP
// (natra's bpf_skb_ecn_set_ce path needs this to surface).
func (k *k3dSubstrate) Up(ctx context.Context) error {
	// Idempotent pre-clean: k3d cluster create fails if the
	// cluster already exists, but a stale one from a half-aborted
	// run is exactly the situation Up should recover from.
	_ = k.deleteCluster(ctx)

	// One control-plane + one agent, no load balancer, flannel
	// host-gw. host-gw avoids vxlan overhead on the shared kernel.
	// --wait blocks k3d until the apiserver responds, so kubeconfig
	// write is safe immediately after.
	args := []string{
		"cluster", "create", k.cluster,
		"--agents", "1",
		"--no-lb",
		"--k3s-arg", "--disable=traefik,servicelb@server:*",
		"--k3s-arg", "--flannel-backend=host-gw@server:*",
		"--wait",
	}
	if out, err := captureCmd(ctx, "k3d", args...); err != nil {
		return fmt.Errorf("k3d cluster create: %w\n%s", err, out)
	}

	// Persist the kubeconfig at the path KubeconfigPath() advertises.
	if out, err := captureCmd(ctx, "k3d", "kubeconfig", "write", k.cluster, "--output", k.KubeconfigPath()); err != nil {
		return fmt.Errorf("k3d kubeconfig write: %w\n%s", err, out)
	}

	// Enable ECN on every node's root netns. Best-effort: failures
	// log but don't abort — natra still drops above-rate non-ECN
	// traffic, just doesn't ECN-mark it.
	server, worker := k.Nodes()
	for _, n := range []string{server, worker} {
		if _, err := captureCmd(ctx, "docker", "exec", n, "sysctl", "-w", "net.ipv4.tcp_ecn=1"); err != nil {
			fmt.Fprintf(os.Stderr, "warn: tcp_ecn=1 on %s failed: %v\n", n, err)
		}
	}

	// Load ifb in the underlying kernel — the upstream bandwidth
	// plugin uses HTB on an IFB device for ingress shaping, and the
	// rancher/k3s image ships the module but doesn't auto-load it.
	// On Mac the underlying kernel is the colima VM; on native
	// Linux it's the host. Best-effort: the warning surfaces but
	// the run continues (only the vanilla phase needs ifb).
	loadIFBInUnderlyingKernel(ctx)
	return nil
}

// loadIFBInUnderlyingKernel modprobes ifb in the kernel that
// actually runs the k3d node containers. The kernel module ships
// with the host distribution; the k3s node container has /lib/
// modules empty and can't load it from inside.
func loadIFBInUnderlyingKernel(ctx context.Context) {
	if _, err := exec.LookPath("colima"); err == nil {
		if _, err := captureCmd(ctx, "colima", "status"); err == nil {
			if out, err := captureCmd(ctx, "colima", "ssh", "--", "sudo", "modprobe", "ifb"); err != nil {
				fmt.Fprintf(os.Stderr, "warn: modprobe ifb on colima failed: %v\n%s", err, out)
			}
			return
		}
	}
	if runtime.GOOS == "linux" {
		if out, err := captureCmd(ctx, "sudo", "modprobe", "ifb"); err != nil {
			fmt.Fprintf(os.Stderr, "warn: modprobe ifb on host failed: %v\n%s", err, out)
		}
	}
}

func (k *k3dSubstrate) Down(ctx context.Context) error { return k.deleteCluster(ctx) }

func (k *k3dSubstrate) deleteCluster(ctx context.Context) error {
	// `k3d cluster delete` returns 0 when the cluster doesn't
	// exist (since modern k3d versions); older versions return 1.
	// Either way we swallow — Down is idempotent by contract.
	_, _ = captureCmd(ctx, "k3d", "cluster", "delete", k.cluster)
	return nil
}

// InstallNatra builds the natra image on the host, imports it into
// the cluster, and applies the installer DaemonSet. Mirrors
// cmd/vm-rig's cmdInstall but uses k3d image import instead of
// limactl copy.
func (k *k3dSubstrate) InstallNatra(ctx context.Context) error {
	if err := k.ImportImage(ctx, k.natraImage, "Dockerfile.cni"); err != nil {
		return fmt.Errorf("import natra image: %w", err)
	}
	// Render the installer manifest with the local image tag and
	// pull policy. The on-disk manifest has the production ghcr
	// tag + IfNotPresent; we substitute to the local tag + Never
	// so the cluster uses our just-imported image.
	manifest, err := os.ReadFile(filepath.Join(k.repoRoot, "deploy", "cni-installer.yaml"))
	if err != nil {
		return fmt.Errorf("read installer manifest: %w", err)
	}
	rewritten := strings.NewReplacer(
		"ghcr.io/terraboops/natra:latest", k.natraImage,
		"imagePullPolicy: IfNotPresent", "imagePullPolicy: Never",
	).Replace(string(manifest))

	cmd := exec.CommandContext(ctx, "kubectl", "apply", "-f", "-")
	cmd.Env = append(os.Environ(), "KUBECONFIG="+k.KubeconfigPath())
	cmd.Stdin = strings.NewReader(rewritten)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("apply installer: %w", err)
	}

	if out, err := captureCmd(ctx, "kubectl", "--kubeconfig", k.KubeconfigPath(),
		"-n", "kube-system", "rollout", "status", "ds/natra-installer", "--timeout=180s"); err != nil {
		return fmt.Errorf("installer rollout: %w\n%s", err, out)
	}
	return nil
}

// ImportImage builds the image from the named Dockerfile under
// deploy/docker/ and imports the resulting local tag into the
// k3d cluster.
func (k *k3dSubstrate) ImportImage(ctx context.Context, image, dockerfile string) error {
	dockerfilePath := filepath.Join(k.repoRoot, "deploy", "docker", dockerfile)
	if out, err := captureCmd(ctx, "docker", "build", "-q", "-t", image, "-f", dockerfilePath, k.repoRoot); err != nil {
		return fmt.Errorf("docker build %s: %w\n%s", image, err, out)
	}
	if out, err := captureCmd(ctx, "k3d", "image", "import", image, "--cluster", k.cluster); err != nil {
		return fmt.Errorf("k3d image import %s: %w\n%s", image, err, out)
	}
	return nil
}

// NodeShell runs `docker exec <node> sh -c <script>` with PATH
// extended to find k3s-bundled tools — tc and crictl live under
// /var/lib/rancher/k3s/data/current/bin/ and aren't on the
// default PATH. The TBF burst patch in particular needs tc, which
// would otherwise fail silently from this NodeShell.
func (k *k3dSubstrate) NodeShell(ctx context.Context, node, script string) ([]byte, error) {
	wrapped := `export PATH=/var/lib/rancher/k3s/data/current/bin:/usr/local/bin:$PATH; ` + script
	cmd := exec.CommandContext(ctx, "docker", "exec", node, "sh", "-c", wrapped)
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	err := cmd.Run()
	return buf.Bytes(), err
}

// EnsureBpftool downloads a static bpftool release once per
// perfrig run, then docker-cps it into the named node. rancher/k3s
// images don't ship bpftool and can't apt-install it (linux-tools
// wants a kernel package the LinuxKit kernel doesn't have).
const (
	bpftoolVersion = "v7.7.0"
	bpftoolURLBase = "https://github.com/libbpf/bpftool/releases/download"
)

func (k *k3dSubstrate) EnsureBpftool(ctx context.Context, node string) error {
	if err := k.cacheBpftoolBinary(ctx); err != nil {
		return fmt.Errorf("cache bpftool: %w", err)
	}
	if k.bpftoolHost == "" {
		return nil // best-effort: cache failed, executor sees zero
	}
	// /usr/local/bin doesn't exist on k3s nodes by default; create
	// it before docker cp.
	_, _ = captureCmd(ctx, "docker", "exec", node, "mkdir", "-p", "/usr/local/bin")
	if out, err := captureCmd(ctx, "docker", "cp", k.bpftoolHost, node+":/usr/local/bin/bpftool"); err != nil {
		return fmt.Errorf("docker cp bpftool to %s: %w\n%s", node, err, out)
	}
	_, _ = captureCmd(ctx, "docker", "exec", node, "chmod", "0755", "/usr/local/bin/bpftool")
	return nil
}

// cacheBpftoolBinary downloads + extracts the static bpftool
// release once and caches it under bin/ in the repo. Sets
// k.bpftoolHost on success.
func (k *k3dSubstrate) cacheBpftoolBinary(ctx context.Context) error {
	if k.bpftoolHost != "" {
		if _, err := os.Stat(k.bpftoolHost); err == nil {
			return nil
		}
	}
	arch := ""
	switch runtime.GOARCH {
	case "arm64":
		arch = "arm64"
	case "amd64":
		arch = "amd64"
	default:
		return fmt.Errorf("unsupported GOARCH %q for bpftool release", runtime.GOARCH)
	}
	cache := filepath.Join(k.repoRoot, "bin", fmt.Sprintf("bpftool-%s-%s", bpftoolVersion, arch))
	if _, err := os.Stat(cache); err == nil {
		k.bpftoolHost = cache
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(cache), 0o755); err != nil {
		return err
	}
	url := fmt.Sprintf("%s/%s/bpftool-%s-%s.tar.gz", bpftoolURLBase, bpftoolVersion, bpftoolVersion, arch)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	client := &http.Client{Timeout: 60 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("download bpftool: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download bpftool: HTTP %d from %s", resp.StatusCode, url)
	}
	// The release is a single-file tarball with `bpftool` at the
	// archive root. Extract via shelling out to `tar`; reimplementing
	// gzip+tar in Go adds dependencies for one binary's worth of
	// gain.
	tmp, err := os.CreateTemp("", "bpftool-*.tar.gz")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if _, err := io.Copy(tmp, resp.Body); err != nil {
		return fmt.Errorf("write bpftool tarball: %w", err)
	}
	_ = tmp.Close()
	extractDir, err := os.MkdirTemp("", "bpftool-extract-")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(extractDir) }()
	if out, err := captureCmd(ctx, "tar", "-xzf", tmp.Name(), "-C", extractDir); err != nil {
		return fmt.Errorf("extract bpftool: %w\n%s", err, out)
	}
	src := filepath.Join(extractDir, "bpftool")
	if _, err := os.Stat(src); err != nil {
		return fmt.Errorf("bpftool archive missing binary: %w", err)
	}
	if err := os.Rename(src, cache); err != nil {
		// Cross-device rename — fall back to copy.
		if err := copyFile(src, cache); err != nil {
			return err
		}
	}
	if err := os.Chmod(cache, 0o755); err != nil {
		return err
	}
	k.bpftoolHost = cache
	fmt.Printf("==> bpftool cached at %s\n", cache)
	return nil
}

func copyFile(src, dst string) error {
	in, err := os.Open(src) //nolint:gosec // src is our own tmp file
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	out, err := os.Create(dst) //nolint:gosec // dst is our own cache path
	if err != nil {
		return err
	}
	defer func() { _ = out.Close() }()
	_, err = io.Copy(out, in)
	return err
}

// captureCmd runs a command and returns its combined output. Used
// for one-shot CLI invocations (k3d, docker, kubectl) where we
// want the output for error context, not streamed to stdout.
func captureCmd(ctx context.Context, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	err := cmd.Run()
	return buf.String(), err
}
