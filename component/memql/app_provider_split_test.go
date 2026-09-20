package memql

import "testing"

// SplitAppReference is the one decomposition of an app door reference, and the
// model pin is the form epic memql#5391 adds (design D8).
func TestSplitAppReference(t *testing.T) {
	cases := []struct {
		name         string
		appId, model string
		ok           bool
	}{
		{"app:claude-code", "claude-code", "", true},
		{"app:codex", "codex", "", true},
		{"app:*", "*", "", true},
		{"  app:claude-code:claude-sonnet-4-6  ", "claude-code", "claude-sonnet-4-6", true},
		{"app:codex:gpt-5.4", "codex", "gpt-5.4", true},
		{"fleet:qwen3.8:27b", "", "", false},
		{"streamClaudeSonnet", "", "", false},
		{"app:", "", "", false},
		{"", "", "", false},
	}
	for _, c := range cases {
		appId, model, ok := SplitAppReference(c.name)
		if ok != c.ok || appId != c.appId || model != c.model {
			t.Fatalf("SplitAppReference(%q) = (%q, %q, %v), want (%q, %q, %v)",
				c.name, appId, model, ok, c.appId, c.model, c.ok)
		}
	}
}

// IsAppReference keeps answering the APP ID and nothing else, so every existing
// caller sees "claude-code" for a pinned entry rather than "claude-code:sonnet"
// -- which would resolve a door nobody has.
func TestIsAppReferenceIgnoresTheModelPin(t *testing.T) {
	if id, ok := IsAppReference("app:claude-code:claude-sonnet-4-6"); !ok || id != "claude-code" {
		t.Fatalf("IsAppReference = (%q, %v), want (\"claude-code\", true)", id, ok)
	}
	if id, ok := IsAppReference("app:*"); !ok || id != AppWildcardId {
		t.Fatalf("IsAppReference(\"app:*\") = (%q, %v), want (%q, true)", id, ok, AppWildcardId)
	}
}

// The ENTRY IS NAMED AFTER THE REFERENCE AN AUTHOR WROTE. The decision record
// has to say what the policy said: `app:claude-code` where the policy wrote
// `app:claude-code:claude-opus-5` would hide the pin from the one reader who
// needs to see it.
func TestAppEntryIsNamedAfterTheAuthoredReference(t *testing.T) {
	r := &ProviderRegistry{}
	entry, ok := r.appEntry(t.Context(), "u1", "claude-code", "claude-opus-5", "app:claude-code:claude-opus-5")
	if !ok || entry == nil {
		t.Fatalf("appEntry returned (%v, %v)", entry, ok)
	}
	if entry.Config.Name != "app:claude-code:claude-opus-5" {
		t.Fatalf("entry name = %q, want the reference the author wrote", entry.Config.Name)
	}
	// Config.Model stays the APP ID. It is what `doorFor` and the ledger's
	// `model` column have always meant for an app door -- which door was
	// taken -- and the PINNED model is a different fact, carried on the
	// provider and reported back by the app.
	if entry.Config.Model != "claude-code" {
		t.Fatalf("entry model = %q, want the app id", entry.Config.Model)
	}
	client, isApp := entry.Client.(*appProvider)
	if !isApp {
		t.Fatalf("entry client = %T, want *appProvider", entry.Client)
	}
	if client.model != "claude-opus-5" {
		t.Fatalf("appProvider.model = %q, want the pin", client.model)
	}
	if client.wildcard {
		t.Fatalf("a named app is not the wildcard")
	}
}
