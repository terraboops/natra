# Natra Architecture

## Overview

Natra is a Kubernetes CNI plugin that provides intelligent TCP rate limiting using eBPF. It's a true drop-in replacement for the standard CNI bandwidth plugin, using standard `kubernetes.io/ingress-bandwidth` annotations with advanced heavy hitter detection.

## Components

### 1. CNI Plugin

**Location**: `cmd/natra`, `pkg/cni`

The CNI plugin is invoked directly by kubelet during Pod network setup and:
- Receives Pod metadata and annotations via stdin (CNI spec)
- Parses `kubernetes.io/ingress-bandwidth` annotation for rate limit configuration
- Loads eBPF programs onto Pod's veth interface
- Configures CMS and Token Bucket parameters in eBPF maps
- Returns success to kubelet (fail-open design)
- Persists eBPF program on veth after plugin exits

**Key Design Decision**: CNI Plugin vs Operator
- **Simpler**: No operator, no CRDs, no gRPC, no Kubernetes API calls
- **Faster**: Direct kubelet invocation, no reconciliation loops
- **Smaller**: ~3,100 SLOC vs ~6,630 SLOC
- **More Reliable**: Fail-open design, never blocks Pod startup
- **Drop-in**: Uses standard Kubernetes annotations

### 2. eBPF Programs

**Location**: `bpf/`

The eBPF programs implement a two-stage rate limiting pipeline attached to each Pod's veth:

**Stage 1: Count-Min Sketch (CMS)**
- Tracks all flows within a Pod with constant memory (width × depth array)
- Identifies heavy hitters exceeding threshold among Pod's flows
- Memory-efficient: 1024×4 = 4,096 counters for ANY number of flows
- Multiple hash functions (typically 3-5) for accuracy

**Stage 2: Token Bucket**
- Rate limits only heavy hitters identified by CMS
- Precise rate control (tokens/sec, burst size)
- Per-flow buckets stored in BPF_MAP_TYPE_HASH
- Periodic cleanup of stale flow buckets

**Why this hybrid approach?**
- **Differentiator**: Standard CNI bandwidth plugin rate limits ALL traffic uniformly
- **Natra**: CMS detects heavy hitters WITHIN Pod's flows, only rate limits those
- **Result**: Legitimate traffic flows freely, malicious heavy hitters throttled
- **Memory**: Constant O(1) space for detection, O(k) space for k heavy hitters

### 3. Deployment (DaemonSet)

**Location**: `deploy/cni-installer.yaml`

A DaemonSet installer copies the CNI binary to `/opt/cni/bin/` on all nodes:
- Runs in `kube-system` namespace
- Uses `hostPath` volume to access `/opt/cni/bin/`
- Privileged container for file system access
- Tolerates all taints to run on all nodes

## CNI Flow

```
┌─────────────────────────────────────────────────────────┐
│ 1. User creates Pod with annotation                    │
│    kubernetes.io/ingress-bandwidth: "10M"               │
└────────────────────┬────────────────────────────────────┘
                     │
                     ▼
┌─────────────────────────────────────────────────────────┐
│ 2. Kubelet invokes natra CNI plugin via stdin          │
│    (JSON with Pod network config + annotations)         │
└────────────────────┬────────────────────────────────────┘
                     │
                     ▼
┌─────────────────────────────────────────────────────────┐
│ 3. CNI plugin parses annotation                        │
│    - Bandwidth limit, CMS config, Token Bucket config   │
└────────────────────┬────────────────────────────────────┘
                     │
                     ▼
┌─────────────────────────────────────────────────────────┐
│ 4. CNI plugin loads eBPF program                       │
│    - Attach to Pod's veth interface (tcx or clsact)    │
│    - Configure CMS maps (width, depth, threshold)       │
│    - Configure Token Bucket maps (rate, burst)          │
└────────────────────┬────────────────────────────────────┘
                     │
                     ▼
┌─────────────────────────────────────────────────────────┐
│ 5. CNI plugin exits successfully                       │
│    - eBPF program persists, attached to veth            │
│    - Returns success JSON to kubelet                    │
│    - Pod startup continues normally                     │
└─────────────────────────────────────────────────────────┘
```

## Fail-Open Design

**Critical**: CNI plugin NEVER blocks Pod startup.

- If eBPF load fails → log error, return success
- If kernel too old → log warning, return success
- If annotation malformed → log warning, use defaults, return success
- If CMS map creation fails → log error, return success

**Philosophy**: Pod availability > rate limiting enforcement

This ensures Natra never becomes a single point of failure in the cluster.

## Annotation Format

**Standard CNI bandwidth plugin format** (for compatibility):
```yaml
apiVersion: v1
kind: Pod
metadata:
  annotations:
    kubernetes.io/ingress-bandwidth: "10M"
```

**Extended Natra format** (for advanced configuration):
```yaml
apiVersion: v1
kind: Pod
metadata:
  annotations:
    kubernetes.io/ingress-bandwidth: |
      {
        "rate": "10M",
        "burst": "20M",
        "cms": {
          "width": 1024,
          "depth": 4,
          "heavyHitterThreshold": 1000
        }
      }
```

## Data Flow (Per-Pod)

```
                              ┌──────────────┐
                              │     Pod      │
                              └──────┬───────┘
                                     │
                                     ▼
                              ┌──────────────┐
                              │     veth     │
                              │  (Pod side)  │
                              └──────┬───────┘
                                     │
                ┌────────────────────┼────────────────────┐
                │    eBPF Program    │ (tcx/clsact)       │
                │    (per-Pod)       │                    │
                └────────────────────┼────────────────────┘
                                     │
        ┌────────────────────────────┼────────────────────────┐
        │                            │                        │
        ▼                            ▼                        ▼
 ┌────────────┐             ┌────────────┐           ┌────────────┐
 │  CMS Map   │             │Token Bucket│           │  Metrics   │
 │  (detect   │             │    (limit  │           │  (export)  │
 │   heavy    │             │    heavy   │           │            │
 │  hitters)  │             │  hitters)  │           │            │
 └────────────┘             └────────────┘           └────────────┘
```

## Kernel Requirements

- **Minimum**: Linux 5.x with TC BPF support (clsact fallback)
- **Recommended**: Linux 6.6+ for tcx support
- **EKS**: Requires AL2023 or newer Bottlerocket AMI

## tcx vs clsact

**tcx (Traffic Control eXpress) - Preferred for kernel 6.6+:**
- Uses BPF links instead of qdiscs
- Coexists with AWS VPC CNI Network Policies (no clsact position conflicts)
- Loaded via cilium/ebpf `link.AttachTCX()` API

**clsact - Fallback for older kernels:**
- Traditional qdisc-based TC attachment
- Works on kernel 5.x+
- May conflict with AWS VPC CNI on some configurations

**Runtime Detection:**
- CNI plugin detects kernel version at Pod creation time
- Automatically selects tcx (6.6+) or clsact (<6.6)
- Logs attachment method for debugging

## AWS VPC CNI Compatibility

**Challenge**: AWS VPC CNI uses hardcoded clsact eBPF at position 1 in tc chain.

**Solution**: tcx operates independently from clsact (different attachment mechanism at same hook points), avoiding position conflicts.

## Future Enhancements

- IPv6 support
- UDP support (if relevant)
- Dynamic CMS resizing based on load
- eBPF CO-RE for kernel portability
- XDP attachment option (earlier in packet path)
- Web UI for visualizing heavy hitters
- Integration with ebpf_exporter for metrics
