package telemetry

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

const (
	DefaultEndpoint = "https://security-responder.rke2.io/v1/check"
	defaultTimeout  = 30 * time.Second
	maxRetries      = 3
	retryDelay      = 2 * time.Second
)

type Data struct {
	AppVersion     string                 `json:"appVersion"`
	ExtraTagInfo   map[string]string      `json:"extraTagInfo"`
	ExtraFieldInfo map[string]interface{} `json:"extraFieldInfo"`
}

func Collect(ctx context.Context, clientset *kubernetes.Clientset) (*Data, error) {
	data := &Data{
		ExtraTagInfo:   make(map[string]string),
		ExtraFieldInfo: make(map[string]interface{}),
	}

	slog.Debug("collecting server version")
	versionInfo, err := clientset.Discovery().ServerVersion()
	if err != nil {
		return nil, fmt.Errorf("failed to get server version: %w", err)
	}
	data.AppVersion = versionInfo.GitVersion
	data.ExtraTagInfo["kubernetesVersion"] = versionInfo.GitVersion
	slog.Debug("collected version", "version", versionInfo.GitVersion)

	slog.Debug("collecting cluster UUID from kube-system namespace")
	namespace, err := clientset.CoreV1().Namespaces().Get(ctx, "kube-system", metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("failed to get kube-system namespace: %w", err)
	}
	data.ExtraTagInfo["clusteruuid"] = string(namespace.UID)
	slog.Debug("collected cluster UUID", "uuid", namespace.UID)

	slog.Debug("collecting node information")
	nodes, err := clientset.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("failed to list nodes: %w", err)
	}

	var serverNodeCount, agentNodeCount, gpuNodeCount int
	var osInfo, selinuxInfo, gpuVendor string

	gpuResources := []corev1.ResourceName{"nvidia.com/gpu", "amd.com/gpu", "intel.com/gpu"}
	gpuVendorMap := map[corev1.ResourceName]string{
		"nvidia.com/gpu": "nvidia",
		"amd.com/gpu":    "amd",
		"intel.com/gpu":  "intel",
	}

	for _, node := range nodes.Items {
		if isControlPlaneNode(&node) {
			serverNodeCount++
		} else {
			agentNodeCount++
		}
		if osInfo == "" {
			osInfo = node.Status.NodeInfo.OSImage
		}
		if selinuxInfo == "" {
			selinuxInfo = getSELinuxStatus(&node)
		}
		for _, res := range gpuResources {
			if qty, ok := node.Status.Allocatable[res]; ok {
				if count, _ := qty.AsInt64(); count > 0 {
					gpuNodeCount++
					if gpuVendor == "" {
						gpuVendor = gpuVendorMap[res]
					}
					break
				}
			}
		}
	}

	data.ExtraFieldInfo["serverNodeCount"] = serverNodeCount
	data.ExtraFieldInfo["agentNodeCount"] = agentNodeCount
	data.ExtraFieldInfo["os"] = osInfo
	data.ExtraFieldInfo["selinux"] = selinuxInfo
	data.ExtraFieldInfo["gpu-nodes"] = gpuNodeCount
	if gpuVendor != "" {
		data.ExtraFieldInfo["gpu-vendor"] = gpuVendor
	}
	slog.Debug("collected nodes", "server", serverNodeCount, "agent", agentNodeCount, "os", osInfo, "selinux", selinuxInfo, "gpu-nodes", gpuNodeCount)

	slog.Debug("detecting CNI plugin")
	cniPlugin, err := detectCNIPlugin(ctx, clientset)
	if err != nil {
		slog.Warn("failed to detect CNI plugin", "error", err)
		cniPlugin = "unknown"
	}
	data.ExtraFieldInfo["cni-plugin"] = cniPlugin
	slog.Debug("detected CNI", "plugin", cniPlugin)

	slog.Debug("detecting ingress controller")
	ingressController, err := detectIngressController(ctx, clientset)
	if err != nil {
		slog.Warn("failed to detect ingress controller", "error", err)
		ingressController = "unknown"
	}
	data.ExtraFieldInfo["ingress-controller"] = ingressController
	slog.Debug("detected ingress", "controller", ingressController)

	slog.Debug("detecting GPU operator")
	gpuOperator := detectGPUOperator(ctx, clientset)
	if gpuOperator != "none" {
		data.ExtraFieldInfo["gpu-operator"] = gpuOperator
	}
	slog.Debug("detected GPU operator", "operator", gpuOperator)

	return data, nil
}

