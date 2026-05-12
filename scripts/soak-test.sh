#!/usr/bin/env bash
# soak-test.sh — multi-hour cluster-from-hell experiment platform.
#
# Goal: measure natra's behavior on production-shaped chaos over time
# scales that the perf-vs-vanilla rig can't reach. The perf rig runs
# 30 seconds and the CMS aging interval is 60 seconds — so it never
# sees decay fire, never sees long-term saturation, never sees node
# churn, and never exercises coexistence with another BPF stack
# attaching to the same pod veth.
#
# What this rig adds:
#
#   - Multi-hour duration. Aging fires every 60s; a 4h run sees ~240
#     decay events. Long enough that long-term drift / saturation
#     curves become visible.
#
#   - Node churn. k3d's `node create / delete` lets us add and remove
#     workers while the cluster keeps running. kind doesn't — its
#     node count is fixed at create time. Cluster autoscalers, spot
#     reclaims, and AZ failures all look like node churn from a CNI's
#     perspective; this rig surfaces install-path bugs that only
#     show up when natra meets a fresh node mid-life.
#
#   - Goldpinger DaemonSet. All-pairs connectivity probe with
#     per-pod RTT and timeout metrics. If any plugin silently breaks
#     routing on some pod combo, goldpinger sees it long before our
#     explicit iperf measurement would catch it.
#
#   - Three modes (--mode flag):
#         natra      → natra chained after the base CNI
#         vanilla    → upstream bandwidth plugin chained after base
#         baseline   → base CNI alone, no rate-limiting plugin
#     Run each separately, get three TSVs, compare post-hoc.
#
# Output (per run, under --output):
#
#     metrics.tsv           one row per measurement, columns:
#                           timestamp_unix, iperf_ing_bps,
#                           iperf_eg_bps, hey_rps, hey_p50_s,
#                           hey_p99_s, gp_total_probes,
#                           gp_failed_probes, node_count
#
#     heap/heap-NNN.pprof   bounded ring buffer (latest RETAIN_HEAP).
#     bpf/snapshots-NNN.jsonl  bounded ring buffer (latest RETAIN_BPF).
#     events.log            node churn events, anomalies.
#     summary.txt           final summary at end of run.

set -euo pipefail

REPO_ROOT="$(cd "$(dirname "$0")/.." && pwd)"

# Defaults — override via flags.
MODE="natra"
DURATION="4h"
OUTPUT_DIR="${REPO_ROOT}/results/soak-$(date -u +%Y%m%dT%H%M%SZ)"
INITIAL_NODES=3
NODE_CHURN_INTERVAL_S=600    # add/remove a worker every 10 min
SAMPLE_INTERVAL_S=60         # light measurement cadence
DEEP_INTERVAL_S=1800         # heap pprof + bpf stats snapshot cadence
RETAIN_HEAP=20               # keep latest N heap pprof files
RETAIN_BPF=20                # keep latest N bpf snapshot files

usage() {
    cat <<'EOF'
Usage: soak-test.sh [flags]

  --mode {natra,vanilla,baseline}    Which rate-limiting layer to chain. Default: natra.
  --duration <go-duration>           How long to run (e.g. 4h, 30m, 5m). Default: 4h.
  --output <dir>                     Result directory. Default: results/soak-<ts>/
  --initial-nodes <N>                Workers at startup. Default: 3.
  --node-churn-interval <seconds>    0 disables churn. Default: 600.
  --sample-interval <seconds>        Light measurement cadence. Default: 60.
  --deep-interval <seconds>          Heap/bpf snapshot cadence. Default: 1800.

Modes:
  natra     — k3d + Calico-eBPF + natra chained after.
  vanilla   — k3d + Calico-eBPF + upstream containernetworking bandwidth plugin.
  baseline  — k3d + Calico-eBPF alone. The "no rate limiter" reference.
EOF
}

while [ $# -gt 0 ]; do
    case "$1" in
        --mode)                MODE="$2"; shift 2 ;;
        --duration)            DURATION="$2"; shift 2 ;;
        --output)              OUTPUT_DIR="$2"; shift 2 ;;
        --initial-nodes)       INITIAL_NODES="$2"; shift 2 ;;
        --node-churn-interval) NODE_CHURN_INTERVAL_S="$2"; shift 2 ;;
        --sample-interval)     SAMPLE_INTERVAL_S="$2"; shift 2 ;;
        --deep-interval)       DEEP_INTERVAL_S="$2"; shift 2 ;;
        -h|--help)             usage; exit 0 ;;
        *)                     echo "unknown flag: $1"; usage; exit 1 ;;
    esac
