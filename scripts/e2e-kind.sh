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

echo "=== Building docker image ==="
docker build -t "${IMAGE_NAME}" \
    --build-arg TAG=e2e-test \
    "${ROOT_DIR}"

echo "=== Loading image into kind ==="
kind load docker-image "${IMAGE_NAME}" --name "${CLUSTER_NAME}"

echo "=== Installing helm chart (debug mode) ==="
helm upgrade --install rke2-security-responder "${ROOT_DIR}/charts/rke2-security-responder" \
    --namespace kube-system \
    --set image.repository=rke2-security-responder \
    --set image.tag=e2e \
    --set extraArgs="{--debug}" \
    --wait

echo "=== Creating test job ==="
kubectl create job --from=cronjob/rke2-security-responder e2e-test-run -n kube-system

echo "=== Waiting for job completion ==="
kubectl wait --for=condition=complete job/e2e-test-run -n kube-system --timeout=60s

echo "=== Checking job logs ==="
POD_NAME=$(kubectl get pods -n kube-system -l job-name=e2e-test-run -o jsonpath='{.items[0].metadata.name}')
kubectl logs -n kube-system "${POD_NAME}"

echo "=== Verifying telemetry collection ==="
LOGS=$(kubectl logs -n kube-system "${POD_NAME}")

if ! echo "${LOGS}" | grep -q "debug mode: skipping send"; then
    echo "ERROR: Expected debug mode message not found"
    exit 1
fi

if ! echo "${LOGS}" | grep -q "serverNodeCount"; then
    echo "ERROR: Expected serverNodeCount in payload not found"
    exit 1
fi

if ! echo "${LOGS}" | grep -q "clusteruuid"; then
    echo "ERROR: Expected clusteruuid in payload not found"
    exit 1
fi

echo "=== E2E test passed ==="
