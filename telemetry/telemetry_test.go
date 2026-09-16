package telemetry

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"sync/atomic"
	"testing"

	"github.com/sirupsen/logrus"
	logtest "github.com/sirupsen/logrus/hooks/test"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

func TestIsNewerVersion(t *testing.T) {
	tests := []struct {
		name      string
		candidate string
		current   string
		expected  bool
	}{
		{"newer patch", "v1.32.5", "v1.32.4", true},
		{"older patch", "v1.32.4", "v1.32.5", false},
		{"equal", "v1.32.5", "v1.32.5", false},
		{"different patch wins over build metadata", "v1.32.5+rke2r1", "v1.32.4+rke2r1", true},
		{"cross-minor", "v1.33.0", "v1.32.5+rke2r1", true},
		{"unparseable current", "v1.32.5", "dev", true},
		{"unparseable candidate", "invalid", "v1.32.4", false},
		{"pre-release newer than older stable", "v1.32.5-rc1", "v1.32.4", true},
		{"pre-release older than same stable", "v1.32.5-rc1", "v1.32.5", false},
		{"rke2r rebuild newer", "v1.36.1+rke2r2", "v1.36.1+rke2r1", true},
		{"rke2r rebuild older", "v1.36.1+rke2r1", "v1.36.1+rke2r2", false},
		{"rke2r rebuild equal", "v1.36.1+rke2r1", "v1.36.1+rke2r1", false},
		{"rke2r rebuild numeric not lex", "v1.36.1+rke2r10", "v1.36.1+rke2r2", true},
		{"missing rebuild treated as 0", "v1.36.1", "v1.36.1+rke2r1", false},
		{"semver core dominates rebuild", "v1.36.2+rke2r1", "v1.36.1+rke2r9", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := isNewerVersion(tt.candidate, tt.current)
			if result != tt.expected {
				t.Errorf("isNewerVersion(%q, %q) = %v, want %v", tt.candidate, tt.current, result, tt.expected)
			}
		})
	}
}

func TestExtractImageVersion(t *testing.T) {
	tests := []struct {
		image    string
		expected string
	}{
		{"nginx:1.21", "1.21"},
		{"nginx:latest", "latest"},
		{"registry.example.com/nginx:v1.0.0", "v1.0.0"},
		{"nginx", ""},
		{"nginx@sha256:abc123", ""},
		{"nginx:v1.0.0@sha256:abc123", "v1.0.0"},
		{"registry.example.com:5000/nginx", ""},
		{"registry.example.com:5000/nginx:v1.0.0", "v1.0.0"},
		{"gcr.io/project/image:tag", "tag"},
		{"", ""},
	}

	for _, tt := range tests {
		t.Run(tt.image, func(t *testing.T) {
			result := extractImageVersion(tt.image)
			if result != tt.expected {
				t.Errorf("extractImageVersion(%q) = %q, want %q", tt.image, result, tt.expected)
			}
		})
	}
}

func TestIsControlPlaneNode(t *testing.T) {
	tests := []struct {
		name     string
		labels   map[string]string
		expected bool
	}{
		{
			name:     "control-plane label",
			labels:   map[string]string{"node-role.kubernetes.io/control-plane": ""},
			expected: true,
		},
		{
			name:     "master label",
			labels:   map[string]string{"node-role.kubernetes.io/master": ""},
			expected: true,
		},
		{
			name:     "both labels",
			labels:   map[string]string{"node-role.kubernetes.io/control-plane": "", "node-role.kubernetes.io/master": ""},
			expected: true,
		},
		{
			name:     "worker node",
			labels:   map[string]string{"node-role.kubernetes.io/worker": ""},
			expected: false,
		},
		{
			name:     "no labels",
			labels:   map[string]string{},
			expected: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Labels: tt.labels}}
			result := isControlPlaneNode(node)
			if result != tt.expected {
				t.Errorf("isControlPlaneNode() = %v, want %v", result, tt.expected)
			}
		})
	}
}

// newNode returns a Linux node with the given RKE2 node-args annotation.
// An empty nodeArgs omits the annotation.
func newNode(name, nodeArgs string) *corev1.Node {
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: name, Annotations: map[string]string{}},
		Status: corev1.NodeStatus{
			NodeInfo: corev1.NodeSystemInfo{OperatingSystem: "linux", OSImage: "test", KernelVersion: "5.0", Architecture: "amd64"},
		},
	}
	if nodeArgs != "" {
		node.Annotations["rke2.io/node-args"] = nodeArgs
	}
	return node
}

func TestGetSELinuxStatus(t *testing.T) {
	tests := []struct {
		name     string
		nodeArgs string
		expected string
	}{
		{"config file true", `["server","--selinux","true","--cni","canal"]`, "enabled"},
		{"config file false", `["agent","--selinux","false"]`, "disabled"},
		{"command line flag", `["server","--selinux","--write-kubeconfig-mode","0644"]`, "enabled"},
		{"command line flag last", `["agent","--selinux"]`, "enabled"},
		{"last value wins", `["server","--selinux","true","--selinux","false"]`, "disabled"},
		{"single dash flag", `["server","-selinux","--cni","canal"]`, "enabled"},
		{"single dash flag with value", `["agent","-selinux=false"]`, "disabled"},
		{"option name as value", `["server","--node-name","selinux"]`, "disabled"},
		{"option absent", `["server","--cni","cilium"]`, "disabled"},
		{"no annotation", "", ""},
		{"malformed annotation", `server --selinux`, ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := getSELinuxStatus(newNode("node-1", tt.nodeArgs)); got != tt.expected {
				t.Errorf("getSELinuxStatus() = %q, want %q", got, tt.expected)
			}
		})
	}

	envTests := []struct {
		name     string
		nodeArgs string
		nodeEnv  string
		expected string
	}{
		{"environment variable", `["server"]`, `{"RKE2_SELINUX":"true"}`, "enabled"},
		{"argument overrides environment variable", `["server","--selinux","false"]`, `{"RKE2_SELINUX":"true"}`, "disabled"},
		{"other environment variables", `["server"]`, `{"RKE2_TOKEN":"********"}`, "disabled"},
		{"environment variable without node-args", "", `{"RKE2_SELINUX":"true"}`, ""},
	}
	for _, tt := range envTests {
		t.Run(tt.name, func(t *testing.T) {
			node := newNode("node-1", tt.nodeArgs)
			node.Annotations["rke2.io/node-env"] = tt.nodeEnv
			if got := getSELinuxStatus(node); got != tt.expected {
				t.Errorf("getSELinuxStatus() = %q, want %q", got, tt.expected)
			}
		})
	}

	t.Run("windows", func(t *testing.T) {
		node := newNode("node-1", `["agent"]`)
		node.Status.NodeInfo.OperatingSystem = "windows"
		if got := getSELinuxStatus(node); got != "" {
			t.Errorf("getSELinuxStatus() = %q, want empty", got)
		}
	})
}

func TestCollect_SELinux(t *testing.T) {
	const enabled, disabled = `["server","--selinux","true"]`, `["agent"]`
	tests := []struct {
		name     string
		nodeArgs []string
		expected string
	}{
		{"all enabled", []string{enabled, enabled}, "enabled"},
		{"all disabled", []string{disabled, disabled}, "disabled"},
		{"mixed", []string{enabled, disabled, enabled}, "mixed"},
		{"nodes without annotation ignored", []string{"", enabled}, "enabled"},
		{"no annotation", []string{"", ""}, "unknown"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			objs := []runtime.Object{&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "kube-system", UID: "uuid"}}}
			for i, args := range tt.nodeArgs {
				objs = append(objs, newNode(fmt.Sprintf("node-%d", i), args))
			}
			data, err := Collect(context.Background(), fake.NewClientset(objs...), nil, "recommended")
			if err != nil {
				t.Fatalf("Collect() error = %v", err)
			}
			if data.ExtraTagInfo["selinux"] != tt.expected {
				t.Errorf("selinux = %q, want %q", data.ExtraTagInfo["selinux"], tt.expected)
			}
		})
	}
}

