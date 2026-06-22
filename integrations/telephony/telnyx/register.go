package telnyx

import (
	"os"

	"github.com/znasllc-io/memql/integrations/telephony"
)

// init registers the Telnyx carrier under its name so
// telephony.SelectCarrier("telnyx") (the default) resolves it. The factory
// reads credentials from the environment at selection time — TELNYX_API_KEY
// (delivered via external-secrets in cluster) and the optional
// TELNYX_CONNECTION_ID fronting the SIP edge — so registration never fails at
// process start; a missing key surfaces a clear error only when telephony is
// actually used.
func init() {
	telephony.RegisterCarrier("telnyx", func() (telephony.CarrierProvider, error) {
		return New(Options{
			APIKey:       os.Getenv("TELNYX_API_KEY"),
			ConnectionID: os.Getenv("TELNYX_CONNECTION_ID"),
		})
	})
}
