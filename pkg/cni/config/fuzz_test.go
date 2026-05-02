package config_test

import (
	"testing"

	"github.com/terraboops/natra/pkg/cni/config"
)

// FuzzParseBandwidthAnnotation fuzzes the public entry point. The bandwidth
// annotation is attacker-influenced (any Pod author can set it), so the
// invariant we enforce is: never panic; if no error is returned, the resulting
// Config must validate.
func FuzzParseBandwidthAnnotation(f *testing.F) {
	seeds := []string{
		"",
		"10M",
		"100",
		"1G",
		"500K",
		"10MiB",
		`{"rate":"10M"}`,
		`{"rate":"10M","burst":"20M"}`,
		`{"rate":"10M","cms":{"width":1024,"depth":4}}`,
		`{}`,
		// known invalid forms — assert error path doesn't panic
		"abc",
		"-10M",
		"10X",
		"10.5M",
		"\x00\x01\x02",
		"{",
		`{"rate":}`,
		`{"rate":"abc"}`,
	}
	for _, s := range seeds {
		f.Add(s)
	}

	f.Fuzz(func(t *testing.T, in string) {
		cfg, err := config.ParseBandwidthAnnotation(in)
		if err != nil {
			return
		}
		if cfg == nil {
			t.Fatalf("nil config with no error for input %q", in)
		}
		if vErr := cfg.Validate(); vErr != nil {
			t.Fatalf("ParseBandwidthAnnotation accepted %q but Validate() rejected it: %v", in, vErr)
		}
	})
}
