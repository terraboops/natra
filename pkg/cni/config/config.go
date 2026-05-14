// Package config parses the bandwidth pod annotations. Two forms are
// accepted:
//
//   - Simple:   "10M"   →  Rate=1_250_000 bytes/s, Burst=2_500_000 bytes
//   - Extended: {"rate":"10M","burst":"15M","heavyHitterThreshold":131072}
//
// Units: rate and burst inputs follow the kubernetes
// kubernetes.io/{ingress,egress}-bandwidth convention — bits per
// second, with SI/IEC suffixes (K=10^3 bits, M=10^6 bits, Ki=2^10
// bits, etc.). The parser converts to bytes (divides by 8) before
// populating Config so the BPF dataplane gets bytes/sec. Matches
// kubelet's runtimeConfig.bandwidth interpretation, so the
// pod-annotation-direct path and the runtimeConfig path produce the
// same Config from the same annotation.
//
// heavyHitterThreshold is in bytes (CMS counts byte volume per flow,
// not packets). 131072 is a 128 KiB example; the default is 256 KiB.
//
// The parser is direction-agnostic — the caller picks the annotation
// key (`kubernetes.io/ingress-bandwidth` or
// `kubernetes.io/egress-bandwidth`) and passes the value through. Each
// direction has its own annotation; they're parsed independently.
//
// Pure parsing, no BPF or kernel dependencies — also the target of
// the Layer 1 fuzz suite.
package config

import (
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"
)

// maxRate caps a single accepted value. Half of int64-max so default
// `burst = rate * 2` can't overflow. ~4.6 EB/s — beyond any real link.
const maxRate = math.MaxInt64 / 2

// Config is what the BPF program needs per Pod: token-bucket refill
// rate, bucket capacity, and the CMS byte-volume above which a flow
// is classified heavy (and so subject to the bucket).
type Config struct {
	Rate                 int64 // bytes/sec; 0 disables rate limiting
	Burst                int64 // bytes; bucket capacity, also the largest single skb that can be admitted
	HeavyHitterThreshold int64 // CMS byte estimate above which a flow goes through the bucket
}

// DefaultHHThresholdBytes is the static fallback for the heavy-hitter
// threshold when no rate-scaled value is computed by the caller.
// 256 KiB is well above any reasonable HTTP request body and below
// what even a brief sustained TCP elephant accumulates (10 Mbps at
// MTU = ~840 packets/s × ~150 ms to cross). The threshold's unit is
// bytes since the BPF CMS counts byte volume per flow, not packets,
// so the meaning is invariant to GRO super-packet coalescing.
const DefaultHHThresholdBytes int64 = 256 * 1024

// DefaultBurstRatio sets the token bucket capacity as a multiple of
// the rate (in seconds of credit). 0.5 = half a second of credit;
// at 10 Mbps that's 625 KB, at 1 Gbps that's 62.5 MB.
//
// Trade-off: bigger burst tolerates spikier traffic without
// triggering throttle; smaller burst keeps the measured average
// throughput closer to the configured rate. Math: a T-second
// measurement of a long-lived elephant averages
// rate × (1 + burst_seconds / T), so 0.5 sec of credit over a
// 15-second window lands at 3.3% over rate — well inside the
// "vanilla-like" 1-5% envelope. (Previous value 2.0 landed at
// ~13% over.) Override per-cluster via defaults.burstRatio.
const DefaultBurstRatio = 0.5

// MinBurstBytes is the floor on the computed burst. At very low
// rates, rate × burstRatio falls below the size of a single
// GRO-coalesced super-packet, which would make the bucket reject
// every packet that arrives over the wire. Set the floor at 64 KB
// — one max-sized GSO super-packet — so any rate-scaled burst
// stays admittable.
const MinBurstBytes int64 = 64 * 1024

// DefaultBurstFor returns the burst (bytes) the parser uses when
// an annotation specifies a rate but no explicit burst. Centralized
// so the simple form, the JSON form, and the runtimeConfig path in
// cmd/natra/main.go all compute burst the same way.
func DefaultBurstFor(rate int64) int64 {
	b := int64(float64(rate) * DefaultBurstRatio)
	if b < MinBurstBytes {
		return MinBurstBytes
	}
	return b
}

// DefaultConfig returns the zero-rate baseline. Callers overwrite
// Rate (and optionally Burst) from the parsed annotation.
//
// HeavyHitterThreshold defaults to 256 KiB. The CMS counts BYTES so
// a flow's estimate is its byte volume, GRO-invariant. The threshold
// has to clear two things:
//
//   - The CMS hash-collision noise floor. With CMS_WIDTH=32768 cells ×
//     CMS_DEPTH=4 rows, the min-across-rows is robust to occasional
//     collisions; the threshold doesn't need to be huge for that.
//   - Real workload tail mice: HTTP requests up to several hundred KB
//     of body, WebSocket frames, mid-sized API responses. 256 KiB is
//     above the ~99th percentile of real HTTP request sizes.
//
// Above the threshold, a real elephant flow accumulates byte volume
// rapidly — at 10 Mbps any single sustained flow crosses 256 KiB in
// ~200 ms. The threshold catches them quickly while leaving tail
// mice intact.
//
// Tunable per-pod via the extended JSON annotation form's
// `heavyHitterThreshold` field, or cluster-wide via the conflist
// `defaults.hhThreshold` written by the installer from
// NATRA_DEFAULT_HH_THRESHOLD.
func DefaultConfig() *Config {
	return &Config{
		HeavyHitterThreshold: DefaultHHThresholdBytes,
	}
}

