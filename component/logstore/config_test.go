package logstore

import "testing"

func TestRetentionDaysClampsAndDefaults(t *testing.T) {
	cases := map[string]int{
		"":     30,
		"abc":  30,
		"45":   45,
		"0":    1,
		"-3":   1,
		"1000": 365,
		" 7 ":  7,
	}
	for raw, want := range cases {
		t.Setenv(EnvRetentionDays, raw)
		if got := RetentionDays(); got != want {
			t.Errorf("%s=%q -> %d, want %d", EnvRetentionDays, raw, got, want)
		}
	}
}

func TestMaxLinesPerSecondClampsAndDefaults(t *testing.T) {
	cases := map[string]int{
		"":        2000,
		"x":       2000,
		"500":     500,
		"1":       10,
		"9999999": 100000,
	}
	for raw, want := range cases {
		t.Setenv(EnvMaxLinesPerSecond, raw)
		if got := MaxLinesPerSecond(); got != want {
			t.Errorf("%s=%q -> %d, want %d", EnvMaxLinesPerSecond, raw, got, want)
		}
	}
}

func TestArchiveContainerFallsBackToTheClusterContainer(t *testing.T) {
	t.Setenv(EnvArchiveContainer, "")
	t.Setenv(envBlobContainer, "")
	if got := ArchiveContainer(); got != "" {
		t.Errorf("neither set -> %q, want empty (no archive, no delete)", got)
	}
	t.Setenv(envBlobContainer, "cluster-blobs")
	if got := ArchiveContainer(); got != "cluster-blobs" {
		t.Errorf("cluster container fallback -> %q", got)
	}
	t.Setenv(EnvArchiveContainer, " logs-archive ")
	if got := ArchiveContainer(); got != "logs-archive" {
		t.Errorf("explicit archive container -> %q", got)
	}
}

func TestResolveNodeIdPrefersTheEnvThenTheHostname(t *testing.T) {
	t.Setenv("MEMQL_NODE_ID", "bff-7")
	if got := resolveNodeId(); got != "bff-7" {
		t.Errorf("MEMQL_NODE_ID -> %q", got)
	}
	t.Setenv("MEMQL_NODE_ID", "")
	if got := resolveNodeId(); got == "" {
		t.Errorf("hostname fallback produced an empty node id")
	}
}
