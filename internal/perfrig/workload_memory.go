package perfrig

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
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
		// kubectl wait needs an explicit resource type when given
		// only a selector; "pods -l app=…" is the documented form.
		if err := kubectl(ctx, kc, nil, "wait", "--for=condition=Ready", "pods",
			"-n", ns, "-l", "app=perf-server-mem", "--timeout=180s"); err != nil {
			return wr, fmt.Errorf("wait scale: %w", err)
		}
		// vanilla phase: re-patch TBF burst on the just-deployed
		// clones too. The initial patch in stagePhase only covered
		// the original perf-server pod; without re-patching, the
		// N-pod memory measurement counts the inflated kubelet-
		// default-burst structures and the per-pod kmem slope
		// includes that extra metadata, skewing the comparison.
		if phase == PhaseVanilla {
			if err := e.patchVanillaTBF(ctx); err != nil {
				return wr, fmt.Errorf("re-patch TBF after scale: %w", err)
			}
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
	// filter just returns no objects and the sum is 0. Separator
	// line between the two bpftool invocations so splitJSONArrays
	// can tell the prog and map arrays apart.
	if phase == PhaseNatra {
		out, err := sub.NodeShell(ctx, node,
			`bpftool -j prog show 2>/dev/null; echo; bpftool -j map show 2>/dev/null`)
		if err == nil {
			snap.bpfMemlockBytes = sumNatraBpfMemlock(out)
		}
		if snap.bpfMemlockBytes == 0 {
			fmt.Fprintf(os.Stderr,
				"==> natra phase bpf memlock = 0; bpftool err=%v outlen=%d head=%.200q\n",
				err, len(out), string(out))
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

// sumNatraBpfMemlock parses one or more concatenated `bpftool -j
// prog show` and `bpftool -j map show` arrays and sums
// bytes_memlock for objects whose `name` starts with `natra_`
// (BPF kernel names are truncated to 15 chars, so the full
// natra_config_map becomes natra_config_ma; the prefix match
// catches both forms).
//
// json.Decoder reads each top-level JSON value in turn, so
// concatenated arrays separated by whitespace are handled
// without a fragile string-split. The earlier splitJSONArrays
// approach split on `]` which broke on nested arrays like
// `map_ids: [...]`, silently returning 0.
func sumNatraBpfMemlock(raw []byte) int64 {
	dec := json.NewDecoder(bytes.NewReader(raw))
	var total int64
	for {
		var items []map[string]any
		if err := dec.Decode(&items); err != nil {
			break // io.EOF or malformed tail
		}
		for _, it := range items {
			name, _ := it["name"].(string)
			if !strings.HasPrefix(name, "natra_") {
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
	if phase == PhaseBaseline {
		return 0, nil
	}
	var name string
	switch phase {
	case PhaseVanilla:
		name = "bandwidth"
	case PhaseNatra:
		name = "natra"
	default:
		return 0, nil
	}
	// CNI binary location varies: /opt/cni/bin/ on kind/upstream,
	// /var/lib/rancher/k3s/data/cni/ on k3s 1.30+, sometimes a
	// versioned subdir. Find the binary rather than guess; bail
	// with 0 if it genuinely isn't anywhere.
	//
	// Peak RSS measurement: prefer /usr/bin/time -v when present
	// (lima debian has it via apt-install). When it's not (k3d
	// rancher/k3s base is busybox, no GNU time), fall back to
	// polling /proc/$pid/status's VmRSS while the plugin runs and
	// reporting the max we observed. The fallback misses the
	// absolute peak if the plugin's lifetime is shorter than one
	// poll cycle, but the CNI plugin runs long enough (BPF load +
	// attach, ~10-50ms) for a tight while-loop to catch the
	// steady-state high-water mark. Same output format as time -v
	// so parseTimeVPeakKB stays unified.
	script := fmt.Sprintf(`
        bin=""
        for p in /opt/cni/bin/%[1]s /var/lib/rancher/k3s/data/cni/%[1]s /var/lib/rancher/k3s/data/current/bin/%[1]s; do
          [ -x "$p" ] && { bin="$p"; break; }
        done
        if [ -z "$bin" ]; then exit 0; fi
        cni_env="CNI_COMMAND=VERSION CNI_CONTAINERID=x CNI_NETNS=/proc/self/ns/net CNI_IFNAME=eth0 CNI_PATH=$(dirname "$bin")"
        if [ -x /usr/bin/time ]; then
          /usr/bin/time -v env $cni_env "$bin" < /dev/null 2>&1 1>/dev/null || true
        else
          # Pure-shell fallback: fork the plugin, poll its VmRSS,
          # report the max as time -v's line format.
          env $cni_env "$bin" < /dev/null > /dev/null 2>&1 &
          pid=$!
          peak=0
          while [ -d /proc/$pid ]; do
            v=$(awk '/^VmRSS:/ {print $2}' /proc/$pid/status 2>/dev/null)
            [ -n "$v" ] && [ "$v" -gt "$peak" ] && peak=$v
          done
          wait $pid 2>/dev/null || true
          echo "Maximum resident set size (kbytes): $peak"
        fi
    `, name)
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
//
// Implementation notes:
//   - crictl needs an explicit --runtime-endpoint on k3s nodes;
//     the default socket isn't where k3s puts containerd.
//   - The label filter format is `key=value` literally; we pass
//     the raw "app=natra-installer" form straight through.
func installerDSPeakRSS(ctx context.Context, sub Substrate, node string, phase Phase) (int64, error) {
	if phase == PhaseBaseline {
		return 0, nil
	}
	label := ""
	switch phase {
	case PhaseNatra:
		// deploy/cni-installer.yaml labels the DS pods `app=natra`,
		// not app=natra-installer (the DS *resource* name is
		// natra-installer but the pod template label is app=natra).
		label = "app=natra"
	case PhaseVanilla:
		label = "app=vanilla-installer"
	}
	// Read VmRSS directly from /proc/<sandbox-pid>/status rather
	// than cgroup memory.current. For an installer DS that's just
	// running pause post-install, the sandbox pid's own VmRSS is
	// exactly the few MB we care about — no cgroup-path
	// interpretation needed. The earlier cgroup approach hit two
	// real problems: (a) on cgroup v1+v2 hybrid mode the "0::"
	// line points at a slice that aggregates many pods (k3d
	// natra showed 575 MB when expected ~few MB), and (b) the
	// path traversal was substrate-dependent.
	//
	// VmRSS is reported in kB; convert to bytes for the
	// MemorySample.InstallerDSBytes field.
	script := fmt.Sprintf(`
        pod=$(%[1]s pods -q --label "%[2]s" 2>/dev/null | head -1)
        if [ -z "$pod" ]; then echo 0; exit 0; fi
        pid=$(%[1]s inspectp "$pod" 2>/dev/null \
            | grep -oE '"pid":[[:space:]]*[0-9]+' \
            | grep -oE '[0-9]+' \
            | head -1)
        if [ -n "$pid" ] && [ -f /proc/$pid/status ]; then
          rss_kb=$(awk '/^VmRSS:/ {print $2}' /proc/$pid/status 2>/dev/null)
          if [ -n "$rss_kb" ]; then
            echo $((rss_kb * 1024))
          else
            echo 0
          fi
        else
          echo 0
        fi
    `, crictlCmd, label)
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
//
// The base manifest is a multi-doc YAML (Pod + Service); we keep
// only the Pod doc when cloning so we don't re-apply the Service
// for each clone (the original Service is already in place from
// stagePhase) and we don't accidentally repoint the Service
// selector at the clones. The pod's existing `app: perf-server`
// label gets rewritten to `app: perf-server-mem` so the wait/
// teardown selector matches just the clones.
func (e *Executor) deployMemoryPods(ctx context.Context, ns string, n int, phase Phase) error {
	server, worker := e.Substrate.Nodes()
	kc := e.Substrate.KubeconfigPath()
	manifest, err := renderPerfManifest(e.RepoRoot, "test/perf/realworld/perf-server.yaml",
		ns, server, worker, e.PerfclientImage)
	if err != nil {
		return err
	}
	if phase == PhaseBaseline {
		manifest = stripBandwidthAnnotations(manifest)
	}
	// Pod doc only — drop the Service section (everything from
	// the first `---` onward).
	if i := strings.Index(manifest, "\n---\n"); i >= 0 {
		manifest = manifest[:i]
	}
	docs := make([]string, 0, n-1)
	for i := 2; i <= n; i++ {
		name := fmt.Sprintf("perf-server-mem-%d", i)
		clone := strings.Replace(manifest, "name: perf-server", "name: "+name, 1)
		// Repoint the pod's existing label in place — no second
		// `labels:` key inserted, so the YAML stays valid and the
		// pod actually carries the clone label.
		clone = strings.Replace(clone, "app: perf-server", "app: perf-server-mem", 1)
		docs = append(docs, clone)
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
