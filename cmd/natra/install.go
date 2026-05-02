package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
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

	pluginsRaw, ok := conflist["plugins"]
	if !ok {
		return fmt.Errorf("source conflist has no 'plugins' field")
	}
	plugins, ok := pluginsRaw.([]any)
	if !ok {
		return fmt.Errorf("source conflist 'plugins' is not an array")
	}

	// Idempotency: if the sibling already exists with natra appended,
	// no-op. Compare against the source to detect upstream-rewrite
	// drift; if they differ we overwrite with the new chained version.
	if existing, err := os.ReadFile(dst); err == nil {
		var have map[string]any
		if json.Unmarshal(existing, &have) == nil {
			if havePlugins, ok := have["plugins"].([]any); ok &&
				len(havePlugins) == len(plugins)+1 {
				last := havePlugins[len(havePlugins)-1]
				if lm, ok := last.(map[string]any); ok && lm["type"] == "natra" {
					// Ensure the upstream plugin set hasn't drifted
					// (e.g. kindnet added a new plugin) by length-
					// matching on the prefix.
					match := true
					for i := 0; i < len(plugins); i++ {
						a, _ := json.Marshal(plugins[i])
						b, _ := json.Marshal(havePlugins[i])
						if string(a) != string(b) {
							match = false
							break
						}
					}
					if match {
						fmt.Fprintf(os.Stderr, "natra: %s up-to-date, skipping\n", dst)
						return nil
					}
				}
			}
		}
	}

	chained := make(map[string]any, len(conflist))
	for k, v := range conflist {
		chained[k] = v
	}
	// Keep the original network name. Containerd CRI loads only one
	// conflist (`maxConfNum: 1`) and identifies it by the "name" field;
	// changing the name to anything else has been observed to leave
	// containerd in a state where `crictl info` reports the conflist
	// loaded but pod sandboxes still fail with "failed to find network
	// info for sandbox". Match what kindnet (or whatever upstream)
	// chose so containerd's existing wiring keeps working.
	//
	// `capabilities.bandwidth: true` is what tells kubelet to populate
	// `runtimeConfig.bandwidth.{ingressRate, ingressBurst}` in the
	// stdin we receive from the kubernetes.io/ingress-bandwidth pod
	// annotation. Without this, the annotation is invisible to the
	// plugin — kubelet doesn't forward arbitrary annotations as
	// runtime config; capabilities is the explicit opt-in protocol.
	chained["plugins"] = append(append([]any{}, plugins...), map[string]any{
		"type":         "natra",
		"capabilities": map[string]any{"bandwidth": true},
	})

	out, err := json.MarshalIndent(chained, "", "  ")
	if err != nil {
		return err
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
