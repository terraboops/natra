#!/usr/bin/env bash
# Layer 5 driver. Inside an lvh VM:
#   1. Build natra (this repo) and the upstream
#      containernetworking/plugins/bandwidth plugin.
#   2. Set up two veth pairs in throwaway netns; attach natra to one,
#      vanilla bandwidth to the other.
#   3. Run scenarios (one_elephant / thousand_mice / mixed) against both.
#   4. Emit JSON metrics; perf_linux_test.go diffs against baselines/.
#
# Phase 0: stub. Phase 1 fills in the real orchestration alongside BPF code.

set -euo pipefail

KERNEL="${1:-unknown}"
echo "natra Layer 5 — kernel=${KERNEL} (Phase 0 stub: harness scaffolded, scenarios deferred to Phase 1)"
exit 0
