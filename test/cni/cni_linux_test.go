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
	"errors"
	"runtime"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"golang.org/x/sys/unix"
)

func TestCNILinux(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "test/cni (Layer 2)")
}

var _ = BeforeSuite(func() {
	// Lock the test goroutine to its OS thread so netns operations
	// don't migrate mid-test and corrupt other tests' namespaces.
	runtime.LockOSThread()
	Expect(requireRoot()).To(Succeed())
	// Ensure /sys/fs/bpf is bpffs. The L2 test container starts with
	// /sys/fs/bpf as a regular directory under sysfs, which makes
	// link.Pin() return EINVAL because the kernel only accepts pin
	// targets on bpffs. Idempotent — a second mount on top is a no-op
	// in our scope (we don't unmount on cleanup; the container goes
	// away after the suite).
	if err := unix.Mount("bpffs", "/sys/fs/bpf", "bpf", 0, ""); err != nil && !errors.Is(err, unix.EBUSY) {
		Fail("mount bpffs at /sys/fs/bpf: " + err.Error())
	}
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

			// With no annotation, natra returns the (empty) PrevResult
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

		// tcx-hostside is the production default. The pin filename uses
		// a dotless suffix ("-link") because bpffs's bpf_lookup rejects
		// any path component containing a '.' on user-mounted
		// subdirectories (kernel/bpf/inode.c) — those names are
		// reserved for populate_bpffs's internal special files. An
		// earlier revision used `<id>-eth0.link` and got EPERM on
		// every Pin call; same shape applies to map pins (`-map`
		// suffix). Test asserts the happy path end-to-end.
		It("default mode attaches ingress via tcx-hostside and pins the link to bpffs", func() {
			By("creating a veth pair end inside the pod netns")
			ns, cleanup, err := newTestNetnsWithVeth("eth0")
			Expect(err).NotTo(HaveOccurred())
			defer cleanup()

			containerID := "test-attach-tcx-hostside"
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

			stdout, stderr, runErr := runPlugin(natra, "ADD", containerID, netnsPath(ns), "eth0", stdin)
			Expect(runErr).NotTo(HaveOccurred(), "stderr: %s", string(stderr))

			var result map[string]any
			Expect(json.Unmarshal(stdout, &result)).To(Succeed())
			Expect(result["cniVersion"]).To(Equal("1.0.0"))

			Expect(string(stderr)).To(ContainSubstring("attached hostside/ingress"))
			Expect(string(stderr)).To(ContainSubstring("rate=10000000"))
			Expect(linkPinExists(containerID, "hostside", "ingress")).To(BeTrue(),
				"tcx-hostside ingress attached but no link pin found")
			Expect(linkPinExists(containerID, "hostside", "egress")).To(BeFalse(),
				"egress was not annotated; no egress pin should exist")

			_, delStderr, delErr := runPlugin(natra, "DEL", containerID, netnsPath(ns), "eth0", stdin)
			Expect(delErr).NotTo(HaveOccurred(), "stderr: %s", string(delStderr))
			Expect(anyLinkPinExists(containerID)).To(BeFalse(),
				"link pin should be gone after DEL")
			Expect(remainingPinsFor(containerID)).To(BeEmpty(),
				"all per-container pins should be cleaned up by DEL")
		})

		It("tcx-podside attaches ingress to pod-side eth0 and pins", func() {
			By("creating a veth pair end inside the pod netns")
			ns, cleanup, err := newTestNetnsWithVeth("eth0")
			Expect(err).NotTo(HaveOccurred())
			defer cleanup()

			containerID := "test-attach-tcx-podside"
			stdin := []byte(`{
				"cniVersion": "1.0.0",
				"name": "natra-test",
				"type": "natra",
				"attachMode": "tcx-podside",
				"runtimeConfig": {
					"podAnnotations": {
						"kubernetes.io/ingress-bandwidth": "10M"
					}
				}
			}`)

			stdout, stderr, runErr := runPlugin(natra, "ADD", containerID, netnsPath(ns), "eth0", stdin)
			Expect(runErr).NotTo(HaveOccurred(), "stderr: %s", string(stderr))

			var result map[string]any
			Expect(json.Unmarshal(stdout, &result)).To(Succeed())
			Expect(result["cniVersion"]).To(Equal("1.0.0"))

			Expect(string(stderr)).To(ContainSubstring("attached podside/ingress"))
			Expect(linkPinExists(containerID, "podside", "ingress")).To(BeTrue(),
				"tcx-podside ingress attached but no link pin found")
			Expect(linkPinExists(containerID, "hostside", "ingress")).To(BeFalse(),
				"explicit podside mode must not pin host-side")

			_, _, delErr := runPlugin(natra, "DEL", containerID, netnsPath(ns), "eth0", stdin)
			Expect(delErr).NotTo(HaveOccurred())
			Expect(remainingPinsFor(containerID)).To(BeEmpty())
		})

		It("attaches via clsact-podside fallback when explicitly requested", func() {
			By("creating a veth pair end inside the pod netns")
			ns, cleanup, err := newTestNetnsWithVeth("eth0")
			Expect(err).NotTo(HaveOccurred())
			defer cleanup()

			containerID := "test-attach-clsact-podside"
			stdin := []byte(`{
				"cniVersion": "1.0.0",
				"name": "natra-test",
				"type": "natra",
				"attachMode": "clsact-podside",
				"runtimeConfig": {
					"podAnnotations": {
						"kubernetes.io/ingress-bandwidth": "10M"
					}
				}
			}`)

			stdout, stderr, runErr := runPlugin(natra, "ADD", containerID, netnsPath(ns), "eth0", stdin)
			Expect(runErr).NotTo(HaveOccurred(), "stderr: %s", string(stderr))

			Expect(string(stderr)).To(ContainSubstring("attached podside/ingress"))

			var result map[string]any
			Expect(json.Unmarshal(stdout, &result)).To(Succeed())
			Expect(result["cniVersion"]).To(Equal("1.0.0"))

			// No tcx link pin in clsact mode (the kernel holds the program
			// reference via the qdisc tree until the veth is deleted).
			Expect(anyLinkPinExists(containerID)).To(BeFalse(),
				"clsact-podside mode should not produce a link pin")
		})

		It("attaches via clsact-hostside when explicitly requested", func() {
			By("creating a veth pair end inside the pod netns")
			ns, cleanup, err := newTestNetnsWithVeth("eth0")
			Expect(err).NotTo(HaveOccurred())
			defer cleanup()

			containerID := "test-attach-clsact-hostside"
			stdin := []byte(`{
				"cniVersion": "1.0.0",
				"name": "natra-test",
				"type": "natra",
				"attachMode": "clsact-hostside",
				"runtimeConfig": {
					"podAnnotations": {
						"kubernetes.io/ingress-bandwidth": "10M"
					}
				}
			}`)

			stdout, stderr, runErr := runPlugin(natra, "ADD", containerID, netnsPath(ns), "eth0", stdin)
			Expect(runErr).NotTo(HaveOccurred(), "stderr: %s", string(stderr))

			Expect(string(stderr)).To(ContainSubstring("attached hostside/ingress"))

			var result map[string]any
			Expect(json.Unmarshal(stdout, &result)).To(Succeed())
			Expect(result["cniVersion"]).To(Equal("1.0.0"))

			// clsact: kernel holds the program via the qdisc tree, no
			// bpffs link pin written by natra in this mode.
			Expect(anyLinkPinExists(containerID)).To(BeFalse(),
				"clsact-hostside mode should not produce a link pin")
		})

		It("default mode attaches egress via tcx-hostside with only egress-bandwidth annotated", func() {
			By("creating a veth pair end inside the pod netns")
			ns, cleanup, err := newTestNetnsWithVeth("eth0")
			Expect(err).NotTo(HaveOccurred())
			defer cleanup()

			containerID := "test-attach-egress-only"
			stdin := []byte(`{
				"cniVersion": "1.0.0",
				"name": "natra-test",
				"type": "natra",
				"runtimeConfig": {
					"podAnnotations": {
						"kubernetes.io/egress-bandwidth": "10M"
					}
				}
			}`)

			stdout, stderr, runErr := runPlugin(natra, "ADD", containerID, netnsPath(ns), "eth0", stdin)
			Expect(runErr).NotTo(HaveOccurred(), "stderr: %s", string(stderr))

			var result map[string]any
			Expect(json.Unmarshal(stdout, &result)).To(Succeed())
			Expect(result["cniVersion"]).To(Equal("1.0.0"))

			Expect(string(stderr)).To(ContainSubstring("attached hostside/egress"))
			Expect(string(stderr)).To(ContainSubstring("rate=10000000"))
			Expect(linkPinExists(containerID, "hostside", "egress")).To(BeTrue(),
				"tcx-hostside egress attached but no link pin found")
			Expect(linkPinExists(containerID, "hostside", "ingress")).To(BeFalse(),
				"ingress was not annotated; no ingress pin should exist")

			_, delStderr, delErr := runPlugin(natra, "DEL", containerID, netnsPath(ns), "eth0", stdin)
			Expect(delErr).NotTo(HaveOccurred(), "stderr: %s", string(delStderr))
			Expect(remainingPinsFor(containerID)).To(BeEmpty(),
				"all per-container pins should be cleaned up by DEL")
		})

		It("default mode attaches both directions when both annotations are present", func() {
			By("creating a veth pair end inside the pod netns")
			ns, cleanup, err := newTestNetnsWithVeth("eth0")
			Expect(err).NotTo(HaveOccurred())
			defer cleanup()

			containerID := "test-attach-bidi"
			stdin := []byte(`{
				"cniVersion": "1.0.0",
				"name": "natra-test",
				"type": "natra",
				"runtimeConfig": {
					"podAnnotations": {
						"kubernetes.io/ingress-bandwidth": "10M",
						"kubernetes.io/egress-bandwidth": "5M"
					}
				}
			}`)

			stdout, stderr, runErr := runPlugin(natra, "ADD", containerID, netnsPath(ns), "eth0", stdin)
			Expect(runErr).NotTo(HaveOccurred(), "stderr: %s", string(stderr))

			var result map[string]any
			Expect(json.Unmarshal(stdout, &result)).To(Succeed())
			Expect(result["cniVersion"]).To(Equal("1.0.0"))

			Expect(string(stderr)).To(ContainSubstring("attached hostside/ingress"))
			Expect(string(stderr)).To(ContainSubstring("attached hostside/egress"))
			Expect(linkPinExists(containerID, "hostside", "ingress")).To(BeTrue(),
				"both annotations present but ingress pin missing")
			Expect(linkPinExists(containerID, "hostside", "egress")).To(BeTrue(),
				"both annotations present but egress pin missing")

			_, delStderr, delErr := runPlugin(natra, "DEL", containerID, netnsPath(ns), "eth0", stdin)
			Expect(delErr).NotTo(HaveOccurred(), "stderr: %s", string(delStderr))
			Expect(remainingPinsFor(containerID)).To(BeEmpty(),
				"DEL must clean up both direction pins")
		})

		It("attaches nothing when neither annotation is present", func() {
			By("creating a veth pair end inside the pod netns")
			ns, cleanup, err := newTestNetnsWithVeth("eth0")
			Expect(err).NotTo(HaveOccurred())
			defer cleanup()

			containerID := "test-attach-neither"
			// No bandwidth annotations on the pod. natra should not
			// load BPF, not attach anything, not pin anything.
			stdin := []byte(`{
				"cniVersion": "1.0.0",
				"name": "natra-test",
				"type": "natra"
			}`)

			stdout, stderr, runErr := runPlugin(natra, "ADD", containerID, netnsPath(ns), "eth0", stdin)
			Expect(runErr).NotTo(HaveOccurred(), "stderr: %s", string(stderr))

			var result map[string]any
			Expect(json.Unmarshal(stdout, &result)).To(Succeed())
			Expect(result["cniVersion"]).To(Equal("1.0.0"))

			Expect(string(stderr)).NotTo(ContainSubstring("attached"),
				"no annotation should mean no attachment")
			Expect(remainingPinsFor(containerID)).To(BeEmpty(),
				"unannotated pod must not produce any pins")
		})

		It("rejects an unknown attachMode at config-parse time", func() {
			ns, cleanup, err := newTestNetns()
			Expect(err).NotTo(HaveOccurred())
			defer cleanup()

			stdin := []byte(`{
				"cniVersion": "1.0.0",
				"name": "natra-test",
				"type": "natra",
				"attachMode": "nonsense",
				"runtimeConfig": {
					"podAnnotations": {
						"kubernetes.io/ingress-bandwidth": "10M"
					}
				}
			}`)

			stdout, _, runErr := runPlugin(natra, "ADD", "test-bad-attach-mode", netnsPath(ns), "eth0", stdin)
			Expect(runErr).To(HaveOccurred(),
				"plugin should exit non-zero on unknown attachMode")
			// skel writes the CNI-spec error reply (with msg) to stdout
			// as JSON, not stderr.
			Expect(string(stdout)).To(ContainSubstring("attachMode"))
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
		It("succeeds (CHECK is a best-effort no-op)", func() {
			ns, cleanup, err := newTestNetns()
			Expect(err).NotTo(HaveOccurred())
			defer cleanup()

			stdin := []byte(`{"cniVersion":"1.0.0","name":"natra-test","type":"natra"}`)

			_, stderr, runErr := runPlugin(natra, "CHECK", "test-check", netnsPath(ns), "eth0", stdin)
			Expect(runErr).NotTo(HaveOccurred(), "stderr: %s", string(stderr))
		})
	})
})
