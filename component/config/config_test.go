package config

import (
	"context"
	"io"
	"log/slog"
	"testing"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestNewConfig(t *testing.T) {
	c := New(testLogger())

	if c.Snapshot() == nil {
		t.Fatal("expected non-nil snapshot")
	}
}

func TestConfigImplementsDependency(t *testing.T) {
	c := New(testLogger())

	c.Start(context.Background())

	if !c.IsRunning() {
		t.Error("expected IsRunning=true")
	}

	select {
	case <-c.Ready():
		// good
	default:
		t.Error("expected Ready channel to be closed after Start")
	}

	if c.Order() != 1 {
		t.Errorf("expected Order=1, got %d", c.Order())
	}

	if c.ComponentName() != ComponentName {
		t.Errorf("expected ComponentName=%q, got %q", ComponentName, c.ComponentName())
	}

	c.Stop(context.Background())
}

func TestLoadFromEnv(t *testing.T) {
	// Set a test env var
	t.Setenv("MEMQL_STEP_CACHE_ENABLED", "true")
	t.Setenv("MEMQL_DEMO_MODE", "false")
	t.Setenv("VERSION", "test-v1.0")

	snap := loadFromEnv()

	if !snap.EngineStepCacheEnabled {
		t.Error("expected EngineStepCacheEnabled=true")
	}
	if snap.DemoMode {
		t.Error("expected DemoMode=false")
	}
	if snap.Version != "test-v1.0" {
		t.Errorf("expected Version=test-v1.0, got %q", snap.Version)
	}
}

func TestEnvBool(t *testing.T) {
	tests := []struct {
		value    string
		expected bool
	}{
		{"true", true},
		{"TRUE", true},
		{"True", true},
		{"1", true},
		{"yes", true},
		{"YES", true},
		{"false", false},
		{"0", false},
		{"no", false},
		{"", false},
		{"  true  ", true},
	}

	for _, tt := range tests {
		t.Run(tt.value, func(t *testing.T) {
			t.Setenv("TEST_BOOL", tt.value)
			if got := envBool("TEST_BOOL"); got != tt.expected {
				t.Errorf("envBool(%q) = %v, want %v", tt.value, got, tt.expected)
			}
		})
	}
}
