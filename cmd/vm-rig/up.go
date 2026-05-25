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

	// Stage 1: server VM. Pick the lima template by CNI choice
	// (flannel default, cilium opt-in via VMRIG_CNI=cilium).
	serverYAML, err := limaYAMLPath(c, "server")
	if err != nil {
		return err
	}
	fmt.Printf("==> bringing up %s (cni=%s)\n", c.ServerName, c.VMRigCNI)
	if !limaExists(c.ServerName) {
		if err := run("limactl", "create", "--name", c.ServerName, serverYAML); err != nil {
			return err
		}
	}
	if err := run("limactl", "start", c.ServerName); err != nil {
		return err
	}

	fmt.Println("==> waiting for k3s server to finish provisioning")
	tokenPath := "/etc/natra-node-token"
	// lima only reports the VM "started" after its boot/provision
	// scripts finish, and the server provision writes this token as
	// its last step (after the event-driven networkd-wait-online +
	// k3s install). So post-start the token effectively already
	// exists; this poll is a failsafe against limactl-shell exec
	// latency / edge races, NOT a calibrated wait. 180s is ample
	// anti-hang insurance — if it trips, provision genuinely failed
	// (inspect the VM; NATRA_VM_KEEP keeps it up).
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

	// Stage 3: export kubeconfig. k3s writes it with
	// server: https://127.0.0.1:6443 — and that's exactly where
	// the lima portForward (see lima-server.yaml) lands the VM's
	// 6443 on the host, so we keep the address as-is. The agent
	// VM joins via the server's lima-shared DHCP IP (captured
	// above into serverIP, port 6443) over the vmnet network.
	fmt.Printf("==> exporting kubeconfig to %s\n", c.KubeconfigPath)
	kc, err := capture("limactl", "shell", c.ServerName, "--", "sudo", "cat", "/etc/rancher/k3s/k3s.yaml")
	if err != nil {
		return err
	}
	if err := os.WriteFile(c.KubeconfigPath, []byte(kc), 0o600); err != nil {
		return err
	}

	// Stage 4: wait for both nodes Ready. Condition poll — there's
	// no event channel through `limactl shell`, so we poll
	// `kubectl get nodes` for the actual readiness condition. The
	// agent's event-driven DHCP wait + k3s-agent install completed
	// before its VM reported started; what remains here is k3s
	// control-plane + flannel CNI convergence (typically 30-90s).
	// 300s is a documented failsafe ceiling, not a calibrated
	// value — the Ready condition is the mechanism.
	fmt.Println("==> waiting for both nodes Ready")
	if err := waitForNodesReady(c, 2, 150, 2*time.Second); err != nil {
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
	lastReady := 0
	lastErr := ""
	for i := 0; i < attempts; i++ {
		out, err := captureKubectl(env, "get", "nodes", "--no-headers")
		if err != nil {
			lastErr = err.Error()
		} else {
			lastErr = ""
			lastReady = 0
			for _, line := range strings.Split(out, "\n") {
				fields := strings.Fields(line)
				if len(fields) >= 2 && fields[1] == "Ready" {
					lastReady++
				}
			}
			if lastReady >= want {
				return nil
			}
		}
		time.Sleep(delay)
	}
	if lastErr != "" {
		return fmt.Errorf("only %d/%d nodes Ready after %d attempts (kubectl: %s)",
			lastReady, want, attempts, lastErr)
	}
	return fmt.Errorf("only %d/%d nodes Ready after %d attempts", lastReady, want, attempts)
}

// limaYAMLPath returns the lima template file for the given role
// ("server" or "agent") based on c.VMRigCNI. Errors loudly on
// unknown values so a typo in VMRIG_CNI surfaces here rather than
// as a confused "file not found".
//
// The agent template doesn't depend on cilium's KPR setting (only
// the server's k3s install args + helm install differ), so the
// cilium and cilium-kpr variants share the same agent file.
func limaYAMLPath(c *Config, role string) (string, error) {
	switch c.VMRigCNI {
	case "flannel", "":
		return filepath.Join(c.RigDir, "lima-"+role+"-flannel.yaml"), nil
	case "cilium":
		return filepath.Join(c.RigDir, "lima-"+role+"-cilium.yaml"), nil
	case "cilium-kpr":
		if role == "agent" {
			return filepath.Join(c.RigDir, "lima-agent-cilium.yaml"), nil
		}
		return filepath.Join(c.RigDir, "lima-"+role+"-cilium-kpr.yaml"), nil
	default:
		return "", fmt.Errorf("VMRIG_CNI=%q is not recognized (want flannel, cilium, or cilium-kpr)", c.VMRigCNI)
	}
}

// renderAgentYAML reads the agent lima template (chosen by
// c.VMRigCNI), substitutes the empty NATRA_K3S_URL / NATRA_K3S_TOKEN
// env values with the real server-side ones, writes the result to
// a temp file, and returns the path. Caller removes when done.
func renderAgentYAML(c *Config, serverIP, token string) (string, error) {
	src, err := limaYAMLPath(c, "agent")
	if err != nil {
		return "", err
	}
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
