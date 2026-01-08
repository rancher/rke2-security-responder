package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"os"

	"github.com/rancher/rke2-security-responder/telemetry"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

var Version = "dev"

var (
	verbose = flag.Bool("verbose", false, "enable verbose logging")
	debug   = flag.Bool("debug", false, "dry-run: collect data but don't send")
)

func main() {
	flag.Parse()

	if *verbose {
		slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug})))
	}

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
		return fmt.Errorf("collect data: %w", err)
	}

	if *debug {
		jsonData, _ := json.MarshalIndent(data, "", "  ")
		slog.Info("debug mode: skipping send", "payload", string(jsonData))
		return nil
	}

	endpoint := os.Getenv("SECURITY_RESPONDER_ENDPOINT")
	if endpoint == "" {
		endpoint = telemetry.DefaultEndpoint
	}

	if err := telemetry.Send(ctx, data, endpoint); err != nil {
		slog.Warn("failed to send (expected in disconnected environments)", "error", err)
	}

	return nil
}
