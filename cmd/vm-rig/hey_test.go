package main

import "testing"

// parseHeyCSV is the seam between hey's text output and the
// assertion; round-tripping a small fixture catches column-order
// drift the moment hey changes its schema.
func TestParseHeyCSV(t *testing.T) {
	csv := []byte(`response-time,DNS+dialup,DNS,Request-write,Response-delay,Response-read,status-code,offset
0.010,0.0,0.0,0.0,0.0,0.0,200,0.0
0.020,0.0,0.0,0.0,0.0,0.0,200,0.0
0.030,0.0,0.0,0.0,0.0,0.0,200,0.0
0.040,0.0,0.0,0.0,0.0,0.0,500,0.0
0.050,0.0,0.0,0.0,0.0,0.0,200,0.0
`)
	res, err := parseHeyCSV(csv, 1.0)
	if err != nil {
		t.Fatalf("parseHeyCSV: %v", err)
	}
	if res.OK != 4 {
		t.Errorf("OK = %d, want 4", res.OK)
	}
	if res.Errors != 1 {
		t.Errorf("Errors = %d, want 1", res.Errors)
	}
	if res.RPS() != 4.0 {
		t.Errorf("RPS = %.2f, want 4.00", res.RPS())
	}
	// 4 OK samples sorted = [0.01, 0.02, 0.03, 0.05]. Nearest-rank
	// p50 over 4 samples lands at idx ceil(0.5*4)-1 = 1 (= 0.02);
	// p99 lands at idx ceil(0.99*4)-1 = 3 (= 0.05).
	if res.P50 != 0.02 {
		t.Errorf("P50 = %.3f, want 0.020", res.P50)
	}
	if res.P99 != 0.05 {
		t.Errorf("P99 = %.3f, want 0.050", res.P99)
	}
	if res.P100 != 0.05 {
		t.Errorf("P100 = %.3f, want 0.050", res.P100)
	}
}

func TestParseHeyCSVEmpty(t *testing.T) {
	_, err := parseHeyCSV([]byte("response-time,status-code\n"), 1.0)
	if err == nil {
		t.Fatal("expected error on header-only CSV, got nil")
	}
}
