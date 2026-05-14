//go:build e2e

// Layer 4 — k3d end-to-end happy path.
//
// Brings up a 2-node k3d cluster (k3s in Docker, flannel host-gw as
// main CNI), builds and loads the natra container image, installs
// the natra DaemonSet (which copies the binary into every CNI bin
// candidate path on every node), deploys the iperf3 client +
// multiple iperf3 server pods covering the topology matrix, runs
// traffic, and verifies bandwidth annotations are enforced (or not)
// for each topology.
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
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
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
	// burstSeconds is how many seconds of free traffic a freshly-full
	// bucket grants. natra's default burst is 0.5 × rate
	// (config.DefaultBurstRatio); in time terms that's 0.5 seconds
	// of credit at the configured rate. The calibration math uses
	// this to pick the throttleDuration that amortizes the
	// front-loaded burst below the cap.
	burstSeconds = 0.5
	// throttleDurationFloor / Ceiling clamp the calibrated duration
	// so pathological calibrations can't make tests trivially short
	// or runaway-slow.
	throttleDurationFloor   = 10
	throttleDurationCeiling = 30
	// throttleCapFloorRatio is a floor on the dynamic cap: even if
	// calibration measures an unusually-tight steady-state, we don't
	// let the cap drop below 1.30× rate. Without this a low-jitter
	// runner could end up with a brittle 1.05× cap that catches
	// nothing. 1.30 chosen empirically: GH-runner kindnet jitter on
	// the second leg of a sequential bidi run (Topology C reverse)
	// settles around 1.20-1.25× rate; floor needs daylight above the
	// measured upper tail.
	throttleCapFloorRatio = 1.30
	// throttleCapMargin is added on top of measured mean + 2σ as a
	// final safety pad. 5% covers minor between-test noise that the
	// 3-sample calibration window won't see.
	throttleCapMargin = 0.05
	// chaosCapMultiplier is the extra slack chaos tests get on top
	// of throttledCap. Chaos runs back-to-back iperf cycles (baseline,
	// during-event, after-event) and bucket state from earlier
	// cycles compounds with the threshold fast-pass budget. 1.5×
	// covers it.
	chaosCapMultiplier = 1.5
)

// throttleDuration is the iperf3 -t value the per-direction throttle
// assertions use. Set by calibrateRig in BeforeSuite based on measured
// jitter; falls back to 15 if calibration fails.
//
// The bucket-saturation math: starting from a full bucket, an iperf3
// run of d seconds averages rate × (1 + burstSeconds/d). To keep that
// below the cap even under jitter j (relative stddev):
//
//	(1 + burstSeconds/d) × (1 + 2j) < (cap / rate)
//	d > burstSeconds / ((cap/rate) / (1 + 2j) - 1)
//
// At burstSeconds=0.5 and j=2% this gives d≈2s; at j=5% it gives d≈5s.
// The floor at throttleDurationFloor (10s) binds in both — short
// measurements are statistically noisy regardless of the burst math.
var throttleDuration = 15

// throttledCap and chaosBpsCap are derived from calibration: the rig
// measures mean μ and stddev σ of a throttled stream on the actual
// runner, then sets throttledCap = μ + 2σ + margin (with a floor at
// 1.20× rate). chaosBpsCap = throttledCap × chaosCapMultiplier. Both
// fall back to safe wide defaults if calibration fails so tests can
// still run.
//
// Why dynamic: natra's effective steady-state depends on the
// heavy-hitter threshold, the burst-to-rate ratio, the GRO settings
// on the runner kernel, and TCP-connection establishment timing. A
// hardcoded cap-ratio has to guess all four; measuring them in
// BeforeSuite means the same test adapts to whatever runner /
// configuration combination it lands on.
var (
	throttledCap = int64(float64(bandwidthBps) * 1.40)
	chaosBpsCap  = int64(float64(bandwidthBps) * 2.00)
)

// unthrottledFloor is the lower bound for a pod that is NOT supposed
// to be throttled. Conservative — k3d cross-node TCP over flannel
// host-gw on a CI runner sustains > 1 Gbps without natra, and
// natra-the-no-op should not change that meaningfully. Not
// jitter-derived because the
// floor is 10× below typical unthrottled throughput; runner jitter
// doesn't approach this gap.
var unthrottledFloor int64 = 100_000_000 // 100 Mbps

