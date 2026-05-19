package perfrig

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// runMemory measures the three memory comparables defined in the
// spec: dataplane kernel memory at 1 → N annotated pods (per-phase
// kernel memory delta attributable to that phase's dataplane
// mechanism), CNI plugin invocation peak RSS via ru_maxrss, and
// persistent installer DaemonSet RSS via cgroup memory.current.
// baseline = 0 (measured as the empirical noise floor).
//
// One sample per invocation, repeated Plan.Samples times. Each
// sample scales 1 → N → 1 by deploying / deleting clone pods so
// the next sample starts from a clean slate.
func (e *Executor) runMemory(ctx context.Context, phase Phase) (WorkloadReport, error) {
	wr := WorkloadReport{Kind: WorkloadMemory}
	ns := e.namespaceForPhase()
	kc := e.Substrate.KubeconfigPath()
	_, worker := e.Substrate.Nodes()
	n := e.MemoryNPodCount
	if n <= 1 {
		n = 8
	}

	if err := e.Substrate.EnsureBpftool(ctx, worker); err != nil {
		return wr, fmt.Errorf("ensure bpftool on %s: %w", worker, err)
	}

	for s := 1; s <= e.Plan.Samples; s++ {
		e.logf("==> [%s] memory sample %d/%d (1→%d pods)\n", phase, s, e.Plan.Samples, n)

		// At-1: perf-server is already deployed from stagePhase.
		base, err := snapshotMemory(ctx, e.Substrate, worker, phase)
		if err != nil {
			return wr, fmt.Errorf("snapshot @1 sample %d: %w", s, err)
		}

		// Scale to N annotated pods. Pods are clones of the
		// perf-server manifest, named perf-server-mem-2..N, pinned
		// to the worker node so all N qdiscs / BPF maps land on the
		// node we're measuring.
		if err := e.deployMemoryPods(ctx, ns, n, phase); err != nil {
			return wr, fmt.Errorf("scale to %d: %w", n, err)
		}
		if err := kubectl(ctx, kc, nil, "wait", "--for=condition=Ready",
			"-n", ns, "-l", "app=perf-server-mem", "--timeout=180s"); err != nil {
			return wr, fmt.Errorf("wait scale: %w", err)
		}

		atN, err := snapshotMemory(ctx, e.Substrate, worker, phase)
		if err != nil {
			return wr, fmt.Errorf("snapshot @N sample %d: %w", s, err)
		}

		// Plugin invocation peak RSS — once per sample, after the
		// scale so the binary location on the node is settled.
		invokeRSS, err := pluginInvokePeakRSS(ctx, e.Substrate, worker, phase)
		if err != nil {
			e.logf("  [%s s%d] plugin invoke RSS unavailable: %v\n", phase, s, err)
		}

		// Installer DaemonSet RSS — persistent userspace.
		dsRSS, err := installerDSPeakRSS(ctx, e.Substrate, worker, phase)
		if err != nil {
			e.logf("  [%s s%d] installer DS RSS unavailable: %v\n", phase, s, err)
		}

		wr.MemorySamples = append(wr.MemorySamples, MemorySample{
			Sample:                   s,
			DataplaneKmemBytes1:      base.kernelMemBytes,
			DataplaneKmemBytesN:      atN.kernelMemBytes,
			VanillaQdiscBytes:        atN.tcQdiscCount, // count, not bytes — see field doc
			NatraBpfMemlockBytes:     atN.bpfMemlockBytes,
			PluginInvokePeakRSSBytes: invokeRSS,
			InstallerDSBytes:         dsRSS,
			NPodCount:                n,
		})
		e.logf("  [%s s%d] kmem@1=%dKB kmem@N=%dKB Δ/pod=%dKB | bpfmemlock=%dB qdiscs=%d | invokeRSS=%dKB dsRSS=%dKB\n",
			phase, s,
			base.kernelMemBytes/1024, atN.kernelMemBytes/1024,
			(atN.kernelMemBytes-base.kernelMemBytes)/int64(n-1)/1024,
			atN.bpfMemlockBytes, atN.tcQdiscCount,
			invokeRSS/1024, dsRSS/1024)

		// Scale back down for the next sample.
		if err := e.teardownMemoryPods(ctx, ns); err != nil {
			return wr, fmt.Errorf("scale-down sample %d: %w", s, err)
		}
	}
	return wr, nil
}

type memSnapshot struct {
	kernelMemBytes  int64
	bpfMemlockBytes int64
	tcQdiscCount    int64
}

