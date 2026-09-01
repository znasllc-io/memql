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
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	langparser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/core/id"
	"github.com/znasllc-io/memql/core/num"
)

// resultConcept is the synthetic MemoryNode concept the capabilities
// return. Never persisted -- the engine round-trips it back to the
// caller and discards it.
const resultConcept = "integration:library:result"

// Integration is the Library edit capability surface. Holds the engine
// handle so it can re-enter Execute for the read-then-write dance.
type Integration struct {
	engine memql.IntegrationEngineAccess

	// --- the analysis / search / train half (memql#4342) ---
	//
	// All of these are OPTIONAL, and each has a documented behaviour when
	// unset, because the edit capabilities above must keep working on a
	// node that wires none of them: no extractor treats every upload as an
	// opaque type (stored bytes, no chunks); no logger falls back to
	// slog.Default; no authorizer refuses every non-operator train
	// (train.go states why deny is the right default there); zero poll
	// settings take the defaults in analysis.go.
	extractor            TextExtractor
	logger               *slog.Logger
	domainAuthorizer     DomainWriteAuthorizer
	artifactPollAttempts int
	artifactPollInterval time.Duration
	// blobFetcher streams stored bytes for the chunked-upload case
	// (memql#4782): hash stamping always, extraction when readable. No
	// fetcher keeps chunked files hash-absent -- "not measured" -- and
	// unextracted; see SetBlobFetcher in analysis.go.
	blobFetcher BlobFetcher
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
		{
			Name:        "addArtifactLabel",
			Description: "Add a label to a Library artifact index row. Idempotent -- a label already present is left alone and nothing is written. Loads the row under a system actor (the caller only supplies artifactId), then merges + writes back under a synthetic actor derived from the row's own ownerUserId, so a caller can only ever label an artifact they own.",
			Handler:     i.handleAddArtifactLabel,
			ArgsSchema: map[string]string{
				"artifactId": "string (required) -- the v1:library:artifact row id",
				"label":      "string (required) -- the label to add; blank is refused",
			},
		},
		{
			Name:        "removeArtifactLabel",
			Description: "Remove a label from a Library artifact index row. Idempotent -- a label already absent is left alone and nothing is written. Same owner-threaded load/write shape as addArtifactLabel.",
			Handler:     i.handleRemoveArtifactLabel,
			ArgsSchema: map[string]string{
				"artifactId": "string (required) -- the v1:library:artifact row id",
				"label":      "string (required) -- the label to remove; blank is refused",
			},
		},
		{
			Name:        "similarArtifacts",
			Description: "Search the caller's Library by meaning (memql#4342). Runs similarTo over the v1:library:fileChunk rows the analysis pass embedded, keeps only chunks owned by the acting user, folds them up to artifacts by best score, and re-reads each surviving artifact through the owner-gated libraryArtifactById before returning it. Pass 'text' to search by a phrase, or 'artifactId' to find artifacts like an existing one (the seed artifact is excluded from its own results).",
			Handler:     i.handleSimilarArtifacts,
			ArgsSchema: map[string]string{
				"text":       "string -- the phrase to search by. Set this OR artifactId.",
				"artifactId": "string -- find artifacts similar to this one; its summary (else its first chunk) becomes the query. Set this OR text.",
				"limit":      "number -- max artifacts to return (default 5, capped at 50)",
			},
			// The handler already ranks by score and the ranking IS the
			// answer -- without this the engine's default sort-by-createdAt
			// would reshuffle the results into the order they were built.
			// Same reason integrations/similarity sets it on similarTo.
			PreserveOrder: true,
		},
		{
			Name:        "trainFile",
			Description: "Train a Library file into a knowledge domain (memql#4342, design D7 -- upload and train are two acts). Reconstructs the file's extracted text from its chunks, runs integration.knowledge.ingest over it with sourceRef 'artifact:<artifactId>', appends the domain to the file's trainedIntoDomainIds and writes an audit event. The file is resolved under the caller's own actor, so another user's file cannot be trained; the domain must be one the caller may write to, decided by the wired DomainWriteAuthorizer (a cluster owner always may; with no authorizer wired everyone else is refused).",
			Handler:     i.handleTrainFile,
			ArgsSchema: map[string]string{
				"fileId":   "string (required) -- the v1:library:file row id, which must belong to the acting user",
				"domainId": "string (required) -- the knowledge domain to train into",
			},
		},
		{
			Name: "restampFileArtifact",
			Description: "Re-stamp a file-backed Library artifact index row from its backing v1:library:file (epic memql#4806). " +
				"The upload route calls it after a supersede: a new version changes facts the INDEX carries -- the title when " +
				"the bytes arrived under a different name, the format and mimeType, and the updatedAt watermark the Library " +
				"sorts and pulses on -- and without this the list shows the old name until the analysis pass happens to finish. " +
				"Idempotent and information-free: it copies what the file row already says, carrying labels / archived / folderId " +
				"forward exactly as the analysis pass's own re-stamp does. The file is read under the CALLER's actor, so it can " +
				"only ever touch an artifact they own.",
			Handler: i.handleRestampFileArtifact,
			ArgsSchema: map[string]string{
				"fileId": "string (required) -- the v1:library:file row id, which must belong to the acting user",
			},
		},
	}
}

