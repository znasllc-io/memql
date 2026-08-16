package config

import (
	"context"
	"io"
	"log/slog"
	"testing"

	"github.com/znasllc-io/memql/core/buildinfo"
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
	t.Setenv("MEMQL_DEMO_MODE", "false")

	snap := loadFromEnv()

	if snap.DemoMode {
		t.Error("expected DemoMode=false")
	}
	if snap.Version != buildinfo.Version() {
		t.Errorf("expected Version=%q, got %q", buildinfo.Version(), snap.Version)
	}
}

// TestVersionIgnoresTheEnvironment pins the half of memql#3998 that is easy to
// undo by accident. `Version` read a `VERSION` env var, which meant a
// deployment could tell a node to claim a release it was not built from -- and
// a version a running process can be TOLD is not a version. The release is a
// build fact now, and the environment has no say in it.
func TestVersionIgnoresTheEnvironment(t *testing.T) {
	t.Setenv("VERSION", "v9.9.9-not-this-build")

	if got := loadFromEnv().Version; got != buildinfo.Version() {
		t.Errorf("Version=%q -- the environment overrode the link-time stamp %q", got, buildinfo.Version())
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

func TestIdentityAuthEnabled(t *testing.T) {
	cases := map[string]bool{
		"":      true, // unset => secure default (auth enforced)
		"true":  true,
		"1":     true,
		"yes":   true,
		"TRUE":  true,
		"false": false,
		"0":     false,
		"no":    false,
		"off":   false,
		"False": false,
	}
	for val, want := range cases {
		t.Setenv("MEMQL_IDENTITY_ENABLED", val)
		if got := IdentityAuthEnabled(); got != want {
			t.Errorf("IdentityAuthEnabled() with %q = %v, want %v", val, got, want)
		}
	}
}
