package calls

import (
	"encoding/hex"
	"encoding/json"
	"io"
	"slices"
	"strings"
	"time"
	"unicode"
)

// QuestionStoredState is native metadata, never a participant-facing projection.
// It excludes question text so replay receipts cannot retain cleared content.
type QuestionStoredState struct {
	Author                                                   QuestionAuthor
	SeatID, SessionID, CredentialID, GrantID, ApprovalDigest string
	GuestAdmissionID, ReviewerKeyID                          string
	JoinedAt                                                 time.Time
	SkillID                                                  string
	SkillVersion                                             int
	Operations                                               []QuestionStoredOperation
}
type QuestionStoredOperation struct {
	Key, Digest, Actor, BudgetKey, Status string
	Revision                              int64
	At                                    time.Time
}

func QuestionDigest(v string) bool {
	b, e := hex.DecodeString(v)
	return e == nil && len(b) == 32 && hex.EncodeToString(b) == v
}
func questionStoredID(v string) bool {
	return len(v) > 0 && len(v) <= 128 && strings.IndexFunc(v, unicode.IsControl) < 0 && !strings.ContainsAny(v, " /\\")
}
func ParseQuestionStoredState(raw string, revision int64, status string, submitted, expires time.Time) (QuestionStoredState, bool) {
	var s QuestionStoredState
	if len(raw) > 8192 {
		return s, false
	}
	decoder := json.NewDecoder(strings.NewReader(raw))
	decoder.DisallowUnknownFields()
	err := decoder.Decode(&s)
	if len(raw) > 8192 || err != nil || decoder.Decode(new(any)) != io.EOF || revision < 1 || revision > 3 || len(s.Operations) != int(revision) || !questionStoredID(s.SeatID) || !questionStoredID(s.SessionID) || !questionStoredID(s.Author.Identity) || !slices.Contains([]string{string(KindPerson), string(KindAgent), string(KindAvatar)}, s.Author.Kind) || strings.TrimSpace(s.Author.Name) == "" || len(s.Author.Name) > 256 || strings.IndexFunc(s.Author.Name, unicode.IsControl) >= 0 || s.JoinedAt.IsZero() || s.JoinedAt.After(submitted) || !ValidWorkflowID(s.SkillID) || s.SkillVersion < 1 {
		return s, false
	}
	if s.Author.Kind == string(KindPerson) && s.GrantID != "" || s.Author.Kind != string(KindPerson) && s.GrantID == "" {
		return s, false
	}
	if s.GrantID != "" && (!questionStoredID(s.GrantID) || !QuestionDigest(s.ApprovalDigest)) || s.GrantID == "" && s.ApprovalDigest != "" || s.CredentialID != "" && !questionStoredID(s.CredentialID) || s.GuestAdmissionID != "" && (!QuestionDigest(s.GuestAdmissionID) || !questionStoredID(s.ReviewerKeyID) || s.GrantID == "") || s.GuestAdmissionID == "" && s.ReviewerKeyID != "" {
		return s, false
	}
	prior := submitted
	for i, op := range s.Operations {
		if !ValidWorkflowID(op.Key) || !QuestionDigest(op.Digest) || !QuestionDigest(op.Actor) || !QuestionDigest(op.BudgetKey) || op.Revision != int64(i+1) || op.At.Before(prior) || !op.At.Before(expires) {
			return s, false
		}
		prior = op.At
		if i == 0 && op.Status != "pending" || i == 1 && !slices.Contains([]string{"selected", "withdrawn", "dismissed"}, op.Status) || i == 2 && (s.Operations[1].Status != "selected" || !slices.Contains([]string{"withdrawn", "dismissed", "resolved"}, op.Status)) {
			return s, false
		}
	}
	return s, s.Operations[len(s.Operations)-1].Status == status
}
