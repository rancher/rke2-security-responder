# rke2-security-responder

RKE2 component for in-cluster security metadata collection and CVE notifications.

## Overview

The RKE2 Security Responder is a Kubernetes component that collects non-personally identifiable cluster metadata and sends it to a security check endpoint. This helps RKE2 maintainers understand deployment patterns relevant to security advisories and helps users stay informed about security updates.

## Architecture

Based on [ADR 010-security-responder](https://github.com/rancher/rke2/blob/master/docs/adrs/010-security-responder.md), this component:

- Runs as a CronJob in the `kube-system` namespace
- Executes thrice daily (every 8 hours: `0 */8 * * *`)
- Collects cluster metadata including:
  - Kubernetes version
  - Cluster UUID (based on kube-system namespace UID)
  - Node counts (control plane vs agent nodes)
  - CNI plugin in use
  - Ingress controller in use
  - Operating system, kernel version, architecture
  - SELinux status
  - GPU node count, vendor, and operator (if present)
- Sends data to a configurable endpoint
- Fails gracefully in disconnected environments
- Minimal resource overhead

## Data Collection

Example payload structure:

```json
{
  "appVersion": "v1.32.2+rke2r1",
  "extraTagInfo": {
    "kubernetesVersion": "v1.32.2",
    "clusteruuid": "53741f60-f208-48fc-ae81-8a969510a598"
  },
  "extraFieldInfo": {
    "serverNodeCount": 3,
    "agentNodeCount": 2,
    "os": "SLE Micro 6.1",
    "kernel": "6.4.0-150600.23.47-default",
    "arch": "amd64",
    "selinux": "enabled",
    "cni-plugin": "cilium",
    "cni-version": "v1.16.5",
    "ingress-controller": "rke2-ingress-nginx",
    "ingress-version": "v1.12.1",
    "gpu-nodes": 2,
    "gpu-vendor": "nvidia",
    "gpu-operator": "nvidia-gpu-operator",
    "gpu-operator-version": "v25.10.1"
  }
}
```

The `clusteruuid` is completely random (the UUID of the `kube-system` namespace) and does not expose any privacy concerns.

## Configuration

### Disabling the Security Responder

To disable the security responder, add the following to your RKE2 configuration:

```yaml
# /etc/rancher/rke2/config.yaml
disable:
  - rke2-security-responder
```

### Helm Chart Values

The component is packaged as a Helm chart with the following configurable values:

- `enabled`: Whether the security responder is enabled (default: `true`)
- `schedule`: CronJob schedule (default: `"0 */8 * * *"`)
- `check.endpoint`: Security check endpoint URL (default: `"https://security-responder.rke2.io/v1/check"`)
- `check.disabled`: Disable security check (default: `false`)
- `image.repository`: Container image repository (default: `"rancher/rke2-security-responder"`)
- `image.tag`: Container image tag (default: `"v0.1.0"`)
- `resources`: Resource limits and requests

## Development

### Building

Build the Go binary with version information:

```bash
make build VERSION=v0.1.0
```

Or directly with Go:

```bash
CGO_ENABLED=0 go build -ldflags "-s -w -X main.Version=v0.1.0" -trimpath -o security-responder main.go
```

Build the container image (uses `rancher/hardened-build-base` and `scratch` for minimal size):

```bash
make docker-build VERSION=v0.1.0 ARCH=amd64
```

Build multi-architecture images:

```bash
make docker-build-multi VERSION=v0.1.0
```

### Testing the Helm Chart

Lint the chart:

```bash
helm lint charts/rke2-security-responder
```

Template the chart:

```bash
helm template rke2-security-responder charts/rke2-security-responder \
  --namespace kube-system
```

Install the chart:

```bash
helm install rke2-security-responder charts/rke2-security-responder \
  --namespace kube-system \
  --create-namespace
```

## License

Apache 2.0 License. See [LICENSE](LICENSE) for full text.

