package main

import (
	"os"
	"testing"
)

func TestRun_DisabledEnvVar(t *testing.T) {
	t.Setenv("DISABLE_SECURITY_RESPONDER_CHECK", "true")

	err := run()
	if err != nil {
		t.Errorf("run() with DISABLE_SECURITY_RESPONDER_CHECK=true returned error: %v", err)
	}
}

func TestRun_DisabledEnvVarNotSet(t *testing.T) {
	// Ensure env var is not set (t.Setenv automatically restores original value)
	t.Setenv("DISABLE_SECURITY_RESPONDER_CHECK", "")
	os.Unsetenv("DISABLE_SECURITY_RESPONDER_CHECK") //nolint:errcheck // test cleanup

	err := run()
	// Expect error because we're not in a k8s cluster
	if err == nil {
		t.Error("run() without k8s cluster should return error")
	}
}
