package calls

import (
	"embed"
	"encoding/json"
	"regexp"
	"strings"
	"unicode/utf16"
	"unicode/utf8"
)

const MessageProtocol = "callkey.application.v1"
const ReactionCatalogDigest = "49b21f2071cdd4bd6f501b04273e61d1e1b326d3f41069247ba0a33432192d62"

//go:embed reaction_identifiers.json
var reactionFiles embed.FS
var reactionIdentifiers = func() map[string]bool {
	b, _ := reactionFiles.ReadFile("reaction_identifiers.json")
	var ids []string
	if json.Unmarshal(b, &ids) != nil {
		panic("invalid bundled reaction catalog")
	}
	out := map[string]bool{}
	for _, s := range ids {
		out[s] = true
	}
	return out
}()

type MessageRequest struct {
	RoomID   string  `json:"roomId"`
	Identity string  `json:"identity"`
	Sequence int64   `json:"sequence"`
	Kind     string  `json:"kind"`
	Text     *string `json:"text,omitempty"`
	Reaction *string `json:"reaction,omitempty"`
	Raised   *bool   `json:"raised,omitempty"`
}
type MessageResult struct {
	Status       string `json:"status"`
	EventID      string `json:"eventId,omitempty"`
	Sequence     int64  `json:"sequence"`
	NextSequence int64  `json:"nextSequence"`
	RetryAfterMS int64  `json:"retryAfterMs,omitempty"`
}
type MessageStatus struct {
	Reason                string           `json:"reason,omitempty"`
	ObservedAt            int64            `json:"observedAt"`
	Available             bool             `json:"available"`
	Protocol              string           `json:"protocol"`
	NextSequence          int64            `json:"nextSequence"`
	ReactionCatalogDigest string           `json:"reactionCatalogDigest"`
	Hands                 []string         `json:"hands"`
	HandSequences         map[string]int64 `json:"handSequences"`
}
type MessagePacket struct {
	Protocol        string  `json:"protocol"`
	EventID         string  `json:"eventId"`
	RoomID          string  `json:"roomId"`
	Identity        string  `json:"identity"`
	SeatID          string  `json:"seatId"`
	Name            string  `json:"name"`
	ParticipantKind string  `json:"participantKind"`
	Sequence        int64   `json:"sequence"`
	PublishedAt     int64   `json:"publishedAt"`
	ExpiresAt       int64   `json:"expiresAt"`
	Kind            string  `json:"kind"`
	Text            *string `json:"text,omitempty"`
	Reaction        *string `json:"reaction,omitempty"`
	Raised          *bool   `json:"raised,omitempty"`
}

var messageIdentityPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)

func ValidMessage(r MessageRequest) bool {
	if !messageIdentityPattern.MatchString(r.RoomID) || !messageIdentityPattern.MatchString(r.Identity) || r.Sequence < 1 || r.Sequence > 9007199254740990 {
		return false
	}
	switch r.Kind {
	case "chat":
		if r.Text == nil || r.Reaction != nil || r.Raised != nil || strings.TrimSpace(*r.Text) == "" || len(*r.Text) > 8000 || !utf8.ValidString(*r.Text) {
			return false
		}
		units := 0
		for _, c := range *r.Text {
			if (c < 32 && c != '\n' && c != '\t') || c == 127 {
				return false
			}
			units += utf16.RuneLen(c)
		}
		return units <= 2000
	case "reaction":
		return r.Text == nil && r.Raised == nil && r.Reaction != nil && reactionIdentifiers[*r.Reaction]
	case "hand":
		return r.Text == nil && r.Reaction == nil && r.Raised != nil
	}
	return false
}
