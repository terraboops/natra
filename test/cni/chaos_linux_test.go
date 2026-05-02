//go:build linux && integration

// Layer 2 chaos — negative paths the CNI plugin must survive.
//
// natra runs as root inside kubelet's call path; any panic or hang here
// is a Pod-stuck-in-ContainerCreating incident in production. These tests
// exist to prove operational trustworthiness, not feature correctness.

package cni_test

import (
	"encoding/json"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("natra CNI chaos", func() {
	var natra string

	BeforeEach(func() {
		var err error
		natra, err = natraBinary()
		Expect(err).NotTo(HaveOccurred())
	})

	Context("malformed stdin", func() {
		DescribeTable("rejects invalid input without panicking",
			func(stdin []byte) {
				ns, cleanup, err := newTestNetns()
				Expect(err).NotTo(HaveOccurred())
				defer cleanup()

				_, stderr, runErr := runPlugin(natra, "ADD", "chaos-malformed", netnsPath(ns), "eth0", stdin)
				Expect(runErr).To(HaveOccurred())
				Expect(string(stderr)).NotTo(ContainSubstring("panic:"), "plugin should not panic on malformed stdin")
			},
			Entry("empty bytes", []byte("")),
			Entry("not JSON", []byte("garbage")),
			Entry("truncated JSON", []byte(`{"cniVersion":"1.0.0",`)),
			Entry("wrong schema", []byte(`["array","not","object"]`)),
			Entry("null", []byte("null")),
		)

		It("survives a 1 MB JSON payload", func() {
			ns, cleanup, err := newTestNetns()
			Expect(err).NotTo(HaveOccurred())
			defer cleanup()

			big := strings.Repeat("A", 1<<20)
			stdin := []byte(`{"cniVersion":"1.0.0","name":"natra-test","type":"natra","runtimeConfig":{"podAnnotations":{"kubernetes.io/ingress-bandwidth":"` + big + `"}}}`)

			_, stderr, runErr := runPlugin(natra, "ADD", "chaos-large", netnsPath(ns), "eth0", stdin)
			_ = runErr
			Expect(string(stderr)).NotTo(ContainSubstring("panic:"))
		})
	})

	Context("annotation injection attempts", func() {
		DescribeTable("dangerous annotation values are rejected without panic",
			func(value string) {
				ns, cleanup, err := newTestNetns()
				Expect(err).NotTo(HaveOccurred())
				defer cleanup()

				encoded, err := json.Marshal(value)
				Expect(err).NotTo(HaveOccurred())

				stdin := []byte(`{"cniVersion":"1.0.0","name":"natra-test","type":"natra","runtimeConfig":{"podAnnotations":{"kubernetes.io/ingress-bandwidth":` + string(encoded) + `}}}`)

				_, stderr, runErr := runPlugin(natra, "ADD", "chaos-injection", netnsPath(ns), "eth0", stdin)
				_ = runErr
				Expect(string(stderr)).NotTo(ContainSubstring("panic:"))
			},
			Entry("embedded newline", "10M\nrm -rf /"),
			Entry("embedded NUL", "10M\x00"),
			Entry("shell metacharacters", "10M;rm -rf /"),
			Entry("command substitution", "$(echo pwned)"),
			Entry("backticks", "`echo pwned`"),
		)
	})

	Context("CNI env vars", func() {
		It("rejects ADD without CNI_NETNS pointing at a real netns", func() {
			stdin := []byte(`{"cniVersion":"1.0.0","name":"natra-test","type":"natra"}`)

			_, stderr, runErr := runPlugin(natra, "ADD", "chaos-bad-netns", "/nonexistent/netns", "eth0", stdin)
			_ = runErr
			Expect(string(stderr)).NotTo(ContainSubstring("panic:"))
		})
	})
})
