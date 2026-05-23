package perfrig

import (
	"context"
	"encoding/json"
	"fmt"
)

// runIperfSweep is the iperf-only workload: for each rate in the
// plan and each direction (ingress = forward, egress = -R), run
// Plan.Samples iperf3 elephant measurements. Reports throughput
// per (rate × direction × sample) into IperfCells; the hey HTTP
// mice story is the mixed workload's job, not this one. Keeping
// them split mirrors the bash rig's sweep_rate_workload vs
// run_mixed_workload separation — each workload measures one
// thing cleanly rather than entangling both.
//
// Today only the first rate in plan.Rates is exercised — the
// staged perf-server is deployed once at the manifest's
// annotation (typically 10M/10M). The full multi-rate sweep is
// the spec's `Rates` slice; per-rate re-annotation + redeploy is
// the small extension that picks it up.
func (e *Executor) runIperfSweep(ctx context.Context, phase Phase) (WorkloadReport, error) {
	wr := WorkloadReport{Kind: WorkloadIperfSweep}
	ns := e.namespaceForPhase()
	kc := e.Substrate.KubeconfigPath()
	rate := e.Plan.Rates[0]

	for s := 1; s <= e.Plan.Samples; s++ {
		e.logf("==> [%s] iperfSweep sample %d/%d (rate=%s)\n", phase, s, e.Plan.Samples, rate)
		ing, err := iperfMeasure(ctx, kc, ns, "iperf3", "-c", "perf-server", "-t", "15", "-J")
		if err != nil {
			return wr, fmt.Errorf("ingress sample %d: %w", s, err)
		}
		eg, err := iperfMeasure(ctx, kc, ns, "iperf3", "-c", "perf-server", "-t", "15", "-R", "-J")
		if err != nil {
			return wr, fmt.Errorf("egress sample %d: %w", s, err)
		}
		wr.IperfCells = append(wr.IperfCells,
			IperfCell{Rate: rate, Direction: "ingress", Kind: "elephant", Sample: s, Bps: ing},
			IperfCell{Rate: rate, Direction: "egress", Kind: "elephant", Sample: s, Bps: eg},
		)
		e.logf("  [%s s%d] iperf ing=%.1fMbps eg=%.1fMbps\n",
			phase, s, ing/1e6, eg/1e6)
	}
	return wr, nil
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
