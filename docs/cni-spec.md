# CNI Specification Compliance

Natra implements the [Container Network Interface (CNI) Specification](https://www.cni.dev/docs/spec/) version 0.4.0.

## CNI Commands

Natra supports all required CNI commands:

### ADD

Invoked when a Pod is created. The plugin:
1. Reads Pod network configuration from stdin (JSON)
2. Parses `kubernetes.io/ingress-bandwidth` annotation
3. Loads eBPF program onto Pod's veth interface
4. Configures CMS and Token Bucket maps
5. Returns success JSON to stdout

**Input** (stdin):
```json
{
  "cniVersion": "0.4.0",
  "name": "natra",
  "type": "natra",
  "runtimeConfig": {
    "podAnnotations": {
      "kubernetes.io/ingress-bandwidth": "10M"
    }
  },
  "prevResult": {
    "interfaces": [
      {
        "name": "eth0",
        "sandbox": "/var/run/netns/pod-123"
      }
    ],
    "ips": [
      {
        "version": "4",
        "interface": 0,
        "address": "10.244.1.5/24"
      }
    ]
  }
}
```

**Output** (stdout):
```json
{
  "cniVersion": "0.4.0",
  "interfaces": [
    {
      "name": "eth0",
      "sandbox": "/var/run/netns/pod-123"
    }
  ],
  "ips": [
    {
      "version": "4",
      "interface": 0,
      "address": "10.244.1.5/24"
    }
  ]
}
```

### DEL

Invoked when a Pod is deleted. The plugin:
1. Detaches eBPF program from veth interface
2. Cleans up BPF maps
3. Returns success

**Input** (stdin):
Same as ADD command

**Output** (stdout):
Empty (success indicated by exit code 0)

### CHECK

Invoked to verify the Pod's network is still correctly configured. The plugin:
1. Verifies eBPF program is still attached
2. Checks BPF maps are accessible
3. Returns success if configuration is valid

**Input** (stdin):
Same as ADD command

**Output** (stdout):
Empty (success indicated by exit code 0)

### VERSION

Returns supported CNI versions.

**Input** (stdin):
Empty

**Output** (stdout):
```json
{
  "cniVersion": "0.4.0",
  "supportedVersions": ["0.3.0", "0.3.1", "0.4.0"]
}
```

## Input Format

Natra expects CNI input on stdin in JSON format per the CNI spec.

### Required Fields

- `cniVersion`: CNI spec version (0.4.0)
- `name`: Network name
- `type`: "natra"

### Optional Fields

- `runtimeConfig.podAnnotations`: Pod annotations (includes bandwidth limits)
- `prevResult`: Result from previous plugin in chain

### Annotation Parsing

Natra parses the `kubernetes.io/ingress-bandwidth` annotation from `runtimeConfig.podAnnotations`:

**Simple format**:
```
"kubernetes.io/ingress-bandwidth": "10M"
```

**Extended format**:
```json
"kubernetes.io/ingress-bandwidth": "{\"rate\":\"10M\",\"burst\":\"20M\",\"cms\":{\"width\":1024,\"depth\":4,\"heavyHitterThreshold\":1000}}"
```

## Output Format

Natra returns CNI output on stdout in JSON format.

### Success Response

**For ADD**:
```json
{
  "cniVersion": "0.4.0",
  "interfaces": [...],
  "ips": [...],
  "dns": {...}
}
```

Natra passes through the `prevResult` from the previous CNI plugin (typically the primary CNI like AWS VPC CNI).

**For DEL, CHECK**:
Empty output with exit code 0

**For VERSION**:
```json
{
  "cniVersion": "0.4.0",
  "supportedVersions": ["0.3.0", "0.3.1", "0.4.0"]
}
```

### Error Response

On error, Natra prints error JSON to stdout and exits with non-zero code:

```json
{
  "cniVersion": "0.4.0",
  "code": 7,
  "msg": "Failed to load eBPF program",
  "details": "kernel version 5.4 does not support tcx"
}
```

**However**, Natra implements **fail-open** design: it logs errors but returns success (exit 0) to avoid blocking Pod startup. Errors are only returned for critical CNI spec violations.

## Environment Variables

CNI plugins receive configuration via environment variables:

- `CNI_COMMAND`: Command to execute (ADD, DEL, CHECK, VERSION)
- `CNI_CONTAINERID`: Container ID
- `CNI_NETNS`: Network namespace path
- `CNI_IFNAME`: Interface name to create/delete
- `CNI_ARGS`: Extra arguments
- `CNI_PATH`: Paths to search for CNI plugins

Natra uses these per the CNI spec.

## Chaining

Natra is designed to run as a **chained plugin** after the primary CNI plugin (e.g., AWS VPC CNI).

### Example CNI Configuration

```json
{
  "cniVersion": "0.4.0",
  "name": "aws-cni",
  "plugins": [
    {
      "type": "aws-cni",
      "...": "primary CNI config"
    },
    {
      "type": "natra",
      "capabilities": {
        "bandwidth": true
      }
    }
  ]
}
```

The primary CNI sets up Pod networking (IP allocation, routing). Natra receives the result and adds eBPF rate limiting.

## Capabilities

Natra declares the `bandwidth` capability:

```json
{
  "capabilities": {
    "bandwidth": true
  }
}
```

This signals to kubelet that Natra can process bandwidth annotations.

## Network Namespace Handling

Natra operates on the Pod's network namespace specified by `CNI_NETNS`:

1. Open network namespace at `CNI_NETNS` path
2. Find veth interface matching `CNI_IFNAME`
3. Attach eBPF program to veth interface in Pod's netns
4. Close network namespace

The eBPF program persists on the veth even after Natra exits.

## Error Handling

### Fail-Open Philosophy

Natra prioritizes Pod availability over rate limiting enforcement:

- **Critical errors**: Return CNI error (e.g., malformed stdin JSON)
- **Operational errors**: Log warning, return success (e.g., eBPF load failure)

### Error Categories

**Critical** (return CNI error):
- Invalid CNI command
- Malformed stdin JSON
- Missing required fields

**Operational** (log and succeed):
- eBPF program load failure
- Kernel too old for tcx
- Malformed bandwidth annotation
- BPF map creation failure

This ensures Natra never becomes a single point of failure.

## Compliance Testing

Natra passes the official CNI plugin test suite:

```bash
# Run CNI plugin tester
go test -v ./test/cni/
```

Tests verify:
- ✓ ADD command creates network configuration
- ✓ DEL command cleans up configuration
- ✓ CHECK command verifies configuration
- ✓ VERSION command returns supported versions
- ✓ Invalid input returns proper errors
- ✓ Chaining works with other CNI plugins

## References

- [CNI Specification](https://www.cni.dev/docs/spec/)
- [CNI Plugin Conventions](https://www.cni.dev/docs/conventions/)
- [Container Network Interface GitHub](https://github.com/containernetworking/cni)
