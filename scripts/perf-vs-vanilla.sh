#!/usr/bin/env bash
# Real-cluster head-to-head: baseline (no plugin) vs natra vs upstream
# containernetworking/plugins/bandwidth.
#
# Spins up three k3d clusters in sequence (k3s in Docker; same shape
# as L4 e2e and the soak rig, standardized on k3d for the project):
#   - Phase 0: flannel alone, no rate-limiting plugin (baseline floor)
#   - Phase A: flannel + natra chained
#   - Phase B: flannel + upstream bandwidth chained
#
# Two workloads per cluster:
#
#   1. iperf-only (legacy). Four phases: ingress/egress × elephant/mice.
#      The "mice" here are iperf3 -P 20 — 20 parallel long-lived TCP
#      flows. Characterizes bucket behavior under parallel elephants.
#
#   2. mixed (CMS fast-pass demo). iperf3 --bidir elephant against
#      perf-server (annotated 10M/10M), plus two parallel `hey` HTTP
#      runs — one against perf-server (annotated mice) and one against
#      bystander (unannotated nginx on the same worker). The annotated-
#      pod-mice column shows whether the plugin can isolate small flows
#      sharing the elephant's bucket; the bystander column shows
#      whether the plugin adds any overhead to neighboring unannotated
#      pods.
#
# Output: docs/perf-vs-vanilla-result.txt with the raw numbers.
#
# Run time: ~18-22 minutes. Docker required on macOS (colima or Docker
# Desktop).

set -euo pipefail

REPO_ROOT="$(cd "$(dirname "$0")/.." && pwd)"
RESULT_FILE="${REPO_ROOT}/docs/perf-vs-vanilla-result.txt"

BASELINE_CLUSTER="natra-vs-vanilla-baseline"
NATRA_CLUSTER="natra-vs-vanilla-natra"
VANILLA_CLUSTER="natra-vs-vanilla-vanilla"
NATRA_IMAGE="ghcr.io/terraboops/natra:vsperf"
PERFCLIENT_IMAGE="ghcr.io/terraboops/natra-perfclient:vsperf"

ELEPHANT_DURATION=15
MICE_PARALLEL=20
MICE_DURATION=10
RATE="10M"

# Mixed-workload phase tuning. The iperf3 --bidir flow runs for
# MIXED_IPERF_DURATION; hey runs for MIXED_HEY_DURATION starting after
# MIXED_HEY_LAG so the bucket is fully drained before hey is measured.
# MIXED_HEY_DURATION + MIXED_HEY_LAG must be ≤ MIXED_IPERF_DURATION so
# iperf is still in flight while hey samples.
MIXED_IPERF_DURATION=30
MIXED_HEY_LAG=5
MIXED_HEY_DURATION=20
MIXED_HEY_CONCURRENCY=50

cleanup() {
    k3d cluster delete "$BASELINE_CLUSTER" 2>/dev/null || true
    k3d cluster delete "$NATRA_CLUSTER" 2>/dev/null || true
    k3d cluster delete "$VANILLA_CLUSTER" 2>/dev/null || true
}

# nodes_for prints the docker container names for every node (server
# + agents) in the named k3d cluster. k3d's node-list output format
# is `k3d-<cluster>-{server,agent}-N`; we filter by the cluster
# column so unrelated k3d clusters on the same daemon don't bleed in.
nodes_for() {
    local cluster="$1"
    k3d node list --no-headers 2>/dev/null \
        | awk -v c="$cluster" '$3 == c {print $1}'
}

# enable_ecn flips tcp_ecn=1 on every node of the named cluster.
# Sets the netns-scoped sysctl in the k3d node's root netns, which
# pod netns created later inherit. iperf3 and hey traffic between
# pods then negotiate ECN-capable connections at handshake time, so
# natra's bpf_skb_ecn_set_ce path can fire on above-rate packets
# instead of dropping them.
enable_ecn() {
    local cluster="$1"
    for node in $(nodes_for "$cluster"); do
        docker exec "$node" sysctl -w net.ipv4.tcp_ecn=1 >/dev/null 2>&1 || \
            echo "warn: tcp_ecn=1 on $node failed (continuing)"
    done
}
trap cleanup EXIT

# preserve_artifacts copies the profile-natra/ directory out of TMPDIR
# to NATRA_PERF_ARTIFACT_DIR (if set) before the trap nukes TMPDIR.
# Useful for iterative analysis where you want pprof / JSONL to outlive
# the script.
preserve_artifacts() {
    if [ -n "${NATRA_PERF_ARTIFACT_DIR:-}" ] && [ -d "$TMPDIR/profile-natra" ]; then
        mkdir -p "$NATRA_PERF_ARTIFACT_DIR"
        cp -a "$TMPDIR/profile-natra" "$NATRA_PERF_ARTIFACT_DIR/profile-natra-$(date -u +%Y%m%dT%H%M%SZ)"
        echo "==> preserved profile artifacts to $NATRA_PERF_ARTIFACT_DIR" >&2
    fi
}

require() {
    command -v "$1" >/dev/null 2>&1 || { echo "missing: $1"; exit 1; }
}

require docker
require k3d
require kubectl
require jq
require curl
require tar

# BPFTOOL_HOST_PATH is populated once by ensure_bpftool() and reused
# across phases. The binary is cached under ${REPO_ROOT}/bin/ so repeat
# script runs don't redownload it.
BPFTOOL_HOST_PATH=""

# Pinned bpftool version. v7.7.0 ships with `features: llvm, skeletons`
# (the build was done with clang ≥ 10), which is the prerequisite for
# `bpftool prog profile`. Earlier versions and distro packages
# (notably debian bookworm's bpftool 7.1.0) are built without llvm
# support and refuse `prog profile` with "Please build bpftool with
# clang >= 10.0.0". Static linkage means it runs on any glibc ≥ 2.x
# kindest/node ships.
BPFTOOL_VERSION="v7.7.0"
BPFTOOL_RELEASE_URL_BASE="https://github.com/libbpf/bpftool/releases/download"

# ensure_bpftool downloads a statically-linked bpftool from the official
# libbpf/bpftool release and caches it under bin/. The kindest/node
# image's own apt can't install bpftool: it ships as part of
# linux-tools-$(uname -r), and on colima's LinuxKit kernel (~6.8.x) no
# matching kernel package exists. Debian's own bpftool package, while
# kernel-independent, is built without llvm support, so `prog profile`
# (the whole reason we want bpftool here) refuses to run.
#
# The upstream release bundles a static binary with llvm+skeletons
# enabled, which gives us cycles/instructions/cache_refs/cache_misses
# counters per BPF program invocation.
#
# Sets BPFTOOL_HOST_PATH on success; leaves it empty on failure so the
# caller can decide whether to skip profile capture.
ensure_bpftool() {
    if [ -n "$BPFTOOL_HOST_PATH" ] && [ -x "$BPFTOOL_HOST_PATH" ]; then
        return 0
    fi
    local arch
    case "$(uname -m)" in
        arm64|aarch64) arch="arm64" ;;
        x86_64|amd64)  arch="amd64" ;;
        *) echo "==> ensure_bpftool: unsupported arch $(uname -m)" >&2; return 1 ;;
    esac
    local cache="${REPO_ROOT}/bin/bpftool-${BPFTOOL_VERSION}-${arch}"
    if [ -x "$cache" ]; then
        BPFTOOL_HOST_PATH="$cache"
        echo "==> bpftool cached at $BPFTOOL_HOST_PATH" >&2
        return 0
    fi
    mkdir -p "${REPO_ROOT}/bin"
    local url="${BPFTOOL_RELEASE_URL_BASE}/${BPFTOOL_VERSION}/bpftool-${BPFTOOL_VERSION}-${arch}.tar.gz"
    local tmp_tgz="$TMPDIR/bpftool.tar.gz"
    local extract="$TMPDIR/bpftool-extract"
    mkdir -p "$extract"
    echo "==> downloading bpftool ${BPFTOOL_VERSION} (${arch}) from libbpf/bpftool release" >&2
    if ! curl -fsSL --retry 3 --retry-delay 2 -o "$tmp_tgz" "$url"; then
        echo "==> ensure_bpftool: curl from $url failed" >&2
        return 1
    fi
    if ! tar -xzf "$tmp_tgz" -C "$extract"; then
        echo "==> ensure_bpftool: tar extract failed" >&2
        return 1
    fi
    if [ ! -f "$extract/bpftool" ]; then
        echo "==> ensure_bpftool: archive missing bpftool binary" >&2
        return 1
    fi
    mv "$extract/bpftool" "$cache"
    chmod 0755 "$cache"
    BPFTOOL_HOST_PATH="$cache"
    echo "==> bpftool cached at $BPFTOOL_HOST_PATH" >&2
}

