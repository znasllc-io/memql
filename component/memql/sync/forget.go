package sync

import "context"

// forget.go -- a delivery that will be tried again must be fetched again.
//
// The runtime tries a failed inbound delivery again (component/datasync's
// retry pass), and a connector may keep a short-circuit of deliveries it has
// already handled: Shopify's remembers a webhook id for fifteen minutes so a
// redelivery costs no Admin round trip. A short-circuit that remembered a
// delivery whose writes never landed would answer the retry "already seen",
// and the change would reach the mirror only at the next sweep.
//
// InboundForgetter is the OPTIONAL half of the contract that closes this. The
// dispatcher calls it when a delivery failed for a reason worth another try --
// the connector's own Apply failed, or Apply succeeded and the runtime could
// not write what it returned -- so the next try is worked as the first was. A
// connector with no short-circuit does not implement it.
type InboundForgetter interface {
	// ForgetInbound drops whatever the connector remembers of this delivery.
	// It must be safe to call for a delivery the connector never saw.
	ForgetInbound(ctx context.Context, req InboundRequest)
}
