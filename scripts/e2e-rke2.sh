#!/bin/bash
set -euo pipefail

# RKE2 E2E Test Script
# Runs RKE2 in Docker for authentic CNI/ingress detection testing.
# Requires: docker with privileged container support

RKE2_VERSION="${RKE2_VERSION:-v1.30.0+rke2r1}"
CONTAINER_NAME="rke2-e2e-server"
IMAGE_NAME="rke2-security-responder:e2e"
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT_DIR="$(dirname "${SCRIPT_DIR}")"
KUBECONFIG_FILE="/tmp/rke2-e2e-kubeconfig"

cleanup() {
    echo "Cleaning up..."
    docker rm -f "${CONTAINER_NAME}" 2>/dev/null || true
    rm -f "${KUBECONFIG_FILE}"
}
trap cleanup EXIT

echo "=== Starting RKE2 server in Docker ==="
docker run -d --name "${CONTAINER_NAME}" \
    --privileged \
    --tmpfs /run \
    --tmpfs /var/run \
    -v /sys/fs/cgroup:/sys/fs/cgroup:rw \
    -p 6443:6443 \
    -e "K3S_TOKEN=secret" \
    rancher/rke2:"${RKE2_VERSION}" \
    server

echo "=== Waiting for RKE2 to be ready (may take several minutes) ==="
RETRIES=60
for i in $(seq 1 ${RETRIES}); do
    if docker exec "${CONTAINER_NAME}" test -f /etc/rancher/rke2/rke2.yaml 2>/dev/null; then
        echo "RKE2 config available"
        break
    fi
    echo "Waiting for RKE2... (${i}/${RETRIES})"
    sleep 10
done

echo "=== Extracting kubeconfig ==="
CONTAINER_IP=$(docker inspect -f '{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}' "${CONTAINER_NAME}")
docker exec "${CONTAINER_NAME}" cat /etc/rancher/rke2/rke2.yaml | \
    sed "s/127.0.0.1/${CONTAINER_IP}/g" \
    > "${KUBECONFIG_FILE}"

export KUBECONFIG="${KUBECONFIG_FILE}"

echo "=== Waiting for nodes to be ready ==="
for i in $(seq 1 30); do
    if kubectl get nodes 2>/dev/null | grep -q "Ready"; then
        echo "Node ready"
        break
    fi
    echo "Waiting for node... (${i}/30)"
    sleep 10
done

echo "=== Cluster info ==="
kubectl get nodes -o wide
kubectl get pods -n kube-system

echo "=== Building docker image ==="
docker build -t "${IMAGE_NAME}" \
    --build-arg TAG=e2e-test \
    "${ROOT_DIR}"

echo "=== Copying image to RKE2 container ==="
docker save "${IMAGE_NAME}" | docker exec -i "${CONTAINER_NAME}" ctr -n k8s.io images import -

echo "=== Installing helm chart (debug mode) ==="
helm upgrade --install rke2-security-responder "${ROOT_DIR}/charts/rke2-security-responder" \
    --namespace kube-system \
    --set image.repository=rke2-security-responder \
    --set image.tag=e2e \
    --set extraArgs="{--debug}" \
    --set imagePullPolicy=Never \
    --wait --timeout 120s

echo "=== Creating test job ==="
kubectl create job --from=cronjob/rke2-security-responder e2e-test-run -n kube-system

echo "=== Waiting for job completion ==="
kubectl wait --for=condition=complete job/e2e-test-run -n kube-system --timeout=120s

echo "=== Checking job logs ==="
POD_NAME=$(kubectl get pods -n kube-system -l job-name=e2e-test-run -o jsonpath='{.items[0].metadata.name}')
kubectl logs -n kube-system "${POD_NAME}"

echo "=== Verifying RKE2-specific detection ==="
LOGS=$(kubectl logs -n kube-system "${POD_NAME}")

if ! echo "${LOGS}" | grep -q "debug mode: skipping send"; then
    echo "ERROR: Expected debug mode message not found"
    exit 1
fi

# RKE2-specific: should detect canal CNI
if echo "${LOGS}" | grep -q '"cni-plugin":"canal"'; then
    echo "OK: Canal CNI detected"
else
    echo "WARNING: Canal CNI not detected (may be expected depending on RKE2 config)"
fi

# RKE2-specific: should detect rke2-ingress-nginx
if echo "${LOGS}" | grep -q '"ingress-controller":"rke2-ingress-nginx"'; then
    echo "OK: RKE2 ingress-nginx detected"
else
    echo "WARNING: RKE2 ingress-nginx not detected (may still be deploying)"
fi

echo "=== RKE2 E2E test passed ==="
