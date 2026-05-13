package main

import "testing"

// TestResolveHHThreshold pins the rate-scaled threshold formula:
// threshold = max(minHHThresholdBytes, rate_bps × time_constant_ms / 1000)
// with explicit cluster-default override taking precedence over the
// scaling.
func TestResolveHHThreshold(t *testing.T) {
	cases := []struct {
		name     string
		defaults *NetConfDefaults
		rateBps  int64
		want     int64
	}{
		{
			name:    "default time constant, 10 Mbps → ~125 KiB",
			rateBps: 10_000_000 / 8, // bits to bytes/sec
			want:    1_250_000 * 100 / 1000,
		},
		{
			name:    "default time constant, 100 Mbps → ~1.25 MiB",
			rateBps: 100_000_000 / 8,
			want:    12_500_000 * 100 / 1000,
		},
		{
			name:    "default time constant, 1 Gbps → ~12.5 MiB",
			rateBps: 1_000_000_000 / 8,
			want:    125_000_000 * 100 / 1000,
		},
		{
			name:    "low rate clamps to 16 KiB floor",
			rateBps: 1000, // 1 KB/s × 100 ms = 100 B, under floor
			want:    16 * 1024,
		},
		{
			name:     "explicit cluster default overrides rate-scaling",
			defaults: &NetConfDefaults{HHThreshold: 999_999},
			rateBps:  1_000_000_000,
			want:     999_999,
		},
		{
			name:     "custom time constant applies",
			defaults: &NetConfDefaults{FastPassTimeConstantMs: 250},
			rateBps:  1_250_000, // 10 Mbps
			want:     1_250_000 * 250 / 1000,
		},
		{
			name:     "explicit HHThreshold beats time constant",
			defaults: &NetConfDefaults{HHThreshold: 100_000, FastPassTimeConstantMs: 250},
			rateBps:  1_000_000_000,
			want:     100_000,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			conf := &NetConf{Defaults: c.defaults}
			got := resolveHHThreshold(conf, c.rateBps)
			if got != c.want {
				t.Errorf("got %d, want %d", got, c.want)
			}
		})
	}
}
