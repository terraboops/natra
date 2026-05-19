package perfrig

import "testing"

// TestParseTimeVPeakKB — `/usr/bin/time -v` reports a "Maximum
// resident set size (kbytes): N" line; the parser must read that
// N from a realistic invocation. Two cases: present (returns the
// number) and absent (returns 0 rather than panicking on a binary
// that lacks /usr/bin/time -v).
func TestParseTimeVPeakKB(t *testing.T) {
	withLine := `Command exited with non-zero status 0
	Command being timed: "/opt/cni/bin/natra"
	User time (seconds): 0.01
	System time (seconds): 0.00
	Percent of CPU this job got: 100%
	Elapsed (wall clock) time (h:mm:ss or m:ss): 0:00.01
	Maximum resident set size (kbytes): 4096
	Average resident set size (kbytes): 0
`
	if got := parseTimeVPeakKB(withLine); got != 4096 {
		t.Errorf("with line: got %d, want 4096", got)
	}
	if got := parseTimeVPeakKB("nothing useful here"); got != 0 {
		t.Errorf("absent line: got %d, want 0", got)
	}
}

// TestSumNatraBpfMemlock — bpftool emits a JSON array per command;
// the workload feeds (prog show + map show) concatenated. The
// parser must sum bytes_memlock over objects named natra_* and
// ignore everything else.
func TestSumNatraBpfMemlock(t *testing.T) {
	// A realistic concatenation: prog show's array followed by
	// map show's array, each with one matching object and one
	// non-matching object (cilium's, in a real setup with both).
	raw := `[{"id":12,"type":"sched_cls","name":"natra_ingress","bytes_memlock":4096},{"id":13,"type":"sched_cls","name":"cil_from_container","bytes_memlock":8192}][{"id":20,"type":"percpu_array","name":"natra_stats_map","bytes_memlock":1024},{"id":21,"type":"hash","name":"other_map","bytes_memlock":2048}]`
	got := sumNatraBpfMemlock([]byte(raw))
	want := int64(4096 + 1024) // natra_ingress + natra_stats_map
	if got != want {
		t.Errorf("sum: got %d, want %d", got, want)
	}
}

// TestSumNatraBpfMemlock_Empty — no input, no panic, zero result.
func TestSumNatraBpfMemlock_Empty(t *testing.T) {
	if got := sumNatraBpfMemlock(nil); got != 0 {
		t.Errorf("nil input: got %d, want 0", got)
	}
	if got := sumNatraBpfMemlock([]byte("")); got != 0 {
		t.Errorf("empty input: got %d, want 0", got)
	}
}

// TestSumNatraBpfMemlock_Malformed — bpftool occasionally prints
// warnings or partial output when a map is being modified. The
// parser must shrug off the un-parseable chunk rather than
// failing the whole run.
func TestSumNatraBpfMemlock_Malformed(t *testing.T) {
	raw := `not valid json at all][{"name":"natra_ingress","bytes_memlock":2048}]`
	got := sumNatraBpfMemlock([]byte(raw))
	if got != 2048 {
		t.Errorf("malformed leading chunk: got %d, want 2048 (second chunk should still parse)", got)
	}
}

// TestStripBandwidthAnnotations — the baseline phase relies on
// stripping these so the bundled bandwidth plugin is inert. A
// regression that silently keeps the annotations would muddy the
// baseline; pin the behavior.
func TestStripBandwidthAnnotations(t *testing.T) {
	in := `apiVersion: v1
kind: Pod
metadata:
  name: perf-server
  annotations:
    kubernetes.io/ingress-bandwidth: "10M"
    kubernetes.io/egress-bandwidth: "10M"
    other.io/something: keep-me
spec:
  containers:
  - name: nginx
`
	out := stripBandwidthAnnotations(in)
	if contains(out, "ingress-bandwidth") || contains(out, "egress-bandwidth") {
		t.Errorf("strip missed an annotation:\n%s", out)
	}
	if !contains(out, "other.io/something") {
		t.Errorf("strip removed an unrelated annotation:\n%s", out)
	}
	if !contains(out, "name: perf-server") {
		t.Errorf("strip damaged the manifest:\n%s", out)
	}
}

func contains(s, sub string) bool { return containsSubstr(s, sub) }
