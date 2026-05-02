package main

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"runtime"

	"github.com/containernetworking/cni/pkg/skel"
	"github.com/containernetworking/cni/pkg/types"
	current "github.com/containernetworking/cni/pkg/types/100"
	"github.com/containernetworking/cni/pkg/version"

	"github.com/terraboops/natra/pkg/bpf"
	"github.com/terraboops/natra/pkg/cni/config"
)

// pinDir holds the bpffs paths for natra's per-pod link pins. Using a
// dedicated subdir keeps natra's pins separate from anything else
// running on the node and makes cleanup easy (cmdDel just unlinks).
const pinDir = "/sys/fs/bpf/natra"

// pinPathFor returns the bpffs pin path for the link attached to the
// given container's interface. Two pods with the same name on different
// nodes get different paths because containerID is unique per kubelet
// pod-sandbox.
func pinPathFor(containerID, ifName string) string {
	return filepath.Join(pinDir, containerID+"-"+ifName+".link")
}

var (
	pluginVersion = "dev"
	commit        = "none"
	date          = "unknown"
)

// NetConf is natra's slice of the CNI stdin payload. Embeds types.NetConf
// for cniVersion / name / type / prevResult.
//
// RuntimeConfig.Bandwidth is populated by kubelet when the conflist
// declares `capabilities.bandwidth: true` (the standard CNI bandwidth
// capability — same one upstream containernetworking/plugins/bandwidth
// uses). Kubelet reads `kubernetes.io/ingress-bandwidth` and
// `kubernetes.io/egress-bandwidth` pod annotations, parses them into
// bytes/sec, and forwards them here. Rates of 0 mean "no limit".
//
// PodAnnotations is preserved for backward compatibility with the
// older annotation-via-runtimeConfig path some setups use; if both
// are present, RuntimeConfig.Bandwidth wins because it's the kubelet-
// canonical channel.
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

// cmdAdd is the heart of the plugin. Order:
//  1. Parse stdin → NetConf.
//  2. Read bandwidth annotation; if absent, pass through unchanged
//     (natra is opt-in via annotation).
//  3. Parse the annotation value into a config.Config.
//  4. Load BPF object, configure rate/burst, attach to pod-side veth
//     ingress inside the pod netns.
//  5. Print PrevResult so the next chained plugin (or kubelet) sees
//     what came in — natra doesn't modify networking, only adds rate
//     limiting on top.
//
// natra's design philosophy is fail-open: if any step from BPF onwards
// fails, we log to stderr and still return success. A pod stuck in
// ContainerCreating because the rate limiter couldn't load is much
// worse than a pod that runs at line rate.
//
// Stderr is captured by the CNI runtime (containerd / kubelet) but
// only on plugin error. We additionally write all log lines to a log
// file at /var/log/natra-cni.log so a successful invocation leaves a
// trace — useful for debugging "is natra even being called?" without
// having to crank container runtime log levels.
func cmdAdd(args *skel.CmdArgs) error {
	logf("ADD containerID=%s netns=%s ifname=%s", args.ContainerID, args.Netns, args.IfName)
	logCaps()
	conf, err := parseConfig(args.StdinData)
	if err != nil {
		return fmt.Errorf("failed to parse config: %w", err)
	}

	cfg := resolveConfig(conf)
	if cfg == nil || cfg.Rate <= 0 {
		logf("no rate limit, passing through")
		return passthrough(args)
	}
	logf("config resolved: rate=%d burst=%d", cfg.Rate, cfg.Burst)

	if err := attachBPF(args, cfg); err != nil {
		// Fail-open: log and continue.
		fmt.Fprintf(os.Stderr, "natra: BPF attach failed (%v) — passing through unrate-limited\n", err)
		logf("attachBPF FAILED: %v", err)
	}

	return passthrough(args)
}

// logf appends a timestamped line to /var/log/natra-cni.log. Best
// effort: if /var/log isn't writable we fall back to stderr only. The
// file is host-mounted by the DaemonSet so a single tail -f from a
// debug shell shows every invocation across all pods on the node.
func logf(format string, args ...any) {
	msg := fmt.Sprintf("[%d %s] ", os.Getpid(), os.Args[0]) + fmt.Sprintf(format, args...) + "\n"
	if f, err := os.OpenFile("/var/log/natra-cni.log", os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o644); err == nil {
		_, _ = f.WriteString(msg)
		_ = f.Close()
	}
}

// logCaps writes our process's effective capabilities to the log.
// Useful for diagnosing EPERM on syscalls like BPF_OBJ_PIN — when
// kubelet invokes natra via the CNI ABI, the runtime may strip caps
// that bpf operations require, even though the parent kubelet has them.
func logCaps() {
	data, err := os.ReadFile("/proc/self/status")
	if err != nil {
		return
	}
	for _, line := range splitLines(data) {
		if len(line) > 4 && line[:4] == "Cap" {
			logf("caps: %s", line)
		}
	}
}

func splitLines(b []byte) []string {
	var out []string
	start := 0
	for i, c := range b {
		if c == '\n' {
			out = append(out, string(b[start:i]))
			start = i + 1
		}
	}
	if start < len(b) {
		out = append(out, string(b[start:]))
	}
	return out
}

