package main

import (
	"fmt"
	"os"
)

// cmdPerfVsVanilla is the planned local-developer driver for the
// natra-vs-upstream-bandwidth comparison. The script equivalent is
// scripts/perf-vs-vanilla.sh; that's the k3d path used in CI, where
// "nodes" are containers in one shared kernel and the dataplane is
// software-only.
//
// vm-rig perfvsvanilla will run the same three-phase comparison
// (baseline / natra / upstream bandwidth) against the two-VM lima
// cluster, where each "node" is a real Linux kernel with its own
// veth+IFB. That's the comparison the docs/perf-vs-vanilla.md gaps
// section calls "Cross-kernel wire" — currently a known unmet need
// at the L4 e2e and k3d perf layers.
//
// IMPLEMENTATION STATUS: scaffolding. Currently bails with a
// blocker message if connectivity is dead, or "not yet implemented"
// if it works. The phase code is intentionally absent — it'll get
// filled in once the cross-VM connectivity blocker is unblocked
// (see scripts/vm-rig/README.md "Cross-VM pod traffic blocker"
// section for the unblock paths).
//
// Planned phases (mirror of scripts/perf-vs-vanilla.sh's structure):
//
//  1. Baseline: uninstall natra DS, deploy unannotated server,
//     measure iperf-only rate sweep + mixed workload. Captures the
//     wire ceiling for the underlying lima/socket_vmnet path.
//  2. natra: reinstall natra DS, deploy annotated server (both
//     directions 10M), warmup buckets, measure same workloads.
//  3. upstream bandwidth: replace natra's conflist with the
//     upstream bandwidth chain (k3s already bundles it), warmup,
//     measure same workloads. Apply the same TBF-burst patch the
//     k3d script does (fix_vanilla_tbf_burst), reaching the lima
//     VM netns via limactl shell + tc.
//
// Phase 1 reuses the existing cmdUp/cmdInstall plumbing — it just
// runs without the install step. Phase 2 is what `vm-rig test`
// already exercises end-to-end. Phase 3 is the new work.
//
// Output: same comparison table shape as
// /tmp/natra-perf-vs-vanilla-result.txt, written to a vm-rig-
// specific path so the two rigs don't fight for the same file.
func cmdPerfVsVanilla(c *Config) error {
	if _, err := os.Stat(c.KubeconfigPath); err != nil {
		return fmt.Errorf("%s not found — run 'vm-rig up' first", c.KubeconfigPath)
	}
	env := []string{"KUBECONFIG=" + c.KubeconfigPath}

	// Connectivity gate: cross-VM iperf must work for the comparison
	// to mean anything. If it doesn't, every "measured" cell will be
	// 0 bps and the comparison is noise. The gate's failure mode is
	// the cross-VM connectivity blocker documented in
	// scripts/vm-rig/README.md; we emit a clear pointer to that and
	// bail rather than producing a misleading zero-row table.
	const namespace = "natra-vm-rig"
	const probeTarget = "iperf-server"
	fmt.Println("==> [perfvsvanilla] probing cross-VM pod connectivity")
	if err := waitForIperfConnect(env, namespace, probeTarget, 10); err != nil {
		return fmt.Errorf(
			"cross-VM iperf probe failed: %w\n"+
				"  vm-rig perfvsvanilla needs cross-VM pod traffic to work. The\n"+
				"  current macOS+lima+Debian rig has a known blocker; see\n"+
				"  scripts/vm-rig/README.md '### Cross-VM pod traffic blocker'\n"+
				"  for the three plausible unblock paths.\n"+
				"  Workaround for now: use the k3d-based comparison instead:\n"+
				"    make perf-vs-vanilla", err)
	}

	// Once the gate clears, the three-phase implementation goes here.
	// Tracking the structure inline so the next iteration filling
	// these in has a clear hook for each.
	fmt.Println("==> [perfvsvanilla] cross-VM connectivity OK")
	fmt.Println()
	fmt.Println("Three-phase comparison not yet implemented in cmd/vm-rig.")
	fmt.Println("Planned shape (see this file's docstring for detail):")
	fmt.Println("  Phase 0 — baseline (no plugin)")
	fmt.Println("  Phase A — natra (already installed by 'vm-rig install')")
	fmt.Println("  Phase B — upstream bandwidth (TBF burst patched in lima netns)")
	fmt.Println()
	fmt.Println("For the k3d-based comparison today, run: make perf-vs-vanilla")
	return nil
}
