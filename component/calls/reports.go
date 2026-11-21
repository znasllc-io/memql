package calls

import "time"

// ReportRequest carries metadata only. Each action accepts its exact subset;
// attachment URLs, media bytes and identity claims are never accepted.
type ReportRequest struct {
	RoomID         string `json:"roomId,omitempty"`
	Identity       string `json:"identity,omitempty"`
	TargetIdentity string `json:"targetIdentity,omitempty"`
	Category       string `json:"category,omitempty"`
	Description    string `json:"description,omitempty"`
	IdempotencyKey string `json:"idempotencyKey,omitempty"`
	ID             string `json:"id,omitempty"`
	Revision       int    `json:"revision,omitempty"`
	Cursor         string `json:"cursor,omitempty"`
}
type ReportCapabilities struct {
	Available    bool `json:"available"`
	MetadataDays int  `json:"metadataDays"`
}
type ReportReceipt struct {
	ID        string    `json:"id"`
	Status    string    `json:"status"`
	Category  string    `json:"category"`
	CreatedAt time.Time `json:"createdAt"`
	ExpiresAt time.Time `json:"expiresAt"`
	Revision  int       `json:"revision"`
	Outcome   string    `json:"outcome,omitempty"`
	CanAppeal bool      `json:"canAppeal"`
}
type ReportResult struct {
	Receipt    *ReportReceipt  `json:"receipt,omitempty"`
	Items      []ReportReceipt `json:"items,omitempty"`
	NextCursor string          `json:"nextCursor,omitempty"`
}
