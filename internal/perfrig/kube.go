package perfrig

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
)

// Substrate-agnostic exec helpers. Everything in here only needs
// KUBECONFIG; it deliberately does not touch the Substrate, so the
// shared executor never blurs the seam.

// Indirected through package-level vars so tests can swap them
// out — the executor's phase loop and workload code call kubectl
// / captureKubectl extensively, and we want L1 tests against the
// fake substrate to exercise that code path without a real cluster.
var (
	kubectl        = realKubectl
	captureKubectl = realCaptureKubectl
)

// realKubectl runs `kubectl <args...>` with the substrate's
// kubeconfig in the environment, streaming stdout/stderr to the
// caller's stdout. Optional stdin lets the caller pipe a rendered
// manifest into `apply -f -`. Every kubectl call in this package
// routes through here so kubeconfig handling can't drift between
// sites.
func realKubectl(ctx context.Context, kubeconfig string, stdin io.Reader, args ...string) error {
	cmd := exec.CommandContext(ctx, "kubectl", args...)
	cmd.Env = append(os.Environ(), "KUBECONFIG="+kubeconfig)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Stdin = stdin
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("kubectl %v: %w", args, err)
	}
	return nil
}

// realCaptureKubectl is realKubectl + capture stdout for parsing.
// Errors include stderr so an RBAC / missing-namespace failure
// surfaces in the error message rather than only in the streamed
// log.
func realCaptureKubectl(ctx context.Context, kubeconfig string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "kubectl", args...)
	cmd.Env = append(os.Environ(), "KUBECONFIG="+kubeconfig)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("kubectl %v: %w (stderr: %s)", args, err, stderr.String())
	}
	return stdout.String(), nil
}