func Send(ctx context.Context, data *Data, endpoint string) error {
	jsonData, err := json.Marshal(data)
	if err != nil {
		return fmt.Errorf("failed to marshal data: %w", err)
	}

	slog.Info("sending data", "endpoint", endpoint)
	slog.Debug("request payload", "size", len(jsonData))

	client := &http.Client{Timeout: defaultTimeout}

	var lastErr error
	for attempt := 1; attempt <= maxRetries; attempt++ {
		if attempt > 1 {
			delay := time.Duration(attempt-1) * retryDelay
			slog.Info("retrying", "attempt", attempt, "max", maxRetries, "delay", delay)
			time.Sleep(delay)
		}

		req, err := http.NewRequestWithContext(ctx, "POST", endpoint, bytes.NewBuffer(jsonData))
		if err != nil {
			return fmt.Errorf("failed to create request: %w", err)
		}
		req.Header.Set("Content-Type", "application/json")

		resp, err := client.Do(req)
		if err != nil {
			lastErr = fmt.Errorf("failed to send request: %w", err)
			slog.Warn("attempt failed", "attempt", attempt, "error", lastErr)
			continue
		}
		_ = resp.Body.Close()

		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			lastErr = fmt.Errorf("unexpected status code: %d", resp.StatusCode)
			slog.Warn("attempt failed", "attempt", attempt, "error", lastErr)
			continue
		}

		slog.Debug("response received", "status", resp.StatusCode)
		slog.Info("data sent", "attempt", attempt)
		return nil
	}

	return lastErr
}

func isControlPlaneNode(node *corev1.Node) bool {
	_, hasControlPlaneLabel := node.Labels["node-role.kubernetes.io/control-plane"]
	_, hasMasterLabel := node.Labels["node-role.kubernetes.io/master"]
	return hasControlPlaneLabel || hasMasterLabel
}

// getSELinuxStatus determines SELinux status from node labels.
// SELinux detection is limited from within containers; this is a best-effort
// approach. Returns "unknown" if not determinable.
func getSELinuxStatus(node *corev1.Node) string {
	if selinux, ok := node.Labels["security.alpha.kubernetes.io/selinux"]; ok {
		if selinux == "enabled" {
			return "enabled"
		}
		return "disabled"
	}
	return "unknown"
}

func detectCNIPlugin(ctx context.Context, clientset *kubernetes.Clientset) (string, error) {
	daemonSets, err := clientset.AppsV1().DaemonSets("kube-system").List(ctx, metav1.ListOptions{})
	if err != nil {
		return "", err
	}

	for _, ds := range daemonSets.Items {
		name := strings.ToLower(ds.Name)
		switch {
		case strings.Contains(name, "canal"):
			return "canal", nil
		case strings.Contains(name, "flannel"):
			return "flannel", nil
		case strings.Contains(name, "calico"):
			return "calico", nil
		case strings.Contains(name, "cilium"):
			return "cilium", nil
		case strings.Contains(name, "weave"):
			return "weave", nil
		}
	}

	return "unknown", nil
}

func detectIngressController(ctx context.Context, clientset *kubernetes.Clientset) (string, error) {
	deployments, err := clientset.AppsV1().Deployments("kube-system").List(ctx, metav1.ListOptions{})
	if err != nil {
		return "", err
	}

	for _, deploy := range deployments.Items {
		name := strings.ToLower(deploy.Name)
		switch {
		case strings.Contains(name, "nginx-ingress"), strings.Contains(name, "rke2-ingress-nginx"):
			return "rke2-ingress-nginx", nil
		case strings.Contains(name, "traefik"):
			return "traefik", nil
		}
	}

	daemonSets, err := clientset.AppsV1().DaemonSets("kube-system").List(ctx, metav1.ListOptions{})
	if err == nil {
		for _, ds := range daemonSets.Items {
			name := strings.ToLower(ds.Name)
			switch {
			case strings.Contains(name, "nginx-ingress"), strings.Contains(name, "rke2-ingress-nginx"):
				return "rke2-ingress-nginx", nil
			case strings.Contains(name, "traefik"):
				return "traefik", nil
			}
		}
	}

	return "none", nil
}

func detectGPUOperator(ctx context.Context, clientset *kubernetes.Clientset) string {
	gpuNamespaces := map[string]string{
		"gpu-operator":              "nvidia-gpu-operator",
		"kube-amd-gpu":              "amd-gpu-operator",
		"inteldeviceplugins-system": "intel-device-plugins",
	}

	for ns, operator := range gpuNamespaces {
		daemonSets, err := clientset.AppsV1().DaemonSets(ns).List(ctx, metav1.ListOptions{})
		if err != nil {
			continue
		}
		for _, ds := range daemonSets.Items {
			name := strings.ToLower(ds.Name)
			if strings.Contains(name, "device-plugin") || strings.Contains(name, "driver") {
				return operator
			}
		}
	}

	return "none"
}