func TestCollect_BasicCluster(t *testing.T) {
	clientset := fake.NewClientset(
		&corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				Name: "kube-system",
				UID:  types.UID("test-cluster-uuid"),
			},
		},
		&corev1.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name:   "server-1",
				Labels: map[string]string{"node-role.kubernetes.io/control-plane": ""},
			},
			Status: corev1.NodeStatus{
				NodeInfo: corev1.NodeSystemInfo{
					OSImage:       "Ubuntu 22.04",
					KernelVersion: "5.15.0",
					Architecture:  "amd64",
				},
			},
		},
		&corev1.Node{
			ObjectMeta: metav1.ObjectMeta{Name: "agent-1"},
			Status: corev1.NodeStatus{
				NodeInfo: corev1.NodeSystemInfo{
					OSImage:       "Ubuntu 22.04",
					KernelVersion: "5.15.0",
					Architecture:  "amd64",
				},
			},
		},
		&corev1.Node{
			ObjectMeta: metav1.ObjectMeta{Name: "agent-2"},
			Status: corev1.NodeStatus{
				NodeInfo: corev1.NodeSystemInfo{
					OSImage:       "Ubuntu 22.04",
					KernelVersion: "5.15.0",
					Architecture:  "amd64",
				},
			},
		},
	)

	data, err := Collect(context.Background(), clientset, nil, "recommended")
	if err != nil {
		t.Fatalf("Collect() error = %v", err)
	}

	if data.ExtraTagInfo["mode"] != "recommended" {
		t.Errorf("mode = %q, want %q", data.ExtraTagInfo["mode"], "recommended")
	}
	if data.ExtraTagInfo["clusteruuid"] != "test-cluster-uuid" {
		t.Errorf("clusteruuid = %q, want %q", data.ExtraTagInfo["clusteruuid"], "test-cluster-uuid")
	}
	if data.ExtraFieldInfo["serverNodeCount"] != 1 {
		t.Errorf("serverNodeCount = %v, want 1", data.ExtraFieldInfo["serverNodeCount"])
	}
	if data.ExtraFieldInfo["agentNodeCount"] != 2 {
		t.Errorf("agentNodeCount = %v, want 2", data.ExtraFieldInfo["agentNodeCount"])
	}
	if data.ExtraTagInfo["os"] != "Ubuntu 22.04" {
		t.Errorf("os = %v, want Ubuntu 22.04", data.ExtraTagInfo["os"])
	}
	if data.ExtraTagInfo["arch"] != "amd64" {
		t.Errorf("arch = %v, want amd64", data.ExtraTagInfo["arch"])
	}
	if data.ExtraTagInfo["node-info-consistent"] != "true" {
		t.Errorf("node-info-consistent = %v, want true", data.ExtraTagInfo["node-info-consistent"])
	}
}

func TestCollect_NodeInfoInconsistent(t *testing.T) {
	clientset := fake.NewClientset(
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "kube-system", UID: "uuid"}},
		&corev1.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name:   "server-1",
				Labels: map[string]string{"node-role.kubernetes.io/control-plane": ""},
			},
			Status: corev1.NodeStatus{
				NodeInfo: corev1.NodeSystemInfo{
					OperatingSystem: "linux",
					OSImage:         "Ubuntu 22.04",
					KernelVersion:   "5.15.0",
					Architecture:    "amd64",
				},
			},
		},
		&corev1.Node{
			ObjectMeta: metav1.ObjectMeta{Name: "agent-1"},
			Status: corev1.NodeStatus{
				NodeInfo: corev1.NodeSystemInfo{
					OperatingSystem: "linux",
					OSImage:         "SLES 15 SP6",
					KernelVersion:   "6.4.0",
					Architecture:    "amd64",
				},
			},
		},
	)

	data, err := Collect(context.Background(), clientset, nil, "recommended")
	if err != nil {
		t.Fatalf("Collect() error = %v", err)
	}

	if data.ExtraTagInfo["node-info-consistent"] != "false" {
		t.Errorf("node-info-consistent = %v, want false", data.ExtraTagInfo["node-info-consistent"])
	}
}

func TestCollect_CNIDetection(t *testing.T) {
	tests := []struct {
		name            string
		namespace       string
		daemonSet       string
		image           string
		expectedCNI     string
		expectedVersion string
	}{
		{"canal", "kube-system", "rke2-canal", "rancher/hardened-calico:v3.26.0", "canal", "v3.26.0"},
		{"flannel", "kube-flannel", "kube-flannel-ds", "flannel/flannel:v0.22.0", "flannel", "v0.22.0"},
		{"calico operator", "calico-system", "calico-node", "rancher/hardened-calico:v3.30.2-build20260801@sha256:abc123", "calico", "v3.30.2-build20260801"},
		{"cilium", "kube-system", "cilium", "cilium/cilium:v1.14.0", "cilium", "v1.14.0"},
		{"antrea", "kube-system", "antrea-agent", "antrea/antrea-agent-ubuntu:v2.4.0", "antrea", "v2.4.0"},
		{"weave", "kube-system", "weave-net", "weaveworks/weave-kube:2.8.1", "weave", "2.8.1"},
		{"untagged", "kube-system", "cilium", "cilium/cilium", "cilium", "unknown"},
		{"unrecognized", "kube-system", "my-cni", "example/cni:v1.0.0", "unknown", "unknown"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clientset := fake.NewClientset(
				&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "kube-system", UID: "uuid"}},
				&corev1.Node{
					ObjectMeta: metav1.ObjectMeta{Name: "node-1"},
					Status:     corev1.NodeStatus{NodeInfo: corev1.NodeSystemInfo{OSImage: "test", KernelVersion: "5.0", Architecture: "amd64"}},
				},
				&appsv1.DaemonSet{
					ObjectMeta: metav1.ObjectMeta{Name: tt.daemonSet, Namespace: tt.namespace},
					Spec: appsv1.DaemonSetSpec{
						Template: corev1.PodTemplateSpec{
							Spec: corev1.PodSpec{
								Containers: []corev1.Container{{Image: tt.image}},
							},
						},
					},
				},
			)

			data, err := Collect(context.Background(), clientset, nil, "recommended")
			if err != nil {
				t.Fatalf("Collect() error = %v", err)
			}

			if data.ExtraTagInfo["cni-plugin"] != tt.expectedCNI {
				t.Errorf("cni-plugin = %v, want %v", data.ExtraTagInfo["cni-plugin"], tt.expectedCNI)
			}
			if data.ExtraTagInfo["cni-version"] != tt.expectedVersion {
				t.Errorf("cni-version = %v, want %v", data.ExtraTagInfo["cni-version"], tt.expectedVersion)
			}
		})
	}
}

// failRequests makes the fake clientset fail the requests for the resource
// that match. A nil match fails all requests.
func failRequests(clientset *fake.Clientset, resource string, match func(k8stesting.Action) bool) {
	clientset.PrependReactor("*", resource, func(action k8stesting.Action) (bool, runtime.Object, error) {
		return match == nil || match(action), nil, apierrors.NewServiceUnavailable("timeout")
	})
}

func TestDetectCNIPlugin_PrefersKubeSystem(t *testing.T) {
	plugin, version := detectCNIPlugin([]appsv1.DaemonSet{
		*newDaemonSet("cattle-monitoring", "cilium-exporter", "example/cilium-exporter:v1.0.0"),
		*newDaemonSet("kube-system", "rke2-canal", "rancher/hardened-calico:v3.26.0"),
	})
	if plugin != "canal" || version != "v3.26.0" {
		t.Errorf("detectCNIPlugin() = %q, %q, want canal, v3.26.0", plugin, version)
	}
}

func TestCollect_WorkloadListErrors(t *testing.T) {
	clientset := fake.NewClientset(
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "kube-system", UID: "uuid"}},
		newDaemonSet("kube-system", "rke2-canal", "rancher/hardened-calico:v3.26.0"),
		newDaemonSet("gpu-operator", "nvidia-device-plugin-daemonset", "nvcr.io/nvidia/k8s-device-plugin:v0.14.0"),
		newDeployment("kube-system", "rke2-traefik", "rancher/hardened-traefik:v3.5.0"),
		newIngressClass("nginx", "k8s.io/ingress-nginx", true),
	)
	for _, resource := range []string{"daemonsets", "deployments"} {
		failRequests(clientset, resource, nil)
	}

	data, err := Collect(context.Background(), clientset, nil, "recommended")
	if err != nil {
		t.Fatalf("Collect() error = %v", err)
	}
	for key, want := range map[string]string{
		"cni-plugin":           "unknown",
		"cni-version":          "unknown",
		"gpu-operator":         "unknown",
		"gpu-operator-version": "unknown",
		"ingress-controller":   "ingress-nginx",
		"ingress-version":      "unknown",
	} {
		if got := data.ExtraTagInfo[key]; got != want {
			t.Errorf("%s = %q, want %q", key, got, want)
		}
	}
}

