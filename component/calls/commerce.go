package calls

import "time"

// AccountLifecycle contains only the reusable age result and bounded effect
// references. Identity documents, selfies, birth dates and payment credentials
// never belong in this visitor projection.
type AccountLifecycle struct {
	Mode                      string             `json:"mode"`
	Adult                     AdultEvidence      `json:"adult"`
	Checkout                  *CheckoutState     `json:"checkout,omitempty"`
	Verification              *VerificationState `json:"verification,omitempty"`
	VerificationAttempts      int                `json:"verificationAttempts"`
	VerificationReservedCents int64              `json:"verificationReservedCents"`
	VerificationSpentCents    int64              `json:"verificationSpentCents"`
	HostingAllowed            bool               `json:"hostingAllowed"`
	BlockingReason            string             `json:"blockingReason,omitempty"`
	MinimumTopUpCents         int64              `json:"minimumTopUpCents"`
	SavedPaymentMethod        bool               `json:"savedPaymentMethod"`
}
type AdultEvidence struct {
	Status     string     `json:"status"`
	Provider   string     `json:"provider"`
	Method     string     `json:"method,omitempty"`
	VerifiedAt *time.Time `json:"verifiedAt,omitempty"`
}
type CheckoutState struct {
	ID          string `json:"id"`
	Status      string `json:"status"`
	URL         string `json:"url,omitempty"`
	AmountCents int64  `json:"amountCents"`
}
type VerificationState struct {
	ID     string `json:"id"`
	Status string `json:"status"`
	URL    string `json:"url,omitempty"`
}
