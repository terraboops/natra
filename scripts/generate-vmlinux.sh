#!/usr/bin/env bash
# Generate vmlinux.h from kernel BTF
# Required for eBPF compilation

set -euo pipefail

echo "==> Generating vmlinux.h"

# Check BTF availability
if [ ! -f /sys/kernel/btf/vmlinux ]; then
    echo "❌ ERROR: /sys/kernel/btf/vmlinux not found"
    echo "BTF support required. Enable CONFIG_DEBUG_INFO_BTF in kernel config."
    exit 1
fi

# Check bpftool
if ! command -v bpftool &> /dev/null; then
    echo "❌ ERROR: bpftool not found"
    echo "Install bpftool to generate vmlinux.h"
    exit 1
fi

# Create headers directory
mkdir -p bpf/headers

# Generate vmlinux.h
echo "Generating vmlinux.h from kernel BTF..."
bpftool btf dump file /sys/kernel/btf/vmlinux format c > bpf/headers/vmlinux.h

echo "✓ Generated bpf/headers/vmlinux.h"
echo "  $(wc -l < bpf/headers/vmlinux.h) lines"
