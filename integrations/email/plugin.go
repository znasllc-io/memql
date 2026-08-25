package email

import "github.com/znasllc-io/memql/component/memql"

// init self-registers the email integration as a plug-in.
//
// Sender resolution is two-stage. NewSenderFromEnv runs synchronously
// at plug-in registration time; if env is fully configured (Graph or
// SMTP), the resulting concrete Sender is used directly. When env
// only yields a LogSender, the plug-in wraps it in a LazySender that
// re-checks v1:platform:globalVariable + globalSecret rows on the
// first Send call -- so a freshly repaved cluster (`make up-refresh`)
// picks up seeded credentials automatically once `go run ./scripts/secrets
// seed` lands, with no identity-binary restart required. (memql#4405:
// this used to name a `dev-refresh` make target and a `dev-secrets.yaml`
// stash; that target has never existed and the seeder reads a .env file.)
func init() {
	memql.RegisterPlugin("email", func(pctx memql.PluginContext) (memql.IntegrationProvider, error) {
		envSender, err := NewSenderFromEnv("", pctx.Logger)
		if err != nil {
			// Returned rather than swallowed: app.materializePlugins fatals on
			// a factory error, which is the point (memql#4477). An install
			// that must deliver mail and cannot is refused at boot, where the
			// operator is watching, instead of at the moment somebody cannot
			// sign in a week later.
			return nil, err
		}
		lazySender := NewLazySender(envSender, pctx.ResolveSystemVariable, pctx.ResolveSystemSecret, pctx.Logger)
		return NewIntegration(lazySender, pctx.Logger), nil
	})
}
