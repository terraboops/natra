//go:build e2e

// Layer 4 chaos — cluster-shaped failure modes that don't fit in Layer 3
// (lvh has no DaemonSet, no Pod scheduler, no veth lifecycle to perturb).

package e2e_test

import "testing"

func TestDaemonSetRestartMidTraffic(t *testing.T) {
	t.Skip("Phase 1: start iperf flow with annotation, kubectl rollout restart daemonset/natra, assert existing flows survive (tcx-attached programs persist across binary restart) and new pods get bandwidth limiting")
}

func TestPodChurnDuringTraffic(t *testing.T) {
	t.Skip("Phase 1: create/delete 50 annotated pods in tight loop with concurrent test flow; assert no veth leaks, no BPF map slot leaks, no ContainerCreating > 30s")
}

func TestNodeDrain(t *testing.T) {
	t.Skip("Phase 1: drain one node, verify pods reschedule and bandwidth limits re-apply on the new node")
}

func TestCNIBinaryCorrupt(t *testing.T) {
	t.Skip("Phase 1: kill DaemonSet, corrupt /opt/cni/bin/natra on a node, attempt pod creation; assert clear error in describe pod, no kernel panic, pod not silently un-rate-limited")
}

func TestAnnotationUpdateOnRunningPod(t *testing.T) {
	t.Skip("Phase 1: characterization test — pin current behavior of changing kubernetes.io/ingress-bandwidth on a running pod (likely: requires pod restart)")
}
