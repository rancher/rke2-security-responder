package telemetry

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
)

func TestExtractImageVersion(t *testing.T) {
	tests := []struct {
		image    string
		expected string
	}{
		{"nginx:1.21", "1.21"},
		{"nginx:latest", "latest"},
		{"registry.example.com/nginx:v1.0.0", "v1.0.0"},
		{"nginx", ""},
		{"nginx@sha256:abc123", "abc123"},        // digest-only: LastIndex finds sha256's colon
		{"nginx:v1.0.0@sha256:abc123", "abc123"}, // tag+digest: LastIndex finds sha256's colon (edge case)
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

func TestGetSELinuxStatus(t *testing.T) {
	tests := []struct {
		name     string
		labels   map[string]string
		expected string
	}{
		{
			name:     "enabled",
			labels:   map[string]string{"security.alpha.kubernetes.io/selinux": "enabled"},
			expected: "enabled",
		},
		{
			name:     "disabled",
			labels:   map[string]string{"security.alpha.kubernetes.io/selinux": "disabled"},
			expected: "disabled",
		},
		{
			name:     "other value",
			labels:   map[string]string{"security.alpha.kubernetes.io/selinux": "permissive"},
			expected: "disabled",
		},
		{
			name:     "no label",
			labels:   map[string]string{},
			expected: "unknown",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Labels: tt.labels}}
			result := getSELinuxStatus(node)
			if result != tt.expected {
				t.Errorf("getSELinuxStatus() = %q, want %q", result, tt.expected)
			}
		})
	}
}

func TestCollect_BasicCluster(t *testing.T) {
	clientset := fake.NewSimpleClientset(
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

	data, err := Collect(context.Background(), clientset)
	if err != nil {
		t.Fatalf("Collect() error = %v", err)
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
	if data.ExtraFieldInfo["os"] != "Ubuntu 22.04" {
		t.Errorf("os = %v, want Ubuntu 22.04", data.ExtraFieldInfo["os"])
	}
	if data.ExtraFieldInfo["arch"] != "amd64" {
		t.Errorf("arch = %v, want amd64", data.ExtraFieldInfo["arch"])
	}
}

func TestCollect_CNIDetection(t *testing.T) {
	tests := []struct {
		name        string
		daemonSet   string
		image       string
		expectedCNI string
	}{
		{"canal", "rke2-canal", "rancher/hardened-calico:v3.26.0", "canal"},
		{"flannel", "kube-flannel-ds", "flannel/flannel:v0.22.0", "flannel"},
		{"calico", "calico-node", "calico/node:v3.26.0", "calico"},
		{"cilium", "cilium", "cilium/cilium:v1.14.0", "cilium"},
		{"weave", "weave-net", "weaveworks/weave-kube:2.8.1", "weave"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clientset := fake.NewSimpleClientset(
				&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "kube-system", UID: "uuid"}},
				&corev1.Node{
					ObjectMeta: metav1.ObjectMeta{Name: "node-1"},
					Status:     corev1.NodeStatus{NodeInfo: corev1.NodeSystemInfo{OSImage: "test", KernelVersion: "5.0", Architecture: "amd64"}},
				},
				&appsv1.DaemonSet{
					ObjectMeta: metav1.ObjectMeta{Name: tt.daemonSet, Namespace: "kube-system"},
					Spec: appsv1.DaemonSetSpec{
						Template: corev1.PodTemplateSpec{
							Spec: corev1.PodSpec{
								Containers: []corev1.Container{{Image: tt.image}},
							},
						},
					},
				},
			)

			data, err := Collect(context.Background(), clientset)
			if err != nil {
				t.Fatalf("Collect() error = %v", err)
			}

			if data.ExtraFieldInfo["cni-plugin"] != tt.expectedCNI {
				t.Errorf("cni-plugin = %v, want %v", data.ExtraFieldInfo["cni-plugin"], tt.expectedCNI)
			}
		})
	}
}

