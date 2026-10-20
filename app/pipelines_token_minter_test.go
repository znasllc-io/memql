package app

import (
	"context"
	"errors"
	"maps"
	"slices"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/identity/githubapp"
)

// TestPipelinesCloneTokenIsNarrowedToTheRepository: the token a step clones
// with reads ONE repository's contents. The installation-wide token would
// reach every repository the installation covers -- from a laptop's process
// environment, or a Job's Secret -- so the mint names the repository and the
// one permission, and an anonymous clone asks GitHub for nothing.
func TestPipelinesCloneTokenIsNarrowedToTheRepository(t *testing.T) {
	type call struct {
		installation int64
		repos        []string
		perms        map[string]string
	}
	var calls []call
	m := pipelinesTokenMinter{mint: func(_ context.Context, installation int64, repos []string, perms map[string]string) (string, time.Time, error) {
		calls = append(calls, call{installation, repos, perms})
		return "ghs_narrow", time.Now().Add(time.Hour), nil
	}}

	token, err := m.CloneToken(context.Background(), 42, "acme", "widget")
	if err != nil || token != "ghs_narrow" {
		t.Fatalf("CloneToken = %q, %v", token, err)
	}
	if len(calls) != 1 || calls[0].installation != 42 || !slices.Equal(calls[0].repos, []string{"widget"}) ||
		!maps.Equal(calls[0].perms, map[string]string{"contents": "read"}) {
		t.Fatalf("mint calls = %+v, want installation 42, repository widget (a name within the account), contents:read", calls)
	}

	if token, err := m.CloneToken(context.Background(), 0, "acme", "widget"); err != nil || token != "" || len(calls) != 1 {
		t.Errorf("an anonymous clone = %q, %v after %d mints; want no token and no call to GitHub", token, err, len(calls))
	}

	failing := pipelinesTokenMinter{mint: func(context.Context, int64, []string, map[string]string) (string, time.Time, error) {
		return "", time.Time{}, githubapp.ErrNotInstalled
	}}
	if _, err := failing.CloneToken(context.Background(), 42, "acme", "widget"); !errors.Is(err, githubapp.ErrNotInstalled) {
		t.Errorf("a refused mint = %v, want GitHub's refusal carried as it is", err)
	}
	if _, err := (pipelinesTokenMinter{}).CloneToken(context.Background(), 42, "acme", "widget"); !errors.Is(err, githubapp.ErrNotConfigured) {
		t.Errorf("no client = %v, want ErrNotConfigured", err)
	}
	if _, err := (&App{}).pipelinesTokenMinterFor().CloneToken(context.Background(), 42, "acme", "widget"); !errors.Is(err, githubapp.ErrNotConfigured) {
		t.Errorf("a node with no packages integration = %v, want ErrNotConfigured", err)
	}
}

func TestPipelinesRepositoryReadTokenIsNarrowedAndCannotWrite(t *testing.T) {
	calls := 0
	m := pipelinesTokenMinter{mint: func(_ context.Context, installation int64, repos []string, perms map[string]string) (string, time.Time, error) {
		calls++
		if installation != 42 || !slices.Equal(repos, []string{"widget"}) || !maps.Equal(perms, map[string]string{"contents": "read", "checks": "read", "pull_requests": "read"}) {
			t.Fatalf("overbroad read grant: %d %v %v", installation, repos, perms)
		}
		return "fixture-read-token", time.Now().Add(time.Hour), nil
	}}
	token, err := m.RepositoryReadToken(context.Background(), 42, "acme", "widget")
	if err != nil || token != "fixture-read-token" || calls != 1 {
		t.Fatalf("read grant: %q %v", token, err)
	}
	for _, invalid := range []struct {
		id          int64
		owner, repo string
	}{{0, "acme", "widget"}, {42, "", "widget"}, {42, "acme", ""}} {
		if _, err := m.RepositoryReadToken(context.Background(), invalid.id, invalid.owner, invalid.repo); err == nil {
			t.Fatal("unscoped grant accepted")
		}
	}
	if calls != 1 {
		t.Fatal("invalid scope reached GitHub")
	}
	if _, err := (pipelinesTokenMinter{}).RepositoryReadToken(context.Background(), 42, "acme", "widget"); !errors.Is(err, githubapp.ErrNotConfigured) {
		t.Fatalf("missing client: %v", err)
	}
	for _, mintErr := range []error{nil, githubapp.ErrNotInstalled} {
		m.mint = func(context.Context, int64, []string, map[string]string) (string, time.Time, error) {
			return "", time.Time{}, mintErr
		}
		if _, err := m.RepositoryReadToken(context.Background(), 42, "acme", "widget"); err == nil {
			t.Fatal("empty or refused credential accepted")
		}
	}
}
