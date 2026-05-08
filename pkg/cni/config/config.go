// Package config parses the bandwidth pod annotations. Two forms are
// accepted:
//
//   - Simple:   "10M"   →  Rate=10_000_000 Burst=20_000_000
//   - Extended: {"rate":"10M","burst":"15M","heavyHitterThreshold":50}
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
// rate, bucket capacity, and the CMS count above which a flow is
// classified heavy (and so subject to the bucket).
type Config struct {
	Rate                 int64 // bytes/sec; 0 disables rate limiting
	Burst                int64 // bytes; bucket capacity, also the largest single skb that can be admitted
	HeavyHitterThreshold int64 // CMS estimate above which a flow goes through the bucket
}

// DefaultConfig returns the zero-rate baseline. Callers overwrite
// Rate (and optionally Burst) from the parsed annotation.
//
// HeavyHitterThreshold defaults to 10 because GRO superpackets at the
// BPF layer can be 30+ TCP segments each, so a 1000-packet threshold
// lets ~27 MB through per flow before throttling kicks in. 10 catches
// real elephants within a few skbs while leaving mice (DNS, brief
// HTTP) untouched.
func DefaultConfig() *Config {
	return &Config{
		HeavyHitterThreshold: 10,
	}
}

// ParseBandwidthAnnotation parses one bandwidth annotation value
// (either kubernetes.io/ingress-bandwidth or
// kubernetes.io/egress-bandwidth — same format both ways). Empty input
// returns DefaultConfig. JSON shapes (any leading whitespace then `{`)
// go through parseJSONConfig; everything else is treated as a simple
// "10M"-style value.
func ParseBandwidthAnnotation(annotation string) (*Config, error) {
	if annotation == "" {
		return DefaultConfig(), nil
	}
	if strings.HasPrefix(strings.TrimSpace(annotation), "{") {
		return parseJSONConfig(annotation)
	}

	rate, err := parseBandwidth(annotation)
	if err != nil {
		return nil, fmt.Errorf("invalid bandwidth format: %w", err)
	}
	cfg := DefaultConfig()
	cfg.Rate = rate
	cfg.Burst = rate * 2
	return cfg, nil
}

// parseJSONConfig parses the extended JSON form. Unspecified fields
// keep the DefaultConfig value; specified ones override.
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
		rate, err := parseBandwidth(raw.Rate)
		if err != nil {
			return nil, fmt.Errorf("invalid rate: %w", err)
		}
		cfg.Rate = rate
	}
	if raw.Burst != "" {
		burst, err := parseBandwidth(raw.Burst)
		if err != nil {
			return nil, fmt.Errorf("invalid burst: %w", err)
		}
		cfg.Burst = burst
	} else if cfg.Rate > 0 {
		cfg.Burst = cfg.Rate * 2
	}
	if raw.HeavyHitterThreshold > 0 {
		cfg.HeavyHitterThreshold = raw.HeavyHitterThreshold
	}
	return cfg, nil
}

// parseBandwidth parses a single quantity like "10M", "500K", "1Gi".
// SI suffixes (K, M, G) are decimal; IEC suffixes (Ki, Mi, Gi) are
// binary. Case-insensitive on the suffix.
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

// suffixMultiplier returns the byte multiplier for the parsed suffix,
// or 0 if the suffix is unknown. Pulled out so parseBandwidth's body
// is one fact per line.
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