// restampResult is what the re-stamp answers with. `restamped` false is not
// an error: the re-stamp is best-effort by construction (a file whose index
// row has not been promoted yet, or whose carry-forward read failed, is
// skipped rather than written over), and the caller -- the upload route --
// has already stored the bytes and moved the head by the time it asks.
type restampResult struct {
	FileId    string `json:"fileId"`
	Restamped bool   `json:"restamped"`
}

// handleRestampFileArtifact re-versions the artifact index row for one file.
//
// It is a thin wrapper over the analysis pass's own restampArtifact, and
// deliberately so: two re-stamps of the same row that disagreed about what to
// carry forward would be the memql#4288 hazard with two authors instead of
// one. The owner threading is the same as every other capability here -- the
// file loads under the CALLER's actor through libraryFileById, so a file that
// is not theirs is a file that is not there, and the write then runs under
// the owner resolved from the row itself.
func (i *Integration) handleRestampFileArtifact(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	if i == nil || i.engine == nil {
		return nil, fmt.Errorf("library.restampFileArtifact: integration not initialized")
	}
	fileId := strings.TrimSpace(asString(args["fileId"]))
	if fileId == "" {
		return nil, fmt.Errorf("library.restampFileArtifact: 'fileId' is required")
	}
	file, err := i.loadFile(ctx, fileId)
	if err != nil {
		return nil, fmt.Errorf("library.restampFileArtifact: %w", err)
	}
	if file == nil {
		// Absent and not-yours are one answer, for the reason every other
		// Library read gives: the caller's own actor decided, and the
		// response must not reintroduce the distinction authorization
		// just erased.
		return nil, fmt.Errorf("library.restampFileArtifact: no such file")
	}
	owner := stringField(file, "ownerUserId")
	if owner == "" {
		return nil, fmt.Errorf("library.restampFileArtifact: the file row names no owner")
	}
	before, promoted := i.artifactForFile(ctx, fileId)
	i.restampArtifact(withUserActor(ctx, owner), fileId)
	return wrapRestampResult(restampResult{
		FileId:    fileId,
		Restamped: promoted && before != nil,
	})
}

