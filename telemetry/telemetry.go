package telemetry

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	semver "github.com/Masterminds/semver/v3"
	"github.com/sirupsen/logrus"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
)

const (
	DefaultEndpoint = "https://security-responder.version.rke2.io/v1/checkupgrade"
	defaultTimeout  = 30 * time.Second
	maxRetries      = 3
	retryDelay      = 2 * time.Second
)

var helmChartGVR = schema.GroupVersionResource{
	Group:    "helm.cattle.io",
	Version:  "v1",
	Resource: "helmcharts",
}

type Data struct {
	AppVersion     string                 `json:"appVersion"`
	ExtraTagInfo   map[string]string      `json:"extraTagInfo"`
	ExtraFieldInfo map[string]interface{} `json:"extraFieldInfo"`
}

type Response struct {
	Versions                 []Version `json:"versions"`
	RequestIntervalInMinutes int       `json:"requestIntervalInMinutes"`
}

type Version struct {
	Name                 string            `json:"name"`
	ReleaseDate          string            `json:"releaseDate"`
	MinUpgradableVersion string            `json:"minUpgradableVersion,omitempty"`
	Tags                 []string          `json:"tags,omitempty"`
	ExtraInfo            map[string]string `json:"extraInfo,omitempty"`
}

func Collect(ctx context.Context, clientset kubernetes.Interface, dynClient dynamic.Interface, mode string) (*Data, error) {
	data := &Data{
		ExtraTagInfo:   make(map[string]string),
		ExtraFieldInfo: make(map[string]interface{}),
	}
	data.ExtraFieldInfo["mode"] = mode
	isMinimal := mode == "minimal"

	logrus.Debug("collecting server version")
	versionInfo, err := clientset.Discovery().ServerVersion()
	if err != nil {
		return nil, fmt.Errorf("failed to get server version: %w", err)
	}
	data.AppVersion = versionInfo.GitVersion
	data.ExtraTagInfo["kubernetesVersion"] = versionInfo.GitVersion
	logrus.WithField("version", versionInfo.GitVersion).Debug("collected version")

	logrus.Debug("collecting cluster UUID from kube-system namespace")
	namespace, err := clientset.CoreV1().Namespaces().Get(ctx, "kube-system", metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("failed to get kube-system namespace: %w", err)
	}
	data.ExtraTagInfo["clusteruuid"] = string(namespace.UID)
	logrus.WithField("uuid", namespace.UID).Debug("collected cluster UUID")

	logrus.Debug("collecting node information")
	nodes, err := clientset.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("failed to list nodes: %w", err)
	}

	var serverNodeCount, agentNodeCount, gpuNodeCount int
	var serverCPU, agentCPU, serverMemory, agentMemory int64
	var operatingSystem, osImage, kernelVersion, arch, selinuxInfo, gpuVendor string

	gpuResources := []corev1.ResourceName{"nvidia.com/gpu", "amd.com/gpu", "intel.com/gpu"}
	gpuVendorMap := map[corev1.ResourceName]string{
		"nvidia.com/gpu": "nvidia",
		"amd.com/gpu":    "amd",
		"intel.com/gpu":  "intel",
	}

	nodeInfoConsistent := true
	for _, node := range nodes.Items {
		cpu := node.Status.Allocatable.Cpu().MilliValue()
		mem := node.Status.Allocatable.Memory().Value()
		if isControlPlaneNode(&node) {
			serverNodeCount++
			serverCPU += cpu
			serverMemory += mem
		} else {
			agentNodeCount++
			agentCPU += cpu
			agentMemory += mem
		}
		if osImage == "" {
			operatingSystem = node.Status.NodeInfo.OperatingSystem
			osImage = node.Status.NodeInfo.OSImage
			kernelVersion = node.Status.NodeInfo.KernelVersion
			arch = node.Status.NodeInfo.Architecture
		} else if node.Status.NodeInfo.OperatingSystem != operatingSystem ||
			node.Status.NodeInfo.OSImage != osImage ||
			node.Status.NodeInfo.KernelVersion != kernelVersion ||
			node.Status.NodeInfo.Architecture != arch {
			nodeInfoConsistent = false
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

	if isMinimal {
		data.ExtraFieldInfo["serverNodeCount"] = -1
		data.ExtraFieldInfo["agentNodeCount"] = -1
		data.ExtraFieldInfo["gpuNodeCount"] = -1
		data.ExtraFieldInfo["serverCPU"] = int64(-1)
		data.ExtraFieldInfo["agentCPU"] = int64(-1)
		data.ExtraFieldInfo["serverMemory"] = int64(-1)
		data.ExtraFieldInfo["agentMemory"] = int64(-1)
	} else {
		data.ExtraFieldInfo["serverNodeCount"] = serverNodeCount
		data.ExtraFieldInfo["agentNodeCount"] = agentNodeCount
		data.ExtraFieldInfo["serverCPU"] = serverCPU
		data.ExtraFieldInfo["agentCPU"] = agentCPU
		data.ExtraFieldInfo["serverMemory"] = serverMemory
		data.ExtraFieldInfo["agentMemory"] = agentMemory
		data.ExtraFieldInfo["gpuNodeCount"] = gpuNodeCount
	}
	data.ExtraFieldInfo["operating-system"] = operatingSystem
	data.ExtraFieldInfo["os"] = osImage
	data.ExtraFieldInfo["kernel"] = kernelVersion
	data.ExtraFieldInfo["arch"] = arch
	data.ExtraFieldInfo["selinux"] = selinuxInfo
	data.ExtraFieldInfo["node-info-consistent"] = nodeInfoConsistent
	if gpuVendor != "" {
		data.ExtraFieldInfo["gpu-vendor"] = gpuVendor
	}
	logrus.WithFields(logrus.Fields{
		"server":       serverNodeCount,
		"agent":        agentNodeCount,
		"serverCPU":    serverCPU,
		"agentCPU":     agentCPU,
		"serverMemory": serverMemory,
		"agentMemory":  agentMemory,
		"gpuNodeCount": gpuNodeCount,
	}).Debug("collected nodes")

	logrus.Debug("collecting kube-system workloads")
	kubeSystemDS, err := clientset.AppsV1().DaemonSets("kube-system").List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("failed to list kube-system daemonsets: %w", err)
	}
	kubeSystemDeploy, err := clientset.AppsV1().Deployments("kube-system").List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("failed to list kube-system deployments: %w", err)
	}

	logrus.Debug("detecting CNI plugin")
	cniPlugin, cniVersion := detectCNIPlugin(kubeSystemDS.Items)
	data.ExtraFieldInfo["cni-plugin"] = cniPlugin
	if cniVersion != "" {
		data.ExtraFieldInfo["cni-version"] = cniVersion
	}
	logrus.WithFields(logrus.Fields{"plugin": cniPlugin, "version": cniVersion}).Debug("detected CNI")

	logrus.Debug("detecting ingress controller")
	ingressController, ingressVersion := detectIngressController(kubeSystemDeploy.Items, kubeSystemDS.Items)
	data.ExtraFieldInfo["ingress-controller"] = ingressController
	if ingressVersion != "" {
		data.ExtraFieldInfo["ingress-version"] = ingressVersion
	}
	logrus.WithFields(logrus.Fields{"controller": ingressController, "version": ingressVersion}).Debug("detected ingress")

	logrus.Debug("detecting GPU operator")
	gpuOperator, gpuOperatorVersion := detectGPUOperator(ctx, clientset)
	if gpuOperator != "none" {
		data.ExtraFieldInfo["gpu-operator"] = gpuOperator
		if gpuOperatorVersion != "" {
			data.ExtraFieldInfo["gpu-operator-version"] = gpuOperatorVersion
		}
	}
	logrus.WithFields(logrus.Fields{"operator": gpuOperator, "version": gpuOperatorVersion}).Debug("detected GPU operator")

	logrus.Debug("detecting Rancher Manager")
	rancherManaged, rancherVersion, rancherInstallUUID := detectRancherManager(ctx, clientset)
	data.ExtraFieldInfo["rancher-managed"] = rancherManaged
	if isMinimal {
		data.ExtraFieldInfo["rancher-version"] = ""
		data.ExtraFieldInfo["rancher-install-uuid"] = ""
	} else {
		if rancherVersion != "" {
			data.ExtraFieldInfo["rancher-version"] = rancherVersion
		}
		if rancherInstallUUID != "" {
			data.ExtraFieldInfo["rancher-install-uuid"] = rancherInstallUUID
		}
	}
	logrus.WithFields(logrus.Fields{"managed": rancherManaged, "version": rancherVersion, "installUUID": rancherInstallUUID}).Debug("detected Rancher")

	logrus.Debug("detecting Prime distribution flag")
	prime, sysDefaultRegistry := detectPrime(ctx, dynClient)
	data.ExtraFieldInfo["rancher-prime"] = prime
	if sysDefaultRegistry != "" {
		data.ExtraFieldInfo["system-default-registry"] = sysDefaultRegistry
	}
	logrus.WithFields(logrus.Fields{"prime": prime, "systemDefaultRegistry": sysDefaultRegistry}).Debug("detected Prime")

	logrus.Debug("detecting IP stack configuration")
	ipStack := detectIPStack(ctx, clientset)
	data.ExtraFieldInfo["ip-stack"] = ipStack
	logrus.WithField("ip-stack", ipStack).Debug("detected IP stack")

	return data, nil
}

