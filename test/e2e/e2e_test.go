//go:build e2e

// Layer 4 — kind end-to-end happy path.
//
// Brings up a 2-node kind cluster (kindnet as main CNI), builds and
// loads the natra container image, installs the natra DaemonSet
// (which copies the binary into /opt/cni/bin on every node), deploys
// the iperf3 client + multiple iperf3 server pods covering the
// topology matrix, runs traffic, and verifies bandwidth annotations
// are enforced (or not) for each topology.
//
// Topologies covered (one Describe block each, plus a smoke check):
//   A. ingress only         — server with kubernetes.io/ingress-bandwidth=10M
//   B. egress only          — server with kubernetes.io/egress-bandwidth=10M, iperf3 -R
//   C. bidi sequential      — both annotations, forward then reverse iperf3
//   D. mixed                — three pods on one node, only one annotated
//   E. none (in-path)       — three unannotated pods served via natra (no-op verified)
//   F. no-plugin regression — same workload with vs without natra DS, delta < 10%
//   G. proxy-like bidi      — both annotations, concurrent forward + reverse iperf3
//
// Topology F is special: it tears down and reinstates the natra
// DaemonSet mid-suite. The Describe runs after A-E and G to avoid
// affecting them. AfterAll restores the DaemonSet before AfterSuite
// deletes the cluster.

package e2e_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
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
	// throttledCap is the headline assertion's upper bound on a
	// throttled iperf measurement. +20% absorbs kindnet/iperf3 jitter.
	throttledCap = int64(float64(bandwidthBps) * 1.20)
	// unthrottledFloor is the lower bound for a pod that is NOT
	// supposed to be throttled. Conservative — kind cross-node TCP
	// over kindnet on a CI runner sustains > 1 Gbps without natra,
	// and natra-the-no-op should not change that meaningfully.
	unthrottledFloor int64 = 100_000_000 // 100 Mbps
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
	installNatraDaemon()

	By("deploying iperf3 client + per-topology server pods")
	mustExec("kubectl", "apply", "-f", repoFile("manifests", "iperf-client.yaml"))
	for _, m := range []string{
		"iperf-server.yaml",
		"iperf-server-egress.yaml",
		"iperf-server-bidi.yaml",
		"iperf-server-mixed-a.yaml",
		"iperf-server-mixed-b.yaml",
		"iperf-server-mixed-c.yaml",
	} {
		mustExec("kubectl", "apply", "-f", repoFile("manifests", m))
	}

	By("waiting for all iperf pods Ready")
	pods := []string{
		"pod/iperf-client",
		"pod/iperf-server",
		"pod/iperf-server-egress",
		"pod/iperf-server-bidi",
		"pod/iperf-server-mixed-a",
		"pod/iperf-server-mixed-b",
		"pod/iperf-server-mixed-c",
	}
	args := append([]string{"wait", "--for=condition=Ready"}, pods...)
	args = append(args, "-n", namespace, "--timeout=180s")
	if err := exec.Command("kubectl", args...).Run(); err != nil {
		// Diagnostic dump to make BeforeSuite failures actionable.
		dump := func(name string, args ...string) {
			GinkgoWriter.Printf("===== %s =====\n", name)
			out, _ := exec.Command(args[0], args[1:]...).CombinedOutput()
			GinkgoWriter.Printf("%s\n", out)
		}
		dump("kubectl get pods", "kubectl", "get", "pods", "-n", namespace, "-o", "wide")
		dump("install init-container logs", "kubectl", "logs", "-n", "kube-system", "-l", "app=natra", "-c", "install", "--tail=80")
		dump("conflist on worker node", "docker", "exec", clusterName+"-worker", "sh", "-c", "ls /etc/cni/net.d && cat /etc/cni/net.d/*.conflist 2>/dev/null")
		Fail("iperf pods failed to reach Ready (see diagnostics above)")
	}
})

var _ = AfterSuite(func() {
	if os.Getenv("NATRA_E2E_KEEP") == "1" {
		GinkgoWriter.Printf("NATRA_E2E_KEEP=1 — leaving cluster %q up for inspection\n", clusterName)
		return
	}
	By("deleting kind cluster")
	_ = exec.Command("kind", "delete", "cluster", "--name", clusterName).Run()
})

