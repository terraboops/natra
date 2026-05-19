package main

import (
	"context"
	"fmt"
	"os"

	"github.com/terraboops/natra/internal/perfrig"
)

// cmdPerfVsVanilla is the lima entrypoint into the shared perfrig
// executor. The actual measurement code (phase loop, workloads,
// memory measurement, report writer) lives in internal/perfrig
// so both rigs run identical logic — the only freedom the k3d rig
// has is which Profile it selects.
//
// PVV_PROFILE selects the profile name; default "full" (the
// vm-rig is the source of truth, so vm-rig defaults to running
// the entire spec). Override with PVV_PROFILE=ci to run the
// CI-shaped subset on lima for parity sanity-checks.
func cmdPerfVsVanilla(c *Config) error {
	const resultPath = "/tmp/natra-vm-rig-perf-vs-vanilla-result.txt"

	profile := perfrig.ProfileFull
	if os.Getenv("PVV_PROFILE") == "ci" {
		profile = perfrig.ProfileCI
	}

	plan, err := perfrig.Apply(perfrig.DefaultSpec, profile)
	if err != nil {
		return fmt.Errorf("apply profile: %w", err)
	}

	fmt.Printf("==> [perfvsvanilla] profile=%s phases=%v rates=%v workloads=%v samples=%d\n",
		plan.Profile, plan.Phases, plan.Rates, plan.Workloads, plan.Samples)

	exec := &perfrig.Executor{
		Plan:            plan,
		Substrate:       newLimaSubstrate(c),
		RepoRoot:        c.RepoRoot,
		Namespace:       "natra-vm-rig",
		PerfclientImage: c.PerfclientImage,
		MemoryNPodCount: 8,
		Log:             os.Stdout,
	}
	rep, err := exec.Run(context.Background())
	if err != nil {
		return err
	}
	return perfrig.WriteReport(rep, resultPath, os.Stdout)
}
