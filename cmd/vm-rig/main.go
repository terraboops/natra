// vm-rig is the natra two-VM kernel-isolated test rig: one k3s
// server VM, one k3s agent VM, each on its own Linux kernel, joined
// via the lima shared network so pod-to-pod traffic crosses a real
// virtual NIC pair instead of a single-kernel bridge.
//
// External tools driven via os/exec: limactl, docker, kubectl. Lima
// doesn't have a stable external Go API; client-go is verbose for
// this workflow; matches the convention test/e2e/e2e_test.go
// already uses (kubectl as a subprocess). iperf3 JSON output and
// any internal logic stay in Go.
//
// Subcommands:
//
//	up        bring up both VMs, join into k3s cluster
//	install   build the natra image, push to both VMs, apply installer
//	test      run the iperf throttle assertion across the VM boundary
//	down      stop and delete both VMs
//	all       up → install → test (with down on exit unless -keep)
package main

import (
	"fmt"
	"os"
)

const usage = `vm-rig — natra kernel-isolated test rig (lima + k3s)

Subcommands:
  up        bring up the two-VM k3s cluster
  install   build and import the natra image, apply installer
  test      run the iperf throttle assertion
  down      tear down both VMs
  all       up + install + test (down on exit unless -keep)

Environment:
  NATRA_VM_KUBECONFIG   kubeconfig output path (default /tmp/natra-vm-rig.kubeconfig)
  NATRA_VM_IMAGE        natra image tag to build/use (default ghcr.io/terraboops/natra:vm-rig)
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}

	cmd := os.Args[1]
	args := os.Args[2:]

	cfg := loadConfig()

	var err error
	switch cmd {
	case "up":
		err = cmdUp(cfg)
	case "install":
		err = cmdInstall(cfg)
	case "test":
		err = cmdTest(cfg)
	case "down":
		err = cmdDown(cfg)
	case "all":
		err = cmdAll(cfg, args)
	case "help", "-h", "--help":
		fmt.Print(usage)
		return
	default:
		fmt.Fprintf(os.Stderr, "unknown subcommand %q\n\n%s", cmd, usage)
		os.Exit(2)
	}

	if err != nil {
		fmt.Fprintf(os.Stderr, "vm-rig: %v\n", err)
		os.Exit(1)
	}
}
