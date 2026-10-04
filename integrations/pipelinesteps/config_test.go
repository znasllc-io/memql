package pipelinesteps

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/node"
)

// envOf is a getenv over a fixed map: the configuration is read through the
// function ConfigFromEnv is handed, never the process environment.
func envOf(kv map[string]string) func(string) string {
	return func(key string) string { return kv[key] }
}

// The environment names are spelled out here rather than taken from the
// package's constants, so a typo in a constant fails this test instead of
// being read back through itself.
func TestConfigFromEnvDefaultsAndClamps(t *testing.T) {
	t.Run("an empty environment is the defaults", func(t *testing.T) {
		got := ConfigFromEnv(envOf(map[string]string{"MEMQL_NODE_ID": "workbench-1"}))
		want := Config{
			Namespace:          "memql-pipelines",
			CloneImage:         "",
			CacheClaim:         "memql-pipelines-cache",
			StepServiceAccount: "memql-pipelines-step",
			RunCeiling:         120 * time.Minute,
			LogStoreMaxLines:   2000,
			ArtifactMaxBytes:   64 << 20,
			ArchiveMaxBytes:    64 << 20,
			DefaultStepTimeout: 20 * time.Minute,
			JobTTL:             30 * time.Minute,
			ScheduleTimeout:    10 * time.Minute,
			PollInterval:       2 * time.Second,
			HeartbeatInterval:  10 * time.Second,
			HeartbeatStale:     45 * time.Second,
			NodeID:             "workbench-1",
		}
		if got != want {
			t.Errorf("ConfigFromEnv(empty) =\n  %+v\nwant\n  %+v", got, want)
		}
	})

	t.Run("values inside the bounds are taken as given, trimmed", func(t *testing.T) {
		got := ConfigFromEnv(envOf(map[string]string{
			"MEMQL_PIPELINES_NAMESPACE":           "  ci-steps ",
			"MEMQL_PIPELINES_CLONE_IMAGE":         " registry.example.com/library/git:2 ",
			"MEMQL_PIPELINES_RUN_MAX_MINUTES":     " 90 ",
			"MEMQL_PIPELINES_LOG_STORE_MAX_LINES": "5000",
			"MEMQL_PIPELINES_ARTIFACT_MAX_BYTES":  "1048576",
			"MEMQL_NODE_ID":                       "  workbench-2\t",
		}))
		if got.Namespace != "ci-steps" {
			t.Errorf("Namespace = %q, want ci-steps", got.Namespace)
		}
		if got.CloneImage != "registry.example.com/library/git:2" {
			t.Errorf("CloneImage = %q, want registry.example.com/library/git:2", got.CloneImage)
		}
		if got.RunCeiling != 90*time.Minute {
			t.Errorf("RunCeiling = %v, want 1h30m0s", got.RunCeiling)
		}
		if got.LogStoreMaxLines != 5000 {
			t.Errorf("LogStoreMaxLines = %d, want 5000", got.LogStoreMaxLines)
		}
		if got.ArtifactMaxBytes != 1<<20 {
			t.Errorf("ArtifactMaxBytes = %d, want 1048576 (the lower bound itself is inside the bounds)", got.ArtifactMaxBytes)
		}
		if got.NodeID != "workbench-2" {
			t.Errorf("NodeID = %q, want the trimmed MEMQL_NODE_ID", got.NodeID)
		}
	})

	// One row per knob and direction. A value the operator set but that makes
	// no sense as a number falls back to the DEFAULT, never to a bound: zero
	// minutes is not a request for a five-minute ceiling.
	type knob struct {
		name string
		read func(Config) int64
	}
	ceiling := knob{"MEMQL_PIPELINES_RUN_MAX_MINUTES", func(c Config) int64 { return int64(c.RunCeiling / time.Minute) }}
	lines := knob{"MEMQL_PIPELINES_LOG_STORE_MAX_LINES", func(c Config) int64 { return int64(c.LogStoreMaxLines) }}
	artifacts := knob{"MEMQL_PIPELINES_ARTIFACT_MAX_BYTES", func(c Config) int64 { return c.ArtifactMaxBytes }}

	for _, tc := range []struct {
		knob knob
		raw  string
		want int64
	}{
		{ceiling, "1", 5},
		{ceiling, "4", 5},
		{ceiling, "5", 5},
		{ceiling, "1440", 1440},
		{ceiling, "1441", 1440},
		{ceiling, "100000", 1440},
		{ceiling, "0", 120},
		{ceiling, "-30", 120},
		{ceiling, "ninety", 120},
		{ceiling, "1.5", 120},
		{ceiling, "90m", 120},

		{lines, "10", 100},
		{lines, "99", 100},
		{lines, "100000", 100000},
		{lines, "100001", 100000},
		{lines, "0", 2000},
		{lines, "-1", 2000},
		{lines, "lots", 2000},

		{artifacts, "1", 1 << 20},
		{artifacts, "1048575", 1 << 20},
		{artifacts, "268435456", 256 << 20},
		{artifacts, "268435457", 256 << 20},
		{artifacts, "1073741824", 256 << 20},
		{artifacts, "99999999999999999999999", 64 << 20},
		{artifacts, "0", 64 << 20},
		{artifacts, "64MiB", 64 << 20},
	} {
		t.Run(tc.knob.name+"="+tc.raw, func(t *testing.T) {
			cfg := ConfigFromEnv(envOf(map[string]string{tc.knob.name: tc.raw}))
			if got := tc.knob.read(cfg); got != tc.want {
				t.Errorf("%s=%q read as %d, want %d", tc.knob.name, tc.raw, got, tc.want)
			}
		})
	}

	t.Run("a blank namespace is the default namespace", func(t *testing.T) {
		got := ConfigFromEnv(envOf(map[string]string{"MEMQL_PIPELINES_NAMESPACE": "   "}))
		if got.Namespace != "memql-pipelines" {
			t.Errorf("Namespace = %q, want memql-pipelines", got.Namespace)
		}
	})
}

// TestNodeIDAgreesWithTheNodeIdentity pins the one thing that would silently
// break re-attachment: the runner stamps its NodeID on the Job's heartbeat and
// on Where.NodeID, and the agent compares those against PeerInfo.node_id,
// which is component/node's Identity.ID. integrations/workbench's selfNodeId
// repeats the same derivation for the same reason; two spellings of "who am I"
// would make a live runner look like somebody else's.
func TestNodeIDAgreesWithTheNodeIdentity(t *testing.T) {
	t.Setenv("MEMQL_NODE_ID", "  workbench-7  ")
	got := ConfigFromEnv(nil).NodeID
	if want := node.NewIdentity("test").ID; got != want || got != "workbench-7" {
		t.Errorf("NodeID = %q, node identity = %q, want both workbench-7", got, want)
	}

	t.Setenv("MEMQL_NODE_ID", "")
	host, err := os.Hostname()
	if err != nil || strings.TrimSpace(host) == "" {
		t.Skipf("no hostname on this machine (%v): the fallback has nothing to agree on", err)
	}
	got = ConfigFromEnv(nil).NodeID
	if want := node.NewIdentity("test").ID; got != want || got != strings.TrimSpace(host) {
		t.Errorf("NodeID with MEMQL_NODE_ID unset = %q, node identity = %q, want both the hostname %q", got, want, host)
	}
}
