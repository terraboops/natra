#!/usr/bin/env bash
# Layer 5 driver. Originally intended to orchestrate veth + iperf inside
# an lvh VM; the actual perf tests now run BPF_PROG_RUN directly out of
# perf_linux_test.go and don't need shell orchestration. This script
# stays around as the Makefile entry point and as the place to add
# real-veth scenarios if/when we need them.

set -euo pipefail

KERNEL="${1:-unknown}"
echo "natra Layer 5 — kernel=${KERNEL} (BPF_PROG_RUN scenarios; see perf_linux_test.go)"
exit 0
