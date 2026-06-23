// Package library owns the server-side edit path for Library documents:
// the append-only version history (v1:library:documentVersion) and the
// user / assistant / restore flows that append to it (memql#1228-1231).
//
// Why a Go integration instead of pure DSL: appending a version is a
// read-then-compute-then-write dance the MemQL DSL cannot express on
// its own --
//
//   - the next versionNumber is (current latest versionNumber) + 1, and
//     MemQL has no arithmetic;
//   - the version row needs a freshly minted id, and MemQL has no
//     id-mint primitive in a mutation body;
//   - optimistic concurrency needs to compare the caller's expected
//     version against the current latest, which is a read + branch.
//
// So the handlers here read the current history (documentVersions-
// ForOwner), compute the next version + parent pointer in Go, and call
// the low-level appendDocumentVersion to append the immutable
// snapshot. They also re-insert the backing generatedOutput (same id,
// new version) so the artifact index + Library viewer -- which resolve
// content through the backing row -- reflect the latest edit, and bump
// the artifact index's updatedAt watermark.
//
// Capabilities:
//
//	editDocument            -- append a new version with new content.
//	                           authorKind=user (memql#1229) or
//	                           authorKind=assistant (memql#1231),
//	                           selected by the caller's authorKind arg.
//	restoreDocumentVersion  -- append a NEW latest version equal to a
//	                           chosen earlier version (memql#1230).
//	                           Restore is forward-only: it never
//	                           destroys history.
//
// Authorization model: owner-threaded. The owning user is read from the
// backing document ROW (not the actor) -- consistent with the existing
// Library mutations, which take ownerUserId from the row's owner because
// promotion / edit runs server-side on the owner's behalf. The append
// runs under a synthetic user actor (withUserActor) so the engine's
// actor-required gate is satisfied and the appended rows are owned by
// the document's owner. The caller (the BFF edit endpoint for the user
// path, the agent tool loop for the assistant path) is responsible for
// the higher-level "may this actor edit this document" decision before
// dispatch; the row-owner threading guarantees the appended history is
// always attributed to the document's owner regardless.
package library

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/core/id"
)

// resultConcept is the synthetic MemoryNode concept the capabilities
// return. Never persisted -- the engine round-trips it back to the
// caller and discards it.
const resultConcept = "integration:library:result"

// Integration is the Library edit capability surface. Holds the engine
// handle so it can re-enter Execute for the read-then-write dance.
type Integration struct {
	engine memql.IntegrationEngineAccess
}

// NewIntegration wires the engine handle. The factory is in plugin.go;
// this constructor is exposed for tests that supply a stub engine.
func NewIntegration(engine memql.IntegrationEngineAccess) *Integration {
	return &Integration{engine: engine}
}

// IntegrationName implements memql.IntegrationProvider.
func (i *Integration) IntegrationName() string { return "library" }

