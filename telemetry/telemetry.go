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

	versionInfo, err := clientset.Discovery().ServerVersion()
	if err != nil {
		return nil, fmt.Errorf("failed to get server version: %w", err)
	}
	data.AppVersion = versionInfo.GitVersion
	data.ExtraTagInfo["kubernetesVersion"] = versionInfo.GitVersion

	namespace, err := clientset.CoreV1().Namespaces().Get(ctx, "kube-system", metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("failed to get kube-system namespace: %w", err)
	}
	data.ExtraTagInfo["clusteruuid"] = string(namespace.UID)

	nodes, err := clientset.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("failed to list nodes: %w", err)
	}

	var serverNodeCount, agentNodeCount int
	var osInfo, selinuxInfo string

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
	}

	data.ExtraFieldInfo["serverNodeCount"] = serverNodeCount
	data.ExtraFieldInfo["agentNodeCount"] = agentNodeCount
	data.ExtraFieldInfo["os"] = osInfo
	data.ExtraFieldInfo["selinux"] = selinuxInfo

	cniPlugin, err := detectCNIPlugin(ctx, clientset)
	if err != nil {
		slog.Warn("failed to detect CNI plugin", "error", err)
		cniPlugin = "unknown"
	}
	data.ExtraFieldInfo["cni-plugin"] = cniPlugin

	ingressController, err := detectIngressController(ctx, clientset)
	if err != nil {
		slog.Warn("failed to detect ingress controller", "error", err)
		ingressController = "unknown"
	}
	data.ExtraFieldInfo["ingress-controller"] = ingressController

	return data, nil
}

func Send(ctx context.Context, data *Data, endpoint string) error {
	jsonData, err := json.Marshal(data)
	if err != nil {
		return fmt.Errorf("failed to marshal telemetry data: %w", err)
	}

	slog.Info("sending telemetry", "endpoint", endpoint)

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

		slog.Info("telemetry sent", "attempt", attempt)
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
