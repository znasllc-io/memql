package calls

import (
	"context"
	"time"
)

// MediaSignalService is the fixed media signalling boundary. It accepts an
// authenticated native media pass, never an upstream URL or arbitrary request.
// Media packets continue to travel directly between the client and LiveKit.
type MediaSignalService interface {
	BeginMediaSignal(context.Context, string) (*MediaSignalAdmission, error)
	// Check validates native authority without registering/superseding a socket
	// or touching provider state. Success is not a provider readiness report.
	CheckMediaSignal(context.Context, string) error
}

// MediaSignalAdmission belongs to one transport connection, not its caller.
// Callbacks are in-process capabilities; none may be exposed as public RPCs.
// The transport must honour callback context deadlines and serialize writes.
type MediaSignalAdmission struct {
	UpstreamURL, UpstreamToken, UpstreamGuard, Room, Identity, IntentID string
	ExpiresAt                                                           time.Time
	// Forward checks current authority under the native revocation gates, then
	// performs one bounded upstream operation. Dial/HTTP and every upstream WS
	// write MUST use it. An error never authorizes forwarding.
	Forward func(context.Context, func(context.Context) error) error
	// Confirm follows a definitive upstream success, before releasing its first
	// response to the client. It settles the pending handshake and rechecks all
	// current authority. Calling it before the upstream responds is unsafe.
	Confirm func(context.Context) error
	// Close must be called on every exit. Definitive means no request was sent,
	// or the upstream actually responded; timeout/EOF after send is ambiguous.
	// Ambiguous handshakes survive process restart as pending cleanup.
	Close func(context.Context, bool) error
}

// MediaCleanupStatus describes provider cleanup, separately from committing a
// native revocation. No field claims that closing signalling cuts off SRTP.
type MediaCleanupStatus struct {
	State          string `json:"state"`
	PendingSignals int    `json:"pending_signals"`
	Reason         string `json:"reason,omitempty"`
}
