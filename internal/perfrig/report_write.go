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

// writeIperfTable summarizes the iperfSweep workload: one row per
// (phase, rate), columns for ingress + egress. Grouping by rate is
// required for the multi-rate sweep (full profile) — collapsing
// across rates would average 10M with 1G with 10G, which is
// nonsense. ci profile produces one rate per phase so the table
// stays compact there.
func writeIperfTable(b *strings.Builder, rep Report) {
	if !anyWorkloadHasData(rep, WorkloadIperfSweep) {
		return
	}
	fmt.Fprintf(b, "\niperfSweep: per-direction elephant throughput (per phase × rate)\n")
	fmt.Fprintf(b, "%-10s  %-6s  %-14s  %-14s\n",
		"Phase", "rate", "iperf ing Mbps", "iperf eg Mbps")
	fmt.Fprintf(b, "%s\n", strings.Repeat("-", 52))
	for _, p := range rep.Phases {
		// Group ingress + egress samples by rate, preserving the
		// order rates first appeared in the workload's IperfCells
		// (which matches plan.Rates order).
		var rateOrder []Rate
		ingByRate := map[Rate][]float64{}
		egByRate := map[Rate][]float64{}
		for _, w := range p.Workloads {
			if w.Kind != WorkloadIperfSweep {
				continue
			}
			for _, c := range w.IperfCells {
				if _, seen := ingByRate[c.Rate]; !seen {
					if _, sawEg := egByRate[c.Rate]; !sawEg {
						rateOrder = append(rateOrder, c.Rate)
					}
				}
				switch c.Direction {
				case "ingress":
					ingByRate[c.Rate] = append(ingByRate[c.Rate], c.Bps)
				case "egress":
					egByRate[c.Rate] = append(egByRate[c.Rate], c.Bps)
				}
			}
		}
		for _, r := range rateOrder {
			fmt.Fprintf(b, "%-10s  %-6s  %-14s  %-14s\n",
				p.Phase, r,
				cell(ingByRate[r], 1e6, 1), cell(egByRate[r], 1e6, 1))
		}
	}
}

// writeMixedTable reports the mixed workload: iperf3 --bidir
// elephant plus annotated mice (CMS fast-pass story) and bystander
// mice (collateral cost on an unannotated neighbor). The trailing
// "mice total" column (annotated + bystander RPS) is the
// apples-to-apples comparison: capping the elephant frees worker
// capacity, and the mice classes (annotated + bystander) share
// it; reading either column alone hides whether the plugin
// honored the annotated bucket fairly or just collapsed annotated
// mice and handed their share to the bystander.
func writeMixedTable(b *strings.Builder, rep Report) {
	if !anyWorkloadHasData(rep, WorkloadMixed) {
		return
	}
	fmt.Fprintf(b, "\nmixed: iperf3 --bidir elephant + concurrent hey mice on annotated + bystander pods\n")
	fmt.Fprintf(b, "%-10s  %-13s  %-13s  %-11s  %-9s  %-11s  %-11s  %-9s  %-11s  %-12s\n",
		"Phase", "ing Mbps", "eg Mbps",
		"pod rps", "p50 ms", "p99 ms",
		"by rps", "p50 ms", "p99 ms",
		"mice total")
	fmt.Fprintf(b, "%s\n", strings.Repeat("-", 124))
	for _, p := range rep.Phases {
		var ing, eg, prps, pp50, pp99, brps, bp50, bp99, totals []float64
		for _, w := range p.Workloads {
			if w.Kind != WorkloadMixed {
				continue
			}
			for _, m := range w.MixedSamples {
				ing = append(ing, m.IperfIngressBps)
				eg = append(eg, m.IperfEgressBps)
				prps = append(prps, m.PodRPS)
				pp50 = append(pp50, m.PodP50)
				pp99 = append(pp99, m.PodP99)
				brps = append(brps, m.BystanderRPS)
				bp50 = append(bp50, m.BystanderP50)
				bp99 = append(bp99, m.BystanderP99)
				// Per-sample total so mean+stddev aggregates
				// correctly when Samples > 1; summing the means
				// would lose stddev signal.
				totals = append(totals, m.PodRPS+m.BystanderRPS)
			}
		}
		fmt.Fprintf(b, "%-10s  %-13s  %-13s  %-11s  %-9s  %-11s  %-11s  %-9s  %-11s  %-12s\n",
			p.Phase,
			cell(ing, 1e6, 1), cell(eg, 1e6, 1),
			cell(prps, 1, 0), cell(pp50, 1, 1), cell(pp99, 1, 1),
			cell(brps, 1, 0), cell(bp50, 1, 1), cell(bp99, 1, 1),
			cell(totals, 1, 0))
	}
}

// anyWorkloadHasData reports whether at least one phase carries
// data for the named workload. Used to skip empty sections in the
// report — a workload that never produced samples (e.g. mixed when
// the bystander deploy failed) shouldn't render an empty header.
func anyWorkloadHasData(rep Report, kind WorkloadKind) bool {
	for _, p := range rep.Phases {
		for _, w := range p.Workloads {
			if w.Kind != kind {
				continue
			}
			if len(w.IperfCells) > 0 || len(w.MixedSamples) > 0 || len(w.MemorySamples) > 0 {
				return true
			}
		}
	}
	return false
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
