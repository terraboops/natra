//go:build linux

package main

import (
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/cilium/ebpf"
	"github.com/cilium/ebpf/rlimit"
)

// dumpStats reads the pinned natra maps for the given container and
// prints stats / config / a CMS histogram summary. Useful for
// post-mortem inspection — tells you whether traffic actually flowed
// through the BPF program and how the CMS classified it.
//
// Invoked from the natra binary via:
//
//	natra dump-stats <containerID>
func dumpStats(args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("usage: natra dump-stats <containerID>")
	}
	containerID := args[0]

	if err := rlimit.RemoveMemlock(); err != nil {
		return err
	}

	open := func(name string) (*ebpf.Map, error) {
		path := filepath.Join(pinDir, containerID+"-"+name+".map")
		return ebpf.LoadPinnedMap(path, nil)
	}

	cfgMap, err := open("config")
	if err != nil {
		return fmt.Errorf("open config map: %w", err)
	}
	defer cfgMap.Close()
	statsMap, err := open("stats")
	if err != nil {
		return fmt.Errorf("open stats map: %w", err)
	}
	defer statsMap.Close()

	zero := uint32(0)
	var rawCfg [24]byte
	if err := cfgMap.Lookup(&zero, &rawCfg); err != nil {
		return fmt.Errorf("lookup config: %w", err)
	}
	rate := binary.LittleEndian.Uint64(rawCfg[0:8])
	burst := binary.LittleEndian.Uint64(rawCfg[8:16])
	hh := binary.LittleEndian.Uint64(rawCfg[16:24])
	fmt.Printf("config: rate=%d burst=%d hh_threshold=%d\n", rate, burst, hh)

	for _, slot := range []struct {
		name string
		idx  uint32
	}{
		{"passed", 0},
		{"throttled", 1},
		{"hh_hits", 2},
	} {
		var values []uint64
		if err := statsMap.Lookup(&slot.idx, &values); err != nil {
			return fmt.Errorf("lookup stats[%s]: %w", slot.name, err)
		}
		var sum uint64
		for _, v := range values {
			sum += v
		}
		fmt.Printf("stats: %s=%d (per-cpu sum)\n", slot.name, sum)
	}

	// CMS map is optional (absent for the placeholder program).
	cmsMap, err := open("cms")
	if err == nil {
		defer cmsMap.Close()
		var (
			zeros, nonZero  int
			max, total      uint32
		)
		var max32 uint32
		_ = max32
		for i := uint32(0); ; i++ {
			var v uint32
			if err := cmsMap.Lookup(&i, &v); err != nil {
				if strings.Contains(err.Error(), "no such key") || strings.Contains(err.Error(), "no such file") {
					break
				}
				return fmt.Errorf("cms[%d]: %w", i, err)
			}
			if v == 0 {
				zeros++
			} else {
				nonZero++
				if v > max {
					max = v
				}
				total += v
			}
		}
		fmt.Printf("cms: zeros=%d nonzero=%d max_count=%d total_increments=%d\n", zeros, nonZero, max, total)
	}

	// Self-check: pin path stats can drift from /sys/fs/bpf/natra layout.
	if entries, err := os.ReadDir(pinDir); err == nil {
		var pinned []string
		for _, e := range entries {
			if strings.HasPrefix(e.Name(), containerID) {
				pinned = append(pinned, e.Name())
			}
		}
		fmt.Printf("pins: %v\n", pinned)
	}
	return nil
}
