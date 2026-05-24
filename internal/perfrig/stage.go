package perfrig

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// stagePhase prepares the cluster for the workload pass: creates
// the namespace, deploys perf-server + perf-client, waits Ready,
// asserts cross-node pod traffic actually flows, runs the
// vanilla-only TBF patch, and warms the bucket / nginx / conntrack
// caches so per-sample numbers are steady-state.
//
// Ported from cmd/vm-rig/perfvsvanilla.go's pvvMeasurePhase. The
// behavior is intentionally identical for the iperf+hey path so
// the new executor produces the same baseline/vanilla/natra story
// the old one did on the same lima rig.
func (e *Executor) stagePhase(ctx context.Context, phase Phase) error {
	server, worker := e.Substrate.Nodes()
	kc := e.Substrate.KubeconfigPath()

	ns := e.Namespace
	if ns == "" {
		ns = "natra-perfrig"
	}

	// 1. Namespace. Wait for the default ServiceAccount before
	// applying any pod into it — k8s creates the SA asynchronously
	// after the namespace, and on cold runners (CI) the first pod
	// apply races the SA controller and fails with
	// "serviceaccount 'default' not found". A short poll is the
	// simplest portable wait (kubectl wait --for=create needs a
	// newer kubectl than every runner has).
	nsYAML := "apiVersion: v1\nkind: Namespace\nmetadata:\n  name: " + ns + "\n"
	if err := kubectl(ctx, kc, strings.NewReader(nsYAML), "apply", "-f", "-"); err != nil {
		return fmt.Errorf("create namespace %s: %w", ns, err)
	}
	saReady := false
	for i := 0; i < 30; i++ {
		if _, err := captureKubectl(ctx, kc, "get", "sa", "default", "-n", ns); err == nil {
			saReady = true
			break
		}
		time.Sleep(2 * time.Second)
	}
	if !saReady {
		return fmt.Errorf("default ServiceAccount in %s never appeared (60s)", ns)
	}

	// 2. perf-server + perf-client. baseline phase strips the
	//    bandwidth annotations from perf-server so the bundled
	//    plugin is inert for it (true unshaped wire). vanilla and
	//    natra keep the annotations.
	_ = kubectl(ctx, kc, nil, "delete", "pod", "perf-server", "perf-client",
		"-n", ns, "--ignore-not-found", "--grace-period=0", "--force")
	for _, m := range []string{
		"test/perf/realworld/perf-server.yaml",
		"test/perf/realworld/perf-client.yaml",
	} {
		manifest, err := renderPerfManifest(e.RepoRoot, m, ns, server, worker, e.PerfclientImage)
		if err != nil {
			return err
		}
		if phase == PhaseBaseline && strings.HasSuffix(m, "perf-server.yaml") {
			manifest = stripBandwidthAnnotations(manifest)
		}
		if err := kubectl(ctx, kc, strings.NewReader(manifest), "apply", "-f", "-"); err != nil {
			return err
		}
	}
	if err := kubectl(ctx, kc, nil, "wait", "--for=condition=Ready",
		"pod/perf-server", "pod/perf-client",
		"-n", ns, "--timeout=180s"); err != nil {
		return err
	}

	// 3. vanilla phase: patch the bundled bandwidth plugin's per-pod
	//    TBF burst down to 1 MB on each node. Without this the
	//    kubelet default burst (~150s of credit) leaves a short run
	//    effectively unshaped.
	if phase == PhaseVanilla {
		e.logf("==> [%s] patching TBF burst to 1mb on both nodes\n", phase)
		if err := e.patchVanillaTBF(ctx); err != nil {
			return fmt.Errorf("TBF patch: %w", err)
		}
	}

	// 4. Cross-node connectivity gate. Poll a 1s iperf3 until it
	//    reports nonzero bps. Without this the first real sample
	//    sometimes lands during the brief window after Ready but
	//    before flannel/cilium has the cross-node route programmed.
	connected := false
	for i := 0; i < 30; i++ {
		out, err := captureKubectl(ctx, kc, "exec", "-n", ns,
			"perf-client", "-c", "tools", "--",
			"iperf3", "-c", "perf-server", "-t", "1", "-J")
		if err == nil {
			var ir iperfJSON
			if json.Unmarshal([]byte(out), &ir) == nil &&
				ir.End.SumReceived.BitsPerSecond > 0 {
				connected = true
				break
			}
		}
		time.Sleep(2 * time.Second)
	}
	if !connected {
		return fmt.Errorf("[%s] cross-pod traffic never came up (perf-client → perf-server)", phase)
	}

	// 5. Warmup. Drain the iperf burst in both directions and prime
	//    the HTTP path (nginx, conntrack, CMS/bucket) so every
	//    sample below is steady-state, not catching a warming
	//    transient.
	e.logf("==> [%s] warming up (iperf burst both directions + HTTP)\n", phase)
	_ = kubectl(ctx, kc, nil, "exec", "-n", ns, "perf-client", "-c", "tools", "--",
		"iperf3", "-c", "perf-server", "-t", "20", "-P", "4")
	_ = kubectl(ctx, kc, nil, "exec", "-n", ns, "perf-client", "-c", "tools", "--",
		"iperf3", "-c", "perf-server", "-t", "20", "-P", "4", "-R")
	_, _ = captureKubectl(ctx, kc,
		"exec", "-n", ns, "perf-client", "-c", "tools", "--",
		"hey", "-z", "5s", "-c", "50", "-disable-keepalive", "http://perf-server:80/")

	// 6. In the natra phase, show what attach mode actually
	//    resolved per direction by grepping the natra log on the
	//    worker. Closes the "did auto pick what we expected?"
	//    ambiguity that bit the cilium investigation.
	if phase == PhaseNatra {
		e.logNatraAttachState(ctx)
	}
	return nil
}

