# Build stage - using hardened build base similar to RKE2
FROM rancher/hardened-build-base:v1.25.5b1 AS builder

ARG BUILDARCH
ARG TAG=v0.1.0
ENV ARCH=${BUILDARCH:-amd64}

RUN apk --no-cache add \
    bash \
    git \
    tzdata \
    ca-certificates

WORKDIR /workspace

# Copy go mod files
COPY go.mod go.sum ./
RUN go mod download

# Copy source code
COPY main.go ./
COPY telemetry/ ./telemetry/

# Build with hardening flags
RUN CGO_ENABLED=0 \
    GOOS=linux \
    GOARCH=${ARCH} \
    go build \
    -ldflags "-s -w -X main.Version=${TAG}" \
    -trimpath \
    -o security-responder \
    .

# Final stage - using scratch for minimal image size
FROM scratch

# Add ca-certificates for HTTPS requests
COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/

# Add timezone data (optional, for time operations)
COPY --from=builder /usr/share/zoneinfo /usr/share/zoneinfo

# Add passwd for non-root user (create minimal passwd file)
COPY --from=builder /etc/passwd /etc/passwd

WORKDIR /

COPY --from=builder /workspace/security-responder /usr/local/bin/security-responder

USER 65532:65532

ENTRYPOINT ["/usr/local/bin/security-responder"]