// snapshotMemory reads the three node-local memory signals via
// NodeShell, returning a tagged value object so the caller can
// diff between snapshots. Each method is the spec's documented
// measurement for that signal.
func snapshotMemory(ctx context.Context, sub Substrate, node string, phase Phase) (memSnapshot, error) {
	var snap memSnapshot

	// /proc/meminfo Slab+KernelStack+PageTables — the same ruler
	// for every phase; the per-phase delta attributes to that
	// phase's mechanism (qdiscs for vanilla, BPF for natra,
	// ~0 for baseline = noise floor).
	out, err := sub.NodeShell(ctx, node,
		`awk '/^Slab:/ {s=$2} /^KernelStack:/ {k=$2} /^PageTables:/ {p=$2} END {print (s+k+p)*1024}' /proc/meminfo`)
	if err != nil {
		return snap, fmt.Errorf("meminfo: %w", err)
	}
	if v, perr := strconv.ParseInt(strings.TrimSpace(string(out)), 10, 64); perr == nil {
		snap.kernelMemBytes = v
	}

	// bpftool memlock — natra phase byte-exact corroboration; zero
	// elsewhere. We always try it; on baseline/vanilla the natra_*
	// filter just returns no objects and the sum is 0.
	if phase == PhaseNatra {
		out, err := sub.NodeShell(ctx, node,
			`(bpftool -j prog show 2>/dev/null; bpftool -j map show 2>/dev/null) || true`)
		if err == nil {
			snap.bpfMemlockBytes = sumNatraBpfMemlock(out)
		}
	}

	// tc qdisc count — vanilla phase corroboration that the qdiscs
	// are there. The actual memory footprint is the meminfo delta;
	// this is "how many" as evidence the kernel-mem delta has the
	// right shape.
	if phase == PhaseVanilla {
		out, err := sub.NodeShell(ctx, node, `tc qdisc show | awk '/qdisc tbf/' | wc -l`)
		if err == nil {
			if v, perr := strconv.ParseInt(strings.TrimSpace(string(out)), 10, 64); perr == nil {
				snap.tcQdiscCount = v
			}
		}
	}

	return snap, nil
}

// sumNatraBpfMemlock parses concatenated `bpftool -j prog show`
// and `bpftool -j map show` output (each a top-level JSON array)
// and sums bytes_memlock for objects named natra_* or matching
// the natra prog names.
func sumNatraBpfMemlock(raw []byte) int64 {
	// bpftool emits one JSON array per command, concatenated. The
	// objects are uniform-ish; we look for "name" and
	// "bytes_memlock" fields. A naive split-on-`][` followed by
	// per-array Unmarshal handles the concatenation without
	// requiring a custom decoder.
	chunks := splitJSONArrays(string(raw))
	var total int64
	for _, chunk := range chunks {
		var items []map[string]any
		if err := json.Unmarshal([]byte(chunk), &items); err != nil {
			continue
		}
		for _, it := range items {
			name, _ := it["name"].(string)
			if !strings.HasPrefix(name, "natra_") && name != "natra_ingress" && name != "natra_egress" {
				continue
			}
			switch v := it["bytes_memlock"].(type) {
			case float64:
				total += int64(v)
			case int64:
				total += v
			}
		}
	}
	return total
}

// splitJSONArrays splits a string containing one or more
// concatenated JSON arrays (`][`) into individual `[...]` strings.
func splitJSONArrays(s string) []string {
	parts := strings.SplitAfter(s, "]")
	var out []string
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if strings.HasPrefix(p, "[") && strings.HasSuffix(p, "]") {
			out = append(out, p)
		} else if strings.HasPrefix(p, "[") {
			out = append(out, p+"]")
		}
	}
	if len(out) == 0 && strings.HasPrefix(strings.TrimSpace(s), "[") {
		return []string{strings.TrimSpace(s)}
	}
	return out
}

// pluginInvokePeakRSS captures the per-CNI-ADD peak RSS of the
// phase's plugin binary via `/usr/bin/time -v <plugin>` with
// `CNI_COMMAND=VERSION` — the lightest possible invocation that
// still loads the binary's runtime, libs and init code, which is
// the comparable startup-cost story. baseline returns 0 (no
// plugin to invoke).
//
// We do not run a full CNI ADD in this harness because (a) it
// would mutate the node, (b) a faithful ADD requires a netns we
// don't want to fabricate just for the measurement, and (c) for
// a natra-vs-vanilla comparable, the binary's startup memory
// shape is the right unit — full-ADD memory would also include
// the system state both plugins touch, which is not the
// plugin's cost.
func pluginInvokePeakRSS(ctx context.Context, sub Substrate, node string, phase Phase) (int64, error) {
	var bin string
	switch phase {
	case PhaseBaseline:
		return 0, nil
	case PhaseVanilla:
		bin = "/opt/cni/bin/bandwidth"
	case PhaseNatra:
		bin = "/opt/cni/bin/natra"
	default:
		return 0, nil
	}
	script := fmt.Sprintf(
		`if [ -x %s ]; then /usr/bin/time -v env CNI_COMMAND=VERSION CNI_CONTAINERID=x CNI_NETNS=/proc/self/ns/net CNI_IFNAME=eth0 CNI_PATH=/opt/cni/bin %s < /dev/null 2>&1 >/dev/null || true; fi`,
		bin, bin)
	out, err := sub.NodeShell(ctx, node, script)
	if err != nil {
		return 0, err
	}
	return parseTimeVPeakKB(string(out)) * 1024, nil
}