// installNatraDaemon applies deploy/cni-installer.yaml with the e2e
// image override and waits for rollout. Used by BeforeSuite and by
// Topology F's reinstatement step.
func installNatraDaemon() {
	// NATRA_E2E_ATTACH_MODE picks the attach path the test rig
	// exercises. Default is "tcx" (production default). Set
	// "clsact-podside" to exercise the fallback explicitly.
	attachMode := os.Getenv("NATRA_E2E_ATTACH_MODE")
	if attachMode == "" || attachMode == "tcx" {
		attachMode = "" // empty in the manifest = tcx default
	}
	mustExec("bash", "-c",
		fmt.Sprintf(`sed -e 's|ghcr.io/terraboops/natra:latest|%s|' -e 's|imagePullPolicy: IfNotPresent|imagePullPolicy: Never|' -e 's|value: ""$|value: "%s"|' %s | kubectl apply -f -`,
			natraImage, attachMode, repoFile("..", "..", "deploy", "cni-installer.yaml")),
	)
	mustExec("kubectl", "rollout", "status", "daemonset/natra-installer",
		"-n", "kube-system", "--timeout=120s")
}

// removeNatraDaemon deletes the natra DaemonSet and waits for the
// installer pods to clear. Topology F uses this to capture a no-natra
// baseline before reinstating.
func removeNatraDaemon() {
	mustExec("kubectl", "delete", "daemonset/natra-installer",
		"-n", "kube-system", "--ignore-not-found", "--wait=true", "--timeout=60s")
	// Also wait for the installer pods to clear so the next ADD on a
	// new pod doesn't race against a still-running install init.
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		out, err := exec.Command("kubectl", "get", "pods",
			"-n", "kube-system", "-l", "app=natra",
			"-o", "jsonpath={.items[*].metadata.name}").Output()
		if err == nil && len(out) == 0 {
			return
		}
		time.Sleep(2 * time.Second)
	}
}

// reapplyPod deletes (with grace=0) and reapplies a pod manifest, then
// waits for it to be Ready. Used by Topology F to ensure a server pod
// gets re-CNI-Add'd through the new conflist after natra reinstall.
func reapplyPod(podName, manifestFile string) {
	_ = exec.Command("kubectl", "delete", "pod", podName,
		"-n", namespace, "--ignore-not-found",
		"--grace-period=0", "--force", "--wait=true").Run()
	mustExec("kubectl", "apply", "-f", repoFile("manifests", manifestFile))
	mustExec("kubectl", "wait", "--for=condition=Ready", "pod/"+podName,
		"-n", namespace, "--timeout=120s")
}

// Topology A — ingress only. Original happy-path coverage.
var _ = Describe("Topology A — ingress only", func() {
	It("connectivity smoke: iperf pods can reach each other", func() {
		ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
		defer cancel()
		bps := runIperf(ctx, iperfOpts{Target: "iperf-server"})
		GinkgoWriter.Printf("smoke throughput: %.2f Mbps\n", float64(bps)/1e6)
		Expect(bps).To(BeNumerically(">", 0),
			"connectivity should work between iperf pods")
	})

	It("enforces ingress-bandwidth at the throttled cap", func() {
		ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
		defer cancel()
		bps := runIperf(ctx, iperfOpts{Target: "iperf-server"})
		logThrottled("ingress", bps)
		Expect(bps).To(BeNumerically("<=", throttledCap),
			"forward iperf3 should be throttled to ≈10 Mbps")
	})
})

// Topology B — egress only. iperf3 reverse: server is the data sender.
var _ = Describe("Topology B — egress only", func() {
	It("enforces egress-bandwidth on the server's outbound traffic", func() {
		ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
		defer cancel()
		bps := runIperf(ctx, iperfOpts{Target: "iperf-server-egress", Reverse: true})
		logThrottled("egress", bps)
		Expect(bps).To(BeNumerically("<=", throttledCap),
			"reverse iperf3 should be throttled to ≈10 Mbps")
	})
})

// Topology C — sequential bidi. Both annotations on one pod, two
// iperf3 runs back-to-back.
var _ = Describe("Topology C — bidi sequential", func() {
	It("throttles forward to the ingress cap", func() {
		ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
		defer cancel()
		bps := runIperf(ctx, iperfOpts{Target: "iperf-server-bidi"})
		logThrottled("bidi/forward", bps)
		Expect(bps).To(BeNumerically("<=", throttledCap),
			"forward iperf3 on a both-annotated pod should be throttled")
	})

	It("throttles reverse to the egress cap", func() {
		ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
		defer cancel()
		bps := runIperf(ctx, iperfOpts{Target: "iperf-server-bidi", Reverse: true})
		logThrottled("bidi/reverse", bps)
		Expect(bps).To(BeNumerically("<=", throttledCap),
			"reverse iperf3 on a both-annotated pod should be throttled")
	})
})

