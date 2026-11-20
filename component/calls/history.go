package calls

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
)

// HistoryRequest is bounded by the service before any graph read. Dates and
// cursor are wire strings so validation has one implementation for every caller.
type HistoryRequest struct {
	Archive                                bool
	Limit, Period                          int
	Cursor, Search, Role, Status, From, To string
}

type HistoryCall struct {
	ID              string `json:"id"`
	Title           string `json:"title"`
	Role            string `json:"role"`
	Status          string `json:"status"`
	StartedAt       string `json:"startedAt,omitempty"`
	EndedAt         string `json:"endedAt,omitempty"`
	DurationSeconds *int64 `json:"durationSeconds"`
	CostCents       *int64 `json:"costCents"`
	CanRename       bool   `json:"canRename"`
	CanArchive      bool   `json:"canArchive"`
	ArchivedAt      string `json:"archivedAt,omitempty"`
	DeleteAt        string `json:"deleteAt,omitempty"`
}

type HistorySeries struct {
	Date            string `json:"date"`
	Calls           *int64 `json:"calls"`
	DurationSeconds *int64 `json:"durationSeconds"`
	CostCents       *int64 `json:"costCents"`
}

type HistoryOverview struct {
	HostedCalls    *int64          `json:"hostedCalls"`
	JoinedCalls    *int64          `json:"joinedCalls"`
	Currency       string          `json:"currency"`
	PeriodDays     int             `json:"periodDays"`
	AvailableCents *int64          `json:"availableCents"`
	ReservedCents  *int64          `json:"reservedCents"`
	SpendCents     *int64          `json:"spendCents"`
	HostedSeconds  *int64          `json:"hostedSeconds"`
	Series         []HistorySeries `json:"series"`
}

type HistoryPage struct {
	AccountID  string          `json:"accountId"`
	Calls      []HistoryCall   `json:"calls"`
	NextCursor string          `json:"nextCursor,omitempty"`
	Collecting bool            `json:"collecting"`
	Overview   HistoryOverview `json:"overview"`
}

type HistoryActionRequest struct {
	Action            string  `json:"action"`
	ID                string  `json:"id"`
	Title             *string `json:"title,omitempty"`
	ConfirmationToken string  `json:"confirmationToken,omitempty"`
	Acknowledged      bool    `json:"acknowledged,omitempty"`
}

type HistoryActionResult struct {
	DeleteAt          string       `json:"deleteAt,omitempty"`
	ConfirmationToken string       `json:"confirmationToken,omitempty"`
	Call              *HistoryCall `json:"call,omitempty"`
}

// HistoryDigest is a length-framed keyed identifier, not authorization. Its
// domain separates history entries, archive confirmations and pagination from
// invitation capabilities. A purged entry's opaque id retains no source ids.
func HistoryDigest(pepper []byte, purpose string, parts ...string) string {
	mac := hmac.New(sha256.New, pepper)
	mac.Write([]byte("memql.calls.history.v1\x00" + purpose + "\x00"))
	for _, part := range parts {
		mac.Write(binary.BigEndian.AppendUint32(nil, uint32(len(part))))
		mac.Write([]byte(part))
	}
	return hex.EncodeToString(mac.Sum(nil))
}

// SealHistoryToken authenticates an opaque bounded payload; callers still must
// validate its scope, expiry and expected row version against fresh state.
func SealHistoryToken(pepper []byte, purpose string, payload []byte) string {
	encoded := base64.RawURLEncoding.EncodeToString(payload)
	return encoded + "." + HistoryDigest(pepper, purpose, encoded)
}

func OpenHistoryToken(pepper []byte, purpose, token string) ([]byte, bool) {
	if len(token) > 4096 {
		return nil, false
	}
	var split int
	for split = len(token) - 1; split >= 0 && token[split] != '.'; split-- {
	}
	if split <= 0 {
		return nil, false
	}
	body, signature := token[:split], token[split+1:]
	if !hmac.Equal([]byte(signature), []byte(HistoryDigest(pepper, purpose, body))) {
		return nil, false
	}
	decoded, err := base64.RawURLEncoding.DecodeString(body)
	return decoded, err == nil
}