# install_bpftool_in_node docker-cps the staged bpftool into the kind
# node at /usr/local/bin/bpftool. Idempotent — re-copy is cheap. Returns
# nonzero if bpftool isn't staged or the copy fails, so the caller can
# skip prog-profile capture without aborting the whole run.
install_bpftool_in_node() {
    local node="$1"
    if [ -z "$BPFTOOL_HOST_PATH" ] || [ ! -x "$BPFTOOL_HOST_PATH" ]; then
        return 1
    fi
    # /usr/local/bin doesn't exist on rancher/k3s (busybox) by default.
    # Create it inside the node container before copying so docker cp
    # has somewhere to land. /bin is read-only on some images, /usr/
    # local/bin is the standard "writable user binaries" directory and
    # k3s nodes do support it once created.
    docker exec "$node" mkdir -p /usr/local/bin >/dev/null 2>&1 || true
    if ! docker cp "$BPFTOOL_HOST_PATH" "$node:/usr/local/bin/bpftool" >&2; then
        echo "==> install_bpftool_in_node: docker cp to $node failed" >&2
        return 1
    fi
    docker exec "$node" chmod 0755 /usr/local/bin/bpftool >/dev/null 2>&1 || true
    return 0
}

# Render iperf manifests with the cluster-specific node names. k3d
# names nodes k3d-<cluster>-{server,agent}-N; the source manifests
# hardcode k3d-natra-e2e-* names (the canonical e2e cluster), so we
# sed them to whichever cluster this run uses. The bidi manifest
# carries both ingress and egress annotations so the same server
# pod can be exercised in either direction without a redeploy.
render_manifests() {
    local cluster_name="$1" outdir="$2"
    local k3d_agent="k3d-${cluster_name}-agent-0"
    local k3d_server="k3d-${cluster_name}-server-0"
    # Rename the pod/service from iperf-server-bidi → iperf-server so
    # the run_workload commands below can use the canonical name and
    # match the L4 e2e shape. The bidi YAML carries both annotations,
    # which is what we want for this two-phase per-direction workload.
    sed -e "s|k3d-natra-e2e-agent-0|${k3d_agent}|" \
        -e "s|k3d-natra-e2e-server-0|${k3d_server}|" \
        -e "s|iperf-server-bidi|iperf-server|g" \
        "${REPO_ROOT}/test/e2e/manifests/iperf-server-bidi.yaml" \
        > "$outdir/iperf-server.yaml"
    sed -e "s|k3d-natra-e2e-agent-0|${k3d_agent}|" \
        -e "s|k3d-natra-e2e-server-0|${k3d_server}|" \
        "${REPO_ROOT}/test/e2e/manifests/iperf-client.yaml" \
        > "$outdir/iperf-client.yaml"
    cp "${REPO_ROOT}/test/e2e/manifests/namespace.yaml" "$outdir/namespace.yaml"
}

# Patch the upstream bandwidth plugin's HTB classes to use a sane
# burst. Kubelet provides no per-pod burst override, so kubelet sends
# (and the plugin uses) a huge default — observed as
# burst=193MB / cburst=386MB on a 10 Mbps annotation. That's
# ~150 seconds of credit, enough that a 30s measurement runs entirely
# inside the burst window without rate-shaping engaging. Apply 1 MB
# burst / 1 MB cburst directly via tc so the measured rate reflects
# the configured rate, not the initial burst.
#
# Idempotent: skips classes that don't exist or already match. Run
# AFTER pods are Ready (so the plugin has installed the qdiscs).
fix_vanilla_htb_burst() {
    local cluster="$1"
    for node in $(nodes_for "$cluster"); do
        docker exec "$node" sh -c '
            for dev in $(tc qdisc show 2>/dev/null | awk "/htb/ {print \$5}" | sort -u); do
                # class 1:30 (hex) = ShapedClassMinorID 48 in the
                # bandwidth plugin. The unshaped class 1:1 has rate
                # 800Gbit and is untouched.
                tc class change dev "$dev" classid 1:30 htb \
                    rate 10mbit ceil 20mbit burst 1mb cburst 1mb \
                    2>/dev/null || true
            done
        '
    done
}

# Render the mixed-workload manifests (perf-server with iperf3+nginx,
# perf-client with iperf3+hey, bystander with nginx only and no
# bandwidth annotations).
render_mixed_manifests() {
    local cluster_name="$1" outdir="$2"
    local k3d_agent="k3d-${cluster_name}-agent-0"
    local k3d_server="k3d-${cluster_name}-server-0"
    sed -e "s|PERF_WORKER_NODE|${k3d_agent}|" \
        "${REPO_ROOT}/test/perf/realworld/perf-server.yaml" \
        > "$outdir/perf-server.yaml"
    sed -e "s|PERF_CONTROL_NODE|${k3d_server}|" \
        "${REPO_ROOT}/test/perf/realworld/perf-client.yaml" \
        > "$outdir/perf-client.yaml"
    sed -e "s|PERF_WORKER_NODE|${k3d_agent}|" \
        "${REPO_ROOT}/test/perf/realworld/bystander.yaml" \
        > "$outdir/bystander.yaml"
}

# warmup_pod drains a freshly-attached qdisc's initial burst-token
# allowance before measurements. Both rate-limiters under test have a
# burst window — natra's is 2× rate (2.5 MB for a 10 Mbps annotation),
# upstream bandwidth's HTB picks up kubelet's default IngressBurst,
# which kubelet sets to a huge value (~193 MB observed on kind nodes,
# ~150 seconds of credit at 10 Mbps). Without a warmup, the first
# elephant flow consumes that burst at line rate before the rate
# limiter actually engages, inflating measurements by ~10× under
# vanilla. After ~12s of forward + 12s of reverse traffic, both
# plugins are at steady-state and subsequent measurements reflect the
# configured rate.
warmup_pod() {
    local namespace="natra-e2e" server="$1" client="$2"
    # 20s × ~100 Mbps line rate ≈ 250 MB transferred — enough to
    # fully drain vanilla's 193 MB HTB burst and natra's 2.5 MB
    # token bucket. -P 4 parallel streams to maximize throughput
    # during the drain since kindnet vxlan single-stream peaks below
    # line rate.
    kubectl exec -n "$namespace" "$client" -- \
        iperf3 -c "$server" -t 20 -P 4 >/dev/null 2>&1 || true
    kubectl exec -n "$namespace" "$client" -- \
        iperf3 -c "$server" -t 20 -P 4 -R >/dev/null 2>&1 || true
    # No post-warmup sleep: the next measurement starts with an
    # empty bucket so it can't borrow any unused-burst headroom.
    # The first measured second of the test will dip slightly
    # below rate, but the rest is true steady-state.
}

