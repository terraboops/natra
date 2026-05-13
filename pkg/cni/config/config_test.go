package config_test

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/terraboops/natra/pkg/cni/config"
)

var _ = Describe("ParseBandwidthAnnotation", func() {
	Context("with empty input", func() {
		It("returns the default config", func() {
			cfg, err := config.ParseBandwidthAnnotation("")
			Expect(err).NotTo(HaveOccurred())
			Expect(cfg).To(Equal(config.DefaultConfig()))
		})
	})

	Context("with simple form", func() {
		// Inputs are bits/sec (k8s convention); Config.Rate is
		// bytes/sec. Expected values are the bit-quantity divided by 8.
		DescribeTable("decimal SI suffixes parse bits/sec -> bytes/sec",
			func(annotation string, expected int64) {
				cfg, err := config.ParseBandwidthAnnotation(annotation)
				Expect(err).NotTo(HaveOccurred())
				Expect(cfg.Rate).To(Equal(expected))
				Expect(cfg.Burst).To(Equal(expected * 2))
			},
			Entry("plain integer", "100", int64(100/8)),
			Entry("explicit B suffix", "100B", int64(100/8)),
			Entry("K (1000 bits)", "10K", int64(10*1000/8)),
			Entry("KB", "10KB", int64(10*1000/8)),
			Entry("M (10 Mbit -> 1.25 MB/s)", "10M", int64(10*1000*1000/8)),
			Entry("MB", "10MB", int64(10*1000*1000/8)),
			Entry("G (1 Gbit -> 125 MB/s)", "1G", int64(1*1000*1000*1000/8)),
			Entry("GB", "1GB", int64(1*1000*1000*1000/8)),
		)

		DescribeTable("binary IEC suffixes parse bits/sec -> bytes/sec",
			func(annotation string, expected int64) {
				cfg, err := config.ParseBandwidthAnnotation(annotation)
				Expect(err).NotTo(HaveOccurred())
				Expect(cfg.Rate).To(Equal(expected))
			},
			Entry("Ki", "10Ki", int64(10*1024/8)),
			Entry("KiB", "10KiB", int64(10*1024/8)),
			Entry("Mi", "10Mi", int64(10*1024*1024/8)),
			Entry("MiB", "10MiB", int64(10*1024*1024/8)),
			Entry("Gi", "1Gi", int64(1*1024*1024*1024/8)),
			Entry("GiB", "1GiB", int64(1*1024*1024*1024/8)),
		)

		It("is case-insensitive on the suffix", func() {
			lo, err := config.ParseBandwidthAnnotation("10m")
			Expect(err).NotTo(HaveOccurred())
			hi, err := config.ParseBandwidthAnnotation("10M")
			Expect(err).NotTo(HaveOccurred())
			Expect(lo.Rate).To(Equal(hi.Rate))
		})

		DescribeTable("rejects malformed input",
			func(annotation string) {
				_, err := config.ParseBandwidthAnnotation(annotation)
				Expect(err).To(HaveOccurred())
			},
			Entry("non-numeric prefix", "abc"),
			Entry("unknown suffix", "10X"),
			Entry("trailing garbage", "10MX"),
			Entry("negative value", "-10M"),
			Entry("decimal", "10.5M"),
		)
	})

	Context("with extended JSON form", func() {
		It("parses rate-only (50 Mbit -> 6.25 MB/s)", func() {
			cfg, err := config.ParseBandwidthAnnotation(`{"rate":"50M"}`)
			Expect(err).NotTo(HaveOccurred())
			Expect(cfg.Rate).To(Equal(int64(50_000_000 / 8)))
			Expect(cfg.Burst).To(Equal(int64(50_000_000 / 8 * 2)))
		})

		It("parses rate + explicit burst (both bits/sec)", func() {
			cfg, err := config.ParseBandwidthAnnotation(`{"rate":"50M","burst":"75M"}`)
			Expect(err).NotTo(HaveOccurred())
			Expect(cfg.Rate).To(Equal(int64(50_000_000 / 8)))
			Expect(cfg.Burst).To(Equal(int64(75_000_000 / 8)))
		})

		It("parses heavyHitterThreshold override (raw bytes)", func() {
			cfg, err := config.ParseBandwidthAnnotation(
				`{"rate":"10M","heavyHitterThreshold":5000}`,
			)
			Expect(err).NotTo(HaveOccurred())
			Expect(cfg.HeavyHitterThreshold).To(Equal(int64(5000)))
		})

		It("rejects malformed JSON", func() {
			_, err := config.ParseBandwidthAnnotation(`{"rate":}`)
			Expect(err).To(HaveOccurred())
		})

		It("rejects bad rate strings inside JSON", func() {
			_, err := config.ParseBandwidthAnnotation(`{"rate":"abc"}`)
			Expect(err).To(HaveOccurred())
		})

		It("preserves defaults for fields not specified", func() {
			cfg, err := config.ParseBandwidthAnnotation(`{"rate":"10M"}`)
			Expect(err).NotTo(HaveOccurred())
			Expect(cfg.HeavyHitterThreshold).To(Equal(config.DefaultConfig().HeavyHitterThreshold))
		})

		It("treats leading whitespace before { as JSON", func() {
			cfg, err := config.ParseBandwidthAnnotation(`   {"rate":"10M"}`)
			Expect(err).NotTo(HaveOccurred())
			Expect(cfg.Rate).To(Equal(int64(10_000_000 / 8)))
		})
	})
})

var _ = Describe("Config.Validate", func() {
	It("accepts the default config", func() {
		Expect(config.DefaultConfig().Validate()).To(Succeed())
	})

	It("rejects negative rate, burst, or threshold", func() {
		c := config.DefaultConfig()
		c.Rate = -1
		Expect(c.Validate()).To(HaveOccurred())

		c = config.DefaultConfig()
		c.Burst = -1
		Expect(c.Validate()).To(HaveOccurred())

		c = config.DefaultConfig()
		c.HeavyHitterThreshold = -1
		Expect(c.Validate()).To(HaveOccurred())
	})
})
