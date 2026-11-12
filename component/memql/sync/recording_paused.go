package sync

import (
	"errors"
	"fmt"
)

// recording_paused.go -- a delivery for a tenant whose recording is paused.
//
// A connector that serves many tenants lets a person pause one of them:
// Shopify's does, one store at a time. While a tenant is paused the origin
// goes on delivering, and what it delivers is not noise. A change can be read
// again later -- the sweep after a resume finds whatever changed -- but a
// deletion cannot: a domain swept by what changed has no way to see a record
// that is gone, so a delete notice dropped during a pause is a record the
// mirror keeps for good.
//
// So a pause HOLDS, it does not lose. The connector answers this error for a
// paused tenant's delivery instead of "nothing to write", the dispatcher keeps
// the delivery as it is -- failed, with no next try, and the try not counted
// -- and the connector puts its tenant's held deliveries back in line once the
// tenant records again. It asks for them by state, not on the event of a
// resume: whatever resumed the tenant, a delivery held for one that records
// does not stay held. Only a pause a person chose is answered this way: a
// tenant the origin has cut off is not waiting for anybody.

// ErrRecordingPaused is a connector's answer for a delivery of a tenant whose
// recording a person paused. It is not a failure of the delivery and it is not
// tried again by itself. Its text is stored on the delivery exactly as it is,
// and is how a held delivery is found again, so it names no connector and no
// tenant: it is one sentence for whoever reads the delivery.
var ErrRecordingPaused = errors.New("connector: recording is paused, so this delivery is held; it goes back in line when recording resumes")

// RecordingPaused is ErrRecordingPaused for one tenant of a connector, said
// for a log. The runtime stores the sentinel's own text, never this one.
func RecordingPaused(connector, tenant string) error {
	return fmt.Errorf("%w (%s %s)", ErrRecordingPaused, connector, tenant)
}

// IsRecordingPaused reports whether a failure is ErrRecordingPaused.
func IsRecordingPaused(err error) bool { return errors.Is(err, ErrRecordingPaused) }
