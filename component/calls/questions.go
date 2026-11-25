package calls

import (
	"strings"
	"time"
	"unicode/utf8"
)

const QuestionPublication = "host_then_room_v1"

type QuestionRequest struct {
	RoomID         string `json:"roomId"`
	Identity       string `json:"identity"`
	RunID          string `json:"runId"`
	Cursor         string `json:"cursor,omitempty"`
	QueueRevision  string `json:"queueRevision,omitempty"`
	Text           string `json:"text,omitempty"`
	Publication    string `json:"publication,omitempty"`
	IdempotencyKey string `json:"idempotencyKey,omitempty"`
	ID             string `json:"id,omitempty"`
	Revision       int64  `json:"revision,omitempty"`
	ExpectedEpoch  int64  `json:"expectedEpoch,omitempty"`
}
type QuestionAuthor struct {
	Name     string `json:"name"`
	Kind     string `json:"kind"`
	Identity string `json:"identity"`
}
type QuestionItem struct {
	CanSelect   bool           `json:"canSelect"`
	CanDismiss  bool           `json:"canDismiss"`
	CanResolve  bool           `json:"canResolve"`
	CanWithdraw bool           `json:"canWithdraw"`
	RunID       string         `json:"runId"`
	ID          string         `json:"id"`
	Revision    int64          `json:"revision"`
	Status      string         `json:"status"`
	Text        string         `json:"text"`
	Author      QuestionAuthor `json:"author"`
	Own         bool           `json:"own"`
	SubmittedAt time.Time      `json:"submittedAt"`
	ExpiresAt   time.Time      `json:"expiresAt"`
}
type QuestionReceipt struct {
	ID       string `json:"id"`
	Revision int64  `json:"revision"`
	Status   string `json:"status"`
}
type Questions struct {
	RunID                  string         `json:"runId"`
	Available              bool           `json:"available"`
	CanSubmit              bool           `json:"canSubmit"`
	CanModerate            bool           `json:"canModerate"`
	CanSeeAll              bool           `json:"canSeeAll"`
	Reason                 string         `json:"reason,omitempty"`
	Publication            string         `json:"publication"`
	MaxTextLength          int            `json:"maxTextLength"`
	MaxOpenPerAdmission    int            `json:"maxOpenPerAdmission"`
	MaxQuestionsPerRoom    int            `json:"maxQuestionsPerRoom"`
	MaxQuestionsPerCallkey int            `json:"maxQuestionsPerCallkey"`
	OwnOpenCount           int            `json:"ownOpenCount"`
	QueueRevision          string         `json:"queueRevision"`
	ExpiresAt              time.Time      `json:"expiresAt"`
	Items                  []QuestionItem `json:"items"`
	Selected               *QuestionItem  `json:"selected,omitempty"`
	NextCursor             string         `json:"nextCursor,omitempty"`
}

func QuestionAction(action string) bool {
	switch action {
	case "questions", "question_submit", "question_withdraw", "question_select", "question_dismiss", "question_resolve":
		return true
	}
	return false
}
func ValidQuestionText(text string) bool {
	if !utf8.ValidString(text) || strings.TrimSpace(text) == "" || utf8.RuneCountInString(text) > 500 {
		return false
	}
	for _, c := range text {
		if c < 32 && c != '\n' && c != '\t' || c >= 127 && c <= 159 {
			return false
		}
	}
	return true
}
func ValidQuestionRequest(r WorkflowRequest) bool {
	q := r.Question
	if !QuestionAction(r.Action) || q == nil || r.BookingID != "" || r.Definition != nil || r.SkillID != "" || r.Version != 0 || r.RunID != "" || r.ExpectedRevision != 0 || r.ExpectedEpoch != 0 || r.IdempotencyKey != "" || r.NodeID != "" || r.Seconds != 0 || len(r.Bindings) != 0 || r.Acknowledged {
		return false
	}
	if !ValidWorkflowID(q.RoomID) || !ValidWorkflowID(q.RunID) || len(q.Identity) == 0 || len(q.Identity) > 128 || len(q.Cursor) > 4096 || len(q.QueueRevision) > 64 {
		return false
	}
	if r.Action == "questions" {
		return q.Text == "" && q.Publication == "" && q.IdempotencyKey == "" && q.ID == "" && q.Revision == 0 && q.ExpectedEpoch == 0 && (q.Cursor == "" || len(q.QueueRevision) == 64)
	}
	if q.Cursor != "" || q.QueueRevision != "" || !ValidWorkflowID(q.IdempotencyKey) {
		return false
	}
	if r.Action == "question_submit" {
		return ValidQuestionText(q.Text) && q.Publication == QuestionPublication && q.ID == "" && q.Revision == 0 && q.ExpectedEpoch == 0
	}
	return q.Text == "" && q.Publication == "" && ValidWorkflowID(q.ID) && q.Revision >= 1 && q.Revision <= 3 && (r.Action == "question_withdraw" && q.ExpectedEpoch == 0 || r.Action != "question_withdraw" && q.ExpectedEpoch >= 1)
}
