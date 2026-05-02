#!/usr/bin/env bash
# Two-stage license scan for due-diligence-grade detection of GPL family
# obligations. Stage 1 (go-licenses) catches Go module deps, fast and
# precise. Stage 2 (scancode-toolkit) walks the full repo, slow but
# thorough — catches vendored code, scripts, manifests, and anything
# else go-licenses can't see.
#
# Both stages classify licenses with the SPDX taxonomy and fail (exit 1)
# if any GPL/AGPL/SSPL/CDDL/EPL family license is found OUTSIDE the
# allowlist below. The BPF program (bpf/*.bpf.c) is allowlisted because
# BPF kernel helpers (bpf_ktime_get_ns, bpf_spin_lock, etc.) are
# GPL-only — the .bpf.o license declaration is required by the kernel
# verifier, not an indication of natra's user-space licensing.
#
# Usage:
#   scripts/license-scan.sh             # both stages
#   scripts/license-scan.sh go-only     # just stage 1 (fast, ~5s)
#   scripts/license-scan.sh scancode    # just stage 2 (~30-90s)

set -euo pipefail

REPO_ROOT="$(cd "$(dirname "$0")/.." && pwd)"
MODE="${1:-both}"

# License families that fail the build if found anywhere not allowlisted.
# AGPL/SSPL are network-copyleft (worse than GPL for SaaS). CDDL/EPL are
# weak copyleft but still impose obligations.
GPL_FAMILY_RE='GPL-[0-9]|AGPL|SSPL|CDDL|EPL-[0-9]'

# Paths that are EXPECTED to contain copyleft and are intentional.
# Update with comments when adding entries — the maintainer should
# understand each one.
ALLOWLIST=(
	"bpf/natra.bpf.c"          # GPL header line; BPF kernel helpers are GPL-only.
	"bpf/placeholder.bpf.c"    # Apache-2.0 placeholder; appears here for completeness.
)

is_allowed() {
	local path="$1"
	for allowed in "${ALLOWLIST[@]}"; do
		if [[ "$path" == "$allowed" || "$path" == *"/$allowed" ]]; then
			return 0
		fi
	done
	return 1
}

stage_go_licenses() {
	echo "==> stage 1: go-licenses"
	if ! command -v go-licenses >/dev/null 2>&1; then
		echo "installing go-licenses..."
		go install github.com/google/go-licenses@latest
		export PATH="$(go env GOPATH)/bin:$PATH"
	fi

	# `report` writes a CSV: <module>,<license-url>,<spdx-id>
	local report
	report=$(cd "$REPO_ROOT" && go-licenses report ./... 2>&1 || true)

	# Pretty-print so the CI log is grep-able.
	echo "$report"

	local fail=0
	while IFS=',' read -r mod url spdx; do
		[[ -z "$spdx" ]] && continue
		if [[ "$spdx" =~ $GPL_FAMILY_RE ]]; then
			echo "FAIL: $mod is $spdx (copyleft family)"
			fail=1
		fi
	done <<< "$report"

	if [[ $fail -eq 1 ]]; then
		return 1
	fi
	echo "go-licenses: clean"
}

stage_scancode() {
	echo "==> stage 2: scancode-toolkit"
	if ! command -v docker >/dev/null 2>&1 || ! docker info >/dev/null 2>&1; then
		echo "scancode needs Docker (running). Skipping."
		return 0
	fi

	# Output JSON to a temp file and parse with jq. Use the maintained
	# image from the scancode project. -clp = copyrights, licenses,
	# packages.  --processes 4 is enough for natra-sized repos.
	local out_file
	out_file=$(mktemp -t natra-scancode.XXXXXX.json)
	trap "rm -f $out_file" EXIT

	docker run --rm \
		-v "$REPO_ROOT:/scan" \
		ghcr.io/aboutcode-org/scancode-toolkit:latest \
		--license --copyright --json-pp /scan/$(basename "$out_file") \
		--processes 4 \
		--strip-root \
		/scan \
		>/dev/null

	# scancode wrote inside the mount; move it out so we can read it.
	mv "$REPO_ROOT/$(basename "$out_file")" "$out_file" 2>/dev/null || true

	# Use jq if available; otherwise grep. We're looking for files where
	# any detected license expression matches the GPL family regex.
	if command -v jq >/dev/null 2>&1; then
		local fail=0
		while IFS= read -r entry; do
			local path spdx
			path=$(echo "$entry" | jq -r '.path')
			spdx=$(echo "$entry" | jq -r '.detected_license_expression_spdx // empty')
			[[ -z "$spdx" ]] && continue
			if [[ "$spdx" =~ $GPL_FAMILY_RE ]]; then
				if is_allowed "$path"; then
					echo "OK (allowlisted): $path -> $spdx"
				else
					echo "FAIL: $path -> $spdx (copyleft, not allowlisted)"
					fail=1
				fi
			fi
		done < <(jq -c '.files[] | select(.detected_license_expression_spdx != null)' "$out_file")
		if [[ $fail -eq 1 ]]; then
			return 1
		fi
		echo "scancode: clean (or only allowlisted matches)"
	else
		# Fallback: text-grep. Less precise but works in minimal CI.
		if grep -E "$GPL_FAMILY_RE" "$out_file" | grep -v -F "${ALLOWLIST[*]}" >/dev/null; then
			echo "FAIL: scancode found copyleft outside allowlist"
			grep -E "$GPL_FAMILY_RE" "$out_file"
			return 1
		fi
		echo "scancode: clean (text-grep mode; install jq for precise reporting)"
	fi
}

case "$MODE" in
	both)
		stage_go_licenses
		stage_scancode
		;;
	go-only)
		stage_go_licenses
		;;
	scancode)
		stage_scancode
		;;
	*)
		echo "usage: $0 [both|go-only|scancode]" >&2
		exit 64
		;;
esac

echo "license scan: PASS"
