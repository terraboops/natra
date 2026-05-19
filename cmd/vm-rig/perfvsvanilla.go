package main

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"strconv"
	"strings"
	"time"
)

// cmdPerfVsVanilla is the local-developer driver for the
// natra-vs-upstream-bandwidth comparison on two REAL kernels —
// the cross-kernel measurement docs/perf-vs-vanilla.md's "Gaps"
// section flagged as missing. The k3d counterpart
// (scripts/perf-vs-vanilla.sh, `make perf-vs-vanilla`) is the
// CI path: cheap, but "nodes" are containers in one shared
// kernel. This is the high-fidelity path: two lima VMs, two
// kernels, a real inter-VM vmnet wire.
//
// Three phases, each on its OWN pristine cluster (full
// down → up → stage → measure → down per phase). It owns the VM
// lifecycle itself — do NOT `vm-rig up` first; `make
// perf-vs-vanilla-vm` just runs this subcommand.
//
//  1. baseline — k3s default. k3s v1.30+ bundles the upstream
//     bandwidth plugin in its conflist, so the annotated
//     perf-server already gets a TBF qdisc, but with kubelet's
//     ~193 MB default burst → effectively unshaped over a short
//     run. This is the congested-shared-wire reference, NOT an
//     idle wire.
//  2. vanilla — same bundled bandwidth plugin, but its per-pod
//     TBF burst patched down to 1 MB via `limactl shell <vm>
//     sudo tc` (each k8s node IS a VM here, so no k3d-style
//     docker/nsenter dance). Elephant now actually capped.
//  3. natra — cmdInstall chains natra; its CMS fast-passes the
//     small fresh-flow HTTP requests around the token bucket
//     while the elephant pays.
//
// Per phase: deploy perf-server (annotated 10M/10M, on the agent
// VM) + perf-client (on the server VM, so traffic crosses the
// kernel boundary), iperf3-warm + http-warm, then measure iperf3
// elephant both directions + hey fresh-connection HTTP mice.
//
// Independent clusters per phase deliberately trade ~3x bring-up
// cost for zero cross-phase confound — no warm-cache / kernel-
// state / ordering bias, which is required for the per-phase
// *latency* numbers to mean anything.
//
// Output: a comparison table on stdout and at
// /tmp/natra-vm-rig-perf-vs-vanilla-result.txt (distinct from the
// k3d script's file so the two rigs don't clobber each other).
func cmdPerfVsVanilla(c *Config) error {
	const namespace = "natra-vm-rig"
	const serverNode = "lima-natra-server" // lima sets hostname to lima-<vm>
	const workerNode = "lima-natra-agent"

	// Each phase runs on its OWN pristine two-VM cluster: full
	// down → up → stage → measure → down. This costs 3x bring-up
	// (~12-15 min each on the static-IP architecture); the reason
	// is per-phase latency. A shared, fixed-order, no-teardown
	// cluster leaks warm page/containerd cache, accumulated kernel
	// networking state, and natra's persistent BPF into later
	// phases, so the last phase (warm) reads faster than the first
	// (cold). Independent clusters also dissolve the phase-ordering
	// constraint — each phase is self-contained, order-irrelevant.
	phases := []struct {
		name  string
		natra bool         // cmdInstall (chain natra) before measuring
		setup func() error // shaper hook, runs after pods are Ready
	}{
		{"baseline", false, nil},
		{"vanilla", false, func() error { return pvvPatchVanillaTBF(c) }},
		{"natra", true, nil},
	}

	// PVV_RUNS: measurement samples per phase (deploy + warmup once
	// per fresh cluster, then measure N times). >1 → the report
	// shows mean ± stddev so run-to-run variance is visible. The
	// cluster is still fresh per *phase* (cross-phase isolation);
	// repeated samples capture within-phase measurement noise.
	runs := 1
	if v := os.Getenv("PVV_RUNS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			runs = n
		}
	}
	fmt.Printf("==> [perfvsvanilla] %d sample(s) per phase\n", runs)

	results := make([]pvvResult, 0, len(phases))
	for _, ph := range phases {
		fmt.Printf("\n========== PHASE %s — fresh cluster ==========\n", ph.name)
		r, err := pvvRunPhase(c, namespace, serverNode, workerNode, ph.name, ph.natra, ph.setup, runs)
		if err != nil {
			return fmt.Errorf("%s phase: %w", ph.name, err)
		}
		results = append(results, r)
	}
	return pvvReport(results)
}

// meanStd returns the arithmetic mean and the sample standard
// deviation (n-1). std is 0 for fewer than two samples.
func meanStd(xs []float64) (mean, std float64) {
	if len(xs) == 0 {
		return 0, 0
	}
	for _, x := range xs {
		mean += x
	}
	mean /= float64(len(xs))
	if len(xs) < 2 {
		return mean, 0
	}
	var v float64
	for _, x := range xs {
		d := x - mean
		v += d * d
	}
	return mean, math.Sqrt(v / float64(len(xs)-1))
}