// Capabilities implements memql.IntegrationProvider.
func (i *Integration) Capabilities() []memql.IntegrationCapability {
	return []memql.IntegrationCapability{
		{
			Name:        "editDocument",
			Description: "Append a new version of a Library document with new content (memql#1229 user edit / memql#1231 assistant edit). Reads the current latest version, computes the next versionNumber + parentVersionId, appends an immutable v1:library:documentVersion snapshot (authorKind=user|assistant), and re-inserts the backing generatedOutput so the Library viewer reflects the edit. Optimistic concurrency: pass expectedVersion to fail the edit when the document moved on under you. ownerUserId is threaded from the document row, not the caller.",
			Handler:     i.handleEditDocument,
			ArgsSchema: map[string]string{
				"documentId":       "string (required) -- the logical document id (the backing v1:library:generatedOutput id)",
				"content":          "string -- the new full content body (markdown / text). Set this OR attachmentId.",
				"attachmentId":     "string -- v1:common:attachment id when the new version is file-backed. Set this OR content.",
				"note":             "string -- optional short note describing the change",
				"authorKind":       "string -- 'user' (default) or 'assistant'",
				"authorId":         "string -- the editing user id (authorKind=user) or agent id (authorKind=assistant)",
				"expectedVersion":  "number -- optional optimistic-concurrency token; the latest versionNumber the caller saw",
				"producedByPlanId": "string -- optional planner plan id when an assistant edit came through the planner",
				"partitionId":      "string -- optional space id the edit happened in",
			},
		},
		{
			Name:        "editDocumentAsAssistant",
			Description: "Assistant-facing edit (memql#1231): append a new version of an existing Library document authored by an agent. Identical to editDocument but hardcodes authorKind=assistant and maps the auto-injected agentId to the version's authorId so the history records the agent as the author + carries the planner provenance. Used by the agent editDocument tool when the user asks to edit / update / revise an existing document.",
			Handler:     i.handleEditDocumentAsAssistant,
			ArgsSchema: map[string]string{
				"documentId":       "string (required) -- the logical document id to revise",
				"content":          "string -- the new full content body",
				"attachmentId":     "string -- v1:common:attachment id when file-backed",
				"note":             "string -- optional short note describing the change",
				"agentId":          "string -- the editing agent id (recorded as authorId); auto-injected by the agent runtime",
				"producedByPlanId": "string -- optional planner plan id",
				"partitionId":      "string -- optional space id; auto-injected by the agent runtime",
				"expectedVersion":  "number -- optional optimistic-concurrency token",
			},
		},
		{
			Name:        "restoreDocumentVersion",
			Description: "Restore a Library document to an earlier version by APPENDING a new latest version equal to the chosen one (memql#1230). Forward-only and non-destructive: history is never deleted; the restore lands as a new version with note 'restored from vN' and authorKind=system. ownerUserId is threaded from the document row.",
			Handler:     i.handleRestoreDocumentVersion,
			ArgsSchema: map[string]string{
				"documentId": "string (required) -- the logical document id",
				"versionId":  "string (required) -- the documentVersion row id to restore content from",
				"authorId":   "string -- optional id of the user who triggered the restore (recorded as provenance)",
			},
		},
	}
}

// editResult is the payload returned by handleEditDocument /
// handleRestoreDocumentVersion. Best-effort observability for the
// caller; the BFF endpoint surfaces newVersion to the client so the
// next edit can pass it as expectedVersion.
type editResult struct {
	DocumentId   string `json:"documentId"`
	VersionId    string `json:"versionId,omitempty"`
	NewVersion   int    `json:"newVersion,omitempty"`
	PriorVersion int    `json:"priorVersion,omitempty"`
	AuthorKind   string `json:"authorKind,omitempty"`
	Conflict     bool   `json:"conflict,omitempty"`
	Reason       string `json:"reason,omitempty"`
}