var timeVPeakRe = regexp.MustCompile(`Maximum resident set size \(kbytes\):\s*(\d+)`)

func parseTimeVPeakKB(out string) int64 {
	m := timeVPeakRe.FindStringSubmatch(out)
	if len(m) < 2 {
		return 0
	}
	v, _ := strconv.ParseInt(m[1], 10, 64)
	return v
}

// installerDSPeakRSS reads the cgroup memory.current of the
// phase's installer DaemonSet pod on the worker node. baseline
// returns 0 (no installer). vanilla returns 0 unless the rig has
// deployed the upstream vanilla-installer DS (deploy/cni-installer
// for the natra phase is the typical case here).
func installerDSPeakRSS(ctx context.Context, sub Substrate, node string, phase Phase) (int64, error) {
	if phase == PhaseBaseline {
		return 0, nil
	}
	label := ""
	switch phase {
	case PhaseNatra:
		label = "app=natra-installer"
	case PhaseVanilla:
		label = "app=vanilla-installer"
	}
	// Find the DS pod's pid namespace on the node and sum its cgroup
	// memory.current. The script is a best-effort one-liner; if the
	// installer DS isn't present, the script prints 0 and exits 0.
	script := fmt.Sprintf(`
        pid=$(crictl ps -q --label io.kubernetes.pod.label.%s 2>/dev/null | head -1 | xargs -r crictl inspect 2>/dev/null | awk '/"pid":/ {print $2}' | tr -d ',' | head -1)
        if [ -n "$pid" ] && [ -f /proc/$pid/cgroup ]; then
          cg=$(awk -F: '$2 ~ /memory/ || $1 == "0" {print $3}' /proc/$pid/cgroup | head -1)
          if [ -f /sys/fs/cgroup$cg/memory.current ]; then
            cat /sys/fs/cgroup$cg/memory.current
          else
            echo 0
          fi
        else
          echo 0
        fi
    `, strings.ReplaceAll(label, "=", "."))
	out, err := sub.NodeShell(ctx, node, script)
	if err != nil {
		return 0, err
	}
	v, _ := strconv.ParseInt(strings.TrimSpace(string(out)), 10, 64)
	return v, nil
}

// deployMemoryPods deploys n-1 clones of perf-server, pinned to
// the worker node, labelled app=perf-server-mem so they can be
// waited on / cleaned up as a set.
func (e *Executor) deployMemoryPods(ctx context.Context, ns string, n int, phase Phase) error {
	server, worker := e.Substrate.Nodes()
	kc := e.Substrate.KubeconfigPath()
	// Render the base perf-server manifest once.
	manifest, err := renderPerfManifest(e.RepoRoot, "test/perf/realworld/perf-server.yaml",
		ns, server, worker, e.PerfclientImage)
	if err != nil {
		return err
	}
	if phase == PhaseBaseline {
		manifest = stripBandwidthAnnotations(manifest)
	}
	// Emit n-1 renamed copies. Naive string transform: replace the
	// pod name "perf-server" with "perf-server-mem-<i>" and add the
	// app label. Two manifests joined with --- so kubectl applies
	// them as one document.
	var docs []string
	for i := 2; i <= n; i++ {
		name := fmt.Sprintf("perf-server-mem-%d", i)
		copy := strings.Replace(manifest, "name: perf-server", "name: "+name, 1)
		// Add the app label under metadata; the manifest already has
		// a labels: section we can extend with sed-style insertion.
		copy = strings.Replace(copy,
			"  name: "+name,
			"  name: "+name+"\n  labels:\n    app: perf-server-mem",
			1)
		docs = append(docs, copy)
	}
	bigDoc := strings.Join(docs, "\n---\n")
	return kubectl(ctx, kc, strings.NewReader(bigDoc), "apply", "-f", "-")
}

// teardownMemoryPods deletes the memory-workload clones so the
// next sample starts at 1 pod again.
func (e *Executor) teardownMemoryPods(ctx context.Context, ns string) error {
	kc := e.Substrate.KubeconfigPath()
	return kubectl(ctx, kc, nil, "delete", "pod",
		"-n", ns, "-l", "app=perf-server-mem",
		"--ignore-not-found", "--grace-period=0", "--force")
}
