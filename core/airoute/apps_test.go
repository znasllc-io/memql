package airoute

import "testing"

// The runnable set is CLOSED and it is declared here rather than in
// component/worker, because the DSL parser has to refuse `app:<id>` for an id
// outside it at LOAD and component/language cannot import component/worker --
// component/worker imports component/language.
func TestRunnableAppsIsClosedAndSorted(t *testing.T) {
	apps := RunnableApps()
	if len(apps) != 2 {
		t.Fatalf("RunnableApps() = %v, want exactly the two the engine drives", apps)
	}
	if apps[0] != AppClaudeCode || apps[1] != AppCodex {
		t.Fatalf("RunnableApps() = %v, want [%q %q] in sorted order", apps, AppClaudeCode, AppCodex)
	}
	for _, id := range apps {
		if !IsRunnableApp(id) {
			t.Fatalf("IsRunnableApp(%q) = false for a member of the set", id)
		}
	}
	// Whitespace is trimmed; an unknown id is not runnable, and neither is the
	// wildcard -- `app:*` is a SELECTOR over the set, not a member of it.
	for _, id := range []string{"", " ", "*", "claude", "Claude-Code", "gemini-cli"} {
		if IsRunnableApp(id) {
			t.Fatalf("IsRunnableApp(%q) = true; the set is closed", id)
		}
	}
	if !IsRunnableApp("  claude-code  ") {
		t.Fatalf("IsRunnableApp trims whitespace")
	}
}

// RunnableApps hands back a COPY. A caller that sorted or appended to the
// returned slice would be editing the closed set for every later reader.
func TestRunnableAppsIsNotAliased(t *testing.T) {
	first := RunnableApps()
	first[0] = "tampered"
	if RunnableApps()[0] != AppClaudeCode {
		t.Fatalf("RunnableApps() returned an alias of the package's own slice")
	}
}
