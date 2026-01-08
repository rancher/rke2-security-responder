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
	var osImage, kernelVersion, arch, selinuxInfo, gpuVendor string

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
		if osImage == "" {
			osImage = node.Status.NodeInfo.OSImage
			kernelVersion = node.Status.NodeInfo.KernelVersion
			arch = node.Status.NodeInfo.Architecture
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
	data.ExtraFieldInfo["os"] = osImage
	data.ExtraFieldInfo["kernel"] = kernelVersion
	data.ExtraFieldInfo["arch"] = arch
	data.ExtraFieldInfo["selinux"] = selinuxInfo
	data.ExtraFieldInfo["gpu-nodes"] = gpuNodeCount
	if gpuVendor != "" {
		data.ExtraFieldInfo["gpu-vendor"] = gpuVendor
	}
	slog.Debug("collected nodes", "server", serverNodeCount, "agent", agentNodeCount, "os", osImage, "kernel", kernelVersion, "arch", arch, "selinux", selinuxInfo, "gpu-nodes", gpuNodeCount)

	slog.Debug("detecting CNI plugin")
	cniPlugin, cniVersion, err := detectCNIPlugin(ctx, clientset)
	if err != nil {
		slog.Warn("failed to detect CNI plugin", "error", err)
		cniPlugin = "unknown"
	}
	data.ExtraFieldInfo["cni-plugin"] = cniPlugin
	if cniVersion != "" {
		data.ExtraFieldInfo["cni-version"] = cniVersion
	}
	slog.Debug("detected CNI", "plugin", cniPlugin, "version", cniVersion)

	slog.Debug("detecting ingress controller")
	ingressController, ingressVersion, err := detectIngressController(ctx, clientset)
	if err != nil {
		slog.Warn("failed to detect ingress controller", "error", err)
		ingressController = "unknown"
	}
	data.ExtraFieldInfo["ingress-controller"] = ingressController
	if ingressVersion != "" {
		data.ExtraFieldInfo["ingress-version"] = ingressVersion
	}
	slog.Debug("detected ingress", "controller", ingressController, "version", ingressVersion)

	slog.Debug("detecting GPU operator")
	gpuOperator, gpuOperatorVersion := detectGPUOperator(ctx, clientset)
	if gpuOperator != "none" {
		data.ExtraFieldInfo["gpu-operator"] = gpuOperator
		if gpuOperatorVersion != "" {
			data.ExtraFieldInfo["gpu-operator-version"] = gpuOperatorVersion
		}
	}
	slog.Debug("detected GPU operator", "operator", gpuOperator, "version", gpuOperatorVersion)

	slog.Debug("detecting Rancher Manager")
	rancherManaged, rancherVersion, rancherInstallUUID := detectRancherManager(ctx, clientset)
	data.ExtraFieldInfo["rancher-managed"] = rancherManaged
	if rancherVersion != "" {
		data.ExtraFieldInfo["rancher-version"] = rancherVersion
	}
	if rancherInstallUUID != "" {
		data.ExtraFieldInfo["rancher-install-uuid"] = rancherInstallUUID
	}
	slog.Debug("detected Rancher", "managed", rancherManaged, "version", rancherVersion, "installUUID", rancherInstallUUID)

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

func extractImageVersion(image string) string {
	if idx := strings.LastIndex(image, ":"); idx != -1 {
		tag := image[idx+1:]
		if atIdx := strings.Index(tag, "@"); atIdx != -1 {
			tag = tag[:atIdx]
		}
		return tag
	}
	return ""
}

func detectCNIPlugin(ctx context.Context, clientset *kubernetes.Clientset) (string, string, error) {
	daemonSets, err := clientset.AppsV1().DaemonSets("kube-system").List(ctx, metav1.ListOptions{})
	if err != nil {
		return "", "", err
	}

	cniPatterns := map[string]string{
		"canal":   "canal",
		"flannel": "flannel",
		"calico":  "calico",
		"cilium":  "cilium",
		"weave":   "weave",
	}

	for _, ds := range daemonSets.Items {
		name := strings.ToLower(ds.Name)
		for pattern, cniName := range cniPatterns {
			if strings.Contains(name, pattern) {
				version := ""
				if len(ds.Spec.Template.Spec.Containers) > 0 {
					version = extractImageVersion(ds.Spec.Template.Spec.Containers[0].Image)
				}
				return cniName, version, nil
			}
		}
	}

	return "unknown", "", nil
}

func detectIngressController(ctx context.Context, clientset *kubernetes.Clientset) (string, string, error) {
	deployments, err := clientset.AppsV1().Deployments("kube-system").List(ctx, metav1.ListOptions{})
	if err != nil {
		return "", "", err
	}

	for _, deploy := range deployments.Items {
		name := strings.ToLower(deploy.Name)
		var ingressName string
		switch {
		case strings.Contains(name, "nginx-ingress"), strings.Contains(name, "rke2-ingress-nginx"):
			ingressName = "rke2-ingress-nginx"
		case strings.Contains(name, "traefik"):
			ingressName = "traefik"
		}
		if ingressName != "" {
			version := ""
			if len(deploy.Spec.Template.Spec.Containers) > 0 {
				version = extractImageVersion(deploy.Spec.Template.Spec.Containers[0].Image)
			}
			return ingressName, version, nil
		}
	}

	daemonSets, err := clientset.AppsV1().DaemonSets("kube-system").List(ctx, metav1.ListOptions{})
	if err == nil {
		for _, ds := range daemonSets.Items {
			name := strings.ToLower(ds.Name)
			var ingressName string
			switch {
			case strings.Contains(name, "nginx-ingress"), strings.Contains(name, "rke2-ingress-nginx"):
				ingressName = "rke2-ingress-nginx"
			case strings.Contains(name, "traefik"):
				ingressName = "traefik"
			}
			if ingressName != "" {
				version := ""
				if len(ds.Spec.Template.Spec.Containers) > 0 {
					version = extractImageVersion(ds.Spec.Template.Spec.Containers[0].Image)
				}
				return ingressName, version, nil
			}
		}
	}

	return "none", "", nil
}

func detectGPUOperator(ctx context.Context, clientset *kubernetes.Clientset) (string, string) {
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
				version := ""
				if len(ds.Spec.Template.Spec.Containers) > 0 {
					version = extractImageVersion(ds.Spec.Template.Spec.Containers[0].Image)
				}
				return operator, version
			}
		}
	}

	return "none", ""
}

func detectRancherManager(ctx context.Context, clientset *kubernetes.Clientset) (managed bool, version, installUUID string) {
	_, err := clientset.CoreV1().Namespaces().Get(ctx, "cattle-system", metav1.GetOptions{})
	if err != nil {
		return false, "", ""
	}

	deploy, err := clientset.AppsV1().Deployments("cattle-system").Get(ctx, "cattle-cluster-agent", metav1.GetOptions{})
	if err != nil {
		return true, "", ""
	}

	if len(deploy.Spec.Template.Spec.Containers) > 0 {
		container := deploy.Spec.Template.Spec.Containers[0]
		version = extractImageVersion(container.Image)
		for _, env := range container.Env {
			if env.Name == "CATTLE_INSTALL_UUID" && env.Value != "" {
				installUUID = env.Value
				break
			}
		}
	}
	return true, version, installUUID
}