# run_workload prints four integers on stdout:
#   ingress_elephant_bps ingress_mice_bps egress_elephant_bps egress_mice_bps
# Forward iperf3 measures throughput INTO the server (ingress); reverse
# (-R) measures throughput OUT of the server (egress).
run_workload() {
    local namespace="natra-e2e"

    echo "==> warming up iperf-server (draining initial burst)" >&2
    warmup_pod iperf-server iperf-client

    local ing_elephant ing_mice eg_elephant eg_mice

    ing_elephant=$(kubectl exec -n "$namespace" iperf-client -- \
        iperf3 -c iperf-server -t "$ELEPHANT_DURATION" -J 2>/dev/null \
        | jq '.end.sum_received.bits_per_second // 0')

    ing_mice=$(kubectl exec -n "$namespace" iperf-client -- \
        iperf3 -c iperf-server -t "$MICE_DURATION" -P "$MICE_PARALLEL" -J 2>/dev/null \
        | jq '.end.sum_received.bits_per_second // 0')

    eg_elephant=$(kubectl exec -n "$namespace" iperf-client -- \
        iperf3 -c iperf-server -t "$ELEPHANT_DURATION" -R -J 2>/dev/null \
        | jq '.end.sum_received.bits_per_second // 0')

    eg_mice=$(kubectl exec -n "$namespace" iperf-client -- \
        iperf3 -c iperf-server -t "$MICE_DURATION" -P "$MICE_PARALLEL" -R -J 2>/dev/null \
        | jq '.end.sum_received.bits_per_second // 0')

    echo "$ing_elephant $ing_mice $eg_elephant $eg_mice"
}

# parse_hey extracts (rps p50 p99 total) from a hey log. Defaults
# zero on any missing field. hey emits "%%" literally in its latency
# distribution block, so the awk pattern matches that.
parse_hey() {
    local log="$1" rps p50 p99 total
    rps=$(awk -F: '/Requests\/sec:/ {gsub(/^ */, "", $2); print $2; exit}' "$log")
    p50=$(awk '/50%% in/ {print $3; exit}' "$log")
    p99=$(awk '/99%% in/ {print $3; exit}' "$log")
    total=$(awk '/Total:/ {gsub(/^ */, "", $2); print $2; exit}' "$log")
    : "${rps:=0}"; : "${p50:=0}"; : "${p99:=0}"; : "${total:=0}"
    echo "$rps $p50 $p99 $total"
}

# run_mixed_workload runs an iperf3 --bidir elephant against perf-
# server (annotated 10M/10M) and two parallel hey HTTP runs — one
# against perf-server (annotated mice; shares the elephant's bucket),
# one against bystander (unannotated; on the same node, sharing only
# the physical uplink). Prints ten values:
#   iperf_ing_bps iperf_eg_bps \
#   pod_rps pod_p50 pod_p99 pod_total \
#   by_rps by_p50 by_p99 by_total
run_mixed_workload() {
    local namespace="natra-e2e" tag="$1" cluster="$2"
    local iperf_log="$TMPDIR/iperf-mixed-${tag}.json"
    local hey_pod_log="$TMPDIR/hey-pod-${tag}.txt"
    local hey_by_log="$TMPDIR/hey-bystander-${tag}.txt"

    # The function's stdout is the channel the caller reads from. Any
    # accidental stdout write in our body (docker exec error messages
    # to stdout, kubectl warnings, etc.) corrupts the read's
    # space-separated parse — observed: kubectl exec emitting "OCI
    # runtime exec failed: ..." somewhere in the chain caused the
    # caller to assign "OCI" to iperf_ing, "runtime" to iperf_eg,
    # and so on across all 10 variables. Save the real stdout on
    # fd 3 and redirect 1→2 for the body so only the final result
    # echo (>&3) lands on the read's input.
    exec 3>&1
    exec 1>&2

    # Profile collector runs on the worker node only when natra is
    # under test. The vanilla and baseline clusters have no natra
    # binary on /opt/cni/bin.
    local profile_started=0
    if [ "$tag" = "natra" ]; then
        start_profile_collector "$cluster" "$tag"
        profile_started=1
    fi

    # Skip the warmup for the baseline phase: there's no bucket to
    # drain, and the warmup itself would just be 40s of stalling
    # traffic with no signal.
    if [ "$tag" != "baseline" ]; then
        echo "==> warming up perf-server (draining initial burst)" >&2
        warmup_pod perf-server perf-client
    fi

    # Start the elephant in the background. --bidir drains both
    # ingress and egress buckets simultaneously, so the annotated-pod
    # hey traffic finds both buckets engaged.
    kubectl exec -n "$namespace" perf-client -- \
        iperf3 -c perf-server -t "$MIXED_IPERF_DURATION" --bidir -J \
        > "$iperf_log" 2>/dev/null &
    local iperf_pid=$!

    # Give the elephant time to fully drain the bucket before hey
    # starts. Without this, the first few requests would see a
    # partially-full bucket and inflate the gap.
    sleep "$MIXED_HEY_LAG"

    # Two parallel hey runs from the same client pod:
    #   - against perf-server (annotated): exercises CMS fast-pass
    #     under natra, queues behind the elephant under vanilla.
    #   - against bystander (unannotated, same node): should be a no-op
    #     under both plugins (no BPF / no HTB attached) and identical
    #     to the baseline.
    # -disable-keepalive means each request is a fresh 5-tuple → new
    # CMS flow_key → stays under the heavy-hitter threshold.
    kubectl exec -n "$namespace" perf-client -- \
        hey -z "${MIXED_HEY_DURATION}s" -c "$MIXED_HEY_CONCURRENCY" \
        -disable-keepalive \
        http://perf-server/ \
        > "$hey_pod_log" 2>&1 &
    local hey_pod_pid=$!

    kubectl exec -n "$namespace" perf-client -- \
        hey -z "${MIXED_HEY_DURATION}s" -c "$MIXED_HEY_CONCURRENCY" \
        -disable-keepalive \
        http://bystander/ \
        > "$hey_by_log" 2>&1 &
    local hey_by_pid=$!

    wait "$hey_pod_pid" || true
    wait "$hey_by_pid" || true
    wait "$iperf_pid" || true

    # Parse iperf3 --bidir output. streams[0] is forward (server's
    # ingress); streams[1] is reverse (server's egress). .end.sum_*
    # aggregates across both streams in --bidir mode so we pull each
    # stream's sender.bits_per_second separately.
    local iperf_ing iperf_eg
    iperf_ing=$(jq '.end.streams[0].sender.bits_per_second // 0' "$iperf_log" 2>/dev/null || echo 0)
    iperf_eg=$(jq '.end.streams[1].sender.bits_per_second // 0' "$iperf_log" 2>/dev/null || echo 0)

    local pod_rps pod_p50 pod_p99 pod_total
    read -r pod_rps pod_p50 pod_p99 pod_total < <(parse_hey "$hey_pod_log")
    local by_rps by_p50 by_p99 by_total
    read -r by_rps by_p50 by_p99 by_total < <(parse_hey "$hey_by_log")

    if [ "$profile_started" = 1 ]; then
        stop_profile_collector "$cluster" "$tag"
    fi

    # Sanity-fence every output value: if any kubectl exec died and
    # produced a non-numeric token (e.g. "OCI runtime exec failed:
    # container is not running"), the caller's `read` would treat
    # the multi-word error as field separators and corrupt every
    # downstream variable — a single error reads as "OCI runtime
    # exec failed: ..." spilled across 10 variables. Validate each
    # is a number; emit 0 + a stderr warning if not, so the caller's
    # echo prints zeros instead of fragments of an error message.
    is_num() { [[ "$1" =~ ^-?[0-9]+(\.[0-9]+)?$ ]]; }
    local v
    for v in iperf_ing iperf_eg pod_rps pod_p50 pod_p99 pod_total \
             by_rps by_p50 by_p99 by_total; do
        if ! is_num "${!v}"; then
            echo "warn: $tag mixed: $v not numeric ('${!v}') — defaulting to 0" >&2
            printf -v "$v" "%s" "0"
        fi
    done

    # Result emit on the original stdout (fd 3), which the caller's
    # `read` consumes. Anything else printed in this function went
    # to stderr (via the exec 1>&2 at the top), so it lands in the
    # logs but doesn't corrupt the field-by-field parse.
    echo "$iperf_ing $iperf_eg $pod_rps $pod_p50 $pod_p99 $pod_total $by_rps $by_p50 $by_p99 $by_total" >&3
    exec 3>&-
}