func wrapRestampResult(r restampResult) ([]memorynodes.MemoryNode, error) {
	payload, err := json.Marshal(r)
	if err != nil {
		return nil, fmt.Errorf("library.restampFileArtifact: marshal result: %w", err)
	}
	return []memorynodes.MemoryNode{{
		ID:        fmt.Sprintf("library:restamp:%s:%d", r.FileId, time.Now().UnixNano()),
		Concept:   resultConcept,
		Type:      memorynodes.NodeTypeObject,
		CreatedAt: time.Now().UTC(),
		Payload:   payload,
	}}, nil
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
	if strings.TrimSpace(ownerUserId) == "" {
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
	if strings.TrimSpace(ownerUserId) == "" {
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

// artifactLabelResult is the payload returned by handleAddArtifactLabel /
// handleRemoveArtifactLabel.
type artifactLabelResult struct {
	ArtifactId string   `json:"artifactId"`
	Label      string   `json:"label"`
	Labels     []string `json:"labels"`
	Changed    bool     `json:"changed"`
}

// handleAddArtifactLabel adds a label to a Library artifact index row.
// Idempotent: a label already present is left alone and nothing is
// written (mergeLabelAdd reports changed=false). Follows
// handleEditDocument's exact load/write shape: loadArtifactUnderOwner
// reads the row under a system actor (the caller only supplies
// artifactId, so the owner isn't known yet) and hands back a context
// carrying a synthetic owner actor derived from the row's OWN
// ownerUserId -- the write then runs under that borrowed authority, which
// is what makes "you cannot label someone else's artifact" true without
// a new mechanism (the row simply will not load for anyone else).
func (i *Integration) handleAddArtifactLabel(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	if i == nil || i.engine == nil {
		return nil, fmt.Errorf("library.addArtifactLabel: integration not initialized")
	}
	artifactId := strings.TrimSpace(asString(args["artifactId"]))
	if artifactId == "" {
		return nil, fmt.Errorf("library.addArtifactLabel: 'artifactId' is required")
	}
	label := strings.TrimSpace(asString(args["label"]))
	if label == "" {
		return nil, fmt.Errorf("library.addArtifactLabel: 'label' is required and cannot be blank")
	}

	row, ownerCtx, err := i.loadArtifactUnderOwner(ctx, artifactId)
	if err != nil {
		return nil, fmt.Errorf("library.addArtifactLabel: %w", err)
	}

	current := stringSliceField(row, "labels")
	merged, changed := mergeLabelAdd(current, label)
	if changed {
		if err := i.writeArtifactLabels(ownerCtx, row, merged); err != nil {
			return nil, fmt.Errorf("library.addArtifactLabel: write back: %w", err)
		}
	} else {
		merged = current
	}

	return wrapLabelResult(artifactLabelResult{
		ArtifactId: artifactId,
		Label:      label,
		Labels:     merged,
		Changed:    changed,
	})
}

// handleRemoveArtifactLabel removes a label from a Library artifact index
// row. Idempotent: a label already absent is left alone and nothing is
// written. Same owner-threaded load/write shape as handleAddArtifactLabel.
func (i *Integration) handleRemoveArtifactLabel(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	if i == nil || i.engine == nil {
		return nil, fmt.Errorf("library.removeArtifactLabel: integration not initialized")
	}
	artifactId := strings.TrimSpace(asString(args["artifactId"]))
	if artifactId == "" {
		return nil, fmt.Errorf("library.removeArtifactLabel: 'artifactId' is required")
	}
	label := strings.TrimSpace(asString(args["label"]))
	if label == "" {
		return nil, fmt.Errorf("library.removeArtifactLabel: 'label' is required and cannot be blank")
	}

	row, ownerCtx, err := i.loadArtifactUnderOwner(ctx, artifactId)
	if err != nil {
		return nil, fmt.Errorf("library.removeArtifactLabel: %w", err)
	}

	current := stringSliceField(row, "labels")
	remaining, changed := mergeLabelRemove(current, label)
	if changed {
		if err := i.writeArtifactLabels(ownerCtx, row, remaining); err != nil {
			return nil, fmt.Errorf("library.removeArtifactLabel: write back: %w", err)
		}
	} else {
		remaining = current
	}

	return wrapLabelResult(artifactLabelResult{
		ArtifactId: artifactId,
		Label:      label,
		Labels:     remaining,
		Changed:    changed,
	})
}

// loadArtifactUnderOwner loads the artifact row under a system actor
// (mirrors loadGeneratedOutput -- the row's owner is not known until it is
// read) and returns it alongside a context carrying a SYNTHETIC owner
// actor derived from the row's own ownerUserId, for the caller to run its
// write-back under. Refuses an unknown artifact or one with no owner,
// exactly as handleEditDocument refuses an ownerless document.
func (i *Integration) loadArtifactUnderOwner(ctx context.Context, artifactId string) (map[string]any, context.Context, error) {
	q := fmt.Sprintf(`query libraryArtifactById(artifactId: %s)`, langparser.QuoteString(artifactId))
	raw, err := i.engine.Execute(systemActorContext(ctx), q)
	if err != nil {
		return nil, nil, fmt.Errorf("load artifact %q: %w", artifactId, err)
	}
	rows := extractRows(raw)
	if len(rows) == 0 {
		return nil, nil, fmt.Errorf("artifact %q not found", artifactId)
	}
	row := rows[0]
	ownerUserId := stringField(row, "ownerUserId")
	if strings.TrimSpace(ownerUserId) == "" {
		return nil, nil, fmt.Errorf("artifact %q has no owner -- cannot attribute the change", artifactId)
	}
	return row, withUserActor(ctx, ownerUserId), nil
}

// writeArtifactLabels re-versions the artifact row via createArtifact,
// carrying every field the row itself currently holds forward and
// replacing labels with the given set. createArtifact's insert body is a
// bare `insert{}` block -- a full replace (D3): a field this call omits
// is absent from the new version, not carried over from the prior one --
// so every artifact field is threaded through explicitly rather than
// leaving any of them to an implicit carry-forward that does not exist.
// artifactEnumFields lists createArtifact's OPTIONAL enum args (mirrors
// the enum declarations on dsl/library/mutations.memql's createArtifact
// args block). lens/kind/source are enums too but required(!), so a row
// that exists at all already carries a valid value for them; format,
// scope and validationStatus are optional, and a real row's stored value
// is routinely blank -- none of the five promotion automations pass
// scope, and a record-lens row has no format at all.
var artifactEnumFields = []string{"format", "scope", "validationStatus"}

// writeArtifactLabels re-versions the artifact row via createArtifact,
// carrying every field the row itself currently holds forward and
// replacing labels with the given set. createArtifact's insert body is a
// bare `insert{}` block -- a full replace (D3): a field this call omits
// is absent from the new version, not carried over from the prior one --
// so every artifact field is threaded through explicitly rather than
// leaving any of them to an implicit carry-forward that does not exist.
//
// The three OPTIONAL ENUM fields (artifactEnumFields) are the one
// exception to "thread every field through": createArtifact's arg
// validation rejects a PRESENT value outside its declared enum set, and
// an empty string IS present -- it is not the same as the caller never
// naming the argument. A blank optional enum on the loaded row (the
// common case: no promotion automation passes scope, a record-lens row
// has no format) must therefore be OMITTED from the call entirely, not
// quoted through as "". Plain-string optionals (summary, mimeType,
// partitionId, ...) have no such set to violate, so they stay
// unconditional -- the same shape touchArtifact and the automations
// already use for them.
func (i *Integration) writeArtifactLabels(ctx context.Context, row map[string]any, labels []string) error {
	parts := []string{
		fmt.Sprintf("sourceConceptRef: %s", langparser.QuoteString(stringField(row, "sourceConceptRef"))),
		fmt.Sprintf("ownerUserId: %s", langparser.QuoteString(stringField(row, "ownerUserId"))),
		fmt.Sprintf("lens: %s", langparser.QuoteString(stringField(row, "lens"))),
		fmt.Sprintf("kind: %s", langparser.QuoteString(stringField(row, "kind"))),
		fmt.Sprintf("source: %s", langparser.QuoteString(stringField(row, "source"))),
		fmt.Sprintf("title: %s", langparser.QuoteString(stringField(row, "title"))),
		fmt.Sprintf("summary: %s", langparser.QuoteString(stringField(row, "summary"))),
		fmt.Sprintf("mimeType: %s", langparser.QuoteString(stringField(row, "mimeType"))),
		fmt.Sprintf("live: %t", boolField(row, "live")),
		fmt.Sprintf("labels: %s", quoteStringArray(labels)),
		fmt.Sprintf("partitionId: %s", langparser.QuoteString(stringField(row, "partitionId"))),
		fmt.Sprintf("agentId: %s", langparser.QuoteString(stringField(row, "agentId"))),
		fmt.Sprintf("producedByPlanId: %s", langparser.QuoteString(stringField(row, "producedByPlanId"))),
		fmt.Sprintf("producedByWorkerId: %s", langparser.QuoteString(stringField(row, "producedByWorkerId"))),
		fmt.Sprintf("producedByWorkerName: %s", langparser.QuoteString(stringField(row, "producedByWorkerName"))),
		// folderId is index-only, like labels: a label write that omitted it
		// would re-file the artifact at root (memql#4781, the #4288 class).
		fmt.Sprintf("folderId: %s", langparser.QuoteString(stringField(row, "folderId"))),
	}
	for _, field := range artifactEnumFields {
		if v := stringField(row, field); v != "" {
			parts = append(parts, fmt.Sprintf("%s: %s", field, langparser.QuoteString(v)))
		}
	}
	q := fmt.Sprintf("mutation createArtifact(%s)", strings.Join(parts, ", "))
	return i.writeArtifactIndex(ctx, q)
}

// writeArtifactIndex is the one seam every Go writer of the artifact index goes
// through. It stamps nothing: createArtifact is deliberately NOT @serverOnly.
//
// That was tried and reverted (memql#4340). Marking it @serverOnly would close
// a real hole -- it takes ownerUserId as an argument, so a client can name an
// arbitrary owner and plant a row in someone else's Library -- but every Go
// writer below runs on a REQUEST-DERIVED context (the label capabilities and
// the document edit path are reached from the portal and from agent tools), so
// satisfying the annotation would mean stamping internal origin on exactly the
// contexts component/auth/call_origin.go warns about, in a package that also
// hosts agent-callable builtins. TestOnlyAllowlistedPackagesStampInternalOrigin
// refuses that shape by default, and rightly.
//
// What makes the current arrangement safe in practice is that no caller here
// supplies ownerUserId: each reads it off a row it already loaded under the
// caller's own actor. The real fix -- promotion running AS the owner, so the
// argument disappears -- is memql#2803.
func (i *Integration) writeArtifactIndex(ctx context.Context, q string) error {
	_, err := i.engine.Execute(ctx, q)
	return err
}

// writeFileArtifact re-versions the index row for a v1:library:file, carrying
// the file's own facts plus the two INDEX-ONLY fields (labels, archived) that
// have no counterpart on the file row and that createArtifact's bare insert{}
// would otherwise erase.
//
// It lives HERE, next to the other two artifact-index writers, rather than in
// analysis.go where its only caller is. Every createArtifact statement in this
// package is in this file, which is what makes "how does this package write the
// index, and where does ownerUserId come from?" answerable by reading one file.
func (i *Integration) writeFileArtifact(ctx context.Context, sourceRef string, file map[string]any, carry artifactCarryForward) error {
	q := fmt.Sprintf(
		`mutation createArtifact(sourceConceptRef: %s, ownerUserId: %s, lens: "artifact", kind: "file", source: %s, title: %s, summary: %s, format: %s, mimeType: %s, live: false, labels: %s, archived: %t, folderId: %s, producedByWorkerId: %s, producedByWorkerName: %s)`,
		langparser.QuoteString(sourceRef),
		langparser.QuoteString(stringField(file, "ownerUserId")),
		langparser.QuoteString(fileArtifactSource(stringField(file, "source"))),
		langparser.QuoteString(fileArtifactTitle(stringField(file, "name"))),
		langparser.QuoteString(stringField(file, "summary")),
		langparser.QuoteString(fileArtifactFormat(stringField(file, "format"))),
		langparser.QuoteString(stringField(file, "mimeType")),
		quoteStringArray(carry.labels),
		carry.archived,
		langparser.QuoteString(carry.folderId),
		// The verified machine provenance travels on the FILE row
		// (memql#4781, design D5); re-sending it here keeps the analysis
		// re-stamp from erasing what the promotion wrote. Read from the
		// file rather than carried from the index because the file row is
		// the verified source of the fact.
		langparser.QuoteString(stringField(file, "uploadedFromWorkerId")),
		langparser.QuoteString(stringField(file, "uploadedFromWorkerName")),
	)
	return i.writeArtifactIndex(ctx, q)
}

// mergeLabelAdd returns labels with label appended if absent. Idempotent:
// a label already present is returned unchanged (changed=false), so a
// caller can skip the write-back entirely on a no-op retry or double-click.
// Compares trimmed so a label written whitespace-padded through some
// OTHER path (createArtifact directly, an automation) is still
// recognised as the same label -- mirrors mergeLabelRemove.
func mergeLabelAdd(labels []string, label string) (merged []string, changed bool) {
	for _, l := range labels {
		if strings.TrimSpace(l) == label {
			return labels, false
		}
	}
	// slices.Clone rather than make([]string, 0, len(labels)+1): CodeQL's
	// go/allocation-size-overflow traces a taint path from the caller's
	// label set to that capacity arithmetic. The overflow is not reachable
	// -- it needs len(labels) at MaxInt, which is one API call per label --
	// but this repo's code-scanning dashboard is at zero open alerts, and a
	// dashboard nobody trusts is worth less than the arithmetic saved.
	return append(slices.Clone(labels), label), true
}

// mergeLabelRemove returns labels with label dropped if present.
// Idempotent: a label already absent is returned unchanged (changed=false).
// Compares trimmed: handleRemoveArtifactLabel trims the caller's label
// before calling this, but a STORED label can carry whitespace padding if
// it was written through some other path (createArtifact directly, an
// automation) that never ran it through mergeLabelAdd's own trim -- an
// exact-match compare would then make that label permanently
// unremovable through this capability.
func mergeLabelRemove(labels []string, label string) (remaining []string, changed bool) {
	found := false
	out := make([]string, 0, len(labels))
	for _, l := range labels {
		if strings.TrimSpace(l) == label {
			found = true
			continue
		}
		out = append(out, l)
	}
	if !found {
		return labels, false
	}
	return out, true
}

func wrapLabelResult(r artifactLabelResult) ([]memorynodes.MemoryNode, error) {
	payload, err := json.Marshal(r)
	if err != nil {
		return nil, fmt.Errorf("library: marshal label result: %w", err)
	}
	return []memorynodes.MemoryNode{{
		ID:        fmt.Sprintf("library:label:%s:%d", r.ArtifactId, time.Now().UnixNano()),
		Concept:   resultConcept,
		Type:      memorynodes.NodeTypeObject,
		CreatedAt: time.Now().UTC(),
		Payload:   payload,
	}}, nil
}

// appendArgs bundles the documentVersion append fields.
//
// No ownerUserId: appendDocumentVersion stamps it from actor.userId
// (memql#2989). Both callers run the append under the backing
// document's owner (`ownerCtx`), which is where that value now comes
// from, and both refuse to proceed when the document has no owner --
// withUserActor returns ctx UNCHANGED for a blank owner, so without
// that guard the version would be attributed to the inbound caller.
type appendArgs struct {
	versionId        string
	documentId       string
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
		`mutation appendDocumentVersion(versionId: %s, documentId: %s, versionNumber: %d, content: %s, attachmentId: %s, authorKind: %s, authorId: %s, note: %s, parentVersionId: %s, producedByPlanId: %s, partitionId: %s)`,
		langparser.QuoteString(a.versionId), langparser.QuoteString(a.documentId), a.versionNumber, langparser.QuoteString(a.content), langparser.QuoteString(a.attachmentId),
		langparser.QuoteString(a.authorKind), langparser.QuoteString(a.authorId), langparser.QuoteString(a.note), langparser.QuoteString(a.parentVersionId), langparser.QuoteString(a.producedByPlanId), langparser.QuoteString(a.partitionId),
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
	// ownerUserId is NOT passed: updateGeneratedOutputContent stamps it
	// from actor.userId (memql#2989). Both callers pass ownerCtx, built
	// with withUserActor from the row's own ownerUserId after refusing to
	// proceed on a blank one, so the re-inserted row keeps the same owner.
	q := fmt.Sprintf(
		`mutation updateGeneratedOutputContent(outputId: %s, title: %s, summary: %s, body: %s, attachmentId: %s, format: %s, mimeType: %s, source: %s, partitionId: %s, producedByPlanId: %s, producedByAgentId: %s)`,
		langparser.QuoteString(stringField(doc, "id")),
		langparser.QuoteString(stringField(doc, "title")),
		langparser.QuoteString(stringField(doc, "summary")),
		langparser.QuoteString(content),
		langparser.QuoteString(attachmentId),
		langparser.QuoteString(format),
		langparser.QuoteString(stringField(doc, "mimeType")),
		langparser.QuoteString(source),
		langparser.QuoteString(stringField(doc, "partitionId")),
		langparser.QuoteString(stringField(doc, "producedByPlanId")),
		langparser.QuoteString(stringField(doc, "producedByAgentId")),
	)
	_, err := i.engine.Execute(ctx, q)
	return err
}

// touchArtifact re-stamps the artifact index row so its updatedAt
// watermark advances after an edit. Best-effort: the version + backing
// content are the source of truth; a failed index re-stamp just leaves
// the Library sort key stale until the next promotion. sourceConceptRef
// is the canonical 'v1:library:generatedOutput:<id>' form.
//
// createArtifact's insert body is a bare `insert{}` block, so MemQL's
// insert-versioning means the re-versioned row carries only the fields
// THIS call names (D3 / memql artifacts-labels spec). That makes every
// INDEX-ONLY field -- one that lives on the artifact row and has no
// counterpart on the backing generatedOutput -- a field this call has to
// read back and re-send, or a document edit silently erases it.
//
// There are two such fields now, and the second one is the point of this
// comment. `labels` was the first (memql#4288). `archived` is the second
// (memql#4340): it is the soft delete, and re-versioning without it would
// UN-ARCHIVE an artifact the owner had thrown away, as a side effect of
// editing the document behind it. That is the #4288 hazard arriving with
// a new field, which is exactly what it was predicted to do -- so the
// carry-forward is read as a SET (currentArtifactCarryForward) rather
// than as one named field, and adding a third index-only field means
// adding it there rather than discovering this comment later.
//
// The lookup is best-effort like the rest of this function -- EXCEPT for
// the specific failure mode of losing that set. currentArtifactCarryForward
// distinguishes "read failed" from "no row yet" / "row carries nothing":
// only the latter two legitimately carry forward as empty. On a genuine
// read failure this function skips the re-stamp ENTIRELY rather than risk
// writing `labels: []` and `archived: false` over whatever the row
// actually holds -- a stale updatedAt watermark is the documented
// best-effort price of a failed re-stamp; destroying real labels, or
// resurrecting an archived row, is not.
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
	carry, ok := i.currentArtifactCarryForward(ctx, sourceRef)
	if !ok {
		return
	}
	q := fmt.Sprintf(
		`mutation createArtifact(sourceConceptRef: %s, ownerUserId: %s, lens: "artifact", kind: "generated_output", source: %s, title: %s, summary: %s, format: %s, mimeType: %s, partitionId: %s, producedByPlanId: %s, labels: %s, archived: %t, folderId: %s)`,
		langparser.QuoteString(sourceRef),
		langparser.QuoteString(stringField(doc, "ownerUserId")),
		langparser.QuoteString(source),
		langparser.QuoteString(stringField(doc, "title")),
		langparser.QuoteString(stringField(doc, "summary")),
		langparser.QuoteString(stringField(doc, "format")),
		langparser.QuoteString(stringField(doc, "mimeType")),
		langparser.QuoteString(stringField(doc, "partitionId")),
		langparser.QuoteString(stringField(doc, "producedByPlanId")),
		quoteStringArray(carry.labels),
		carry.archived,
		langparser.QuoteString(carry.folderId),
	)
	_ = i.writeArtifactIndex(ctx, q)
}

// artifactCarryForward is the set of INDEX-ONLY artifact fields a
// re-version has to re-send: fields that live on the artifact row and have
// no counterpart on the backing document, so createArtifact's bare
// `insert{}` would drop them.
//
// A struct rather than a widening return list because the set grows -- it
// went from one member to two between memql#4288 and memql#4340 -- and a
// third member must land here, in one place, rather than as a fourth
// return value some caller forgets to thread.
type artifactCarryForward struct {
	labels   []string
	archived bool
	// folderId is the third member (memql#4781), exactly as this struct's
	// comment predicted a third would land: the Files app's filing lives on
	// the INDEX row only, so a re-version that omits it silently re-files
	// the artifact at root as a side effect of an edit or an analysis
	// re-stamp.
	folderId string
}

// currentArtifactCarryForward reads the CURRENT artifact index row's
// index-only fields for a source ref, so touchArtifact can carry them
// forward into its re-version call. Resolves the row via
// libraryArtifactBySourceConceptRef -- a declared-payload-field filter,
// not a Go-side re-derivation of createArtifact's hash-based id, so a
// future change to that DSL expression cannot silently reopen this exact
// lookup.
//
// ok=false is a GENUINE read failure -- the caller must not treat that as
// "carries nothing" (see touchArtifact). ok=true with a zero-valued result
// covers both legitimate cases: no row promoted yet, and a row that has
// never been labelled or archived.
func (i *Integration) currentArtifactCarryForward(ctx context.Context, sourceRef string) (artifactCarryForward, bool) {
	q := fmt.Sprintf(`query libraryArtifactBySourceConceptRef(sourceConceptRef: %s)`, langparser.QuoteString(sourceRef))
	raw, err := i.engine.Execute(ctx, q)
	if err != nil {
		return artifactCarryForward{}, false
	}
	rows := extractRows(raw)
	if len(rows) == 0 {
		return artifactCarryForward{}, true
	}
	return artifactCarryForward{
		labels:   stringSliceField(rows[0], "labels"),
		archived: boolField(rows[0], "archived"),
		folderId: stringField(rows[0], "folderId"),
	}, true
}

// loadGeneratedOutput reads the backing document row. Runs under a
// system actor for the lookup (the row may be needed before the owner
// is known); the actual edit writes run under the owner actor.
func (i *Integration) loadGeneratedOutput(ctx context.Context, documentId string) (map[string]any, error) {
	q := fmt.Sprintf(`query generatedOutputById(outputId: %s)`, langparser.QuoteString(documentId))
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
	q := fmt.Sprintf(`query documentVersionsForOwner(documentId: %s)`, langparser.QuoteString(documentId))
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
	return auth.ContextWithUserActor(ctx, ownerUserId)
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

// stringSliceField coerces a row's array field (e.g. artifact.labels) to
// []string. The engine's AsMap() round-trip (structpb -> Go) decodes a
// JSONB string array as []any with each element a string; the []string
// case is a defensive fallback for a caller that already holds native Go
// values (e.g. a test stub). Returns nil for an absent/wrong-typed field --
// callers treat that the same as "no labels".
func stringSliceField(m map[string]any, key string) []string {
	if m == nil {
		return nil
	}
	switch v := m[key].(type) {
	case []string:
		return v
	case []any:
		out := make([]string, 0, len(v))
		for _, item := range v {
			if s, ok := item.(string); ok {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

// boolField coerces a row field to bool. Missing/wrong-typed is false.
func boolField(m map[string]any, key string) bool {
	if m == nil {
		return false
	}
	b, _ := m[key].(bool)
	return b
}

// quoteStringArray renders a []string as a MemQL array-literal call
// argument value (`["a", "b"]`) -- the form parseValue/parseArray accept in
// a named call argument position (e.g. `labels: ["a", "b"]`). Every element
// is quoted through langparser.QuoteString, exactly like every neighbouring
// string field in this file; never hand-interpolated. A nil/empty slice
// renders as `[]`, the empty-array literal.
func quoteStringArray(items []string) string {
	parts := make([]string, len(items))
	for idx, item := range items {
		parts[idx] = langparser.QuoteString(item)
	}
	return "[" + strings.Join(parts, ", ") + "]"
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
//
// The two answers differ on purpose. An int64 SATURATES because its callers
// are version ordinals (expectedVersion, versionNumber) and an ordering that
// inverts is worse than one that stops; a float ZEROES because there is no
// fractional version and a caller sending one is already outside the
// contract.
func intArg(v any) (int, bool) {
	switch t := v.(type) {
	case int:
		return t, true
	case int64:
		return num.ClampInt64(t), true
	case float64:
		return num.Float64OrZero(t), true
	case float32:
		return num.Float64OrZero(float64(t)), true
	case json.Number:
		// Prefer the integer parse so a large whole number keeps full
		// precision; the int64 result is bound-checked below. Fall back
		// to the float parse for fractional values.
		if i, err := t.Int64(); err == nil {
			return num.ClampInt64(i), true
		}
		if f, err := t.Float64(); err == nil {
			return num.Float64OrZero(f), true
		}
	}
	return 0, false
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