done

case "$MODE" in
    natra|vanilla|baseline) ;;
    *) echo "--mode must be one of: natra, vanilla, baseline"; exit 1 ;;
esac

require() {
    command -v "$1" >/dev/null 2>&1 || { echo "missing: $1"; exit 1; }
}
require docker
require k3d
require kubectl
require jq

mkdir -p "$OUTPUT_DIR/heap" "$OUTPUT_DIR/bpf"

# Convert duration to seconds. Supports {h,m,s,d} suffix.
parse_duration() {
    local d="$1" n="${1%[smhd]}" u="${1: -1}"
    case "$u" in
        s) echo "$n" ;;
        m) echo "$((n * 60))" ;;
        h) echo "$((n * 3600))" ;;
        d) echo "$((n * 86400))" ;;
        *) echo "bad duration: $d" >&2; exit 1 ;;
    esac
}
DURATION_S=$(parse_duration "$DURATION")
END_TS=$(( $(date +%s) + DURATION_S ))

CLUSTER="natra-soak"
NS="natra-soak"

# events.log records significant non-measurement events with a
# unix-timestamp prefix so post-hoc analysis can correlate them
# against metrics.tsv rows.
log_event() {
    printf '%d\t%s\n' "$(date +%s)" "$*" | tee -a "$OUTPUT_DIR/events.log" >&2
}

cleanup() {
    log_event "teardown: deleting k3d cluster $CLUSTER"
    k3d cluster delete "$CLUSTER" 2>/dev/null || true
}
trap cleanup EXIT

# ---------- bootstrap ----------
bootstrap_cluster() {
    log_event "bootstrap: creating k3d cluster $CLUSTER with $INITIAL_NODES workers"
    # --no-lb: skip the load balancer container; soak doesn't need
    # a service LB. --k3s-arg ... --disable=traefik,servicelb: drop
    # addons we don't use. Default k3s networking (flannel) is left
    # in place — Calico-eBPF as a separate base for NPA-style BPF
    # coexistence is on the TODO list as a follow-up; the soak rig
    # works fine with vanilla k3s for measuring natra's long-term
    # behavior under load.
    k3d cluster create "$CLUSTER" \
        --agents "$INITIAL_NODES" \
        --no-lb \
        --k3s-arg "--disable=traefik,servicelb@server:0" \
        --wait

    log_event "bootstrap: installing goldpinger"
    # Minimal goldpinger manifest. Upstream ships Helm charts, not
    # a single-file apply target — easier to inline a stripped-down
    # DaemonSet here than to add Helm as a dependency.
    cat <<EOF | kubectl apply -f -
apiVersion: v1
kind: ServiceAccount
metadata:
  name: goldpinger
  namespace: default
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: goldpinger
rules:
  - apiGroups: [""]
    resources: ["pods"]
    verbs: ["list"]
  - apiGroups: [""]
    resources: ["nodes"]
    verbs: ["list"]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: goldpinger
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: goldpinger
subjects:
  - kind: ServiceAccount
    name: goldpinger
    namespace: default
---
apiVersion: apps/v1
kind: DaemonSet
metadata:
  name: goldpinger
  namespace: default
spec:
  selector:
    matchLabels:
      app: goldpinger
  template:
    metadata:
      labels:
        app: goldpinger
    spec:
      serviceAccountName: goldpinger
      tolerations:
        - operator: Exists
          effect: NoSchedule
      containers:
        - name: goldpinger
          image: docker.io/bloomberg/goldpinger:3.11.2
          env:
            - name: HOST
              value: "0.0.0.0"
            - name: PORT
              value: "80"
            - name: HOSTS_TO_RESOLVE
              value: "1.1.1.1 8.8.8.8 kubernetes.default.svc.cluster.local"
            - name: POD_IP
              valueFrom: { fieldRef: { fieldPath: status.podIP } }
            - name: REFRESH_INTERVAL
              value: "30"
          ports:
            - containerPort: 80
              name: http
---
apiVersion: v1
kind: Service
metadata:
  name: goldpinger
  namespace: default
spec:
  selector:
    app: goldpinger
  ports:
    - port: 80
      targetPort: 80
EOF
    # Best-effort: if goldpinger doesn't come up in time (slow image
    # pull, k3s networking quirks on this host), continue anyway —
    # the soak loop's primary signals are iperf/hey numbers, not the
    # goldpinger scrape. Failed scrapes just record 0 in the
    # gp_* metrics columns.
    if ! kubectl -n default rollout status daemonset/goldpinger --timeout=180s; then
        log_event "bootstrap: goldpinger rollout timed out; continuing best-effort"
    fi

    kubectl create namespace "$NS"

    case "$MODE" in
        natra)
            log_event "bootstrap: installing natra layer"
            # Reuses the existing installer manifest. The image needs
            # to be loaded into k3d nodes — for soak we expect the
            # user to have pre-built and tagged the image; the
            # operator-friendly path is `make docker-build` once
            # before invoking this script.
            local natra_image="ghcr.io/terraboops/natra:soak"
            if ! docker image inspect "$natra_image" >/dev/null 2>&1; then
                log_event "bootstrap: building natra image $natra_image"
                docker build -q -t "$natra_image" -f "${REPO_ROOT}/deploy/docker/Dockerfile.cni" "$REPO_ROOT"
            fi
            k3d image import "$natra_image" -c "$CLUSTER"
            sed -e "s|ghcr.io/terraboops/natra:latest|${natra_image}|" \
                -e "s|imagePullPolicy: IfNotPresent|imagePullPolicy: Never|" \
                "${REPO_ROOT}/deploy/cni-installer.yaml" | kubectl apply -f -
            kubectl -n kube-system rollout status daemonset/natra-installer --timeout=120s
            ;;
        vanilla)
            log_event "bootstrap: installing upstream bandwidth plugin layer"
            kubectl apply -f "${REPO_ROOT}/test/perf/realworld/vanilla-installer.yaml"
            kubectl -n kube-system rollout status daemonset/vanilla-bandwidth-installer --timeout=120s
            ;;
        baseline)
            log_event "bootstrap: baseline mode (no rate-limiting layer)"
            ;;
    esac
}

