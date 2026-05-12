//go:build linux

// natra profile — periodically samples BPF program runtime stats
// (cumulative ns + run count, deltas computed downstream) and pinned
// map state for every loaded natra/vanilla program. Output is JSONL
// so a shell pipeline can collect-and-summarize without parsing
// streaming JSON. Optional heap-profile snapshots per tick let
// pprof analyze allocator behavior under sustained load.
//
// Usage (from natra binary):
//
//	natra profile [-interval 5s] [-output PATH] [-heap-dir DIR] [-pin-dir /sys/fs/bpf/natra]
//
// Requires CAP_BPF or root. Linux 5.8+ for runtime stats. Designed
// to run as a sidecar / docker exec on kind worker nodes during the
// perf-vs-vanilla mixed workload — the place where natra is under
// the most packet load and where allocator / hot-path regressions
// would surface.

package main

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"runtime/pprof"
	"strings"
	"syscall"
	"time"

	"github.com/cilium/ebpf"
	"golang.org/x/sys/unix"
)

// progSnapshot is a per-program datum at a single tick. Runtime/RunCount
// are cumulative since BPF stats were enabled; downstream computes
// deltas to get ns/op and ops/sec.
type progSnapshot struct {
	ID        uint32 `json:"id"`
	Name      string `json:"name"`
	Tag       string `json:"tag"`
	RuntimeNS int64  `json:"runtime_ns"`
	RunCount  uint64 `json:"run_count"`
	RecMisses uint64 `json:"recursion_misses"`
	InsnCount uint32 `json:"verified_insns"`
}

// podSnapshot is the per-pod (per-containerID) state. statsMap counters
// give per-direction passed/throttled/hh_hits; cmsZeros vs cmsNonZero
// gives a fill-distribution signal we can compare across ticks to see
// CMS saturation drift over time.
type podSnapshot struct {
	ContainerID   string                  `json:"container_id"`
	Configs       map[string]configRecord `json:"configs"`   // keyed by "ingress"/"egress"
	Stats         map[string]statRecord   `json:"stats"`     // keyed by "ingress"/"egress"
	CMSZeros      int                     `json:"cms_zeros"` // cells with count=0
	CMSNonZero    int                     `json:"cms_nonzero"`
	CMSMaxCount   uint32                  `json:"cms_max_count"`
	CMSTotalCount uint64                  `json:"cms_total_count"`
	BucketTokens  map[string]uint64       `json:"bucket_tokens"` // keyed by direction
}

type configRecord struct {
	RateBps     uint64 `json:"rate_bps"`
	BurstBytes  uint64 `json:"burst_bytes"`
	HHThreshold uint64 `json:"hh_threshold"`
}

type statRecord struct {
	Passed    uint64 `json:"passed"`
	Throttled uint64 `json:"throttled"`
	HHHits    uint64 `json:"hh_hits"`
}

// snapshot is one JSONL record — one per profile tick.
type snapshot struct {
	Time     time.Time      `json:"time"`
	Programs []progSnapshot `json:"programs"`
	Pods     []podSnapshot  `json:"pods"`
}

