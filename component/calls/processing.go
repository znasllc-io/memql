package calls

// Processing consent is scoped to one actual admission, not merely a cookie or
// account. Re-entering a room changes Identity and resets every opt-in.
type ProcessingConsentRequest struct {
	RoomID        string `json:"roomId"`
	Identity      string `json:"identity"`
	Revision      string `json:"revision"`
	Safety        bool   `json:"safety"`
	Transcription bool   `json:"transcription"`
	Insights      bool   `json:"insights"`
}
type ProcessingConsent struct {
	Revision      string `json:"revision"`
	Safety        bool   `json:"safety"`
	Transcription bool   `json:"transcription"`
	Insights      bool   `json:"insights"`
}
type ProcessingSpeaker struct {
	Seat            string `json:"seat"`
	Identity        string `json:"identity"`
	ConsentRevision string `json:"consentRevision"`
	Transcription   bool   `json:"transcription"`
	Insights        bool   `json:"insights"`
}
type ProcessingCapabilities struct {
	Safety        bool `json:"safety"`
	Transcription bool `json:"transcription"`
	Insights      bool `json:"insights"`
}
type ProcessingStatus struct {
	Reason       string                 `json:"reason,omitempty"`
	Capabilities ProcessingCapabilities `json:"capabilities"`
	Available    bool                   `json:"available"`
	Eligible     bool                   `json:"eligible"`
	Active       bool                   `json:"active"`
	Epoch        int64                  `json:"epoch"`
	Consent      ProcessingConsent      `json:"consent"`
	Speakers     []ProcessingSpeaker    `json:"speakers"`
}