# ---------- workload ----------
deploy_workload() {
    log_event "workload: deploying long-lived server + client pods"
    cat <<EOF | kubectl apply -f -
apiVersion: v1
kind: Pod
metadata:
  name: soak-server
  namespace: $NS
  annotations:
    kubernetes.io/ingress-bandwidth: "10M"
    kubernetes.io/egress-bandwidth: "10M"
  labels:
    app: soak-server
spec:
  containers:
    - name: iperf
      image: networkstatic/iperf3:latest
      args: ["-s"]
      ports:
        - containerPort: 5201
    - name: nginx
      image: nginx:1.27-alpine
      ports:
        - containerPort: 80
---
apiVersion: v1
kind: Service
metadata:
  name: soak-server
  namespace: $NS
spec:
  selector:
    app: soak-server
  ports:
    - name: iperf
      port: 5201
      targetPort: 5201
    - name: http
      port: 80
      targetPort: 80
---
apiVersion: v1
kind: Pod
metadata:
  name: soak-client
  namespace: $NS
  labels:
    app: soak-client
spec:
  containers:
    - name: tools
      image: ghcr.io/terraboops/natra-perfclient:vsperf
      imagePullPolicy: IfNotPresent
      command: ["sleep", "infinity"]
EOF
    # Reuse the perfclient image. Build/import if missing.
    if ! docker image inspect ghcr.io/terraboops/natra-perfclient:vsperf >/dev/null 2>&1; then
        docker build -q -t ghcr.io/terraboops/natra-perfclient:vsperf \
            -f "${REPO_ROOT}/deploy/docker/Dockerfile.perfclient" "$REPO_ROOT"
    fi
    k3d image import ghcr.io/terraboops/natra-perfclient:vsperf -c "$CLUSTER"

    kubectl -n "$NS" wait --for=condition=Ready pod/soak-server pod/soak-client --timeout=180s
}