func profileCmd(args []string) error {
	fs := flag.NewFlagSet("profile", flag.ExitOnError)
	interval := fs.Duration("interval", 5*time.Second, "snapshot interval")
	output := fs.String("output", "", "JSONL output file (default stdout)")
	heapDir := fs.String("heap-dir", "", "if set, write a heap pprof of this process per tick")
	pinPath := fs.String("pin-dir", pinDir, "bpffs directory where natra pins maps")
	if err := fs.Parse(args); err != nil {
		return err
	}

	// Enable BPF_STATS_RUN_TIME for the duration of the profile run.
	// Per-FD lifetime: closing the FD restores the prior global state.
	statsCloser, err := ebpf.EnableStats(unix.BPF_STATS_RUN_TIME)
	if err != nil {
		return fmt.Errorf("EnableStats (kernel 5.8+, CAP_BPF needed): %w", err)
	}
	defer func() { _ = statsCloser.Close() }()

	var w io.Writer = os.Stdout
	if *output != "" {
		f, err := os.Create(*output)
		if err != nil {
			return fmt.Errorf("create output %s: %w", *output, err)
		}
		defer func() { _ = f.Close() }()
		w = f
	}

	if *heapDir != "" {
		if err := os.MkdirAll(*heapDir, 0o755); err != nil {
			return fmt.Errorf("create heap dir %s: %w", *heapDir, err)
		}
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	enc := json.NewEncoder(w)
	tick := time.NewTicker(*interval)
	defer tick.Stop()

	tickNum := 0
	for {
		s := takeSnapshot(*pinPath)
		if err := enc.Encode(s); err != nil {
			fmt.Fprintf(os.Stderr, "natra profile: encode: %v\n", err)
		}
		if *heapDir != "" {
			writeHeapProfile(*heapDir, tickNum)
		}
		tickNum++

		select {
		case <-ctx.Done():
			return nil
		case <-tick.C:
		}
	}
}

func takeSnapshot(pinPath string) snapshot {
	s := snapshot{Time: time.Now()}
	s.Programs = collectProgramStats()
	s.Pods = collectPodSnapshots(pinPath)
	return s
}

// collectProgramStats walks every loaded BPF program by ID, filters
// to ones whose name starts with "natra_" or "vanilla_", and records
// runtime stats. Programs without stats available (kernel < 5.8 or
// stats not enabled) have zeroes — downstream just sees no delta.
func collectProgramStats() []progSnapshot {
	var out []progSnapshot
	var nextID ebpf.ProgramID
	for {
		id, err := ebpf.ProgramGetNextID(nextID)
		if err != nil {
			break
		}
		nextID = id
		prog, err := ebpf.NewProgramFromID(id)
		if err != nil {
			continue
		}
		info, err := prog.Info()
		if err != nil {
			_ = prog.Close()
			continue
		}
		if !strings.HasPrefix(info.Name, "natra_") &&
			!strings.HasPrefix(info.Name, "vanilla_") {
			_ = prog.Close()
			continue
		}

		rec := progSnapshot{
			ID:   uint32(id),
			Name: info.Name,
			Tag:  info.Tag,
		}
		if insn, ok := info.VerifiedInstructions(); ok {
			rec.InsnCount = insn
		}
		if stats, err := prog.Stats(); err == nil && stats != nil {
			rec.RuntimeNS = stats.Runtime.Nanoseconds()
			rec.RunCount = stats.RunCount
			rec.RecMisses = stats.RecursionMisses
		}
		_ = prog.Close()
		out = append(out, rec)
	}
	return out
}

// collectPodSnapshots walks pinPath looking for natra's per-pod map
// pins (`<containerID>-<name>-map`). Groups by containerID and reads
// the config / bucket / stats / cms maps for each.
//
// We intentionally only summarize CMS (zeros / nonzeros / max / total)
// rather than dumping all 8192 cells — the summary is enough to
// observe drift over time, and dumping per-cell would balloon the
// JSONL output during long workloads.
func collectPodSnapshots(pinPath string) []podSnapshot {
	entries, err := os.ReadDir(pinPath)
	if err != nil {
		return nil
	}

	// Group pin filenames by containerID.
	byContainer := map[string]map[string]string{} // containerID → map-suffix → path
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, "-map") {
			continue
		}
		// Filenames are "<containerID>-<suffix>-map". Find the suffix
		// (config/bucket/stats/cms) by stripping the trailing "-map"
		// and reading back from the last hyphen.
		trimmed := strings.TrimSuffix(name, "-map")
		idx := strings.LastIndex(trimmed, "-")
		if idx < 0 {
			continue
		}
		containerID := trimmed[:idx]
		suffix := trimmed[idx+1:]
		switch suffix {
		case "config", "bucket", "stats", "cms":
		default:
			continue
		}
		if byContainer[containerID] == nil {
			byContainer[containerID] = make(map[string]string)
		}
		byContainer[containerID][suffix] = filepath.Join(pinPath, name)
	}

	var snaps []podSnapshot
	for cid, paths := range byContainer {
		snaps = append(snaps, readPodMaps(cid, paths))
	}
	return snaps
}

