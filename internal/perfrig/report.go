package perfrig

// Report is the assembled output of one Executor run. The shape is
// substrate-independent: same fields whether the run came from
// limaSubstrate or k3dSubstrate, so the writer can produce one
// table format consumed by both rigs' docs.
type Report struct {
	Substrate string // "vm-rig" / "k3d" / "fake" — only for the header
	Profile   string // "full" / "ci" — same
	Phases    []PhaseReport
}

// PhaseReport holds every workload's results within one phase
// (baseline / vanilla / natra). One per phase per run.
type PhaseReport struct {
	Phase     Phase
	Workloads []WorkloadReport
}

// WorkloadReport is one workload's measurements within one phase.
// Sample slices have len == Plan.Samples once measured. The
// meaning of each slice is workload-specific:
//
//   - iperfSweep: IperfCells, one per (rate × direction × kind).
//   - mixed:      MixedSamples.
//   - memory:     MemorySamples.
//
// The other slices stay empty for workloads that don't use them.
// This keeps the report a flat, easily-serialized shape; a future
// move to per-workload report types is straightforward if the
// surface area grows.
type WorkloadReport struct {
	Kind          WorkloadKind
	IperfCells    []IperfCell
	MixedSamples  []MixedSample
	MemorySamples []MemorySample
}

// IperfCell is one (rate × direction × elephant|mice) measurement,
// one entry per sample. Bps is the receiver-side bits/sec from
// iperf3's `end.sum_received`.
type IperfCell struct {
	Rate      Rate
	Direction string // "ingress" / "egress"
	Kind      string // "elephant" / "mice"
	Sample    int
	Bps       float64
}

// MixedSample is one sample of the mixed workload (annotated
// elephant + hey mice + bystander).
type MixedSample struct {
	Sample int

	// Iperf throughput of the annotated elephant (bps, both
	// directions from the --bidir run).
	IperfIngressBps float64
	IperfEgressBps  float64

	// hey results from the annotated server pod (CMS fast-pass).
	PodRPS, PodP50, PodP99 float64

	// hey results from the unannotated bystander (collateral cost).
	BystanderRPS, BystanderP50, BystanderP99 float64
}

// MemorySample is one sample of the three memory comparables —
// dataplane kernel memory at 1 pod and at N pods (so the slope is
// derivable downstream), plus the per-invocation peak and the
// persistent installer DS RSS.
type MemorySample struct {
	Sample int

	// DataplaneKmemBytes1 is the node kernel memory delta
	// (Slab+KernelStack+PageTables, per /proc/meminfo) attributable
	// to one annotated pod. Always measured; corroborating numbers
	// below are mechanism-specific.
	DataplaneKmemBytes1 int64
	DataplaneKmemBytesN int64 // same metric, with N=8 pods (see executor.go MemoryNPodCount)

	// VanillaQdiscBytes is `tc -s qdisc show` reported byte counts
	// summed across the per-pod TBF qdiscs at N pods. Corroborates
	// the kernel-mem delta in the vanilla phase only; zero in
	// baseline/natra.
	VanillaQdiscBytes int64

	// NatraBpfMemlockBytes is the bpftool-reported byte-exact BPF
	// memlock of all natra_* programs and maps at N pods. Zero in
	// baseline/vanilla; the byte-exact corroboration of the
	// kernel-mem delta in the natra phase.
	NatraBpfMemlockBytes int64

	// PluginInvokePeakRSSBytes is the per-CNI-ADD peak RSS
	// (`ru_maxrss`) of the phase's plugin binary, measured by
	// invoking it on the worker node in a controlled harness with
	// a representative CNI ADD stdin/env. Zero in baseline.
	PluginInvokePeakRSSBytes int64

	// InstallerDSBytes is the persistent userspace RSS (cgroup
	// memory.current) of the phase's installer DaemonSet pod on
	// the worker node. Zero in baseline.
	InstallerDSBytes int64

	// NPodCount is the value of N used for the @N measurements.
	// Recorded with the sample so a future change to N is visible
	// in archived results.
	NPodCount int
}
