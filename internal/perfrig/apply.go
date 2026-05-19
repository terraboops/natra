package perfrig

import (
	"fmt"
	"strings"
)

// Plan is the concrete, validated run description the executor
// consumes — the result of narrowing a Spec by a Profile. The
// executor never reads the Spec or Profile directly; Plan is the
// only thing that's actually run, so the subset relationship is
// guaranteed once Plan exists.
type Plan struct {
	Phases    []Phase
	Rates     []Rate
	Workloads []WorkloadKind
	Samples   int
	Profile   string // for the report header
}

// Apply narrows spec by profile, validating that the profile only
// selects values that are already in the spec and a sample count
// that is at most the spec's. Returns an error rather than silently
// widening — see the package doc on the "k3d ⊂ vm-rig" property.
//
// Validation is intentionally strict: a profile that names a rate
// or workload absent from the spec is a typo or a drift attempt
// (e.g. adding a k3d-only rate), and we want the build to fail
// rather than the run to silently include it.
func Apply(spec Spec, profile Profile) (Plan, error) {
	rateSet := make(map[Rate]struct{}, len(spec.Rates))
	for _, r := range spec.Rates {
		rateSet[r] = struct{}{}
	}
	wkSet := make(map[WorkloadKind]struct{}, len(spec.Workloads))
	for _, w := range spec.Workloads {
		wkSet[w] = struct{}{}
	}

	for _, r := range profile.Rates {
		if _, ok := rateSet[r]; !ok {
			return Plan{}, fmt.Errorf("profile %q: rate %q not in spec (have: %s)",
				profile.Name, r, joinRates(spec.Rates))
		}
	}
	for _, w := range profile.Workloads {
		if _, ok := wkSet[w]; !ok {
			return Plan{}, fmt.Errorf("profile %q: workload %q not in spec (have: %s)",
				profile.Name, w, joinWorkloads(spec.Workloads))
		}
	}
	if profile.Samples < 1 {
		return Plan{}, fmt.Errorf("profile %q: Samples must be >= 1 (got %d)",
			profile.Name, profile.Samples)
	}
	if profile.Samples > spec.Samples {
		return Plan{}, fmt.Errorf("profile %q: Samples=%d exceeds spec.Samples=%d (a profile can narrow, never widen)",
			profile.Name, profile.Samples, spec.Samples)
	}
	if len(profile.Rates) == 0 {
		return Plan{}, fmt.Errorf("profile %q: no rates selected", profile.Name)
	}
	if len(profile.Workloads) == 0 {
		return Plan{}, fmt.Errorf("profile %q: no workloads selected", profile.Name)
	}

	return Plan{
		Phases:    spec.Phases, // always all three; profiles don't narrow phases
		Rates:     profile.Rates,
		Workloads: profile.Workloads,
		Samples:   profile.Samples,
		Profile:   profile.Name,
	}, nil
}

func joinRates(rs []Rate) string {
	out := make([]string, len(rs))
	for i, r := range rs {
		out[i] = string(r)
	}
	return strings.Join(out, ",")
}

func joinWorkloads(ws []WorkloadKind) string {
	out := make([]string, len(ws))
	for i, w := range ws {
		out[i] = string(w)
	}
	return strings.Join(out, ",")
}