func Send(ctx context.Context, data *Data, endpoint string) (*Response, error) {
	jsonData, err := json.Marshal(data)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal data: %w", err)
	}

	logrus.WithField("endpoint", endpoint).Info("sending data")
	logrus.WithField("size", len(jsonData)).Debug("request payload")

	client := &http.Client{Timeout: defaultTimeout}

	var lastErr error
	for attempt := 1; attempt <= maxRetries; attempt++ {
		if attempt > 1 {
			delay := time.Duration(attempt-1) * retryDelay
			logrus.WithFields(logrus.Fields{"attempt": attempt, "max": maxRetries, "delay": delay}).Info("retrying")
			time.Sleep(delay)
		}

		req, err := http.NewRequestWithContext(ctx, "POST", endpoint, bytes.NewBuffer(jsonData))
		if err != nil {
			return nil, fmt.Errorf("failed to create request: %w", err)
		}
		req.Header.Set("Content-Type", "application/json")

		resp, err := client.Do(req)
		if err != nil {
			lastErr = fmt.Errorf("failed to send request: %w", err)
			logrus.WithField("attempt", attempt).WithError(lastErr).Warn("attempt failed")
			continue
		}

		body, err := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if err != nil {
			lastErr = fmt.Errorf("failed to read response: %w", err)
			logrus.WithField("attempt", attempt).WithError(lastErr).Warn("attempt failed")
			continue
		}

		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			lastErr = fmt.Errorf("unexpected status code: %d", resp.StatusCode)
			logrus.WithField("attempt", attempt).WithError(lastErr).Warn("attempt failed")
			continue
		}

		var response Response
		if err := json.Unmarshal(body, &response); err != nil {
			logrus.WithError(err).Warn("failed to parse response")
			logrus.WithField("attempt", attempt).Info("data sent")
			return nil, nil
		}

		newer := filterNewerVersions(response.Versions, data.AppVersion)
		current := filterCurrentVersion(response.Versions, data.AppVersion)
		logrus.WithFields(logrus.Fields{
			"versions":        len(response.Versions),
			"newer":           len(newer),
			"intervalMinutes": response.RequestIntervalInMinutes,
		}).Info("response received")
		logRecommendations(newer, current)

		logrus.WithField("attempt", attempt).Info("data sent")
		return &response, nil
	}

	return nil, lastErr
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

