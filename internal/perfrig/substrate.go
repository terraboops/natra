package perfrig

import "context"

// Substrate is the seam. It exposes only the operations that
// genuinely differ between lima (each k8s node is a VM, root via
// `limactl shell`) and k3d (each k8s node is a docker container,
// root via `docker exec`). Anything reachable through KUBECONFIG
// (kubectl exec into pods, kubectl apply, manifest rendering) is
// substrate-independent and lives in the shared executor, not
// here — keeping this interface narrow is what stops drift from
// creeping back in via conditionals.
//
// Two impls today:
//   - limaSubstrate (cmd/perfrig with --substrate=vm-rig)
//   - k3dSubstrate  (cmd/perfrig with --substrate=k3d, invoked by
//     scripts/perf-vs-vanilla.sh after the cluster bootstrap)
//
// New methods belong here only when two impls genuinely diverge
// on a real operation; prefer extending the executor over widening
// the interface.
type Substrate interface {
	// Up brings the substrate's cluster online from scratch. The
	// executor calls Down then Up at the start of every phase so no
	// state leaks across phases.
	Up(ctx context.Context) error

	// Down tears the cluster down. Idempotent: must succeed even
	// when no cluster exists.
	Down(ctx context.Context) error

	// InstallNatra chains natra into the cluster's CNI conflist and
	// waits for the installer DaemonSet to roll out. Only called in
	// the natra phase.
	InstallNatra(ctx context.Context) error

	// ImportImage builds an image from the named Dockerfile (relative
	// to the repo root) and makes it available inside the cluster's
	// nodes. Used to import the perf-client image into both VMs (or
	// both k3d nodes) so iperf/hey can reach the workload.
	ImportImage(ctx context.Context, image, dockerfile string) error

	// KubeconfigPath returns the kubeconfig for kubectl operations
	// against this substrate's cluster.
	KubeconfigPath() string

	// Nodes returns the k8s node names for the control-plane node
	// and the worker. The naming convention differs per substrate
	// (lima-natra-{server,agent} vs k3d-…-{server,agent}-0).
	Nodes() (server, worker string)

	// NodeShell runs a root shell command on the named node. Used
	// for the TBF burst patch, bpftool snapshots, /proc/meminfo
	// reads, ru_maxrss capture, and any other node-local work. The
	// script is passed as a single string to sh -c; multi-line
	// scripts are fine.
	NodeShell(ctx context.Context, node, script string) ([]byte, error)

	// EnsureBpftool guarantees `bpftool` is on PATH on the named
	// node. lima VMs typically have it; k3d nodes need it installed
	// (the bash rig's install_bpftool_in_node logic). No-op if
	// already present.
	EnsureBpftool(ctx context.Context, node string) error

	// Name identifies the substrate in report headers and log lines
	// ("vm-rig" or "k3d").
	Name() string
}
