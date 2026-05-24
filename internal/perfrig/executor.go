package perfrig

import (
	"context"
	"fmt"
	"io"
	"os"
)

// Executor runs a Plan against a Substrate, one phase at a time,
// each on a fresh cluster, and produces a Report. The phase loop
// is the substrate-independent core; per-phase work (Up/Down,
// install, workload execution, memory measurement) calls through
// the Substrate or into substrate-agnostic helpers in this package.
//
// The fresh-cluster-per-phase contract is enforced HERE rather
// than in any substrate: Down → Up → optionally InstallNatra at
// the top of each phase, and an unconditional Down deferred at
// the bottom so half-measured phases never leak state into the
// next. The vm-rig arc established this as required for per-phase
// latency numbers to mean anything; baking it into the executor
// keeps every substrate honest.
type Executor struct {
	Plan      Plan
	Substrate Substrate

	// RepoRoot is the absolute path to natra/ on the machine
	// orchestrating the run. The executor opens manifest templates
	// under test/perf/realworld/ relative to this.
	RepoRoot string

	// Namespace is the workload namespace. Created once per phase
	// after the fresh cluster comes up. Default "natra-perfrig".
	Namespace string

	// PerfclientImage is the iperf3+hey image tag that's been
	// imported into the substrate's nodes. The perf-server /
	// perf-client manifests reference it via the template
	// placeholder.
	PerfclientImage string

	// MemoryNPodCount is the N in the memory workload's 1→N pod
	// slope. Default 8; configurable so a future tighter or looser
	// pod sweep can be requested without re-coding.
	MemoryNPodCount int

	// Log is where progress prints; nil silences. The executor is
	// otherwise side-effect-free in tests.
	Log io.Writer
}

// Run executes every phase in plan.Phases against substrate and
// returns the assembled Report. The phase loop is the only place
// fresh-cluster discipline is enforced — see the type doc.
func (e *Executor) Run(ctx context.Context) (Report, error) {
	rep := Report{
		Substrate: e.Substrate.Name(),
		Profile:   e.Plan.Profile,
	}
	// Log the requested NATRA_ATTACH_MODE up front so post-mortems
	// don't have to guess what auto resolved to; per-phase
	// stage.go grep against /var/log/natra-cni.log adds the actual
	// per-direction side that natra picked.
	mode := os.Getenv("NATRA_ATTACH_MODE")
	if mode == "" {
		mode = "auto (chain: tcx-pod → clsact-pod → tcx-host → clsact-host)"
	}
	e.logf("==> [perfrig] NATRA_ATTACH_MODE requested: %s\n", mode)
	for _, ph := range e.Plan.Phases {
		e.logf("\n========== PHASE %s — fresh cluster ==========\n", ph)
		phaseRep, err := e.runPhase(ctx, ph)
		if err != nil {
			return rep, fmt.Errorf("%s phase: %w", ph, err)
		}
		rep.Phases = append(rep.Phases, phaseRep)
	}
	return rep, nil
}

// runPhase implements the fresh-cluster contract: down → up →
// optionally install → stage workload pods → workloads → down
// (always). The deferred Down catches the error path so a workload
// failure never leaves a stale cluster to bias the next phase.
func (e *Executor) runPhase(ctx context.Context, phase Phase) (PhaseReport, error) {
	pr := PhaseReport{Phase: phase}

	_ = e.Substrate.Down(ctx) // wipe any stale state before Up
	defer func() {
		e.logf("==> [%s] tearing down cluster\n", phase)
		_ = e.Substrate.Down(ctx)
	}()

	e.logf("==> [%s] bringing up a fresh cluster\n", phase)
	if err := e.Substrate.Up(ctx); err != nil {
		return pr, fmt.Errorf("cluster up: %w", err)
	}

	// Every phase needs the perfclient image (perf-client pod
	// stays ImagePullBackOff otherwise). Importing unconditionally
	// before any phase-specific install removes the easy bug class
	// of "we forgot perfclient in this branch" — lima got bit by
	// it (no perfclient in baseline/vanilla), k3d got bit by it
	// (no perfclient in natra). docker build is cached so the
	// repeated calls are cheap.
	e.logf("==> [%s] importing perfclient image\n", phase)
	if err := e.Substrate.ImportImage(ctx, e.PerfclientImage, "Dockerfile.perfclient"); err != nil {
		return pr, fmt.Errorf("import perfclient: %w", err)
	}

	if phase == PhaseNatra {
		e.logf("==> [%s] installing natra (image + chained conflist)\n", phase)
		if err := e.Substrate.InstallNatra(ctx); err != nil {
			return pr, fmt.Errorf("natra install: %w", err)
		}
	}

	// One-time per-phase stage: namespace, perf-server, perf-client,
	// connectivity gate, vanilla TBF patch (vanilla phase only), and
	// warmup. Done once so the workloads below run against a stable
	// steady state.
	if err := e.stagePhase(ctx, phase); err != nil {
		return pr, fmt.Errorf("stage phase: %w", err)
	}

	for _, w := range e.Plan.Workloads {
		e.logf("==> [%s] workload %s (samples=%d)\n", phase, w, e.Plan.Samples)
		wr, err := e.runWorkload(ctx, phase, w)
		if err != nil {
			return pr, fmt.Errorf("workload %s: %w", w, err)
		}
		pr.Workloads = append(pr.Workloads, wr)
	}
	return pr, nil
}

// runWorkload dispatches to the per-workload implementation. New
// workloads add a case here and a workload_*.go file; the
// interface for each is uniform — they return a fully-populated
// WorkloadReport for the (phase, plan) combination.
func (e *Executor) runWorkload(ctx context.Context, phase Phase, kind WorkloadKind) (WorkloadReport, error) {
	wr := WorkloadReport{Kind: kind}
	switch kind {
	case WorkloadIperfSweep:
		return e.runIperfSweep(ctx, phase)
	case WorkloadMixed:
		return e.runMixed(ctx, phase)
	case WorkloadMemory:
		return e.runMemory(ctx, phase)
	default:
		return wr, fmt.Errorf("unknown workload kind %q", kind)
	}
}

func (e *Executor) logf(format string, args ...any) {
	if e.Log == nil {
		return
	}
	_, _ = fmt.Fprintf(e.Log, format, args...)
}
