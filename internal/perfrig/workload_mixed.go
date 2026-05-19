package perfrig

import (
	"context"
)

// runMixed is the bystander-aware workload: annotated elephant via
// iperf3 --bidir on perf-server while hey mice hit perf-server and
// an unannotated bystander on the same worker node. It reports
// annotated-pod RPS / p50 / p99 (the CMS fast-pass story) and
// bystander RPS / p99 (collateral cost — natra must not charge an
// unannotated neighbor).
//
// Stub for the first lima validation pass. The iperfSweep workload
// above captures the annotated-pod story (elephant + mice on one
// pod) end-to-end and is what the existing rig has always run; the
// bystander dimension lands in the next slice with the bash
// render_mixed_manifests / run_mixed_workload port.
func (e *Executor) runMixed(_ context.Context, phase Phase) (WorkloadReport, error) {
	wr := WorkloadReport{Kind: WorkloadMixed}
	e.logf("  [%s] mixed workload: bystander port pending (next slice)\n", phase)
	return wr, nil
}