func (i *Integration) handleEditDocument(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	if i == nil || i.engine == nil {
		return nil, fmt.Errorf("library.editDocument: integration not initialized")
	}
	documentId := strings.TrimSpace(asString(args["documentId"]))
	if documentId == "" {
		return nil, fmt.Errorf("library.editDocument: 'documentId' is required")
	}
	content := asString(args["content"])
	attachmentId := strings.TrimSpace(asString(args["attachmentId"]))
	if strings.TrimSpace(content) == "" && attachmentId == "" {
		return nil, fmt.Errorf("library.editDocument: provide 'content' and/or 'attachmentId' -- a version must carry a body or an attachment")
	}
	note := strings.TrimSpace(asString(args["note"]))
	authorKind := strings.TrimSpace(asString(args["authorKind"]))
	if authorKind == "" {
		authorKind = "user"
	}
	if authorKind != "user" && authorKind != "assistant" && authorKind != "system" {
		return nil, fmt.Errorf("library.editDocument: invalid authorKind %q (want user|assistant|system)", authorKind)
	}
	authorId := strings.TrimSpace(asString(args["authorId"]))
	producedByPlanId := strings.TrimSpace(asString(args["producedByPlanId"]))
	partitionId := strings.TrimSpace(asString(args["partitionId"]))
	expectedVersion, hasExpected := intArg(args["expectedVersion"])

	// Resolve the backing generatedOutput to thread the owner + carry
	// the document spine (title / source / format) onto both the new
	// version's re-insert and -- when no history exists yet -- to seed
	// version 1's owner. The documentId IS the generatedOutput id.
	doc, err := i.loadGeneratedOutput(ctx, documentId)
	if err != nil {
		return nil, fmt.Errorf("library.editDocument: load document %q: %w", documentId, err)
	}
	if doc == nil {
		return nil, fmt.Errorf("library.editDocument: document %q not found", documentId)
	}
	ownerUserId := stringField(doc, "ownerUserId")
	if ownerUserId == "" {
		return nil, fmt.Errorf("library.editDocument: document %q has no owner -- cannot attribute the edit", documentId)
	}

	// Read the current history under the owner actor to compute the next
	// version number + parent pointer. The owner-threaded actor keeps
	// the read inside the owned-row authz model.
	ownerCtx := withUserActor(ctx, ownerUserId)
	latest, latestNum, err := i.latestVersion(ownerCtx, documentId)
	if err != nil {
		return nil, fmt.Errorf("library.editDocument: read history for %q: %w", documentId, err)
	}

	// Optimistic concurrency: if the caller named an expectedVersion and
	// it no longer matches the current latest, the document moved on
	// under them -- reject rather than silently clobbering the diverged
	// history with a stale base. (The history itself is never lost; this
	// just refuses to ADD a version on top of an unexpected base.)
	if hasExpected && expectedVersion != latestNum {
		return wrapResult(editResult{
			DocumentId:   documentId,
			PriorVersion: latestNum,
			Conflict:     true,
			Reason:       fmt.Sprintf("optimistic version conflict: expected latest v%d, but the document is at v%d", expectedVersion, latestNum),
		})
	}

	parentVersionId := ""
	if latest != nil {
		parentVersionId = stringField(latest, "id")
	}
	nextNum := latestNum + 1

	versionId := string(id.New().MustFromMap(map[string]any{
		"kind":       "documentVersion",
		"documentId": documentId,
		"version":    nextNum,
		"at":         time.Now().UTC().Format(time.RFC3339Nano),
	}))

	if err := i.appendVersion(ownerCtx, appendArgs{
		versionId:        versionId,
		documentId:       documentId,
		ownerUserId:      ownerUserId,
		versionNumber:    nextNum,
		content:          content,
		attachmentId:     attachmentId,
		authorKind:       authorKind,
		authorId:         authorId,
		note:             note,
		parentVersionId:  parentVersionId,
		producedByPlanId: producedByPlanId,
		partitionId:      partitionId,
	}); err != nil {
		return nil, fmt.Errorf("library.editDocument: append version: %w", err)
	}

	// Re-insert the backing generatedOutput (same id, new node version)
	// so the artifact index + Library viewer reflect the latest content.
	if err := i.updateBackingContent(ownerCtx, doc, content, attachmentId); err != nil {
		return nil, fmt.Errorf("library.editDocument: update backing content: %w", err)
	}
	// Bump the artifact index watermark (idempotent re-stamp).
	i.touchArtifact(ownerCtx, doc)

	return wrapResult(editResult{
		DocumentId:   documentId,
		VersionId:    versionId,
		NewVersion:   nextNum,
		PriorVersion: latestNum,
		AuthorKind:   authorKind,
	})
}

// handleEditDocumentAsAssistant is the assistant-facing edit path
// (memql#1231). It normalizes the agent-runtime args (authorKind is
// always assistant; the auto-injected agentId becomes the version's
// authorId) and delegates to handleEditDocument so both paths share the
// exact same append + optimistic-concurrency + backing-content logic.
func (i *Integration) handleEditDocumentAsAssistant(ctx context.Context, args map[string]any, depth int) ([]memorynodes.MemoryNode, error) {
	merged := make(map[string]any, len(args)+2)
	for k, v := range args {
		merged[k] = v
	}
	merged["authorKind"] = "assistant"
	// The agent runtime auto-injects agentId; record it as the version
	// author. An explicit authorId (rare) is respected if present.
	if strings.TrimSpace(asString(merged["authorId"])) == "" {
		merged["authorId"] = asString(args["agentId"])
	}
	return i.handleEditDocument(ctx, merged, depth)
}

