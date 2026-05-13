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
#   - Cilium as the base CNI, installed with kubeProxyReplacement +
#     bpf.masquerade + native routing. Mirrors the AWS VPC CNI +
#     Network Policy Agent dataplane shape: another eBPF program is
#     already on clsact/tcx for every pod veth before our rate-
#     limiter ever runs CNI ADD. Same adversarial coexistence that
#     the upstream bandwidth plugin can't handle on AWS today.
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
OUTPUT_DIR="/tmp/natra-soak-$(date -u +%Y%m%dT%H%M%SZ)"
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
  --output <dir>                     Result directory. Default: /tmp/natra-soak-<ts>/
  --initial-nodes <N>                Workers at startup. Default: 3.
  --node-churn-interval <seconds>    0 disables churn. Default: 600.
  --sample-interval <seconds>        Light measurement cadence. Default: 60.
  --deep-interval <seconds>          Heap/bpf snapshot cadence. Default: 1800.

Modes:
  natra     — k3d + Cilium (NPA-shaped) + natra chained after.
  vanilla   — k3d + Cilium (NPA-shaped) + upstream containernetworking bandwidth plugin.
  baseline  — k3d + Cilium (NPA-shaped) alone. The "no rate limiter" reference.

The Cilium install mirrors AWS VPC CNI + Network Policy Agent: Cilium
owns kube-proxy, owns masquerade, uses native (no-tunnel) routing,
and attaches BPF programs at every clsact/tcx hook on every pod veth
before any chained rate-limiter sees the pod. Same adversarial
coexistence profile that the upstream bandwidth plugin can't handle
on AWS today. CILIUM_VERSION env var pins the install (default 1.16.5).
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
# cilium CLI installs Cilium as the base CNI on the k3d cluster. On
# macOS: `brew install cilium-cli`. The cilium CLI handles CRDs,
# RBAC, operator + agent DaemonSet, and the post-install readiness
# probe — much less brittle than vendoring a rendered manifest.
require cilium

