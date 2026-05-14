package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// cmdTest runs two assertions back-to-back against the vm-rig:
//
//  1. iperf throttle: pin iperf-client to the server VM and
//     iperf-server to the agent VM so traffic crosses the inter-VM
//     virtual NIC pair, measure receiver bps, assert it stays
//     within the 1.30× slack cap.
//  2. hey fast-pass: deploy perf-server (iperf+nginx, both
//     directions annotated 10M) and perf-client (iperf3+hey baked
//     in), kubectl exec hey against the nginx target, parse the
//     CSV, assert RPS clears a floor that proves CMS classification
//     is letting HTTP mice bypass the bucket.
//
// Both pieces run against the same cluster, share the namespace,
// and both source traffic from the server VM toward the agent VM
// so the cross-kernel signal is the same.
func cmdTest(c *Config) error {
	if _, err := os.Stat(c.KubeconfigPath); err != nil {
		return fmt.Errorf("%s not found — run 'vm-rig up' first", c.KubeconfigPath)
	}

	const (
		namespace  = "natra-vm-rig"
		serverNode = "lima-natra-server" // lima sets in-VM hostname to lima-<vm>
		workerNode = "lima-natra-agent"
	)
	env := []string{"KUBECONFIG=" + c.KubeconfigPath}

	// Ensure namespace exists. `kubectl apply` with a dry-run YAML
	// is the standard idempotent shape.
	if err := kubectl(env,
		strings.NewReader("apiVersion: v1\nkind: Namespace\nmetadata:\n  name: "+namespace+"\n"),
		"apply", "-f", "-"); err != nil {
		return err
	}

	if err := testIperfThrottle(c, env, namespace, serverNode, workerNode); err != nil {
		return err
	}
	if err := testEgressOnly(c, env, namespace, serverNode, workerNode); err != nil {
		return err
	}
	if err := testHeyFastPass(c, env, namespace, serverNode, workerNode); err != nil {
		return err
	}
	return nil
}

// testEgressOnly: Topology B in L4-suite terms — server pod has only
// the egress annotation, no ingress. iperf3 -R measures egress
// (server → client). natra should attach the egress program and
// throttle to 10 Mbps; the absence of an ingress annotation should
// mean natra skips the ingress attach entirely. A regression that
// attaches both programs regardless of annotation would still pass
// the bidi test (where both are annotated) but is caught here by
// the asymmetric configuration.
func testEgressOnly(c *Config, env []string, namespace, serverNode, workerNode string) error {
	const (
		rateBitsPS  = 10_000_000
		slackFactor = 1.30
		podName     = "iperf-server-egress"
	)

	fmt.Println("==> [egress-only] deploying egress-annotated server")
	manifest, err := renderE2EManifest(c, "iperf-server-egress.yaml", namespace, serverNode, workerNode)
	if err != nil {
		return err
	}
	if err := kubectl(env, strings.NewReader(manifest), "apply", "-f", "-"); err != nil {
		return err
	}

	fmt.Println("==> [egress-only] waiting for pod Ready")
	if err := kubectl(env, nil,
		"wait", "--for=condition=Ready",
		"pod/"+podName,
		"-n", namespace, "--timeout=120s"); err != nil {
		return err
	}

	// Reuse the existing iperf-client (already deployed by the
	// bidi step). Drain the egress bucket before measuring.
	fmt.Println("==> [egress-only] warming up (draining egress bucket)")
	_ = kubectl(env, nil,
		"exec", "-n", namespace, "iperf-client", "--",
		"iperf3", "-c", podName, "-t", "20", "-P", "4", "-R")

	fmt.Println("==> [egress-only] measuring throttled throughput")
	out, err := captureKubectl(env,
		"exec", "-n", namespace, "iperf-client", "--",
		"iperf3", "-c", podName, "-t", "15", "-R", "-J")
	if err != nil {
		return err
	}
	var res iperfResult
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		return fmt.Errorf("parse iperf JSON: %w", err)
	}

	measured := res.End.SumReceived.BitsPerSecond
	cap := float64(rateBitsPS) * slackFactor

	fmt.Printf("  [egress-only] reverse direction, measured: %s\n", fmtBps(measured))
	fmt.Printf("  [egress-only] cap: %s (rate × %.2f slack)\n", fmtBps(cap), slackFactor)

	if measured > cap {
		return fmt.Errorf("[egress-only] measured throughput %s exceeds cap %s",
			fmtBps(measured), fmtBps(cap))
	}
	fmt.Println("PASS [egress-only]: egress throttled even with no ingress annotation.")
	return nil
}

