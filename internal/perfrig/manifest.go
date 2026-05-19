package perfrig

import (
	"io"
	"os"
	"strings"
)

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
