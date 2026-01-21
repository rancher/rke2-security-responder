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
  - Operating system, OS image, kernel version, architecture
  - SELinux status
  - GPU node count, vendor, and operator (if present)
  - Rancher Manager status, version, and install UUID (if managed)
  - IP stack configuration (IPv4-only, IPv6-only, or dual-stack)
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
    "operating-system": "linux",
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
    "gpu-operator-version": "v25.10.1",
    "rancher-managed": true,
    "rancher-version": "v2.9.3",
    "rancher-install-uuid": "53741f60-f208-48fc-ae81-8a969510a598",
    "ip-stack": "dual-stack"
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

### Versioning

The version is automatically derived from git tags using `git describe --tags --always --dirty`:

| Git State | Version Example | Notes |
|-----------|-----------------|-------|
| Clean tag | `v0.1.0` | Exact tag match |
| Commits after tag | `v0.1.0-5-gabcdef0` | 5 commits after v0.1.0 |
| Uncommitted changes | `v0.1.0-dirty` | Working tree modified |
| No tags | `abcdef0` | Short commit hash |
| Fallback | `dev` | Git not available |

Override with: `make build VERSION=v1.0.0`

### Development Builds

Non-release versions automatically set `extraFieldInfo.dev: true` for server-side filtering. A release version is a clean semver tag like `v1.2.3`, `v1.2.3-rc1`, or `v1.2.3+rke2r1`.

| Condition | `dev` field |
|-----------|-------------|
| Clean release tag (`v1.2.3`) | absent |
| Commits after tag (`v1.2.3-5-gabcdef`) | `true` |
| Dirty working tree (`v1.2.3-dirty`) | `true` |
| No tag (commit hash only) | `true` |
| Version contains "dev" or "test" | `true` |
| `SECURITY_RESPONDER_DEV=true` env | `true` |

Example:
```bash
# Tagged release (no dev flag)
git tag v0.1.0 && make build

# Development build (dev flag set automatically)
make build  # Uses git describe, dirty tree = dev

# Force dev flag via env
SECURITY_RESPONDER_DEV=true ./bin/security-responder
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

### Testing

#### GitHub Actions CI

The CI pipeline (`.github/workflows/ci.yml`) runs automatically on push/PR to main:

| Job | Description |
|-----|-------------|
| **Build and Test** | Compiles Go code, runs unit tests, `go vet`, format check |
| **Lint** | Runs `golangci-lint` with errcheck, govet, staticcheck, gosec |
| **Helm Chart Lint** | Validates Helm chart syntax and templates |
| **Docker Build** | Builds container images for amd64 and arm64 |
| **E2E Tests** | Deploys to a kind cluster and validates telemetry collection |

#### Local Testing

Three Makefile targets for different test scopes:

| Target | Description | Prerequisites |
|--------|-------------|---------------|
| `make test-unit` | Unit tests with race detector (~12s) | Go 1.22+ |
| `make test-e2e-kind` | E2E tests using kind cluster | Go, Docker, kind, helm, kubectl |
| `make test-e2e-rke2` | E2E tests using RKE2-in-Docker | Go, Docker (privileged), helm, kubectl |

**Unit tests** (`make test-unit`):
- Tests telemetry collection logic using a fake Kubernetes clientset
- Tests HTTP endpoint communication using `httptest` servers
- Tests helper functions (image version extraction, node role detection, SELinux status)
- No cluster or network access required

**kind E2E** (`make test-e2e-kind`):
- Creates a disposable kind cluster
- Builds and loads the container image
- Deploys via Helm in debug mode (collects but doesn't send)
- Validates that telemetry payload contains expected fields
- Automatically cleans up the cluster on exit
- Suitable for CI environments

**RKE2 E2E** (`make test-e2e-rke2`):
- Runs an actual RKE2 server in a privileged Docker container
- Tests detection of RKE2-specific components (Canal CNI, rke2-ingress-nginx)
- More resource-intensive; intended for local validation
- Requires privileged Docker access (`--privileged`)
- Not run in CI due to resource requirements

#### Running Tests

```bash
# Unit tests only (fast, no dependencies beyond Go)
make test-unit

# Full local validation with kind
make test-all

# RKE2-specific detection testing (requires privileged Docker)
make test-e2e-rke2

# Individual CI-style checks
make vet
make lint
make helm-lint
```

## License

Apache 2.0 License. See [LICENSE](LICENSE) for full text.