func repoFile(parts ...string) string {
	_, thisFile, _, _ := runtime.Caller(0)
	return filepath.Join(append([]string{filepath.Dir(thisFile)}, parts...)...)
}

func repoRoot() string {
	_, thisFile, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(thisFile), "..", "..")
}

var _ = BeforeSuite(func() {
	requireBinary("k3d")
	requireBinary("kubectl")
	requireBinary("docker")

	By("creating k3d cluster")
	// flannel backend = host-gw: VXLAN encap is ~30 Mbps on colima
	// (no hw offload in LinuxKit), which is below the rate-limit
	// caps under test and makes throttle assertions meaningless.
	// host-gw uses direct route entries between nodes on the same
	// docker bridge — near-line-rate, what the assertions need.
	mustExec("k3d", "cluster", "create", clusterName,
		"--agents", "1",
		"--no-lb",
		"--k3s-arg", "--disable=traefik,servicelb@server:0",
		"--k3s-arg", "--flannel-backend=host-gw@server:0",
		"--wait",
	)

	By("building natra container image")
	mustExecIn(repoRoot(), "docker", "build",
		"-t", natraImage,
		"-f", "deploy/docker/Dockerfile.cni",
		".",
	)

	By("loading natra image into k3d cluster")
	mustExec("k3d", "image", "import", natraImage, "--cluster", clusterName)

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
		dump("kubectl describe pod iperf-server (CNI Warnings here)", "kubectl", "describe", "pod", "iperf-server", "-n", namespace)
		dump("kubectl events -n natra-e2e", "kubectl", "get", "events", "-n", namespace,
			"--sort-by=.lastTimestamp", "-o", "wide")
		dump("install init-container logs", "kubectl", "logs", "-n", "kube-system", "-l", "app=natra", "-c", "install", "--tail=80")
		dump("conflist on agent node", "docker", "exec", "k3d-"+clusterName+"-agent-0", "sh", "-c",
			"ls /var/lib/rancher/k3s/agent/etc/cni/net.d && cat /var/lib/rancher/k3s/agent/etc/cni/net.d/*.conflist 2>/dev/null")
		dump("natra CNI log on agent node", "docker", "exec", "k3d-"+clusterName+"-agent-0", "sh", "-c",
			"tail -100 /var/log/natra-cni.log 2>/dev/null || echo '(no natra log)'")
		dump("natra binary caps", "docker", "exec", "k3d-"+clusterName+"-agent-0", "sh", "-c",
			"ls -l /var/lib/rancher/k3s/data/cni/natra && getcap /var/lib/rancher/k3s/data/cni/natra 2>&1 || true")
		dump("containerd CNI config (bin_dir search path)", "docker", "exec",
			"k3d-"+clusterName+"-agent-0", "sh", "-c",
			"grep -E 'bin_dir|conf_dir|cni' /var/lib/rancher/k3s/agent/etc/containerd/config.toml 2>/dev/null || "+
				"find / -name 'config.toml' -path '*containerd*' -exec grep -lE 'cni' {} + 2>/dev/null | head -3")
		dump("which directories actually have natra", "docker", "exec",
			"k3d-"+clusterName+"-agent-0", "sh", "-c",
			"find / -name 'natra' -type f 2>/dev/null | head -10")
		Fail("iperf pods failed to reach Ready (see diagnostics above)")
	}

	By("calibrating throttle duration and caps against measured jitter")
	calibrateRig()
})

var _ = AfterSuite(func() {
	if os.Getenv("NATRA_E2E_KEEP") == "1" {
		GinkgoWriter.Printf("NATRA_E2E_KEEP=1 — leaving cluster %q up for inspection\n", clusterName)
		return
	}
	By("deleting k3d cluster")
	_ = exec.Command("k3d", "cluster", "delete", clusterName).Run()
})

