package identity

import "testing"

// The post-login landing is the MemQL OS shell (epic memql#4984). It was the
// portal until that epic retired it, and these cases are the portal ones with
// the host changed -- the RULE has not moved, only the label it composes.

func TestShellHomeURLFromClusterDomain(t *testing.T) {
	got := ShellHomeURL("memql.localhost", "https://identity.example.test")
	if got != "https://os.memql.localhost/" {
		t.Fatalf("ShellHomeURL = %q, want the cluster domain to win", got)
	}
}

func TestShellHomeURLFromIdentityBaseURL(t *testing.T) {
	got := ShellHomeURL("", "https://identity.memql.localhost")
	if got != "https://os.memql.localhost/" {
		t.Fatalf("ShellHomeURL = %q, want identity. rewritten to os.", got)
	}
}

func TestShellHomeURLEmptyWhenUnnameable(t *testing.T) {
	// A host that is not identity.<something> cannot be rewritten, and
	// guessing would land the browser at an origin nothing serves.
	if got := ShellHomeURL("", "https://auth.example.com"); got != "" {
		t.Fatalf("ShellHomeURL = %q, want empty when the host is not identity.<domain>", got)
	}
	if got := ShellHomeURL("", ""); got != "" {
		t.Fatalf("ShellHomeURL = %q, want empty with nothing to derive from", got)
	}
}

// The fallback is same-origin, never /admin/ -- that console is 410 Gone.
func TestDefaultPostLoginLandingNeverAdmin(t *testing.T) {
	if got := DefaultPostLoginLanding("", "https://auth.example.com"); got != "/me" {
		t.Fatalf("DefaultPostLoginLanding = %q, want the same-origin /me fallback", got)
	}
	if got := DefaultPostLoginLanding("acme.example", ""); got != "https://os.acme.example/" {
		t.Fatalf("DefaultPostLoginLanding = %q, want the shell origin", got)
	}
}
