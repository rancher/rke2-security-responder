# AGENTS.md

## Build Commands

```bash
make build                    # Build binary (VERSION=v0.1.0 for release)
make test                     # Run tests (-v for verbose, -run TestName for single)
make lint                     # golangci-lint (see .golangci.yml)
make fmt                      # Format code
make helm-lint                # Lint Helm chart
make docker-build VERSION=v0.1.0 ARCH=amd64
make install-hooks            # Git pre-commit hooks
```

## Architecture

- **main.go**: Orchestration - env checks, k8s client init, calls telemetry
- **telemetry/telemetry.go**: `Collect()` gathers cluster metadata; `Send()` posts with retry (3x, 2s delay)
- **charts/rke2-security-responder/**: Helm chart, CronJob runs every 8h
- Read-only k8s API access via ClusterRole
- Graceful degradation in disconnected environments
- Always keep documentation (README.md), charts, code, and tests uptodate and consistent.

## Logging

Use `logrus` with structured fields:
```go
logrus.WithField("version", v).Info("starting")
logrus.WithFields(logrus.Fields{"server": n, "agent": m}).Debug("collected")
logrus.WithError(err).Warn("failed")
```

`logrus`, even though marked upstream as in maintenance mode, is required due to component standardization with other Rancher projects.

## Error Handling

Wrap with context: `fmt.Errorf("failed to X: %w", err)`

Non-critical errors: log warning and continue.

## Security

- Never log secrets/tokens/credentials
- HTTP requests must use context with timeout
- Use HTTPS and TLS wherever possible
- Container runs as non-root (UID 65532) from scratch image
- Disable via RKE2's `disable: [rke2-security-responder]` config

## Testing

Table-driven tests with subtests. Use `k8s.io/client-go/kubernetes/fake` for k8s mocks.

## Pre-commit Hooks

Runs: go fmt, go vet, golangci-lint, helm lint. **Never bypass with `--no-verify`.**

**Never commit or push automatically. Ask for human review.**

## Dependencies

Go 1.22+, k8s.io/client-go v0.35.0, logrus v1.9.4