func parseCVEs(raw string) []string {
	if raw == "" {
		return nil
	}
	var cves []string
	for _, s := range strings.Split(raw, ",") {
		if cve := strings.TrimSpace(s); cve != "" {
			cves = append(cves, cve)
		}
	}
	return cves
}

// isNewerVersion compares versions by release date when available. This prevents
// a semver-higher but older-released build from being treated as a newer upgrade.
// When the date is unavailable or equal, it falls back to semver and RKE2 rebuild
// suffix comparison for consistency with the existing version-ordering rules.
func isNewerVersion(candidate, current string) bool {
	c, err := semver.NewVersion(candidate)
	if err != nil {
		return false
	}
	cur, err := semver.NewVersion(current)
	if err != nil {
		return true // unparseable current → show all
	}

	if c.GreaterThan(cur) {
		return true
	}
	if cur.GreaterThan(c) {
		return false
	}
	return rke2BuildNumber(c.Metadata()) > rke2BuildNumber(cur.Metadata())
}

func isVersionNewer(candidate, current Version) bool {
	candidateDate, candOK := parseVersionDate(candidate.ReleaseDate)
	currentDate, curOK := parseVersionDate(current.ReleaseDate)

	if candOK && curOK {
		if !candidateDate.After(currentDate) {
			return false
		}

		candidateSemver, candSemErr := semver.NewVersion(candidate.Name)
		currentSemver, curSemErr := semver.NewVersion(current.Name)
		if candSemErr == nil && curSemErr == nil {
			return candidateSemver.Major() > currentSemver.Major() ||
				(candidateSemver.Major() == currentSemver.Major() && candidateSemver.Minor() > currentSemver.Minor()) ||
				(candidateSemver.Major() == currentSemver.Major() && candidateSemver.Minor() == currentSemver.Minor() && candidateSemver.Patch() > currentSemver.Patch()) ||
				(candidateSemver.Major() == currentSemver.Major() && candidateSemver.Minor() == currentSemver.Minor() && candidateSemver.Patch() == currentSemver.Patch() && rke2BuildNumber(candidateSemver.Metadata()) > rke2BuildNumber(currentSemver.Metadata()))
		}
		return true
	}

	candidateSemver, candSemErr := semver.NewVersion(candidate.Name)
	currentSemver, curSemErr := semver.NewVersion(current.Name)
	if candSemErr == nil && curSemErr == nil {
		if currentSemver.GreaterThan(candidateSemver) {
			return false
		}
		if candidateSemver.GreaterThan(currentSemver) {
			return true
		}
		return rke2BuildNumber(candidateSemver.Metadata()) > rke2BuildNumber(currentSemver.Metadata())
	}

	if candOK && !curOK {
		return true
	}
	if !candOK && curOK {
		return false
	}
	return isNewerVersion(candidate.Name, current.Name)
}