# Profile collector: runs `natra profile` on the worker node as a
# background process for the duration of the mixed workload. Captures
# per-tick BPF program stats (runtime_ns / run_count → derived ns/op)
# and per-pod map state (CMS fill, stats counters) so a regression in
# the hot path or a CMS-saturation drift can be inspected after the
# run.
#
# Output lands at ${TMPDIR}/profile-${tag}/snapshots.jsonl and
# heap-NNNNN.pprof; analyze with jq + go tool pprof respectively.
start_profile_collector() {
    local cluster="$1" tag="$2"
    local profile_dir="$TMPDIR/profile-${tag}"
    mkdir -p "$profile_dir"

    # The profile process writes inside the kind node to a known path;
    # we copy out via docker cp at stop time. setsid + nohup so the
    # process survives the docker-exec shell exiting (default SIGHUP
    # on exec-shell teardown was killing the collector before its
    # first snapshot landed).
    docker exec "k3d-${cluster}-agent-0" mkdir -p /var/log/natra-profile
    docker exec "k3d-${cluster}-agent-0" sh -c '
        setsid nohup /opt/cni/bin/natra profile \
            -interval 2s \
            -output /var/log/natra-profile/snapshots.jsonl \
            -heap-dir /var/log/natra-profile/heap \
            </dev/null \
            >/var/log/natra-profile/profile.log 2>&1 &
        echo $! > /var/log/natra-profile/profile.pid
    '
    # Brief settle so the first snapshot is on disk and the collector
    # has stabilized before workload traffic starts.
    sleep 1
    echo "==> started natra profile collector on k3d-${cluster}-agent-0" >&2

    # bpftool prog profile gives cycles + instructions per
    # natra_ingress / natra_egress invocation — finer-grained than
    # the runtime_ns/run_count `natra profile` collects. Runs for
    # 25s in the background; the natra mixed workload lasts ~30s,
    # so this overlaps the steady-state phase. Captures land at
    # ${profile_dir}/bpftool-prog-profile.txt.
    #
    # bpftool isn't in the kindest/node image by default and the
    # node's own apt can't install it (linux-tools-* is kernel-versioned
    # and no package matches colima's LinuxKit kernel). Even debian's
    # plain bpftool package is built without llvm support, so `prog
    # profile` refuses to run. ensure_bpftool() downloads the official
    # static binary from libbpf/bpftool releases (built with clang ≥ 10,
    # so `features: llvm, skeletons` is set) and caches it under bin/.
    # install_bpftool_in_node copies that staged binary into the kind
    # node at /usr/local/bin/bpftool.
    if ensure_bpftool && install_bpftool_in_node "k3d-${cluster}-agent-0"; then
        # Loosen perf paranoia and enable BPF stats so the in-kernel
        # runtime_ns / run_cnt counters update for `bpftool prog show`.
        # Both knobs are netns-root-scoped and idempotent.
        docker exec "k3d-${cluster}-agent-0" sh -c '
            sysctl -w kernel.perf_event_paranoid=-1 >/dev/null 2>&1 || true
            sysctl -w kernel.bpf_stats_enabled=1   >/dev/null 2>&1 || true
        ' || true
        docker exec "k3d-${cluster}-agent-0" sh -c '
            set +e
            if ! command -v bpftool >/dev/null 2>&1; then
                echo "bpftool not on PATH after install; skipping prog profile" > /var/log/natra-profile/bpftool.txt
                exit 0
            fi
            # Resolve natra prog IDs by name. There may be multiple if
            # several pods are attached; capture each separately so we
            # can spot per-pod variance. `bpftool prog show` lines look
            # like "<id>: <type>  name <name>  tag …" — split on the
            # name keyword to pull (id, name) reliably.
            ids=$(bpftool prog show 2>/dev/null | \
                awk "/ name (natra_ingress|natra_egress) / {
                    id=\$1; sub(/:\$/, \"\", id);
                    for(i=1;i<=NF;i++) if(\$i==\"name\"){ print id\":\"\$(i+1); break }
                }")
            if [ -z "$ids" ]; then
                echo "no natra programs loaded yet" > /var/log/natra-profile/bpftool.txt
                exit 0
            fi
            : > /var/log/natra-profile/bpftool.txt
            : > /var/log/natra-profile/bpftool.stderr
            # Phase 1: capture per-prog run_time_ns / run_cnt at the
            # start of the workload. These are kernel-side counters that
            # need no hardware PMU and work in any VM.
            for entry in $ids; do
                pid="${entry%%:*}"
                bpftool prog show id "$pid" -j > /var/log/natra-profile/before-$pid.json 2>/dev/null
            done
            # Phase 2: try `bpftool prog profile` (cycles/instructions/
            # cache_*). Requires PERF_TYPE_HARDWARE counters; works on
            # bare-metal Linux and most KVM setups. Apple Virtualization.
            # framework (colima `vm-type vz`) does NOT expose hw PMUs to
            # guests, and bpftool will fail with "failed to create event
            # cycles on cpu N". The fallback (phase 3) records the run-
            # time delta from `bpftool prog show` so the artifact is
            # still meaningful in that environment.
            for entry in $ids; do
                pid="${entry%%:*}"
                pname="${entry##*:}"
                (
                    echo "=== prog id=$pid name=$pname ==="
                    # v7.7.0 metric names: cycles, instructions,
                    # llc_misses, l1d_loads, dtlb_misses, itlb_misses.
                    # All are PERF_TYPE_HARDWARE; perf_event_open fails
                    # if the host kernel has no PMU exposed.
                    bpftool prog profile id "$pid" duration 25 \
                        cycles instructions llc_misses l1d_loads 2>>/var/log/natra-profile/bpftool.stderr
                    echo
                ) >> /var/log/natra-profile/bpftool.txt &
            done
            wait
            # Phase 3: snapshot run_time_ns/run_cnt again and append a
            # delta block. Even when prog profile fails, this gives us
            # actual per-program kernel time per call.
            echo "" >> /var/log/natra-profile/bpftool.txt
            echo "=== run_time_ns / run_cnt deltas ===" >> /var/log/natra-profile/bpftool.txt
            for entry in $ids; do
                pid="${entry%%:*}"
                pname="${entry##*:}"
                after=$(bpftool prog show id "$pid" -j 2>/dev/null)
                before=$(cat /var/log/natra-profile/before-$pid.json 2>/dev/null)
                if [ -n "$before" ] && [ -n "$after" ]; then
                    # bpftool may return either a bare object or a
                    # single-element array for `prog show id N -j`
                    # depending on version. Normalize to a bare object
                    # with `if type == array then .[0] else . end` so
                    # the delta math works either way.
                    printf "%s\n%s\n" "$before" "$after" | jq -s "
                        map(if type == \"array\" then .[0] else . end) |
                        {
                            prog_id: $pid,
                            name: \"$pname\",
                            run_time_ns_delta: ((.[1].run_time_ns // 0) - (.[0].run_time_ns // 0)),
                            run_cnt_delta:     ((.[1].run_cnt     // 0) - (.[0].run_cnt     // 0)),
                            ns_per_op: (if ((.[1].run_cnt // 0) - (.[0].run_cnt // 0)) > 0
                                then (((.[1].run_time_ns // 0) - (.[0].run_time_ns // 0)) / ((.[1].run_cnt // 0) - (.[0].run_cnt // 0)))
                                else 0 end)
                        }
                    " >> /var/log/natra-profile/bpftool.txt 2>>/var/log/natra-profile/bpftool.stderr || \
                    echo "{\"prog_id\": $pid, \"name\": \"$pname\", \"error\": \"jq merge failed\"}" >> /var/log/natra-profile/bpftool.txt
                fi
            done
            # If `prog profile` recorded errors but never produced data
            # rows, append the stderr so the diagnosis is in the file.
            if [ -s /var/log/natra-profile/bpftool.stderr ]; then
                echo "" >> /var/log/natra-profile/bpftool.txt
                echo "=== bpftool prog profile stderr ===" >> /var/log/natra-profile/bpftool.txt
                cat /var/log/natra-profile/bpftool.stderr >> /var/log/natra-profile/bpftool.txt
                echo "" >> /var/log/natra-profile/bpftool.txt
                echo "Note: hardware PMU unavailable in this VM. The Apple" >> /var/log/natra-profile/bpftool.txt
                echo "Virtualization.framework backend used by colima --vm-type=vz" >> /var/log/natra-profile/bpftool.txt
                echo "does not expose PERF_TYPE_HARDWARE counters to the guest." >> /var/log/natra-profile/bpftool.txt
                echo "Use colima --vm-type=qemu or run on bare-metal Linux for" >> /var/log/natra-profile/bpftool.txt
                echo "real cycles/instructions/cache_* per BPF invocation. The" >> /var/log/natra-profile/bpftool.txt
                echo "run_time_ns/run_cnt block above still reflects real kernel" >> /var/log/natra-profile/bpftool.txt
                echo "time per invocation, just without PMU breakdown." >> /var/log/natra-profile/bpftool.txt
            fi
        ' >/dev/null 2>&1 &
    else
        echo "==> bpftool unavailable; skipping prog profile" >&2
        docker exec "k3d-${cluster}-agent-0" sh -c \
            'echo "bpftool unavailable on host (ensure_bpftool failed); skipping prog profile" > /var/log/natra-profile/bpftool.txt' \
            >/dev/null 2>&1 || true
    fi
    # Diagnostic: dump state of /var/log/natra-profile/ so a missing
    # snapshot or a startup error in the profile binary shows up
    # immediately rather than as a silent "no snapshots written".
    docker exec "k3d-${cluster}-agent-0" sh -c '
        echo "  profile pid: $(cat /var/log/natra-profile/profile.pid 2>/dev/null)";
        echo "  profile process: $(ps -p $(cat /var/log/natra-profile/profile.pid 2>/dev/null) -o comm= 2>/dev/null || echo NOT_RUNNING)";
        echo "  profile dir:";
        ls -la /var/log/natra-profile/ 2>&1 | sed "s/^/    /";
        echo "  profile.log first 5 lines:";
        head -5 /var/log/natra-profile/profile.log 2>&1 | sed "s/^/    /";
    ' >&2 || true
}

stop_profile_collector() {
    local cluster="$1" tag="$2"
    local profile_dir="$TMPDIR/profile-${tag}"

    docker exec "k3d-${cluster}-agent-0" sh -c '
        pid=$(cat /var/log/natra-profile/profile.pid 2>/dev/null || true)
        if [ -n "$pid" ]; then
            kill "$pid" 2>/dev/null || true
            # Wait briefly for SIGTERM-handler to flush the last record.
            for i in 1 2 3 4 5; do
                kill -0 "$pid" 2>/dev/null || break
                sleep 0.2
            done
        fi
    '
    # Copy out the artifacts. Surface errors loudly — silent docker
    # cp failures previously masked the real reason snapshots weren't
    # landing locally.
    if ! docker cp "k3d-${cluster}-agent-0:/var/log/natra-profile/snapshots.jsonl" \
        "$profile_dir/snapshots.jsonl"; then
        echo "==> docker cp snapshots.jsonl FAILED" >&2
    fi
    docker cp "k3d-${cluster}-agent-0:/var/log/natra-profile/heap" \
        "$profile_dir/heap" || \
        echo "==> docker cp heap-dir failed (skipping)" >&2
    docker cp "k3d-${cluster}-agent-0:/var/log/natra-profile/profile.log" \
        "$profile_dir/profile.log" || true
    docker cp "k3d-${cluster}-agent-0:/var/log/natra-profile/bpftool.txt" \
        "$profile_dir/bpftool-prog-profile.txt" || true
    echo "==> profile artifacts: $profile_dir" >&2
    summarize_profile "$profile_dir/snapshots.jsonl"
    preserve_artifacts
}

# summarize_profile prints first/last snapshot deltas — most useful
# single-number from the JSONL: average ns/op per program over the
# whole run, plus CMS fill drift.
summarize_profile() {
    local jsonl="$1"
    if [ ! -s "$jsonl" ]; then
        echo "==> profile: no snapshots written" >&2
        return
    fi
    # jq does the heavy lifting: pull first and last lines, compute
    # per-program delta runtime / delta count.
    local n
    n=$(wc -l < "$jsonl" | tr -d ' ')
    echo "==> profile: $n snapshots" >&2
    jq -s '
        # Strip nanosecond fraction before parsing — fromdateiso8601
        # only handles ISO 8601 without fractional seconds.
        def parse_t: sub("\\.[0-9]+Z"; "Z") | fromdateiso8601;
        {
            duration_s: ((.[-1].time | parse_t) - (.[0].time | parse_t)),
            programs: (
                [.[0].programs[], .[-1].programs[]]
                | group_by(.id)
                | map(select(length == 2))
                | map({
                    name: .[0].name,
                    delta_runtime_ns: (.[1].runtime_ns - .[0].runtime_ns),
                    delta_run_count: (.[1].run_count - .[0].run_count),
                    avg_ns_per_op: (if (.[1].run_count - .[0].run_count) > 0
                        then (((.[1].runtime_ns - .[0].runtime_ns) | tonumber) / ((.[1].run_count - .[0].run_count) | tonumber))
                        else 0 end)
                })
            ),
            pods: (
                [.[0].pods[]? // empty, .[-1].pods[]? // empty]
                | group_by(.container_id)
                | map(select(length == 2))
                | map({
                    container_id: .[0].container_id,
                    cms_zeros_start: .[0].cms_zeros,
                    cms_zeros_end: .[1].cms_zeros,
                    cms_nonzero_start: .[0].cms_nonzero,
                    cms_nonzero_end: .[1].cms_nonzero,
                    cms_max_start: .[0].cms_max_count,
                    cms_max_end: .[1].cms_max_count
                })
            )
        }
    ' "$jsonl" >&2
}

TMPDIR=$(mktemp -d)
trap 'rm -rf "$TMPDIR"; cleanup' EXIT

# k3d cluster bootstrap is the same for every phase: 1 agent + 1
# server, no LoadBalancer, drop traefik/servicelb addons we don't use.
# Default flannel CNI stays in place; natra (Phase A) and the vanilla
# bandwidth plugin (Phase B) chain after it.
#
# flannel backend forced to host-gw: VXLAN (k3s default) goes through
# software encap which is brutally slow on colima/LinuxKit (no hw
# offload). Observed: 30 Mbps baseline on VXLAN vs ~Gbps on host-gw,
# making the "is natra limiting below the unlimited baseline?" test
# meaningless because the unlimited baseline lands at the limit. The
# k3d nodes share the same docker bridge network so host-gw direct
# routing works without extra config.
bootstrap_k3d() {
    local cluster="$1"
    k3d cluster create "$cluster" \
        --agents 1 \
        --no-lb \
        --k3s-arg "--disable=traefik,servicelb@server:0" \
        --k3s-arg "--flannel-backend=host-gw@server:0" \
        --wait
}

# Build images once, up front. Each cluster reuses them via k3d image import.
echo "==> building natra image: $NATRA_IMAGE"
docker build -q -t "$NATRA_IMAGE" -f "${REPO_ROOT}/deploy/docker/Dockerfile.cni" "$REPO_ROOT" >/dev/null

echo "==> building perfclient image: $PERFCLIENT_IMAGE"
docker build -q -t "$PERFCLIENT_IMAGE" -f "${REPO_ROOT}/deploy/docker/Dockerfile.perfclient" "$REPO_ROOT" >/dev/null

# ---- Phase 0: baseline (flannel only, no rate-limiting) ----
echo
echo "===================================================================="
echo "Phase 0: baseline (flannel, no rate-limiting plugin chained)"
echo "===================================================================="
mkdir -p "$TMPDIR/baseline"
render_manifests "$BASELINE_CLUSTER" "$TMPDIR/baseline"
render_mixed_manifests "$BASELINE_CLUSTER" "$TMPDIR/baseline"

bootstrap_k3d "$BASELINE_CLUSTER"
k3d image import "$PERFCLIENT_IMAGE" --cluster "$BASELINE_CLUSTER"
enable_ecn "$BASELINE_CLUSTER"

kubectl apply -f "$TMPDIR/baseline/namespace.yaml"
# No plugin DaemonSet here — flannel's conflist alone, so the
# kubernetes.io/{ingress,egress}-bandwidth annotations on perf-server
# are present but not processed by any chained plugin. Traffic flows
# at line rate; this row is the "what does the cluster do unaided"
# floor for the comparison.

kubectl apply -f "$TMPDIR/baseline/iperf-server.yaml"
kubectl apply -f "$TMPDIR/baseline/iperf-client.yaml"
kubectl apply -f "$TMPDIR/baseline/perf-server.yaml"
kubectl apply -f "$TMPDIR/baseline/perf-client.yaml"
kubectl apply -f "$TMPDIR/baseline/bystander.yaml"
kubectl wait --for=condition=Ready \
    pod/iperf-server pod/iperf-client pod/perf-server pod/perf-client pod/bystander \
    -n natra-e2e --timeout=180s

echo "==> running iperf-only workload (phase 0)"
read -r baseline_ing_elephant baseline_ing_mice baseline_eg_elephant baseline_eg_mice < <(run_workload)
echo "  baseline ingress elephant=$baseline_ing_elephant bps  mice=$baseline_ing_mice bps"
echo "  baseline egress  elephant=$baseline_eg_elephant bps  mice=$baseline_eg_mice bps"

echo "==> running mixed workload (phase 0)"
read -r baseline_mixed_iperf_ing baseline_mixed_iperf_eg \
        baseline_mixed_pod_rps baseline_mixed_pod_p50 baseline_mixed_pod_p99 baseline_mixed_pod_total \
        baseline_mixed_by_rps baseline_mixed_by_p50 baseline_mixed_by_p99 baseline_mixed_by_total \
    < <(run_mixed_workload baseline "$BASELINE_CLUSTER")
echo "  baseline mixed iperf ingress=$baseline_mixed_iperf_ing bps  egress=$baseline_mixed_iperf_eg bps"
echo "  baseline mixed pod hey  rps=$baseline_mixed_pod_rps  p50=$baseline_mixed_pod_p50  p99=$baseline_mixed_pod_p99"
echo "  baseline mixed bystander rps=$baseline_mixed_by_rps  p50=$baseline_mixed_by_p50  p99=$baseline_mixed_by_p99"

k3d cluster delete "$BASELINE_CLUSTER"

# ---- Phase A: natra ----
echo
echo "===================================================================="
echo "Phase A: natra"
echo "===================================================================="
mkdir -p "$TMPDIR/natra"
render_manifests "$NATRA_CLUSTER" "$TMPDIR/natra"
render_mixed_manifests "$NATRA_CLUSTER" "$TMPDIR/natra"

bootstrap_k3d "$NATRA_CLUSTER"
k3d image import "$NATRA_IMAGE" "$PERFCLIENT_IMAGE" --cluster "$NATRA_CLUSTER"
enable_ecn "$NATRA_CLUSTER"

kubectl apply -f "$TMPDIR/natra/namespace.yaml"
# NATRA_PERF_ATTACH_MODE picks the attach path. Default is auto;
# other options are tcx-{host,pod}side, clsact-{host,pod}side.
# NATRA_PERF_EDT_PACING={1,true} flips the cluster-default EDT
# pacing knob — natra installs fq on each pod eth0 and uses
# EDT-stamped skbs for above-rate egress instead of dropping.
#
# k3s puts CNI under /var/lib/rancher/k3s/{data/cni,agent/etc/cni/
# net.d}, so the installer's bin / conflist hostPaths get sed'd
# from the kind-style defaults at apply time.
ATTACH_MODE="${NATRA_PERF_ATTACH_MODE:-}"
if [ "$ATTACH_MODE" = "tcx-hostside" ]; then ATTACH_MODE=""; fi
EDT_PACING="${NATRA_PERF_EDT_PACING:-}"
# awk patterns are scoped so we only touch the natra init container's
# imagePullPolicy and env vars — never the pause sidecar. k3s nodes
# don't have registry.k8s.io/pause:3.10 locally, so flipping its
# policy to Never gives ErrImageNeverPull and the whole DS stalls.
# The natra-image-line regex escapes slashes for awk's BRE.
natra_image_re=$(echo "$NATRA_IMAGE" | sed 's|/|\\/|g')
sed -e "s|ghcr.io/terraboops/natra:latest|${NATRA_IMAGE}|" \
    -e "s|path: /opt/cni/bin|path: /var/lib/rancher/k3s/data/cni|" \
    -e "s|path: /etc/cni/net.d|path: /var/lib/rancher/k3s/agent/etc/cni/net.d|" \
    "${REPO_ROOT}/deploy/cni-installer.yaml" | \
    awk -v am="$ATTACH_MODE" -v ep="$EDT_PACING" -v nire="$natra_image_re" '
        $0 ~ "image: " nire { print; getline; if ($1 == "imagePullPolicy:") sub(/IfNotPresent/, "Never"); print; next }
        /name: NATRA_ATTACH_MODE/ { print; getline; sub(/value: ".*"/, "value: \"" am "\""); print; next }
        /name: NATRA_EDT_PACING/  { print; getline; sub(/value: ".*"/, "value: \"" ep "\""); print; next }
        { print }
    ' | kubectl apply -f -
kubectl rollout status daemonset/natra-installer -n kube-system --timeout=120s

kubectl apply -f "$TMPDIR/natra/iperf-server.yaml"
kubectl apply -f "$TMPDIR/natra/iperf-client.yaml"
kubectl apply -f "$TMPDIR/natra/perf-server.yaml"
kubectl apply -f "$TMPDIR/natra/perf-client.yaml"
kubectl apply -f "$TMPDIR/natra/bystander.yaml"
kubectl wait --for=condition=Ready \
    pod/iperf-server pod/iperf-client pod/perf-server pod/perf-client pod/bystander \
    -n natra-e2e --timeout=180s

echo "==> running iperf-only workload (phase A)"
read -r natra_ing_elephant natra_ing_mice natra_eg_elephant natra_eg_mice < <(run_workload)
echo "  natra ingress elephant=$natra_ing_elephant bps  mice=$natra_ing_mice bps"
echo "  natra egress  elephant=$natra_eg_elephant bps  mice=$natra_eg_mice bps"

echo "==> running mixed workload (phase A)"
read -r natra_mixed_iperf_ing natra_mixed_iperf_eg \
        natra_mixed_pod_rps natra_mixed_pod_p50 natra_mixed_pod_p99 natra_mixed_pod_total \
        natra_mixed_by_rps natra_mixed_by_p50 natra_mixed_by_p99 natra_mixed_by_total \
    < <(run_mixed_workload natra "$NATRA_CLUSTER")
echo "  natra mixed iperf ingress=$natra_mixed_iperf_ing bps  egress=$natra_mixed_iperf_eg bps"
echo "  natra mixed pod hey  rps=$natra_mixed_pod_rps  p50=$natra_mixed_pod_p50  p99=$natra_mixed_pod_p99"
echo "  natra mixed bystander rps=$natra_mixed_by_rps  p50=$natra_mixed_by_p50  p99=$natra_mixed_by_p99"

k3d cluster delete "$NATRA_CLUSTER"

# ---- Phase B: upstream bandwidth plugin ----
echo
echo "===================================================================="
echo "Phase B: upstream containernetworking/plugins/bandwidth"
echo "===================================================================="
mkdir -p "$TMPDIR/vanilla"
render_manifests "$VANILLA_CLUSTER" "$TMPDIR/vanilla"
render_mixed_manifests "$VANILLA_CLUSTER" "$TMPDIR/vanilla"

bootstrap_k3d "$VANILLA_CLUSTER"
k3d image import "$PERFCLIENT_IMAGE" --cluster "$VANILLA_CLUSTER"
enable_ecn "$VANILLA_CLUSTER"

# Load ifb on each k3d node — the upstream bandwidth plugin uses
# HTB on an IFB device, and the kindest/node image (which k3d also
# uses under the hood) ships the module but doesn't auto-load it.
# Doing this before the DaemonSet's install container patches the
# conflist guarantees the bandwidth plugin can create the IFB
# device when kubelet first invokes it.
for node in $(nodes_for "$VANILLA_CLUSTER"); do
    docker exec "$node" modprobe ifb || \
        echo "warn: modprobe ifb on $node failed (continuing)"
done

kubectl apply -f "$TMPDIR/vanilla/namespace.yaml"
# vanilla-installer.yaml's bin hostPath is /opt/cni/bin (kind);
# k3s wants /var/lib/rancher/k3s/data/cni. sed at apply time.
sed -e 's|path: /opt/cni/bin|path: /var/lib/rancher/k3s/data/cni|' \
    -e 's|path: /etc/cni/net.d|path: /var/lib/rancher/k3s/agent/etc/cni/net.d|' \
    "${REPO_ROOT}/test/perf/realworld/vanilla-installer.yaml" \
    | kubectl apply -f -
kubectl rollout status daemonset/vanilla-bandwidth-installer -n kube-system --timeout=120s

kubectl apply -f "$TMPDIR/vanilla/iperf-server.yaml"
kubectl apply -f "$TMPDIR/vanilla/iperf-client.yaml"
kubectl apply -f "$TMPDIR/vanilla/perf-server.yaml"
kubectl apply -f "$TMPDIR/vanilla/perf-client.yaml"
kubectl apply -f "$TMPDIR/vanilla/bystander.yaml"
kubectl wait --for=condition=Ready \
    pod/iperf-server pod/iperf-client pod/perf-server pod/perf-client pod/bystander \
    -n natra-e2e --timeout=180s

# Patch HTB burst on every veth/ifb the bandwidth plugin created.
# Without this, kubelet's default-huge burst lets the first ~150s of
# traffic flow unshaped — measurements that fit inside that window
# never see HTB engage.
fix_vanilla_htb_burst "$VANILLA_CLUSTER"

echo "==> running iperf-only workload (phase B)"
read -r vanilla_ing_elephant vanilla_ing_mice vanilla_eg_elephant vanilla_eg_mice < <(run_workload)
echo "  vanilla ingress elephant=$vanilla_ing_elephant bps  mice=$vanilla_ing_mice bps"
echo "  vanilla egress  elephant=$vanilla_eg_elephant bps  mice=$vanilla_eg_mice bps"

echo "==> running mixed workload (phase B)"
read -r vanilla_mixed_iperf_ing vanilla_mixed_iperf_eg \
        vanilla_mixed_pod_rps vanilla_mixed_pod_p50 vanilla_mixed_pod_p99 vanilla_mixed_pod_total \
        vanilla_mixed_by_rps vanilla_mixed_by_p50 vanilla_mixed_by_p99 vanilla_mixed_by_total \
    < <(run_mixed_workload vanilla "$VANILLA_CLUSTER")
echo "  vanilla mixed iperf ingress=$vanilla_mixed_iperf_ing bps  egress=$vanilla_mixed_iperf_eg bps"
echo "  vanilla mixed pod hey  rps=$vanilla_mixed_pod_rps  p50=$vanilla_mixed_pod_p50  p99=$vanilla_mixed_pod_p99"
echo "  vanilla mixed bystander rps=$vanilla_mixed_by_rps  p50=$vanilla_mixed_by_p50  p99=$vanilla_mixed_by_p99"

k3d cluster delete "$VANILLA_CLUSTER"

# Format human-readable output.
fmt_bps() {
    awk -v b="$1" 'BEGIN {
        if (b == 0) { print "0.00 Mbps"; exit }
        printf "%.2f Mbps", b/1e6
    }'
}

fmt_rps() {
    awk -v r="$1" 'BEGIN {
        if (r+0 == 0) { print "0 req/s"; exit }
        printf "%.0f req/s", r
    }'
}

fmt_secs() {
    awk -v s="$1" 'BEGIN {
        if (s+0 == 0) { print "  -    "; exit }
        printf "%6.1f ms", s*1000
    }'
}

cat <<EOF | tee "$RESULT_FILE"
natra vs upstream containernetworking/plugins/bandwidth — k3d cluster head-to-head
====================================================================================
Three configurations: baseline (flannel alone, no rate-limiting), natra, upstream
containernetworking/plugins/bandwidth. Same workloads against each.

Workload 1: iperf-only (legacy). Same iperf3 client/server, ${RATE} annotation each direction.
  - ingress: forward iperf3 (client → server, charged to server's ingress)
  - egress:  reverse iperf3 -R (server → client, charged to server's egress)
  - Phase 1: ${ELEPHANT_DURATION}s single elephant flow
  - Phase 2: ${MICE_DURATION}s × ${MICE_PARALLEL} parallel long-lived TCP flows
Iperf goodput, receiver-side aggregate (sum_received.bits_per_second).

Direction  Plugin                          Elephant            Mice (${MICE_PARALLEL}× parallel)
-------------------------------------------------------------------------------------------
ingress    baseline (no plugin)            $(fmt_bps "$baseline_ing_elephant")        $(fmt_bps "$baseline_ing_mice")
ingress    natra                           $(fmt_bps "$natra_ing_elephant")        $(fmt_bps "$natra_ing_mice")
ingress    upstream bandwidth              $(fmt_bps "$vanilla_ing_elephant")        $(fmt_bps "$vanilla_ing_mice")
egress     baseline (no plugin)            $(fmt_bps "$baseline_eg_elephant")        $(fmt_bps "$baseline_eg_mice")
egress     natra                           $(fmt_bps "$natra_eg_elephant")        $(fmt_bps "$natra_eg_mice")
egress     upstream bandwidth              $(fmt_bps "$vanilla_eg_elephant")        $(fmt_bps "$vanilla_eg_mice")


Workload 2: mixed (elephant + HTTP mice on annotated pod + HTTP mice on unannotated
bystander). perf-server (annotated 10M/10M) runs iperf3 + nginx; bystander
(unannotated, same node) runs nginx. Client runs iperf3 --bidir against perf-server
for ${MIXED_IPERF_DURATION}s and (starting ${MIXED_HEY_LAG}s later) two parallel
hey runs (-c ${MIXED_HEY_CONCURRENCY} -disable-keepalive -z ${MIXED_HEY_DURATION}s)
against perf-server and bystander. Each hey request is a fresh TCP connection →
distinct flow_key → stays under heavy-hitter threshold.

The annotated-pod-mice column shows whether the plugin can isolate small flows
inside an annotated pod's budget. The bystander column shows whether the plugin
adds any overhead to neighboring unannotated pods.

                       │ Elephant flows           │ Annotated mice (perf-server) │ Bystander mice (unannotated)
Plugin                 │ ingress       egress     │ RPS          p50      p99    │ RPS          p50      p99
---------------------------------------------------------------------------------------------------------------------
baseline (no plugin)   │ $(fmt_bps "$baseline_mixed_iperf_ing")  $(fmt_bps "$baseline_mixed_iperf_eg")  │ $(fmt_rps "$baseline_mixed_pod_rps")   $(fmt_secs "$baseline_mixed_pod_p50")  $(fmt_secs "$baseline_mixed_pod_p99") │ $(fmt_rps "$baseline_mixed_by_rps")   $(fmt_secs "$baseline_mixed_by_p50")  $(fmt_secs "$baseline_mixed_by_p99")
natra                  │ $(fmt_bps "$natra_mixed_iperf_ing")  $(fmt_bps "$natra_mixed_iperf_eg")  │ $(fmt_rps "$natra_mixed_pod_rps")   $(fmt_secs "$natra_mixed_pod_p50")  $(fmt_secs "$natra_mixed_pod_p99") │ $(fmt_rps "$natra_mixed_by_rps")   $(fmt_secs "$natra_mixed_by_p50")  $(fmt_secs "$natra_mixed_by_p99")
upstream bandwidth     │ $(fmt_bps "$vanilla_mixed_iperf_ing")  $(fmt_bps "$vanilla_mixed_iperf_eg")  │ $(fmt_rps "$vanilla_mixed_pod_rps")   $(fmt_secs "$vanilla_mixed_pod_p50")  $(fmt_secs "$vanilla_mixed_pod_p99") │ $(fmt_rps "$vanilla_mixed_by_rps")   $(fmt_secs "$vanilla_mixed_by_p50")  $(fmt_secs "$vanilla_mixed_by_p99")


Raw numbers:
  baseline_ingress_elephant=$baseline_ing_elephant
  baseline_ingress_mice=$baseline_ing_mice
  baseline_egress_elephant=$baseline_eg_elephant
  baseline_egress_mice=$baseline_eg_mice
  baseline_mixed_iperf_ingress=$baseline_mixed_iperf_ing
  baseline_mixed_iperf_egress=$baseline_mixed_iperf_eg
  baseline_mixed_pod_rps=$baseline_mixed_pod_rps
  baseline_mixed_pod_p50=$baseline_mixed_pod_p50
  baseline_mixed_pod_p99=$baseline_mixed_pod_p99
  baseline_mixed_pod_total=$baseline_mixed_pod_total
  baseline_mixed_bystander_rps=$baseline_mixed_by_rps
  baseline_mixed_bystander_p50=$baseline_mixed_by_p50
  baseline_mixed_bystander_p99=$baseline_mixed_by_p99
  baseline_mixed_bystander_total=$baseline_mixed_by_total
  natra_ingress_elephant=$natra_ing_elephant
  natra_ingress_mice=$natra_ing_mice
  natra_egress_elephant=$natra_eg_elephant
  natra_egress_mice=$natra_eg_mice
  natra_mixed_iperf_ingress=$natra_mixed_iperf_ing
  natra_mixed_iperf_egress=$natra_mixed_iperf_eg
  natra_mixed_pod_rps=$natra_mixed_pod_rps
  natra_mixed_pod_p50=$natra_mixed_pod_p50
  natra_mixed_pod_p99=$natra_mixed_pod_p99
  natra_mixed_pod_total=$natra_mixed_pod_total
  natra_mixed_bystander_rps=$natra_mixed_by_rps
  natra_mixed_bystander_p50=$natra_mixed_by_p50
  natra_mixed_bystander_p99=$natra_mixed_by_p99
  natra_mixed_bystander_total=$natra_mixed_by_total
  vanilla_ingress_elephant=$vanilla_ing_elephant
  vanilla_ingress_mice=$vanilla_ing_mice
  vanilla_egress_elephant=$vanilla_eg_elephant
  vanilla_egress_mice=$vanilla_eg_mice
  vanilla_mixed_iperf_ingress=$vanilla_mixed_iperf_ing
  vanilla_mixed_iperf_egress=$vanilla_mixed_iperf_eg
  vanilla_mixed_pod_rps=$vanilla_mixed_pod_rps
  vanilla_mixed_pod_p50=$vanilla_mixed_pod_p50
  vanilla_mixed_pod_p99=$vanilla_mixed_pod_p99
  vanilla_mixed_pod_total=$vanilla_mixed_pod_total
  vanilla_mixed_bystander_rps=$vanilla_mixed_by_rps
  vanilla_mixed_bystander_p50=$vanilla_mixed_by_p50
  vanilla_mixed_bystander_p99=$vanilla_mixed_by_p99
  vanilla_mixed_bystander_total=$vanilla_mixed_by_total

Generated by scripts/perf-vs-vanilla.sh on $(date -u +%Y-%m-%dT%H:%M:%SZ)
EOF
