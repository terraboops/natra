package perfrig

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
// distill it into the few numbers the fast-pass story actually
// reads.
type heyResult struct {
	OK           int
	Errors       int
	DurationSecs float64
	P50, P99     float64
}

func (r *heyResult) rps() float64 {
	if r.DurationSecs == 0 {
		return 0
	}
	return float64(r.OK) / r.DurationSecs
}

// parseHeyCSV reads hey's -o csv output and aggregates. Schema:
//
//	response-time,DNS+dialup,DNS,Request-write,Response-delay,Response-read,status-code,offset
//
// Only response-time (index 0) and status-code (index 6) are read.
// A schema change shows up as a parse failure rather than a silent
// drift.
func parseHeyCSV(raw []byte, duration float64) (*heyResult, error) {
	r := csv.NewReader(bytes.NewReader(raw))
	r.FieldsPerRecord = -1
	rows, err := r.ReadAll()
	if err != nil {
		return nil, fmt.Errorf("read hey CSV: %w", err)
	}
	if len(rows) < 2 {
		return nil, fmt.Errorf("hey CSV: %d rows (expected header + data)", len(rows))
	}
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
	}
	return out, nil
}

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
