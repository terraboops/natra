package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// cmdUp brings up both VMs and joins them into one k3s cluster.
// Idempotent: re-running against already-existing VMs is a no-op
// modulo lima's "already running" message.
func cmdUp(c *Config) error {
	if err := requirePrereqs(); err != nil {
		return err
	}
	if err := checkSocketVmnet(); err != nil {
		return err
	}

	// Stage 1: server VM.
	fmt.Printf("==> bringing up %s\n", c.ServerName)
	if !limaExists(c.ServerName) {
		if err := run("limactl", "create", "--name", c.ServerName,
			filepath.Join(c.RigDir, "lima-server.yaml")); err != nil {
			return err
		}
	}
	if err := run("limactl", "start", c.ServerName); err != nil {
		return err
	}

	fmt.Println("==> waiting for k3s server to finish provisioning")
	tokenPath := "/etc/natra-node-token"
	if err := waitForFile(c.ServerName, tokenPath, 90, 2*time.Second); err != nil {
		// Dump k3s logs to make timeouts diagnosable.
		_ = run("limactl", "shell", c.ServerName, "--", "sudo", "journalctl", "-u", "k3s", "--no-pager", "-n", "30")
		return fmt.Errorf("k3s server provisioning timed out: %w", err)
	}

	token, err := capture("limactl", "shell", c.ServerName, "--", "cat", tokenPath)
	if err != nil {
		return err
	}
	serverIP, err := capture("limactl", "shell", c.ServerName, "--", "cat", "/etc/natra-server-ip")
	if err != nil {
		return err
	}
	fmt.Printf("==> server up at %s, token captured (%d chars)\n", serverIP, len(token))

	// Stage 2: agent VM. Render the agent YAML with the join URL +
	// token inlined — limactl's --set syntax for nested env keys
	// isn't stable across versions, and inlining is the same number
	// of lines with no version coupling.
	fmt.Printf("==> bringing up %s\n", c.AgentName)
	if !limaExists(c.AgentName) {
		rendered, err := renderAgentYAML(c, serverIP, token)
		if err != nil {
			return err
		}
		defer func() { _ = os.Remove(rendered) }()

		if err := run("limactl", "create", "--name", c.AgentName, rendered); err != nil {
			return err
		}
	}
	if err := run("limactl", "start", c.AgentName); err != nil {
		return err
	}

	// Stage 3: export kubeconfig. k3s writes the kubeconfig with
	// server: https://127.0.0.1:6443 — rewrite to the shared-network
	// IP so the host's kubectl can reach the API server.
	fmt.Printf("==> exporting kubeconfig to %s\n", c.KubeconfigPath)
	kc, err := capture("limactl", "shell", c.ServerName, "--", "sudo", "cat", "/etc/rancher/k3s/k3s.yaml")
	if err != nil {
		return err
	}
	kc = strings.Replace(kc,
		"server: https://127.0.0.1:6443",
		"server: https://"+serverIP+":6443", 1)
	if err := os.WriteFile(c.KubeconfigPath, []byte(kc), 0o600); err != nil {
		return err
	}

	// Stage 4: wait for both nodes to register as Ready.
	fmt.Println("==> waiting for both nodes Ready")
	if err := waitForNodesReady(c, 2, 60, 2*time.Second); err != nil {
		_, _ = captureKubectl([]string{"KUBECONFIG=" + c.KubeconfigPath}, "get", "nodes")
		return err
	}
	if err := run("env", "KUBECONFIG="+c.KubeconfigPath, "kubectl", "get", "nodes", "-o", "wide"); err != nil {
		return err
	}
	fmt.Printf("\nvm-rig up. Use it with: export KUBECONFIG=%s\n", c.KubeconfigPath)
	return nil
}

func requirePrereqs() error {
	for _, bin := range []string{"limactl", "kubectl", "docker"} {
		if _, err := capture("command", "-v", bin); err != nil {
			return fmt.Errorf("%s not on PATH (need: limactl, kubectl, docker)", bin)
		}
	}
	return nil
}

func checkSocketVmnet() error {
	if runtime.GOOS != "darwin" {
		return nil
	}
	for _, path := range []string{
		"/opt/homebrew/var/run/socket_vmnet",
		"/usr/local/var/run/socket_vmnet",
	} {
		if _, err := os.Stat(path); err == nil {
			return nil
		}
	}
	return errors.New("socket_vmnet is required for VM-to-VM networking on macOS.\n" +
		"  Install: brew install socket_vmnet\n" +
		"  Start:   sudo brew services start socket_vmnet")
}

func limaExists(name string) bool {
	out, err := capture("limactl", "list", name, "--format", "{{.Name}}")
	if err != nil {
		return false
	}
	return out == name
}

func waitForFile(vm, path string, attempts int, delay time.Duration) error {
	for i := 0; i < attempts; i++ {
		if _, err := capture("limactl", "shell", vm, "--", "test", "-f", path); err == nil {
			return nil
		}
		time.Sleep(delay)
	}
	return fmt.Errorf("%s not present in %s after %d attempts", path, vm, attempts)
}

func waitForNodesReady(c *Config, want, attempts int, delay time.Duration) error {
	env := []string{"KUBECONFIG=" + c.KubeconfigPath}
	for i := 0; i < attempts; i++ {
		out, err := captureKubectl(env, "get", "nodes", "--no-headers")
		if err == nil {
			ready := 0
			for _, line := range strings.Split(out, "\n") {
				fields := strings.Fields(line)
				if len(fields) >= 2 && fields[1] == "Ready" {
					ready++
				}
			}
			if ready >= want {
				return nil
			}
		}
		time.Sleep(delay)
	}
	return fmt.Errorf("only %d/%d nodes Ready after %d attempts", -1, want, attempts)
}

// renderAgentYAML reads lima-agent.yaml, substitutes the empty
// NATRA_K3S_URL / NATRA_K3S_TOKEN env values with the real
// server-side ones, writes the result to a temp file, and returns
// the path. Caller removes when done.
func renderAgentYAML(c *Config, serverIP, token string) (string, error) {
	src := filepath.Join(c.RigDir, "lima-agent.yaml")
	in, err := os.ReadFile(src)
	if err != nil {
		return "", err
	}
	body := string(in)
	body = strings.Replace(body,
		`NATRA_K3S_URL: ""`,
		`NATRA_K3S_URL: "https://`+serverIP+`:6443"`, 1)
	body = strings.Replace(body,
		`NATRA_K3S_TOKEN: ""`,
		`NATRA_K3S_TOKEN: "`+token+`"`, 1)

	tmp, err := os.CreateTemp("", "natra-vm-rig-agent-*.yaml")
	if err != nil {
		return "", err
	}
	if _, err := tmp.WriteString(body); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmp.Name())
		return "", err
	}
	if err := tmp.Close(); err != nil {
		return "", err
	}
	return tmp.Name(), nil
}
