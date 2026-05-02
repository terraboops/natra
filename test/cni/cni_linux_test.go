//go:build linux && integration

// Layer 2 — CNI protocol happy-path tests.
//
// Exercises natra's CNI ADD/DEL/CHECK verbs by exec'ing the built binary
// with canonical CNI env vars and stdin, then asserting on stdout JSON
// shape and side effects in a throwaway network namespace. Matches the
// way kubelet invokes CNI plugins, so any arg-parsing or stdin-handling
// regression shows up here.

package cni_test

import (
	"encoding/json"
	"runtime"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

func TestCNILinux(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "test/cni (Layer 2)")
}

var _ = BeforeSuite(func() {
	// Lock the test goroutine to its OS thread so netns operations don't
	// migrate mid-test and corrupt other tests' namespaces.
	runtime.LockOSThread()
	Expect(requireRoot()).To(Succeed())
})

var _ = Describe("natra CNI binary", func() {
	var natra string

	BeforeEach(func() {
		var err error
		natra, err = natraBinary()
		Expect(err).NotTo(HaveOccurred())
	})

	Context("ADD with no bandwidth annotation", func() {
		It("passes through with a valid CNI 1.0.0 result", func() {
			ns, cleanup, err := newTestNetns()
			Expect(err).NotTo(HaveOccurred())
			defer cleanup()

			stdin := []byte(`{
				"cniVersion": "1.0.0",
				"name": "natra-test",
				"type": "natra"
			}`)

			stdout, stderr, runErr := runPlugin(natra, "ADD", "test-no-annotation", netnsPath(ns), "eth0", stdin)
			Expect(runErr).NotTo(HaveOccurred(), "stderr: %s", string(stderr))

			// Phase 0: with no annotation, natra returns the (empty) PrevResult
			// directly. Just assert it's parseable JSON with cniVersion.
			var result map[string]any
			Expect(json.Unmarshal(stdout, &result)).To(Succeed())
			Expect(result["cniVersion"]).To(Equal("1.0.0"))
		})
	})

	Context("ADD with simple bandwidth annotation", func() {
		It("attempts BPF attach and fail-opens when interface absent", func() {
			ns, cleanup, err := newTestNetns()
			Expect(err).NotTo(HaveOccurred())
			defer cleanup()

			stdin := []byte(`{
				"cniVersion": "1.0.0",
				"name": "natra-test",
				"type": "natra",
				"runtimeConfig": {
					"podAnnotations": {
						"kubernetes.io/ingress-bandwidth": "10M"
					}
				}
			}`)

			stdout, stderr, runErr := runPlugin(natra, "ADD", "test-annotation", netnsPath(ns), "eth0", stdin)
			Expect(runErr).NotTo(HaveOccurred(), "stderr: %s", string(stderr))

			// natra entered the empty test netns, didn't find eth0, logged
			// the BPF-attach failure, and fell through to passthrough. The
			// fail-open path is the design contract — pod startup must not
			// block on rate-limit setup.
			Expect(string(stderr)).To(ContainSubstring("BPF attach failed"))
			Expect(string(stderr)).To(ContainSubstring("passing through unrate-limited"))

			var result map[string]any
			Expect(json.Unmarshal(stdout, &result)).To(Succeed())
			Expect(result["cniVersion"]).To(Equal("1.0.0"))
		})

		It("attaches the BPF program when the target interface exists in the pod netns", func() {
			By("creating a veth pair end inside the pod netns")
			ns, cleanup, err := newTestNetnsWithVeth("eth0")
			Expect(err).NotTo(HaveOccurred())
			defer cleanup()

			stdin := []byte(`{
				"cniVersion": "1.0.0",
				"name": "natra-test",
				"type": "natra",
				"runtimeConfig": {
					"podAnnotations": {
						"kubernetes.io/ingress-bandwidth": "10M"
					}
				}
			}`)

			stdout, stderr, runErr := runPlugin(natra, "ADD", "test-attach-success", netnsPath(ns), "eth0", stdin)
			Expect(runErr).NotTo(HaveOccurred(), "stderr: %s", string(stderr))

			// Success path: stderr reports the attach with parsed rate
			// (10M = 10_000_000 bytes/sec) and a non-zero ifindex.
			Expect(string(stderr)).To(ContainSubstring("attached to eth0"))
			Expect(string(stderr)).To(ContainSubstring("rate=10000000"))

			var result map[string]any
			Expect(json.Unmarshal(stdout, &result)).To(Succeed())
			Expect(result["cniVersion"]).To(Equal("1.0.0"))
		})
	})

	Context("DEL", func() {
		It("succeeds even with no prior ADD (CNI idempotency requirement)", func() {
			ns, cleanup, err := newTestNetns()
			Expect(err).NotTo(HaveOccurred())
			defer cleanup()

			stdin := []byte(`{"cniVersion":"1.0.0","name":"natra-test","type":"natra"}`)

			_, stderr, runErr := runPlugin(natra, "DEL", "test-del", netnsPath(ns), "eth0", stdin)
			Expect(runErr).NotTo(HaveOccurred(), "stderr: %s", string(stderr))
		})
	})

	Context("CHECK", func() {
		It("succeeds in Phase 0 (no BPF state to verify yet)", func() {
			ns, cleanup, err := newTestNetns()
			Expect(err).NotTo(HaveOccurred())
			defer cleanup()

			stdin := []byte(`{"cniVersion":"1.0.0","name":"natra-test","type":"natra"}`)

			_, stderr, runErr := runPlugin(natra, "CHECK", "test-check", netnsPath(ns), "eth0", stdin)
			Expect(runErr).NotTo(HaveOccurred(), "stderr: %s", string(stderr))
		})
	})
})
