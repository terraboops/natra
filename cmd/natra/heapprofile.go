//go:build heapprofile

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"runtime/pprof"
	"time"
)

// maybeWriteHeapProfile dumps the Go heap profile of the natra CNI
// process at end of cmdAdd when NATRA_HEAP_PROFILE_DIR is set in the
// environment. Only in builds with `-tags heapprofile`: linking
// runtime/pprof into every CNI invocation costs ~100 KB of peak RSS. Files land at <dir>/cmdadd-<unixnano>-<containerID>.pprof,
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
