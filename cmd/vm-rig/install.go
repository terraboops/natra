package main

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// cmdInstall builds the natra container image on the host, copies
// it into both VMs as a tarball, imports each into k3s's embedded
// containerd, then applies the installer DaemonSet.
func cmdInstall(c *Config) error {
	if _, err := os.Stat(c.KubeconfigPath); err != nil {
		return fmt.Errorf("%s not found — run 'vm-rig up' first", c.KubeconfigPath)
	}

	// Build and push both images. natra is the CNI plugin DaemonSet
	// image; perfclient bundles iperf3 + hey for the test phases
	// (iperf3 elephant throttle, hey HTTP-mice fast-pass).
	for _, img := range []struct {
		tag, dockerfile, label string
	}{
		{c.NatraImage, "Dockerfile.cni", "natra"},
		{c.PerfclientImage, "Dockerfile.perfclient", "perfclient"},
	} {
		fmt.Printf("==> building %s image (%s)\n", img.label, img.tag)
		if err := run("docker", "build", "-q", "-t", img.tag,
			"-f", filepath.Join(c.RepoRoot, "deploy", "docker", img.dockerfile),
			c.RepoRoot); err != nil {
			return err
		}
	}

	// Export each image to a tar, copy to both VMs, import into the
	// k3s-embedded containerd. The same temp tarball is reused per
	// image — one round-trip per (image × VM).
	tarFile := "/tmp/natra-vm-rig.tar"
	defer func() { _ = os.Remove(tarFile) }()
	for _, img := range []string{c.NatraImage, c.PerfclientImage} {
		fmt.Printf("==> exporting %s tarball\n", img)
		if err := run("docker", "save", "-o", tarFile, img); err != nil {
			return err
		}
		for _, vm := range []string{c.ServerName, c.AgentName} {
			fmt.Printf("==> copying image to %s\n", vm)
			if err := run("limactl", "copy", tarFile, vm+":/tmp/natra-vm-rig.tar"); err != nil {
				return err
			}
			fmt.Printf("==> importing image in %s\n", vm)
			if err := run("limactl", "shell", vm, "--",
				"sudo", "k3s", "ctr", "-n", "k8s.io", "images", "import",
				"/tmp/natra-vm-rig.tar"); err != nil {
				return err
			}
		}
	}

	fmt.Println("==> applying installer DaemonSet")
	rendered, err := renderInstallerManifest(c)
	if err != nil {
		return err
	}
	if err := kubectl(
		[]string{"KUBECONFIG=" + c.KubeconfigPath},
		strings.NewReader(rendered),
		"apply", "-f", "-"); err != nil {
		return err
	}

	fmt.Println("==> waiting for installer DaemonSet rollout")
	if err := kubectl(
		[]string{"KUBECONFIG=" + c.KubeconfigPath},
		nil,
		"rollout", "status",
		"daemonset/natra-installer", "-n", "kube-system",
		"--timeout=120s"); err != nil {
		return err
	}

	fmt.Println("natra installed on the vm-rig cluster.")
	return nil
}

// importImage builds one image from deploy/docker/<dockerfile> on
// the host, then exports + copies + imports it into both VMs'
// k3s-embedded containerd. Standalone (not used by cmdInstall,
// which has its own two-image loop) so callers that need only one
// image — e.g. perfvsvanilla's perfclient — don't drag in the
// natra DaemonSet apply. Same mechanics as cmdInstall's loop.
func importImage(c *Config, image, dockerfile string) error {
	fmt.Printf("==> building image %s (%s)\n", image, dockerfile)
	if err := run("docker", "build", "-q", "-t", image,
		"-f", filepath.Join(c.RepoRoot, "deploy", "docker", dockerfile),
		c.RepoRoot); err != nil {
		return err
	}
	tarFile := "/tmp/natra-vm-rig-importimage.tar"
	defer func() { _ = os.Remove(tarFile) }()
	if err := run("docker", "save", "-o", tarFile, image); err != nil {
		return err
	}
	for _, vm := range []string{c.ServerName, c.AgentName} {
		fmt.Printf("==> copying %s to %s\n", image, vm)
		if err := run("limactl", "copy", tarFile, vm+":/tmp/natra-vm-rig-importimage.tar"); err != nil {
			return err
		}
		fmt.Printf("==> importing %s in %s\n", image, vm)
		if err := run("limactl", "shell", vm, "--",
			"sudo", "k3s", "ctr", "-n", "k8s.io", "images", "import",
			"/tmp/natra-vm-rig-importimage.tar"); err != nil {
			return err
		}
	}
	return nil
}

// renderInstallerManifest reads deploy/cni-installer.yaml and
// rewrites it for the vm-rig:
//   - image: pinned to the local vm-rig tag built above
//   - imagePullPolicy on the natra init container: Never
//     (we want the imported copy, not a registry pull)
//   - conflist hostPath: k3s's /var/lib/rancher/k3s/agent/etc/cni/net.d
//     in place of the default /etc/cni/net.d
//
// The pause sidecar's imagePullPolicy stays IfNotPresent — flipping
// it to Never breaks k3s nodes that don't have the pause image
// cached.
func renderInstallerManifest(c *Config) (string, error) {
	src := filepath.Join(c.RepoRoot, "deploy", "cni-installer.yaml")
	f, err := os.Open(src)
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()

	var out strings.Builder
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)

	const natraTag = "ghcr.io/terraboops/natra:latest"
	const k8sEtcCNI = "path: /etc/cni/net.d"
	const k3sEtcCNI = "path: /var/lib/rancher/k3s/agent/etc/cni/net.d"

	// State: when we see the natra init-container image line, the
	// next imagePullPolicy line should be rewritten to Never. The
	// pause sidecar's image line doesn't trigger this flag, so its
	// IfNotPresent stays.
	flipNextPullPolicy := false
	for scanner.Scan() {
		line := scanner.Text()
		switch {
		case strings.Contains(line, natraTag):
			out.WriteString(strings.Replace(line, natraTag, c.NatraImage, 1))
			out.WriteByte('\n')
			flipNextPullPolicy = true
		case flipNextPullPolicy && strings.Contains(line, "imagePullPolicy:"):
			out.WriteString(strings.Replace(line, "IfNotPresent", "Never", 1))
			out.WriteByte('\n')
			flipNextPullPolicy = false
		case strings.Contains(line, k8sEtcCNI):
			out.WriteString(strings.Replace(line, k8sEtcCNI, k3sEtcCNI, 1))
			out.WriteByte('\n')
		default:
			out.WriteString(line)
			out.WriteByte('\n')
		}
	}
	if err := scanner.Err(); err != nil {
		return "", err
	}
	return out.String(), nil
}
