package campaigns

// ConsentKind is one append-only event on the per-subscriber stream
// (memql#4141). Current status is derived from the latest kind -- there
// is no mutable status column.
const (
	ConsentGrant     = "grant"
	ConsentWithdraw  = "withdraw"
	ConsentBounce    = "bounce"
	ConsentComplaint = "complaint"
	ConsentSuppress  = "suppress"
)

// ConsentEvent is one row of the export stream. Date is OccurredAt,
// source is Source, status is derived via DeriveConsentStatus.
type ConsentEvent struct {
	Kind       string
	Source     string
	OccurredAt string
	Reason     string
}

// DeriveConsentStatus returns the current status, date, and source from
// the latest event (events[0] is newest). An empty stream has no status.
func DeriveConsentStatus(newestFirst []ConsentEvent) (status, date, source string, ok bool) {
	if len(newestFirst) == 0 {
		return "", "", "", false
	}
	e := newestFirst[0]
	return e.Kind, e.OccurredAt, e.Source, true
}

// SuppressReasonRequired reports whether a suppress event is missing its
// required reason.
func SuppressReasonRequired(kind, reason string) bool {
	return kind == ConsentSuppress && reason == ""
}
