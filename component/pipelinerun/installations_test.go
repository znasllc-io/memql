package pipelinerun

import (
	"errors"
	"slices"
	"testing"

	"github.com/znasllc-io/memql/component/packages/githubapp"
)

// installations_test.go -- the permissions-changed prompt's read (epic
// memql#5479, D15): which installations of the cluster's app have not
// accepted what it now asks for.

func TestInstallationsNamesOnlyThoseThatLagTheApp(t *testing.T) {
	h := newHarness(t)
	h.github.installations = []githubapp.AppInstallation{
		{ID: 1, Account: "current", AccountType: "Organization", HTMLURL: "https://github.com/organizations/current/settings/installations/1",
			Permissions: map[string]string{"checks": "write", "contents": "read", "merge_queues": "read", "metadata": "read", "pull_requests": "write"}},
		{ID: 2, Account: "acme", AccountType: "User", HTMLURL: "https://github.com/settings/installations/2",
			Permissions: map[string]string{"contents": "read", "metadata": "read"}},
		{ID: 3, Account: "readonly", AccountType: "Organization", HTMLURL: "https://github.com/organizations/readonly/settings/installations/3",
			Permissions: map[string]string{"checks": "read", "contents": "write", "merge_queues": "read", "metadata": "read", "pull_requests": "read"},
			SuspendedAt: "2026-10-01T00:00:00Z"},
	}

	got, err := h.integ.Installations(personCtx(ownerID))
	if err != nil {
		t.Fatalf("installations: %v", err)
	}
	if len(got) != 2 || got[0].InstallationID != 2 || got[1].InstallationID != 3 {
		t.Fatalf("only the installations that lag, in id order: %+v", got)
	}
	if !slices.Equal(got[0].Missing, []string{"checks:write", "merge_queues:read", "pull_requests:read"}) ||
		got[0].Account != "acme" || got[0].AccountType != "User" || got[0].HTMLURL != "https://github.com/settings/installations/2" {
		t.Errorf("acme lags by everything pipelines added: %+v", got[0])
	}
	// A WIDER grant than asked (contents write, pull_requests write on
	// "current") is not a lag; a narrower one (checks read) is.
	if !slices.Equal(got[1].Missing, []string{"checks:write"}) || !got[1].Suspended {
		t.Errorf("readonly lags on checks alone, and says it is suspended: %+v", got[1])
	}
}

func TestInstallationsOnAClusterWithNoAppAreNone(t *testing.T) {
	h := newHarness(t)
	h.github.unconfigured = true
	got, err := h.integ.Installations(personCtx(ownerID))
	if err != nil || len(got) != 0 {
		t.Errorf("no app, no installations, no error: %+v %v", got, err)
	}
	h.github.unconfigured = false
	h.github.installationsErr = errors.New("github is down")
	if _, err := h.integ.Installations(personCtx(ownerID)); err == nil {
		t.Errorf("a GitHub that could not be asked is an error, never an empty list")
	}
	if _, err := h.integ.Installations(automationCtx()); err == nil {
		t.Errorf("nobody asks nothing")
	}
}
