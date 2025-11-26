package main

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/containernetworking/cni/pkg/skel"
	"github.com/containernetworking/cni/pkg/types"
	current "github.com/containernetworking/cni/pkg/types/100"
	"github.com/containernetworking/cni/pkg/version"
)

var (
	pluginVersion = "dev"
	commit        = "none"
	date          = "unknown"
)

// NetConf represents the CNI network configuration
type NetConf struct {
	types.NetConf
	// RuntimeConfig contains runtime configuration from kubelet
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

// cmdAdd is called when a Pod is created
func cmdAdd(args *skel.CmdArgs) error {
	conf, err := parseConfig(args.StdinData)
	if err != nil {
		return fmt.Errorf("failed to parse config: %w", err)
	}

	// Get bandwidth annotation
	bandwidth := getBandwidthAnnotation(conf)
	if bandwidth == "" {
		// No bandwidth annotation, pass through
		return passthrough(args)
	}

	// TODO Phase 1: Parse bandwidth config
	// TODO Phase 1: Load eBPF program
	// TODO Phase 1: Attach to veth interface
	// TODO Phase 1: Configure CMS and Token Bucket maps

	// For now, log and pass through (fail-open)
	fmt.Fprintf(os.Stderr, "natra: bandwidth annotation found: %s (eBPF not yet implemented)\n", bandwidth)

	return passthrough(args)
}

// cmdDel is called when a Pod is deleted
func cmdDel(args *skel.CmdArgs) error {
	// TODO Phase 1: Detach eBPF program from veth
	// TODO Phase 1: Clean up BPF maps

	// For now, always succeed (eBPF cleanup happens automatically when veth is deleted)
	return nil
}

// cmdCheck is called to verify Pod network is still correctly configured
func cmdCheck(args *skel.CmdArgs) error {
	// TODO Phase 1: Verify eBPF program is still attached
	// TODO Phase 1: Verify BPF maps are accessible

	// For now, always succeed
	return nil
}

// parseConfig parses the CNI network configuration from stdin
func parseConfig(stdin []byte) (*NetConf, error) {
	conf := &NetConf{}
	if err := json.Unmarshal(stdin, conf); err != nil {
		return nil, fmt.Errorf("failed to parse network config: %w", err)
	}
	return conf, nil
}

// getBandwidthAnnotation retrieves the bandwidth annotation from Pod annotations
func getBandwidthAnnotation(conf *NetConf) string {
	if conf.RuntimeConfig.PodAnnotations == nil {
		return ""
	}

	// Check for standard Kubernetes bandwidth annotation
	if bw, ok := conf.RuntimeConfig.PodAnnotations["kubernetes.io/ingress-bandwidth"]; ok {
		return bw
	}

	return ""
}

// passthrough returns the previous result unchanged (for chained CNI plugins)
func passthrough(args *skel.CmdArgs) error {
	conf, err := parseConfig(args.StdinData)
	if err != nil {
		return err
	}

	// If there's a previous result, return it
	if conf.PrevResult != nil {
		result, err := current.NewResultFromResult(conf.PrevResult)
		if err != nil {
			return fmt.Errorf("failed to convert previous result: %w", err)
		}
		return types.PrintResult(result, conf.CNIVersion)
	}

	// No previous result - return empty success
	result := &current.Result{
		CNIVersion: conf.CNIVersion,
	}
	return types.PrintResult(result, conf.CNIVersion)
}