// logNatraAttachState reads /var/log/natra-cni.log on the worker
// node and prints the natra binary's own "attached:" entries — one
// per attached direction. The lines include the resolved side
// (hostside/podside), direction (ingress/egress), ifindex, rate,
// burst, and HH threshold. Best-effort: silent if the log isn't
// readable or has no entries.
func (e *Executor) logNatraAttachState(ctx context.Context) {
	_, worker := e.Substrate.Nodes()
	out, err := e.Substrate.NodeShell(ctx, worker,
		`grep -h "attached:" /var/log/natra-cni.log 2>/dev/null | tail -n 8 || true`)
	if err != nil || len(strings.TrimSpace(string(out))) == 0 {
		e.logf("==> [natra] resolved attach state: (no entries in /var/log/natra-cni.log yet)\n")
		return
	}
	e.logf("==> [natra] resolved attach state (most recent attach entries on %s):\n", worker)
	for _, line := range strings.Split(strings.TrimRight(string(out), "\n"), "\n") {
		e.logf("    %s\n", line)
	}
}

// patchVanillaTBF rewrites the bundled bandwidth plugin's per-pod
// TBF burst to 1 MB on both nodes via Substrate.NodeShell. The
// rate field is preserved (different annotations map to different
// rates; only the burst needs squashing).
func (e *Executor) patchVanillaTBF(ctx context.Context) error {
	server, worker := e.Substrate.Nodes()
	const script = `for dev in $(tc qdisc show | awk '/qdisc tbf/ {print $5}' | sort -u); do
  rate=$(tc qdisc show dev "$dev" | sed -n 's/.*rate \([0-9A-Za-z]*\).*/\1/p' | head -1)
  [ -n "$rate" ] || continue
  tc qdisc change dev "$dev" root tbf rate "$rate" burst 1mb latency 50ms 2>/dev/null || true
done`
	for _, node := range []string{server, worker} {
		if _, err := e.Substrate.NodeShell(ctx, node, script); err != nil {
			return fmt.Errorf("tbf patch on %s: %w", node, err)
		}
	}
	return nil
}

// namespaceForPhase returns the executor's configured namespace,
// applying the default. Workloads call this rather than re-reading
// the field so the default is in one place.
func (e *Executor) namespaceForPhase() string {
	if e.Namespace == "" {
		return "natra-perfrig"
	}
	return e.Namespace
}
