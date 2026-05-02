#!/usr/bin/env bash
# Layer 3 entry point. Boots a qemu VM via lvh at the requested kernel,
# mounts the natra repo at /workspace, and runs the in-VM test runner.
#
# Usage:
#   test/bpf/run-in-vm.sh [KERNEL]
#
# Default KERNEL: 6.6.
#
# Prerequisites: lvh, qemu-system-x86_64, KVM (`/dev/kvm` accessible).
# On macOS: this won't work directly — see TODO_LINUX.md §"Layer 3 local
# on Mac" for the lima/orbstack escape hatch.

set -euo pipefail

KERNEL="${1:-6.6}"
REPO_ROOT="$(cd "$(dirname "$0")/../.." && pwd)"

if ! command -v lvh >/dev/null 2>&1; then
	echo "lvh not found on PATH. Install: go install github.com/cilium/little-vm-helper/cmd/lvh@latest" >&2
	exit 127
fi

if [ ! -e /dev/kvm ]; then
	echo "/dev/kvm not present — Layer 3 needs KVM. See TODO_LINUX.md." >&2
	exit 1
fi

exec lvh run \
	--image "quay.io/lvh-images/kind:${KERNEL}" \
	--mount "${REPO_ROOT}:/workspace" \
	-- /workspace/test/bpf/in-vm-runner.sh "${KERNEL}"
