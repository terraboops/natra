#!/usr/bin/env bash
# Verify kernel version and tcx support
# Checks if system meets requirements for Natra

set -euo pipefail

echo "==> Verifying Kernel Requirements"
echo ""

# Check kernel version
KERNEL_VERSION=$(uname -r | cut -d'-' -f1)
MAJOR=$(echo "$KERNEL_VERSION" | cut -d'.' -f1)
MINOR=$(echo "$KERNEL_VERSION" | cut -d'.' -f2)

echo "Kernel version: $KERNEL_VERSION"

# Minimum: kernel 5.x for TC BPF support
if [ "$MAJOR" -lt 5 ]; then
    echo "❌ ERROR: Kernel version too old (need 5.x+)"
    exit 1
fi

echo "✓ Kernel meets minimum requirements (5.x+ for TC BPF)"

# Check for tcx support (kernel 6.6+)
if [ "$MAJOR" -gt 6 ] || ([ "$MAJOR" -eq 6 ] && [ "$MINOR" -ge 6 ]); then
    echo "✓ tcx support available (kernel 6.6+)"
    echo "  → Will use tcx for qdisc-less BPF attachment"
else
    echo "⚠ tcx not available (kernel < 6.6)"
    echo "  → Will fall back to clsact attachment"
    echo "  → Consider upgrading to kernel 6.6+ for full tcx support"
fi

# Check for bpftool
if command -v bpftool &> /dev/null; then
    echo "✓ bpftool found: $(bpftool version)"
else
    echo "⚠ bpftool not found - install for development"
fi

# Check for BTF support
if [ -f /sys/kernel/btf/vmlinux ]; then
    echo "✓ BTF available (/sys/kernel/btf/vmlinux)"
else
    echo "❌ ERROR: BTF not available - cannot generate vmlinux.h"
    exit 1
fi

echo ""
echo "✅ System meets Natra requirements"
