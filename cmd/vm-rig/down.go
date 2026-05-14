package main

import (
	"fmt"
	"os"
)

// cmdDown stops and deletes both VMs. Idempotent — missing VMs are
// not errors. Cleans up the kubeconfig the host wrote at up time;
// the cached cloud images stay (lima keeps them between runs).
func cmdDown(c *Config) error {
	for _, vm := range []string{c.AgentName, c.ServerName} {
		if !limaExists(vm) {
			fmt.Printf("==> %s not present, skipping\n", vm)
			continue
		}
		fmt.Printf("==> stopping %s\n", vm)
		// limactl stop --force is the documented way to bypass
		// graceful-shutdown timeouts; we don't care about clean
		// shutdown of test-rig VMs.
		_ = run("limactl", "stop", "--force", vm)
		fmt.Printf("==> deleting %s\n", vm)
		if err := run("limactl", "delete", "--force", vm); err != nil {
			return err
		}
	}
	_ = os.Remove(c.KubeconfigPath)
	fmt.Println("vm-rig torn down.")
	return nil
}
