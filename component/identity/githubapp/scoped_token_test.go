package githubapp

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"
)

// scoped_token_test.go -- the narrowed installation token (epic memql#5477).
// The substrate's runner clones a repository with a token that reads that
// one repository's contents and nothing else; the installation-wide token the
// cache holds is a bearer for every repository the installation covers, and
// must never be what a Job carries.

const (
	scopedToken = "ghs_SCOPED_widget_contents"
	wideToken   = "ghs_WIDE_installation"
)

// mintHub answers the mint with a token that says which kind was asked for,
// read off the body the request carried.
func mintHub(expiresAt string) *fakeHub {
	hub := newHub()
	hub.on("/app/installations/42/access_tokens", func(*http.Request) (int, string) {
		bodies := hub.sentBodies()
		token := wideToken
		if last := bodies[len(bodies)-1]; strings.Contains(last, `"repositories"`) {
			token = scopedToken
		}
		return http.StatusCreated, `{"token":"` + token + `","expires_at":"` + expiresAt + `"}`
	})
	return hub
}

func TestScopedInstallationTokenIsNarrowedAndNeverCached(t *testing.T) {
	now := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	hub := mintHub("2026-10-03T13:00:00Z")
	c := testClient(t, hub, &now)
	ctx := context.Background()

	token, expires, err := c.ScopedInstallationToken(ctx, 42, []string{"widget"}, map[string]string{"contents": "read"})
	if err != nil {
		t.Fatalf("scoped mint: %v", err)
	}
	if token != scopedToken || !expires.Equal(time.Date(2026, 10, 3, 13, 0, 0, 0, time.UTC)) {
		t.Fatalf("got %q expiring %v", token, expires)
	}

	// THE NARROWING IS ON THE WIRE: the repositories and the permissions, as
	// GitHub's access_tokens body names them.
	req := hub.seen()[0]
	if req.Method != http.MethodPost || req.Header.Get("Content-Type") != "application/json" {
		t.Fatalf("a scoped mint is a POST with a JSON body, got %s %q", req.Method, req.Header.Get("Content-Type"))
	}
	var body struct {
		Repositories []string          `json:"repositories"`
		Permissions  map[string]string `json:"permissions"`
	}
	if err := json.Unmarshal([]byte(hub.sentBodies()[0]), &body); err != nil {
		t.Fatalf("body: %v", err)
	}
	if strings.Join(body.Repositories, ",") != "widget" || len(body.Permissions) != 1 || body.Permissions["contents"] != "read" {
		t.Fatalf("the mint must name exactly what the token may do: %+v", body)
	}
	// Asked as the APP, never under anybody's token.
	if !strings.HasPrefix(req.Header.Get("Authorization"), "Bearer ey") {
		t.Fatalf("the mint must present the app assertion, got %q", req.Header.Get("Authorization"))
	}

	// UNCACHED: a second scoped mint goes to GitHub again.
	if _, _, err := c.ScopedInstallationToken(ctx, 42, []string{"widget"}, map[string]string{"contents": "read"}); err != nil {
		t.Fatal(err)
	}
	if got := hub.hits("/app/installations/42/access_tokens"); got != 2 {
		t.Fatalf("a scoped token must never be served from a cache: %d mints", got)
	}

	// AND IT NEVER ENTERS THE CACHE THE WIDE TOKEN LIVES IN, in either order:
	// the installation-wide call mints its own, and a scoped call after a wide
	// one is cached still mints a narrowed token rather than handing out the
	// wide one.
	wide, err := c.InstallationToken(ctx, 42)
	if err != nil || wide != wideToken {
		t.Fatalf("the installation-wide token: %q %v", wide, err)
	}
	again, _, err := c.ScopedInstallationToken(ctx, 42, []string{"widget"}, map[string]string{"contents": "read"})
	if err != nil || again != scopedToken {
		t.Fatalf("a scoped mint after a cached wide token answered %q (%v)", again, err)
	}
	if got := hub.hits("/app/installations/42/access_tokens"); got != 4 {
		t.Fatalf("want four mints, got %d", got)
	}
}

// A request that would mint a WIDE token -- no repositories, or no
// permissions, which GitHub reads as "all of them" -- is refused before any
// request, and so is a repository named with its owner, which GitHub would
// refuse anyway.
func TestScopedInstallationTokenRefusesAWideRequest(t *testing.T) {
	now := time.Now().UTC()
	hub := mintHub(now.Add(time.Hour).Format(time.RFC3339))
	c := testClient(t, hub, &now)
	for name, tc := range map[string]struct {
		repositories []string
		permissions  map[string]string
	}{
		"no repositories":        {nil, map[string]string{"contents": "read"}},
		"a blank repository":     {[]string{" "}, map[string]string{"contents": "read"}},
		"an owner/name":          {[]string{"acme/widget"}, map[string]string{"contents": "read"}},
		"no permissions":         {[]string{"widget"}, nil},
		"a blank permission":     {[]string{"widget"}, map[string]string{"contents": ""}},
		"no installation at all": {[]string{"widget"}, map[string]string{"contents": "read"}},
	} {
		t.Run(name, func(t *testing.T) {
			installation := int64(42)
			if name == "no installation at all" {
				installation = 0
			}
			token, _, err := c.ScopedInstallationToken(context.Background(), installation, tc.repositories, tc.permissions)
			if err == nil || token != "" {
				t.Fatalf("want a refusal and no token, got %q (%v)", token, err)
			}
		})
	}
	if n := len(hub.seen()); n != 0 {
		t.Fatalf("%d request(s) left for mints that would not have been narrowed", n)
	}
}

func TestScopedInstallationTokenAnswersTheInstallationsOwnFacts(t *testing.T) {
	now := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	ctx := context.Background()

	// An uninstalled app is ErrNotInstalled, as for the wide token.
	gone := testClient(t, newHub(), &now)
	if _, _, err := gone.ScopedInstallationToken(ctx, 42, []string{"widget"}, map[string]string{"contents": "read"}); !errors.Is(err, ErrNotInstalled) {
		t.Fatalf("want ErrNotInstalled, got %v", err)
	}
	// An unconfigured client never asks.
	hub := newHub()
	bare := New(Config{}, WithHTTPClient(&http.Client{Transport: hub}))
	if _, _, err := bare.ScopedInstallationToken(ctx, 42, []string{"widget"}, map[string]string{"contents": "read"}); !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("want ErrNotConfigured, got %v", err)
	}
	if len(hub.seen()) != 0 {
		t.Fatal("an unconfigured client made a request")
	}
	// An expiry GitHub did not state readably is a minute from now, never
	// forever.
	unreadable := testClient(t, mintHub("soon"), &now)
	_, expires, err := unreadable.ScopedInstallationToken(ctx, 42, []string{"widget"}, map[string]string{"contents": "read"})
	if err != nil || !expires.Equal(now.Add(time.Minute)) {
		t.Fatalf("want an expiry one minute out, got %v (%v)", expires, err)
	}
}
