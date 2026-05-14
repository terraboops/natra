#!/usr/bin/env bash
# Full vm-rig run: up → install natra → topology test → (optional)
# teardown. Each phase is a separate script you can also run on its
# own.
#
# Env vars:
#   NATRA_VM_KEEP=1   leave the VMs up after tests for inspection.
#                     Default tears down on exit (success or fail)
#                     so a flaky run doesn't leak two VMs.

set -euo pipefail

REPO_ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
RIG_DIR="${REPO_ROOT}/scripts/vm-rig"

if [ "${NATRA_VM_KEEP:-0}" != "1" ]; then
    trap '"$RIG_DIR/down.sh" || true' EXIT
fi

bash "$RIG_DIR/up.sh"
bash "$RIG_DIR/install-natra.sh"
bash "$RIG_DIR/run-tests.sh"
