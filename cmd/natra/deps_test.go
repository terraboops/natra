package main

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

// The CNI binary runs once per pod sandbox, and its peak RSS scales
// with how much of the binary gets paged in. These packages cost
// hundreds of KB each without being needed on the CNI path: regexp
// (compiled at init by cni/pkg/utils, hence internal/cniskel),
// runtime/pprof (natra-tools, or -tags heapprofile).
var forbiddenInPlugin = []string{
	"regexp",
	"runtime/pprof",
	"github.com/containernetworking/cni/pkg/skel",
	"github.com/containernetworking/cni/pkg/utils",
}

func TestPluginDoesNotLinkHeavyPackages(t *testing.T) {
	cmd := exec.Command("go", "list", "-deps", ".")
	cmd.Env = append(os.Environ(), "GOOS=linux", "CGO_ENABLED=0")
	out, err := cmd.Output()
	if err != nil {
		t.Skipf("go list unavailable: %v", err)
	}
	deps := map[string]bool{}
	for _, p := range strings.Fields(string(out)) {
		deps[p] = true
	}
	for _, p := range forbiddenInPlugin {
		if deps[p] {
			t.Errorf("cmd/natra links %s; keep it out of the CNI binary (see deps_test.go)", p)
		}
	}
}
