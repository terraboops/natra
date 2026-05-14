package main

import (
	"bytes"
	"encoding/csv"
	"fmt"
	"math"
	"sort"
	"strconv"
)

// heyResult is the aggregated view of one `hey -o csv` run.
// hey-the-binary's CSV output is one row per HTTP request; we
// distill it into the few numbers the natra fast-pass assertion
// actually reads.
type heyResult struct {
	// Total non-error requests counted. heyResult treats any
	// status outside 200-399 as a failure and excludes it from
	// the latency percentiles.
	OK int
	// Total error requests (status >= 400, or network failures
	// reported as status 0 by hey).
	Errors int
	// DurationSecs is the wall-clock window the rig asked hey to
	// run (the -z flag we passed). Used to compute RPS without
	// relying on hey's own summary line.
	DurationSecs float64
	// Response-time percentiles in seconds, computed over the OK
	// set. p100 is the slowest single request.
	P50, P99, P100 float64
}

// RPS is total OK requests divided by the asked-for duration.
// This is intentionally not "max throughput hey could push" — it's
// the rate that actually completed inside the measurement window,
// which is what matters for the fast-pass assertion.
func (r *heyResult) RPS() float64 {
	if r.DurationSecs == 0 {
		return 0
	}
	return float64(r.OK) / r.DurationSecs
}

// parseHeyCSV reads hey's -o csv output and produces an aggregated
// heyResult. CSV schema (as of hey master / v0.1.4):
//
//	response-time,DNS+dialup,DNS,Request-write,Response-delay,Response-read,status-code,offset
//
// Only response-time and status-code matter for our assertion. The
// other columns are timing breakdowns we don't currently use.
func parseHeyCSV(raw []byte, duration float64) (*heyResult, error) {
	r := csv.NewReader(bytes.NewReader(raw))
	r.FieldsPerRecord = -1 // tolerate the header having a different field count
	rows, err := r.ReadAll()
	if err != nil {
		return nil, fmt.Errorf("read hey CSV: %w", err)
	}
	if len(rows) < 2 {
		return nil, fmt.Errorf("hey CSV: %d rows (expected header + data)", len(rows))
	}

	// Skip the header row. Header column "response-time" sits at
	// index 0; "status-code" at index 6. Lock to those indices —
	// any future schema change shows up as a parse failure.
	latencies := make([]float64, 0, len(rows)-1)
	out := &heyResult{DurationSecs: duration}
	for _, row := range rows[1:] {
		if len(row) < 7 {
			continue
		}
		rt, err := strconv.ParseFloat(row[0], 64)
		if err != nil {
			continue
		}
		status, err := strconv.Atoi(row[6])
		if err != nil {
			continue
		}
		if status < 200 || status >= 400 {
			out.Errors++
			continue
		}
		out.OK++
		latencies = append(latencies, rt)
	}

	if len(latencies) > 0 {
		sort.Float64s(latencies)
		out.P50 = percentile(latencies, 0.50)
		out.P99 = percentile(latencies, 0.99)
		out.P100 = latencies[len(latencies)-1]
	}
	return out, nil
}

// percentile pulls the k-th percentile out of a sorted slice via
// nearest-rank. Good enough for an assertion threshold; not an
// HDR-histogram-precision number.
func percentile(sorted []float64, k float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	idx := int(math.Ceil(k*float64(len(sorted)))) - 1
	if idx < 0 {
		idx = 0
	}
	if idx >= len(sorted) {
		idx = len(sorted) - 1
	}
	return sorted[idx]
}
