package perfrig

import (
	"context"
	"fmt"
	"sync"
)

// FakeSubstrate is the unit-test stand-in. It records every method
// call and returns canned outputs for NodeShell so executor logic
// (phase loop, parsing of bpftool / meminfo / iperf output) is
// L1-testable with zero clusters.
//
// Test-only — lives next to the real impls so a small interface
// drift fails the build at this file rather than at the real
// substrates.
type FakeSubstrate struct {
	mu         sync.Mutex
	Calls      []string                                  // ordered log of method calls, for sequence assertions
	ShellOut   map[string]string                         // script-substring → canned stdout (first match wins)
	ShellFn    func(node, script string) (string, error) // optional override for dynamic responses
	UpErr      error
	DownErr    error
	InstallErr error
	ImportErr  error
	Kubeconf   string
	ServerN    string
	WorkerN    string
}

// NewFakeSubstrate returns a FakeSubstrate with sensible defaults.
func NewFakeSubstrate() *FakeSubstrate {
	return &FakeSubstrate{
		ShellOut: map[string]string{},
		Kubeconf: "/tmp/fake-kubeconfig",
		ServerN:  "fake-server",
		WorkerN:  "fake-worker",
	}
}

func (f *FakeSubstrate) record(call string) {
	f.mu.Lock()
	f.Calls = append(f.Calls, call)
	f.mu.Unlock()
}

func (f *FakeSubstrate) Up(_ context.Context) error {
	f.record("Up")
	return f.UpErr
}

func (f *FakeSubstrate) Down(_ context.Context) error {
	f.record("Down")
	return f.DownErr
}

func (f *FakeSubstrate) InstallNatra(_ context.Context) error {
	f.record("InstallNatra")
	return f.InstallErr
}

func (f *FakeSubstrate) ImportImage(_ context.Context, image, dockerfile string) error {
	f.record(fmt.Sprintf("ImportImage(%s,%s)", image, dockerfile))
	return f.ImportErr
}

func (f *FakeSubstrate) KubeconfigPath() string { return f.Kubeconf }

func (f *FakeSubstrate) Nodes() (string, string) { return f.ServerN, f.WorkerN }

// NodeShell first consults ShellFn (if set), then falls back to a
// substring match against ShellOut. Unknown scripts return an empty
// stdout and a nil error — tests that care about a specific script
// should pre-populate ShellOut or set ShellFn.
func (f *FakeSubstrate) NodeShell(_ context.Context, node, script string) ([]byte, error) {
	f.record(fmt.Sprintf("NodeShell(%s,%.40q)", node, script))
	if f.ShellFn != nil {
		out, err := f.ShellFn(node, script)
		return []byte(out), err
	}
	for needle, out := range f.ShellOut {
		if needle != "" && containsSubstr(script, needle) {
			return []byte(out), nil
		}
	}
	return nil, nil
}

func (f *FakeSubstrate) EnsureBpftool(_ context.Context, node string) error {
	f.record(fmt.Sprintf("EnsureBpftool(%s)", node))
	return nil
}

func (f *FakeSubstrate) Name() string { return "fake" }

// containsSubstr is a tiny stdlib-free substring check so this file
// can avoid importing strings just for that one call.
func containsSubstr(s, sub string) bool {
	if len(sub) == 0 {
		return true
	}
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
