package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"regexp"
	"strings"

	"github.com/rancher/rke2-security-responder/telemetry"
	"github.com/sirupsen/logrus"
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
		logrus.SetLevel(logrus.DebugLevel)
	}

	if err := run(); err != nil {
		logrus.WithError(err).Fatal("run failed")
	}
}

func run() error {
	logrus.WithField("version", Version).Info("starting")

	if os.Getenv("DISABLE_SECURITY_RESPONDER_CHECK") == "true" {
		logrus.Info("security check disabled via DISABLE_SECURITY_RESPONDER_CHECK")
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

	// Mark non-release builds for server-side filtering
	// Clean tags: v1.2.3, v1.2.3-rc1, v1.2.3+rke2r1
	// Non-clean: v1.2.3-5-gabcdef (commits after tag), v1.2.3-dirty, abcdef (no tag), dev
	if !isReleaseVersion(Version) || os.Getenv("SECURITY_RESPONDER_DEV") == "true" {
		data.ExtraFieldInfo["dev"] = true
	}

	if *debug {
		jsonData, _ := json.MarshalIndent(data, "", "  ")
		logrus.WithField("payload", string(jsonData)).Info("debug mode: skipping send")
		return nil
	}

	endpoint := os.Getenv("SECURITY_RESPONDER_ENDPOINT")
	if endpoint == "" {
		endpoint = telemetry.DefaultEndpoint
	}

	if _, err := telemetry.Send(ctx, data, endpoint); err != nil {
		logrus.WithError(err).Warn("failed to send (expected in disconnected environments)")
	}

	return nil
}

// releaseVersionRe matches clean release tags: v1.2.3, v1.2.3-rc1, v1.2.3+rke2r1
// but NOT git describe output like v1.2.3-5-gabcdef or v1.2.3-dirty
var releaseVersionRe = regexp.MustCompile(`^v\d+\.\d+\.\d+([+-][a-zA-Z][a-zA-Z0-9]*)?$`)

func isReleaseVersion(version string) bool {
	v := strings.ToLower(version)
	// Explicit exclusions for git describe non-release patterns
	if strings.Contains(v, "dirty") || strings.Contains(v, "dev") || strings.Contains(v, "test") {
		return false
	}
	// Pattern like v1.2.3-5-gabcdef (commits after tag)
	if matched, _ := regexp.MatchString(`-\d+-g[a-f0-9]+`, v); matched {
		return false
	}
	return releaseVersionRe.MatchString(v)
}
