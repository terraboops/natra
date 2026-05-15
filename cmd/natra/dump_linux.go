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

	"github.com/terraboops/natra/pkg/bpf"
)

// dumpStats reads the pinned natra maps for the given container and
// prints stats / config / a CMS histogram summary, per direction. The
// pinned files use the same containerID-rooted naming convention as
// cmd/natra/main.go writes; link pins ("-<side>-<direction>-link")
// are listed but their contents aren't dumped — the kernel exposes
// link state via bpftool, not via map reads.
//
// Useful for post-mortem inspection — tells you whether traffic
// actually flowed through the BPF program in either direction and how
// the CMS classified it.
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
		path := filepath.Join(pinDir, containerID+"-"+name+"-map")
		return ebpf.LoadPinnedMap(path, nil)
	}

	cfgMap, err := open("config")
	if err != nil {
		return fmt.Errorf("open config map: %w", err)
	}
	defer func() { _ = cfgMap.Close() }()
	statsMap, err := open("stats")
	if err != nil {
		return fmt.Errorf("open stats map: %w", err)
	}
	defer func() { _ = statsMap.Close() }()

	for _, dir := range []bpf.Direction{bpf.DirectionIngress, bpf.DirectionEgress} {
		key := uint32(dir)
		var rawCfg [24]byte
		if err := cfgMap.Lookup(&key, &rawCfg); err != nil {
			return fmt.Errorf("lookup config[%s]: %w", dir, err)
		}
		rate := binary.LittleEndian.Uint64(rawCfg[0:8])
		burst := binary.LittleEndian.Uint64(rawCfg[8:16])
		hh := binary.LittleEndian.Uint64(rawCfg[16:24])
		if rate == 0 {
			// Zero-rate slot means this direction wasn't attached for
			// this container. Skip the section so the output isn't
			// cluttered with empties.
			continue
		}
		fmt.Printf("[%s] config: rate=%d burst=%d hh_threshold=%d\n", dir, rate, burst, hh)

		counters := map[string]uint64{}
		for _, slot := range []struct {
			name string
			idx  uint32
		}{
			{"passed", bpf.StatPassed},
			{"throttled", bpf.StatThrottled},
			{"hh_hits", bpf.StatHHHits},
			{"ecn_marked", bpf.StatECNMarked},
			{"edt_delayed", bpf.StatEDTDelayed},
			{"dropped", bpf.StatDropped},
		} {
			statKey := bpf.StatKey(dir, slot.idx)
			var values []uint64
			if err := statsMap.Lookup(&statKey, &values); err != nil {
				return fmt.Errorf("lookup stats[%s/%s]: %w", dir, slot.name, err)
			}
			var sum uint64
			for _, v := range values {
				sum += v
			}
			counters[slot.name] = sum
			fmt.Printf("[%s] stats: %s=%d (per-cpu sum)\n", dir, slot.name, sum)
		}

		// Disposition mix: percentage of above-rate events by outcome.
		// Under sustained over-rate egress with EDT pacing, expect
		// mostly edt_delayed with occasional ecn_marked when the 50 ms
		// EDT-delay bound trips (MAX_EDT_DELAY_NS in bpf/natra.bpf.c).
		// Ingress (and egress with EDT off) will show ecn_marked +
		// dropped. The three counters sum to throttled by construction.
		if t := counters["throttled"]; t > 0 {
			pct := func(c uint64) float64 { return 100 * float64(c) / float64(t) }
			fmt.Printf("[%s] disposition mix: EDT=%.0f%% ECN=%.0f%% drop=%.0f%% (of %d throttled)\n",
				dir, pct(counters["edt_delayed"]), pct(counters["ecn_marked"]),
				pct(counters["dropped"]), t)
		}
	}

	// CMS map is optional (absent for the placeholder program). Print
	// a single histogram summary across both halves; per-direction
	// breakdown isn't worth the verbosity here.
	//
	// Each cell is `struct cms_cell { u64 bytes; u32 last_decay_idx; }`
	// — see bpf/natra.bpf.c. We only summarize the byte counter;
	// last_decay_idx is debugging detail and not interesting in aggregate.
	cmsMap, err := open("cms")
	if err == nil {
		defer func() { _ = cmsMap.Close() }()
		var (
			zeros, nonZero int
			max, total     uint64
		)
		for i := uint32(0); ; i++ {
			var cell struct {
				Bytes        uint64
				LastDecayIdx uint32
				_            uint32
			}
			if err := cmsMap.Lookup(&i, &cell); err != nil {
				if strings.Contains(err.Error(), "no such key") || strings.Contains(err.Error(), "no such file") {
					break
				}
				return fmt.Errorf("cms[%d]: %w", i, err)
			}
			if cell.Bytes == 0 {
				zeros++
			} else {
				nonZero++
				if cell.Bytes > max {
					max = cell.Bytes
				}
				total += cell.Bytes
			}
		}
		fmt.Printf("cms: zeros=%d nonzero=%d max_bytes=%d total_bytes=%d\n", zeros, nonZero, max, total)
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
