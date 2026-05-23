#!/usr/bin/env bash
# perf-vs-vanilla: the k3d driver for the natra-vs-upstream-
# bandwidth comparison. Thin wrapper now — the actual work lives
# in `cmd/perfrig` (substrate-agnostic executor + k3dSubstrate),
# the same code path the vm-rig (lima) takes via `cmd/vm-rig
# perfvsvanilla`. "k3d ⊂ vm-rig" is enforced by an Apply()
# subset-validation test in internal/perfrig.
#
# Profile is `ci` by default (single rate 10M, one sample) for
# fast feedback; override with PERF_PROFILE=full for the full
# rate sweep + Samples=3.
#
# CI: `.github/workflows/perf.yml` (and any local CI cron) invokes
# `make perf-vs-vanilla`, which calls this script. No CI workflow
# changes are needed for the substrate consolidation.
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "$0")/.." && pwd)"
PROFILE="${PERF_PROFILE:-ci}"
CLUSTER="${PERF_CLUSTER:-natra-perfrig}"
NATRA_IMAGE="${PERF_NATRA_IMAGE:-ghcr.io/terraboops/natra:perfrig}"
PERFCLIENT_IMAGE="${PERF_PERFCLIENT_IMAGE:-ghcr.io/terraboops/natra-perfclient:perfrig}"

# Required tools — surface a clear error here rather than letting
# cmd/perfrig fail mid-phase.
for tool in docker k3d kubectl; do
    command -v "$tool" >/dev/null 2>&1 || {
        echo "missing required tool: $tool" >&2
        exit 1
    }
done

cd "$REPO_ROOT"
exec go run ./cmd/perfrig \
    --substrate=k3d \
    --profile="$PROFILE" \
    --cluster="$CLUSTER" \
    --image="$NATRA_IMAGE" \
    --perfclient-image="$PERFCLIENT_IMAGE"
