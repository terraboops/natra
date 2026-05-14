package main

// iperfResult is the subset of iperf3's -J output that the throttle
// assertion reads. iperf3's full schema has dozens of fields per
// stream + interval + cpu_utilization; we only need the receiver's
// aggregated bits-per-second.
//
// Replaces the shell rig's `jq '.end.sum_received.bits_per_second'`
// call — typed parse, no jq dependency, schema mismatches surface
// at unmarshal time instead of as a silent "0".
type iperfResult struct {
	End struct {
		SumReceived struct {
			BitsPerSecond float64 `json:"bits_per_second"`
		} `json:"sum_received"`
		SumSent struct {
			BitsPerSecond float64 `json:"bits_per_second"`
		} `json:"sum_sent"`
	} `json:"end"`
}