// cmdDel removes the bpffs pin we created in cmdAdd. Removing the pin
// drops the kernel's last reference to the tcx link, which detaches
// the BPF program from the (about-to-be-deleted) pod veth. CNI DEL is
// idempotent — a missing pin is not an error.
func cmdDel(args *skel.CmdArgs) error {
	pinPath := pinPathFor(args.ContainerID, args.IfName)
	if err := os.Remove(pinPath); err != nil && !os.IsNotExist(err) {
		// Per CNI spec, DEL should succeed if there's nothing to clean
		// up. We log unexpected errors to stderr so kubelet captures
		// them but still return nil — failing DEL would block the
		// pod's teardown forever.
		fmt.Fprintf(os.Stderr, "natra: failed to remove pin %s: %v\n", pinPath, err)
	}
	return nil
}

// cmdCheck is best-effort: re-entering the pod netns and verifying a
// tcx program is attached would require listing tcx links per ifindex,
// which cilium/ebpf doesn't expose without a link reference. For now
// we treat CHECK as a no-op success — kubelet uses CHECK as a liveness
// hint, and a false-positive isn't worse than the existing fail-open.
func cmdCheck(args *skel.CmdArgs) error {
	return nil
}

func parseConfig(stdin []byte) (*NetConf, error) {
	conf := &NetConf{}
	if err := json.Unmarshal(stdin, conf); err != nil {
		return nil, fmt.Errorf("failed to parse network config: %w", err)
	}
	return conf, nil
}

// resolveConfig reads natra's per-pod rate limit out of the parsed
// CNI stdin. Two channels, in order of preference:
//
//   1. RuntimeConfig.Bandwidth — set by kubelet when the conflist
//      entry declares `capabilities.bandwidth: true`. This is the
//      canonical Kubernetes way and what kubelet derives from the
//      `kubernetes.io/ingress-bandwidth` pod annotation. Rate is in
//      BITS per second (kubelet/upstream-bandwidth convention); we
//      divide by 8 to get the bytes/sec our BPF program expects.
//
//   2. RuntimeConfig.PodAnnotations — legacy / non-standard path some
//      setups use, where the raw annotation string is passed through
//      and we parse it ourselves. Already bytes/sec at that point.
//
// Burst is clamped to 2× rate. Kubelet's default burst is MaxUint32
// (4 GB) when the annotation doesn't specify one — that's effectively
// "no burst limit" and would let a high-rate flow saturate the link
// for ~30s before the token bucket caught up. 2× rate is a one-second
// burst window, the same heuristic the upstream bandwidth plugin uses.
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
		out.TokenBucketRate = out.Rate
		out.TokenBucketBurst = out.Burst
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

// attachBPF enters the pod's network namespace, looks up the interface
// kubelet asked us to operate on (CNI_IFNAME, typically "eth0"),
// loads the BPF object, configures the bucket with the parsed rate /
// burst values, and attaches the program to that interface's ingress
// hook.
//
// Critical: we MUST leave the calling OS thread in the same netns we
// entered with. CNI's skel framework runs a post-flight check after
// the plugin returns; if the plugin's netns equals CNI_NETNS, skel
// emits "code 8" and exits 1, regardless of what we wrote to stdout.
// `restoreNetns` is deferred immediately after the switch so any error
// path (including verifier rejection inside Load) returns us to origin.
//
// The Program is intentionally NOT closed on success — closing severs
// the tcx link, which would unattach the BPF program. We "leak" the
// Program when it's actually owned by the kernel: tcx attachments
// persist after the userspace reference is gone, and the kernel cleans
// them up when the underlying interface is deleted (which happens at
// pod teardown, when the next chained plugin removes the veth pair).
func attachBPF(args *skel.CmdArgs, cfg *config.Config) error {
	// netns operations require locked OS thread — netns.Set switches the
	// CALLING thread, so a goroutine migration mid-flow would leave us
	// in a different namespace than we think.
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
	pinPath := pinPathFor(args.ContainerID, args.IfName)
	if err := prog.AttachIngress(iface.Index, pinPath); err != nil {
		_ = prog.Close()
		return fmt.Errorf("attach BPF to %s ingress: %w", args.IfName, err)
	}

	fmt.Fprintf(os.Stderr, "natra: attached to %s (ifindex=%d) rate=%d bps burst=%d bytes pin=%s\n",
		args.IfName, iface.Index, cfg.Rate, cfg.Burst, pinPath)
	logf("attached: ifname=%s ifindex=%d rate=%d burst=%d pin=%s", args.IfName, iface.Index, cfg.Rate, cfg.Burst, pinPath)
	return nil
}

func passthrough(args *skel.CmdArgs) error {
	conf, err := parseConfig(args.StdinData)
	if err != nil {
		return err
	}

	// In a chained call kubelet writes the upstream plugin's Result
	// into stdin as `prevResult`. encoding/json populates
	// conf.RawPrevResult (a generic map), but the typed PrevResult
	// only gets populated when we ask the version framework to parse
	// it for us. Without this call, natra always treats prevResult as
	// nil and writes back a minimal Result missing the IPs that
	// kindnet/ptp produced — containerd then sees no network info for
	// the sandbox and pod creation hangs in ContainerCreating with
	// "failed to find network info for sandbox" errors.
	if err := version.ParsePrevResult(&conf.NetConf); err != nil {
		return fmt.Errorf("parse prevResult: %w", err)
	}

	if conf.PrevResult != nil {
		result, err := current.NewResultFromResult(conf.PrevResult)
		if err != nil {
			return fmt.Errorf("failed to convert previous result: %w", err)
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