CILIUM_VERSION="${CILIUM_VERSION:-1.16.5}"

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
    # Pre-pull all workload images on the HOST first, then `k3d image
    # import` them into the cluster. Repeated cluster-tear-up cycles
    # would otherwise burn through Docker Hub's anonymous rate limit
    # (~100 pulls per 6h per IP). Pulling from mirror.gcr.io
    # (Google's pull-through cache for Docker Hub) sidesteps the
    # limit entirely; pulling from ghcr.io is unaffected to begin with.
    local images=(
        "mirror.gcr.io/networkstatic/iperf3:latest"
        "mirror.gcr.io/library/nginx:1.27-alpine"
        "mirror.gcr.io/bloomberg/goldpinger:3.11.2"
        "ghcr.io/terraboops/natra-perfclient:vsperf"
    )
    for img in "${images[@]}"; do
        if ! docker image inspect "$img" >/dev/null 2>&1; then
            log_event "bootstrap: pulling $img"
            docker pull "$img" 2>&1 | tail -1 || log_event "bootstrap: warn — pull $img failed (Docker Hub rate limit?)"
        fi
    done

    log_event "bootstrap: creating k3d cluster $CLUSTER with $INITIAL_NODES workers"
    # k3s args:
    #   --disable=traefik,servicelb: drop addons we don't use.
    #   --disable=kube-proxy: Cilium replaces it (NPA-shape requires
    #     Cilium to own as much of the dataplane as practical).
    #   --disable-network-policy: don't run k3s's policy controller;
    #     Cilium handles policy.
    #   --flannel-backend=none: disable k3s's default flannel CNI so
    #     Cilium can install as the base.
    # Pods stay Pending until Cilium is up — handled by the
    # cilium-status --wait below.
    k3d cluster create "$CLUSTER" \
        --agents "$INITIAL_NODES" \
        --no-lb \
        --k3s-arg "--disable=traefik,servicelb@server:0" \
        --k3s-arg "--disable=kube-proxy@server:0" \
        --k3s-arg "--disable-network-policy@server:0" \
        --k3s-arg "--flannel-backend=none@server:0" \
        --wait

    log_event "bootstrap: importing pre-pulled images into $CLUSTER"
    for img in "${images[@]}"; do
        if docker image inspect "$img" >/dev/null 2>&1; then
            k3d image import "$img" -c "$CLUSTER" 2>&1 | tail -1 || true
        fi
    done

    log_event "bootstrap: installing Cilium $CILIUM_VERSION (NPA-shaped install)"
    # NPA-shaped install: Cilium owns kube-proxy, owns masquerade,
    # uses native routing (no tunnel — mirrors AWS VPC CNI's
    # routable pod CIDR), and attaches BPF programs at every
    # clsact/tcx hook on every pod veth. By the time natra (or the
    # upstream bandwidth plugin) runs CNI ADD on a pod, Cilium's
    # BPF program is already on the qdisc — same adversarial
    # coexistence profile that breaks the upstream bandwidth plugin
    # on AWS VPC CNI + NPA today.
    #
    # The k3sHostRoot setting points Cilium at k3s's data dir for
    # cgroup discovery; without it, kubeProxyReplacement fails to
    # find the cgroup2 mount on k3d nodes.
    #
    # ipam.mode=kubernetes uses k3s's PodCIDR allocations directly
    # rather than Cilium's own pool; simpler on k3d.
    cilium install --version "$CILIUM_VERSION" \
        --set kubeProxyReplacement=true \
        --set bpf.masquerade=true \
        --set routingMode=native \
        --set autoDirectNodeRoutes=true \
        --set ipv4NativeRoutingCIDR=10.42.0.0/16 \
        --set ipam.mode=kubernetes \
        --set k8sServiceHost=auto \
        --set k8sServicePort=6443 \
        --set cgroup.autoMount.enabled=false \
        --set cgroup.hostRoot=/sys/fs/cgroup \
        --set operator.replicas=1 \
        2>&1 | tail -5
    log_event "bootstrap: waiting for Cilium to become Ready (3-5 min)"
    cilium status --wait --wait-duration 5m 2>&1 | tail -10

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
          image: mirror.gcr.io/bloomberg/goldpinger:3.11.2
          imagePullPolicy: IfNotPresent
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
            log_event "bootstrap: installing natra layer (k3s-adapted)"
            local natra_image="ghcr.io/terraboops/natra:soak"
            if ! docker image inspect "$natra_image" >/dev/null 2>&1; then
                log_event "bootstrap: building natra image $natra_image"
                docker build -q -t "$natra_image" -f "${REPO_ROOT}/deploy/docker/Dockerfile.cni" "$REPO_ROOT"
            fi
            k3d image import "$natra_image" -c "$CLUSTER"
            # k3s embeds its own CNI plugins under
            # /var/lib/rancher/k3s/data/cni/ and writes conflists to
            # /var/lib/rancher/k3s/agent/etc/cni/net.d/ — different
            # paths than kind/EKS which both use /opt/cni/bin and
            # /etc/cni/net.d. Sed-rewrite the installer manifest's
            # hostPath volumes so natra writes to k3s's actual
            # locations. The container-side mount paths stay
            # /host/opt/cni/bin and /host/etc/cni/net.d because
            # those are referenced by the install-container script.
            # NOT rewriting imagePullPolicy globally: that would also
            # break the `pause` main container, whose registry.k8s.io
            # image is never imported and needs a real pull. The
            # natra init container's image IS imported, so we only
            # need to make sure the manifest's natra-image line
            # matches the imported tag — kubelet's default
            # IfNotPresent then uses the imported copy.
            # Bin-dir sed dropped — the installer now declares
            # /opt/cni/bin, /var/lib/rancher/k3s/data/cni, and /bin
            # as separate hostPath volumes and writes natra to each.
            # Whichever bin_dir containerd is configured for, finds
            # natra. Conf-dir rewrite stays since k3s puts conflists
            # in a non-standard path.
            sed -e "s|ghcr.io/terraboops/natra:latest|${natra_image}|" \
                -e "s|path: /etc/cni/net.d|path: /var/lib/rancher/k3s/agent/etc/cni/net.d|" \
                "${REPO_ROOT}/deploy/cni-installer.yaml" | kubectl apply -f -
            kubectl -n kube-system rollout status daemonset/natra-installer --timeout=180s
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
      image: mirror.gcr.io/networkstatic/iperf3:latest
      imagePullPolicy: IfNotPresent
      args: ["-s"]
      ports:
        - containerPort: 5201
    - name: nginx
      image: mirror.gcr.io/library/nginx:1.27-alpine
      imagePullPolicy: IfNotPresent
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
    # hey emits "Requests/sec:\t<value>" with a TAB between the label
    # and the number, not a space — so -F: leaves the leading tab in
    # $2, which then leaks into the TSV and shifts every subsequent
    # column by one. Strip both spaces AND tabs from $2.
    rps=$(awk -F: '/Requests\/sec:/ {gsub(/^[ \t]*/, "", $2); print $2; exit}' "$hey_log")
    p50=$(awk '/50%% in/ {print $3; exit}' "$hey_log")
    p99=$(awk '/99%% in/ {print $3; exit}' "$hey_log")
    : "${rps:=0}"; : "${p50:=0}"; : "${p99:=0}"

    # Goldpinger /metrics scrape via in-cluster service. Sum probe
    # totals and failures across all pods. Best-effort — under node
    # churn the apiserver briefly 502s on kubectl exec; we wrap in
    # `|| true` subshells so a failed scrape just records 0 instead
    # of killing the script (set -o pipefail otherwise propagates
    # the kubectl error through the pipe).
    local gp_total gp_failed
    gp_total=$( (kubectl exec -n "$NS" soak-client -- \
        sh -c 'wget -qO- http://goldpinger.default.svc.cluster.local:80/metrics 2>/dev/null || true' 2>/dev/null || true) \
        | awk '/^goldpinger_peers_response_time_s_count/ {sum+=$NF} END{print sum+0}')
    gp_failed=$( (kubectl exec -n "$NS" soak-client -- \
        sh -c 'wget -qO- http://goldpinger.default.svc.cluster.local:80/metrics 2>/dev/null || true' 2>/dev/null || true) \
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
                | awk '$2 == "agent" {print $1}' | tail -1)
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
        # Snapshot every agent — natra pins are per-node, and the
        # workload pod might be on any of them. Picking one
        # arbitrary worker would only see that worker's pins
        # (often empty if the pod landed elsewhere).
        local workers
        workers=$(k3d node list --no-headers 2>/dev/null \
            | awk '$2 == "agent" {print $1}')
        if [ -z "$workers" ]; then
            log_event "snapshot: no agents found, skipping tick $n"
            continue
        fi
        local worker_count=0
        local worker
        for worker in $workers; do
            worker_count=$((worker_count + 1))
        done

        local idx
        idx=$(printf '%03d' $((n % RETAIN_BPF)))
        log_event "snapshot[$idx]: running natra profile on $worker_count agent(s)"
        # Single-shot natra profile: -interval longer than we wait
        # so it only writes one snapshot, then we kill it.
        for worker in $workers; do
            docker exec "$worker" mkdir -p /var/log/natra-soak 2>/dev/null || true
            # natra binary path differs by CNI base. The installer
            # writes to /opt/cni/bin on kind/EKS-shaped clusters, but
            # k3s puts its CNI plugins under
            # /var/lib/rancher/k3s/data/cni. Probe for whichever
            # exists on this worker.
            local natra_bin
            natra_bin=$(docker exec "$worker" sh -c '
                for p in /opt/cni/bin/natra /var/lib/rancher/k3s/data/cni/natra; do
                    if [ -x "$p" ]; then echo "$p"; exit 0; fi
                done
                exit 1
            ' 2>/dev/null || echo "")
            if [ -z "$natra_bin" ]; then
                log_event "snapshot[$idx]: natra not on $worker, skipping"
                continue
            fi
            # `natra profile -once` takes a single snapshot and exits.
            # k3d node containers are minimal alpine (no setsid); the
            # prior background+SIGTERM dance silently failed.
            # Foreground run blocks until the snapshot is on disk,
            # then docker cp always finds the file.
            local short
            short="${worker##*-}" # last hyphen-delimited segment, e.g. "agent-0"
            docker exec "$worker" "$natra_bin" profile \
                -once \
                -output "/var/log/natra-soak/snapshots-${idx}-${short}.jsonl" \
                -heap-dir "/var/log/natra-soak/heap-${idx}-${short}" \
                2>>"$OUTPUT_DIR/events.log" || \
                log_event "snapshot[$idx]: profile failed on $worker"
            docker cp "$worker:/var/log/natra-soak/snapshots-${idx}-${short}.jsonl" \
                "$OUTPUT_DIR/bpf/" 2>/dev/null || \
                log_event "snapshot[$idx]: cp snapshots failed for $worker"
            docker cp "$worker:/var/log/natra-soak/heap-${idx}-${short}" \
                "$OUTPUT_DIR/heap/" 2>/dev/null || \
                log_event "snapshot[$idx]: cp heap failed for $worker"
        done
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
