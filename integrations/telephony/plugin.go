package telephony

import (
	"os"
	"strconv"

	"github.com/znasllc-io/memql/component/memql"
)

// Built-in carriers (e.g. telnyx) self-register via their own init() and are
// blank-imported at the app level (app/plugins_core.go) -- telephony cannot
// import them itself (they import telephony, which would cycle).

// init self-registers the telephony integration as a plug-in. Credentials are
// environment-delivered (external-secrets in cluster); a missing carrier key
// surfaces only when a capability is used, and absent LiveKit creds disable the
// SIP capabilities cleanly -- so the plug-in loads everywhere.
func init() {
	memql.RegisterPlugin("telephony", func(pctx memql.PluginContext) (memql.IntegrationProvider, error) {
		integ, err := New(Config{
			CarrierName:          os.Getenv("MEMQL_TELEPHONY_CARRIER"),
			LiveKitURL:           firstEnv("LIVEKIT_URL", "MEMQL_POLYPHON_LIVEKIT_URL"),
			LiveKitAPIKey:        os.Getenv("MEMQL_POLYPHON_LIVEKIT_API_KEY"),
			LiveKitAPISecret:     os.Getenv("MEMQL_POLYPHON_LIVEKIT_API_SECRET"),
			SIPEdgeURI:           os.Getenv("MEMQL_TELEPHONY_SIP_EDGE_URI"),
			OutboundSIPAddress:   os.Getenv("MEMQL_TELEPHONY_OUTBOUND_SIP_ADDRESS"),
			OutboundAuthUsername: os.Getenv("MEMQL_TELEPHONY_OUTBOUND_AUTH_USERNAME"),
			OutboundAuthPassword: os.Getenv("MEMQL_TELEPHONY_OUTBOUND_AUTH_PASSWORD"),
			ModelCostPerMinute:   parseFloatEnv("MEMQL_TELEPHONY_MODEL_COST_PER_MINUTE"),
		}, pctx.Logger)
		if err != nil {
			return nil, err
		}
		integ.SetEngine(pctx.Engine)
		return integ, nil
	})
}

func firstEnv(keys ...string) string {
	for _, k := range keys {
		if v := os.Getenv(k); v != "" {
			return v
		}
	}
	return ""
}

func parseFloatEnv(key string) float64 {
	v := os.Getenv(key)
	if v == "" {
		return 0
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil {
		return 0
	}
	return f
}
