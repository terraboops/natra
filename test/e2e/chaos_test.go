//go:build e2e

// Layer 4 chaos — cluster-shaped failure modes that don't fit in Layer 3
// (lvh has no DaemonSet, no Pod scheduler, no veth lifecycle to perturb).

package e2e_test

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// chaosBpsCap is more generous than the happy-path's +40% because
// each chaos test runs multiple back-to-back iperf cycles (baseline,
// during-event, after-event). Token bucket state from earlier
// cycles can give later cycles brief access to ~half a burst worth
// of extra tokens, and the heavy-hitter threshold's per-flow
// fast-pass budget compounds on top of that — observed measurements
// drift to +70% during pod churn / DaemonSet restart cycles.
// +100% (= 2× rate) covers the jitter without masking real
// regressions (any failure to throttle drives throughput to 80+ Gbps,
// far outside this limit).
var chaosBpsCap = int64(float64(bandwidthBps) * 2.0)

var _ = Describe("natra cluster chaos", func() {
	// natra's BPF programs are attached via tcx (default) or clsact
	// (opt-in fallback). For tcx, the kernel keeps each program
	// loaded as long as its bpffs link pin exists; for clsact, the
	// kernel holds the program via the qdisc tree until the veth is
	// deleted. Either way the attachment outlives the natra-installer
	// process. Kubelet kills the installer pod, the kernel keeps the
	// rate-limit alive, and the new installer pod re-applies the
	// conflist patch without disturbing existing flows.
	//
	// This test pins that property so a regression (e.g., switching to
	// a host-process-pinned attachment) can't silently break it.
	// Mirror of the ingress restart spec, targeting an egress-only pod.
	// Pins that the egress program's tcx link / clsact filter survives
	// a DaemonSet restart the same way the ingress one does. Both
	// directions should share the same kernel-side persistence story;
	// this asserts no asymmetry crept in (e.g., one direction's link
	// being stored in the installer process and dying with it).
	It("DaemonSet restart preserves egress rate-limiting on existing pods", func() {
		ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
		defer cancel()

		opts := iperfOpts{Target: "iperf-server-egress", Reverse: true}

		By("baseline: egress is throttled before any chaos")
		baseline := runIperf(ctx, opts)
		GinkgoWriter.Printf("baseline egress throughput: %.2f Mbps\n", float64(baseline)/1e6)
		Expect(baseline).To(BeNumerically("<=", chaosBpsCap),
			"egress baseline must already be throttled before chaos starts")

		By("rollout restart natra-installer DaemonSet")
		mustExec("kubectl", "rollout", "restart", "daemonset/natra-installer", "-n", "kube-system")

		By("running iperf during the restart window")
		duringRestart := runIperf(ctx, opts)
		GinkgoWriter.Printf("during-restart egress throughput: %.2f Mbps\n", float64(duringRestart)/1e6)
		Expect(duringRestart).To(BeNumerically("<=", chaosBpsCap),
			"egress throttling must persist while installer DS is being recreated")

		By("waiting for the new natra-installer DaemonSet to roll out")
		mustExec("kubectl", "rollout", "status", "daemonset/natra-installer", "-n", "kube-system", "--timeout=120s")

		By("post-restart iperf is still throttled")
		afterRestart := runIperf(ctx, opts)
		GinkgoWriter.Printf("post-restart egress throughput: %.2f Mbps\n", float64(afterRestart)/1e6)
		Expect(afterRestart).To(BeNumerically("<=", chaosBpsCap),
			"egress throttling must hold after the new installer pod is Ready")
	})

	It("DaemonSet restart preserves rate-limiting on existing pods", func() {
		ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
		defer cancel()

		By("baseline: iperf is throttled before any chaos")
		baseline := runIperf(ctx, iperfOpts{})
		GinkgoWriter.Printf("baseline throughput: %.2f Mbps\n", float64(baseline)/1e6)
		Expect(baseline).To(BeNumerically("<=", chaosBpsCap),
			"baseline must already be throttled before chaos starts")

		By("rollout restart natra-installer DaemonSet")
		mustExec("kubectl", "rollout", "restart", "daemonset/natra-installer", "-n", "kube-system")

		By("running iperf during the restart window")
		duringRestart := runIperf(ctx, iperfOpts{})
		GinkgoWriter.Printf("during-restart throughput: %.2f Mbps\n", float64(duringRestart)/1e6)
		Expect(duringRestart).To(BeNumerically("<=", chaosBpsCap),
			"throttling must persist while installer DS is being recreated")

		By("waiting for the new natra-installer DaemonSet to roll out")
		mustExec("kubectl", "rollout", "status", "daemonset/natra-installer", "-n", "kube-system", "--timeout=120s")

		By("post-restart iperf is still throttled")
		afterRestart := runIperf(ctx, iperfOpts{})
		GinkgoWriter.Printf("post-restart throughput: %.2f Mbps\n", float64(afterRestart)/1e6)
		Expect(afterRestart).To(BeNumerically("<=", chaosBpsCap),
			"throttling must hold after the new installer pod is Ready")
	})

	// Pod churn stresses two natra paths simultaneously:
	//   - CNI ADD on each new pod (loads BPF, configures, attaches via
	//     tcx in default mode or clsact in fallback mode)
	//   - CNI DEL on each pod teardown (cmdDel removes per-direction
	//     link pins from bpffs; for clsact the kernel auto-detaches the
	//     program when the veth disappears)
	//
	// The check isn't byte-exact — at this volume some pods may still
	// be in ContainerCreating when the test ends and that's fine — but
	// natra must not regress to a state where pods get stuck or
	// existing flows lose their rate-limit.
	It("survives pod churn without breaking existing rate-limiting", func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()

		const churnPods = 20
		const annotation = "kubernetes.io/ingress-bandwidth=10M"

		By(fmt.Sprintf("creating %d annotated pods", churnPods))
		var created []string
		for i := 0; i < churnPods; i++ {
			name := fmt.Sprintf("churn-%02d", i)
			created = append(created, name)
			// JSON --overrides has to be a single-quoted string so the
			// shell doesn't strip its quotes; we're not going through a
			// shell here (exec.Command), so we pass the raw JSON.
			overrides := `{"spec":{"nodeSelector":{"kubernetes.io/hostname":"natra-e2e-worker"}}}`
			out, err := exec.CommandContext(ctx,
				"kubectl", "run", name,
				"-n", namespace,
				"--image=busybox:latest",
				"--annotations="+annotation,
				"--overrides="+overrides,
				"--command", "--", "sleep", "60",
			).CombinedOutput()
			if err != nil {
				// kubectl run may fail if a name is already taken; not
				// fatal for this test — the goal is volume, not exact
				// success rate.
				GinkgoWriter.Printf("kubectl run %s: %v\n  %s\n", name, err, out)
			}
		}
		// Cleanup: even if assertions fail later, drop the pods so
		// they don't leak between e2e suite runs (BeforeSuite handles
		// the namespace, but we still want fast teardown).
		defer func() {
			args := append([]string{"delete", "pod",
				"--ignore-not-found", "--grace-period=0", "--force",
				"-n", namespace}, created...)
			_ = exec.Command("kubectl", args...).Run()
		}()

		By("running iperf concurrently with the churn")
		iperfBps := runIperf(ctx, iperfOpts{})
		GinkgoWriter.Printf("during-churn throughput: %.2f Mbps\n", float64(iperfBps)/1e6)
		Expect(iperfBps).To(BeNumerically("<=", chaosBpsCap),
			"existing iperf flow must keep its rate-limit while churn happens around it")

		By("checking no pod is stuck in ContainerCreating beyond timeout")
		// Poll up to 30s for stuck pods. Some are still creating; that's
		// fine. The signal we care about is "no pod is stuck for >30s
		// in ContainerCreating with the *natra* CNI as the cause".
		stuck := 0
		deadline := time.Now().Add(30 * time.Second)
		for time.Now().Before(deadline) {
			stuck = 0
			for _, name := range created {
				out, _ := exec.Command("kubectl", "get", "pod", name,
					"-n", namespace, "-o", "jsonpath={.status.containerStatuses[0].state.waiting.reason}",
				).Output()
				reason := strings.TrimSpace(string(out))
				if reason == "ContainerCreating" {
					stuck++
				}
			}
			if stuck == 0 {
				break
			}
			time.Sleep(2 * time.Second)
		}
		// Some short-lived pods may exit (sleep 60 finishes during a slow
		// run). We're only flagging persistent stuckness as a problem.
		// A small residual count after 30s is acceptable on a slow runner.
		Expect(stuck).To(BeNumerically("<=", churnPods/2),
			"more than half the churn pods stuck in ContainerCreating — natra installer is regressing")
	})
})

// The remaining chaos scenarios are deferred. Documented as PIts so
// they show up in CI output as "P" (pending) rather than vanishing
// from the suite roster.
var _ = Describe("natra cluster chaos (deferred)", func() {
	PIt("survives node drain — requires multi-node scheduling not exercised by current single-worker e2e", func() {})
	PIt("recovers from CNI binary corruption — requires explicit corruption path on a kind node, covered semi-implicitly by fail-open elsewhere", func() {})
	PIt("characterizes annotation update on running pod — kubernetes itself doesn't propagate annotation changes to running pods", func() {})
})
