# Install paths

natra is a CNI plugin binary — a single Linux ELF that kubelet/containerd
invokes on every pod sandbox create/delete. The DaemonSet in the quick
start is one way to get that binary onto every node and the CNI
conflist patched, but it's not the only way. Pick the path that fits
how you manage node images.

The three supported paths:

| Path                  | Binary placement              | Conflist chained by   | Node-image touched? |
|-----------------------|-------------------------------|-----------------------|---------------------|
| **DaemonSet**         | Init container `install`s the ELF onto every CNI bin dir the distro might use (`/opt/cni/bin/`, `/var/lib/rancher/k3s/data/cni/`, and `/bin/`) | Init container patches `/etc/cni/net.d/*.conflist` | No |
| **Baked into image**  | Built into your node image at `/opt/cni/bin/natra` (e.g. via Packer / image-builder) | Static `/etc/cni/net.d/00-natra-*.conflist` shipped with the image | Yes |
| **Manual**            | Operator scp's / configures via Ansible | Operator runs `natra install-cni-chain /etc/cni/net.d/` once per node | Yes |

All three end up with natra at a path containerd's CNI search
finds, file caps set, and a chained conflist next to the main
CNI's. The DaemonSet path writes the binary to multiple
candidate paths because different k8s distros configure their
container runtime with different `cni.bin_dir` values — installing
everywhere is cheaper than introspecting per-distro.

## 1. DaemonSet (default)

The `deploy/cni-installer.yaml` manifest creates a DaemonSet that
runs a privileged init container on every node. The init container:

- Mounts bpffs at `/sys/fs/bpf` (idempotent — skips if already mounted).
- Copies `/usr/local/bin/natra` from the image to every CNI bin
  candidate path (`/opt/cni/bin/natra`,
  `/var/lib/rancher/k3s/data/cni/natra`, `/bin/natra`).
- Sets file caps with
  `setcap cap_bpf,cap_net_admin,cap_perfmon,cap_sys_resource+ep`.
- Waits for at least one `*.conflist` to appear under
  `/etc/cni/net.d/`, then runs `natra install-cni-chain` to write the
  chained `00-natra-*.conflist` sibling.
- Exits.

The main container is `registry.k8s.io/pause:3.10` — no work, just
keeps the Pod present so the DaemonSet controller is happy.

```bash
kubectl apply -f deploy/cni-installer.yaml
```

When to use: most clusters. Standard k8s plumbing, no node-image
changes, easy to upgrade by `kubectl set image`.

When *not* to use: managed services where DaemonSets carry overhead
you'd rather avoid (one extra `pause` container per node), or
declarative-image clusters (Talos, k0s with image overlays, certain
GitOps-managed bare-metal setups) where the right place to inject
node-level binaries is the image itself.

## 2. Baked into the node image

Bypass the DaemonSet entirely by shipping natra in your node image.
Two pieces to include:

```dockerfile
# In your node image Dockerfile / Packer template / image-builder
# config:
COPY --from=ghcr.io/terraboops/natra:latest /usr/local/bin/natra /opt/cni/bin/natra
RUN setcap cap_bpf,cap_net_admin,cap_perfmon,cap_sys_resource+ep /opt/cni/bin/natra
COPY 00-natra-cilium.conflist /etc/cni/net.d/00-natra-cilium.conflist
```

The conflist is a vanilla chained version of whatever main CNI you
run. Generate it once on a representative node with:

```bash
natra install-cni-chain /etc/cni/net.d/
```

…then copy the resulting `00-natra-*.conflist` into your image source.

Also make sure bpffs is mounted at `/sys/fs/bpf` at boot. Modern
systemd handles this automatically; if you're rolling your own init,
add `bpffs /sys/fs/bpf bpf defaults 0 0` to `/etc/fstab` or the
equivalent.

When to use: declarative-image clusters, or any setup where adding a
DaemonSet feels like a wart.

When *not* to use: clusters where node image churn is expensive
(every natra upgrade needs an image rebuild and rolling node
replacement).

## 3. Manual install on a running node

For one-off deployments, lab clusters, or a single node you're
debugging on:

```bash
# Get the binary onto the node
scp natra <node>:/opt/cni/bin/natra

# Set caps and chain natra into the existing CNI config
ssh <node> sudo bash -c '
    setcap cap_bpf,cap_net_admin,cap_perfmon,cap_sys_resource+ep /opt/cni/bin/natra
    mountpoint -q /sys/fs/bpf || mount -t bpf bpffs /sys/fs/bpf
    /opt/cni/bin/natra install-cni-chain /etc/cni/net.d/
'
```

`install-cni-chain` reads every `*.conflist` in the directory that
doesn't start with `00-natra-`, appends natra's entry (with
`capabilities.bandwidth: true`), and writes a sibling
`00-natra-<original>.conflist`. The originals stay untouched.
Idempotent — re-running with no source changes is a no-op.

When to use: single-node debugging, evaluating natra on an existing
cluster without committing to a DaemonSet.

When *not* to use: anything more than a handful of nodes — there's no
upgrade path. Use Ansible / a similar config-management tool if you
need this pattern at scale, or use one of the other two paths.

## Removing natra

Whichever path you used, removal is the same three things:

```bash
# 1. Stop kubelet from invoking natra
rm /etc/cni/net.d/00-natra-*.conflist

# 2. Remove the binary
rm /opt/cni/bin/natra

# 3. Drop any leftover BPF state from previously-attached pods
rm -rf /sys/fs/bpf/natra/
```

Existing pods keep their BPF programs attached (the kernel holds the
references via clsact qdiscs / pinned TCX links); they'll lose
rate-limiting only when the pod restarts and the new sandbox doesn't
chain natra. For an immediate clean-up of currently-attached
programs, kill the pods.

If you used the DaemonSet, also `kubectl delete -f
deploy/cni-installer.yaml`.
