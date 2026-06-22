package telnyx

import (
	"os"

	"github.com/znasllc-io/memql/integrations/telephony"
)

// init registers the Telnyx carrier under its name so
// telephony.SelectCarrier("telnyx") (the default) resolves it. The factory
// reads credentials from the environment at selection time —
// MEMQL_TELEPHONY_TELNYX_API_KEY (delivered via external-secrets in cluster)
// and the optional MEMQL_TELEPHONY_TELNYX_CONNECTION_ID fronting the SIP edge —
// so registration never fails at process start; a missing key surfaces a clear
// error only when telephony is actually used. MEMQL_TELEPHONY_TELNYX_BASE_URL
// optionally overrides the API root (defaults to the live Telnyx v2 API).
func init() {
	telephony.RegisterCarrier("telnyx", func() (telephony.CarrierProvider, error) {
		return New(Options{
			APIKey:       os.Getenv("MEMQL_TELEPHONY_TELNYX_API_KEY"),
			ConnectionID: os.Getenv("MEMQL_TELEPHONY_TELNYX_CONNECTION_ID"),
			BaseURL:      os.Getenv("MEMQL_TELEPHONY_TELNYX_BASE_URL"),
		})
	})
}
