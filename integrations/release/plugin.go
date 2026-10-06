package release

import (
	"context"

	"github.com/znasllc-io/memql/component/identity/githubconnect"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/packages/githubapp"
)

// plugin.go -- registration.
//
// ALWAYS REGISTERS, even with nothing configured. The builtins are declared in
// dsl/cluster/builtins.memql, which every node loads, and a capability that
// exists in the DSL and not in the registry is a boot-time resolution failure
// rather than a quiet no-op. With no MEMQL_RELEASE_REPO seeded the capability
// refuses with release_repo_unconfigured, which is a true sentence about that
// installation and exactly what the portal card renders as its setup state.
//
// The name is a LITERAL rather than a constant, for integrations/shopify's
// reason: the module-taxonomy gate finds every registration by scanning the
// SOURCE (it has to -- a registry populated by init() only shows the plugins
// the scanning binary imports) and its pattern reads a string literal.
func init() {
	memql.RegisterPlugin("release", func(pctx memql.PluginContext) (memql.IntegrationProvider, error) {
		app := &githubconnect.Resolver{Rows: githubconnect.RowReader{
			Variable: pctx.ResolveSystemVariable,
			Secret:   pctx.ResolveSystemSecret,
		}}
		return NewIntegration(pctx.Logger, pctx.Engine, resolver{
			systemSecret:   pctx.ResolveSystemSecret,
			systemVariable: pctx.ResolveSystemVariable,
			env:            osEnv,
			appToken: func(ctx context.Context, repo repoRef) (string, error) {
				cfg, _ := app.Current(ctx)
				client := githubapp.New(githubapp.Config{
					AppId: cfg.AppID, Slug: cfg.AppSlug, ClientId: cfg.ClientID,
					ClientSecret: cfg.ClientSecret, PrivateKeyB64: cfg.PrivateKeyB64,
				})
				return mintReleaseToken(ctx, client, repo)
			},
		}), nil
	})
}