// testIperfThrottle: bidi elephant flow → expect both directions
// throttled within rate × 1.30. Forward iperf3 stresses the
// server's ingress (client → server) and reverse iperf3 -R stresses
// the server's egress (server → client). Same shape as Topology C
// in the L4 e2e suite.
func testIperfThrottle(c *Config, env []string, namespace, serverNode, workerNode string) error {
	const (
		rateBitsPS  = 10_000_000 // 10 Mbps annotation, both directions
		slackFactor = 1.30
	)

	// Use the bidi-annotated manifest (both ingress and egress at
	// 10M); rename the pod from iperf-server-bidi → iperf-server so
	// the iperf3 client invocations stay symmetric with the L4
	// suite. Same pattern scripts/perf-vs-vanilla.sh uses.
	fmt.Println("==> [iperf] deploying iperf client + bidi-annotated server")
	clientManifest, err := renderE2EManifest(c, "iperf-client.yaml", namespace, serverNode, workerNode)
	if err != nil {
		return err
	}
	if err := kubectl(env, strings.NewReader(clientManifest),
		"apply", "-f", "-"); err != nil {
		return err
	}
	serverManifest, err := renderE2EManifest(c, "iperf-server-bidi.yaml", namespace, serverNode, workerNode)
	if err != nil {
		return err
	}
	serverManifest = strings.ReplaceAll(serverManifest, "iperf-server-bidi", "iperf-server")
	if err := kubectl(env, strings.NewReader(serverManifest),
		"apply", "-f", "-"); err != nil {
		return err
	}

	fmt.Println("==> [iperf] waiting for pods Ready")
	if err := kubectl(env, nil,
		"wait", "--for=condition=Ready",
		"pod/iperf-client", "pod/iperf-server",
		"-n", namespace, "--timeout=120s"); err != nil {
		return err
	}

	// Drain both directions' buckets. natra's burst defaults to
	// 0.5 × rate (~625 KB at 10 Mbps); a fresh bucket lets the
	// first measured second run at line rate. 20 seconds × 4
	// parallel streams flushes the forward (ingress) burst; a
	// second pass with -R flushes the reverse (egress) burst.
	fmt.Println("==> [iperf] warming up (draining buckets, both directions)")
	_ = kubectl(env, nil,
		"exec", "-n", namespace, "iperf-client", "--",
		"iperf3", "-c", "iperf-server", "-t", "20", "-P", "4")
	_ = kubectl(env, nil,
		"exec", "-n", namespace, "iperf-client", "--",
		"iperf3", "-c", "iperf-server", "-t", "20", "-P", "4", "-R")

	cap := float64(rateBitsPS) * slackFactor

	for _, leg := range []struct {
		label     string // "ingress" / "egress"
		direction string // "forward (-c)" / "reverse (-R)"
		args      []string
	}{
		{"ingress", "forward", []string{"iperf3", "-c", "iperf-server", "-t", "15", "-J"}},
		{"egress", "reverse", []string{"iperf3", "-c", "iperf-server", "-t", "15", "-R", "-J"}},
	} {
		fmt.Printf("==> [iperf/%s] measuring throttled throughput\n", leg.label)
		args := append([]string{"exec", "-n", namespace, "iperf-client", "--"}, leg.args...)
		out, err := captureKubectl(env, args...)
		if err != nil {
			return err
		}
		var res iperfResult
		if err := json.Unmarshal([]byte(out), &res); err != nil {
			return fmt.Errorf("parse iperf JSON (%s): %w", leg.label, err)
		}
		measured := res.End.SumReceived.BitsPerSecond

		fmt.Printf("  [%s] %s direction, measured: %s\n", leg.label, leg.direction, fmtBps(measured))
		fmt.Printf("  [%s] cap: %s (rate × %.2f slack)\n", leg.label, fmtBps(cap), slackFactor)
		if measured > cap {
			return fmt.Errorf("[iperf/%s] measured throughput %s exceeds cap %s",
				leg.label, fmtBps(measured), fmtBps(cap))
		}
	}
	fmt.Println("PASS [iperf]: both directions throttled within cap on a real two-kernel cluster.")
	return nil
}