// installNatraDaemon applies deploy/cni-installer.yaml with the e2e
// image override and waits for rollout. Used by BeforeSuite and by
// Topology F's reinstatement step.
//
// The installer manifest ships with kind-style host paths
// (/opt/cni/bin, /etc/cni/net.d). k3s puts CNI bits under
// /var/lib/rancher/k3s/{data/cni,agent/etc/cni/net.d}, so we sed
// those in at apply time. Same pattern as scripts/soak-test.sh.
func installNatraDaemon() {
	// NATRA_E2E_ATTACH_MODE picks the attach path the test rig
	// exercises. Default is "" which the binary treats as auto. Set
	// to any of {tcx-hostside, tcx-podside, clsact-hostside,
	// clsact-podside, auto} to exercise that combination explicitly.
	attachMode := os.Getenv("NATRA_E2E_ATTACH_MODE")
	// awk patterns are scoped so we touch only the natra init
	// container's lines, not the pause sidecar's. A global
	// s|imagePullPolicy: IfNotPresent|...|Never| would flip the
	// pause container's policy too — and k3s nodes don't have
	// registry.k8s.io/pause locally, so kubelet then refuses to
	// start the sidecar with ErrImageNeverPull. Same logic for
	// NATRA_ATTACH_MODE's env value: targeted by name so it
	// doesn't also rewrite NATRA_EDT_PACING.
	mustExec("bash", "-c",
		fmt.Sprintf(
			`sed -e 's|ghcr.io/terraboops/natra:latest|%s|' `+
				`-e 's|path: /etc/cni/net.d|path: /var/lib/rancher/k3s/agent/etc/cni/net.d|' `+
				`%s | `+
				`awk -v am=%q '`+
				`/image: %s/ { print; getline; if ($1 == "imagePullPolicy:") sub(/IfNotPresent/, "Never"); print; next } `+
				`/name: NATRA_ATTACH_MODE/ { print; getline; sub(/value: ".*"/, "value: \"" am "\""); print; next } `+
				`{ print }`+
				`' | `+
				`kubectl apply -f -`,
			natraImage,
			repoFile("..", "..", "deploy", "cni-installer.yaml"),
			attachMode,
			// awk's `/regex/` is BRE; escape the slashes in the image ref.
			strings.ReplaceAll(natraImage, "/", `\/`),
		),
	)
	if err := exec.Command("kubectl", "rollout", "status",
		"daemonset/natra-installer", "-n", "kube-system",
		"--timeout=120s").Run(); err != nil {
		// Daemonset rollout failed — surface installer state so the
		// next CI failure has something to grep. Without these dumps
		// we just see "0 of 2 updated pods are available" and the
		// underlying reason (image pull error, init container hang,
		// node taint mismatch) is invisible.
		dump := func(name string, args ...string) {
			GinkgoWriter.Printf("===== %s =====\n", name)
			out, _ := exec.Command(args[0], args[1:]...).CombinedOutput()
			GinkgoWriter.Printf("%s\n", out)
		}
		dump("kubectl get pods (kube-system, wide)", "kubectl", "get", "pods", "-n", "kube-system", "-o", "wide")
		dump("kubectl describe ds/natra-installer", "kubectl", "describe", "ds/natra-installer", "-n", "kube-system")
		dump("kubectl logs natra-installer init (all pods)", "kubectl", "logs", "-n", "kube-system", "-l", "app=natra", "-c", "install", "--tail=200", "--prefix=true")
		dump("kubectl get events (kube-system, last 30)", "kubectl", "get", "events", "-n", "kube-system",
			"--sort-by=.lastTimestamp", "-o", "wide")
		dump("k3d node list", "k3d", "node", "list")
		Fail(fmt.Sprintf("natra-installer rollout failed: %v (see diagnostics above)", err))
	}
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

// waitForServiceEndpoints polls until the matching Service has at
// least one endpoint, up to 30s. kubectl wait Ready returns when the
// pod's container is ready, but Service routing depends on the
// kube-proxy and the Endpoints/EndpointSlice controllers picking the
// pod up — that can lag by several seconds, and a too-soon iperf3
// dial against a freshly-created service hits "connection refused"
// or routes to nothing and reports 0 bps.
func waitForServiceEndpoints(svcName string) {
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		out, err := exec.Command("kubectl", "get", "endpoints", svcName,
			"-n", namespace, "-o", "jsonpath={.subsets[*].addresses[*].ip}").Output()
		if err == nil && len(out) > 0 {
			return
		}
		time.Sleep(2 * time.Second)
	}
	GinkgoWriter.Printf("WARN: %s endpoints did not populate within 30s\n", svcName)
}

