// Command perfrig is the substrate-flexible entry into the shared
// perfrig executor. Today it drives k3d via k3dSubstrate; the lima
// path keeps its existing entry (`cmd/vm-rig perfvsvanilla` →
// limaSubstrate), so each binary owns its substrate's lifecycle
// primitives while running the same internal/perfrig executor.
//
// Usage:
//
//	perfrig --substrate=k3d --profile=ci
//	perfrig --substrate=k3d --profile=full --cluster=natra-perfrig
//
// Invoked by scripts/perf-vs-vanilla.sh and `make perf-vs-vanilla`
// after this consolidation. CI workflows continue to use those
// targets, so the CI surface doesn't change.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"runtime"

	"github.com/terraboops/natra/internal/perfrig"
)

func main() {
	var (
		substrate = flag.String("substrate", "k3d",
			"perfrig substrate: k3d (lima entry is cmd/vm-rig perfvsvanilla)")
		profile = flag.String("profile", "ci", "perfrig profile: ci or full")
		cluster = flag.String("cluster", "natra-perfrig", "k3d cluster name")
		image   = flag.String("image", "ghcr.io/terraboops/natra:perfrig",
			"natra image tag built and imported into the cluster")
		perfImg = flag.String("perfclient-image", "ghcr.io/terraboops/natra-perfclient:perfrig",
			"perfclient image tag (iperf3 + hey)")
	)
	flag.Parse()

	if *substrate != "k3d" {
		fmt.Fprintf(os.Stderr,
			"perfrig: --substrate=%q not supported here; "+
				"use 'cmd/vm-rig perfvsvanilla' for lima\n",
			*substrate)
		os.Exit(2)
	}

	var prof perfrig.Profile
	switch *profile {
	case "ci":
		prof = perfrig.ProfileCI
	case "full":
		prof = perfrig.ProfileFull
	default:
		fmt.Fprintf(os.Stderr, "perfrig: unknown --profile %q (want ci or full)\n", *profile)
		os.Exit(2)
	}

	plan, err := perfrig.Apply(perfrig.DefaultSpec, prof)
	if err != nil {
		fmt.Fprintln(os.Stderr, "apply profile:", err)
		os.Exit(1)
	}

	repoRoot := findRepoRoot()
	sub := newK3dSubstrate(*cluster, *image, *perfImg, repoRoot)

	fmt.Printf("==> [perfrig] substrate=%s profile=%s cluster=%s\n",
		sub.Name(), plan.Profile, *cluster)
	fmt.Printf("    phases=%v rates=%v workloads=%v samples=%d\n",
		plan.Phases, plan.Rates, plan.Workloads, plan.Samples)

	exec := &perfrig.Executor{
		Plan:            plan,
		Substrate:       sub,
		RepoRoot:        repoRoot,
		Namespace:       "natra-perfrig",
		PerfclientImage: *perfImg,
		MemoryNPodCount: 8,
		Log:             os.Stdout,
	}
	rep, err := exec.Run(context.Background())
	if err != nil {
		fmt.Fprintln(os.Stderr, "perfrig run:", err)
		os.Exit(1)
	}
	const resultPath = "/tmp/natra-k3d-perf-vs-vanilla-result.txt"
	if err := perfrig.WriteReport(rep, resultPath, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "write report:", err)
		os.Exit(1)
	}
}

// findRepoRoot walks up from this source file's directory until
// it finds a go.mod, the only stable repo-root marker that works
// from any build location.
func findRepoRoot() string {
	_, here, _, _ := runtime.Caller(0)
	dir := filepath.Dir(here)
	for i := 0; i < 8; i++ {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		dir = filepath.Dir(dir)
	}
	// Fall back to cwd; the rig will surface the path mismatch
	// when it tries to open a manifest.
	wd, _ := os.Getwd()
	return wd
}