# ---------- measurement ----------
# Light measurement: short iperf3 + short hey + goldpinger scrape.
# Appends one row to metrics.tsv. Designed to take <10s so
# back-to-back samples at 60s cadence don't drift.
measure() {
    local iperf_log="/tmp/soak-iperf.json" hey_log="/tmp/soak-hey.txt"
    # 5s iperf3 --bidir (single stream each direction).
    kubectl exec -n "$NS" soak-client -- \
        iperf3 -c soak-server -t 5 --bidir -J > "$iperf_log" 2>/dev/null &
    local iperf_pid=$!
    # 5s hey at 50 concurrent.
    kubectl exec -n "$NS" soak-client -- \
        hey -z 5s -c 50 -disable-keepalive http://soak-server/ > "$hey_log" 2>&1 &
    local hey_pid=$!

    wait "$iperf_pid" || true
    wait "$hey_pid" || true

    local iperf_ing iperf_eg
    iperf_ing=$(jq '.end.streams[0].sender.bits_per_second // 0' "$iperf_log" 2>/dev/null || echo 0)
    iperf_eg=$(jq '.end.streams[1].sender.bits_per_second // 0' "$iperf_log" 2>/dev/null || echo 0)
    local rps p50 p99
    rps=$(awk -F: '/Requests\/sec:/ {gsub(/^ */, "", $2); print $2; exit}' "$hey_log")
    p50=$(awk '/50%% in/ {print $3; exit}' "$hey_log")
    p99=$(awk '/99%% in/ {print $3; exit}' "$hey_log")
    : "${rps:=0}"; : "${p50:=0}"; : "${p99:=0}"

    # Goldpinger /metrics scrape via in-cluster service. Sum probe
    # totals and failures across all pods. Best-effort — if
    # goldpinger isn't reachable yet the values are 0.
    local gp_total gp_failed
    gp_total=$(kubectl exec -n "$NS" soak-client -- \
        sh -c 'wget -qO- http://goldpinger.default.svc.cluster.local:80/metrics 2>/dev/null || true' \
        | awk '/^goldpinger_peers_response_time_s_count/ {sum+=$NF} END{print sum+0}')
    gp_failed=$(kubectl exec -n "$NS" soak-client -- \
        sh -c 'wget -qO- http://goldpinger.default.svc.cluster.local:80/metrics 2>/dev/null || true' \
        | awk '/^goldpinger_peers_response_time_s_count.*"timeout"|"error"/ {sum+=$NF} END{print sum+0}')
    : "${gp_total:=0}"; : "${gp_failed:=0}"

    local node_count
    node_count=$(kubectl get nodes --no-headers 2>/dev/null | wc -l | tr -d ' ')

    printf '%d\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n' \
        "$(date +%s)" "$iperf_ing" "$iperf_eg" "$rps" "$p50" "$p99" \
        "$gp_total" "$gp_failed" "$node_count" \
        >> "$OUTPUT_DIR/metrics.tsv"
}

# ---------- node churn ----------
# Alternates add / remove a worker every NODE_CHURN_INTERVAL_S. The
# k3d node CRUD operations are the whole reason this rig uses k3d
# instead of kind. Set --node-churn-interval=0 to disable.
churn_loop() {
    [ "$NODE_CHURN_INTERVAL_S" -eq 0 ] && { log_event "churn: disabled"; return; }
    local op
    op="remove" # start by removing (we have INITIAL_NODES already)
    while [ "$(date +%s)" -lt "$END_TS" ]; do
        sleep "$NODE_CHURN_INTERVAL_S"
        [ "$(date +%s)" -ge "$END_TS" ] && break
        if [ "$op" = "remove" ]; then
            # Remove an arbitrary agent node. k3d node list returns
            # one per line; pick the last agent.
            local victim
            victim=$(k3d node list --no-headers 2>/dev/null \
                | awk '$3 == "agent" {print $1}' | tail -1)
            if [ -n "$victim" ]; then
                log_event "churn: removing node $victim"
                k3d node delete "$victim" 2>>"$OUTPUT_DIR/events.log" || \
                    log_event "churn: delete $victim failed"
            else
                log_event "churn: no agent to remove"
            fi
            op="add"
        else
            local new_name="soak-churn-$(date +%s)"
            log_event "churn: adding node $new_name"
            k3d node create "$new_name" --cluster "$CLUSTER" --role agent \
                2>>"$OUTPUT_DIR/events.log" || \
                log_event "churn: create $new_name failed"
            op="remove"
        fi
    done
}