// Topology D — mixed. Three server pods on the worker, only mixed-a
// is annotated. mixed-b and mixed-c must keep baseline throughput so
// natra doesn't penalize unannotated pods sharing a node.
var _ = Describe("Topology D — mixed (only some pods annotated)", func() {
	It("throttles only the annotated pod; other pods stay near baseline", func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
		defer cancel()

		bpsA := runIperf(ctx, iperfOpts{Target: "iperf-server-mixed-a"})
		bpsB := runIperf(ctx, iperfOpts{Target: "iperf-server-mixed-b"})
		bpsC := runIperf(ctx, iperfOpts{Target: "iperf-server-mixed-c"})

		GinkgoWriter.Printf("mixed-a (annotated)   throughput: %.2f Mbps\n", float64(bpsA)/1e6)
		GinkgoWriter.Printf("mixed-b (unannotated) throughput: %.2f Mbps\n", float64(bpsB)/1e6)
		GinkgoWriter.Printf("mixed-c (unannotated) throughput: %.2f Mbps\n", float64(bpsC)/1e6)

		Expect(bpsA).To(BeNumerically("<=", throttledCap),
			"annotated pod should be throttled")
		Expect(bpsB).To(BeNumerically(">=", unthrottledFloor),
			"unannotated mixed-b should not be throttled")
		Expect(bpsC).To(BeNumerically(">=", unthrottledFloor),
			"unannotated mixed-c should not be throttled")
	})
})

// Topology E — none. All three pods are unannotated; natra is in the
// CNI path but should be a no-op for them.
var _ = Describe("Topology E — no annotations (natra is no-op)", func() {
	It("all unannotated pods reach baseline throughput with natra installed", func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
		defer cancel()

		bpsB := runIperf(ctx, iperfOpts{Target: "iperf-server-mixed-b"})
		bpsC := runIperf(ctx, iperfOpts{Target: "iperf-server-mixed-c"})

		GinkgoWriter.Printf("mixed-b throughput (no annotation, natra in path): %.2f Mbps\n",
			float64(bpsB)/1e6)
		GinkgoWriter.Printf("mixed-c throughput (no annotation, natra in path): %.2f Mbps\n",
			float64(bpsC)/1e6)

		Expect(bpsB).To(BeNumerically(">=", unthrottledFloor),
			"natra without an annotation must not throttle traffic")
		Expect(bpsC).To(BeNumerically(">=", unthrottledFloor),
			"natra without an annotation must not throttle traffic")
	})
})

// Topology G — proxy-like simultaneous bidirectional. Both annotations
// on one pod, forward and reverse iperf3 run concurrently. Catches
// cross-direction state corruption (shared map slots, lock contention)
// that sequential runs can miss.
var _ = Describe("Topology G — proxy-like simultaneous bidirectional", func() {
	It("throttles both directions independently under concurrent traffic", func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()

		var (
			wg       sync.WaitGroup
			fwdBps   int64
			revBps   int64
			fwdErr   error
			revErr   error
		)
		wg.Add(2)
		go func() {
			defer wg.Done()
			fwdBps, fwdErr = runIperfWithError(ctx, iperfOpts{
				Target: "iperf-server-bidi", Duration: 10,
			})
		}()
		go func() {
			defer wg.Done()
			revBps, revErr = runIperfWithError(ctx, iperfOpts{
				Target: "iperf-server-bidi", Reverse: true, Duration: 10,
			})
		}()
		wg.Wait()

		Expect(fwdErr).NotTo(HaveOccurred(), "concurrent forward iperf3 failed")
		Expect(revErr).NotTo(HaveOccurred(), "concurrent reverse iperf3 failed")

		GinkgoWriter.Printf("concurrent forward (ingress) throughput: %.2f Mbps\n", float64(fwdBps)/1e6)
		GinkgoWriter.Printf("concurrent reverse (egress)  throughput: %.2f Mbps\n", float64(revBps)/1e6)

		Expect(fwdBps).To(BeNumerically("<=", throttledCap),
			"ingress should throttle even while egress traffic flows simultaneously")
		Expect(revBps).To(BeNumerically("<=", throttledCap),
			"egress should throttle even while ingress traffic flows simultaneously")
	})
})

