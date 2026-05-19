package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// cmdPerfVsVanilla is the planned local-developer driver for the
// natra-vs-upstream-bandwidth comparison. The script equivalent is
// scripts/perf-vs-vanilla.sh; that's the k3d path used in CI, where
// "nodes" are containers in one shared kernel and the dataplane is
// software-only.
//
// vm-rig perfvsvanilla will run the same three-phase comparison
// (baseline / natra / upstream bandwidth) against the two-VM lima
// cluster, where each "node" is a real Linux kernel with its own
// veth+IFB. That's the comparison the docs/perf-vs-vanilla.md gaps
// section calls "Cross-kernel wire" — currently a known unmet need
// at the L4 e2e and k3d perf layers.
//
// IMPLEMENTATION STATUS: scaffolding. Currently bails with a
// blocker message if connectivity is dead, or "not yet implemented"
// if it works. The phase code is intentionally absent — it'll get
// filled in once the cross-VM connectivity blocker is unblocked
// (see scripts/vm-rig/README.md "Cross-VM pod traffic blocker"
// section for the unblock paths).
//
// Planned phases (mirror of scripts/perf-vs-vanilla.sh's structure):
//
//  1. Baseline: uninstall natra DS, deploy unannotated server,
//     measure iperf-only rate sweep + mixed workload. Captures the
//     wire ceiling for the underlying lima/socket_vmnet path.
//  2. natra: reinstall natra DS, deploy annotated server (both
//     directions 10M), warmup buckets, measure same workloads.
//  3. upstream bandwidth: replace natra's conflist with the
//     upstream bandwidth chain (k3s already bundles it), warmup,
//     measure same workloads. Apply the same TBF-burst patch the
//     k3d script does (fix_vanilla_tbf_burst), reaching the lima
//     VM netns via limactl shell + tc.
//
// Phase 1 reuses the existing cmdUp/cmdInstall plumbing — it just
// runs without the install step. Phase 2 is what `vm-rig test`
// already exercises end-to-end. Phase 3 is the new work.
//
// Output: same comparison table shape as
// /tmp/natra-perf-vs-vanilla-result.txt, written to a vm-rig-
// specific path so the two rigs don't fight for the same file.
func cmdPerfVsVanilla(c *Config) error {
	if _, err := os.Stat(c.KubeconfigPath); err != nil {
		return fmt.Errorf("%s not found — run 'vm-rig up' first", c.KubeconfigPath)
	}
	env := []string{"KUBECONFIG=" + c.KubeconfigPath}

	// Connectivity gate: cross-VM iperf must work for the comparison
	// to mean anything. If it doesn't, every "measured" cell will be
	// 0 bps and the comparison is noise. The gate's failure mode is
	// the cross-VM connectivity blocker documented in
	// scripts/vm-rig/README.md; we emit a clear pointer to that and
	// bail rather than producing a misleading zero-row table.
	const namespace = "natra-vm-rig"
	const probeTarget = "iperf-server"
	fmt.Println("==> [perfvsvanilla] probing cross-VM pod connectivity")
	if err := waitForIperfConnect(env, namespace, probeTarget, 10); err != nil {
		return fmt.Errorf(
			"cross-VM iperf probe failed: %w\n"+
				"  vm-rig perfvsvanilla needs cross-VM pod traffic to work. The\n"+
				"  current macOS+lima+Debian rig has a known blocker; see\n"+
				"  scripts/vm-rig/README.md '### Cross-VM pod traffic blocker'\n"+
				"  for the three plausible unblock paths.\n"+
				"  Workaround for now: use the k3d-based comparison instead:\n"+
				"    make perf-vs-vanilla", err)
	}

	fmt.Println("==> [perfvsvanilla] cross-VM connectivity OK")

	const serverNode = "lima-natra-server" // lima sets hostname to lima-<vm>
	const workerNode = "lima-natra-agent"

	// One two-VM cluster, shaper swapped in place across three
	// phases. (k3d uses three throwaway clusters because they cost
	// ~30s each; a real two-kernel vm-rig cluster costs minutes, so
	// build-once / swap-shaper is both faster and a stricter
	// comparison — identical kernels, wire, and pods, only the
	// shaper changes.) Order baseline → vanilla → natra: vanilla's
	// tc patch is transient (gone when the pod is recreated); natra
	// installs a persistent DaemonSet + conflist, so it goes last
	// and never needs uninstalling.
	if err := importImage(c, c.PerfclientImage, "Dockerfile.perfclient"); err != nil {
		return fmt.Errorf("import perfclient image: %w", err)
	}

	var results []pvvResult

	base, err := pvvMeasurePhase(c, env, namespace, serverNode, workerNode, "baseline", nil)
	if err != nil {
		return fmt.Errorf("baseline phase: %w", err)
	}
	results = append(results, base)

	van, err := pvvMeasurePhase(c, env, namespace, serverNode, workerNode, "vanilla",
		func() error { return pvvPatchVanillaTBF(c) })
	if err != nil {
		return fmt.Errorf("vanilla phase: %w", err)
	}
	results = append(results, van)

	fmt.Println("==> [natra] installing natra (DaemonSet + chained conflist)")
	if err := cmdInstall(c); err != nil {
		return fmt.Errorf("natra install: %w", err)
	}
	nat, err := pvvMeasurePhase(c, env, namespace, serverNode, workerNode, "natra", nil)
	if err != nil {
		return fmt.Errorf("natra phase: %w", err)
	}
	results = append(results, nat)

	return pvvReport(results)
}