func (i *Integration) handleRestoreDocumentVersion(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	if i == nil || i.engine == nil {
		return nil, fmt.Errorf("library.restoreDocumentVersion: integration not initialized")
	}
	documentId := strings.TrimSpace(asString(args["documentId"]))
	if documentId == "" {
		return nil, fmt.Errorf("library.restoreDocumentVersion: 'documentId' is required")
	}
	sourceVersionId := strings.TrimSpace(asString(args["versionId"]))
	if sourceVersionId == "" {
		return nil, fmt.Errorf("library.restoreDocumentVersion: 'versionId' is required")
	}
	authorId := strings.TrimSpace(asString(args["authorId"]))

	doc, err := i.loadGeneratedOutput(ctx, documentId)
	if err != nil {
		return nil, fmt.Errorf("library.restoreDocumentVersion: load document %q: %w", documentId, err)
	}
	if doc == nil {
		return nil, fmt.Errorf("library.restoreDocumentVersion: document %q not found", documentId)
	}
	ownerUserId := stringField(doc, "ownerUserId")
	if ownerUserId == "" {
		return nil, fmt.Errorf("library.restoreDocumentVersion: document %q has no owner", documentId)
	}
	ownerCtx := withUserActor(ctx, ownerUserId)

	// Read the chosen source version (its content) + the current latest
	// (for the next number + parent pointer). Both come from the same
	// owner-scoped history read.
	history, err := i.versionHistory(ownerCtx, documentId)
	if err != nil {
		return nil, fmt.Errorf("library.restoreDocumentVersion: read history: %w", err)
	}
	var source map[string]any
	latestNum := 0
	var latest map[string]any
	for _, v := range history {
		if stringField(v, "id") == sourceVersionId {
			source = v
		}
		if n := intField(v, "versionNumber"); n >= latestNum {
			latestNum = n
			latest = v
		}
	}
	if source == nil {
		return nil, fmt.Errorf("library.restoreDocumentVersion: version %q not found in document %q history (or not owned by caller)", sourceVersionId, documentId)
	}

	sourceNum := intField(source, "versionNumber")
	content := stringField(source, "content")
	attachmentId := stringField(source, "attachmentId")
	parentVersionId := ""
	if latest != nil {
		parentVersionId = stringField(latest, "id")
	}
	nextNum := latestNum + 1

	versionId := string(id.New().MustFromMap(map[string]any{
		"kind":       "documentVersion",
		"documentId": documentId,
		"version":    nextNum,
		"restoredOf": sourceVersionId,
		"at":         time.Now().UTC().Format(time.RFC3339Nano),
	}))

	if err := i.appendVersion(ownerCtx, appendArgs{
		versionId:       versionId,
		documentId:      documentId,
		ownerUserId:     ownerUserId,
		versionNumber:   nextNum,
		content:         content,
		attachmentId:    attachmentId,
		authorKind:      "system",
		authorId:        authorId,
		note:            fmt.Sprintf("restored from v%d", sourceNum),
		parentVersionId: parentVersionId,
	}); err != nil {
		return nil, fmt.Errorf("library.restoreDocumentVersion: append version: %w", err)
	}

	if err := i.updateBackingContent(ownerCtx, doc, content, attachmentId); err != nil {
		return nil, fmt.Errorf("library.restoreDocumentVersion: update backing content: %w", err)
	}
	i.touchArtifact(ownerCtx, doc)

	return wrapResult(editResult{
		DocumentId:   documentId,
		VersionId:    versionId,
		NewVersion:   nextNum,
		PriorVersion: latestNum,
		AuthorKind:   "system",
	})
}

// appendArgs bundles the documentVersion append fields.
type appendArgs struct {
	versionId        string
	documentId       string
	ownerUserId      string
	versionNumber    int
	content          string
	attachmentId     string
	authorKind       string
	authorId         string
	note             string
	parentVersionId  string
	producedByPlanId string
	partitionId      string
}

func (i *Integration) appendVersion(ctx context.Context, a appendArgs) error {
	q := fmt.Sprintf(
		`appendDocumentVersion({versionId: %q, documentId: %q, ownerUserId: %q, versionNumber: %d, content: %q, attachmentId: %q, authorKind: %q, authorId: %q, note: %q, parentVersionId: %q, producedByPlanId: %q, partitionId: %q})`,
		a.versionId, a.documentId, a.ownerUserId, a.versionNumber, a.content, a.attachmentId,
		a.authorKind, a.authorId, a.note, a.parentVersionId, a.producedByPlanId, a.partitionId,
	)
	_, err := i.engine.Execute(ctx, q)
	return err
}

// updateBackingContent re-inserts the generatedOutput row (same id) with
// the new latest content, carrying the existing spine fields forward.
func (i *Integration) updateBackingContent(ctx context.Context, doc map[string]any, content, attachmentId string) error {
	source := stringField(doc, "source")
	if source == "" {
		source = "agent_generated"
	}
	format := stringField(doc, "format")
	q := fmt.Sprintf(
		`updateGeneratedOutputContent({outputId: %q, ownerUserId: %q, title: %q, summary: %q, body: %q, attachmentId: %q, format: %q, mimeType: %q, source: %q, partitionId: %q, producedByPlanId: %q, producedByAgentId: %q})`,
		stringField(doc, "id"),
		stringField(doc, "ownerUserId"),
		stringField(doc, "title"),
		stringField(doc, "summary"),
		content,
		attachmentId,
		format,
		stringField(doc, "mimeType"),
		source,
		stringField(doc, "partitionId"),
		stringField(doc, "producedByPlanId"),
		stringField(doc, "producedByAgentId"),
	)
	_, err := i.engine.Execute(ctx, q)
	return err
}

