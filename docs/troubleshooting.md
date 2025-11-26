# Troubleshooting

## Common Issues

### Kernel Requirements

#### Issue: "Kernel version too old"

```
ERROR: Kernel version too old (need 5.x+)
```

**Solution**: Upgrade to Linux kernel 5.x or newer for TC BPF support.

#### Issue: "tcx not available"

```
tcx not available (kernel < 6.6)
```

**Solution**: This is a warning, not an error. Natra will fall back to clsact attachment. For full tcx support, upgrade to kernel 6.6+.

**EKS users**: Use AL2023 or newer Bottlerocket AMI for kernel 6.6+.

#### Issue: "BTF not available"

```
ERROR: BTF not available - cannot generate vmlinux.h
```

**Solution**: Enable `CONFIG_DEBUG_INFO_BTF` in kernel config and rebuild kernel.

### eBPF Development

#### Issue: "Failed to load eBPF program"

**Common causes**:
1. eBPF program not compiled
2. Kernel lacks required features
3. Permission denied (not running as root)

**Debug steps**:
```bash
# Check if program exists
ls -la bpf/tc_ratelimit.o

# Verify kernel features
zgrep BPF /proc/config.gz

# Load with verbose output
sudo bpftool prog load bpf/tc_ratelimit.o /sys/fs/bpf/natra verbose
```

#### Issue: "Map creation failed"

**Solution**: Check kernel limits for BPF maps:
```bash
# View limits
ulimit -l

# Increase locked memory limit (required for BPF maps)
ulimit -l unlimited
```

#### Issue: "Cannot read /sys/kernel/debug/tracing/trace_pipe"

**Solution**: Mount debugfs:
```bash
sudo mount -t debugfs none /sys/kernel/debug
```

### CNI Plugin Issues

#### Issue: "Pod stuck in ContainerCreating"

**Common causes**:
1. CNI plugin binary missing from `/opt/cni/bin/`
2. CNI configuration invalid
3. CNI plugin crashing

**Debug steps**:
```bash
# Check if CNI binary exists on node
ssh node1 ls -la /opt/cni/bin/natra

# Check kubelet logs for CNI errors
journalctl -u kubelet | grep -i cni

# Check CNI installer DaemonSet
kubectl get pods -n kube-system -l app=natra
kubectl logs -n kube-system -l component=cni-installer
```

#### Issue: "Failed to attach to veth"

**Common causes**:
1. veth interface doesn't exist yet
2. Insufficient permissions
3. Another program already attached

**Debug steps**:
```bash
# List interfaces in Pod's network namespace
sudo ip netns exec <netns> ip link show

# Check existing tc filters
sudo tc filter show dev <veth> ingress

# Check for existing tcx programs
sudo bpftool link list
```

### AWS VPC CNI Conflicts

#### Issue: "Cannot attach tc qdisc - device busy"

**Cause**: AWS VPC CNI has hardcoded clsact at position 1.

**Solution**: Use tcx instead of clsact (requires kernel 6.6+):
```bash
# Verify tcx support
./scripts/verify-kernel.sh

# Check current attachment
sudo bpftool link list
```

If kernel < 6.6, consider:
1. Upgrade to AL2023 or newer Bottlerocket
2. Use different node group with newer kernel
3. Coordinate with VPC CNI team for position reordering

### Performance Issues

#### Issue: "High packet drop rate"

**Possible causes**:
1. Token bucket rate too low
2. Heavy hitter threshold too aggressive
3. CMS false positives

**Debug steps**:
```bash
# Dump CMS counters (on node)
sudo bpftool map dump name cms_counters

# Dump token buckets (on node)
sudo bpftool map dump name token_buckets

# Adjust configuration via annotation
kubectl annotate pod <pod-name> \
  kubernetes.io/ingress-bandwidth='{"rate":"100M","burst":"200M","cms":{"width":2048,"heavyHitterThreshold":2000}}' \
  --overwrite
```

#### Issue: "CMS false positives"

**Solution**: Increase CMS width and/or depth:
- Width: Number of counters per row (e.g., 2048, 4096)
- Depth: Number of hash functions (e.g., 5, 7)
- Trade-off: More memory vs. better accuracy

### Build Issues

#### Issue: "go mod tidy timeout"

**Solution**: Set GOPROXY:
```bash
export GOPROXY=https://proxy.golang.org,direct
go mod tidy
```

### Deployment Issues

#### Issue: "Image pull failed"

**Solution**: Ensure images are built and pushed:
```bash
# Build image
make docker-build

# For Kind: Load into cluster
kind load docker-image ghcr.io/terraboops/natra:latest

# For real cluster: Push to registry
make docker-push
```

#### Issue: "CNI installer not running"

**Debug steps**:
```bash
# Check DaemonSet status
kubectl get daemonset -n kube-system natra-installer

# Check pod logs
kubectl logs -n kube-system -l app=natra -l component=cni-installer

# Verify binary was copied
kubectl exec -n kube-system <installer-pod> -- ls -la /host/opt/cni/bin/natra
```

## Getting Help

- **GitHub Issues**: https://github.com/terraboops/natra/issues
- **Documentation**: docs/

## Diagnostic Commands

### Quick Health Check

```bash
# Verify kernel
./scripts/verify-kernel.sh

# Check CNI installer
kubectl get pods -n kube-system -l app=natra
kubectl logs -n kube-system -l app=natra --tail=50

# Check eBPF programs (on node)
sudo bpftool prog list | grep natra
sudo bpftool map list | grep natra
```

### Collect Logs

```bash
# CNI installer logs
kubectl logs -n kube-system daemonset/natra-installer > cni-installer.log

# eBPF trace (run on node)
sudo cat /sys/kernel/debug/tracing/trace_pipe > ebpf-trace.log

# Kubelet CNI logs
journalctl -u kubelet | grep -i cni > kubelet-cni.log
```