func TestCollect_IngressDetection(t *testing.T) {
	tests := []struct {
		name            string
		deploymentName  string
		image           string
		expectedIngress string
		expectedVersion string
	}{
		{"nginx", "rke2-ingress-nginx-controller", "rancher/nginx-ingress-controller:v1.9.0", "rke2-ingress-nginx", "v1.9.0"},
		{"legacy nginx chart", "rke2-ingress-nginx-nginx-ingress-controller", "rancher/nginx-ingress-controller:v0.35.0", "rke2-ingress-nginx", "v0.35.0"},
		{"traefik", "traefik", "traefik:v2.10", "traefik", "v2.10"},
		{"untagged", "traefik", "traefik", "traefik", "unknown"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clientset := fake.NewClientset(
				&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "kube-system", UID: "uuid"}},
				&corev1.Node{
					ObjectMeta: metav1.ObjectMeta{Name: "node-1"},
					Status:     corev1.NodeStatus{NodeInfo: corev1.NodeSystemInfo{OSImage: "test", KernelVersion: "5.0", Architecture: "amd64"}},
				},
				&appsv1.Deployment{
					ObjectMeta: metav1.ObjectMeta{Name: tt.deploymentName, Namespace: "kube-system"},
					Spec: appsv1.DeploymentSpec{
						Template: corev1.PodTemplateSpec{
							Spec: corev1.PodSpec{
								Containers: []corev1.Container{{Image: tt.image}},
							},
						},
					},
				},
			)

			data, err := Collect(context.Background(), clientset, nil, "recommended")
			if err != nil {
				t.Fatalf("Collect() error = %v", err)
			}

			if data.ExtraTagInfo["ingress-controller"] != tt.expectedIngress {
				t.Errorf("ingress-controller = %v, want %v", data.ExtraTagInfo["ingress-controller"], tt.expectedIngress)
			}
			if data.ExtraTagInfo["ingress-version"] != tt.expectedVersion {
				t.Errorf("ingress-version = %v, want %v", data.ExtraTagInfo["ingress-version"], tt.expectedVersion)
			}
		})
	}
}

func newIngressClass(name, controller string, isDefault bool) *networkingv1.IngressClass {
	class := &networkingv1.IngressClass{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec:       networkingv1.IngressClassSpec{Controller: controller},
	}
	if isDefault {
		class.Annotations = map[string]string{networkingv1.AnnotationIsDefaultIngressClass: "true"}
	}
	return class
}

func TestCollect_IngressClass(t *testing.T) {
	custom := newIngressClass("internal", "example.com/secret-ingress", false)
	tests := []struct {
		name            string
		objs            []runtime.Object
		listErr         error
		expectedIngress string
		expectedVersion string
	}{
		{"no ingress class", nil, nil, "none", "none"},
		{"known controller", []runtime.Object{custom, newIngressClass("haproxy", "haproxy.org/ingress-controller/haproxy", false)}, nil, "haproxy", "unknown"},
		{"default class wins", []runtime.Object{newIngressClass("kong", "ingress-controllers.konghq.com/kong", false), newIngressClass("nginx", "k8s.io/ingress-nginx", true)}, nil, "ingress-nginx", "unknown"},
		{"unknown controller skipped", []runtime.Object{custom, newIngressClass("nginx", "k8s.io/ingress-nginx", false)}, nil, "ingress-nginx", "unknown"},
		{"unknown default class", []runtime.Object{newIngressClass("internal", "example.com/secret-ingress", true), newIngressClass("nginx", "k8s.io/ingress-nginx", false)}, nil, "other", "unknown"},
		{"unknown controller only", []runtime.Object{custom}, nil, "other", "unknown"},
		{"other nginx in kube-system", []runtime.Object{newIngressClass("nginx", "nginx.org/ingress-controller", true), newDeployment("kube-system", "ingress-nginx-ingress-controller", "nginx/nginx-ingress:5.2.0")}, nil, "f5-nginx", "unknown"},
		{"daemonset outside kube-system ignored", []runtime.Object{newDaemonSet("traefik", "traefik", "traefik:v3.5.0")}, nil, "none", "none"},
		{"bundled controller wins", []runtime.Object{newIngressClass("nginx", "k8s.io/ingress-nginx", true), newDaemonSet("kube-system", "rke2-traefik", "rancher/hardened-traefik:v3.5.0")}, nil, "traefik", "v3.5.0"},
		{"list error", nil, apierrors.NewForbidden(schema.GroupResource{Group: "networking.k8s.io", Resource: "ingressclasses"}, "", nil), "unknown", "unknown"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clientset := fake.NewClientset(append(tt.objs, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "kube-system", UID: "uuid"}})...)
			if tt.listErr != nil {
				clientset.PrependReactor("list", "ingressclasses", func(k8stesting.Action) (bool, runtime.Object, error) {
					return true, nil, tt.listErr
				})
			}
			data, err := Collect(context.Background(), clientset, nil, "recommended")
			if err != nil {
				t.Fatalf("Collect() error = %v", err)
			}
			if data.ExtraTagInfo["ingress-controller"] != tt.expectedIngress {
				t.Errorf("ingress-controller = %q, want %q", data.ExtraTagInfo["ingress-controller"], tt.expectedIngress)
			}
			if data.ExtraTagInfo["ingress-version"] != tt.expectedVersion {
				t.Errorf("ingress-version = %q, want %q", data.ExtraTagInfo["ingress-version"], tt.expectedVersion)
			}
		})
	}
}

func TestCollect_GPUDetection(t *testing.T) {
	clientset := fake.NewClientset(
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "kube-system", UID: "uuid"}},
		&corev1.Node{
			ObjectMeta: metav1.ObjectMeta{Name: "gpu-node-1"},
			Status: corev1.NodeStatus{
				NodeInfo: corev1.NodeSystemInfo{OSImage: "test", KernelVersion: "5.0", Architecture: "amd64"},
				Allocatable: corev1.ResourceList{
					"nvidia.com/gpu": resource.MustParse("2"),
				},
			},
		},
		&corev1.Node{
			ObjectMeta: metav1.ObjectMeta{Name: "cpu-node-1"},
			Status: corev1.NodeStatus{
				NodeInfo: corev1.NodeSystemInfo{OSImage: "test", KernelVersion: "5.0", Architecture: "amd64"},
			},
		},
	)

	data, err := Collect(context.Background(), clientset, nil, "recommended")
	if err != nil {
		t.Fatalf("Collect() error = %v", err)
	}

	if data.ExtraFieldInfo["gpuNodeCount"] != 1 {
		t.Errorf("gpuNodeCount = %v, want 1", data.ExtraFieldInfo["gpuNodeCount"])
	}
	if data.ExtraTagInfo["gpu-vendor"] != "nvidia" {
		t.Errorf("gpu-vendor = %v, want nvidia", data.ExtraTagInfo["gpu-vendor"])
	}
}

func newDeployment(namespace, name, image string, env ...corev1.EnvVar) *appsv1.Deployment {
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: appsv1.DeploymentSpec{
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{Containers: []corev1.Container{{Image: image, Env: env}}},
			},
		},
	}
}

func newDaemonSet(namespace, name, image string) *appsv1.DaemonSet {
	return &appsv1.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: appsv1.DaemonSetSpec{
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{Containers: []corev1.Container{{Image: image}}},
			},
		},
	}
}

func newRancherSetting(name, value string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "management.cattle.io/v3",
		"kind":       "Setting",
		"metadata":   map[string]interface{}{"name": name},
		"value":      value,
	}}
}