func TestCollect_IngressDetection(t *testing.T) {
	tests := []struct {
		name            string
		deploymentName  string
		image           string
		expectedIngress string
	}{
		{"nginx", "rke2-ingress-nginx-controller", "rancher/nginx-ingress-controller:v1.9.0", "rke2-ingress-nginx"},
		{"traefik", "traefik", "traefik:v2.10", "traefik"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clientset := fake.NewSimpleClientset(
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

			data, err := Collect(context.Background(), clientset)
			if err != nil {
				t.Fatalf("Collect() error = %v", err)
			}

			if data.ExtraFieldInfo["ingress-controller"] != tt.expectedIngress {
				t.Errorf("ingress-controller = %v, want %v", data.ExtraFieldInfo["ingress-controller"], tt.expectedIngress)
			}
		})
	}
}

func TestCollect_GPUDetection(t *testing.T) {
	clientset := fake.NewSimpleClientset(
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

	data, err := Collect(context.Background(), clientset)
	if err != nil {
		t.Fatalf("Collect() error = %v", err)
	}

	if data.ExtraFieldInfo["gpu-nodes"] != 1 {
		t.Errorf("gpu-nodes = %v, want 1", data.ExtraFieldInfo["gpu-nodes"])
	}
	if data.ExtraFieldInfo["gpu-vendor"] != "nvidia" {
		t.Errorf("gpu-vendor = %v, want nvidia", data.ExtraFieldInfo["gpu-vendor"])
	}
}

func TestCollect_RancherManaged(t *testing.T) {
	clientset := fake.NewSimpleClientset(
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "kube-system", UID: "uuid"}},
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "cattle-system"}},
		&corev1.Node{
			ObjectMeta: metav1.ObjectMeta{Name: "node-1"},
			Status:     corev1.NodeStatus{NodeInfo: corev1.NodeSystemInfo{OSImage: "test", KernelVersion: "5.0", Architecture: "amd64"}},
		},
		&appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{Name: "cattle-cluster-agent", Namespace: "cattle-system"},
			Spec: appsv1.DeploymentSpec{
				Template: corev1.PodTemplateSpec{
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{{
							Image: "rancher/rancher-agent:v2.8.0",
							Env: []corev1.EnvVar{
								{Name: "CATTLE_INSTALL_UUID", Value: "rancher-install-uuid-123"},
							},
						}},
					},
				},
			},
		},
	)

	data, err := Collect(context.Background(), clientset)
	if err != nil {
		t.Fatalf("Collect() error = %v", err)
	}

	if data.ExtraFieldInfo["rancher-managed"] != true {
		t.Errorf("rancher-managed = %v, want true", data.ExtraFieldInfo["rancher-managed"])
	}
	if data.ExtraFieldInfo["rancher-version"] != "v2.8.0" {
		t.Errorf("rancher-version = %v, want v2.8.0", data.ExtraFieldInfo["rancher-version"])
	}
	if data.ExtraFieldInfo["rancher-install-uuid"] != "rancher-install-uuid-123" {
		t.Errorf("rancher-install-uuid = %v, want rancher-install-uuid-123", data.ExtraFieldInfo["rancher-install-uuid"])
	}
}

func TestCollect_MissingKubeSystem(t *testing.T) {
	clientset := fake.NewSimpleClientset()

	_, err := Collect(context.Background(), clientset)
	if err == nil {
		t.Error("Collect() expected error for missing kube-system namespace")
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

func TestCollect_GPUOperatorDetection(t *testing.T) {
	clientset := fake.NewSimpleClientset(
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

	data, err := Collect(context.Background(), clientset)
	if err != nil {
		t.Fatalf("Collect() error = %v", err)
	}

	if data.ExtraFieldInfo["gpu-operator"] != "nvidia-gpu-operator" {
		t.Errorf("gpu-operator = %v, want nvidia-gpu-operator", data.ExtraFieldInfo["gpu-operator"])
	}
}

func TestCollect_IngressAsDaemonSet(t *testing.T) {
	clientset := fake.NewSimpleClientset(
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

	data, err := Collect(context.Background(), clientset)
	if err != nil {
		t.Fatalf("Collect() error = %v", err)
	}

	if data.ExtraFieldInfo["ingress-controller"] != "rke2-ingress-nginx" {
		t.Errorf("ingress-controller = %v, want rke2-ingress-nginx", data.ExtraFieldInfo["ingress-controller"])
	}
}