// pvvRunPhase stands up a pristine two-VM cluster, optionally
// installs natra, deploys the workload, measures, and ALWAYS tears
// the cluster down (even on error) so zero state leaks into the
// next phase. The defer guarantees teardown on every exit path —
// a half-measured phase must not pollute the next one.
func pvvRunPhase(c *Config, namespace, serverNode, workerNode, phase string,
	withNatra bool, setup func() error, runs int) (pvvResult, error) {
	var r pvvResult
	r.phase = phase

	// Clean any stale instance first so cmdUp builds genuinely
	// fresh (cmdUp skips create when the instance already exists).
	_ = cmdDown(c)
	defer func() {
		fmt.Printf("==> [%s] tearing down cluster\n", phase)
		_ = cmdDown(c)
	}()

	fmt.Printf("==> [%s] bringing up a fresh two-VM cluster\n", phase)
	if err := cmdUp(c); err != nil {
		return r, fmt.Errorf("cluster up: %w", err)
	}

	if withNatra {
		fmt.Printf("==> [%s] installing natra (image + chained conflist)\n", phase)
		if err := cmdInstall(c); err != nil { // imports natra + perfclient, applies DS
			return r, fmt.Errorf("natra install: %w", err)
		}
	} else if err := importImage(c, c.PerfclientImage, "Dockerfile.perfclient"); err != nil {
		return r, fmt.Errorf("import perfclient image: %w", err)
	}

	env := []string{"KUBECONFIG=" + c.KubeconfigPath}
	if err := kubectl(env,
		strings.NewReader("apiVersion: v1\nkind: Namespace\nmetadata:\n  name: "+namespace+"\n"),
		"apply", "-f", "-"); err != nil {
		return r, fmt.Errorf("create namespace %s: %w", namespace, err)
	}
	return pvvMeasurePhase(c, env, namespace, serverNode, workerNode, phase, setup, runs)
}

// pvvResult holds one phase's measurement samples (one entry per
// PVV_RUNS iteration) of perf-server on the real two-kernel
// cluster. The report aggregates to mean ± stddev.
type pvvResult struct {
	phase  string
	ingest []float64 // forward iperf3 → server ingress, bps
	egress []float64 // reverse iperf3 -R → server egress, bps
	heyRPS []float64
	heyP50 []float64 // ms
	heyP99 []float64 // ms
}