// testHeyFastPass: many short HTTP requests against an annotated
// nginx target → expect the CMS fast-pass to let them through
// (high RPS, low latency) even though the same pod's iperf
// elephant would be throttled to ~10 Mbps. This is natra's design
// wedge vs upstream HTB; the assertion is a generous RPS floor
// that any working fast-pass clears trivially and a broken one
// (every request queued behind the bucket) misses by orders of
// magnitude.
func testHeyFastPass(c *Config, env []string, namespace, serverNode, workerNode string) error {
	const (
		heyDurationS = 15
		heyConcurr   = 50
		heyRPSFloor  = 200.0 // generous; perf-vs-vanilla numbers show natra at thousands of RPS, vanilla at ~12
	)

	fmt.Println()
	fmt.Println("==> [hey] deploying perf-server (nginx + iperf, annotated 10M) + perf-client")
	for _, m := range []string{
		"test/perf/realworld/perf-server.yaml",
		"test/perf/realworld/perf-client.yaml",
	} {
		manifest, err := renderPerfManifest(c, m, namespace, serverNode, workerNode, c.PerfclientImage)
		if err != nil {
			return err
		}
		if err := kubectl(env, strings.NewReader(manifest),
			"apply", "-f", "-"); err != nil {
			return err
		}
	}

	fmt.Println("==> [hey] waiting for pods Ready")
	if err := kubectl(env, nil,
		"wait", "--for=condition=Ready",
		"pod/perf-server", "pod/perf-client",
		"-n", namespace, "--timeout=180s"); err != nil {
		return err
	}

	// hey -z <duration> -c <conn> -disable-keepalive — each request
	// is a fresh TCP connection, so the 5-tuple changes per request
	// and the CMS estimate per flow stays below the heavy-hitter
	// threshold. -o csv keeps the parser stable across hey versions.
	fmt.Printf("==> [hey] running hey for %ds against perf-server:80\n", heyDurationS)
	out, err := captureKubectl(env,
		"exec", "-n", namespace, "perf-client", "-c", "tools", "--",
		"hey",
		"-z", fmt.Sprintf("%ds", heyDurationS),
		"-c", strconv.Itoa(heyConcurr),
		"-disable-keepalive",
		"-o", "csv",
		"http://perf-server:80/")
	if err != nil {
		return err
	}

	res, err := parseHeyCSV([]byte(out), float64(heyDurationS))
	if err != nil {
		return fmt.Errorf("parse hey output: %w", err)
	}

	fmt.Println()
	fmt.Printf("perf-server (annotated 10M ingress + egress) on %s ← perf-client on %s\n", workerNode, serverNode)
	fmt.Printf("  hey ok:     %d requests\n", res.OK)
	fmt.Printf("  hey errors: %d\n", res.Errors)
	fmt.Printf("  hey rps:    %.0f\n", res.RPS())
	fmt.Printf("  hey p50:    %.1f ms\n", res.P50*1000)
	fmt.Printf("  hey p99:    %.1f ms\n", res.P99*1000)
	fmt.Printf("  floor:      %.0f rps (CMS fast-pass should clear this with headroom)\n", heyRPSFloor)

	if res.RPS() < heyRPSFloor {
		return fmt.Errorf("[hey] %0.f rps below floor %.0f — CMS fast-pass may be regressed",
			res.RPS(), heyRPSFloor)
	}
	if res.Errors > res.OK/10 {
		return fmt.Errorf("[hey] errors %d > 10%% of ok %d", res.Errors, res.OK)
	}
	fmt.Println("PASS [hey]: HTTP mice fast-passed through CMS while the pod's elephant cap is 10 Mbps.")
	return nil
}

func fmtBps(bps float64) string {
	return fmt.Sprintf("%.2f Mbps", bps/1e6)
}

// renderE2EManifest reads test/e2e/manifests/<name>, swaps the
// k3d node-name nodeSelectors for the lima ones, and rewrites the
// hardcoded natra-e2e namespace to the rig's namespace. The
// manifests stay shared with the L4 e2e suite — this is just a
// nodeSelector/namespace overlay.
func renderE2EManifest(c *Config, name, namespace, serverNode, workerNode string) (string, error) {
	src := filepath.Join(c.RepoRoot, "test", "e2e", "manifests", name)
	return renderManifest(src, strings.NewReplacer(
		"k3d-natra-e2e-agent-0", workerNode,
		"k3d-natra-e2e-server-0", serverNode,
		"namespace: natra-e2e", "namespace: "+namespace,
	))
}

// renderPerfManifest reads a test/perf/realworld/<path> manifest
// and rewrites the placeholder node names (PERF_WORKER_NODE /
// PERF_CONTROL_NODE) and the perfclient image tag. perf-client.yaml
// also lives in test/perf/realworld with the same shape — both
// are templated the same way.
func renderPerfManifest(c *Config, relPath, namespace, serverNode, workerNode, image string) (string, error) {
	src := filepath.Join(c.RepoRoot, relPath)
	return renderManifest(src, strings.NewReplacer(
		"PERF_WORKER_NODE", workerNode,
		"PERF_CONTROL_NODE", serverNode,
		"namespace: natra-e2e", "namespace: "+namespace,
		"ghcr.io/terraboops/natra-perfclient:vsperf", image,
	))
}

// renderManifest is the shared path: read a YAML file, apply a
// string replacer line by line. Lots of strings.Replace.Replace is
// fine because the replacements are short and the manifests are
// small.
func renderManifest(src string, repl *strings.Replacer) (string, error) {
	f, err := os.Open(src)
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()

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
