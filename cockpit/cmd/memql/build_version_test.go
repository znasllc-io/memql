package main

import "testing"

func TestVersionWithRevision(t *testing.T) {
	for _, tc := range []struct {
		base, revision string
		dirty          bool
		want           string
	}{
		{"0.16.0", "abc123", false, "0.16.0+gabc123"},
		{"0.16.0", "abc123", true, "0.16.0+gabc123.dirty"},
		{"0.16.0+custom", "abc123", false, "0.16.0+custom.gabc123"},
		{"0.16.0+custom", "", false, "0.16.0+custom.unknown"},
		{"STAMPED", "", false, "STAMPED+unknown"},
	} {
		if got := versionWithRevision(tc.base, tc.revision, tc.dirty); got != tc.want {
			t.Fatalf("got %q, want %q", got, tc.want)
		}
	}
}
