package main

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"os/exec"
)

// run executes a command with stdout/stderr inherited from the
// parent, so progress shows up live. Returns an error that names
// the command so failures are self-describing.
func run(name string, args ...string) error {
	cmd := exec.Command(name, args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s %v: %w", name, args, err)
	}
	return nil
}

// kubectl runs `kubectl <args...>` with an extra environment overlay
// (typically KUBECONFIG=...) and an optional stdin reader for
// streaming rendered manifests into `apply -f -`. Every kubectl
// invocation in this binary goes through here so KUBECONFIG handling
// can't drift between call sites.
func kubectl(env []string, stdin io.Reader, args ...string) error {
	cmd := exec.Command("kubectl", args...)
	cmd.Env = append(os.Environ(), env...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Stdin = stdin
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("kubectl %v: %w", args, err)
	}
	return nil
}

// capture runs a command and returns its stdout as a trimmed
// string. Errors include the captured stderr — important when the
// command failed for an obvious reason that we want to surface.
func capture(name string, args ...string) (string, error) {
	cmd := exec.Command(name, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	if err != nil {
		return "", fmt.Errorf("%s %v: %w (stderr: %s)", name, args, err, stderr.String())
	}
	return trimRight(stdout.String()), nil
}

// captureKubectl runs `kubectl <args...>` with an extra environment
// overlay (KUBECONFIG=...) and captures stdout. Errors include
// the captured stderr — important when kubectl fails for an
// obvious reason we want to surface (RBAC, missing namespace, etc).
func captureKubectl(env []string, args ...string) (string, error) {
	cmd := exec.Command("kubectl", args...)
	cmd.Env = append(os.Environ(), env...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	if err != nil {
		return "", fmt.Errorf("kubectl %v: %w (stderr: %s)", args, err, stderr.String())
	}
	return trimRight(stdout.String()), nil
}

func trimRight(s string) string {
	for len(s) > 0 && (s[len(s)-1] == '\n' || s[len(s)-1] == ' ' || s[len(s)-1] == '\t') {
		s = s[:len(s)-1]
	}
	return s
}