// pvvResult is one phase's measurement of the annotated perf-server
// (10M ingress + egress) on the real two-kernel cluster.
type pvvResult struct {
	phase     string
	ingestBps float64 // forward iperf3 → server ingress
	egressBps float64 // reverse iperf3 -R → server egress
	heyRPS    float64
	heyP50ms  float64
	heyP99ms  float64
}

// pvvMeasurePhase (re)deploys perf-server + perf-client, runs the
// optional shaper-setup hook once the server pod's qdiscs exist,
// then measures iperf3 elephant (both directions) and hey HTTP mice.
// Recreating the pods each phase guarantees a fresh CNI ADD so the
// phase's shaper attaches cleanly.
func pvvMeasurePhase(c *Config, env []string, namespace, serverNode, workerNode, phase string,
	setup func() error) (pvvResult, error) {
	var r pvvResult
	r.phase = phase
	fmt.Printf("\n==> [%s] (re)deploying perf-server + perf-client\n", phase)

	_ = kubectl(env, nil, "delete", "pod", "perf-server", "perf-client",
		"-n", namespace, "--ignore-not-found", "--grace-period=0", "--force")
	for _, m := range []string{
		"test/perf/realworld/perf-server.yaml",
		"test/perf/realworld/perf-client.yaml",
	} {
		manifest, err := renderPerfManifest(c, m, namespace, serverNode, workerNode, c.PerfclientImage)
		if err != nil {
			return r, err
		}
		if err := kubectl(env, strings.NewReader(manifest), "apply", "-f", "-"); err != nil {
			return r, err
		}
	}
	if err := kubectl(env, nil, "wait", "--for=condition=Ready",
		"pod/perf-server", "pod/perf-client",
		"-n", namespace, "--timeout=180s"); err != nil {
		return r, err
	}

	// The shaper hook runs after the pod is Ready — the bundled
	// bandwidth plugin's TBF qdiscs only exist post-CNI-ADD, so the
	// burst patch must come after.
	if setup != nil {
		fmt.Printf("==> [%s] applying shaper setup\n", phase)
		if err := setup(); err != nil {
			return r, fmt.Errorf("shaper setup: %w", err)
		}
	}

	if err := waitForIperfConnect(env, namespace, "perf-server", 30); err != nil {
		return r, fmt.Errorf("cross-VM connectivity: %w", err)
	}

	// Warm up: drain the initial token/burst allowance in both
	// directions so the measured second reflects steady state.
	fmt.Printf("==> [%s] warming up (draining burst, both directions)\n", phase)
	_ = kubectl(env, nil, "exec", "-n", namespace, "perf-client", "-c", "tools", "--",
		"iperf3", "-c", "perf-server", "-t", "20", "-P", "4")
	_ = kubectl(env, nil, "exec", "-n", namespace, "perf-client", "-c", "tools", "--",
		"iperf3", "-c", "perf-server", "-t", "20", "-P", "4", "-R")

	for _, leg := range []struct {
		label string
		args  []string
		dst   *float64
	}{
		{"ingress", []string{"iperf3", "-c", "perf-server", "-t", "15", "-J"}, &r.ingestBps},
		{"egress", []string{"iperf3", "-c", "perf-server", "-t", "15", "-R", "-J"}, &r.egressBps},
	} {
		fmt.Printf("==> [%s/%s] measuring elephant\n", phase, leg.label)
		args := append([]string{"exec", "-n", namespace, "perf-client", "-c", "tools", "--"}, leg.args...)
		out, err := captureKubectl(env, args...)
		if err != nil {
			return r, fmt.Errorf("%s iperf: %w", leg.label, err)
		}
		var ir iperfResult
		if err := json.Unmarshal([]byte(out), &ir); err != nil {
			return r, fmt.Errorf("parse %s iperf JSON: %w", leg.label, err)
		}
		*leg.dst = ir.End.SumReceived.BitsPerSecond
	}

	fmt.Printf("==> [%s/hey] measuring HTTP mice (fresh-conn, CMS fast-pass)\n", phase)
	const heyDur = 15
	heyOut, err := captureKubectl(env,
		"exec", "-n", namespace, "perf-client", "-c", "tools", "--",
		"hey", "-z", strconv.Itoa(heyDur)+"s", "-c", "50",
		"-disable-keepalive", "-o", "csv", "http://perf-server:80/")
	if err != nil {
		return r, fmt.Errorf("hey: %w", err)
	}
	hr, err := parseHeyCSV([]byte(heyOut), float64(heyDur))
	if err != nil {
		return r, fmt.Errorf("parse hey: %w", err)
	}
	r.heyRPS, r.heyP50ms, r.heyP99ms = hr.RPS(), hr.P50*1000, hr.P99*1000

	fmt.Printf("  [%s] ingress=%s egress=%s | hey %.0f rps p50=%.1fms p99=%.1fms\n",
		phase, fmtBps(r.ingestBps), fmtBps(r.egressBps), r.heyRPS, r.heyP50ms, r.heyP99ms)
	return r, nil
}

