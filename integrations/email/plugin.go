package email

import "github.com/visionarys-io/memql/component/memql"

// init self-registers the email integration as a plug-in.
//
// Sender resolution is two-stage. NewSenderFromEnv runs synchronously
// at plug-in registration time; if env is fully configured (Graph or
// SMTP), the resulting concrete Sender is used directly. When env
// only yields a LogSender, the plug-in wraps it in a LazySender that
// re-checks v1:platform:globalVariable + globalSecret rows on the
// first Send call -- so a fresh `make dev-refresh` cluster picks up
// dev-secrets.yaml-seeded credentials automatically once the seed
// step lands, with no identity-binary restart required.
func init() {
	memql.RegisterPlugin("email", func(pctx memql.PluginContext) (memql.IntegrationProvider, error) {
		envSender := NewSenderFromEnv("", pctx.Logger)
		lazySender := NewLazySender(envSender, pctx.ResolveSystemVariable, pctx.ResolveSystemSecret, pctx.Logger)
		return NewIntegration(lazySender, pctx.Logger), nil
	})
}
