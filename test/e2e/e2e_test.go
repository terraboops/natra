//go:build e2e

// Layer 4 — kind end-to-end happy path.
//
// Brings up a 2-node kind cluster (kindnet as main CNI), builds + loads
// the natra container image, installs the natra DaemonSet (which copies
// the binary into /opt/cni/bin on every node), deploys iperf3 server +
// client on different nodes, runs traffic, and verifies the bandwidth
// limit is being enforced.
//
// Phase 0 expectation: cluster + DaemonSet come up, iperf connectivity
// works, but the bandwidth assertion fails because Phase-0 natra doesn't
// actually attach BPF programs (it's just installing the binary).
// Phase 1 will replace this with real BPF-based rate-limiting.

package e2e_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

func TestE2E(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "test/e2e (Layer 4)")
}

const (
	clusterName  = "natra-e2e"
	namespace    = "natra-e2e"
	natraImage   = "ghcr.io/terraboops/natra:e2e"
	bandwidthBps = 10_000_000 // 10 Mbps annotation under test
)

func repoFile(parts ...string) string {
	_, thisFile, _, _ := runtime.Caller(0)
	return filepath.Join(append([]string{filepath.Dir(thisFile)}, parts...)...)
}

func repoRoot() string {
	_, thisFile, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(thisFile), "..", "..")
}

var _ = BeforeSuite(func() {
	requireBinary("kind")
	requireBinary("kubectl")
	requireBinary("docker")

	By("creating kind cluster")
	mustExec("kind", "create", "cluster",
		"--name", clusterName,
		"--config", repoFile("kind-config.yaml"),
		"--wait", "120s",
	)

	By("building natra container image")
	mustExecIn(repoRoot(), "docker", "build",
		"-t", natraImage,
		"-f", "deploy/docker/Dockerfile.cni",
		".",
	)

	By("loading natra image into kind cluster")
	mustExec("kind", "load", "docker-image", natraImage, "--name", clusterName)

	By("creating test namespace")
	mustExec("kubectl", "apply", "-f", repoFile("manifests", "namespace.yaml"))

	By("installing natra DaemonSet (with overridden image)")
	mustExec("bash", "-c",
		fmt.Sprintf("sed 's|ghcr.io/terraboops/natra:latest|%s|; s|imagePullPolicy: IfNotPresent|imagePullPolicy: Never|' %s | kubectl apply -f -",
			natraImage, repoFile("..", "..", "deploy", "cni-installer.yaml")),
	)

	By("waiting for natra DaemonSet to roll out")
	mustExec("kubectl", "rollout", "status", "daemonset/natra-installer",
		"-n", "kube-system", "--timeout=120s")

	By("deploying iperf3 server + client pods")
	mustExec("kubectl", "apply", "-f", repoFile("manifests", "iperf-server.yaml"))
	mustExec("kubectl", "apply", "-f", repoFile("manifests", "iperf-client.yaml"))

	By("waiting for iperf pods Ready")
	mustExec("kubectl", "wait", "--for=condition=Ready",
		"pod/iperf-server", "pod/iperf-client",
		"-n", namespace, "--timeout=120s")
})

var _ = AfterSuite(func() {
	By("deleting kind cluster")
	_ = exec.Command("kind", "delete", "cluster", "--name", clusterName).Run()
})

var _ = Describe("natra e2e", func() {
	It("kindnet provides connectivity and natra DaemonSet installs the binary", func() {
		ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
		defer cancel()

		bps := runIperf(ctx)
		GinkgoWriter.Printf("measured throughput: %.2f Mbps (target after Phase 1: ≤ %.2f Mbps)\n",
			float64(bps)/1e6, float64(bandwidthBps)/1e6)

		// Phase 0 smoke: assert there's *some* throughput (connectivity
		// works, kind cluster is healthy, iperf pods can reach each
		// other across nodes). The actual rate-limit assertion lives
		// in PIt below and turns on with Phase 1 BPF code.
		Expect(bps).To(BeNumerically(">", 0),
			"smoke check: connectivity should work between iperf pods")
	})

	PIt("enforces ingress-bandwidth annotation (Phase 1 — needs BPF dataplane)", func() {
		ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
		defer cancel()
		bps := runIperf(ctx)
		Expect(bps).To(BeNumerically("<=", int64(float64(bandwidthBps)*1.20)),
			"throughput should be at or below the bandwidth annotation (+20%% slack)")
	})
})

func requireBinary(name string) {
	_, err := exec.LookPath(name)
	Expect(err).NotTo(HaveOccurred(), "Layer 4 needs %q on PATH", name)
}

func mustExec(name string, args ...string) {
	out, err := exec.Command(name, args...).CombinedOutput()
	if err != nil {
		Fail(fmt.Sprintf("%s %v failed: %v\n%s", name, args, err, out))
	}
}

func mustExecIn(dir, name string, args ...string) {
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		Fail(fmt.Sprintf("(in %s) %s %v failed: %v\n%s", dir, name, args, err, out))
	}
}

// runIperf executes a 5-second TCP iperf3 transfer from iperf-client to
// the iperf-server pod and returns the receiver-side bits-per-second
// from the summary section of iperf3's JSON output.
func runIperf(ctx context.Context) int64 {
	cmd := exec.CommandContext(ctx, "kubectl", "exec", "-n", namespace,
		"iperf-client", "--",
		"iperf3", "-c", "iperf-server", "-t", "5", "-J",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		Fail(fmt.Sprintf("iperf exec failed: %v\n%s", err, out))
	}
	var report struct {
		End struct {
			SumReceived struct {
				BitsPerSecond float64 `json:"bits_per_second"`
			} `json:"sum_received"`
		} `json:"end"`
	}
	if err := json.Unmarshal(out, &report); err != nil {
		Fail(fmt.Sprintf("parse iperf JSON: %v\n%s", err, out))
	}
	return int64(report.End.SumReceived.BitsPerSecond)
}
