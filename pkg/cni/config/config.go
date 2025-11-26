package config

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// Config holds CNI plugin configuration parsed from Pod annotations
type Config struct {
	// Rate limit in bytes per second
	Rate int64

	// Burst size in bytes
	Burst int64

	// CMS configuration
	CMSWidth             int
	CMSDepth             int
	HeavyHitterThreshold int64

	// Token Bucket configuration
	TokenBucketRate  int64
	TokenBucketBurst int64
}

// DefaultConfig returns a Config with sensible defaults
func DefaultConfig() *Config {
	return &Config{
		Rate:                 0, // No rate limit
		Burst:                0,
		CMSWidth:             1024,
		CMSDepth:             4,
		HeavyHitterThreshold: 1000,
		TokenBucketRate:      100,
		TokenBucketBurst:     200,
	}
}

// ParseBandwidthAnnotation parses the kubernetes.io/ingress-bandwidth annotation
// Supports both simple format ("10M") and extended JSON format
func ParseBandwidthAnnotation(annotation string) (*Config, error) {
	if annotation == "" {
		return DefaultConfig(), nil
	}

	cfg := DefaultConfig()

	// Try parsing as JSON first (extended format)
	if strings.HasPrefix(strings.TrimSpace(annotation), "{") {
		return parseJSONConfig(annotation)
	}

	// Simple format: "10M", "1G", etc.
	rate, err := parseBandwidth(annotation)
	if err != nil {
		return nil, fmt.Errorf("invalid bandwidth format: %w", err)
	}

	cfg.Rate = rate
	cfg.Burst = rate * 2 // Default burst is 2x rate
	cfg.TokenBucketRate = rate
	cfg.TokenBucketBurst = cfg.Burst

	return cfg, nil
}

// parseJSONConfig parses the extended JSON configuration format
func parseJSONConfig(data string) (*Config, error) {
	cfg := DefaultConfig()

	var raw struct {
		Rate  string `json:"rate"`
		Burst string `json:"burst"`
		CMS   struct {
			Width                int   `json:"width"`
			Depth                int   `json:"depth"`
			HeavyHitterThreshold int64 `json:"heavyHitterThreshold"`
		} `json:"cms"`
		TokenBucket struct {
			Rate  int64 `json:"rate"`
			Burst int64 `json:"burst"`
		} `json:"tokenBucket"`
	}

	if err := json.Unmarshal([]byte(data), &raw); err != nil {
		return nil, fmt.Errorf("failed to parse JSON config: %w", err)
	}

	// Parse rate
	if raw.Rate != "" {
		rate, err := parseBandwidth(raw.Rate)
		if err != nil {
			return nil, fmt.Errorf("invalid rate: %w", err)
		}
		cfg.Rate = rate
		cfg.TokenBucketRate = rate
	}

	// Parse burst
	if raw.Burst != "" {
		burst, err := parseBandwidth(raw.Burst)
		if err != nil {
			return nil, fmt.Errorf("invalid burst: %w", err)
		}
		cfg.Burst = burst
		cfg.TokenBucketBurst = burst
	} else if cfg.Rate > 0 {
		cfg.Burst = cfg.Rate * 2
		cfg.TokenBucketBurst = cfg.Burst
	}

	// CMS config
	if raw.CMS.Width > 0 {
		cfg.CMSWidth = raw.CMS.Width
	}
	if raw.CMS.Depth > 0 {
		cfg.CMSDepth = raw.CMS.Depth
	}
	if raw.CMS.HeavyHitterThreshold > 0 {
		cfg.HeavyHitterThreshold = raw.CMS.HeavyHitterThreshold
	}

	// Token bucket config (overrides rate/burst if specified)
	if raw.TokenBucket.Rate > 0 {
		cfg.TokenBucketRate = raw.TokenBucket.Rate
	}
	if raw.TokenBucket.Burst > 0 {
		cfg.TokenBucketBurst = raw.TokenBucket.Burst
	}

	return cfg, nil
}

// parseBandwidth parses bandwidth strings like "10M", "1G", "500K"
func parseBandwidth(s string) (int64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, nil
	}

	// Extract numeric part and suffix
	var numStr string
	var suffix string
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

	// Apply multiplier based on suffix
	switch suffix {
	case "", "B":
		return num, nil
	case "K", "KB":
		return num * 1000, nil
	case "M", "MB":
		return num * 1000 * 1000, nil
	case "G", "GB":
		return num * 1000 * 1000 * 1000, nil
	case "KI", "KIB":
		return num * 1024, nil
	case "MI", "MIB":
		return num * 1024 * 1024, nil
	case "GI", "GIB":
		return num * 1024 * 1024 * 1024, nil
	default:
		return 0, fmt.Errorf("unknown suffix: %s", suffix)
	}
}

// Validate checks if configuration is valid
func (c *Config) Validate() error {
	if c.CMSWidth <= 0 {
		return fmt.Errorf("CMS width must be positive")
	}
	if c.CMSDepth <= 0 {
		return fmt.Errorf("CMS depth must be positive")
	}
	if c.Rate < 0 {
		return fmt.Errorf("rate cannot be negative")
	}
	if c.Burst < 0 {
		return fmt.Errorf("burst cannot be negative")
	}
	return nil
}
