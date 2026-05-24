package perfrig

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// runIperfSweep is the iperf-only workload: for each rate in the
// plan, deploy a per-rate perf-server pod annotated at that rate,
// run Plan.Samples iperf3 elephant measurements in each direction
// (ingress = forward, egress = -R), then clean up the per-rate pods.
//
// Per-rate redeploy is required because kubelet reads bandwidth
// annotations at CNI ADD time and doesn't refresh on annotation
// edits to a running pod. Multi-rate sweep needs separate
// admission-time configurations, so it gets separate pods.
//
// Mirrors the bash apply_rate_sweep_servers → sweep_rate_workload
// pattern: pods are deployed once per phase, each rate measured
// Plan.Samples times, all pods deleted at the end.
//
// The staged perf-server (deployed by stagePhase) is the mixed +
// memory workloads' target; iperfSweep deploys its own
// perf-server-r<label> pods so this workload's rate sweep doesn't
// stomp on the other workloads' assumed pod shape.
func (e *Executor) runIperfSweep(ctx context.Context, phase Phase) (WorkloadReport, error) {
	wr := WorkloadReport{Kind: WorkloadIperfSweep}
	ns := e.namespaceForPhase()
	kc := e.Substrate.KubeconfigPath()

	// Deploy per-rate pods up front so the wait + (vanilla only)
	// TBF patch happen once for all rates rather than per-rate.
	rateNames := make([]string, 0, len(e.Plan.Rates))
	for _, r := range e.Plan.Rates {
		name := iperfSweepPodName(r)
		if err := e.deployIperfSweepPod(ctx, ns, name, r, phase); err != nil {
			return wr, fmt.Errorf("deploy %s: %w", name, err)
		}
		rateNames = append(rateNames, name)
	}
	defer func() {
		// Best-effort teardown — failures here only matter for the
		// next phase, which gets a fresh cluster anyway via
		// runPhase's deferred Down.
		_ = kubectl(ctx, kc, nil, "delete", "pod",
			"-n", ns, "-l", "rig=perf-server-rateSweep",
			"--ignore-not-found", "--grace-period=0", "--force")
	}()
	if err := kubectl(ctx, kc, nil, "wait", "--for=condition=Ready", "pods",
		"-n", ns, "-l", "rig=perf-server-rateSweep", "--timeout=180s"); err != nil {
		return wr, fmt.Errorf("wait per-rate pods: %w", err)
	}
	if phase == PhaseVanilla {
		// Re-patch TBF burst — the per-rate pods just got their
		// own kubelet-default-burst TBF qdiscs; patch them too so
		// the measurement reflects the configured rate, not the
		// initial-burst window.
		if err := e.patchVanillaTBF(ctx); err != nil {
			return wr, fmt.Errorf("re-patch TBF after rate-pod deploy: %w", err)
		}
	}

	for ri, rate := range e.Plan.Rates {
		target := rateNames[ri]
		for s := 1; s <= e.Plan.Samples; s++ {
			e.logf("==> [%s] iperfSweep sample %d/%d (rate=%s target=%s)\n",
				phase, s, e.Plan.Samples, rate, target)
			ing, err := iperfMeasure(ctx, kc, ns, "iperf3", "-c", target, "-t", "15", "-J")
			if err != nil {
				return wr, fmt.Errorf("ingress sample %d (rate=%s): %w", s, rate, err)
			}
			eg, err := iperfMeasure(ctx, kc, ns, "iperf3", "-c", target, "-t", "15", "-R", "-J")
			if err != nil {
				return wr, fmt.Errorf("egress sample %d (rate=%s): %w", s, rate, err)
			}
			wr.IperfCells = append(wr.IperfCells,
				IperfCell{Rate: rate, Direction: "ingress", Kind: "elephant", Sample: s, Bps: ing},
				IperfCell{Rate: rate, Direction: "egress", Kind: "elephant", Sample: s, Bps: eg},
			)
			e.logf("  [%s s%d rate=%s] iperf ing=%.1fMbps eg=%.1fMbps\n",
				phase, s, rate, ing/1e6, eg/1e6)
		}
	}
	return wr, nil
}

