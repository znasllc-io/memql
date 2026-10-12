package main

import (
	"github.com/znasllc-io/memql/component/campaigns"
	"github.com/znasllc-io/memql/component/server"
	"net/url"
	"slices"
	"testing"
)

func TestEmailLinkPathsAgreeWithWhatTheRendererBuilds(t *testing.T) {
	cfg := campaigns.Config{UnsubscribeSecret: "email-path-test-secret", UnsubscribeBaseURL: "https://example.test"}
	p := campaigns.LinkPayload{OwnerUserID: "owner", RecipientID: "recipient", EmailDigest: campaigns.EmailDigest("reader@example.test"), ExpiresAt: 2000000000, TemplateID: "template", TemplateRevision: "2026-10-01T00:00:00Z"}
	for _, tc := range []struct {
		path  string
		build func(campaigns.Config, campaigns.LinkPayload) (string, error)
	}{{campaigns.ConfirmPath, campaigns.ConfirmURL}, {campaigns.PreferencesPath, campaigns.PreferencesURL}, {campaigns.ViewPath, campaigns.ViewURL}} {
		if !slices.Contains(server.EmailLinkPaths(), tc.path) {
			t.Fatalf("undeclared %q", tc.path)
		}
		address, err := tc.build(cfg, p)
		if err != nil {
			t.Fatal(err)
		}
		parsed, err := url.Parse(address)
		if err != nil {
			t.Fatal(err)
		}
		if parsed.Path != tc.path {
			t.Fatalf("builder path=%q declared=%q", parsed.Path, tc.path)
		}
	}
}
