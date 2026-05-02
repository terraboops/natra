package config_test

import (
	"testing"

	"github.com/terraboops/natra/pkg/cni/config"
)

func BenchmarkParseBandwidthAnnotationSimple(b *testing.B) {
	for b.Loop() {
		_, _ = config.ParseBandwidthAnnotation("10M")
	}
}

func BenchmarkParseBandwidthAnnotationJSON(b *testing.B) {
	in := `{"rate":"10M","burst":"20M",` +
		`"cms":{"width":1024,"depth":4,"heavyHitterThreshold":1000},` +
		`"tokenBucket":{"rate":100,"burst":200}}`
	for b.Loop() {
		_, _ = config.ParseBandwidthAnnotation(in)
	}
}

func BenchmarkValidate(b *testing.B) {
	cfg := config.DefaultConfig()
	for b.Loop() {
		_ = cfg.Validate()
	}
}
