// natra-tools holds natra's operator subcommands. They live outside
// the CNI binary so the per-pod `natra` invocation doesn't page in
// their code (runtime/pprof alone is ~100 KB of peak RSS). `natra
// <subcommand>` execs this binary from the same directory, so the
// documented commands are unchanged.
package main

import (
	"fmt"
	"os"
)

const usage = `usage: natra-tools <command> [args]

commands:
  install-cni-chain <conflist-dir>   chain natra into existing conflists
  dump-stats <containerID>           print a pod's pinned BPF stats
  profile [flags]                    sample BPF program runtime stats`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, usage)
		os.Exit(64)
	}
	var err error
	switch os.Args[1] {
	case "install-cni-chain":
		err = installCNIChain(os.Args[2:])
	case "dump-stats":
		err = dumpStats(os.Args[2:])
	case "profile":
		err = profileCmd(os.Args[2:])
	default:
		fmt.Fprintf(os.Stderr, "natra-tools: unknown command %q\n\n%s\n", os.Args[1], usage)
		os.Exit(64)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
