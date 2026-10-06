package githubapp

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"
)

// TestInstallationsListsEveryInstallationTheAppSees: the app asking as itself
// (the app JWT), page by page until a short page, each installation with the
// permissions and events it has ACCEPTED -- the facts a permissions-changed
// prompt compares against what the app now asks for.
func TestInstallationsListsEveryInstallationTheAppSees(t *testing.T) {
	now := time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)
	hub := newHub()
	var pages []string
	hub.on("/app/installations", func(r *http.Request) (int, string) {
		if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") || strings.Count(r.Header.Get("Authorization"), ".") != 2 {
			t.Errorf("the list is asked under the app JWT: %q", r.Header.Get("Authorization"))
		}
		page := r.URL.Query().Get("page")
		pages = append(pages, page)
		if r.URL.Query().Get("per_page") != "100" {
			t.Errorf("per_page = %q", r.URL.Query().Get("per_page"))
		}
		if page == "1" {
			var b strings.Builder
			b.WriteString("[")
			for i := 0; i < 100; i++ {
				if i > 0 {
					b.WriteString(",")
				}
				b.WriteString(`{"id":` + itoa(1000+i) + `,"account":{"login":"org` + itoa(i) + `","type":"Organization"},"html_url":"https://github.com/organizations/org` + itoa(i) + `/settings/installations/` + itoa(1000+i) + `","permissions":{"checks":"write","contents":"read"},"events":["push"]}`)
			}
			b.WriteString("]")
			return 200, b.String()
		}
		return 200, `[{"id":7,"account":{"login":"acme","type":"User"},"html_url":"https://github.com/settings/installations/7","permissions":{"contents":"read","metadata":"read"},"events":[],"suspended_at":"2026-10-01T00:00:00Z"}]`
	})
	c := testClient(t, hub, &now)

	got, err := c.Installations(context.Background())
	if err != nil {
		t.Fatalf("installations: %v", err)
	}
	if len(got) != 101 || strings.Join(pages, ",") != "1,2" {
		t.Fatalf("every page until a short one: %d installations over pages %v", len(got), pages)
	}
	last := got[100]
	if last.ID != 7 || last.Account != "acme" || last.AccountType != "User" || last.HTMLURL != "https://github.com/settings/installations/7" ||
		last.Permissions["contents"] != "read" || last.Permissions["checks"] != "" || last.SuspendedAt == "" {
		t.Errorf("installation = %+v", last)
	}
}

func TestInstallationsOnAClusterWithNoAppIsNotConfigured(t *testing.T) {
	c := New(Config{})
	if _, err := c.Installations(context.Background()); err != ErrNotConfigured {
		t.Errorf("err = %v, want ErrNotConfigured", err)
	}
}

func itoa(n int) string {
	const digits = "0123456789"
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{digits[n%10]}, b...)
		n /= 10
	}
	return string(b)
}
