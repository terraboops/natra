package main

import (
	"os"
	"path/filepath"
	"runtime"
)

// Config bundles the knobs the rig reads from env vars + the
// derived rig-internal paths. One Config is built at startup and
// passed to every subcommand so the values can't drift between
// phases of a single run.
type Config struct {
	// repoRoot is natra/, two levels up from this source file at
	// build time. Used to locate scripts/vm-rig/*.yaml templates
	// and deploy/cni-installer.yaml.
	RepoRoot string

	// rigDir is repoRoot/scripts/vm-rig — where the lima YAML
	// templates live. (The templates themselves stay as files so a
	// reader can see + tweak them without recompiling.)
	RigDir string

	// VM names. Lima sets the in-VM hostname to lima-<name>; k3s
	// registers nodes by hostname, so the Kubernetes node names
	// will be lima-natra-server and lima-natra-agent.
	ServerName string
	AgentName  string

	// KubeconfigPath is where up writes the (rewritten) kubeconfig
	// after bringing the cluster up; every other subcommand reads
	// it from here.
	KubeconfigPath string

	// NatraImage is the local image tag built and imported into
	// the VMs. Pinned to a vm-rig-specific tag so it doesn't
	// collide with the L4 e2e ":e2e" tag or production ":latest".
	NatraImage string

	// PerfclientImage is the load-generator image (iperf3 + hey)
	// built from deploy/docker/Dockerfile.perfclient. Imported into
	// both VMs and used by the hey HTTP-mice test.
	PerfclientImage string

	// VMRigCNI picks which lima YAML templates `up` provisions.
	// "flannel" (default): flannel host-gw, the simpler / lower-
	// overhead configuration; the canonical two-kernel headline
	// numbers in docs/perf-vs-vanilla.md come from it.
	// "cilium": cilium as CNI (cni.exclusive=false, KPR off,
	// hostRouting off) — proxies for AWS NPA, exercises the
	// bpf_mprog coexistence path at pod-eth0 TCX.
	VMRigCNI string
}

func loadConfig() *Config {
	c := &Config{
		ServerName:      "natra-server",
		AgentName:       "natra-agent",
		KubeconfigPath:  envOr("NATRA_VM_KUBECONFIG", "/tmp/natra-vm-rig.kubeconfig"),
		NatraImage:      envOr("NATRA_VM_IMAGE", "ghcr.io/terraboops/natra:vm-rig"),
		PerfclientImage: envOr("NATRA_VM_PERFCLIENT_IMAGE", "ghcr.io/terraboops/natra-perfclient:vm-rig"),
		VMRigCNI:        envOr("VMRIG_CNI", "flannel"),
	}
	_, thisFile, _, _ := runtime.Caller(0)
	c.RepoRoot = filepath.Join(filepath.Dir(thisFile), "..", "..")
	c.RigDir = filepath.Join(c.RepoRoot, "scripts", "vm-rig")
	return c
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