// calibrateRig measures kindnet/iperf3 jitter under natra throttling on
// this runner. Outputs:
//   - throttleDuration: how long each per-direction iperf3 run should
//     be so bucket-saturation amortizes below the dynamic cap.
//   - throttledCap: μ + 2σ + margin, with a floor at
//     throttleCapFloorRatio × rate. Used by every happy-path
//     throttle assertion.
//   - chaosBpsCap: throttledCap × chaosCapMultiplier. Used by every
//     chaos assertion.
//
// On any error keeps safe fallback values. Calibration on iperf-server
// (annotated 10M ingress) leaves its bucket drained — that's fine,
// it's what makes Topology A's enforcement assertion robust.
//
// Procedure:
//  1. Drain iperf-server's bucket with a long iperf3 so subsequent
//     short measurements aren't bucket-saturation-inflated.
//  2. Run 3 short measurements back-to-back (each starts near empty
//     bucket → measured rate ≈ steady-state, variance ≈ jitter).
//  3. Compute mean μ, stddev σ, relative jitter j = σ/μ.
//  4. throttledCap = μ × (1 + 2j + margin), floored at
//     rate × throttleCapFloorRatio.
//  5. throttleDuration solves
//     (1 + burstSeconds/d) × (1+2j) < (cap / rate), clamped to
//     [throttleDurationFloor, throttleDurationCeiling].
//
// The 2σ band is the symmetric noise budget; the +margin handles
// minor between-test variance the 3-sample window won't see.
func calibrateRig() {
	defer func() {
		// stdout (fmt.Printf) so the summary lands in CI logs even on
		// passing runs; GinkgoWriter buffers and only flushes on failure.
		fmt.Printf("calibration summary: throttleDuration=%ds throttledCap=%.2fMbps chaosBpsCap=%.2fMbps (rate=%.2fMbps annotation)\n",
			throttleDuration,
			float64(throttledCap)/1e6,
			float64(chaosBpsCap)/1e6,
			float64(bandwidthBps)/1e6)
	}()

	const drainSeconds = 10
	const calibSamples = 3
	const calibSeconds = 5

	drainCtx, drainCancel := context.WithTimeout(context.Background(), 60*time.Second)
	if _, err := runIperfWithError(drainCtx, iperfOpts{Target: "iperf-server", Duration: drainSeconds}); err != nil {
		GinkgoWriter.Printf("calibration drain failed (%v); keeping fallback caps + duration\n", err)
		drainCancel()
		return
	}
	drainCancel()

	samples := make([]float64, 0, calibSamples)
	for i := 0; i < calibSamples; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		bps, err := runIperfWithError(ctx, iperfOpts{Target: "iperf-server", Duration: calibSeconds})
		cancel()
		if err != nil || bps == 0 {
			GinkgoWriter.Printf("calibration sample %d/%d failed (err=%v bps=%d); keeping fallbacks\n",
				i+1, calibSamples, err, bps)
			return
		}
		samples = append(samples, float64(bps))
	}

	var sum float64
	for _, s := range samples {
		sum += s
	}
	mean := sum / float64(len(samples))
	if mean <= 0 {
		GinkgoWriter.Printf("calibration mean=0; keeping fallbacks\n")
		return
	}
	var sumSq float64
	for _, s := range samples {
		d := s - mean
		sumSq += d * d
	}
	stddev := math.Sqrt(sumSq / float64(len(samples)))
	jitter := stddev / mean

	// Dynamic cap: μ + 2σ + margin, floored at rate × floorRatio so
	// a freakishly-tight calibration doesn't produce a brittle cap.
	capFromMean := mean * (1.0 + 2.0*jitter + throttleCapMargin)
	capFloor := float64(bandwidthBps) * throttleCapFloorRatio
	if capFromMean < capFloor {
		capFromMean = capFloor
	}
	throttledCap = int64(capFromMean)
	chaosBpsCap = int64(capFromMean * chaosCapMultiplier)

	// Duration: same math as before but reads the cap from the
	// just-derived dynamic value instead of a hardcoded ratio.
	dynCapRatio := capFromMean / float64(bandwidthBps)
	safe := 1.0 + 2.0*jitter
	if dynCapRatio <= safe+0.005 {
		throttleDuration = throttleDurationCeiling
		fmt.Printf("calibration: mean=%.2fMbps stddev=%.2fMbps jitter=%.2f%% overshoot=%.1f%% — high jitter, clamping duration to %ds\n",
			mean/1e6, stddev/1e6, jitter*100, (mean/float64(bandwidthBps)-1)*100, throttleDuration)
		return
	}
	optimal := math.Ceil(burstSeconds / (dynCapRatio/safe - 1.0))
	d := int(optimal)
	if d < throttleDurationFloor {
		d = throttleDurationFloor
	}
	if d > throttleDurationCeiling {
		d = throttleDurationCeiling
	}
	throttleDuration = d
	fmt.Printf("calibration: mean=%.2fMbps stddev=%.2fMbps jitter=%.2f%% overshoot=%.1f%% → throttleDuration=%ds (raw %.1fs, clamped to [%d, %d])\n",
		mean/1e6, stddev/1e6, jitter*100, (mean/float64(bandwidthBps)-1)*100, throttleDuration, optimal, throttleDurationFloor, throttleDurationCeiling)
}

