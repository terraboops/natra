package perfrig

import (
	"strings"
	"testing"
)

// TestApply_FullPassesThrough — the full profile is, by
// construction, the entire spec; Apply must accept it and emit a
// Plan with exactly the spec's rates and workloads.
func TestApply_FullPassesThrough(t *testing.T) {
	plan, err := Apply(DefaultSpec, ProfileFull)
	if err != nil {
		t.Fatalf("Apply(full): %v", err)
	}
	if len(plan.Rates) != len(DefaultSpec.Rates) {
		t.Errorf("full plan rates: got %v, want all of %v", plan.Rates, DefaultSpec.Rates)
	}
	if len(plan.Workloads) != len(DefaultSpec.Workloads) {
		t.Errorf("full plan workloads: got %v, want all of %v", plan.Workloads, DefaultSpec.Workloads)
	}
	if plan.Samples != ProfileFull.Samples {
		t.Errorf("full plan Samples: got %d, want %d", plan.Samples, ProfileFull.Samples)
	}
}

// TestApply_CINarrows — the ci profile must be strictly narrower
// than the spec on rates and samples; Apply must accept it without
// complaint.
func TestApply_CINarrows(t *testing.T) {
	plan, err := Apply(DefaultSpec, ProfileCI)
	if err != nil {
		t.Fatalf("Apply(ci): %v", err)
	}
	if len(plan.Rates) >= len(DefaultSpec.Rates) {
		t.Errorf("ci should narrow rates: plan has %d, spec has %d", len(plan.Rates), len(DefaultSpec.Rates))
	}
	if plan.Samples > DefaultSpec.Samples {
		t.Errorf("ci Samples=%d > spec Samples=%d (should narrow, not widen)", plan.Samples, DefaultSpec.Samples)
	}
}

// TestApply_CISubsetOfFull — the structural invariant. This is the
// test that makes "k3d ⊂ vm-rig" a compile/test guarantee rather
// than a convention: if a future change adds a rate or workload to
// ci that is not in full, this test fails before it lands.
func TestApply_CISubsetOfFull(t *testing.T) {
	full := setOfRates(ProfileFull.Rates)
	for _, r := range ProfileCI.Rates {
		if _, ok := full[r]; !ok {
			t.Errorf("ci rate %q is NOT in full profile — ci must be a subset of full", r)
		}
	}
	fullW := setOfWorkloads(ProfileFull.Workloads)
	for _, w := range ProfileCI.Workloads {
		if _, ok := fullW[w]; !ok {
			t.Errorf("ci workload %q is NOT in full profile — ci must be a subset of full", w)
		}
	}
	if ProfileCI.Samples > ProfileFull.Samples {
		t.Errorf("ci Samples=%d > full Samples=%d — ci must be a subset of full",
			ProfileCI.Samples, ProfileFull.Samples)
	}
}

// TestApply_RejectsRateOutsideSpec — a profile that names a rate
// the spec doesn't have should fail at Apply time, not at run time.
// This catches drift like "someone added Rate100G to ProfileCI but
// forgot DefaultSpec".
func TestApply_RejectsRateOutsideSpec(t *testing.T) {
	bad := Profile{
		Name:      "bad",
		Rates:     []Rate{Rate10M, "100G"}, // 100G not in DefaultSpec
		Workloads: []WorkloadKind{WorkloadIperfSweep},
		Samples:   1,
	}
	_, err := Apply(DefaultSpec, bad)
	if err == nil {
		t.Fatal("Apply with rate outside spec: expected error, got nil")
	}
	if !strings.Contains(err.Error(), "100G") {
		t.Errorf("error should name the offending rate; got %q", err.Error())
	}
}

// TestApply_RejectsWorkloadOutsideSpec — same as the rate case but
// for workload kinds.
func TestApply_RejectsWorkloadOutsideSpec(t *testing.T) {
	bad := Profile{
		Name:      "bad",
		Rates:     []Rate{Rate10M},
		Workloads: []WorkloadKind{"someFutureWorkload"},
		Samples:   1,
	}
	_, err := Apply(DefaultSpec, bad)
	if err == nil {
		t.Fatal("Apply with workload outside spec: expected error, got nil")
	}
	if !strings.Contains(err.Error(), "someFutureWorkload") {
		t.Errorf("error should name the offending workload; got %q", err.Error())
	}
}

// TestApply_RejectsSamplesOverSpec — a profile cannot widen the
// sample count beyond the spec's maximum.
func TestApply_RejectsSamplesOverSpec(t *testing.T) {
	bad := Profile{
		Name:      "bad",
		Rates:     []Rate{Rate10M},
		Workloads: []WorkloadKind{WorkloadIperfSweep},
		Samples:   DefaultSpec.Samples + 1,
	}
	_, err := Apply(DefaultSpec, bad)
	if err == nil {
		t.Fatal("Apply with Samples > spec.Samples: expected error, got nil")
	}
	if !strings.Contains(err.Error(), "exceeds") {
		t.Errorf("error should say the profile exceeds the spec; got %q", err.Error())
	}
}

// TestApply_RejectsZeroSamples — a profile with Samples=0 would
// produce a Plan that measures nothing; treat that as a config
// error rather than emitting an empty report.
func TestApply_RejectsZeroSamples(t *testing.T) {
	bad := Profile{
		Name:      "bad",
		Rates:     []Rate{Rate10M},
		Workloads: []WorkloadKind{WorkloadIperfSweep},
		Samples:   0,
	}
	if _, err := Apply(DefaultSpec, bad); err == nil {
		t.Fatal("Apply with Samples=0: expected error, got nil")
	}
}

// TestApply_RejectsEmptyRatesOrWorkloads — a profile with nothing
// to run is also a config error rather than a successful no-op.
func TestApply_RejectsEmptyRatesOrWorkloads(t *testing.T) {
	noRates := Profile{Name: "x", Workloads: []WorkloadKind{WorkloadIperfSweep}, Samples: 1}
	if _, err := Apply(DefaultSpec, noRates); err == nil {
		t.Error("Apply with no rates: expected error, got nil")
	}
	noWorkloads := Profile{Name: "x", Rates: []Rate{Rate10M}, Samples: 1}
	if _, err := Apply(DefaultSpec, noWorkloads); err == nil {
		t.Error("Apply with no workloads: expected error, got nil")
	}
}

func setOfRates(rs []Rate) map[Rate]struct{} {
	out := make(map[Rate]struct{}, len(rs))
	for _, r := range rs {
		out[r] = struct{}{}
	}
	return out
}

func setOfWorkloads(ws []WorkloadKind) map[WorkloadKind]struct{} {
	out := make(map[WorkloadKind]struct{}, len(ws))
	for _, w := range ws {
		out[w] = struct{}{}
	}
	return out
}