func TestCollect_RancherRole(t *testing.T) {
	uuidEnv := corev1.EnvVar{Name: "CATTLE_INSTALL_UUID", Value: "install-uuid"}
	cattleSystem := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "cattle-system"}}
	webhook := newDeployment("cattle-system", "rancher-webhook", "rancher/rancher-webhook:v0.8.1")
	agent := newDeployment("cattle-system", "cattle-cluster-agent", "rancher/rancher-agent:v2.12.1", uuidEnv)
	tests := []struct {
		name        string
		objs        []runtime.Object
		dynObjs     []runtime.Object
		failRes     string
		wantManaged string
		wantRole    string
		wantVersion string
		wantUUID    any
	}{
		{
			name:        "no cattle-system",
			wantManaged: "false", wantRole: "none", wantVersion: "none", wantUUID: nil,
		},
		{
			name:        "downstream",
			objs:        []runtime.Object{cattleSystem, webhook, agent},
			wantManaged: "true", wantRole: "downstream", wantVersion: "v2.12.1", wantUUID: "install-uuid",
		},
		{
			name: "downstream prefers the server version variable",
			objs: []runtime.Object{cattleSystem, newDeployment("cattle-system", "cattle-cluster-agent", "registry.example.com/rancher/rancher-agent:v2.12.1@sha256:abc123",
				corev1.EnvVar{Name: "CATTLE_SERVER_VERSION", Value: "v2.12.2"}, uuidEnv)},
			wantManaged: "true", wantRole: "downstream", wantVersion: "v2.12.2", wantUUID: "install-uuid",
		},
		{
			name:        "server",
			objs:        []runtime.Object{cattleSystem, webhook, newDeployment("cattle-system", "rancher", "registry.rancher.com/rancher/rancher:v2.12.1")},
			dynObjs:     []runtime.Object{newRancherSetting("install-uuid", "install-uuid")},
			wantManaged: "true", wantRole: "server", wantVersion: "v2.12.1", wantUUID: "install-uuid",
		},
		{
			name:        "server without install-uuid setting",
			objs:        []runtime.Object{cattleSystem, newDeployment("cattle-system", "rancher", "rancher/rancher:v2.12.1")},
			wantManaged: "true", wantRole: "server", wantVersion: "v2.12.1", wantUUID: nil,
		},
		{
			name:        "hosted server",
			objs:        []runtime.Object{cattleSystem, agent, newDeployment("cattle-system", "rancher", "rancher/rancher:v2.12.3")},
			dynObjs:     []runtime.Object{newRancherSetting("install-uuid", "local-uuid")},
			wantManaged: "true", wantRole: "server", wantVersion: "v2.12.3", wantUUID: "local-uuid",
		},
		{
			name:        "no Rancher workload",
			objs:        []runtime.Object{cattleSystem, webhook},
			wantManaged: "unknown", wantRole: "unknown", wantVersion: "unknown", wantUUID: nil,
		},
		{
			name:        "namespace read error",
			objs:        []runtime.Object{cattleSystem, agent},
			failRes:     "namespaces",
			wantManaged: "unknown", wantRole: "unknown", wantVersion: "unknown", wantUUID: nil,
		},
		{
			name:        "deployment list error",
			objs:        []runtime.Object{cattleSystem, agent},
			failRes:     "deployments",
			wantManaged: "unknown", wantRole: "unknown", wantVersion: "unknown", wantUUID: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clientset := fake.NewClientset(append(tt.objs, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "kube-system", UID: "uuid"}})...)
			if tt.failRes != "" {
				failRequests(clientset, tt.failRes, func(action k8stesting.Action) bool {
					get, isGet := action.(k8stesting.GetAction)
					return action.GetNamespace() == "cattle-system" || (isGet && get.GetName() == "cattle-system")
				})
			}
			data, err := Collect(context.Background(), clientset, newDynClient(tt.dynObjs...), "recommended")
			if err != nil {
				t.Fatalf("Collect() error = %v", err)
			}
			for key, want := range map[string]string{"rancher-managed": tt.wantManaged, "rancher-role": tt.wantRole, "rancher-version": tt.wantVersion} {
				if got := data.ExtraTagInfo[key]; got != want {
					t.Errorf("%s = %q, want %q", key, got, want)
				}
			}
			if got := data.ExtraFieldInfo["rancher-install-uuid"]; got != tt.wantUUID {
				t.Errorf("rancher-install-uuid = %v, want %v", got, tt.wantUUID)
			}
		})
	}
}

func TestSend_Success(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("expected POST, got %s", r.Method)
		}
		if r.Header.Get("Content-Type") != "application/json" {
			t.Errorf("expected Content-Type application/json, got %s", r.Header.Get("Content-Type"))
		}

		w.WriteHeader(http.StatusOK)
		resp := Response{
			Versions: []Version{
				{Name: "v1.30.1", ReleaseDate: "2024-01-01"},
			},
			RequestIntervalInMinutes: 480,
		}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer server.Close()

	data := &Data{
		AppVersion:     "v1.30.0",
		ExtraTagInfo:   map[string]string{"clusteruuid": "test"},
		ExtraFieldInfo: map[string]interface{}{"serverNodeCount": 1},
	}

	resp, err := Send(context.Background(), data, server.URL)
	if err != nil {
		t.Fatalf("Send() error = %v", err)
	}
	if resp == nil {
		t.Fatal("Send() returned nil response")
	}
	if len(resp.Versions) != 1 {
		t.Errorf("expected 1 version, got %d", len(resp.Versions))
	}
	if resp.Versions[0].Name != "v1.30.1" {
		t.Errorf("version name = %q, want v1.30.1", resp.Versions[0].Name)
	}
}

func TestSend_RetryOnError(t *testing.T) {
	var attempts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		attempt := attempts.Add(1)
		if attempt < 3 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(Response{})
	}))
	defer server.Close()

	data := &Data{AppVersion: "test", ExtraTagInfo: map[string]string{}, ExtraFieldInfo: map[string]interface{}{}}

	_, err := Send(context.Background(), data, server.URL)
	if err != nil {
		t.Fatalf("Send() error = %v", err)
	}
	if attempts.Load() != 3 {
		t.Errorf("expected 3 attempts, got %d", attempts.Load())
	}
}

func TestSend_AllRetriesFail(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	data := &Data{AppVersion: "test", ExtraTagInfo: map[string]string{}, ExtraFieldInfo: map[string]interface{}{}}

	_, err := Send(context.Background(), data, server.URL)
	if err == nil {
		t.Error("Send() expected error after all retries fail")
	}
}

func TestSend_MalformedResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("not json"))
	}))
	defer server.Close()

	data := &Data{AppVersion: "test", ExtraTagInfo: map[string]string{}, ExtraFieldInfo: map[string]interface{}{}}

	resp, err := Send(context.Background(), data, server.URL)
	if err != nil {
		t.Errorf("Send() error = %v, want nil (graceful degradation)", err)
	}
	if resp != nil {
		t.Errorf("Send() response = %v, want nil", resp)
	}
}

func TestSend_WithCVEExtraInfo(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		resp := Response{
			Versions: []Version{
				{
					Name:        "v1.32.5",
					ReleaseDate: "2025-04-01",
					ExtraInfo: map[string]string{
						"cves":            "CVE-2025-1234,CVE-2025-5678",
						"releaseNotesURL": "https://github.com/rancher/rke2/releases/tag/v1.32.5%2Brke2r1",
					},
				},
				{
					Name:        "v1.32.4",
					ReleaseDate: "2025-03-01",
					ExtraInfo: map[string]string{
						"releaseNotesURL": "https://github.com/rancher/rke2/releases/tag/v1.32.4%2Brke2r1",
					},
				},
			},
			RequestIntervalInMinutes: 480,
		}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer server.Close()

	data := &Data{
		AppVersion:     "v1.32.3+rke2r1",
		ExtraTagInfo:   map[string]string{"clusteruuid": "test"},
		ExtraFieldInfo: map[string]interface{}{"serverNodeCount": 1},
	}

	resp, err := Send(context.Background(), data, server.URL)
	if err != nil {
		t.Fatalf("Send() error = %v", err)
	}
	if resp == nil {
		t.Fatal("Send() returned nil response")
	}
	if len(resp.Versions) != 2 {
		t.Errorf("expected 2 versions, got %d", len(resp.Versions))
	}
	if resp.Versions[0].ExtraInfo["cves"] != "CVE-2025-1234,CVE-2025-5678" {
		t.Errorf("ExtraInfo[cves] = %q, want CVE-2025-1234,CVE-2025-5678", resp.Versions[0].ExtraInfo["cves"])
	}
}

func TestCollect_GPUOperatorDetection(t *testing.T) {
	clientset := fake.NewClientset(
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "kube-system", UID: "uuid"}},
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "gpu-operator"}},
		&corev1.Node{
			ObjectMeta: metav1.ObjectMeta{Name: "node-1"},
			Status:     corev1.NodeStatus{NodeInfo: corev1.NodeSystemInfo{OSImage: "test", KernelVersion: "5.0", Architecture: "amd64"}},
		},
		&appsv1.DaemonSet{
			ObjectMeta: metav1.ObjectMeta{Name: "nvidia-device-plugin-daemonset", Namespace: "gpu-operator"},
			Spec: appsv1.DaemonSetSpec{
				Template: corev1.PodTemplateSpec{
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{{Image: "nvcr.io/nvidia/k8s-device-plugin:v0.14.0"}},
					},
				},
			},
		},
	)

	data, err := Collect(context.Background(), clientset, nil, "recommended")
	if err != nil {
		t.Fatalf("Collect() error = %v", err)
	}

	if data.ExtraTagInfo["gpu-operator"] != "nvidia-gpu-operator" {
		t.Errorf("gpu-operator = %v, want nvidia-gpu-operator", data.ExtraTagInfo["gpu-operator"])
	}
}