func parseVersionDate(raw string) (time.Time, bool) {
	if raw == "" {
		return time.Time{}, false
	}
	t, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return time.Time{}, false
	}
	return t, true
}

func rke2BuildNumber(metadata string) int {
	suffix, ok := strings.CutPrefix(metadata, "rke2r")
	if !ok {
		return 0
	}
	n, err := strconv.Atoi(suffix)
	if err != nil {
		return 0
	}
	return n
}

func filterNewerVersions(versions []Version, current string) []Version {
	currentVersion := Version{Name: current}
	for i := range versions {
		if versions[i].Name == current {
			currentVersion = versions[i]
			break
		}
	}

	out := make([]Version, 0, len(versions))
	for _, v := range versions {
		if v.Name == current {
			continue
		}
		if isVersionNewer(v, currentVersion) {
			out = append(out, v)
		}
	}
	return sortVersionsByReleaseDateDescending(out)
}

func sortVersionsByReleaseDateDescending(versions []Version) []Version {
	out := append([]Version(nil), versions...)
	sort.Slice(out, func(i, j int) bool {
		iTime, iErr := time.Parse(time.RFC3339, out[i].ReleaseDate)
		jTime, jErr := time.Parse(time.RFC3339, out[j].ReleaseDate)
		switch {
		case iErr != nil && jErr != nil:
			return out[i].Name < out[j].Name
		case iErr != nil:
			return false
		case jErr != nil:
			return true
		default:
			return iTime.After(jTime)
		}
	})
	return out
}

func filterCurrentVersion(versions []Version, current string) *Version {
	for i := range versions {
		if versions[i].Name == current {
			return &versions[i]
		}
	}
	return nil
}

