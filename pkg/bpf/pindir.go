package bpf

// PinDir holds the bpffs paths for natra's per-pod tcx-link pins and
// per-pod map pins. A dedicated subdir keeps natra's pins separate from
// other tooling on the node and makes cleanup straightforward (DEL
// removes the per-container files). Shared by the CNI plugin, which
// writes the pins, and natra-tools, which reads them.
const PinDir = "/sys/fs/bpf/natra"