// runIperfWithDiagnostics is runIperf plus a 0-bps retry loop and a
// debug dump on persistent failure. Used by Topology F where a 0
// measurement is not just a failed assertion — it means we have no
// signal at all.
//
// 0-bps on a freshly-created pod usually means the Service routing
// hasn't propagated yet (kube-proxy's iptables / Endpoints
// controller can lag a few seconds past kubectl-Ready). Brief
// retries handle that without affecting topologies where 0 bps would
// be a real signal.
func runIperfWithDiagnostics(ctx context.Context, opts iperfOpts) int64 {
	const maxAttempts = 3
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		bps, err := runIperfWithError(ctx, opts)
		if err != nil {
			Fail(err.Error())
		}
		if bps > 0 {
			if attempt > 1 {
				GinkgoWriter.Printf("iperf3 succeeded on attempt %d/%d: %.2f Mbps\n",
					attempt, maxAttempts, float64(bps)/1e6)
			}
			return bps
		}
		if attempt < maxAttempts {
			GinkgoWriter.Printf("iperf3 returned 0 bps (attempt %d/%d); retrying after 3s\n",
				attempt, maxAttempts)
			time.Sleep(3 * time.Second)
		}
	}
	dumpDebugState(opts.Target)
	return 0
}

// dumpDebugState writes a snapshot of cluster + service state to
// GinkgoWriter so a 0-bps iperf result is debuggable from CI logs.
func dumpDebugState(svcName string) {
	dump := func(label string, args ...string) {
		out, _ := exec.Command(args[0], args[1:]...).CombinedOutput()
		GinkgoWriter.Printf("===== %s =====\n%s\n", label, out)
	}
	dump("kubectl get pods -n natra-e2e -o wide",
		"kubectl", "get", "pods", "-n", namespace, "-o", "wide")
	dump("kubectl get endpoints "+svcName,
		"kubectl", "get", "endpoints", svcName, "-n", namespace, "-o", "yaml")
	dump("kubectl describe pod "+svcName,
		"kubectl", "describe", "pod", svcName, "-n", namespace)
	dump("retry iperf3 with stderr",
		"kubectl", "exec", "-n", namespace, "iperf-client", "--",
		"iperf3", "-c", svcName, "-t", "3", "-J")
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
//
// Uses the dynamically-calibrated throttleDuration (see
// calibrateThrottleDuration). The bucket-saturation rate over a
// duration d starting from a full bucket is rate × (1 + burstSeconds/d);
// at d=10 against jitter on a slow runner that crosses the +20% cap.
// Calibration measures the per-runner jitter and picks the shortest
// duration with comfortable margin.
var _ = Describe("Topology B — egress only", func() {
	It("enforces egress-bandwidth on the server's outbound traffic", func() {
		ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
		defer cancel()
		bps := runIperf(ctx, iperfOpts{Target: "iperf-server-egress", Reverse: true, Duration: throttleDuration})
		logThrottled("egress", bps)
		Expect(bps).To(BeNumerically("<=", throttledCap),
			"reverse iperf3 should be throttled to ≈10 Mbps")
	})
})

// Topology C — sequential bidi. Both annotations on one pod, two
// iperf3 runs back-to-back. Uses calibrated throttleDuration for the
// same burst-dilution reason as Topology B.
var _ = Describe("Topology C — bidi sequential", func() {
	It("throttles forward to the ingress cap", func() {
		ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
		defer cancel()
		bps := runIperf(ctx, iperfOpts{Target: "iperf-server-bidi", Duration: throttleDuration})
		logThrottled("bidi/forward", bps)
		Expect(bps).To(BeNumerically("<=", throttledCap),
			"forward iperf3 on a both-annotated pod should be throttled")
	})

	It("throttles reverse to the egress cap", func() {
		ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
		defer cancel()
		bps := runIperf(ctx, iperfOpts{Target: "iperf-server-bidi", Reverse: true, Duration: throttleDuration})
		logThrottled("bidi/reverse", bps)
		Expect(bps).To(BeNumerically("<=", throttledCap),
			"reverse iperf3 on a both-annotated pod should be throttled")
	})
})

// Topology D — mixed. Three server pods on the worker, only mixed-a
// is annotated. mixed-b and mixed-c must keep baseline throughput so
// natra doesn't penalize unannotated pods sharing a node.
//
// mixed-a uses the calibrated throttleDuration. mixed-b and mixed-c
// stay at the default 5s — they're unthrottled, so the floor
// assertion is robust to short measurement windows.
var _ = Describe("Topology D — mixed (only some pods annotated)", func() {
	It("throttles only the annotated pod; other pods stay near baseline", func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
		defer cancel()

		bpsA := runIperf(ctx, iperfOpts{Target: "iperf-server-mixed-a", Duration: throttleDuration})
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
			wg     sync.WaitGroup
			fwdBps int64
			revBps int64
			fwdErr error
			revErr error
		)
		wg.Add(2)
		go func() {
			defer wg.Done()
			fwdBps, fwdErr = runIperfWithError(ctx, iperfOpts{
				Target: "iperf-server-bidi", Duration: throttleDuration,
			})
		}()
		go func() {
			defer wg.Done()
			revBps, revErr = runIperfWithError(ctx, iperfOpts{
				Target: "iperf-server-bidi", Reverse: true, Duration: throttleDuration,
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
// the same workload, asserts the delta is below `tolerance`.
//
// The point of this test is structural: catch a regression where
// natra starts charging unannotated pods (e.g. attaching BPF when
// neither annotation is present, or burning softirq cycles per
// packet on the unannotated path). Such a regression would show up
// as a multi-tens-of-percent throughput drop, not a single-digit
// one. `tolerance` is wide enough to absorb GH-runner jitter
// (observed 13.8% on a docs-only commit) while still tripping on
// the structural regressions this test exists to detect.
//
// Runs after the others so its DS-uninstall doesn't perturb them.
// AfterAll restores the DS even on failure.
var _ = Describe("Topology F — no-plugin regression", Ordered, func() {
	const tolerance = 0.20
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

		By("deploying unannotated server (flannel-only path)")
		reapplyPod(podName, manifest)

		// Brief settle window. kubectl wait --for=condition=Ready
		// returns when the container is ready, but Service endpoints
		// and CoreDNS take a beat longer to propagate.
		waitForServiceEndpoints(podName)

		baselineNoPlugin = runIperfWithDiagnostics(ctx, iperfOpts{Target: podName, Duration: throttleDuration})
		GinkgoWriter.Printf("no-plugin baseline: %.2f Mbps\n", float64(baselineNoPlugin)/1e6)
		Expect(baselineNoPlugin).To(BeNumerically(">", 0),
			"baseline should produce a real number")
	})

	It("re-measures with natra installed and asserts delta within tolerance", func() {
		ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
		defer cancel()

		By("reinstalling natra DaemonSet")
		installNatraDaemon()

		By("redeploying server pod so it picks up natra in the chained conflist")
		reapplyPod(podName, manifest)
		waitForServiceEndpoints(podName)

		baselineWithPlugin = runIperfWithDiagnostics(ctx, iperfOpts{Target: podName, Duration: throttleDuration})
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
	GinkgoWriter.Printf("[%s] throttled throughput: %.2f Mbps (cap rate %.2f Mbps; effective cap %.2f Mbps)\n",
		direction, float64(bps)/1e6, float64(bandwidthBps)/1e6, float64(throttledCap)/1e6)
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