func logRecommendations(newer []Version, current *Version) {
	latest := false
	if current != nil {
		for _, tag := range current.Tags {
			if strings.EqualFold(tag, "latest") {
				latest = true
				logrus.Warnf("The installed RKE2 version %s is the latest released version", current.Name)
				break
			}
		}

		if !latest {
			cves := parseCVEs(current.ExtraInfo["cves"])
			if len(cves) > 0 {
				logrus.Warnf("The installed RKE2 version %s includes CVEs. These are the %d most relevant: %s. Please upgrade to a newer version to fix security vulnerabilities", current.Name, len(cves), strings.Join(cves, ", "))
			}
		}
	}

	for _, v := range newer {
		fields := logrus.Fields{"version": v.Name, "releaseDate": v.ReleaseDate}
		if url := v.ExtraInfo["releaseNotesURL"]; url != "" {
			fields["releaseNotesURL"] = url
		}
		logrus.WithFields(fields).Info("available version")
	}
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

func detectCNIPlugin(daemonSets []appsv1.DaemonSet) (string, string) {
	cniPatterns := map[string]string{
		"canal":   "canal",
		"flannel": "flannel",
		"calico":  "calico",
		"cilium":  "cilium",
		"weave":   "weave",
	}

	for _, ds := range daemonSets {
		name := strings.ToLower(ds.Name)
		for pattern, cniName := range cniPatterns {
			if strings.Contains(name, pattern) {
				version := ""
				if len(ds.Spec.Template.Spec.Containers) > 0 {
					version = extractImageVersion(ds.Spec.Template.Spec.Containers[0].Image)
				}
				return cniName, version
			}
		}
	}

	return "unknown", ""
}

func detectIngressController(deployments []appsv1.Deployment, daemonSets []appsv1.DaemonSet) (string, string) {
	for _, deploy := range deployments {
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
			return ingressName, version
		}
	}

	for _, ds := range daemonSets {
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
			return ingressName, version
		}
	}

	return "none", ""
}

func detectGPUOperator(ctx context.Context, clientset kubernetes.Interface) (string, string) {
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

func detectRancherManager(ctx context.Context, clientset kubernetes.Interface) (managed bool, version, installUUID string) {
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

// detectIPStack determines the cluster's IP stack configuration from the kubernetes service.
func detectIPStack(ctx context.Context, clientset kubernetes.Interface) string {
	kubeSvc, err := clientset.CoreV1().Services("default").Get(ctx, "kubernetes", metav1.GetOptions{})
	if err != nil {
		logrus.WithError(err).Warn("failed to get kubernetes service for IP stack detection")
		return "unknown"
	}
	if len(kubeSvc.Spec.IPFamilies) == 0 {
		return "unknown"
	}
	hasIPv4, hasIPv6 := false, false
	for _, f := range kubeSvc.Spec.IPFamilies {
		switch f {
		case corev1.IPv4Protocol:
			hasIPv4 = true
		case corev1.IPv6Protocol:
			hasIPv6 = true
		}
	}
	switch {
	case hasIPv4 && hasIPv6:
		return "dual-stack"
	case hasIPv4:
		return "ipv4-only"
	case hasIPv6:
		return "ipv6-only"
	default:
		return "unknown"
	}
}

// detectPrime reads global.prime.enabled and global.systemDefaultRegistry from
// HelmChart spec.set, which RKE2 injects at bootstrap (rancher/rke2#9859).
// Returns "unknown" when the CRD/RBAC/charts are absent or no chart carries the
// key (pre-PR-9859 cluster). Positive wins on HA mismatch.
func detectPrime(ctx context.Context, dynClient dynamic.Interface) (string, string) {
	if dynClient == nil {
		return "unknown", ""
	}
	list, err := dynClient.Resource(helmChartGVR).Namespace("kube-system").List(ctx, metav1.ListOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) || meta.IsNoMatchError(err) || apierrors.IsForbidden(err) {
			logrus.WithError(err).Debug("HelmChart CRD or RBAC unavailable; prime=unknown")
			return "unknown", ""
		}
		logrus.WithError(err).Warn("failed to list HelmCharts for Prime detection")
		return "unknown", ""
	}
	state := "unknown"
	var registry string
	for i := range list.Items {
		set, _, _ := unstructured.NestedMap(list.Items[i].Object, "spec", "set")
		if state != "true" {
			if s, ok := set["global.prime.enabled"].(string); ok {
				if b, err := strconv.ParseBool(s); err == nil {
					switch {
					case b:
						state = "true"
					case state == "unknown":
						state = "false"
					}
				}
			}
		}
		if registry == "" {
			if r, ok := set["global.systemDefaultRegistry"].(string); ok {
				registry = r
			}
		}
		if state == "true" && registry != "" {
			break
		}
	}
	return state, registry
}
