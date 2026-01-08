# Contributing to RKE2 Security Responder

Thank you for your interest in contributing to the RKE2 Security Responder!

## Development Setup

### Prerequisites

- Go 1.22 or later
- Docker (for building container images)
- Helm 3.x (for chart development)
- Kubernetes cluster (for testing)

### Building

Build the Go binary:

```bash
make build
```

Or directly with Go:

```bash
go build -o security-responder main.go
```

### Testing

Run Go tests:

```bash
make test
```

Lint the Go code:

```bash
make vet
go vet ./...
```

Format the code:

```bash
make fmt
```

### Helm Chart Development

Lint the Helm chart:

```bash
make helm-lint
```

Template the chart:

```bash
make helm-template
```

Test installation:

```bash
helm install rke2-security-responder ./charts/rke2-security-responder \
  --namespace kube-system \
  --dry-run --debug
```

### Container Image

Build the container image:

```bash
make docker-build
```

## Code Standards

- Follow standard Go conventions and formatting (use `go fmt`)
- Add comments for exported functions and types
- Keep functions small and focused
- Handle errors appropriately
- Write secure code (no secrets in logs, validate inputs, etc.)

## Security Considerations

- Never log sensitive data (cluster UUIDs are considered non-sensitive)
- All telemetry data must be non-personally identifiable
- The component must fail gracefully in disconnected environments
- Use minimal RBAC permissions (only what's needed)
- Container must run as non-root with minimal privileges

## Submitting Changes

1. Fork the repository
2. Create a feature branch
3. Make your changes
4. Test thoroughly
5. Run linters and formatters
6. Submit a pull request

## Testing in a Cluster

To test the security responder in a real cluster:

1. Build and push your container image:
   ```bash
   docker build -t myregistry/rke2-security-responder:dev .
   docker push myregistry/rke2-security-responder:dev
   ```

2. Install with your custom image:
   ```bash
   helm install rke2-security-responder ./charts/rke2-security-responder \
     --namespace kube-system \
     --set image.repository=myregistry/rke2-security-responder \
     --set image.tag=dev
   ```

3. Trigger a manual run:
   ```bash
   kubectl create job --from=cronjob/rke2-security-responder test-run -n kube-system
   ```

4. Check logs:
   ```bash
   kubectl logs -n kube-system -l job-name=test-run
   ```

## Questions?

Feel free to open an issue for any questions or concerns!
