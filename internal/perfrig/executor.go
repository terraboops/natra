package perfrig

import (
	"context"
	"fmt"
	"io"
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
	// Log is where progress prints; nil → os.Stdout via the rig's
	// CLI. The executor is otherwise side-effect-free in tests.
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
// optionally install → workloads → down (always). The deferred
// Down catches the error path so a workload failure never leaves
// a stale cluster to bias the next phase.
func (e *Executor) runPhase(ctx context.Context, phase Phase) (PhaseReport, error) {
	pr := PhaseReport{Phase: phase}

	// Clean any pre-existing cluster so Up genuinely starts fresh.
	_ = e.Substrate.Down(ctx)

	defer func() {
		e.logf("==> [%s] tearing down cluster\n", phase)
		_ = e.Substrate.Down(ctx)
	}()

	e.logf("==> [%s] bringing up a fresh cluster\n", phase)
	if err := e.Substrate.Up(ctx); err != nil {
		return pr, fmt.Errorf("cluster up: %w", err)
	}

	if phase == PhaseNatra {
		e.logf("==> [%s] installing natra (image + chained conflist)\n", phase)
		if err := e.Substrate.InstallNatra(ctx); err != nil {
			return pr, fmt.Errorf("natra install: %w", err)
		}
	}

	// Workloads land in the next slice; for now the phase loop just
	// proves the fresh-cluster contract end-to-end and records that
	// each workload was visited. The TODO is deliberate and visible
	// — anything missing in the report will surface as zeros, which
	// the report writer flags.
	for _, w := range e.Plan.Workloads {
		e.logf("==> [%s] workload %s (samples=%d) — pending wiring\n", phase, w, e.Plan.Samples)
		pr.Workloads = append(pr.Workloads, WorkloadReport{Kind: w})
	}

	return pr, nil
}

func (e *Executor) logf(format string, args ...any) {
	if e.Log == nil {
		return
	}
	_, _ = fmt.Fprintf(e.Log, format, args...)
}
