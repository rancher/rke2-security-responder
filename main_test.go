package main

import (
	"testing"
)

func TestIsReleaseVersion(t *testing.T) {
	tests := []struct {
		version string
		want    bool
	}{
		// Clean release versions
		{"v1.2.3", true},
		{"v1.32.2", true},
		{"v0.1.0", true},
		{"v1.2.3-rc1", true},
		{"v1.2.3-alpha", true},
		{"v1.2.3+rke2r1", true},
		{"V1.2.3", true}, // case insensitive

		// Non-release versions (git describe output)
		{"v1.2.3-5-gabcdef0", false},       // commits after tag
		{"v1.2.3-dirty", false},            // uncommitted changes
		{"v1.2.3-5-gabcdef0-dirty", false}, // both
		{"abcdef0", false},                 // just commit hash
		{"94b83fc", false},                 // short commit hash
		{"dev", false},                     // default/fallback
		{"v1.2.3-dev", false},              // dev suffix
		{"v1.2.3-test", false},             // test suffix
		{"", false},                        // empty
	}

	for _, tt := range tests {
		t.Run(tt.version, func(t *testing.T) {
			got := isReleaseVersion(tt.version)
			if got != tt.want {
				t.Errorf("isReleaseVersion(%q) = %v, want %v", tt.version, got, tt.want)
			}
		})
	}
}

func TestRun_OutsideCluster(t *testing.T) {
	err := run()
	if err == nil {
		t.Error("run() outside k8s cluster should return error")
	}
}
