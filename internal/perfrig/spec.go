// Package perfrig is the shared executor for the natra-vs-upstream
// perf comparison. One spec, one phase loop, one set of workloads;
// the only thing that differs between rigs is the [Substrate] impl.
//
// The two callers today:
//
//   - cmd/vm-rig perfvsvanilla   → limaSubstrate, --profile=full
//   - scripts/perf-vs-vanilla.sh → k3dSubstrate, --profile=ci
//
// "k3d ⊂ vm-rig" is a structural property here, not a convention:
// [Apply] only accepts profiles that narrow the spec, and a unit
// test asserts the `ci` profile is a subset of `full` over the
// actual values in this package.
package perfrig

// Phase is one of the three comparison phases. Each runs on a
// pristine cluster — the fresh-cluster-per-phase property the
// vm-rig arc established is enforced by the executor, not the
// phase.
type Phase string

const (
	// PhaseBaseline runs the perf-server unannotated, so the
	// k3s-bundled bandwidth plugin is inert for it. The unshaped-
	// wire reference.
	PhaseBaseline Phase = "baseline"

	// PhaseVanilla keeps the annotations and patches the bundled
	// bandwidth plugin's per-pod TBF burst down to 1 MB via
	// Substrate.NodeShell — without the patch the kubelet default
	// burst (~150s of credit) leaves a short run effectively
	// unshaped.
	PhaseVanilla Phase = "vanilla"

	// PhaseNatra chains natra via Substrate.InstallNatra. Its CMS
	// fast-passes small fresh-flow HTTP requests around the token
	// bucket while elephants pay.
	PhaseNatra Phase = "natra"
)

// Rate is an annotated bandwidth cap exercised by the iperf sweep.
// The string form matches what Kubernetes wants in
// `kubernetes.io/{ingress,egress}-bandwidth` annotations.
type Rate string

const (
	Rate10M Rate = "10M"
	Rate1G  Rate = "1G"
	Rate10G Rate = "10G"
)

// WorkloadKind selects one of the workloads the executor knows
// how to run. New workloads are added by extending this enum +
// implementing the corresponding workload_*.go.
type WorkloadKind string

const (
	// WorkloadIperfSweep — per rate, per direction, elephant +
	// parallel mice. Reports bps per cell.
	WorkloadIperfSweep WorkloadKind = "iperfSweep"

	// WorkloadMixed — annotated elephant via iperf3 --bidir, hey
	// HTTP mice, and an unannotated bystander on the same worker.
	// Reports annotated mice RPS/p50/p99 (CMS fast-pass) and
	// bystander RPS/p99 (collateral cost).
	WorkloadMixed WorkloadKind = "mixed"

	// WorkloadMemory — three memory comparables per phase: dataplane
	// kernel memory per annotated pod (1→N slope; qdisc for vanilla,
	// BPF for natra), CNI plugin invocation peak RSS, persistent
	// installer DaemonSet RSS. baseline = 0 (measured as the noise
	// floor).
	WorkloadMemory WorkloadKind = "memory"
)

// Spec is the full experiment definition — the union of everything
// either rig can run. A [Profile] selects a subset for an actual
// run. The Spec value lives in DefaultSpec; anything not in
// DefaultSpec.Rates / .Workloads cannot appear in any profile
// ([Apply] validates this).
type Spec struct {
	// Phases is always all three. Skipping a phase isn't a
	// meaningful profile choice — the comparison only makes sense
	// across all of baseline/vanilla/natra.
	Phases []Phase

	// Rates is the full rate sweep (typically 10M, 1G, 10G). The CI
	// profile narrows this to keep run time bounded.
	Rates []Rate

	// Workloads is every workload kind. A profile may run a subset.
	Workloads []WorkloadKind

	// Samples is the maximum sample count the spec permits. A
	// profile's Samples must be ≤ Spec.Samples.
	Samples int
}

// Profile narrows a Spec for an actual run. CI runs the `ci`
// profile (subset of rates/workloads, fewer samples); local
// vm-rig runs `full` (everything).
//
// [Apply] rejects profiles that widen the spec — a profile can
// only ever select less, never more. This makes "k3d ⊂ vm-rig"
// a structural property: the profile system cannot express a
// k3d-only test.
type Profile struct {
	Name      string
	Rates     []Rate
	Workloads []WorkloadKind
	Samples   int
}

// DefaultSpec is the spec that drives both rigs today. Adding a
// rate or workload here makes it available to all profiles; no
// profile can introduce one that isn't here.
var DefaultSpec = Spec{
	Phases:    []Phase{PhaseBaseline, PhaseVanilla, PhaseNatra},
	Rates:     []Rate{Rate10M, Rate1G, Rate10G},
	Workloads: []WorkloadKind{WorkloadIperfSweep, WorkloadMixed, WorkloadMemory},
	Samples:   3,
}

// ProfileFull runs the entire Spec — vm-rig's local high-fidelity
// pass.
var ProfileFull = Profile{
	Name:      "full",
	Rates:     []Rate{Rate10M, Rate1G, Rate10G},
	Workloads: []WorkloadKind{WorkloadIperfSweep, WorkloadMixed, WorkloadMemory},
	Samples:   3,
}

// ProfileCI is the GitHub-runnable subset: a single rate, all three
// workloads, one sample. Sized to fit the current
// perf-vs-vanilla.sh ~18-22 min window.
var ProfileCI = Profile{
	Name:      "ci",
	Rates:     []Rate{Rate10M},
	Workloads: []WorkloadKind{WorkloadIperfSweep, WorkloadMixed, WorkloadMemory},
	Samples:   1,
}
