package perfrig

import (
	"fmt"
	"io"
	"os"
	"strings"
)

// ApplyNatraAttachModeEnv lets the caller override the installer
// manifest's default `NATRA_ATTACH_MODE: ""` (which the binary
// resolves to the auto fallback chain). Forcing an explicit mode
// is the knob for compositions where auto's first pick is
// bypassed by another dataplane — cilium with kube-proxy-
// replacement routes pod-to-pod through host-side BPF, bypassing
// pod-eth0 where tcx-podside attaches; tcx-hostside puts natra
// where cilium delivers traffic to.
//
// The substitution is a literal string replace of the empty env
// value to the requested one. Whitespace and key context are
// included so other empty-quoted env values (NATRA_EDT_PACING, …)
// aren't accidentally rewritten.
func ApplyNatraAttachModeEnv(manifest string) string {
	mode := os.Getenv("NATRA_ATTACH_MODE")
	if mode == "" {
		return manifest
	}
	const oldEnv = "- name: NATRA_ATTACH_MODE\n              value: \"\""
	const newFmt = "- name: NATRA_ATTACH_MODE\n              value: %q"
	return strings.Replace(manifest, oldEnv,
		fmt.Sprintf(newFmt, mode), 1)
}

// renderManifest reads a YAML file and applies a string-replacer.
// Same shape as cmd/vm-rig's helper; pulled into the package so the
// executor can render manifests without depending on cmd/vm-rig.
func renderManifest(src string, repl *strings.Replacer) (string, error) {
	f, err := os.Open(src) //nolint:gosec // path comes from the rig itself, not user input
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	b, err := io.ReadAll(f)
	if err != nil {
		return "", err
	}
	return repl.Replace(string(b)), nil
}

// renderPerfManifest substitutes the perf-server / perf-client
// template placeholders. Matches cmd/vm-rig's existing renderer so
// the manifests in test/perf/realworld stay one source of truth
// for both rigs.
func renderPerfManifest(repoRoot, relPath, namespace, serverNode, workerNode, image string) (string, error) {
	src := repoRoot + "/" + relPath
	return renderManifest(src, strings.NewReplacer(
		"PERF_WORKER_NODE", workerNode,
		"PERF_CONTROL_NODE", serverNode,
		"namespace: natra-e2e", "namespace: "+namespace,
		"ghcr.io/terraboops/natra-perfclient:vsperf", image,
	))
}

// stripBandwidthAnnotations removes the kubernetes.io/{ingress,
// egress}-bandwidth annotations from a rendered manifest. Used by
// the baseline phase so the k3s-bundled bandwidth plugin is inert
// for the perf-server — without this strip, the elephant runs
// under the bundled TBF with kubelet's huge default burst (and
// "baseline" stops being an unshaped wire).
func stripBandwidthAnnotations(manifest string) string {
	lines := strings.Split(manifest, "\n")
	keep := make([]string, 0, len(lines))
	for _, line := range lines {
		if strings.Contains(line, "kubernetes.io/ingress-bandwidth") ||
			strings.Contains(line, "kubernetes.io/egress-bandwidth") {
			continue
		}
		keep = append(keep, line)
	}
	return strings.Join(keep, "\n")
}