// touchArtifact re-stamps the artifact index row so its updatedAt
// watermark advances after an edit. Best-effort: the version + backing
// content are the source of truth; a failed index re-stamp just leaves
// the Library sort key stale until the next promotion. sourceConceptRef
// is the canonical 'v1:library:generatedOutput:<id>' form.
func (i *Integration) touchArtifact(ctx context.Context, doc map[string]any) {
	docId := stringField(doc, "id")
	if docId == "" {
		return
	}
	sourceRef := "v1:library:generatedOutput:" + docId
	source := stringField(doc, "source")
	if source == "" {
		source = "agent_generated"
	}
	q := fmt.Sprintf(
		`createArtifact({sourceConceptRef: %q, ownerUserId: %q, lens: "artifact", kind: "generated_output", source: %q, title: %q, summary: %q, format: %q, mimeType: %q, partitionId: %q, producedByPlanId: %q})`,
		sourceRef,
		stringField(doc, "ownerUserId"),
		source,
		stringField(doc, "title"),
		stringField(doc, "summary"),
		stringField(doc, "format"),
		stringField(doc, "mimeType"),
		stringField(doc, "partitionId"),
		stringField(doc, "producedByPlanId"),
	)
	_, _ = i.engine.Execute(ctx, q)
}

// loadGeneratedOutput reads the backing document row. Runs under a
// system actor for the lookup (the row may be needed before the owner
// is known); the actual edit writes run under the owner actor.
func (i *Integration) loadGeneratedOutput(ctx context.Context, documentId string) (map[string]any, error) {
	q := fmt.Sprintf(`generatedOutputById({outputId: %q})`, documentId)
	raw, err := i.engine.Execute(systemActorContext(ctx), q)
	if err != nil {
		return nil, err
	}
	rows := extractRows(raw)
	if len(rows) == 0 {
		return nil, nil
	}
	return rows[0], nil
}

// versionHistory returns every retained version of the document under
// the owner actor.
func (i *Integration) versionHistory(ctx context.Context, documentId string) ([]map[string]any, error) {
	q := fmt.Sprintf(`documentVersionsForOwner({documentId: %q})`, documentId)
	raw, err := i.engine.Execute(ctx, q)
	if err != nil {
		return nil, err
	}
	return extractRows(raw), nil
}

// latestVersion returns the current latest version row + its number
// (0 + nil when no history exists yet).
func (i *Integration) latestVersion(ctx context.Context, documentId string) (map[string]any, int, error) {
	history, err := i.versionHistory(ctx, documentId)
	if err != nil {
		return nil, 0, err
	}
	var latest map[string]any
	latestNum := 0
	for _, v := range history {
		if n := intField(v, "versionNumber"); n >= latestNum {
			latestNum = n
			latest = v
		}
	}
	return latest, latestNum, nil
}

func wrapResult(r editResult) ([]memorynodes.MemoryNode, error) {
	payload, err := json.Marshal(r)
	if err != nil {
		return nil, fmt.Errorf("library: marshal result: %w", err)
	}
	return []memorynodes.MemoryNode{{
		ID:        fmt.Sprintf("library:edit:%s:%d", r.DocumentId, time.Now().UnixNano()),
		Concept:   resultConcept,
		Type:      memorynodes.NodeTypeObject,
		CreatedAt: time.Now().UTC(),
		Payload:   payload,
	}}, nil
}

// withUserActor stamps a synthetic user actor so the append + content
// writes are attributed to (and authorized for) the document's owner.
// Mirrors integrations/agents/integration.go's withUserActor.
func withUserActor(ctx context.Context, ownerUserId string) context.Context {
	if strings.TrimSpace(ownerUserId) == "" {
		return ctx
	}
	claims := map[string]any{
		"sub":   ownerUserId,
		"email": ownerUserId,
		"role":  "user",
	}
	token := auth.BuildTokenInfo(claims)
	ctx = auth.ContextWithClaims(ctx, claims)
	return auth.ContextWithToken(ctx, token)
}

