package sitepreview

import (
	"context"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/memql"
)

// plugin.go -- self-registration and the environment-derived half of Config.
//
// REGISTERED ON EVERY NODE TYPE, for integrations/customDomain's reason
// exactly: the three builtins are declared in dsl/platform/builtins.memql,
// which EVERY binary loads, and a capability present in the DSL and absent from
// the registry is a boot-time resolution failure. Gating the registration by
// build tag would make whether an operator can open a preview depend on which
// replica their browser's stream landed on -- a coin flip at two replicas,
// which is the class of bug memql#4352 closed for workers.
//
// It is a COMPONENT in the module taxonomy and not an integration, and the test
// module_taxonomy_test.go states is what decides it: it calls nobody's API. The
// one outbound call any of this makes is the Storefront probe, and that
// deliberately lives in integrations/shopify -- the package this repository
// already classifies as the one that talks to Shopify -- reached from here
// through a builtin over the engine.

func init() {
	memql.RegisterPlugin("sitePreview", func(pctx memql.PluginContext) (memql.IntegrationProvider, error) {
		return NewIntegration(engineAdapter{pctx.Engine}, ConfigFromEnv(), pctx.Logger), nil
	})
}

// engineAdapter narrows IntegrationEngineAccess to the one method this package
// uses. Narrow deliberately: nothing here should be able to reach the AI
// provider registry or the tool surface.
type engineAdapter struct{ engine memql.IntegrationEngineAccess }

func (a engineAdapter) Execute(ctx context.Context, query string) (any, error) {
	return a.engine.Execute(ctx, query)
}

// Config is the one value this package takes from the environment.
type Config struct {
	// GrantTTL is how long a minted preview lasts, already clamped.
	GrantTTL time.Duration
}

// ConfigFromEnv reads MEMQL_SITE_PREVIEW_GRANT_TTL_MINUTES.
//
// UNSET, UNPARSEABLE AND OUT OF RANGE ALL FALL BACK, and the clamp is in
// component/sitepreview rather than here so the bound is stated once beside the
// reason for it.
func ConfigFromEnv() Config {
	minutes := 0
	if raw := strings.TrimSpace(os.Getenv("MEMQL_SITE_PREVIEW_GRANT_TTL_MINUTES")); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil {
			minutes = n
		}
	}
	return Config{GrantTTL: memql.ClampPreviewGrantTTL(time.Duration(minutes) * time.Minute)}
}

// PreviewEnterURL composes the link an operator opens. A thin forward to the
// shared spelling, so this package never states the path itself.
func PreviewEnterURL(hostname, token string) string { return memql.PreviewEnterURL(hostname, token) }

// callerUserID is the authenticated caller, or "" when there is none.
//
// A GRANT BELONGS TO A PERSON. A connector or an anonymous actor has nobody to
// own one, and handing them a preview URL for an unpublished storefront would
// be a credential owned by nothing -- so both resolve to "" here and handleOpen
// refuses by name.
func callerUserID(ctx context.Context) string {
	ac, ok := auth.AccessFromContext(ctx)
	if !ok || ac == nil {
		return ""
	}
	if ac.IsAnonymousActor() || ac.IsConnector() {
		return ""
	}
	return strings.TrimSpace(ac.UserId)
}
