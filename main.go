package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"

	"github.com/rancher/rke2-security-responder/telemetry"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

var Version = "dev"

func main() {
	if err := run(); err != nil {
		slog.Error("fatal", "error", err)
		os.Exit(1)
	}
}

func run() error {
	slog.Info("starting", "version", Version)

	if os.Getenv("DISABLE_SECURITY_RESPONDER_CHECK") == "true" {
		slog.Info("security check disabled via DISABLE_SECURITY_RESPONDER_CHECK")
		return nil
	}

	config, err := rest.InClusterConfig()
	if err != nil {
		return fmt.Errorf("in-cluster config: %w", err)
	}

	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		return fmt.Errorf("kubernetes client: %w", err)
	}

	ctx := context.Background()

	data, err := telemetry.Collect(ctx, clientset)
	if err != nil {
		return fmt.Errorf("collect telemetry: %w", err)
	}

	endpoint := os.Getenv("SECURITY_RESPONDER_ENDPOINT")
	if endpoint == "" {
		endpoint = telemetry.DefaultEndpoint
	}

	if err := telemetry.Send(ctx, data, endpoint); err != nil {
		slog.Warn("failed to send telemetry (expected in disconnected environments)", "error", err)
	}

	return nil
}