func TestCollect_IngressAsDaemonSet(t *testing.T) {
	clientset := fake.NewClientset(
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "kube-system", UID: "uuid"}},
		&corev1.Node{
			ObjectMeta: metav1.ObjectMeta{Name: "node-1"},
			Status:     corev1.NodeStatus{NodeInfo: corev1.NodeSystemInfo{OSImage: "test", KernelVersion: "5.0", Architecture: "amd64"}},
		},
		&appsv1.DaemonSet{
			ObjectMeta: metav1.ObjectMeta{Name: "rke2-ingress-nginx-controller", Namespace: "kube-system"},
			Spec: appsv1.DaemonSetSpec{
				Template: corev1.PodTemplateSpec{
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{{Image: "rancher/nginx-ingress-controller:v1.9.0"}},
					},
				},
			},
		},
	)

	data, err := Collect(context.Background(), clientset, nil, "recommended")
	if err != nil {
		t.Fatalf("Collect() error = %v", err)
	}

	if data.ExtraTagInfo["ingress-controller"] != "rke2-ingress-nginx" {
		t.Errorf("ingress-controller = %v, want rke2-ingress-nginx", data.ExtraTagInfo["ingress-controller"])
	}
}

func TestCollect_IPStackFromService(t *testing.T) {
	tests := []struct {
		name       string
		ipFamilies []corev1.IPFamily
		expected   string
	}{
		{"ipv4-only", []corev1.IPFamily{corev1.IPv4Protocol}, "ipv4-only"},
		{"ipv6-only", []corev1.IPFamily{corev1.IPv6Protocol}, "ipv6-only"},
		{"dual-stack", []corev1.IPFamily{corev1.IPv4Protocol, corev1.IPv6Protocol}, "dual-stack"},
		{"dual-stack-v6-first", []corev1.IPFamily{corev1.IPv6Protocol, corev1.IPv4Protocol}, "dual-stack"},
		{"empty-families", []corev1.IPFamily{}, "unknown"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clientset := fake.NewClientset(
				&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "kube-system", UID: "uuid"}},
				&corev1.Node{
					ObjectMeta: metav1.ObjectMeta{Name: "node-1"},
					Status: corev1.NodeStatus{
						NodeInfo: corev1.NodeSystemInfo{OSImage: "test", KernelVersion: "5.0", Architecture: "amd64"},
					},
				},
				&corev1.Service{
					ObjectMeta: metav1.ObjectMeta{Name: "kubernetes", Namespace: "default"},
					Spec:       corev1.ServiceSpec{IPFamilies: tt.ipFamilies},
				},
			)

			data, err := Collect(context.Background(), clientset, nil, "recommended")
			if err != nil {
				t.Fatalf("Collect() error = %v", err)
			}

			if data.ExtraTagInfo["ip-stack"] != tt.expected {
				t.Errorf("ip-stack = %v, want %v", data.ExtraTagInfo["ip-stack"], tt.expected)
			}
		})
	}
}

func TestCollect_IPStackNoService(t *testing.T) {
	clientset := fake.NewClientset(
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "kube-system", UID: "uuid"}},
		&corev1.Node{
			ObjectMeta: metav1.ObjectMeta{Name: "node-1"},
			Status:     corev1.NodeStatus{NodeInfo: corev1.NodeSystemInfo{OSImage: "test", KernelVersion: "5.0", Architecture: "amd64"}},
		},
	)

	data, err := Collect(context.Background(), clientset, nil, "recommended")
	if err != nil {
		t.Fatalf("Collect() error = %v", err)
	}

	if data.ExtraTagInfo["ip-stack"] != "unknown" {
		t.Errorf("ip-stack = %v, want unknown", data.ExtraTagInfo["ip-stack"])
	}
}

func TestCollect_MinimalMode(t *testing.T) {
	clientset := fake.NewClientset(
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "kube-system", UID: "uuid"}},
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "cattle-system"}},
		&corev1.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name:   "server-1",
				Labels: map[string]string{"node-role.kubernetes.io/control-plane": ""},
			},
			Status: corev1.NodeStatus{
				NodeInfo: corev1.NodeSystemInfo{OSImage: "test", KernelVersion: "5.0", Architecture: "amd64"},
				Allocatable: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("4"),
					corev1.ResourceMemory: resource.MustParse("8Gi"),
				},
			},
		},
		&corev1.Node{
			ObjectMeta: metav1.ObjectMeta{Name: "agent-1"},
			Status: corev1.NodeStatus{
				NodeInfo: corev1.NodeSystemInfo{OSImage: "test", KernelVersion: "5.0", Architecture: "amd64"},
				Allocatable: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("8"),
					corev1.ResourceMemory: resource.MustParse("16Gi"),
				},
			},
		},
		&appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{Name: "cattle-cluster-agent", Namespace: "cattle-system"},
			Spec: appsv1.DeploymentSpec{
				Template: corev1.PodTemplateSpec{
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{{
							Image: "rancher/rancher-agent:v2.8.0",
							Env: []corev1.EnvVar{
								{Name: "CATTLE_INSTALL_UUID", Value: "test-uuid"},
							},
						}},
					},
				},
			},
		},
	)

	data, err := Collect(context.Background(), clientset, nil, "minimal")
	if err != nil {
		t.Fatalf("Collect() error = %v", err)
	}

	// Mode field should be set
	if data.ExtraTagInfo["mode"] != "minimal" {
		t.Errorf("mode = %q, want %q", data.ExtraTagInfo["mode"], "minimal")
	}

	// Node counts should be -1 in minimal mode
	if data.ExtraFieldInfo["serverNodeCount"] != -1 {
		t.Errorf("serverNodeCount = %v, want -1", data.ExtraFieldInfo["serverNodeCount"])
	}
	if data.ExtraFieldInfo["agentNodeCount"] != -1 {
		t.Errorf("agentNodeCount = %v, want -1", data.ExtraFieldInfo["agentNodeCount"])
	}
	if data.ExtraFieldInfo["gpuNodeCount"] != -1 {
		t.Errorf("gpuNodeCount = %v, want -1", data.ExtraFieldInfo["gpuNodeCount"])
	}

	// CPU/memory should be -1 in minimal mode
	if data.ExtraFieldInfo["serverCPU"] != int64(-1) {
		t.Errorf("serverCPU = %v, want -1", data.ExtraFieldInfo["serverCPU"])
	}
	if data.ExtraFieldInfo["agentCPU"] != int64(-1) {
		t.Errorf("agentCPU = %v, want -1", data.ExtraFieldInfo["agentCPU"])
	}
	if data.ExtraFieldInfo["serverMemory"] != int64(-1) {
		t.Errorf("serverMemory = %v, want -1", data.ExtraFieldInfo["serverMemory"])
	}
	if data.ExtraFieldInfo["agentMemory"] != int64(-1) {
		t.Errorf("agentMemory = %v, want -1", data.ExtraFieldInfo["agentMemory"])
	}

	// Rancher-managed and the role should still be present
	if data.ExtraTagInfo["rancher-managed"] != "true" {
		t.Errorf("rancher-managed = %v, want true", data.ExtraTagInfo["rancher-managed"])
	}
	if data.ExtraTagInfo["rancher-role"] != "downstream" {
		t.Errorf("rancher-role = %v, want downstream", data.ExtraTagInfo["rancher-role"])
	}

	// Rancher version is redacted and UUID empty in minimal mode
	if data.ExtraTagInfo["rancher-version"] != "redacted" {
		t.Errorf("rancher-version = %v, want redacted", data.ExtraTagInfo["rancher-version"])
	}
	if data.ExtraFieldInfo["rancher-install-uuid"] != "" {
		t.Errorf("rancher-install-uuid = %v, want empty string", data.ExtraFieldInfo["rancher-install-uuid"])
	}

	// OS info should still be present
	if data.ExtraTagInfo["os"] != "test" {
		t.Errorf("os = %v, want test", data.ExtraTagInfo["os"])
	}
	if data.ExtraTagInfo["arch"] != "amd64" {
		t.Errorf("arch = %v, want amd64", data.ExtraTagInfo["arch"])
	}
}