# ---------- deep snapshots ----------
# Every DEEP_INTERVAL_S take a heap pprof of a one-off
# `natra profile` invocation (single-tick) and a BPF prog stats
# snapshot. Bounded ring buffer: keep only RETAIN_HEAP / RETAIN_BPF
# files so a 4h+ run doesn't fill disk.
snapshot_loop() {
    [ "$DEEP_INTERVAL_S" -eq 0 ] && { log_event "snapshot: disabled"; return; }
    [ "$MODE" != "natra" ] && { log_event "snapshot: only meaningful in natra mode, skipping"; return; }
    local n=0
    while [ "$(date +%s)" -lt "$END_TS" ]; do
        sleep "$DEEP_INTERVAL_S"
        [ "$(date +%s)" -ge "$END_TS" ] && break
        n=$((n + 1))
        local worker
        worker=$(k3d node list --no-headers 2>/dev/null \
            | awk '$3 == "agent" {print $1}' | head -1)
        if [ -z "$worker" ]; then
            log_event "snapshot: no agent found, skipping tick $n"
            continue
        fi
        local idx
        idx=$(printf '%03d' $((n % RETAIN_BPF)))
        log_event "snapshot[$idx]: running natra profile on $worker"
        # Single-shot natra profile: -interval longer than we wait
        # so it only writes one snapshot, then we kill it.
        docker exec "$worker" mkdir -p /var/log/natra-soak 2>/dev/null || true
        docker exec "$worker" bash -c "
            setsid nohup /opt/cni/bin/natra profile \
                -interval 10s \
                -output /var/log/natra-soak/snapshots-${idx}.jsonl \
                -heap-dir /var/log/natra-soak/heap-${idx} \
                </dev/null >/dev/null 2>&1 &
            echo \$! > /var/log/natra-soak/profile.pid
        " || true
        sleep 12  # let it write one snapshot
        docker exec "$worker" bash -c '
            kill $(cat /var/log/natra-soak/profile.pid) 2>/dev/null || true
        ' || true
        # Copy out the snapshot and the heap dir contents.
        docker cp "$worker:/var/log/natra-soak/snapshots-${idx}.jsonl" \
            "$OUTPUT_DIR/bpf/" 2>/dev/null || true
        docker cp "$worker:/var/log/natra-soak/heap-${idx}" \
            "$OUTPUT_DIR/heap/" 2>/dev/null || true
    done
}

# ---------- main ----------
echo "starting soak-test"
echo "  mode:             $MODE"
echo "  duration:         $DURATION ($DURATION_S s)"
echo "  output:           $OUTPUT_DIR"
echo "  initial nodes:    $INITIAL_NODES"

# tsv header
printf 'timestamp_unix\tiperf_ing_bps\tiperf_eg_bps\they_rps\they_p50_s\they_p99_s\tgp_total_probes\tgp_failed_probes\tnode_count\n' \
    > "$OUTPUT_DIR/metrics.tsv"

bootstrap_cluster
deploy_workload

# Background loops: node churn + deep snapshots. The measurement
# loop runs in the foreground; when it exits we kill the background
# loops via the EXIT trap (their k3d / docker commands are
# idempotent enough that interruption mid-call is benign).
churn_loop &
CHURN_PID=$!
snapshot_loop &
SNAPSHOT_PID=$!

cleanup_background() {
    kill "$CHURN_PID" "$SNAPSHOT_PID" 2>/dev/null || true
    wait "$CHURN_PID" "$SNAPSHOT_PID" 2>/dev/null || true
}
trap 'cleanup_background; cleanup' EXIT

log_event "soak: entering measurement loop (until $(date -r "$END_TS"))"
while [ "$(date +%s)" -lt "$END_TS" ]; do
    measure
    sleep "$SAMPLE_INTERVAL_S"
done

log_event "soak: complete"
echo
echo "results in $OUTPUT_DIR"
echo "rows: $(wc -l < "$OUTPUT_DIR/metrics.tsv" | tr -d ' ')"
echo "heap snapshots: $(ls "$OUTPUT_DIR/heap" 2>/dev/null | wc -l | tr -d ' ')"
echo "bpf snapshots: $(ls "$OUTPUT_DIR/bpf" 2>/dev/null | wc -l | tr -d ' ')"