// iperfSweepPodName produces a stable pod name for a given rate.
// Convention matches the bash rig: "perf-server-r10m",
// "perf-server-r1g", "perf-server-r10g".
func iperfSweepPodName(r Rate) string {
	return "perf-server-r" + strings.ToLower(string(r))
}

// deployIperfSweepPod renders the perf-server manifest with the
// pod renamed for this rate and its bandwidth annotations
// rewritten to the rate. Baseline phase strips the annotations
// so the bundled bandwidth plugin is inert for it. The Pod doc
// only is applied (the Service block is dropped — the per-rate
// pods aren't fronted by a Service; iperf3 -c uses the pod IP
// resolved by name via cluster DNS, which kubelet sets up via
// the pod's hostname even without a Service).
//
// Wait — pod IP resolution by name requires either a Service or
// a Headless-Service DNS entry. Without a Service for this pod
// name, iperf3 -c perf-server-r10m won't resolve. Solution:
// keep the Service block too, renaming it to match. Each per-
// rate pod gets its own Service for DNS resolution.
func (e *Executor) deployIperfSweepPod(ctx context.Context, ns, name string, rate Rate, phase Phase) error {
	server, worker := e.Substrate.Nodes()
	manifest, err := renderPerfManifest(e.RepoRoot,
		"test/perf/realworld/perf-server.yaml", ns, server, worker, e.PerfclientImage)
	if err != nil {
		return err
	}
	// Rewrite the pod + service names from perf-server to
	// perf-server-r<label>. The base manifest has 'name: perf-
	// server' on both the Pod and the Service; replacing all
	// occurrences is safe because the label selector matches the
	// pod's app label which we rewrite separately below.
	manifest = strings.ReplaceAll(manifest, "name: perf-server", "name: "+name)
	// Rewrite the labels + selector. The base has `app:
	// perf-server` on both the pod and the Service selector.
	// Replacing both with the same per-rate label keeps the
	// Service routing only to its own per-rate pod.
	manifest = strings.ReplaceAll(manifest, "app: perf-server", "app: "+name)
	// Add a second label so the bulk delete on workload teardown
	// can wait on / clean up all per-rate pods together.
	manifest = strings.Replace(manifest,
		"app: "+name+"\nspec:",
		"app: "+name+"\n    rig: perf-server-rateSweep\nspec:", 1)

	// Rewrite the bandwidth annotation rates. Manifest baseline is
	// "10M"; rewrite to whatever rate this pod targets.
	annot := string(rate)
	manifest = strings.ReplaceAll(manifest,
		`kubernetes.io/ingress-bandwidth: "10M"`,
		`kubernetes.io/ingress-bandwidth: "`+annot+`"`)
	manifest = strings.ReplaceAll(manifest,
		`kubernetes.io/egress-bandwidth: "10M"`,
		`kubernetes.io/egress-bandwidth: "`+annot+`"`)
	if phase == PhaseBaseline {
		manifest = stripBandwidthAnnotations(manifest)
	}

	if err := kubectl(ctx, e.Substrate.KubeconfigPath(),
		strings.NewReader(manifest), "apply", "-f", "-"); err != nil {
		return err
	}
	// Tiny pause so kubelet has scheduled the pod before the
	// follow-up wait, which sometimes races against a not-yet-
	// admitted pod when the apply returns immediately.
	time.Sleep(500 * time.Millisecond)
	return nil
}

// iperfMeasure runs one iperf3 invocation through the perf-client
// "tools" container and returns the receiver-side bps. Shared by
// the iperfSweep and mixed workloads.
func iperfMeasure(ctx context.Context, kubeconfig, ns string, iperfArgs ...string) (float64, error) {
	full := append([]string{"exec", "-n", ns, "perf-client", "-c", "tools", "--"}, iperfArgs...)
	out, err := captureKubectl(ctx, kubeconfig, full...)
	if err != nil {
		return 0, err
	}
	var ir iperfJSON
	if err := json.Unmarshal([]byte(out), &ir); err != nil {
		return 0, fmt.Errorf("parse iperf JSON: %w", err)
	}
	return ir.End.SumReceived.BitsPerSecond, nil
}
