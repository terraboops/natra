#!/usr/bin/env bash
# Run a command inside a privileged Linux container with the natra repo
# mounted at /workspace. Used by Mac dev to execute Linux-only test runners
# (Layer 2: CNI protocol tests need network namespaces).
#
# Usage:
#   scripts/run-in-docker.sh <command> [args...]
#
# Environment:
#   NATRA_DOCKER_IMAGE  override the image (default: golang:1.27)

set -euo pipefail

IMAGE="${NATRA_DOCKER_IMAGE:-golang:1.27}"
REPO_ROOT="$(cd "$(dirname "$0")/.." && pwd)"

# Missing or stopped Docker exits 69 (EX_UNAVAILABLE) rather than 0:
# `make ci` and the per-layer targets would otherwise report layers
# that never ran as passing.
if ! command -v docker >/dev/null 2>&1; then
	cat >&2 <<'EOF'
Docker is not on PATH. This wrapper needs Docker (colima or Docker
Desktop on macOS, dockerd on Linux) to provide a Linux kernel for tests
that need network namespaces. See TODO_LINUX.md for alternatives.
EOF
	exit 69
fi

if ! docker info >/dev/null 2>&1; then
	cat >&2 <<'EOF'
Docker is installed but not responding. Start the daemon (colima start,
Docker Desktop, dockerd) and retry. See TODO_LINUX.md for alternatives.
EOF
	exit 69
fi

if [ "$#" -lt 1 ]; then
	echo "usage: $0 <command> [args...]" >&2
	exit 64
fi

# A checkout outside the Docker VM's shared paths mounts as an empty
# directory, and the command then fails with something unrelated (e.g.
# make's "No rule to make target"). Check for it up front.
MOUNT_CHECK='[ -f /workspace/go.mod ] || { echo "${NATRA_REPO_ROOT} is empty inside the container: the Docker VM does not share that path. colima mounts only \$HOME by default; move the checkout under \$HOME or add a mount in ~/.colima/default/colima.yaml." >&2; exit 69; }'

# --privileged is required because the test code creates real network
# namespaces inside the container (CAP_NET_ADMIN + access to /proc/self/ns/net
# manipulation that unprivileged containers don't get).
exec docker run --rm \
	--privileged \
	-v "${REPO_ROOT}:/workspace" \
	-w /workspace \
	-e CGO_ENABLED=0 \
	-e GOFLAGS="${GOFLAGS:-}" \
	-e NATRA_REPO_ROOT="${REPO_ROOT}" \
	"${IMAGE}" \
	bash -c "${MOUNT_CHECK}; $*"
