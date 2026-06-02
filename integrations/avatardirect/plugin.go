package avatardirect

import (
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/polyphon"
)

// init self-registers the direct Anam avatar integration as a core plug-in so
// the node copresent calls (the bff) carries it. The LiveKit transport creds
// are read from the POLYPHON_LIVEKIT_* env at factory time; the Anam key is
// resolved lazily at request time from v1:platform:globalSecret (env fallback),
// so the factory succeeds on a fresh install -- the "key not configured"
// surface error fires only when a caller invokes startSession for an
// anam-vendor agent.
func init() {
	memql.RegisterPlugin("avatardirect", func(pctx memql.PluginContext) (memql.IntegrationProvider, error) {
		return New(pctx.Engine, pctx.ResolveSystemSecret, polyphon.ConfigFromEnv(), pctx.Logger), nil
	})
}
