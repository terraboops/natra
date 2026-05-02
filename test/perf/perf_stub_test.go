//go:build !linux

package perf_test

import "testing"

func TestPerfLayerSkipped(t *testing.T) {
	t.Skip("Layer 5 perf tests require Linux + lvh; see TODO_LINUX.md for the lima/orbstack escape hatch")
}