// Topology F — no-plugin regression. Tears down natra, runs an
// unannotated workload to capture baseline, reinstates natra, runs
// the same workload, asserts delta < 10%.
//
// Runs after the others so its DS-uninstall doesn't perturb them.
// AfterAll restores the DS even on failure.
var _ = Describe("Topology F — no-plugin regression", Ordered, func() {
	const tolerance = 0.10
	const podName = "iperf-server-noplugin"
	const manifest = "iperf-server-noplugin.yaml"

	var baselineNoPlugin, baselineWithPlugin int64

	AfterAll(func() {
		// Always reinstate natra and clean the test pod, even on
		// failure, so subsequent suite runs (or NATRA_E2E_KEEP=1
		// inspection) get a sane state.
		_ = exec.Command("kubectl", "delete", "pod", podName,
			"-n", namespace, "--ignore-not-found",
			"--grace-period=0", "--force").Run()
		// Idempotent — already-installed DS is a no-op.
		installNatraDaemon()
	})

	It("captures baseline throughput without natra", func() {
		ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
		defer cancel()

		By("removing the natra DaemonSet")
		removeNatraDaemon()

		By("deploying unannotated server (kindnet-only path)")
		reapplyPod(podName, manifest)

		baselineNoPlugin = runIperf(ctx, iperfOpts{Target: podName})
		GinkgoWriter.Printf("no-plugin baseline: %.2f Mbps\n", float64(baselineNoPlugin)/1e6)
		Expect(baselineNoPlugin).To(BeNumerically(">", 0),
			"baseline should produce a real number")
	})

	It("re-measures with natra installed and asserts delta < 10%", func() {
		ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
		defer cancel()

		By("reinstalling natra DaemonSet")
		installNatraDaemon()

		By("redeploying server pod so it picks up natra in the chained conflist")
		reapplyPod(podName, manifest)

		baselineWithPlugin = runIperf(ctx, iperfOpts{Target: podName})
		GinkgoWriter.Printf("with-natra baseline: %.2f Mbps\n", float64(baselineWithPlugin)/1e6)
		Expect(baselineWithPlugin).To(BeNumerically(">", 0),
			"with-plugin throughput should produce a real number")

		Expect(baselineNoPlugin).To(BeNumerically(">", 0),
			"baseline must have been captured first")
		delta := float64(baselineNoPlugin-baselineWithPlugin) / float64(baselineNoPlugin)
		if delta < 0 {
			delta = -delta
		}
		GinkgoWriter.Printf("regression delta: %.1f%% (tolerance %.0f%%)\n", delta*100, tolerance*100)
		Expect(delta).To(BeNumerically("<=", tolerance),
			"natra without an annotation must not introduce >%.0f%% throughput regression", tolerance*100)
	})
})

// logThrottled writes the human-readable summary line that the
// existing tests depend on for log inspection.
func logThrottled(direction string, bps int64) {
	GinkgoWriter.Printf("[%s] throttled throughput: %.2f Mbps (cap %.2f Mbps with +20%% slack)\n",
		direction, float64(bps)/1e6, float64(bandwidthBps)/1e6)
}

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

type iperfOpts struct {
	Target   string // service name to dial (default "iperf-server")
	Reverse  bool   // -R flag
	Duration int    // seconds; default 5
}

// runIperf executes a TCP iperf3 transfer from iperf-client to the
// named server pod and returns the receiver-side bits-per-second from
// the summary section of iperf3's JSON output. Fails the spec on any
// exec or parse error.
func runIperf(ctx context.Context, opts iperfOpts) int64 {
	bps, err := runIperfWithError(ctx, opts)
	if err != nil {
		Fail(err.Error())
	}
	return bps
}

// runIperfWithError is the non-failing variant. Used by Topology G's
// concurrent goroutines so a single iperf failure doesn't tear down
// the goroutine before its peer can finish.
func runIperfWithError(ctx context.Context, opts iperfOpts) (int64, error) {
	target := opts.Target
	if target == "" {
		target = "iperf-server"
	}
	duration := opts.Duration
	if duration == 0 {
		duration = 5
	}

	args := []string{
		"exec", "-n", namespace, "iperf-client", "--",
		"iperf3", "-c", target, "-t", fmt.Sprintf("%d", duration), "-J",
	}
	if opts.Reverse {
		args = append(args, "-R")
	}
	cmd := exec.CommandContext(ctx, "kubectl", args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return 0, fmt.Errorf("iperf exec failed: %v\n%s", err, out)
	}
	var report struct {
		End struct {
			SumReceived struct {
				BitsPerSecond float64 `json:"bits_per_second"`
			} `json:"sum_received"`
		} `json:"end"`
	}
	if err := json.Unmarshal(out, &report); err != nil {
		return 0, fmt.Errorf("parse iperf JSON: %v\n%s", err, out)
	}
	return int64(report.End.SumReceived.BitsPerSecond), nil
}