// systemActorContext wraps ctx with a synthetic system actor for the
// pre-owner document lookup.
func systemActorContext(ctx context.Context) context.Context {
	return auth.ContextWithToken(ctx, &auth.TokenInfo{
		Subject: "system:libraryEdit",
		Claims: map[string]any{
			"sub":  "system:libraryEdit",
			"role": "system",
		},
	})
}

func asString(v any) string {
	if v == nil {
		return ""
	}
	s, _ := v.(string)
	return s
}

func stringField(m map[string]any, key string) string {
	if m == nil {
		return ""
	}
	if v, ok := m[key]; ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}

// intField coerces a row field to int across the float64 / json.Number /
// int representations the engine round-trip can produce.
func intField(m map[string]any, key string) int {
	if m == nil {
		return 0
	}
	n, _ := intArg(m[key])
	return n
}

// intArg coerces an arbitrary arg value to int, reporting whether a
// numeric value was present.
func intArg(v any) (int, bool) {
	switch t := v.(type) {
	case int:
		return t, true
	case int64:
		return safeIntFromInt64(t), true
	case float64:
		return safeIntFromFloat(t), true
	case float32:
		return safeIntFromFloat(float64(t)), true
	case json.Number:
		// Prefer the integer parse so a large whole number keeps full
		// precision; the int64 result is bound-checked below. Fall back
		// to the float parse for fractional values.
		if i, err := t.Int64(); err == nil {
			return safeIntFromInt64(i), true
		}
		if f, err := t.Float64(); err == nil {
			return safeIntFromFloat(f), true
		}
	}
	return 0, false
}

// safeIntFromInt64 narrows an int64 to int, clamping to the 32-bit range
// (the worst-case int width) so the conversion is overflow-safe on every
// platform. Bound-checked on the int64 source directly so the narrowing
// is provably bounded (go/incorrect-integer-conversion).
func safeIntFromInt64(v int64) int {
	if v > math.MaxInt32 {
		return math.MaxInt32
	}
	if v < math.MinInt32 {
		return math.MinInt32
	}
	return int(v)
}

// safeIntFromFloat narrows a float64 to int. It bounds the float to the
// 32-bit range (exactly representable in float64) and then funnels through
// safeIntFromInt64 -- the single int->int narrowing sink -- rather than
// converting the float to int directly. A direct int(float64) is flagged
// by go/incorrect-integer-conversion because the float carries no provable
// integer bound; routing through the int64 sink keeps the narrowing safe
// and recognizable. These args are small counts / limits.
func safeIntFromFloat(v float64) int {
	if math.IsNaN(v) || v > math.MaxInt32 || v < math.MinInt32 {
		return 0
	}
	return safeIntFromInt64(int64(v))
}

// extractRows normalizes the engine's Execute return into a uniform
// []map[string]any with payload fields at the top level. Mirrors
// integrations/dailyspace/dailyspace.go's extractRows.
func extractRows(raw any) []map[string]any {
	if raw == nil {
		return nil
	}
	if res, ok := raw.(*memql.ExecuteResult); ok && res != nil {
		raw = res.OutputPayload()
	}
	if raw == nil {
		return nil
	}
	if bundle, ok := raw.(*memqlv1.GraphBundle); ok && bundle != nil {
		out := make([]map[string]any, 0, len(bundle.GetNodes()))
		for _, n := range bundle.GetNodes() {
			if n == nil {
				continue
			}
			row := map[string]any{
				"id":      n.GetId(),
				"concept": n.GetConcept(),
			}
			if payload := n.GetPayload(); payload != nil {
				for k, v := range payload.AsMap() {
					row[k] = v
				}
			}
			out = append(out, row)
		}
		return out
	}
	switch v := raw.(type) {
	case []map[string]any:
		return v
	case []any:
		out := make([]map[string]any, 0, len(v))
		for _, item := range v {
			if m, ok := item.(map[string]any); ok {
				out = append(out, m)
			}
		}
		return out
	case map[string]any:
		if bundle, ok := v["bundle"].(map[string]any); ok {
			if nodes, ok := bundle["nodes"].([]any); ok {
				out := make([]map[string]any, 0, len(nodes))
				for _, n := range nodes {
					if m, ok := n.(map[string]any); ok {
						out = append(out, m)
					}
				}
				return out
			}
		}
		return []map[string]any{v}
	}
	return nil
}
