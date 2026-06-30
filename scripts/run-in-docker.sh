#!/usr/bin/env bash
# Run a command inside a privileged Linux container with the natra repo
# mounted at /workspace. Used by Mac dev to execute Linux-only test runners
# (Layer 2: CNI protocol tests need network namespaces).
#
# Usage:
#   scripts/run-in-docker.sh <command> [args...]
#
# Environment:
#   NATRA_DOCKER_IMAGE  override the image (default: golang:1.26)

set -euo pipefail

IMAGE="${NATRA_DOCKER_IMAGE:-golang:1.26}"
REPO_ROOT="$(cd "$(dirname "$0")/.." && pwd)"

if ! command -v docker >/dev/null 2>&1; then
	cat >&2 <<'EOF'
Docker is not on PATH. This wrapper needs Docker (Docker Desktop on macOS,
dockerd on Linux) to provide a Linux kernel for tests that need network
namespaces.

Install Docker Desktop from https://docker.com/products/docker-desktop and
restart this command. See TODO_LINUX.md for alternatives.
EOF
	exit 0
fi

if ! docker info >/dev/null 2>&1; then
	cat >&2 <<'EOF'
Docker is installed but not responding (daemon may be stopped). Start
Docker Desktop and retry. See TODO_LINUX.md for alternatives.
EOF
	exit 0
fi

if [ "$#" -lt 1 ]; then
	echo "usage: $0 <command> [args...]" >&2
	exit 64
fi

# --privileged is required because the test code creates real network
# namespaces inside the container (CAP_NET_ADMIN + access to /proc/self/ns/net
# manipulation that unprivileged containers don't get).
exec docker run --rm \
	--privileged \
	-v "${REPO_ROOT}:/workspace" \
	-w /workspace \
	-e CGO_ENABLED=0 \
	-e GOFLAGS="${GOFLAGS:-}" \
	"${IMAGE}" \
	bash -c "$*"