// pvvPatchVanillaTBF rewrites the k3s-bundled bandwidth plugin's
// per-pod TBF burst (kubelet sets it to ~150s of credit, which
// leaves a short test effectively unshaped) down to 1 MB so the
// measured rate reflects the configured cap. On the vm-rig each
// k8s node IS a VM, so unlike the k3d path this is a plain
// `limactl shell <vm> -- sudo tc ...` — no docker/nsenter dance.
// Idempotent; preserves each qdisc's configured rate.
func pvvPatchVanillaTBF(c *Config) error {
	const script = `for dev in $(tc qdisc show | awk '/qdisc tbf/ {print $5}' | sort -u); do
  rate=$(tc qdisc show dev "$dev" | sed -n 's/.*rate \([0-9A-Za-z]*\).*/\1/p' | head -1)
  [ -n "$rate" ] || continue
  tc qdisc change dev "$dev" root tbf rate "$rate" burst 1mb latency 50ms 2>/dev/null || true
done`
	for _, vm := range []string{c.ServerName, c.AgentName} {
		if err := run("limactl", "shell", vm, "--", "sudo", "sh", "-c", script); err != nil {
			return fmt.Errorf("tbf patch on %s: %w", vm, err)
		}
	}
	return nil
}

// pvvReport prints the comparison table to stdout and writes it to
// a vm-rig-specific result file (distinct from the k3d script's so
// the two rigs don't clobber each other).
func pvvReport(results []pvvResult) error {
	const resultPath = "/tmp/natra-vm-rig-perf-vs-vanilla-result.txt"
	var b strings.Builder
	fmt.Fprintf(&b, "natra vs upstream bandwidth — vm-rig (two real kernels, lima)\n")
	fmt.Fprintf(&b, "================================================================\n")
	fmt.Fprintf(&b, "perf-server annotated 10M ingress + egress, on %s; perf-client\n", "lima-natra-agent")
	fmt.Fprintf(&b, "on lima-natra-server — traffic crosses the inter-VM vmnet wire,\n")
	fmt.Fprintf(&b, "each node its own kernel. iperf3 elephant (receiver-side bps);\n")
	fmt.Fprintf(&b, "hey = fresh-connection HTTP mice (CMS fast-pass demonstrator).\n")
	fmt.Fprintf(&b, "Generated %s\n\n", time.Now().UTC().Format(time.RFC3339))
	fmt.Fprintf(&b, "%-10s  %-12s  %-12s  %10s  %9s  %9s\n",
		"Phase", "iperf ing", "iperf eg", "hey rps", "p50 ms", "p99 ms")
	fmt.Fprintf(&b, "%s\n", strings.Repeat("-", 72))
	for _, r := range results {
		fmt.Fprintf(&b, "%-10s  %-12s  %-12s  %10.0f  %9.1f  %9.1f\n",
			r.phase, fmtBps(r.ingestBps), fmtBps(r.egressBps),
			r.heyRPS, r.heyP50ms, r.heyP99ms)
	}
	out := b.String()
	fmt.Print("\n" + out)
	if err := os.WriteFile(resultPath, []byte(out), 0o644); err != nil {
		return fmt.Errorf("write result file: %w", err)
	}
	fmt.Printf("\nResult written to %s\n", resultPath)
	return nil
}
