package campaigns

import (
	"context"
	"encoding/json"
	"time"
)

// composition.go -- the state of an AI composition a template carries (the
// optional composition object of concept v1:campaigns:template): which goal is
// writing it, what was asked, how it ended, and, in plain sentences, each file
// that was left out and why.
//
// The object is {goalId, state, startedAt, finishedAt, error, request:
// {layoutId, name, description, fileIds, referenceFileId}, baseRevision, notes}.
// state is writing, done or failed; the times are RFC3339 and empty when not
// yet known. notes is never null.

type compositionRequest struct {
	LayoutID, Name, Description string
	FileIDs                     []string
	ReferenceFileID             string
}

type composition struct {
	GoalID, State, Error                string
	StartedAt, FinishedAt, BaseRevision time.Time
	Request                             compositionRequest
	Notes                               []string
}

// compositionWire is the composition as the row holds it.
type compositionWire struct {
	GoalID       string      `json:"goalId"`
	State        string      `json:"state"`
	StartedAt    string      `json:"startedAt"`
	FinishedAt   string      `json:"finishedAt"`
	Error        string      `json:"error"`
	Request      requestWire `json:"request"`
	BaseRevision string      `json:"baseRevision"`
	Notes        []string    `json:"notes"`
}

type requestWire struct {
	LayoutID        string   `json:"layoutId"`
	Name            string   `json:"name"`
	Description     string   `json:"description"`
	FileIDs         []string `json:"fileIds"`
	ReferenceFileID string   `json:"referenceFileId"`
}

func compositionTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339Nano)
}

func orEmpty(list []string) []string {
	if list == nil {
		return []string{}
	}
	return list
}

// object is the composition as the JSON object the row stores.
func (c composition) object() (map[string]any, error) {
	wire := compositionWire{
		GoalID: c.GoalID, State: c.State, Error: c.Error,
		StartedAt: compositionTime(c.StartedAt), FinishedAt: compositionTime(c.FinishedAt), BaseRevision: compositionTime(c.BaseRevision),
		Request: requestWire{
			LayoutID: c.Request.LayoutID, Name: c.Request.Name, Description: c.Request.Description,
			FileIDs: orEmpty(c.Request.FileIDs), ReferenceFileID: c.Request.ReferenceFileID,
		},
		Notes: orEmpty(c.Notes),
	}
	raw, err := json.Marshal(wire)
	if err != nil {
		return nil, err
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// compositionFromRow reads the composition of a stored template, or reports
// false when the template has none (or one that does not read).
func compositionFromRow(row map[string]any) (composition, bool) {
	stored, ok := row["composition"].(map[string]any)
	if !ok || len(stored) == 0 {
		return composition{}, false
	}
	raw, err := plainJSON(stored)
	if err != nil {
		return composition{}, false
	}
	var wire compositionWire
	if err := json.Unmarshal(raw, &wire); err != nil {
		return composition{}, false
	}
	return composition{
		GoalID: wire.GoalID, State: wire.State, Error: wire.Error,
		StartedAt: templateTime(wire.StartedAt), FinishedAt: templateTime(wire.FinishedAt), BaseRevision: templateTime(wire.BaseRevision),
		Request: compositionRequest{
			LayoutID: wire.Request.LayoutID, Name: wire.Request.Name, Description: wire.Request.Description,
			FileIDs: wire.Request.FileIDs, ReferenceFileID: wire.Request.ReferenceFileID,
		},
		Notes: wire.Notes,
	}, true
}

// writeComposition replaces a template's composition and nothing else: a new
// row version, under the template's lock, that writes the stored content back
// as it is (the status included). It answers the new revision.
//
// It authorizes for itself: the stored template must belong to accountID, and
// the caller must be able to write templates in it (mayWriteTemplates).
func (w *Worker) writeComposition(ctx context.Context, accountID, templateID string, c composition) (time.Time, error) {
	object, err := c.object()
	if err != nil {
		return time.Time{}, err
	}
	if w.templateGate == nil {
		return time.Time{}, errTemplateGateUnavailable
	}
	release, err := w.lockTemplate(ctx, templateID)
	if err != nil {
		return time.Time{}, err
	}
	defer release()
	rows, err := w.store.rows(ctx, call("query", "templateById", arg{"templateId", templateID}))
	if err != nil {
		return time.Time{}, err
	}
	if len(rows) == 0 {
		return time.Time{}, errTemplateNotFound
	}
	if !sameOrganization(accountID, str(rows[0], "accountId")) {
		return time.Time{}, errTemplateOrgMoved
	}
	if !w.mayWriteTemplates(ctx, accountID) {
		return time.Time{}, errTemplateNeedsWrite
	}
	prior := templateTime(rows[0]["createdAt"])
	if prior.IsZero() {
		return time.Time{}, errTemplateNoRevision
	}
	content := storedTemplate(rows[0])
	content.composition = object
	revision := w.templateRevision(prior)
	values := content.values(templateID, revision)
	values["status"] = str(rows[0], "status")
	if err := w.writeTemplate(ctx, "updateTemplate", values); err != nil {
		return time.Time{}, err
	}
	return revision, nil
}
