package safety

import (
	"errors"
	"testing"
)

func http(url string) ActionDescriptor {
	return NewHTTPAction(SurfaceComputerUseHeadless, "GET", url, "", CallerContext{})
}

func TestHTTPUserinfo(t *testing.T) {
	deny := []string{
		"https://alice:secret@example.com/x",
		"http://token@api.example.com/v1",
	}
	for _, u := range deny {
		cls, err := ruleHTTPUserinfo(http(u))
		if err != nil || cls.Tier != TierHigh || !hasCategory(cls.Categories, CategoryCredentialAccess) {
			t.Errorf("userinfo %q: expected high credential_access, got %+v err=%v", u, cls, err)
		}
	}
	allow := []string{
		"https://example.com/x",
		"https://example.com/x?token=abc", // token in query is not userinfo (this rule)
	}
	for _, u := range allow {
		if _, err := ruleHTTPUserinfo(http(u)); !errors.Is(err, ErrNoClassifierOpinion) {
			t.Errorf("clean URL %q: expected escalation", u)
		}
	}
}

func TestHTTPLocalNetwork(t *testing.T) {
	deny := []string{
		"http://localhost/x",
		"https://LOCALHOST:8080/x",
		"http://127.0.0.1/",
		"http://127.0.0.5/",
		"http://169.254.169.254/latest/meta-data/", // AWS metadata
		"http://10.0.0.1/",
		"http://172.16.5.1/",
		"http://192.168.1.1/",
		"http://[::1]/",
		"http://printer.local/",
		"http://0.0.0.0/",
	}
	for _, u := range deny {
		cls, err := ruleHTTPLocalNetwork(http(u))
		if err != nil || cls.Tier != TierHigh {
			t.Errorf("local %q: expected high, got %+v err=%v", u, cls, err)
		}
	}
	allow := []string{
		"https://api.example.com/x",
		"https://github.com/",
		"https://8.8.8.8/",
	}
	for _, u := range allow {
		if _, err := ruleHTTPLocalNetwork(http(u)); !errors.Is(err, ErrNoClassifierOpinion) {
			t.Errorf("public %q: expected escalation", u)
		}
	}
}

func TestWebhookHostAllowlist(t *testing.T) {
	// Empty allowlist = allow all (parity with engine config).
	empty := NewWebhookHostAllowlistRule(nil)
	if _, err := empty.Fn(http("https://example.com")); !errors.Is(err, ErrNoClassifierOpinion) {
		t.Error("empty allowlist should never deny")
	}
	// Configured allowlist denies non-listed hosts (case-insensitive).
	r := NewWebhookHostAllowlistRule([]string{"good.example", "API.example.COM"})
	cls, err := r.Fn(http("https://good.example/x"))
	if !errors.Is(err, ErrNoClassifierOpinion) {
		t.Errorf("allowlisted host: expected escalation (not deny), got %+v err=%v", cls, err)
	}
	cls, err = r.Fn(http("https://api.example.com/v1"))
	if !errors.Is(err, ErrNoClassifierOpinion) {
		t.Errorf("allowlisted (cased) host: expected escalation, got %+v err=%v", cls, err)
	}
	cls, err = r.Fn(http("https://evil.example/x"))
	if err != nil || cls.Tier != TierHigh {
		t.Errorf("non-allowlisted: expected high deny, got %+v err=%v", cls, err)
	}
	// Non-http/webhook actions escape the rule.
	desc := NewExecAction(SurfaceWorkbench, "ls", CallerContext{})
	if _, err := r.Fn(desc); !errors.Is(err, ErrNoClassifierOpinion) {
		t.Error("non-http action should escalate")
	}
}

// hasCategory is a tiny test helper that mirrors the obvious
// `slices.Contains`-style check without dragging in the slices
// package just for tests.
func hasCategory(cats []Category, want Category) bool {
	for _, c := range cats {
		if c == want {
			return true
		}
	}
	return false
}
