package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"runtime/pprof"
	"strings"
	"time"

	"github.com/containernetworking/cni/pkg/skel"
	"github.com/containernetworking/cni/pkg/types"
	current "github.com/containernetworking/cni/pkg/types/100"
	"github.com/containernetworking/cni/pkg/version"

	"github.com/terraboops/natra/pkg/bpf"
	"github.com/terraboops/natra/pkg/cni/config"
)

// pinDir holds the bpffs paths for natra's per-pod tcx-link pins and
// per-pod map pins. A dedicated subdir keeps natra's pins separate from
// other tooling on the node and makes cleanup straightforward (cmdDel
// removes the per-container files).
const pinDir = "/sys/fs/bpf/natra"

// pinPathFor is the bpffs path for the pinned tcx link of a given
// container's veth in a given (side, direction). Side is embedded
// instead of the interface name — pod-side eth0 is uniform across
// pods but host-side veth names are random per pod, so encoding the
// side makes ls output legible regardless.
//
// Bpffs forbids dots in pin file names (kernel/bpf/inode.c::bpf_lookup
// returns EPERM on any name containing '.' when the parent dir has
// any S_IALLUGO bits — those names are reserved for kernel-internal
// special files created by populate_bpffs). So no `.link` extension;
// we use a `-link` suffix instead.
func pinPathFor(containerID string, side bpf.Side, dir bpf.Direction) string {
	return filepath.Join(pinDir, containerID+"-"+side.String()+"-"+dir.String()+"-link")
}

var (
	pluginVersion = "dev"
	commit        = "none"
	date          = "unknown"
)

// NetConf is natra's slice of the CNI stdin payload. Embeds
// types.NetConf for cniVersion / name / type / prevResult.
//
// RuntimeConfig.Bandwidth is what kubelet populates when the conflist
// declares `capabilities.bandwidth: true`. PodAnnotations is the
// older direct path; if both channels are present, Bandwidth wins.
// AttachMode is the top-level natra-specific knob — one of
// "tcx-hostside" (default), "tcx-podside", "clsact-hostside",
// "clsact-podside".
//
// Defaults is the cluster-wide knob block, written by the installer
// from NATRA_DEFAULT_* env vars. Lets an operator change natra's
// baseline (heavy-hitter threshold, burst-to-rate ratio) without
// rebuilding the binary. Per-pod annotations still win where they
// specify a value.
type NetConf struct {
	types.NetConf
	AttachMode    string           `json:"attachMode,omitempty"`
	Defaults      *NetConfDefaults `json:"defaults,omitempty"`
	RuntimeConfig struct {
		Bandwidth *struct {
			IngressRate  int64 `json:"ingressRate,omitempty"`
			IngressBurst int64 `json:"ingressBurst,omitempty"`
			EgressRate   int64 `json:"egressRate,omitempty"`
			EgressBurst  int64 `json:"egressBurst,omitempty"`
		} `json:"bandwidth,omitempty"`
		PodAnnotations map[string]string `json:"podAnnotations,omitempty"`
	} `json:"runtimeConfig,omitempty"`
}

// NetConfDefaults holds cluster-wide defaults. Both fields are
// optional; zero means "use natra's compiled-in default".
type NetConfDefaults struct {
	// HHThreshold overrides the heavy-hitter threshold default
	// (compiled-in: 10). Lower = stricter shaping (mice become
	// elephants faster); higher = more mice protection.
	HHThreshold int64 `json:"hhThreshold,omitempty"`
	// BurstRatio overrides the default burst-to-rate ratio
	// (compiled-in: 2). A 10 Mbps annotation with ratio 2 gives a
	// 2.5 MB token bucket (= 2 seconds of credit).
	BurstRatio float64 `json:"burstRatio,omitempty"`
}

// attachAttempt is a single (hook, side) pair to try.
type attachAttempt struct {
	hook bpf.Hook
	side bpf.Side
}

func (a attachAttempt) String() string {
	return fmt.Sprintf("%s-%s", a.hook, a.side)
}

