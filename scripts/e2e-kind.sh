#!/bin/bash
set -euo pipefail

CLUSTER_NAME="rke2-sr-e2e"
IMAGE_NAME="rke2-security-responder:e2e"
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT_DIR="$(dirname "${SCRIPT_DIR}")"

cleanup() {
    echo "Cleaning up..."
    kind delete cluster --name "${CLUSTER_NAME}" 2>/dev/null || true
}
trap cleanup EXIT

echo "=== Creating kind cluster ==="
kind create cluster --name "${CLUSTER_NAME}" --wait 120s

echo "=== Deploying mock responder endpoint ==="
kubectl apply -f - <<EOF
apiVersion: v1
kind: Pod
metadata:
  name: mock-responder
  namespace: kube-system
  labels:
    app: mock-responder
spec:
  containers:
  - name: mock
    image: hashicorp/http-echo
    args: ["-text={\"versions\":[{\"name\":\"v1.30.1\",\"releaseDate\":\"2024-01-01\"}],\"requestIntervalInMinutes\":480}"]
    ports:
    - containerPort: 5678
---
apiVersion: v1
kind: Service
metadata:
  name: mock-responder
  namespace: kube-system
spec:
  selector:
    app: mock-responder
  ports:
  - port: 80
    targetPort: 5678
EOF

echo "=== Waiting for mock responder to be ready ==="
kubectl wait --for=condition=ready pod/mock-responder -n kube-system --timeout=60s

echo "=== Building docker image ==="
docker build -t "${IMAGE_NAME}" \
    --build-arg TAG=e2e-test \
    "${ROOT_DIR}"

echo "=== Loading image into kind ==="
kind load docker-image "${IMAGE_NAME}" --name "${CLUSTER_NAME}"

echo "=== Installing helm chart (with mock endpoint) ==="
helm upgrade --install rke2-security-responder "${ROOT_DIR}/charts/rke2-security-responder" \
    --namespace kube-system \
    --set image.repository=rke2-security-responder \
    --set image.tag=e2e \
    --set check.endpoint="http://mock-responder.kube-system.svc.cluster.local:80" \
    --wait

echo "=== Creating test job ==="
kubectl create job --from=cronjob/rke2-security-responder e2e-test-run -n kube-system

echo "=== Waiting for job completion ==="
kubectl wait --for=condition=complete job/e2e-test-run -n kube-system --timeout=60s

echo "=== Checking job logs ==="
POD_NAME=$(kubectl get pods -n kube-system -l job-name=e2e-test-run -o jsonpath='{.items[0].metadata.name}')
kubectl logs -n kube-system "${POD_NAME}"

echo "=== Verifying telemetry send ==="
LOGS=$(kubectl logs -n kube-system "${POD_NAME}")

if ! echo "${LOGS}" | grep -q "data sent"; then
    echo "ERROR: Expected 'data sent' message not found"
    exit 1
fi

if ! echo "${LOGS}" | grep -q "serverNodeCount"; then
    echo "ERROR: Expected serverNodeCount in logs not found"
    exit 1
fi

if ! echo "${LOGS}" | grep -q "response received"; then
    echo "ERROR: Expected 'response received' message not found"
    exit 1
fi

echo "=== E2E test passed ==="
