#!/usr/bin/env bash
# Real-cluster head-to-head: baseline (no plugin) vs natra vs upstream
# containernetworking/plugins/bandwidth.
#
# Spins up three kind clusters in sequence:
#   - Phase 0: kindnet alone, no rate-limiting plugin (baseline floor)
#   - Phase A: kindnet + natra chained
#   - Phase B: kindnet + upstream bandwidth chained
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
    kind delete cluster --name "$BASELINE_CLUSTER" 2>/dev/null || true
    kind delete cluster --name "$NATRA_CLUSTER" 2>/dev/null || true
    kind delete cluster --name "$VANILLA_CLUSTER" 2>/dev/null || true
}

# enable_ecn flips tcp_ecn=1 on every node of the named cluster.
# Sets the netns-scoped sysctl in the kind node's root netns, which
# pod netns created later inherit. iperf3 and hey traffic between
# pods then negotiate ECN-capable connections at handshake time, so
# natra's bpf_skb_ecn_set_ce path can fire on above-rate packets
# instead of dropping them.
enable_ecn() {
    local cluster="$1"
    for node in $(kind get nodes --name "$cluster"); do
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
require kind
require kubectl
require jq

# Render iperf manifests with the cluster-specific node names. kind
# names nodes <cluster>-{control-plane,worker}; the source manifests
# hardcode natra-e2e-* names, so we sed them at runtime. The bidi
# manifest carries both ingress and egress annotations so the same
# server pod can be exercised in either direction without a redeploy.
render_manifests() {
    local cluster_name="$1" outdir="$2"
    # Rename the pod/service from iperf-server-bidi → iperf-server so
    # the run_workload commands below can use the canonical name and
    # match the L4 e2e shape. The bidi YAML carries both annotations,
    # which is what we want for this two-phase per-direction workload.
    sed -e "s|natra-e2e-worker|${cluster_name}-worker|" \
        -e "s|natra-e2e-control-plane|${cluster_name}-control-plane|" \
        -e "s|iperf-server-bidi|iperf-server|g" \
        "${REPO_ROOT}/test/e2e/manifests/iperf-server-bidi.yaml" \
        > "$outdir/iperf-server.yaml"
    sed -e "s|natra-e2e-worker|${cluster_name}-worker|" \
        -e "s|natra-e2e-control-plane|${cluster_name}-control-plane|" \
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
    for node in $(kind get nodes --name "$cluster"); do
        docker exec "$node" bash -c '
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
    sed -e "s|PERF_WORKER_NODE|${cluster_name}-worker|" \
        "${REPO_ROOT}/test/perf/realworld/perf-server.yaml" \
        > "$outdir/perf-server.yaml"
    sed -e "s|PERF_CONTROL_NODE|${cluster_name}-control-plane|" \
        "${REPO_ROOT}/test/perf/realworld/perf-client.yaml" \
        > "$outdir/perf-client.yaml"
    sed -e "s|PERF_WORKER_NODE|${cluster_name}-worker|" \
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

    echo "$iperf_ing $iperf_eg $pod_rps $pod_p50 $pod_p99 $pod_total $by_rps $by_p50 $by_p99 $by_total"
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
    docker exec "${cluster}-worker" mkdir -p /var/log/natra-profile
    docker exec "${cluster}-worker" bash -c '
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
    echo "==> started natra profile collector on ${cluster}-worker" >&2
    # Diagnostic: dump state of /var/log/natra-profile/ so a missing
    # snapshot or a startup error in the profile binary shows up
    # immediately rather than as a silent "no snapshots written".
    docker exec "${cluster}-worker" bash -c '
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

    docker exec "${cluster}-worker" bash -c '
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
    if ! docker cp "${cluster}-worker:/var/log/natra-profile/snapshots.jsonl" \
        "$profile_dir/snapshots.jsonl"; then
        echo "==> docker cp snapshots.jsonl FAILED" >&2
    fi
    docker cp "${cluster}-worker:/var/log/natra-profile/heap" \
        "$profile_dir/heap" || \
        echo "==> docker cp heap-dir failed (skipping)" >&2
    docker cp "${cluster}-worker:/var/log/natra-profile/profile.log" \
        "$profile_dir/profile.log" || true
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

# Build images once, up front. Both clusters reuse them via kind load.
echo "==> building natra image: $NATRA_IMAGE"
docker build -q -t "$NATRA_IMAGE" -f "${REPO_ROOT}/deploy/docker/Dockerfile.cni" "$REPO_ROOT" >/dev/null

echo "==> building perfclient image: $PERFCLIENT_IMAGE"
docker build -q -t "$PERFCLIENT_IMAGE" -f "${REPO_ROOT}/deploy/docker/Dockerfile.perfclient" "$REPO_ROOT" >/dev/null

# ---- Phase 0: baseline (kindnet only, no rate-limiting) ----
echo
echo "===================================================================="
echo "Phase 0: baseline (kindnet, no rate-limiting plugin chained)"
echo "===================================================================="
mkdir -p "$TMPDIR/baseline"
render_manifests "$BASELINE_CLUSTER" "$TMPDIR/baseline"
render_mixed_manifests "$BASELINE_CLUSTER" "$TMPDIR/baseline"

kind create cluster --name "$BASELINE_CLUSTER" \
    --config "${REPO_ROOT}/test/e2e/kind-config.yaml" --wait 120s
kind load docker-image "$PERFCLIENT_IMAGE" --name "$BASELINE_CLUSTER"
enable_ecn "$BASELINE_CLUSTER"

kubectl apply -f "$TMPDIR/baseline/namespace.yaml"
# No plugin DaemonSet here — kindnet's conflist alone, so the
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

kind delete cluster --name "$BASELINE_CLUSTER"

# ---- Phase A: natra ----
echo
echo "===================================================================="
echo "Phase A: natra"
echo "===================================================================="
mkdir -p "$TMPDIR/natra"
render_manifests "$NATRA_CLUSTER" "$TMPDIR/natra"
render_mixed_manifests "$NATRA_CLUSTER" "$TMPDIR/natra"

kind create cluster --name "$NATRA_CLUSTER" \
    --config "${REPO_ROOT}/test/e2e/kind-config.yaml" --wait 120s
kind load docker-image "$NATRA_IMAGE" "$PERFCLIENT_IMAGE" --name "$NATRA_CLUSTER"
enable_ecn "$NATRA_CLUSTER"

kubectl apply -f "$TMPDIR/natra/namespace.yaml"
# NATRA_PERF_ATTACH_MODE picks the attach path. Default is auto;
# other options are tcx-{host,pod}side, clsact-{host,pod}side.
# NATRA_PERF_EDT_PACING={1,true} flips the cluster-default EDT
# pacing knob — natra installs fq on each pod eth0 and uses
# EDT-stamped skbs for above-rate egress instead of dropping.
ATTACH_MODE="${NATRA_PERF_ATTACH_MODE:-}"
if [ "$ATTACH_MODE" = "tcx-hostside" ]; then ATTACH_MODE=""; fi
EDT_PACING="${NATRA_PERF_EDT_PACING:-}"
sed -e "s|ghcr.io/terraboops/natra:latest|${NATRA_IMAGE}|" \
    -e "s|imagePullPolicy: IfNotPresent|imagePullPolicy: Never|" \
    "${REPO_ROOT}/deploy/cni-installer.yaml" | \
    awk -v am="$ATTACH_MODE" -v ep="$EDT_PACING" '
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

kind delete cluster --name "$NATRA_CLUSTER"

# ---- Phase B: upstream bandwidth plugin ----
echo
echo "===================================================================="
echo "Phase B: upstream containernetworking/plugins/bandwidth"
echo "===================================================================="
mkdir -p "$TMPDIR/vanilla"
render_manifests "$VANILLA_CLUSTER" "$TMPDIR/vanilla"
render_mixed_manifests "$VANILLA_CLUSTER" "$TMPDIR/vanilla"

kind create cluster --name "$VANILLA_CLUSTER" \
    --config "${REPO_ROOT}/test/e2e/kind-config.yaml" --wait 120s
kind load docker-image "$PERFCLIENT_IMAGE" --name "$VANILLA_CLUSTER"
enable_ecn "$VANILLA_CLUSTER"

# Load ifb on each kind node — the upstream bandwidth plugin uses
# HTB on an IFB device, and the kind base image ships the module but
# doesn't auto-load it. Doing this before the DaemonSet's install
# container patches the conflist guarantees the bandwidth plugin can
# create the IFB device when kubelet first invokes it.
for node in $(kind get nodes --name "$VANILLA_CLUSTER"); do
    docker exec "$node" modprobe ifb || \
        echo "warn: modprobe ifb on $node failed (continuing)"
done

kubectl apply -f "$TMPDIR/vanilla/namespace.yaml"
kubectl apply -f "${REPO_ROOT}/test/perf/realworld/vanilla-installer.yaml"
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

kind delete cluster --name "$VANILLA_CLUSTER"

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
natra vs upstream containernetworking/plugins/bandwidth — kind cluster head-to-head
====================================================================================
Three configurations: baseline (kindnet alone, no rate-limiting), natra, upstream
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