// resolveAttachStrategy parses NetConf.AttachMode into the ordered
// list of attempts to make. Empty string ("" or "auto") expands to
// the default fallback chain:
//
//	tcx-hostside → tcx-podside → clsact-hostside → clsact-podside
//
// The chain prefers tcx (multi-program friendly, the cilium / AWS
// NPA shape) and host-side (peer of pod's eth0, same surface other
// BPF stacks use). Real-world fallbacks: old kernels (< 6.6) skip
// every tcx step and land on clsact; locked-down host netns skips
// hostside and lands on podside.
//
// Explicit values ("tcx-hostside", etc.) produce a single-attempt
// list with no fallback — the operator asked for exactly that
// combination, so honor it.
func resolveAttachStrategy(s string) ([]attachAttempt, error) {
	switch s {
	case "", "auto":
		return []attachAttempt{
			{bpf.HookTCX, bpf.SideHost},
			{bpf.HookTCX, bpf.SidePod},
			{bpf.HookClsact, bpf.SideHost},
			{bpf.HookClsact, bpf.SidePod},
		}, nil
	case "tcx-hostside":
		return []attachAttempt{{bpf.HookTCX, bpf.SideHost}}, nil
	case "tcx-podside":
		return []attachAttempt{{bpf.HookTCX, bpf.SidePod}}, nil
	case "clsact-hostside":
		return []attachAttempt{{bpf.HookClsact, bpf.SideHost}}, nil
	case "clsact-podside":
		return []attachAttempt{{bpf.HookClsact, bpf.SidePod}}, nil
	default:
		return nil, fmt.Errorf(
			"unknown attachMode %q (want one of: auto, %s)",
			s, "tcx-hostside, tcx-podside, clsact-hostside, clsact-podside",
		)
	}
}