func TestCollect_TagsAndFields(t *testing.T) {
	tags := func(mode, rancherVersion string) map[string]string {
		return map[string]string{
			"mode":                    mode,
			"clusteruuid":             "uuid",
			"operating-system":        "unknown",
			"os":                      "unknown",
			"arch":                    "unknown",
			"selinux":                 "unknown",
			"node-info-consistent":    "true",
			"gpu-vendor":              "none",
			"cni-plugin":              "unknown",
			"cni-version":             "unknown",
			"ingress-controller":      "none",
			"ingress-version":         "none",
			"gpu-operator":            "none",
			"gpu-operator-version":    "none",
			"rancher-managed":         "false",
			"rancher-role":            "none",
			"rancher-version":         rancherVersion,
			"rancher-prime":           "unknown",
			"system-default-registry": "unknown",
			"ip-stack":                "unknown",
		}
	}
	tests := []struct {
		mode       string
		wantTags   map[string]string
		wantFields map[string]interface{}
	}{
		{
			mode:     "recommended",
			wantTags: tags("recommended", "none"),
			wantFields: map[string]interface{}{
				"serverNodeCount": 0, "agentNodeCount": 0, "gpuNodeCount": 0,
				"serverCPU": int64(0), "agentCPU": int64(0), "serverMemory": int64(0), "agentMemory": int64(0),
				"kernel": "",
			},
		},
		{
			mode:     "minimal",
			wantTags: tags("minimal", "redacted"),
			wantFields: map[string]interface{}{
				"serverNodeCount": -1, "agentNodeCount": -1, "gpuNodeCount": -1,
				"serverCPU": int64(-1), "agentCPU": int64(-1), "serverMemory": int64(-1), "agentMemory": int64(-1),
				"kernel": "", "rancher-install-uuid": "",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.mode, func(t *testing.T) {
			clientset := fake.NewClientset(
				&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "kube-system", UID: "uuid"}},
			)
			data, err := Collect(context.Background(), clientset, nil, tt.mode)
			if err != nil {
				t.Fatalf("Collect() error = %v", err)
			}
			if !maps.Equal(data.ExtraTagInfo, tt.wantTags) {
				t.Errorf("ExtraTagInfo = %v, want %v", data.ExtraTagInfo, tt.wantTags)
			}
			if !reflect.DeepEqual(data.ExtraFieldInfo, tt.wantFields) {
				t.Errorf("ExtraFieldInfo = %v, want %v", data.ExtraFieldInfo, tt.wantFields)
			}
		})
	}
}

func TestCollect_RecommendedModeIncludesRancherDetails(t *testing.T) {
	clientset := fake.NewClientset(
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "kube-system", UID: "uuid"}},
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "cattle-system"}},
		&corev1.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name:   "server-1",
				Labels: map[string]string{"node-role.kubernetes.io/control-plane": ""},
			},
			Status: corev1.NodeStatus{
				NodeInfo: corev1.NodeSystemInfo{OSImage: "test", KernelVersion: "5.0", Architecture: "amd64"},
				Allocatable: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("4"),
					corev1.ResourceMemory: resource.MustParse("8Gi"),
				},
			},
		},
		&appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{Name: "cattle-cluster-agent", Namespace: "cattle-system"},
			Spec: appsv1.DeploymentSpec{
				Template: corev1.PodTemplateSpec{
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{{
							Image: "rancher/rancher-agent:v2.8.0",
							Env: []corev1.EnvVar{
								{Name: "CATTLE_INSTALL_UUID", Value: "test-uuid"},
							},
						}},
					},
				},
			},
		},
	)

	data, err := Collect(context.Background(), clientset, nil, "recommended")
	if err != nil {
		t.Fatalf("Collect() error = %v", err)
	}

	// Mode field should be set
	if data.ExtraTagInfo["mode"] != "recommended" {
		t.Errorf("mode = %q, want %q", data.ExtraTagInfo["mode"], "recommended")
	}

	// Node counts should have actual values
	if data.ExtraFieldInfo["serverNodeCount"] != 1 {
		t.Errorf("serverNodeCount = %v, want 1", data.ExtraFieldInfo["serverNodeCount"])
	}

	// CPU/memory should have actual values
	serverCPU, ok := data.ExtraFieldInfo["serverCPU"].(int64)
	if !ok || serverCPU == 0 {
		t.Errorf("serverCPU = %v, want non-zero", data.ExtraFieldInfo["serverCPU"])
	}

	// Rancher details should be present in recommended mode
	if data.ExtraTagInfo["rancher-version"] != "v2.8.0" {
		t.Errorf("rancher-version = %v, want v2.8.0", data.ExtraTagInfo["rancher-version"])
	}
	if data.ExtraFieldInfo["rancher-install-uuid"] != "test-uuid" {
		t.Errorf("rancher-install-uuid = %v, want test-uuid", data.ExtraFieldInfo["rancher-install-uuid"])
	}
}

var (
	primeGVRToListKind = map[schema.GroupVersionResource]string{helmChartGVR: "HelmChartList"}
	primeScheme        = runtime.NewScheme()
)

// newHelmChart mirrors RKE2, which injects both keys together, so an empty
// registry is still set whenever primeEnabled is.
func newHelmChart(name, primeEnabled, registry string) *unstructured.Unstructured {
	set := map[string]interface{}{}
	if primeEnabled != "" {
		set["global.prime.enabled"] = primeEnabled
	}
	if primeEnabled != "" || registry != "" {
		set["global.systemDefaultRegistry"] = registry
	}
	spec := map[string]interface{}{"chart": name}
	if len(set) > 0 {
		spec["set"] = set
	}
	return &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "helm.cattle.io/v1",
			"kind":       "HelmChart",
			"metadata": map[string]interface{}{
				"name":      name,
				"namespace": "kube-system",
			},
			"spec": spec,
		},
	}
}

func newDynClient(objs ...runtime.Object) dynamic.Interface {
	return dynamicfake.NewSimpleDynamicClientWithCustomListKinds(primeScheme, primeGVRToListKind, objs...)
}

func TestDetectPrime(t *testing.T) {
	tests := []struct {
		name         string
		client       dynamic.Interface
		wantPrime    string
		wantRegistry string
	}{
		{
			name:         "nil client",
			client:       nil,
			wantPrime:    "unknown",
			wantRegistry: "",
		},
		{
			name:         "no helmcharts",
			client:       newDynClient(),
			wantPrime:    "unknown",
			wantRegistry: "",
		},
		{
			name: "prime enabled",
			client: newDynClient(
				newHelmChart("rke2-coredns", "true", "registry.rancher.com"),
				newHelmChart("rke2-canal", "true", "registry.rancher.com"),
			),
			wantPrime:    "true",
			wantRegistry: "registry.rancher.com",
		},
		{
			name: "prime disabled",
			client: newDynClient(
				newHelmChart("rke2-coredns", "false", ""),
			),
			wantPrime:    "false",
			wantRegistry: "none",
		},
		{
			name: "key absent on all charts",
			client: newDynClient(
				newHelmChart("rke2-coredns", "", ""),
				newHelmChart("rke2-canal", "", ""),
			),
			wantPrime:    "unknown",
			wantRegistry: "",
		},
		{
			name: "HA mismatch positive wins",
			client: newDynClient(
				newHelmChart("rke2-canal", "false", ""),
				newHelmChart("rke2-coredns", "true", "registry.rancher.com"),
			),
			wantPrime:    "true",
			wantRegistry: "registry.rancher.com",
		},
		{
			name: "malformed bool value ignored",
			client: newDynClient(
				newHelmChart("rke2-coredns", "not-a-bool", "my.mirror.example.com"),
			),
			wantPrime:    "unknown",
			wantRegistry: "my.mirror.example.com",
		},
		{
			name: "registry only no prime key",
			client: newDynClient(
				newHelmChart("rke2-coredns", "", "my.mirror.example.com"),
			),
			wantPrime:    "unknown",
			wantRegistry: "my.mirror.example.com",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotPrime, gotRegistry := detectPrime(context.Background(), tt.client)
			if gotPrime != tt.wantPrime {
				t.Errorf("detectPrime() prime = %q, want %q", gotPrime, tt.wantPrime)
			}
			if gotRegistry != tt.wantRegistry {
				t.Errorf("detectPrime() registry = %q, want %q", gotRegistry, tt.wantRegistry)
			}
		})
	}
}

func runPrimeCollect(t *testing.T, mode string, charts ...*unstructured.Unstructured) *Data {
	t.Helper()
	objs := make([]runtime.Object, len(charts))
	for i, c := range charts {
		objs[i] = c
	}
	clientset := fake.NewClientset(
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "kube-system", UID: types.UID("test-uuid")}},
		&corev1.Service{
			ObjectMeta: metav1.ObjectMeta{Name: "kubernetes", Namespace: "default"},
			Spec:       corev1.ServiceSpec{IPFamilies: []corev1.IPFamily{corev1.IPv4Protocol}},
		},
	)
	data, err := Collect(context.Background(), clientset, newDynClient(objs...), mode)
	if err != nil {
		t.Fatalf("Collect() error = %v", err)
	}
	return data
}

