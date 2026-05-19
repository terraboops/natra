package perfrig

import (
	"fmt"
	"io"
	"math"
	"os"
	"strings"
	"time"
)

// WriteReport prints the comparison tables to out and (if path is
// non-empty) writes the same content to a file. The shape is the
// same for both rigs — the only header difference is the
// Substrate / Profile tag, so docs/perf-vs-vanilla.md can render
// either rig's output side by side.
func WriteReport(rep Report, path string, out io.Writer) error {
	var b strings.Builder
	fmt.Fprintf(&b, "natra vs upstream bandwidth — %s (profile=%s)\n", rep.Substrate, rep.Profile)
	fmt.Fprintf(&b, "================================================================\n")
	fmt.Fprintf(&b, "Generated %s\n", time.Now().UTC().Format(time.RFC3339))

	writeIperfTable(&b, rep)
	writeMixedTable(&b, rep)
	writeMemoryTable(&b, rep)

	s := b.String()
	_, _ = fmt.Fprint(out, "\n"+s)
	if path != "" {
		if err := os.WriteFile(path, []byte(s), 0o644); err != nil {
			return fmt.Errorf("write result file %s: %w", path, err)
		}
		_, _ = fmt.Fprintf(out, "\nResult written to %s\n", path)
	}
	return nil
}

// writeIperfTable summarizes the iperfSweep+mixed throughput data.
// Today the iperfSweep workload also fills MixedSamples (rps/p50/p99
// from the hey HTTP path), so this table covers the
// throughput-and-mice story end to end per phase.
func writeIperfTable(b *strings.Builder, rep Report) {
	fmt.Fprintf(b, "\nPhase summary (iperf elephant + hey HTTP mice on the annotated pod)\n")
	fmt.Fprintf(b, "%-10s  %-14s  %-14s  %-14s  %-11s  %-11s\n",
		"Phase", "iperf ing Mbps", "iperf eg Mbps", "hey rps", "p50 ms", "p99 ms")
	fmt.Fprintf(b, "%s\n", strings.Repeat("-", 84))
	for _, p := range rep.Phases {
		ing, eg, rps, p50, p99 := collectMixedSeries(p)
		fmt.Fprintf(b, "%-10s  %-14s  %-14s  %-14s  %-11s  %-11s\n",
			p.Phase,
			cell(ing, 1e6, 1), cell(eg, 1e6, 1),
			cell(rps, 1, 0), cell(p50, 1, 1), cell(p99, 1, 1))
	}
}

func writeMixedTable(b *strings.Builder, rep Report) {
	// Mixed-with-bystander lands in the next slice; until then this
	// section is suppressed rather than printing an empty header.
	hasBystander := false
	for _, p := range rep.Phases {
		for _, w := range p.Workloads {
			if w.Kind == WorkloadMixed && len(w.MixedSamples) > 0 {
				hasBystander = true
				break
			}
		}
	}
	if !hasBystander {
		return
	}
	fmt.Fprintf(b, "\n(mixed workload with bystander — pending wiring)\n")
}

// writeMemoryTable renders the three-comparable memory section.
// baseline rows should hover at the empirical noise floor; vanilla
// rows show qdisc footprint (kernel-mem delta) with the qdisc
// count as corroboration; natra rows show BPF footprint with the
// bpftool memlock byte-exact number.
func writeMemoryTable(b *strings.Builder, rep Report) {
	hasMem := false
	for _, p := range rep.Phases {
		for _, w := range p.Workloads {
			if w.Kind == WorkloadMemory && len(w.MemorySamples) > 0 {
				hasMem = true
			}
		}
	}
	if !hasMem {
		return
	}
	fmt.Fprintf(b, "\nMemory comparison (per worker node, baseline = 0 / noise floor)\n")
	fmt.Fprintf(b, "%-10s  %-14s  %-14s  %-12s  %-12s  %-14s  %-12s\n",
		"Phase", "kmem/pod KB", "kmem@N KB", "qdiscs@N", "bpf memlock", "invokeRSS KB", "DS RSS KB")
	fmt.Fprintf(b, "%s\n", strings.Repeat("-", 96))
	for _, p := range rep.Phases {
		var perPod, atN, qdiscs, bpfmem, invokeRSS, dsRSS []float64
		var n int
		for _, w := range p.Workloads {
			if w.Kind != WorkloadMemory {
				continue
			}
			for _, m := range w.MemorySamples {
				if m.NPodCount > 1 {
					perPod = append(perPod, float64(m.DataplaneKmemBytesN-m.DataplaneKmemBytes1)/float64(m.NPodCount-1))
				}
				atN = append(atN, float64(m.DataplaneKmemBytesN))
				qdiscs = append(qdiscs, float64(m.VanillaQdiscBytes))
				bpfmem = append(bpfmem, float64(m.NatraBpfMemlockBytes))
				invokeRSS = append(invokeRSS, float64(m.PluginInvokePeakRSSBytes))
				dsRSS = append(dsRSS, float64(m.InstallerDSBytes))
				if m.NPodCount > 0 {
					n = m.NPodCount
				}
			}
		}
		_ = n
		fmt.Fprintf(b, "%-10s  %-14s  %-14s  %-12s  %-12s  %-14s  %-12s\n",
			p.Phase,
			cell(perPod, 1024, 1),
			cell(atN, 1024, 1),
			cell(qdiscs, 1, 0),
			cell(bpfmem, 1, 0),
			cell(invokeRSS, 1024, 1),
			cell(dsRSS, 1024, 1))
	}
}

func collectMixedSeries(p PhaseReport) (ing, eg, rps, p50, p99 []float64) {
	for _, w := range p.Workloads {
		for _, m := range w.MixedSamples {
			ing = append(ing, m.IperfIngressBps)
			eg = append(eg, m.IperfEgressBps)
			rps = append(rps, m.PodRPS)
			p50 = append(p50, m.PodP50)
			p99 = append(p99, m.PodP99)
		}
	}
	return
}

// cell renders mean (and ±stddev when >1 sample) at the given
// scale, rounded to dp decimal places. "—" if the slice is empty,
// so a missing measurement reads as missing rather than 0.
func cell(xs []float64, scale float64, dp int) string {
	if len(xs) == 0 {
		return "—"
	}
	m, s := meanStd(xs)
	if len(xs) < 2 {
		return fmt.Sprintf("%.*f", dp, m/scale)
	}
	return fmt.Sprintf("%.*f±%.*f", dp, m/scale, dp, s/scale)
}

// meanStd returns the arithmetic mean and sample standard deviation
// (n-1). std is 0 for fewer than two samples.
func meanStd(xs []float64) (mean, std float64) {
	if len(xs) == 0 {
		return 0, 0
	}
	for _, x := range xs {
		mean += x
	}
	mean /= float64(len(xs))
	if len(xs) < 2 {
		return mean, 0
	}
	var v float64
	for _, x := range xs {
		d := x - mean
		v += d * d
	}
	return mean, math.Sqrt(v / float64(len(xs)-1))
}
