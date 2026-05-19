package perfrig

import (
	"bytes"
	"context"
	"io"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// repoRoot returns the natra/ root for the test, derived from this
// source file's location. Lets stagePhase render real manifests
// even though kubectl is stubbed.
func repoRoot() string {
	_, here, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(here), "..", "..")
}

// planContractOnly is a workload-less Plan, used by the phase-
// loop tests so they don't touch real kubectl. Constructed
// directly rather than via Apply (which rightly rejects empty
// workloads) — these tests are about the substrate call sequence,
// not the workload code.
func planContractOnly() Plan {
	return Plan{
		Phases:    DefaultSpec.Phases,
		Rates:     []Rate{Rate10M},
		Workloads: nil,
		Samples:   1,
		Profile:   "test",
	}
}

// TestExecutor_FreshClusterPerPhase — the executor's central
// contract. Each phase must see Down → Up at the top and a
// trailing Down at the bottom, regardless of phase. Tested via
// the call log on the fake substrate.
func TestExecutor_FreshClusterPerPhase(t *testing.T) {
	withStubKubectl(t)
	fake := NewFakeSubstrate()
	plan := planContractOnly()
	var logBuf bytes.Buffer
	e := &Executor{Plan: plan, Substrate: fake, RepoRoot: repoRoot(), Log: &logBuf}
	if _, err := e.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}

	// Each phase: opening Down, then Up, then (natra only) Install,
	// then the deferred Down. We expect exactly 3 phases × 2 Downs
	// + 3 Ups + 1 InstallNatra in the call log.
	var ups, downs, installs int
	for _, c := range fake.Calls {
		switch c {
		case "Up":
			ups++
		case "Down":
			downs++
		case "InstallNatra":
			installs++
		}
	}
	if ups != 3 {
		t.Errorf("Up calls: got %d, want 3 (one per phase)", ups)
	}
	if downs != 6 {
		t.Errorf("Down calls: got %d, want 6 (opening + deferred per phase)", downs)
	}
	if installs != 1 {
		t.Errorf("InstallNatra calls: got %d, want 1 (natra phase only)", installs)
	}
}

// TestExecutor_PhaseOrder — the executor must run phases in the
// order DefaultSpec declares (baseline, vanilla, natra). The order
// matters only for the log; cross-phase isolation is the
// fresh-cluster contract, not order. But the test pins the
// expected sequence so accidental reordering shows up in the diff.
func TestExecutor_PhaseOrder(t *testing.T) {
	withStubKubectl(t)
	fake := NewFakeSubstrate()
	plan := planContractOnly()
	var logBuf bytes.Buffer
	e := &Executor{Plan: plan, Substrate: fake, RepoRoot: repoRoot(), Log: &logBuf}
	rep, err := e.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	got := []Phase{}
	for _, p := range rep.Phases {
		got = append(got, p.Phase)
	}
	want := []Phase{PhaseBaseline, PhaseVanilla, PhaseNatra}
	if len(got) != len(want) {
		t.Fatalf("phase count: got %v, want %v", got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Errorf("phase[%d]: got %s, want %s", i, got[i], want[i])
		}
	}
}

// TestExecutor_WorkloadsRecorded — every workload in the plan must
// appear in every phase's report (so the report writer can render
// every cell, even if a workload's data is empty pending wiring).
func TestExecutor_WorkloadsRecorded(t *testing.T) {
	withStubKubectl(t)
	fake := NewFakeSubstrate()
	// Workload-less plan: the workload dispatch is exercised by the
	// individual workload tests; this one only asserts the phase
	// loop preserves the empty workload list per phase.
	plan := planContractOnly()
	e := &Executor{Plan: plan, Substrate: fake, RepoRoot: repoRoot(), Log: &bytes.Buffer{}}
	rep, err := e.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	for _, p := range rep.Phases {
		if len(p.Workloads) != 0 {
			t.Errorf("phase %s: got %d workloads, want 0 (none in plan)", p.Phase, len(p.Workloads))
		}
	}
}

// TestExecutor_DeferredDownOnError — a failing Up in the middle of
// a phase must still trigger the deferred Down, so the next phase
// starts from a clean slate. The fresh-cluster guarantee has to
// hold on the error path too, not just the happy path.
func TestExecutor_DeferredDownOnError(t *testing.T) {
	withStubKubectl(t)
	fake := NewFakeSubstrate()
	fake.UpErr = errBoom
	plan := planContractOnly()
	e := &Executor{Plan: plan, Substrate: fake, RepoRoot: repoRoot(), Log: &bytes.Buffer{}}
	_, err := e.Run(context.Background())
	if err == nil {
		t.Fatal("Run with Up error: expected error, got nil")
	}
	if !strings.Contains(err.Error(), "cluster up") {
		t.Errorf("error should wrap the cluster-up failure; got %q", err.Error())
	}
	// One opening Down, then a failed Up, then the deferred Down.
	// The executor returns from the first phase, so only that
	// phase's calls are present.
	want := []string{"Down", "Up", "Down"}
	if len(fake.Calls) != len(want) {
		t.Fatalf("call sequence: got %v, want %v", fake.Calls, want)
	}
	for i, c := range fake.Calls {
		if c != want[i] {
			t.Errorf("call[%d]: got %q, want %q", i, c, want[i])
		}
	}
}

// errBoom is the canonical "expected error" for table tests; named
// rather than literal so a future refactor can match on identity.
var errBoom = &boomError{}

type boomError struct{}

func (*boomError) Error() string { return "boom" }

// withStubKubectl swaps the package-level kubectl/captureKubectl
// for no-ops + a tiny canned iperf JSON, restoring on test
// cleanup. The phase-contract tests do not exercise the workload
// code, but stagePhase calls kubectl for the namespace + manifest
// apply + wait; this stub keeps them honest about the call shape
// without needing a real cluster.
func withStubKubectl(t *testing.T) {
	t.Helper()
	prevK := kubectl
	prevC := captureKubectl
	kubectl = func(_ context.Context, _ string, _ io.Reader, _ ...string) error { return nil }
	captureKubectl = func(_ context.Context, _ string, _ ...string) (string, error) {
		// Connectivity-gate poll expects nonzero
		// end.sum_received.bits_per_second to declare "connected".
		return `{"end":{"sum_received":{"bits_per_second":1.0}}}`, nil
	}
	t.Cleanup(func() {
		kubectl = prevK
		captureKubectl = prevC
	})
}