func TestCollect_PrimeDetected(t *testing.T) {
	data := runPrimeCollect(t, "recommended", newHelmChart("rke2-coredns", "true", "registry.rancher.com"))
	if data.ExtraTagInfo["rancher-prime"] != "true" {
		t.Errorf("rancher-prime = %v, want %q", data.ExtraTagInfo["rancher-prime"], "true")
	}
	if data.ExtraTagInfo["system-default-registry"] != "registry.rancher.com" {
		t.Errorf("system-default-registry = %v, want %q", data.ExtraTagInfo["system-default-registry"], "registry.rancher.com")
	}
}

func TestCollect_PrimeNotDetected(t *testing.T) {
	data := runPrimeCollect(t, "recommended", newHelmChart("rke2-coredns", "false", ""))
	if data.ExtraTagInfo["rancher-prime"] != "false" {
		t.Errorf("rancher-prime = %v, want %q", data.ExtraTagInfo["rancher-prime"], "false")
	}
	if data.ExtraTagInfo["system-default-registry"] != "none" {
		t.Errorf("system-default-registry = %v, want %q", data.ExtraTagInfo["system-default-registry"], "none")
	}
}

func TestCollect_PrimeUnknown(t *testing.T) {
	data := runPrimeCollect(t, "recommended")
	if data.ExtraTagInfo["rancher-prime"] != "unknown" {
		t.Errorf("rancher-prime = %v, want %q", data.ExtraTagInfo["rancher-prime"], "unknown")
	}
}

func TestCollect_PrimeMinimalMode(t *testing.T) {
	data := runPrimeCollect(t, "minimal", newHelmChart("rke2-coredns", "true", "registry.rancher.com"))
	if data.ExtraTagInfo["rancher-prime"] != "true" {
		t.Errorf("rancher-prime (minimal) = %v, want %q", data.ExtraTagInfo["rancher-prime"], "true")
	}
	if data.ExtraTagInfo["system-default-registry"] != "registry.rancher.com" {
		t.Errorf("system-default-registry (minimal) = %v, want %q", data.ExtraTagInfo["system-default-registry"], "registry.rancher.com")
	}
}

func TestRke2BuildNumber(t *testing.T) {
	cases := []struct {
		in   string
		want int
	}{
		{"", 0},
		{"rke2r1", 1},
		{"rke2r12", 12},
		{"rke2r", 0},
		{"rke2rX", 0},
		{"r1", 0},
		{"rke2r1.dirty", 0},
	}
	for _, c := range cases {
		t.Run(c.in, func(t *testing.T) {
			if got := rke2BuildNumber(c.in); got != c.want {
				t.Errorf("rke2BuildNumber(%q) = %d, want %d", c.in, got, c.want)
			}
		})
	}
}

func TestFilterNewerVersions(t *testing.T) {
	versions := []Version{
		{Name: "v1.32.4", ReleaseDate: "2024-01-10T00:00:00Z"},
		{Name: "v1.32.5", ReleaseDate: "2024-02-01T00:00:00Z"},
		{Name: "v1.33.0+rke2r1", ReleaseDate: "2024-03-01T00:00:00Z"},
		{Name: "v1.32.5+rke2r2", ReleaseDate: "2024-02-15T00:00:00Z"},
	}
	got := filterNewerVersions(versions, "v1.32.5+rke2r1")
	names := make([]string, 0, len(got))
	for _, v := range got {
		names = append(names, v.Name)
	}
	want := []string{"v1.33.0+rke2r1", "v1.32.5+rke2r2"}
	if !slices.Equal(names, want) {
		t.Errorf("filterNewerVersions names = %v, want %v", names, want)
	}
}

func TestFilterNewerVersions_SortsByReleaseDateDescending(t *testing.T) {
	versions := []Version{
		{Name: "v1.36.1+rke2r1", ReleaseDate: "2026-05-18T17:26:43Z"},
		{Name: "v1.36.3+rke2r1", ReleaseDate: "2026-08-04T21:12:44Z"},
		{Name: "v1.36.2+rke2r1", ReleaseDate: "2026-06-25T00:54:04Z"},
		{Name: "v1.36.4+rke2r1", ReleaseDate: "2026-09-01T00:00:00Z"},
	}

	got := filterNewerVersions(versions, "v1.36.0+rke2r1")
	want := []string{"v1.36.4+rke2r1", "v1.36.3+rke2r1", "v1.36.2+rke2r1", "v1.36.1+rke2r1"}

	names := make([]string, 0, len(got))
	for _, v := range got {
		names = append(names, v.Name)
	}
	if !slices.Equal(names, want) {
		t.Fatalf("filtered names = %v, want %v", names, want)
	}
}

func TestFilterNewerVersions_IgnoresSemverHigherButOlderReleaseDate(t *testing.T) {
	versions := []Version{
		{Name: "v1.34.7+rke2r1", ReleaseDate: "2026-04-24T12:41:04Z"},
		{Name: "v1.35.1+rke2r1", ReleaseDate: "2026-02-13T19:01:09Z"},
		{Name: "v1.35.7+rke2r1", ReleaseDate: "2026-08-04T20:03:30Z"},
	}

	got := filterNewerVersions(versions, "v1.34.7+rke2r1")
	names := make([]string, 0, len(got))
	for _, v := range got {
		names = append(names, v.Name)
	}
	want := []string{"v1.35.7+rke2r1"}
	if !slices.Equal(names, want) {
		t.Fatalf("filtered names = %v, want %v", names, want)
	}
}

func TestFilterNewerVersions_IgnoresLowerMinorEvenIfLaterDate(t *testing.T) {
	versions := []Version{
		{Name: "v1.34.7+rke2r1", ReleaseDate: "2026-04-24T12:41:04Z"},
		{Name: "v1.33.13+rke2r2", ReleaseDate: "2026-08-04T20:09:56Z"},
		{Name: "v1.35.7+rke2r1", ReleaseDate: "2026-08-04T20:03:30Z"},
	}

	got := filterNewerVersions(versions, "v1.34.7+rke2r1")
	names := make([]string, 0, len(got))
	for _, v := range got {
		names = append(names, v.Name)
	}
	want := []string{"v1.35.7+rke2r1"}
	if !slices.Equal(names, want) {
		t.Fatalf("filtered names = %v, want %v", names, want)
	}
}

func captureLogs(t *testing.T) *logtest.Hook {
	t.Helper()
	prevLevel := logrus.GetLevel()
	logrus.SetLevel(logrus.DebugLevel)
	hook := logtest.NewGlobal()
	t.Cleanup(func() {
		hook.Reset()
		logrus.StandardLogger().ReplaceHooks(make(logrus.LevelHooks))
		logrus.SetLevel(prevLevel)
	})
	return hook
}

func entriesWithMsg(hook *logtest.Hook, msg string) []*logrus.Entry {
	var out []*logrus.Entry
	for _, e := range hook.AllEntries() {
		if e.Message == msg {
			out = append(out, e)
		}
	}
	return out
}

func newSendStub(t *testing.T, resp Response) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(resp)
	}))
}

func TestSend_OnlyOlderVersions_Silent(t *testing.T) {
	hook := captureLogs(t)
	server := newSendStub(t, Response{
		Versions: []Version{
			{Name: "v1.32.4", ReleaseDate: "2025-03-01"},
			{Name: "v1.33.0", ReleaseDate: "2025-02-01"},
		},
		RequestIntervalInMinutes: 480,
	})
	defer server.Close()

	data := &Data{AppVersion: "v1.34.0", ExtraTagInfo: map[string]string{}, ExtraFieldInfo: map[string]interface{}{}}
	if _, err := Send(context.Background(), data, server.URL); err != nil {
		t.Fatalf("Send() error = %v", err)
	}
	if n := len(entriesWithMsg(hook, "available version")); n != 0 {
		t.Errorf("available version logged %d times, want 0", n)
	}
	rr := entriesWithMsg(hook, "response received")
	if len(rr) == 0 {
		t.Fatal("response received not logged")
	}
	if rr[0].Data["newer"] != 0 {
		t.Errorf("newer = %v, want 0", rr[0].Data["newer"])
	}
	if rr[0].Data["versions"] != 2 {
		t.Errorf("versions = %v, want 2", rr[0].Data["versions"])
	}
	if rr[0].Data["intervalMinutes"] != 480 {
		t.Errorf("intervalMinutes = %v, want 480", rr[0].Data["intervalMinutes"])
	}
	if len(entriesWithMsg(hook, "data sent")) == 0 {
		t.Error("data sent not logged")
	}
	if len(entriesWithMsg(hook, "newer version fixes security vulnerabilities")) > 0 {
		t.Error("CVE warn must not fire when no newer version present")
	}
}