func main() {
	// natra is normally invoked by kubelet via the CNI ABI (no CLI
	// args; stdin + env vars). The exceptions are subcommands the
	// DaemonSet uses: `install-cni-chain` patches existing conflists
	// to chain natra in, and `dump-stats` reads the pinned maps for
	// a given containerID.
	if len(os.Args) > 1 && os.Args[1] == "install-cni-chain" {
		if err := installCNIChain(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}
	if len(os.Args) > 1 && os.Args[1] == "dump-stats" {
		if err := dumpStats(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}
	if len(os.Args) > 1 && os.Args[1] == "profile" {
		if err := profileCmd(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}

	skel.PluginMainFuncs(
		skel.CNIFuncs{
			Add:   cmdAdd,
			Del:   cmdDel,
			Check: cmdCheck,
		},
		version.All,
		fmt.Sprintf("natra CNI plugin %s (commit: %s, built: %s)", pluginVersion, commit, date),
	)
}

// cmdAdd handles a CNI ADD. Order:
//  1. Parse stdin → NetConf.
//  2. Resolve bandwidth per direction from runtimeConfig + annotations;
//     pass through if neither direction has a rate.
//  3. Load BPF object, configure each direction's rate/burst, attach
//     each direction's program to the chosen veth side.
//  4. Print PrevResult so the next chained plugin (or kubelet) sees
//     what came in — natra doesn't modify networking, only adds rate
//     limiting on top.
//
// Past stdin parsing, natra is fail-open: anything that goes wrong is
// logged and the plugin still returns success. A Pod stuck in
// ContainerCreating because BPF wouldn't load is worse than a Pod
// running unrate-limited.
//
// Stderr is captured by the CNI runtime (containerd, kubelet) but only
// on plugin error. We also append every log line to /var/log/natra-cni.log
// so successful invocations leave a trace — useful for "is natra even
// being called?" without cranking up runtime log levels.
func cmdAdd(args *skel.CmdArgs) error {
	defer maybeWriteHeapProfile(args.ContainerID)
	logf("ADD containerID=%s netns=%s ifname=%s", args.ContainerID, args.Netns, args.IfName)
	logCaps()
	conf, err := parseConfig(args.StdinData)
	if err != nil {
		return fmt.Errorf("parse config: %w", err)
	}

	strategy, err := resolveAttachStrategy(conf.AttachMode)
	if err != nil {
		return fmt.Errorf("attachMode: %w", err)
	}

	ingressCfg := resolveDirectionConfig(conf, bpf.DirectionIngress)
	egressCfg := resolveDirectionConfig(conf, bpf.DirectionEgress)
	if ingressCfg == nil && egressCfg == nil {
		logf("no rate limit on either direction, passing through")
		return passthrough(args, conf)
	}
	logf("config resolved: ingress=%v egress=%v strategy=%v",
		describeCfg(ingressCfg), describeCfg(egressCfg), strategy)

	if err := attachBPF(args, ingressCfg, egressCfg, strategy); err != nil {
		// Fail-open: log and continue.
		fmt.Fprintf(os.Stderr, "natra: BPF attach failed (%v) — passing through unrate-limited\n", err)
		logf("attachBPF FAILED: %v", err)
	}

	return passthrough(args, conf)
}

// describeCfg formats a Config for the trace log; "off" when nil.
func describeCfg(cfg *config.Config) string {
	if cfg == nil {
		return "off"
	}
	return fmt.Sprintf("rate=%d burst=%d hh=%d", cfg.Rate, cfg.Burst, cfg.HeavyHitterThreshold)
}

// logf appends a line to /var/log/natra-cni.log. Best effort — if the
// path isn't writable we drop the message. The file is host-mounted by
// the DaemonSet so `tail -f /var/log/natra-cni.log` on a node shows
// every CNI invocation across every pod.
func logf(format string, args ...any) {
	msg := fmt.Sprintf("[%d %s] ", os.Getpid(), os.Args[0]) + fmt.Sprintf(format, args...) + "\n"
	if f, err := os.OpenFile("/var/log/natra-cni.log", os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o644); err == nil {
		_, _ = f.WriteString(msg)
		_ = f.Close()
	}
}

// logCaps writes the effective-capability lines from /proc/self/status
// to the log. Useful for diagnosing EPERM on BPF_OBJ_PIN and similar:
// kubelet may strip caps the parent process has when invoking the
// CNI plugin, even with file-cap setcap on the binary.
func logCaps() {
	f, err := os.Open("/proc/self/status")
	if err != nil {
		return
	}
	defer func() { _ = f.Close() }()
	scan := bufio.NewScanner(f)
	for scan.Scan() {
		line := scan.Text()
		if strings.HasPrefix(line, "Cap") {
			logf("caps: %s", line)
		}
	}
}

// cmdDel cleans up the bpffs pins for this container. Two kinds of pins
// can exist:
//
//   - The tcx-link pin (<containerID>-<side>-<direction>-link).
//     Removing it drops the kernel's last reference to the link, which
//     detaches the BPF program. Only present in HookTCX modes.
//   - The per-container map pins (<containerID>-{config,bucket,stats,cms}-map).
//     Useful for the dump-stats subcommand. Both hook modes write these.
//
// Walks the pin dir once and removes everything with the container's
// prefix. CNI DEL is idempotent — missing files are not errors. We
// don't bother distinguishing modes because both branches end at the
// same result: the container's pins are gone.
func cmdDel(args *skel.CmdArgs) error {
	prefix := args.ContainerID + "-"
	entries, err := os.ReadDir(pinDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		fmt.Fprintf(os.Stderr, "natra: read pin dir %s: %v\n", pinDir, err)
		return nil
	}
	for _, e := range entries {
		if !strings.HasPrefix(e.Name(), prefix) {
			continue
		}
		path := filepath.Join(pinDir, e.Name())
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			fmt.Fprintf(os.Stderr, "natra: remove pin %s: %v\n", path, err)
		}
	}
	return nil
}

// cmdCheck is a no-op success. Re-entering the pod netns to verify the
// clsact filter is attached would require listing tc filters per
// ifindex; kubelet uses CHECK as a liveness hint and a false positive
// is no worse than the fail-open path elsewhere.
func cmdCheck(*skel.CmdArgs) error {
	return nil
}

func parseConfig(stdin []byte) (*NetConf, error) {
	conf := &NetConf{}
	if err := json.Unmarshal(stdin, conf); err != nil {
		return nil, fmt.Errorf("parse network config: %w", err)
	}
	return conf, nil
}

// resolveDirectionConfig pulls the per-direction rate limit out of the
// parsed stdin. Two channels, in order of preference:
//
//  1. RuntimeConfig.Bandwidth, populated by kubelet when the conflist
//     declares `capabilities.bandwidth: true`. Rate is in bits/sec
//     (kubelet/upstream convention); we divide by 8 to get bytes/sec
//     for the BPF program.
//
//  2. RuntimeConfig.PodAnnotations, the older path where the raw
//     annotation string passes through and we parse it ourselves.
//     Already bytes/sec.
//
// Burst is clamped to 2× rate. Kubelet sets burst to MaxUint32 (~4 GB)
// when the annotation doesn't specify one, which would let a high-rate
// flow saturate the link for ~30s before the bucket caught up. 2× rate
// is a one-second burst window — same heuristic the upstream plugin uses.
//
// Returns nil if neither channel produces a usable rate for this
// direction. Each direction is resolved independently so a pod with
// only one annotation gets one program attached, not both.
func resolveDirectionConfig(conf *NetConf, dir bpf.Direction) *config.Config {
	annotationKey := "kubernetes.io/ingress-bandwidth"
	var rateBits, burstBits int64
	if conf.RuntimeConfig.Bandwidth != nil {
		switch dir {
		case bpf.DirectionIngress:
			rateBits = conf.RuntimeConfig.Bandwidth.IngressRate
			burstBits = conf.RuntimeConfig.Bandwidth.IngressBurst
		case bpf.DirectionEgress:
			rateBits = conf.RuntimeConfig.Bandwidth.EgressRate
			burstBits = conf.RuntimeConfig.Bandwidth.EgressBurst
		}
	}
	if dir == bpf.DirectionEgress {
		annotationKey = "kubernetes.io/egress-bandwidth"
	}

	// Cluster-default burst ratio. Falls back to 2× when the
	// conflist doesn't set one — same value baked into pkg/cni/config.
	burstRatio := 2.0
	if conf.Defaults != nil && conf.Defaults.BurstRatio > 0 {
		burstRatio = conf.Defaults.BurstRatio
	}

	if rateBits > 0 {
		out := config.DefaultConfig()
		out.Rate = rateBits / 8 // bits → bytes
		burst := burstBits / 8
		maxBurst := int64(float64(out.Rate) * burstRatio)
		if burst <= 0 || burst > maxBurst {
			burst = maxBurst
		}
		out.Burst = burst
		// Cluster-default heavy-hitter threshold. kubelet's
		// runtimeConfig.bandwidth has no per-pod way to specify
		// this, so the cluster default is the only knob.
		if conf.Defaults != nil && conf.Defaults.HHThreshold > 0 {
			out.HeavyHitterThreshold = conf.Defaults.HHThreshold
		}
		return out
	}
	if conf.RuntimeConfig.PodAnnotations != nil {
		if bw, ok := conf.RuntimeConfig.PodAnnotations[annotationKey]; ok && bw != "" {
			parsed, err := config.ParseBandwidthAnnotation(bw)
			if err != nil {
				fmt.Fprintf(os.Stderr, "natra: %s annotation %q invalid (%v)\n", annotationKey, bw, err)
				return nil
			}
			if parsed != nil && parsed.Rate > 0 {
				// Apply cluster defaults only where the annotation
				// didn't (heuristically: value matches the library's
				// compiled-in default). Explicit per-pod values win.
				if conf.Defaults != nil {
					if conf.Defaults.HHThreshold > 0 && parsed.HeavyHitterThreshold == config.DefaultConfig().HeavyHitterThreshold {
						parsed.HeavyHitterThreshold = conf.Defaults.HHThreshold
					}
					if conf.Defaults.BurstRatio > 0 && parsed.Burst == parsed.Rate*2 {
						parsed.Burst = int64(float64(parsed.Rate) * burstRatio)
					}
				}
				return parsed
			}
		}
	}
	return nil
}

// attachBPF tries each attempt in `strategy` in order until one
// succeeds. For each attempt:
//   - resolve the right side's ifindex (entering pod netns if podside)
//   - load + configure a fresh BPF Program
//   - attach every requested direction
//   - on success: pin maps, announce, return nil
//   - on failure: close program, remove any pins this attempt wrote,
//     fall through to the next attempt
//
// Loading BPF per failed attempt is wasteful but bounded — the
// strategy is at most 4 long and in practice the first attempt
// succeeds on every modern kernel. The simplicity wins.
//
// netns handling: each attempt may need a different netns (pod-side
// vs host-side), so the netns switch and restore live inside the
// per-attempt helper. The outer LockOSThread is what guarantees the
// thread stays put through each switch.
//
// We don't close the chosen attempt's prog on success. For HookTCX,
// each direction's link is pinned to bpffs and the kernel holds the
// program reference via the link until the pin is removed (cmdDel).
// For HookClsact, the kernel holds the program reference via the
// qdisc tree.
func attachBPF(args *skel.CmdArgs, ingressCfg, egressCfg *config.Config, strategy []attachAttempt) error {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	if err := os.MkdirAll(pinDir, 0o755); err != nil {
		return fmt.Errorf("create pin dir %s: %w", pinDir, err)
	}

	attemptErrs := make([]string, 0, len(strategy))
	for _, attempt := range strategy {
		err := tryAttachOne(args, ingressCfg, egressCfg, attempt)
		if err == nil {
			return nil
		}
		attemptErrs = append(attemptErrs, fmt.Sprintf("%s: %v", attempt, err))
		logf("attach attempt %s failed: %v (continuing fallback)", attempt, err)
	}
	return fmt.Errorf("all attach attempts failed:\n  %s", strings.Join(attemptErrs, "\n  "))
}

// tryAttachOne does a single end-to-end attach attempt: resolve
// ifindex, load BPF, configure, attach every requested direction,
// pin maps. On any error it tears down anything it wrote (links,
// pin files, the loaded Program) and returns the error to the
// caller, who decides whether to fall through to the next attempt.
func tryAttachOne(args *skel.CmdArgs, ingressCfg, egressCfg *config.Config, attempt attachAttempt) error {
	ifIndex, restore, err := resolveIfIndex(args.Netns, args.IfName, attempt.side)
	if err != nil {
		return err
	}
	defer restore()

	prog, err := bpf.Load()
	if err != nil {
		return fmt.Errorf("load BPF: %w", err)
	}

	type attachStep struct {
		dir bpf.Direction
		cfg *config.Config
	}
	var steps []attachStep
	if ingressCfg != nil {
		steps = append(steps, attachStep{bpf.DirectionIngress, ingressCfg})
	}
	if egressCfg != nil {
		steps = append(steps, attachStep{bpf.DirectionEgress, egressCfg})
	}

	rollback := func(upto int) {
		for j := 0; j < upto; j++ {
			path := pinPathFor(args.ContainerID, attempt.side, steps[j].dir)
			if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
				fmt.Fprintf(os.Stderr, "natra: rollback remove %s: %v\n", path, err)
			}
		}
	}

	for i, step := range steps {
		if err := prog.Configure(step.dir, bpf.Config{
			RateBps:     uint64(step.cfg.Rate),
			BurstBytes:  uint64(step.cfg.Burst),
			HHThreshold: uint64(step.cfg.HeavyHitterThreshold),
		}); err != nil {
			rollback(i)
			_ = prog.Close()
			return fmt.Errorf("configure BPF (%s): %w", step.dir, err)
		}
		opts := bpf.AttachOptions{
			Direction: step.dir,
			Side:      attempt.side,
			Hook:      attempt.hook,
			IfIndex:   ifIndex,
			PinPath:   pinPathFor(args.ContainerID, attempt.side, step.dir),
		}
		if err := prog.Attach(opts); err != nil {
			rollback(i)
			_ = prog.Close()
			return fmt.Errorf("attach BPF %s: %w", step.dir, err)
		}
	}

	if err := prog.PinMaps(pinDir, args.ContainerID); err != nil {
		fmt.Fprintf(os.Stderr, "natra: map pin failed (%v) — continuing without debug pins\n", err)
	}
	for _, step := range steps {
		announce(ifIndex, attempt.side, step.dir, step.cfg)
	}
	return nil
}

// resolveIfIndex finds the kernel ifindex to attach to based on the
// chosen side, and returns a restore function that cleans up any
// netns state (the caller defers it).
//
// For SidePod: enter the pod netns, look up the named interface,
// stay in the netns (caller's attach happens here).
//
// For SideHost: briefly enter the pod netns to find the host-side
// peer's ifindex via VethPeerIndex, then return to the host netns.
// The restore is a no-op because we exit the netns inline.
func resolveIfIndex(netnsPath, ifName string, side bpf.Side) (int, func(), error) {
	switch side {
	case bpf.SidePod:
		restore, err := enterNetns(netnsPath)
		if err != nil {
			return 0, func() {}, fmt.Errorf("enter pod netns %s: %w", netnsPath, err)
		}
		iface, err := net.InterfaceByName(ifName)
		if err != nil {
			restore()
			return 0, func() {}, fmt.Errorf("interface %q in pod netns: %w", ifName, err)
		}
		return iface.Index, restore, nil
	case bpf.SideHost:
		idx, err := hostsidePeerIfIndex(netnsPath, ifName)
		if err != nil {
			return 0, func() {}, err
		}
		return idx, func() {}, nil
	default:
		return 0, func() {}, fmt.Errorf("unknown bpf.Side %v", side)
	}
}

// maybeWriteHeapProfile dumps the Go heap profile of the natra CNI
// process at end of cmdAdd when NATRA_HEAP_PROFILE_DIR is set in the
// environment. Files land at <dir>/cmdadd-<unixnano>-<containerID>.pprof,
// one per invocation — kubelet typically calls a CNI plugin once per
// pod sandbox, so the file count grows with pod churn.
//
// Aggregated across many ADDs during the perf-vs-vanilla rig, the
// profile shows what natra allocates per-invocation; useful for
// catching allocator regressions in the ADD hot path.
func maybeWriteHeapProfile(containerID string) {
	dir := os.Getenv("NATRA_HEAP_PROFILE_DIR")
	if dir == "" {
		return
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return
	}
	runtime.GC()
	path := filepath.Join(dir, fmt.Sprintf("cmdadd-%d-%s.pprof", time.Now().UnixNano(), containerID))
	f, err := os.Create(path)
	if err != nil {
		return
	}
	defer func() { _ = f.Close() }()
	_ = pprof.WriteHeapProfile(f)
}

// announce writes the "attached" line to stderr (kubelet captures it)
// and to the natra log. Two destinations because each is read by a
// different audience: kubelet on plugin error, the log file on every
// invocation regardless. One announce per direction.
func announce(ifIndex int, side bpf.Side, dir bpf.Direction, cfg *config.Config) {
	fmt.Fprintf(os.Stderr, "natra: attached %s/%s on ifindex=%d rate=%d bps burst=%d bytes\n",
		side, dir, ifIndex, cfg.Rate, cfg.Burst)
	logf("attached: side=%s direction=%s ifindex=%d rate=%d burst=%d hh=%d",
		side, dir, ifIndex, cfg.Rate, cfg.Burst, cfg.HeavyHitterThreshold)
}

func passthrough(args *skel.CmdArgs, conf *NetConf) error {
	// In a chained call kubelet writes the upstream plugin's Result
	// into stdin as `prevResult`. encoding/json fills conf.RawPrevResult
	// (a generic map), but conf.PrevResult only gets populated after
	// version.ParsePrevResult walks it. Skipping this call leaves
	// PrevResult nil and we write back a minimal Result missing the
	// IPs kindnet/ptp produced — containerd then sees no network info
	// for the sandbox and pod creation hangs in ContainerCreating with
	// "failed to find network info for sandbox" errors.
	if err := version.ParsePrevResult(&conf.NetConf); err != nil {
		return fmt.Errorf("parse prevResult: %w", err)
	}

	if conf.PrevResult != nil {
		result, err := current.NewResultFromResult(conf.PrevResult)
		if err != nil {
			return fmt.Errorf("convert previous result: %w", err)
		}
		return types.PrintResult(result, conf.CNIVersion)
	}

	// No PrevResult means natra was invoked as a standalone plugin (not
	// chained behind a main CNI). Construct a minimal valid Result so
	// libcni's validation accepts it. The CNI library requires *some*
	// shape; an empty Result is rejected.
	result := &current.Result{
		CNIVersion: conf.CNIVersion,
		Interfaces: []*current.Interface{{Name: args.IfName, Sandbox: args.Netns}},
	}
	return types.PrintResult(result, conf.CNIVersion)
}
