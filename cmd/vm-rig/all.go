package main

import (
	"flag"
	"fmt"
	"os"
)

// cmdAll wires up + install + test together. Tears the rig down on
// exit (success or fail) so a flaky run doesn't leak two VMs. Pass
// -keep to leave the VMs up for kubectl exec inspection.
func cmdAll(c *Config, args []string) error {
	fs := flag.NewFlagSet("all", flag.ExitOnError)
	keep := fs.Bool("keep", os.Getenv("NATRA_VM_KEEP") == "1",
		"leave the VMs up after tests for inspection")
	if err := fs.Parse(args); err != nil {
		return err
	}

	if !*keep {
		defer func() {
			fmt.Println()
			fmt.Println("==> tearing down vm-rig")
			_ = cmdDown(c)
		}()
	}

	if err := cmdUp(c); err != nil {
		return err
	}
	if err := cmdInstall(c); err != nil {
		return err
	}
	return cmdTest(c)
}