func TestSend_MixedVersions_OnlyNewerLogged(t *testing.T) {
	hook := captureLogs(t)
	server := newSendStub(t, Response{
		Versions: []Version{
			{Name: "v1.36.0+rke2r1", ReleaseDate: "2025-01-01"},
			{Name: "v1.36.1+rke2r1", ReleaseDate: "2025-01-15", ExtraInfo: map[string]string{"cves": "CVE-2026-9"}},
			{Name: "v1.36.1+rke2r2", ReleaseDate: "2025-02-01", ExtraInfo: map[string]string{"cves": "CVE-2026-9"}},
			{Name: "v1.37.0+rke2r1", ReleaseDate: "2025-03-01"},
			{Name: "v1.35.0+rke2r1", ReleaseDate: "2024-10-01"},
		},
		RequestIntervalInMinutes: 480,
	})
	defer server.Close()

	data := &Data{AppVersion: "v1.36.1+rke2r1", ExtraTagInfo: map[string]string{}, ExtraFieldInfo: map[string]interface{}{}}
	if _, err := Send(context.Background(), data, server.URL); err != nil {
		t.Fatalf("Send() error = %v", err)
	}

	avail := entriesWithMsg(hook, "available version")
	names := make([]string, 0, len(avail))
	for _, e := range avail {
		names = append(names, e.Data["version"].(string))
	}
	wantNames := map[string]bool{"v1.36.1+rke2r2": true, "v1.37.0+rke2r1": true}
	if len(names) != len(wantNames) {
		t.Fatalf("available versions = %v, want keys %v", names, wantNames)
	}
	for _, v := range names {
		if !wantNames[v] {
			t.Errorf("unexpected available version %q", v)
		}
	}

	rr := entriesWithMsg(hook, "response received")
	if len(rr) == 0 || rr[0].Data["newer"] != 2 || rr[0].Data["versions"] != 5 {
		t.Errorf("response received = %+v, want versions=5 newer=2", rr)
	}

	warn := entriesWithMsg(hook, "The installed RKE2 version v1.36.1+rke2r1 includes CVEs. These are the 1 most relevant: CVE-2026-9. Please upgrade to a newer version to fix security vulnerabilities")
	if len(warn) != 1 {
		t.Fatalf("current-version CVE warn count = %d, want 1", len(warn))
	}
	if len(warn[0].Data) != 0 {
		t.Errorf("warning fields = %+v, want none", warn[0].Data)
	}

	entries := hook.AllEntries()
	warnIdx := -1
	firstAvailIdx := -1
	for i, e := range entries {
		if e.Message == "The installed RKE2 version v1.36.1+rke2r1 includes CVEs. These are the 1 most relevant: CVE-2026-9. Please upgrade to a newer version to fix security vulnerabilities" {
			warnIdx = i
		}
		if e.Message == "available version" && firstAvailIdx == -1 {
			firstAvailIdx = i
		}
	}
	if warnIdx == -1 || firstAvailIdx == -1 || warnIdx >= firstAvailIdx {
		t.Fatalf("log ordering = %+v; expected CVE warning before available version entries", entries)
	}
}

func TestSend_LatestTaggedVersion_LogsLatestMessageWithoutCVEs(t *testing.T) {
	hook := captureLogs(t)
	server := newSendStub(t, Response{
		Versions: []Version{
			{Name: "v1.36.3+rke2r1", Tags: []string{"latest", "v1.36"}, ReleaseDate: "2026-08-04T21:12:44Z", ExtraInfo: map[string]string{"cves": "CVE-2026-33818,CVE-2026-39821,CVE-2026-46600,CVE-2026-56852,CVE-2026-56853"}},
			{Name: "v1.36.2+rke2r1", ReleaseDate: "2026-06-25T00:54:04Z"},
		},
		RequestIntervalInMinutes: 60,
	})
	defer server.Close()

	data := &Data{AppVersion: "v1.36.3+rke2r1", ExtraTagInfo: map[string]string{}, ExtraFieldInfo: map[string]interface{}{}}
	if _, err := Send(context.Background(), data, server.URL); err != nil {
		t.Fatalf("Send() error = %v", err)
	}

	latestMsg := entriesWithMsg(hook, "The installed RKE2 version v1.36.3+rke2r1 is the latest released version")
	if len(latestMsg) != 1 {
		t.Fatalf("latest version log count = %d, want 1", len(latestMsg))
	}
	if len(entriesWithMsg(hook, "The installed RKE2 version v1.36.3+rke2r1 includes CVEs. These are the 5 most relevant: CVE-2026-33818, CVE-2026-39821, CVE-2026-46600, CVE-2026-56852, CVE-2026-56853. Please upgrade to a newer version to fix security vulnerabilities")) != 0 {
		t.Fatal("CVE warning must not be logged for latest-tagged release")
	}
}

func TestSend_UnparseableAppVersion_ShowsAll(t *testing.T) {
	hook := captureLogs(t)
	server := newSendStub(t, Response{
		Versions: []Version{
			{Name: "v1.32.4"},
			{Name: "v1.33.0"},
		},
		RequestIntervalInMinutes: 480,
	})
	defer server.Close()

	data := &Data{AppVersion: "dev", ExtraTagInfo: map[string]string{}, ExtraFieldInfo: map[string]interface{}{}}
	if _, err := Send(context.Background(), data, server.URL); err != nil {
		t.Fatalf("Send() error = %v", err)
	}
	if n := len(entriesWithMsg(hook, "available version")); n != 2 {
		t.Errorf("available version count = %d, want 2", n)
	}
	rr := entriesWithMsg(hook, "response received")
	if len(rr) == 0 || rr[0].Data["newer"] != 2 {
		t.Errorf("response received newer = %+v, want 2", rr)
	}
}

func TestFilterCurrentVersion(t *testing.T) {
	tests := []struct {
		name           string
		currentVersion string
		versions       []Version
		wantName       string
		wantCVEs       []string
		wantNil        bool
	}{
		{
			name:           "returns exact current version",
			currentVersion: "v1.35.5+rke2r1",
			versions: []Version{
				{Name: "v1.35.5+rke2r1", ExtraInfo: map[string]string{"cves": "CVE-CURRENT-1,CVE-CURRENT-2"}},
				{Name: "v1.35.6+rke2r1", ExtraInfo: map[string]string{"cves": "CVE-A"}},
				{Name: "v1.35.5+rke2r2", ExtraInfo: map[string]string{"cves": "CVE-B,CVE-A"}},
				{Name: "v1.36.2+rke2r1", ExtraInfo: map[string]string{"cves": "CVE-C"}},
			},
			wantName: "v1.35.5+rke2r1",
			wantCVEs: []string{"CVE-CURRENT-1", "CVE-CURRENT-2"},
		},
		{
			name:           "returns current version even when cves are empty",
			currentVersion: "v1.35.5+rke2r1",
			versions: []Version{
				{Name: "v1.35.5+rke2r1"},
				{Name: "v1.35.5+rke2r2"},
				{Name: "v1.35.6+rke2r1", ExtraInfo: map[string]string{"cves": "CVE-A"}},
			},
			wantName: "v1.35.5+rke2r1",
			wantCVEs: nil,
		},
		{
			name:           "returns nil when current version missing",
			currentVersion: "v1.35.5+rke2r1",
			versions: []Version{
				{Name: "v1.35.5+rke2r2"},
				{Name: "v1.35.6+rke2r1"},
			},
			wantNil: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := filterCurrentVersion(tt.versions, tt.currentVersion)
			if tt.wantNil {
				if got != nil {
					t.Fatalf("filterCurrentVersion() = %+v, want nil", *got)
				}
				return
			}
			if got == nil {
				t.Fatal("filterCurrentVersion() = nil, want current version")
			}
			if got.Name != tt.wantName {
				t.Fatalf("filterCurrentVersion().Name = %q, want %q", got.Name, tt.wantName)
			}
			if cves := parseCVEs(got.ExtraInfo["cves"]); !slices.Equal(cves, tt.wantCVEs) {
				t.Fatalf("parseCVEs(current.ExtraInfo[cves]) = %v, want %v", cves, tt.wantCVEs)
			}
		})
	}
}
