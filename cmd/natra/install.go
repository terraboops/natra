package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// installCNIChain finds every *.conflist in the given directory, builds
// a chained version with {"type":"natra"} appended to the plugins
// array, and writes it as a sibling file with a "00-" prefix so it
// sorts ahead alphabetically. The original conflist is left untouched.
//
// Containerd's CNI loader picks the *first* conflist alphabetically.
// By writing 00-<original>.conflist, we ensure containerd uses our
// chained version while the upstream installer (kindnet, calico, etc.)
// can keep rewriting the original on its own schedule without fighting
// us. Earlier attempts to in-place-patch the original triggered a
// containerd v2.x reload race surfacing as "failed to find network
// info for sandbox".
func installCNIChain(args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("usage: natra install-cni-chain <conflist-dir>")
	}
	dir := args[0]

	entries, err := os.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("read conflist dir %s: %w", dir, err)
	}

	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || filepath.Ext(name) != ".conflist" {
			continue
		}
		// Don't recurse into our own sibling.
		if strings.HasPrefix(name, "00-natra-") {
			continue
		}
		src := filepath.Join(dir, name)
		dst := filepath.Join(dir, "00-natra-"+name)
		if err := writeChainedSibling(src, dst); err != nil {
			return fmt.Errorf("chain %s -> %s: %w", src, dst, err)
		}
	}
	return nil
}

func writeChainedSibling(src, dst string) error {
	data, err := os.ReadFile(src)
	if err != nil {
		return err
	}

	var conflist map[string]any
	if err := json.Unmarshal(data, &conflist); err != nil {
		return fmt.Errorf("parse JSON: %w", err)
	}

	plugins, ok := conflist["plugins"].([]any)
	if !ok {
		return fmt.Errorf("source conflist has no plugins array")
	}

	// Keep the original network name and any other top-level fields,
	// just append natra to the plugins list. Containerd CRI loads at
	// most one conflist (maxConfNum:1) and keys it by "name" — renaming
	// has left containerd in a half-loaded state where `crictl info`
	// reports the conflist but pods fail with "failed to find network
	// info for sandbox".
	//
	// `capabilities.bandwidth: true` tells kubelet to populate
	// runtimeConfig.bandwidth from the kubernetes.io/ingress-bandwidth
	// pod annotation. Without it the annotation is invisible — kubelet
	// only forwards what the conflist explicitly opts into.
	chained := make(map[string]any, len(conflist))
	for k, v := range conflist {
		chained[k] = v
	}
	natraEntry := map[string]any{
		"type":         "natra",
		"capabilities": map[string]any{"bandwidth": true},
	}
	// NATRA_ATTACH_MODE on the install init container surfaces here.
	// Empty (default) → omit; the binary auto-detects (tcx then
	// clsact, hostside then podside). "auto" is accepted explicitly.
	// Any of the four exact-mode strings also passes through and
	// disables auto-detection. Reject anything else loudly so a typo
	// in the DaemonSet env doesn't silently fall through to a
	// default that was wrong for this node.
	const (
		attachAutoVal      = "auto"
		attachExplicitVals = "tcx-hostside, tcx-podside, clsact-hostside, clsact-podside"
	)
	if mode := os.Getenv("NATRA_ATTACH_MODE"); mode != "" {
		switch mode {
		case attachAutoVal, "tcx-hostside", "tcx-podside", "clsact-hostside", "clsact-podside":
			natraEntry["attachMode"] = mode
		default:
			return fmt.Errorf(
				"NATRA_ATTACH_MODE=%q is not recognized (want one of: %s, %s)",
				mode, attachAutoVal, attachExplicitVals,
			)
		}
	}

	// Cluster-tier knobs. Both go into the natra entry's "defaults"
	// block; per-pod annotations override on a field-by-field basis.
	defaults := map[string]any{}
	if t := os.Getenv("NATRA_DEFAULT_HH_THRESHOLD"); t != "" {
		v, err := strconv.ParseInt(t, 10, 64)
		if err != nil || v <= 0 {
			return fmt.Errorf("NATRA_DEFAULT_HH_THRESHOLD=%q must be a positive integer", t)
		}
		defaults["hhThreshold"] = v
	}
	if r := os.Getenv("NATRA_DEFAULT_BURST_RATIO"); r != "" {
		v, err := strconv.ParseFloat(r, 64)
		if err != nil || v <= 0 {
			return fmt.Errorf("NATRA_DEFAULT_BURST_RATIO=%q must be a positive number", r)
		}
		defaults["burstRatio"] = v
	}
	// NATRA_EDT_PACING={auto,on,off}. Tri-state:
	//   auto (default if env unset): probe fq install at CNI ADD;
	//     use EDT pacing when probe succeeds, fall back to drop
	//     semantics when it fails.
	//   on: require EDT; refuse to attach if fq install fails.
	//   off: never install fq, never set the EDT bit.
	// The string form is passed through to the conflist so the CNI
	// binary makes the final attach-time call per pod.
	const edtOffVal = "off"
	switch e := os.Getenv("NATRA_EDT_PACING"); e {
	case "", attachAutoVal:
		// auto is the implicit default — omit from conflist
	case "1", "true", "yes", "on":
		defaults["edtPacing"] = "on"
	case "0", "false", "no", edtOffVal:
		defaults["edtPacing"] = edtOffVal
	default:
		return fmt.Errorf("NATRA_EDT_PACING=%q is not recognized (want one of: %s, on, %s)", e, attachAutoVal, edtOffVal)
	}
	if len(defaults) > 0 {
		natraEntry["defaults"] = defaults
	}
	chained["plugins"] = append(append([]any{}, plugins...), natraEntry)

	out, err := json.MarshalIndent(chained, "", "  ")
	if err != nil {
		return err
	}

	// Idempotency: if the existing sibling has identical bytes to what
	// we'd write, skip. Containerd's inotify watch fires on every
	// rewrite even of identical content, and rapid rewrites have raced
	// with sandbox creation in the past.
	if existing, err := os.ReadFile(dst); err == nil && bytes.Equal(existing, out) {
		fmt.Fprintf(os.Stderr, "natra: %s up-to-date, skipping\n", dst)
		return nil
	}

	tmp := dst + ".natra-tmp"
	if err := os.WriteFile(tmp, out, 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, dst); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	fmt.Fprintf(os.Stderr, "natra: wrote chained %s (sourced from %s)\n", dst, src)
	return nil
}
