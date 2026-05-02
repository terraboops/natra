package main

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"runtime"

	"github.com/containernetworking/cni/pkg/skel"
	"github.com/containernetworking/cni/pkg/types"
	current "github.com/containernetworking/cni/pkg/types/100"
	"github.com/containernetworking/cni/pkg/version"

	"github.com/terraboops/natra/pkg/bpf"
	"github.com/terraboops/natra/pkg/cni/config"
)

var (
	pluginVersion = "dev"
	commit        = "none"
	date          = "unknown"
)

// NetConf is natra's slice of the CNI stdin payload. Embeds types.NetConf
// for cniVersion / name / type / prevResult, plus our runtime config slot
// where kubelet places pod annotations.
type NetConf struct {
	types.NetConf
	RuntimeConfig struct {
		PodAnnotations map[string]string `json:"podAnnotations,omitempty"`
	} `json:"runtimeConfig,omitempty"`
}

func main() {
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
func cmdAdd(args *skel.CmdArgs) error {
	conf, err := parseConfig(args.StdinData)
	if err != nil {
		return fmt.Errorf("failed to parse config: %w", err)
	}

	bandwidth := getBandwidthAnnotation(conf)
	if bandwidth == "" {
		return passthrough(args)
	}

	cfg, err := config.ParseBandwidthAnnotation(bandwidth)
	if err != nil {
		fmt.Fprintf(os.Stderr, "natra: bandwidth annotation %q invalid (%v) — passing through unrate-limited\n", bandwidth, err)
		return passthrough(args)
	}
	if cfg.Rate <= 0 {
		// Rate of zero means "no limit" — same as no annotation.
		return passthrough(args)
	}

	if err := attachBPF(args, cfg); err != nil {
		// Fail-open: log and continue.
		fmt.Fprintf(os.Stderr, "natra: BPF attach failed (%v) — passing through unrate-limited\n", err)
	}

	return passthrough(args)
}

// cmdDel relies on the kernel auto-cleaning tcx attachments when the
// underlying interface (the pod-side veth) is deleted by the next
// chained CNI plugin. natra holds no state outside the kernel.
func cmdDel(args *skel.CmdArgs) error {
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

func getBandwidthAnnotation(conf *NetConf) string {
	if conf.RuntimeConfig.PodAnnotations == nil {
		return ""
	}
	if bw, ok := conf.RuntimeConfig.PodAnnotations["kubernetes.io/ingress-bandwidth"]; ok {
		return bw
	}
	return ""
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

	if err := prog.AttachIngress(iface.Index); err != nil {
		_ = prog.Close()
		return fmt.Errorf("attach BPF to %s ingress: %w", args.IfName, err)
	}

	fmt.Fprintf(os.Stderr, "natra: attached to %s (ifindex=%d) rate=%d bps burst=%d bytes\n",
		args.IfName, iface.Index, cfg.Rate, cfg.Burst)
	return nil
}

func passthrough(args *skel.CmdArgs) error {
	conf, err := parseConfig(args.StdinData)
	if err != nil {
		return err
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
