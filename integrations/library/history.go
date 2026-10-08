package library

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	langparser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/integrations/library/versionstore"
)

// The DSL owns reads, writes and editorial policy. This boundary handles exact
// byte snapshots, cursor arithmetic and durable, cross-replica branch identity.
func (i *Integration) historyRows(ctx context.Context, name string, args map[string]any) ([]map[string]any, error) {
	if value, ok := args["documentId"].(string); ok {
		args["documentId"] = memql.BareShortId(value)
		args["documentIdAlias"] = "v1:library:generatedOutput:" + memql.BareShortId(value)
	}
	call, err := langparser.RenderCall(name, args)
	if err != nil {
		return nil, err
	}
	result, err := i.engine.Execute(ctx, "query "+call)
	if err != nil {
		return nil, err
	}
	return extractRows(result), nil
}
func initialVersionID(document string) string { return "initial-" + memql.BareShortId(document) }
func (i *Integration) initialVersion(ctx context.Context, document string) (map[string]any, error) {
	rows, err := i.historyRows(ctx, "libraryDocumentVersionByNumber", map[string]any{"documentId": document, "versionNumber": 0})
	if err != nil || len(rows) == 0 {
		return nil, err
	}
	return rows[0], nil
}

// Called under the document write gate, before the first edit. Existing history
// is never fabricated: documents edited before this feature keep only retained versions.
func (i *Integration) ensureInitialVersion(ctx context.Context, doc, latest map[string]any) (map[string]any, error) {
	if latest != nil {
		return latest, nil
	}
	args := map[string]any{"versionId": initialVersionID(stringField(doc, "id")), "documentId": memql.BareShortId(stringField(doc, "id")), "versionNumber": 0, "versionAt": timestampField(doc, "createdAt"), "content": stringField(doc, "body"), "attachmentId": stringField(doc, "attachmentId"), "authorKind": "system", "note": "Initial snapshot", "producedByRunId": stringField(doc, "producedByRunId"), "partitionId": stringField(doc, "partitionId")}
	if _, err := versionstore.Append(ctx, i.engine, args); err != nil {
		return nil, err
	}
	args["id"] = args["versionId"]
	args["createdAt"] = args["versionAt"]
	return args, nil
}
func historyTime(row map[string]any) string {
	for _, key := range []string{"versionUploadedAt", "uploadedAt", "createdAt"} {
		if value := timestampField(row, key); value != "" {
			return value
		}
	}
	return ""
}
func historyEntry(row map[string]any, version int, revision string) map[string]any {
	return map[string]any{"version": version, "revision": revision, "createdAt": historyTime(row), "note": stringField(row, "note"), "authorKind": stringField(row, "authorKind"), "authorId": stringField(row, "authorId"), "producedByRunId": stringField(row, "producedByRunId")}
}
func (i *Integration) handleDocumentHistory(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	ctx = memql.ContextWithFreshRead(ctx)
	doc, err := i.reviewDocument(ctx, asString(args["artifactId"]))
	if err != nil {
		return nil, err
	}
	before := doc.version + 1
	if value, exists := args["beforeVersion"]; exists && value != nil {
		n, ok := intArg(value)
		if !ok || n < 0 {
			return nil, fmt.Errorf("invalid history cursor")
		}
		before = min(before, n)
	}
	entries := []map[string]any{}
	if before > doc.version {
		metadata := doc.backing
		if doc.kind == "generated_output" {
			snapshots, e := i.historyRows(ctx, "libraryDocumentVersionByNumber", map[string]any{"documentId": doc.source, "versionNumber": doc.version})
			if e != nil {
				return nil, e
			}
			if len(snapshots) == 1 {
				metadata = snapshots[0]
			}
		}
		head := historyEntry(metadata, doc.version, doc.revision)
		head["current"] = true
		entries = append(entries, head)
	}
	name, key := "libraryDocumentHistoryPage", "documentId"
	if doc.kind == "file" {
		name, key = "libraryFileHistoryPage", "fileId"
	}
	rows, err := i.historyRows(ctx, name, map[string]any{key: doc.source, "beforeVersion": min(before, doc.version)})
	if err != nil {
		return nil, err
	}
	next := 0
	for _, row := range rows {
		n, _ := intArg(row["versionNumber"])
		rev := timestampField(row, "createdAt")
		if doc.kind == "file" {
			rev = fmt.Sprintf("file:%d", n)
		}
		entries = append(entries, historyEntry(row, n, rev))
		next = n
	}
	root := doc.artifact
	var origin map[string]any
	if doc.kind == "generated_output" {
		origin, err = i.initialVersion(ctx, doc.source)
		if err != nil {
			return nil, err
		}
		if value := stringField(origin, "branchRootArtifactId"); value != "" {
			root = value
		}
	}
	branches := []map[string]any{}
	seeds, err := i.historyRows(ctx, "libraryDocumentBranches", map[string]any{"rootArtifactId": root})
	if err != nil {
		return nil, err
	}
	for _, seed := range seeds {
		refs, readErr := i.historyRows(ctx, "libraryArtifactBySourceConceptRef", map[string]any{"sourceConceptRef": "v1:library:generatedOutput:" + memql.BareShortId(stringField(seed, "documentId"))})
		if readErr != nil {
			return nil, readErr
		}
		if len(refs) != 1 || boolField(refs[0], "archived") {
			continue
		}
		branches = append(branches, map[string]any{"artifactId": memql.BareShortId(stringField(refs[0], "id")), "name": stringField(seed, "branchName")})
	}
	return reviewResult(map[string]any{"versions": entries, "hasMore": len(rows) == 50, "beforeVersion": next, "currentVersion": doc.version, "rootArtifactId": root, "parentArtifactId": stringField(origin, "branchParentArtifactId"), "parentVersion": origin["branchParentVersion"], "branchName": stringField(origin, "branchName"), "branches": branches, "branchesHasMore": len(seeds) == 100})
}
func (i *Integration) historicalContent(ctx context.Context, doc reviewDocument, number int) (string, string, error) {
	if number < 0 || number > doc.version {
		return "", "", fmt.Errorf("this version is unavailable")
	}
	row := doc.backing
	revision := doc.revision
	bodyKey := "body"
	if number != doc.version {
		name, key := "libraryDocumentVersionByNumber", "documentId"
		bodyKey = "content"
		if doc.kind == "file" {
			name, key = "libraryFileVersionByNumber", "fileId"
		}
		rows, err := i.historyRows(ctx, name, map[string]any{key: doc.source, "versionNumber": number})
		if err != nil {
			return "", "", err
		}
		if len(rows) != 1 {
			return "", "", fmt.Errorf("this version was not retained")
		}
		row = rows[0]
		revision = timestampField(row, "createdAt")
		if doc.kind == "file" {
			revision = fmt.Sprintf("file:%d", number)
		}
	}
	const limit = 2 * 1024 * 1024
	var content []byte
	if doc.kind == "file" {
		if i.blobFetcher == nil {
			return "", "", fmt.Errorf("document storage is unavailable on this node")
		}
		stream, err := i.blobFetcher.DownloadStreamURL(ctx, stringField(row, "blobUrl"))
		if err != nil {
			return "", "", fmt.Errorf("could not read this saved version")
		}
		defer stream.Close()
		content, err = io.ReadAll(io.LimitReader(stream, limit+1))
		if err != nil {
			return "", "", err
		}
	} else {
		content = []byte(stringField(row, bodyKey))
	}
	if len(content) > limit || !utf8.Valid(content) {
		return "", "", fmt.Errorf("version preview supports UTF-8 Markdown up to 2 MiB")
	}
	return string(content), revision, nil
}
func (i *Integration) handleDocumentVersion(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	n, ok := intArg(args["versionNumber"])
	if !ok || n < 0 {
		return nil, fmt.Errorf("choose a saved version")
	}
	ctx = memql.ContextWithFreshRead(ctx)
	doc, release, err := i.lockReviewDocument(ctx, asString(args["artifactId"]))
	if err != nil {
		return nil, err
	}
	defer release()
	content, revision, err := i.historicalContent(ctx, doc, n)
	if err != nil {
		return nil, err
	}
	return reviewResult(map[string]any{"version": n, "revision": revision, "content": content})
}
func (i *Integration) handleForkDocumentVersion(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	if err := revisionWriter(ctx); err != nil {
		return nil, err
	}
	number, ok := intArg(args["versionNumber"])
	name := strings.TrimSpace(asString(args["name"]))
	request := asString(args["requestId"])
	revision := asString(args["revision"])
	if !ok || number < 0 || name == "" || len(name) > 128 || (strings.ContainsAny(name, "/\\") || strings.IndexFunc(name, unicode.IsControl) >= 0) || !reviewRequestID.MatchString(request) || revision == "" {
		return nil, fmt.Errorf("choose a version and a branch name of up to 128 characters")
	}
	ctx = memql.ContextWithFreshRead(ctx)
	parent, err := i.reviewDocument(ctx, asString(args["artifactId"]))
	if err != nil {
		return nil, err
	}
	ac, _ := auth.AccessFromContext(ctx)
	output := fmt.Sprintf("branch-%x", sha256.Sum256([]byte(ac.UserId+"\x00"+request)))
	release, err := i.versionGate(ctx, output)
	if err != nil {
		return nil, err
	}
	defer release()
	seed, err := i.initialVersion(ctx, output)
	if err != nil {
		return nil, err
	}
	if seed != nil {
		n, _ := intArg(seed["branchParentVersion"])
		if stringField(seed, "branchParentArtifactId") != parent.artifact || n != number || stringField(seed, "branchParentRevision") != revision || stringField(seed, "branchName") != name {
			return nil, fmt.Errorf("this branch request already belongs to another version or name")
		}
	} else {
		var unlock func()
		parent, unlock, err = i.lockReviewDocument(ctx, parent.artifact)
		if err != nil {
			return nil, err
		}
		defer unlock()
		content, actual, readErr := i.historicalContent(ctx, parent, number)
		if readErr != nil {
			return nil, readErr
		}
		if actual != revision {
			return nil, fmt.Errorf("the selected version changed; open it again before branching")
		}
		root := parent.artifact
		if parent.kind == "generated_output" {
			origin, e := i.initialVersion(ctx, parent.source)
			if e != nil {
				return nil, e
			}
			if value := stringField(origin, "branchRootArtifactId"); value != "" {
				root = value
			}
		}
		at := time.Now().UTC().Format(time.RFC3339Nano)
		_, err = versionstore.Append(ctx, i.engine, map[string]any{"versionId": initialVersionID(output), "documentId": output, "versionNumber": 0, "versionAt": at, "content": content, "authorKind": "system", "note": fmt.Sprintf("Branched from v%d", number), "branchRootArtifactId": root, "branchParentArtifactId": parent.artifact, "branchParentVersion": number, "branchParentRevision": revision, "branchName": name})
		if err != nil {
			return nil, err
		}
		seed, err = i.initialVersion(ctx, output)
		if err != nil {
			return nil, err
		}
		if seed == nil {
			return nil, fmt.Errorf("branch snapshot was not confirmed; retry this request")
		}
	}
	// The immutable seed is also a recovery receipt. Once a branch has advanced,
	// replaying its creation must never overwrite its new head.
	backing, err := i.loadGeneratedOutput(ctx, output)
	if err != nil {
		return nil, err
	}
	if backing == nil {
		backing = map[string]any{"id": output, "ownerUserId": parent.owner, "title": name, "format": "markdown", "mimeType": "text/markdown", "source": "derived"}
		if err = i.updateBackingContent(ctx, backing, stringField(seed, "content"), "", timestampField(seed, "createdAt")); err != nil {
			return nil, err
		}
	}
	i.touchArtifact(ctx, backing)
	refs, err := i.historyRows(ctx, "libraryArtifactBySourceConceptRef", map[string]any{"sourceConceptRef": "v1:library:generatedOutput:" + output})
	if err != nil {
		return nil, err
	}
	if len(refs) != 1 || boolField(refs[0], "archived") {
		return nil, fmt.Errorf("branch could not be opened; retry to recover it")
	}
	return reviewResult(map[string]any{"artifactId": memql.BareShortId(stringField(refs[0], "id")), "name": name, "documentId": output})
}
