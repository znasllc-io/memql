//go:build bff

package app

import (
	"github.com/znasllc-io/memql/component/campaigns"
	"github.com/znasllc-io/memql/component/server"
	"strings"
)

// mountEmailLinkEndpoints serves the owner-approved recipient browser pages
// (2026-10-11, email-layouts-automations design section 4.4). A recipient has no
// MemQL session; each page verifies its own sealed capability. Always mounted:
// an absent secret rejects links rather than silently removing their routes.
//
// A page that cannot help names whom to write to from the organization's own
// email connection, through the email sender resolved when the page asks (the
// campaigns worker's seam, for the worker's reason: the registry fills after this).
func (a *App) mountEmailLinkEndpoints() {
	handler := campaigns.NewEmailLinkHandler(&CampaignsEngineAdapter{Engine: a.engine}, campaigns.LoadConfig(), a.Logger, a.gateDBGetter()).WithSender(a.campaignEmailSender)
	for _, path := range server.EmailLinkPaths() {
		a.handleRoute("GET "+path, handler)
		// GETs are safe for link scanners. Only these two deliberate form actions
		// change consent; a view page has no write surface.
		if !strings.HasSuffix(path, campaigns.ViewPath) {
			a.handleRoute("POST "+path, handler)
		}
	}
}