func readPodMaps(containerID string, paths map[string]string) podSnapshot {
	snap := podSnapshot{
		ContainerID:  containerID,
		Configs:      map[string]configRecord{},
		Stats:        map[string]statRecord{},
		BucketTokens: map[string]uint64{},
	}

	if p, ok := paths["config"]; ok {
		if m, err := ebpf.LoadPinnedMap(p, nil); err == nil {
			for dir := uint32(0); dir < 2; dir++ {
				var raw [24]byte
				if err := m.Lookup(&dir, &raw); err == nil {
					snap.Configs[dirName(dir)] = configRecord{
						RateBps:     binary.LittleEndian.Uint64(raw[0:8]),
						BurstBytes:  binary.LittleEndian.Uint64(raw[8:16]),
						HHThreshold: binary.LittleEndian.Uint64(raw[16:24]),
					}
				}
			}
			_ = m.Close()
		}
	}

	if p, ok := paths["stats"]; ok {
		if m, err := ebpf.LoadPinnedMap(p, nil); err == nil {
			for dir := uint32(0); dir < 2; dir++ {
				snap.Stats[dirName(dir)] = statRecord{
					Passed:    readStatSlot(m, dir, 0),
					Throttled: readStatSlot(m, dir, 1),
					HHHits:    readStatSlot(m, dir, 2),
				}
			}
			_ = m.Close()
		}
	}

	if p, ok := paths["bucket"]; ok {
		if m, err := ebpf.LoadPinnedMap(p, nil); err == nil {
			for dir := uint32(0); dir < 2; dir++ {
				// token_bucket: u32 lock + u32 pad + u64 tokens + u64 last_update_ns.
				var raw [24]byte
				if err := m.Lookup(&dir, &raw); err == nil {
					snap.BucketTokens[dirName(dir)] = binary.LittleEndian.Uint64(raw[8:16])
				}
			}
			_ = m.Close()
		}
	}

	if p, ok := paths["cms"]; ok {
		if m, err := ebpf.LoadPinnedMap(p, nil); err == nil {
			for i := uint32(0); ; i++ {
				var v uint32
				err := m.Lookup(&i, &v)
				if err != nil {
					if errors.Is(err, ebpf.ErrKeyNotExist) {
						break
					}
					break
				}
				if v == 0 {
					snap.CMSZeros++
				} else {
					snap.CMSNonZero++
					if v > snap.CMSMaxCount {
						snap.CMSMaxCount = v
					}
					snap.CMSTotalCount += uint64(v)
				}
			}
			_ = m.Close()
		}
	}

	return snap
}

func readStatSlot(m *ebpf.Map, dir, slot uint32) uint64 {
	key := dir*3 + slot
	var values []uint64
	if err := m.Lookup(&key, &values); err != nil {
		return 0
	}
	var sum uint64
	for _, v := range values {
		sum += v
	}
	return sum
}

func dirName(d uint32) string {
	if d == 1 {
		return "egress"
	}
	return "ingress"
}

// writeHeapProfile writes the current Go heap profile to heap-NNNNN.pprof
// in dir. Triggered runtime.GC first so the dump reflects live objects
// after a sweep, not in-flight collector state.
func writeHeapProfile(dir string, tick int) {
	runtime.GC()
	path := filepath.Join(dir, fmt.Sprintf("heap-%05d.pprof", tick))
	f, err := os.Create(path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "natra profile: heap profile %s: %v\n", path, err)
		return
	}
	defer func() { _ = f.Close() }()
	if err := pprof.WriteHeapProfile(f); err != nil {
		fmt.Fprintf(os.Stderr, "natra profile: WriteHeapProfile: %v\n", err)
	}
}
