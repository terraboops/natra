package perfrig

// iperfJSON is the subset of iperf3's -J output the executor reads.
// iperf3's full schema has dozens of fields per stream + interval
// + cpu_utilization; we only need the receiver-side aggregated
// bits-per-second.
//
// Typed parse, no jq dependency — a schema mismatch surfaces at
// json.Unmarshal time rather than as a silent 0.
type iperfJSON struct {
	End struct {
		SumReceived struct {
			BitsPerSecond float64 `json:"bits_per_second"`
		} `json:"sum_received"`
		SumSent struct {
			BitsPerSecond float64 `json:"bits_per_second"`
		} `json:"sum_sent"`
	} `json:"end"`
}
