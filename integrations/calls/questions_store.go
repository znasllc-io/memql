package calls

import (
	"context"
	"encoding/json"
	"slices"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	callrules "github.com/znasllc-io/memql/component/calls"
	langparser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/memql"
)

const questionConcept = "v1:calls:workflowQuestion"

type questionOperation = callrules.QuestionStoredOperation
type questionState = callrules.QuestionStoredState
type questionRow struct {
	ID, SiteID, RoomID, RunID, AuthorKeyID, AuthorAdmission, Status, Body string
	Version, SubmittedAt, RoomDeadline, ExpiresAt                         time.Time
	Revision                                                              int64
	State                                                                 questionState
}

func questionActive(q *questionRow) bool { return q.Status == "pending" || q.Status == "selected" }
func questionFromRow(row map[string]any, body bool) (*questionRow, error) {
	q := &questionRow{ID: memql.BareShortId(rowString(row, "id")), SiteID: memql.BareShortId(rowString(row, "siteId")), RoomID: memql.BareShortId(rowString(row, "roomId")), RunID: rowString(row, "runId"), AuthorKeyID: memql.BareShortId(rowString(row, "authorKeyId")), AuthorAdmission: rowString(row, "authorAdmission"), Version: rowTime(row, "createdAt"), SubmittedAt: rowTime(row, "submittedAt"), RoomDeadline: rowTime(row, "roomDeadline"), ExpiresAt: rowTime(row, "expiresAt"), Revision: int64(rowInt(row, "revision")), Status: rowString(row, "status"), Body: rowString(row, "body")}

	data := rowString(row, "stateData")
	if !callrules.ValidWorkflowID(q.ID) || !callrules.ValidWorkflowID(q.RunID) || q.SiteID == "" || q.RoomID == "" || q.AuthorKeyID == "" || !callrules.QuestionDigest(q.AuthorAdmission) || q.Version.IsZero() || q.SubmittedAt.IsZero() || q.RoomDeadline.Before(q.SubmittedAt) || !q.ExpiresAt.After(q.SubmittedAt) || q.ExpiresAt.After(q.SubmittedAt.Add(25*time.Hour)) || q.ExpiresAt.After(q.RoomDeadline.Add(time.Hour)) {
		return nil, accountRefusal("questions_unavailable")
	}
	var valid bool
	q.State, valid = callrules.ParseQuestionStoredState(data, q.Revision, q.Status, q.SubmittedAt, q.ExpiresAt)
	if !valid || body && (questionActive(q) && !callrules.ValidQuestionText(q.Body) || !questionActive(q) && q.Body != "") {
		return nil, accountRefusal("questions_unavailable")
	}
	return q, nil
}
func (s *Store) questionQuery(ctx context.Context, owner, name string, args map[string]any, cursor string) ([]map[string]any, string, error) {
	call, e := langparser.RenderCall(name, args)
	if e != nil {
		return nil, "", accountRefusal("questions_unavailable")
	}
	owned, ok := asOwner(auth.ContextWithInternalOrigin(memql.ContextWithFreshRead(ctx)), owner)
	if !ok {
		return nil, "", errNoOwner
	}
	result, e := s.engine.Execute(memql.ContextWithCursor(memql.ContextWithRootsOnlyRead(owned), cursor), "query "+call)
	if e != nil {
		return nil, "", accountRefusal("questions_unavailable")
	}
	out, rows, e := accountingInventoryPage(result)
	if e != nil {
		return nil, "", accountRefusal("questions_unavailable")
	}
	next, e := memql.ResultPageCursor(out)
	if e != nil {
		return nil, "", accountRefusal("questions_unavailable")
	}
	return rows, next, nil
}
func (s *Store) questionInventory(ctx context.Context, owner, site, room string) ([]*questionRow, error) {
	rows := []*questionRow{}
	cursor := ""
	seen := map[string]bool{}
	cursors := map[string]bool{}
	for range 101 {
		page, next, e := s.questionQuery(ctx, owner, "callsQuestionInventory", map[string]any{"siteId": site, "roomId": room}, cursor)
		if e != nil {
			return nil, e
		}
		for _, raw := range page {
			q, e := questionFromRow(raw, false)
			if e != nil {
				return nil, e
			}
			if q.SiteID != site || q.RoomID != room || seen[q.ID] || len(rows) >= 100 {
				return nil, accountRefusal("questions_unavailable")
			}
			seen[q.ID] = true
			rows = append(rows, q)
		}
		if next == "" {
			return rows, nil
		}
		if len(page) == 0 || cursors[next] {
			return nil, accountRefusal("questions_unavailable")
		}
		cursors[next] = true
		cursor = next
	}
	return nil, accountRefusal("questions_unavailable")
}
func (s *Store) question(ctx context.Context, owner, id string) (*questionRow, error) {
	rows, next, e := s.questionQuery(ctx, owner, "callsQuestionById", map[string]any{"id": id}, "")
	if e != nil {
		return nil, e
	}
	// A full native page can carry a continuation even for an exact-ID query.
	// Consume its terminal empty page instead of treating a cursor as another row.
	if next != "" {
		extra, tail, e := s.questionQuery(ctx, owner, "callsQuestionById", map[string]any{"id": id}, next)
		if e != nil {
			return nil, e
		}
		if len(extra) != 0 || tail != "" {
			return nil, accountRefusal("questions_unavailable")
		}
	}
	if len(rows) > 1 {
		return nil, accountRefusal("questions_unavailable")
	}
	if len(rows) == 0 {
		return nil, nil
	}
	q, e := questionFromRow(rows[0], true)
	if e != nil {
		return nil, e
	}
	if q.ID != id {
		return nil, accountRefusal("questions_unavailable")
	}
	return q, nil
}
func (s *Store) questionPage(ctx context.Context, owner string, ids []string, cursor string) ([]*questionRow, string, error) {
	if len(ids) == 0 && cursor == "" {
		return []*questionRow{}, "", nil
	}
	page, next, e := s.questionQuery(ctx, owner, "callsQuestionPage", map[string]any{"ids": ids}, cursor)
	if e != nil {
		return nil, "", e
	}
	if len(page) > 20 {
		return nil, "", accountRefusal("questions_unavailable")
	}
	out := []*questionRow{}
	seen := map[string]bool{}
	for _, raw := range page {
		q, e := questionFromRow(raw, true)
		if e != nil {
			return nil, "", e
		}
		if !slices.Contains(ids, q.ID) || q.Status != "pending" || seen[q.ID] {
			return nil, "", accountRefusal("questions_unavailable")
		}
		seen[q.ID] = true
		out = append(out, q)
	}
	return out, next, nil
}
func (s *Store) saveQuestion(ctx context.Context, owner string, q *questionRow, now time.Time) error {
	data, e := json.Marshal(q.State)
	if e != nil || len(data) > 8192 || q.Revision < 1 || q.Revision > 3 || !now.Before(q.ExpiresAt) {
		return accountRefusal("questions_unavailable")
	}
	if _, valid := callrules.ParseQuestionStoredState(string(data), q.Revision, q.Status, q.SubmittedAt, q.ExpiresAt); !valid || q.RoomDeadline.Before(q.SubmittedAt) || !q.ExpiresAt.After(q.SubmittedAt) || q.ExpiresAt.After(q.SubmittedAt.Add(25*time.Hour)) || q.ExpiresAt.After(q.RoomDeadline.Add(time.Hour)) || questionActive(q) && !callrules.ValidQuestionText(q.Body) || !questionActive(q) && q.Body != "" {
		return accountRefusal("questions_unavailable")
	}
	version := memql.VersionTimeAfter(q.Version, now)
	call, e := langparser.RenderCall("saveCallsQuestion", map[string]any{"questionId": q.ID, "versionTime": versionStamp(version), "siteId": q.SiteID, "roomId": q.RoomID, "runId": q.RunID, "authorKeyId": q.AuthorKeyID, "authorAdmission": q.AuthorAdmission, "submittedAt": versionStamp(q.SubmittedAt), "roomDeadline": versionStamp(q.RoomDeadline), "expiresAt": versionStamp(q.ExpiresAt), "revision": q.Revision, "status": q.Status, "body": q.Body, "stateData": string(data)})
	fence := absent(questionConcept, q.ID)
	if !q.Version.IsZero() {
		fence = at(questionConcept, q.ID, q.Version)
	}
	e = s.writeAs(auth.ContextWithInternalOrigin(ctx), owner, fence, "temporary moderated question", call, e)
	if e == nil {
		q.Version = version
	}
	return e
}
