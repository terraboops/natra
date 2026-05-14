package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// cmdTest pins the iperf-client to the server VM and iperf-server
// to the agent VM, so iperf3 traffic crosses the inter-VM virtual
// NIC pair (real two-kernel handoff, not a shared bridge in one
// kernel). Asserts the measured throughput stays within the 1.30×
// cap the L4 e2e uses.
func cmdTest(c *Config) error {
	if _, err := os.Stat(c.KubeconfigPath); err != nil {
		return fmt.Errorf("%s not found — run 'vm-rig up' first", c.KubeconfigPath)
	}

	const (
		namespace   = "natra-vm-rig"
		serverNode  = "lima-natra-server" // lima sets in-VM hostname to lima-<vm>
		workerNode  = "lima-natra-agent"
		rateBitsPS  = 10_000_000 // 10 Mbps annotation
		slackFactor = 1.30
	)
	env := []string{"KUBECONFIG=" + c.KubeconfigPath}

	// Ensure namespace exists. `kubectl apply` with a dry-run YAML
	// is the standard idempotent shape.
	if err := kubectl(env,
		strings.NewReader("apiVersion: v1\nkind: Namespace\nmetadata:\n  name: "+namespace+"\n"),
		"apply", "-f", "-"); err != nil {
		return err
	}

	fmt.Println("==> deploying iperf client + server")
	for _, m := range []string{"iperf-client.yaml", "iperf-server.yaml"} {
		manifest, err := renderIperfManifest(c, m, namespace, serverNode, workerNode)
		if err != nil {
			return err
		}
		if err := kubectl(env, strings.NewReader(manifest),
			"apply", "-f", "-"); err != nil {
			return err
		}
	}

	fmt.Println("==> waiting for iperf pods Ready")
	if err := kubectl(env, nil,
		"wait", "--for=condition=Ready",
		"pod/iperf-client", "pod/iperf-server",
		"-n", namespace, "--timeout=120s"); err != nil {
		return err
	}

	// Drain the bucket. natra's burst is 2× rate (~2.5 MB at
	// 10 Mbps); a fresh bucket lets the first measured second run
	// at line rate. 20 seconds × 4 parallel streams is enough to
	// flush both natra and (would-be-)vanilla burst windows.
	fmt.Println("==> warming up iperf-server (draining bucket)")
	_ = kubectl(env, nil,
		"exec", "-n", namespace, "iperf-client", "--",
		"iperf3", "-c", "iperf-server", "-t", "20", "-P", "4")

	fmt.Println("==> measuring throttled throughput")
	out, err := captureWithEnv(env,
		"kubectl", "exec", "-n", namespace, "iperf-client", "--",
		"iperf3", "-c", "iperf-server", "-t", "15", "-J")
	if err != nil {
		return err
	}
	var res iperfResult
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		return fmt.Errorf("parse iperf JSON: %w", err)
	}

	measured := res.End.SumReceived.BitsPerSecond
	cap := float64(rateBitsPS) * slackFactor

	fmt.Println()
	fmt.Printf("iperf-server (annotated 10 Mbps) on %s ← iperf-client on %s\n", workerNode, serverNode)
	fmt.Printf("  measured: %s\n", fmtBps(measured))
	fmt.Printf("  cap:      %s (rate × %.2f slack)\n", fmtBps(cap), slackFactor)

	if measured > cap {
		return fmt.Errorf("measured throughput %s exceeds cap %s", fmtBps(measured), fmtBps(cap))
	}
	fmt.Println("PASS: ingress throttled within cap on a real two-kernel cluster.")
	return nil
}

func fmtBps(bps float64) string {
	return fmt.Sprintf("%.2f Mbps", bps/1e6)
}

// renderIperfManifest reads test/e2e/manifests/<name>, swaps the
// k3d node-name nodeSelectors for the lima ones, and rewrites the
// hardcoded natra-e2e namespace to the rig's namespace. The
// manifests stay shared with the L4 e2e suite — this is just a
// nodeSelector/namespace overlay.
func renderIperfManifest(c *Config, name, namespace, serverNode, workerNode string) (string, error) {
	src := filepath.Join(c.RepoRoot, "test", "e2e", "manifests", name)
	f, err := os.Open(src)
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()

	const (
		k3dAgent  = "k3d-natra-e2e-agent-0"
		k3dServer = "k3d-natra-e2e-server-0"
		nsE2E     = "namespace: natra-e2e"
	)
	repl := strings.NewReplacer(
		k3dAgent, workerNode,
		k3dServer, serverNode,
		nsE2E, "namespace: "+namespace,
	)

	var out strings.Builder
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	for scanner.Scan() {
		out.WriteString(repl.Replace(scanner.Text()))
		out.WriteByte('\n')
	}
	if err := scanner.Err(); err != nil {
		return "", err
	}
	return out.String(), nil
}
