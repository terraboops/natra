package perfrig

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
)

// runIperfSweep is the iperf workload: for each rate in the plan,
// run Plan.Samples iperf3 measurements in each direction (ingress
// = forward, egress = -R reverse) and one hey HTTP run for the
// mice. The current implementation runs the elephant cleanly; the
// "parallel small-flow mice" iperf-side measurement the bash rig
// adds on top is a follow-on (the hey HTTP path already exercises
// the mice latency story on a real, fresh-flow workload).
//
// Today only the first rate in plan.Rates is exercised — the
// staged perf-server is deployed once at the manifest's
// annotation (typically 10M/10M). Per-rate re-annotation +
// redeploy is straightforward to add and lands when the multi-
// rate sweep ports in; for the first lima validation a single
// rate at the manifest's value is what the existing rig has
// always run.
func (e *Executor) runIperfSweep(ctx context.Context, phase Phase) (WorkloadReport, error) {
	wr := WorkloadReport{Kind: WorkloadIperfSweep}
	ns := e.namespaceForPhase()
	kc := e.Substrate.KubeconfigPath()

	// The manifest's annotation (10M/10M) is the rate we measure
	// against today. A future multi-rate pass will re-annotate +
	// redeploy per rate.
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

		// hey mice — the CMS fast-pass story. Same cluster, same
		// pod, fresh-connection HTTP requests against perf-server's
		// nginx. Recorded as MixedSamples (which has PodRPS/p50/p99
		// fields) rather than IperfCells; the report writer ignores
		// the zero bystander fields on the iperfSweep rows.
		const heyDur = 15
		heyOut, err := captureKubectl(ctx, kc,
			"exec", "-n", ns, "perf-client", "-c", "tools", "--",
			"hey", "-z", strconv.Itoa(heyDur)+"s", "-c", "50",
			"-disable-keepalive", "-o", "csv", "http://perf-server:80/")
		if err != nil {
			return wr, fmt.Errorf("hey sample %d: %w", s, err)
		}
		hr, err := parseHeyCSV([]byte(heyOut), float64(heyDur))
		if err != nil {
			return wr, fmt.Errorf("parse hey sample %d: %w", s, err)
		}
		wr.MixedSamples = append(wr.MixedSamples, MixedSample{
			Sample:          s,
			IperfIngressBps: ing,
			IperfEgressBps:  eg,
			PodRPS:          hr.rps(),
			PodP50:          hr.P50 * 1000,
			PodP99:          hr.P99 * 1000,
		})
		e.logf("  [%s s%d] ing=%.1fMbps eg=%.1fMbps | hey %.0f rps p50=%.1fms p99=%.1fms\n",
			phase, s, ing/1e6, eg/1e6, hr.rps(), hr.P50*1000, hr.P99*1000)
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
