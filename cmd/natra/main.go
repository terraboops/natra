package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/containernetworking/cni/pkg/skel"
	"github.com/containernetworking/cni/pkg/types"
	current "github.com/containernetworking/cni/pkg/types/100"
	"github.com/containernetworking/cni/pkg/version"

	"github.com/terraboops/natra/pkg/bpf"
	"github.com/terraboops/natra/pkg/cni/config"
)

// pinDir holds the bpffs paths for natra's per-pod map pins. A
// dedicated subdir keeps natra's pins separate from other tooling on
// the node and makes cleanup straightforward (cmdDel removes the
// per-container files).
const pinDir = "/sys/fs/bpf/natra"

var (
	pluginVersion = "dev"
	commit        = "none"
	date          = "unknown"
)

// NetConf is natra's slice of the CNI stdin payload. Embeds
// types.NetConf for cniVersion / name / type / prevResult.
//
// RuntimeConfig.Bandwidth is what kubelet populates when the conflist
// declares `capabilities.bandwidth: true` — kubelet reads the
// kubernetes.io/{ingress,egress}-bandwidth annotations, parses them
// into bytes/sec, and forwards them here. Rate=0 means no limit.
//
// PodAnnotations is the older path where the raw annotation string is
// passed through under runtimeConfig. Some setups still use it; if
// both channels are present we prefer Bandwidth because it's what
// kubelet actually populates today.
type NetConf struct {
	types.NetConf
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

func main() {
	// natra is normally invoked by kubelet via the CNI ABI (no CLI args,
	// uses stdin + env vars). The exception is `install-cni-chain`,
	// which the DaemonSet's install container calls to patch existing
	// CNI conflists to include natra. Keeping it in-binary avoids a
	// second image / extra dependencies (jq, awk, etc.) that have
	// historically been fragile.
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
//  2. Resolve bandwidth from runtimeConfig; pass through if no rate.
//  3. Load BPF object, configure rate/burst, attach to the pod-side
//     veth ingress inside the pod netns.
//  5. Print PrevResult so the next chained plugin (or kubelet) sees
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
	logf("ADD containerID=%s netns=%s ifname=%s", args.ContainerID, args.Netns, args.IfName)
	logCaps()
	conf, err := parseConfig(args.StdinData)
	if err != nil {
		return fmt.Errorf("parse config: %w", err)
	}

	cfg := resolveConfig(conf)
	if cfg == nil || cfg.Rate <= 0 {
		logf("no rate limit, passing through")
		return passthrough(args, conf)
	}
	logf("config resolved: rate=%d burst=%d", cfg.Rate, cfg.Burst)

	if err := attachBPF(args, cfg); err != nil {
		// Fail-open: log and continue.
		fmt.Fprintf(os.Stderr, "natra: BPF attach failed (%v) — passing through unrate-limited\n", err)
		logf("attachBPF FAILED: %v", err)
	}

	return passthrough(args, conf)
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

// cmdDel cleans up the bpffs map pins for this container. The clsact
// filter on the veth is *not* unpinned here — the kernel auto-detaches
// it when the chained CNI plugin's DEL deletes the underlying veth.
// What we do clean up is the map pins (config / bucket / stats / cms)
// that PinMaps wrote, which would otherwise leak per-pod files under
// /sys/fs/bpf/natra/ for the lifetime of the node.
//
// CNI DEL is idempotent: missing files are not errors.
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

// resolveConfig pulls the per-pod rate limit out of the parsed stdin.
// Two channels, in order of preference:
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
// Returns nil if neither channel produces a usable rate.
func resolveConfig(conf *NetConf) *config.Config {
	if conf.RuntimeConfig.Bandwidth != nil && conf.RuntimeConfig.Bandwidth.IngressRate > 0 {
		out := config.DefaultConfig()
		out.Rate = conf.RuntimeConfig.Bandwidth.IngressRate / 8 // bits → bytes
		burst := conf.RuntimeConfig.Bandwidth.IngressBurst / 8
		if burst <= 0 || burst > out.Rate*2 {
			burst = out.Rate * 2
		}
		out.Burst = burst
		return out
	}
	if conf.RuntimeConfig.PodAnnotations != nil {
		if bw, ok := conf.RuntimeConfig.PodAnnotations["kubernetes.io/ingress-bandwidth"]; ok && bw != "" {
			parsed, err := config.ParseBandwidthAnnotation(bw)
			if err != nil {
				fmt.Fprintf(os.Stderr, "natra: bandwidth annotation %q invalid (%v)\n", bw, err)
				return nil
			}
			return parsed
		}
	}
	return nil
}

// attachBPF enters the pod's network namespace, finds the interface
// kubelet asked us to operate on (CNI_IFNAME, typically "eth0"),
// loads the BPF object, configures the bucket, and attaches the
// program to the interface's ingress hook.
//
// We have to leave the calling OS thread in the same netns we entered
// with. CNI's skel framework checks after the plugin returns and exits
// "code 8" if the plugin's netns matches CNI_NETNS, regardless of what
// stdout said. The restore is deferred immediately after the switch
// so any error path returns us to origin.
//
// We don't close prog on success — closing would drop the userspace
// reference but the kernel keeps the clsact filter referenced by the
// qdisc tree, so packets keep flowing through the rate limiter. Cleanup
// happens at pod teardown when the next chained plugin's DEL deletes
// the underlying veth.
func attachBPF(args *skel.CmdArgs, cfg *config.Config) error {
	// netns.Set switches the calling thread, so a goroutine migration
	// mid-flow would leave us in a different namespace than we think.
	// Lock the thread for the duration of the netns dance.
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	restore, err := enterNetns(args.Netns)
	if err != nil {
		return fmt.Errorf("enter pod netns %s: %w", args.Netns, err)
	}
	defer restore()

	iface, err := net.InterfaceByName(args.IfName)
	if err != nil {
		return fmt.Errorf("interface %q in pod netns: %w", args.IfName, err)
	}

	prog, err := bpf.Load()
	if err != nil {
		return fmt.Errorf("load BPF: %w", err)
	}
	// Note: we don't defer prog.Close() — see attachBPF docstring.

	if err := prog.Configure(bpf.Config{
		RateBps:     uint64(cfg.Rate),
		BurstBytes:  uint64(cfg.Burst),
		HHThreshold: uint64(cfg.HeavyHitterThreshold),
	}); err != nil {
		_ = prog.Close()
		return fmt.Errorf("configure BPF: %w", err)
	}

	if err := os.MkdirAll(pinDir, 0o755); err != nil {
		_ = prog.Close()
		return fmt.Errorf("create pin dir %s: %w", pinDir, err)
	}
	if err := prog.AttachIngress(iface.Index); err != nil {
		_ = prog.Close()
		return fmt.Errorf("attach BPF to %s ingress: %w", args.IfName, err)
	}
	// Pin the maps so `natra dump-stats <containerID>` can read live
	// stats and CMS counters from a separate process. Best-effort —
	// pinning failure (EPERM in some environments) doesn't tear down
	// the attachment.
	if err := prog.PinMaps(pinDir, args.ContainerID); err != nil {
		fmt.Fprintf(os.Stderr, "natra: map pin failed (%v) — continuing without debug pins\n", err)
	}

	announce(args, iface.Index, cfg)
	return nil
}

// announce writes the "attached" line to stderr (kubelet captures it)
// and to the natra log. Two destinations because each is read by a
// different audience: kubelet on plugin error, the log file on every
// invocation regardless.
func announce(args *skel.CmdArgs, ifIndex int, cfg *config.Config) {
	fmt.Fprintf(os.Stderr, "natra: attached to %s (ifindex=%d) rate=%d bps burst=%d bytes\n",
		args.IfName, ifIndex, cfg.Rate, cfg.Burst)
	logf("attached: ifname=%s ifindex=%d rate=%d burst=%d hh=%d",
		args.IfName, ifIndex, cfg.Rate, cfg.Burst, cfg.HeavyHitterThreshold)
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