// pvvMeasurePhase (re)deploys perf-server + perf-client, runs the
// optional shaper-setup hook once the server pod's qdiscs exist,
// then measures iperf3 elephant (both directions) and hey HTTP mice.
// Recreating the pods each phase guarantees a fresh CNI ADD so the
// phase's shaper attaches cleanly.
func pvvMeasurePhase(c *Config, env []string, namespace, serverNode, workerNode, phase string,
	setup func() error, runs int) (pvvResult, error) {
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
		// baseline = a genuinely unshaped wire, not "bandwidth with
		// kubelet's huge default burst". k3s bundles the upstream
		// bandwidth plugin in its conflist, but it only installs a
		// qdisc for *annotated* pods — so stripping the bandwidth
		// annotations from the baseline perf-server makes the
		// bundled plugin inert for it and the elephant runs at the
		// raw cross-kernel wire ceiling. vanilla/natra keep the
		// annotations (each shapes the same workload its own way).
		if phase == "baseline" && m == "test/perf/realworld/perf-server.yaml" {
			var keep []string
			for _, line := range strings.Split(manifest, "\n") {
				if strings.Contains(line, "kubernetes.io/ingress-bandwidth") ||
					strings.Contains(line, "kubernetes.io/egress-bandwidth") {
					continue
				}
				keep = append(keep, line)
			}
			manifest = strings.Join(keep, "\n")
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

	// Cross-VM connectivity gate (pvv uses perf-client/tools →
	// perf-server, not test.go's iperf-client convention, so we
	// can't reuse waitForIperfConnect). Poll a 1s iperf3 until it
	// reports nonzero throughput or attempts run out.
	connected := false
	for i := 0; i < 30; i++ {
		out, perr := captureKubectl(env, "exec", "-n", namespace,
			"perf-client", "-c", "tools", "--",
			"iperf3", "-c", "perf-server", "-t", "1", "-J")
		if perr == nil {
			var ir iperfResult
			if json.Unmarshal([]byte(out), &ir) == nil &&
				ir.End.SumReceived.BitsPerSecond > 0 {
				connected = true
				break
			}
		}
		time.Sleep(2 * time.Second)
	}
	if !connected {
		return r, fmt.Errorf("[%s] cross-VM pod traffic never came up "+
			"(perf-client → perf-server)", phase)
	}

	// One-time warmup (NOT per sample): drain the iperf burst in
	// both directions and prime the HTTP path (nginx, conntrack,
	// CMS/bucket) so every sample below is steady-state.
	fmt.Printf("==> [%s] warming up (iperf burst both directions + HTTP)\n", phase)
	_ = kubectl(env, nil, "exec", "-n", namespace, "perf-client", "-c", "tools", "--",
		"iperf3", "-c", "perf-server", "-t", "20", "-P", "4")
	_ = kubectl(env, nil, "exec", "-n", namespace, "perf-client", "-c", "tools", "--",
		"iperf3", "-c", "perf-server", "-t", "20", "-P", "4", "-R")
	_, _ = captureKubectl(env, "exec", "-n", namespace, "perf-client", "-c", "tools", "--",
		"hey", "-z", "5s", "-c", "50", "-disable-keepalive", "http://perf-server:80/")

	// Sample loop: deploy + warmup happened once on this fresh
	// cluster; repeat the measurement `runs` times to capture
	// run-to-run variance (reported as mean ± stddev).
	const heyDur = 15
	measureIperf := func(label string, args ...string) (float64, error) {
		full := append([]string{"exec", "-n", namespace, "perf-client", "-c", "tools", "--"}, args...)
		out, err := captureKubectl(env, full...)
		if err != nil {
			return 0, fmt.Errorf("%s iperf: %w", label, err)
		}
		var ir iperfResult
		if err := json.Unmarshal([]byte(out), &ir); err != nil {
			return 0, fmt.Errorf("parse %s iperf JSON: %w", label, err)
		}
		return ir.End.SumReceived.BitsPerSecond, nil
	}
	for s := 1; s <= runs; s++ {
		fmt.Printf("==> [%s] sample %d/%d\n", phase, s, runs)
		ing, err := measureIperf("ingress", "iperf3", "-c", "perf-server", "-t", "15", "-J")
		if err != nil {
			return r, err
		}
		eg, err := measureIperf("egress", "iperf3", "-c", "perf-server", "-t", "15", "-R", "-J")
		if err != nil {
			return r, err
		}
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
		r.ingest = append(r.ingest, ing)
		r.egress = append(r.egress, eg)
		r.heyRPS = append(r.heyRPS, hr.RPS())
		r.heyP50 = append(r.heyP50, hr.P50*1000)
		r.heyP99 = append(r.heyP99, hr.P99*1000)
		fmt.Printf("  [%s s%d] ingress=%s egress=%s | hey %.0f rps p50=%.1fms p99=%.1fms\n",
			phase, s, fmtBps(ing), fmtBps(eg), hr.RPS(), hr.P50*1000, hr.P99*1000)
	}

	im, is := meanStd(r.ingest)
	em, es := meanStd(r.egress)
	rm, rs := meanStd(r.heyRPS)
	fmt.Printf("  [%s mean±sd over %d] ingress=%s±%.2fMbps egress=%s±%.2fMbps hey=%.0f±%.0f rps\n",
		phase, runs, fmtBps(im), is/1e6, fmtBps(em), es/1e6, rm, rs)
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
	fmt.Fprintf(&b, "perf-server on lima-natra-agent, perf-client on lima-natra-server\n")
	fmt.Fprintf(&b, "— traffic crosses the inter-VM vmnet wire, each node its own\n")
	fmt.Fprintf(&b, "kernel. baseline has no bandwidth annotation (unshaped wire);\n")
	fmt.Fprintf(&b, "vanilla and natra annotate 10M/10M. iperf3 elephant\n")
	fmt.Fprintf(&b, "(receiver-side bps); hey = fresh-connection HTTP mice.\n")
	n := 0
	if len(results) > 0 {
		n = len(results[0].ingest)
	}
	fmt.Fprintf(&b, "Generated %s — %d sample(s)/phase", time.Now().UTC().Format(time.RFC3339), n)
	if n > 1 {
		fmt.Fprintf(&b, ", cells are mean±stddev")
	}
	fmt.Fprintf(&b, "\n\n")

	// cell renders mean (and ±stddev when >1 sample) at the given
	// scale; e.g. scale 1e6 for bps→Mbps, 1 for rps/ms.
	cell := func(xs []float64, scale float64, dp int) string {
		m, s := meanStd(xs)
		if len(xs) < 2 {
			return fmt.Sprintf("%.*f", dp, m/scale)
		}
		return fmt.Sprintf("%.*f±%.*f", dp, m/scale, dp, s/scale)
	}
	fmt.Fprintf(&b, "%-10s  %-14s  %-14s  %-14s  %-11s  %-11s\n",
		"Phase", "iperf ing Mbps", "iperf eg Mbps", "hey rps", "p50 ms", "p99 ms")
	fmt.Fprintf(&b, "%s\n", strings.Repeat("-", 84))
	for _, r := range results {
		fmt.Fprintf(&b, "%-10s  %-14s  %-14s  %-14s  %-11s  %-11s\n",
			r.phase,
			cell(r.ingest, 1e6, 1), cell(r.egress, 1e6, 1),
			cell(r.heyRPS, 1, 0), cell(r.heyP50, 1, 1), cell(r.heyP99, 1, 1))
	}
	out := b.String()
	fmt.Print("\n" + out)
	if err := os.WriteFile(resultPath, []byte(out), 0o644); err != nil {
		return fmt.Errorf("write result file: %w", err)
	}
	fmt.Printf("\nResult written to %s\n", resultPath)
	return nil
}