// ParseBandwidthAnnotation parses one bandwidth annotation value
// (either kubernetes.io/ingress-bandwidth or
// kubernetes.io/egress-bandwidth — same format both ways). Empty input
// returns DefaultConfig. JSON shapes (any leading whitespace then `{`)
// go through parseJSONConfig; everything else is treated as a simple
// "10M"-style value.
//
// The string parses as bits/sec per k8s convention; the returned
// Config carries bytes/sec, so a "10M" annotation becomes 1_250_000
// bytes/sec (10 Mbit/s).
func ParseBandwidthAnnotation(annotation string) (*Config, error) {
	if annotation == "" {
		return DefaultConfig(), nil
	}
	if strings.HasPrefix(strings.TrimSpace(annotation), "{") {
		return parseJSONConfig(annotation)
	}

	rateBits, err := parseBandwidth(annotation)
	if err != nil {
		return nil, fmt.Errorf("invalid bandwidth format: %w", err)
	}
	cfg := DefaultConfig()
	cfg.Rate = rateBits / 8
	cfg.Burst = DefaultBurstFor(cfg.Rate)
	return cfg, nil
}

// parseJSONConfig parses the extended JSON form. Unspecified fields
// keep the DefaultConfig value; specified ones override.
//
// "rate" and "burst" follow the same bits/sec convention as the
// simple form — both are divided by 8 to populate the bytes/sec
// Config fields. heavyHitterThreshold is a raw byte count (CMS unit).
func parseJSONConfig(data string) (*Config, error) {
	var raw struct {
		Rate                 string `json:"rate"`
		Burst                string `json:"burst"`
		HeavyHitterThreshold int64  `json:"heavyHitterThreshold"`
	}
	if err := json.Unmarshal([]byte(data), &raw); err != nil {
		return nil, fmt.Errorf("parse JSON config: %w", err)
	}

	cfg := DefaultConfig()
	if raw.Rate != "" {
		rateBits, err := parseBandwidth(raw.Rate)
		if err != nil {
			return nil, fmt.Errorf("invalid rate: %w", err)
		}
		cfg.Rate = rateBits / 8
	}
	if raw.Burst != "" {
		burstBits, err := parseBandwidth(raw.Burst)
		if err != nil {
			return nil, fmt.Errorf("invalid burst: %w", err)
		}
		cfg.Burst = burstBits / 8
	} else if cfg.Rate > 0 {
		cfg.Burst = DefaultBurstFor(cfg.Rate)
	}
	if raw.HeavyHitterThreshold > 0 {
		cfg.HeavyHitterThreshold = raw.HeavyHitterThreshold
	}
	return cfg, nil
}

// parseBandwidth parses a single quantity like "10M", "500K", "1Gi"
// and returns the scaled integer (units agnostic — the caller
// interprets the result as bits/sec, bytes, etc. per its own
// convention). SI suffixes (K, M, G) are decimal; IEC suffixes
// (Ki, Mi, Gi) are binary. Case-insensitive on the suffix.
func parseBandwidth(s string) (int64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, nil
	}

	var numStr, suffix string
	for i, c := range s {
		if c < '0' || c > '9' {
			numStr = s[:i]
			suffix = strings.ToUpper(s[i:])
			break
		}
	}
	if numStr == "" {
		numStr = s
	}

	num, err := strconv.ParseInt(numStr, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid number: %s", numStr)
	}

	multiplier := suffixMultiplier(suffix)
	if multiplier == 0 {
		return 0, fmt.Errorf("unknown suffix: %s", suffix)
	}
	if num > maxRate/multiplier {
		return 0, fmt.Errorf("value too large: %d%s exceeds maxRate", num, suffix)
	}
	return num * multiplier, nil
}

// suffixMultiplier returns the multiplier for the parsed suffix, or
// 0 if the suffix is unknown. Caller interprets the resulting scaled
// integer per its own unit convention. Pulled out so parseBandwidth's
// body is one fact per line.
func suffixMultiplier(suffix string) int64 {
	switch suffix {
	case "", "B":
		return 1
	case "K", "KB":
		return 1000
	case "M", "MB":
		return 1000 * 1000
	case "G", "GB":
		return 1000 * 1000 * 1000
	case "KI", "KIB":
		return 1024
	case "MI", "MIB":
		return 1024 * 1024
	case "GI", "GIB":
		return 1024 * 1024 * 1024
	}
	return 0
}

// Validate rejects internally-inconsistent configs. Doesn't apply
// defaults — that's DefaultConfig's job.
func (c *Config) Validate() error {
	if c.Rate < 0 {
		return fmt.Errorf("rate cannot be negative")
	}
	if c.Burst < 0 {
		return fmt.Errorf("burst cannot be negative")
	}
	if c.HeavyHitterThreshold < 0 {
		return fmt.Errorf("heavyHitterThreshold cannot be negative")
	}
	return nil
}
