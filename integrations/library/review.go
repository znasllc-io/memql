package library

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"reflect"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	langparser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/core/id"
	"github.com/znasllc-io/memql/integrations/library/reviewstore"
)

type reviewDocument struct {
	artifact, source, kind, owner, revision string
	version                                 int
	backing                                 map[string]any
}

// Both reads retain the caller. An index label alone cannot grant access to
// private backing bytes, and a comment never grants access to either row.
func (i *Integration) reviewDocument(ctx context.Context, artifactID string) (reviewDocument, error) {
	var doc reviewDocument
	call, err := langparser.RenderCall("libraryArtifactById", map[string]any{"artifactId": artifactID})
	if err != nil {
		return doc, err
	}
	raw, err := i.engine.Execute(ctx, "query "+call)
	if err != nil {
		return doc, err
	}
	rows := extractRows(raw)
	if len(rows) != 1 || boolField(rows[0], "archived") {
		return doc, fmt.Errorf("document is unavailable")
	}
	artifact := rows[0]
	doc.artifact, doc.source, doc.kind = memql.BareShortId(stringField(artifact, "id")), stringField(artifact, "sourceConceptRef"), stringField(artifact, "kind")
	var backing map[string]any
	switch doc.kind {
	case "file":
		call, err = langparser.RenderCall("libraryFileById", map[string]any{"fileId": doc.source})
		if err != nil {
			return doc, err
		}
		raw, err = i.engine.Execute(ctx, "query "+call)
		if err != nil {
			return doc, err
		}
		rows = extractRows(raw)
		if len(rows) == 1 {
			backing = rows[0]
		}
		doc.version, _ = intArg(backing["versionNumber"])
		if doc.version < 1 {
			doc.version = 1
		}
		doc.revision = fmt.Sprintf("file:%d", doc.version)
	case "generated_output":
		backing, err = i.loadGeneratedOutput(ctx, doc.source)
		if err != nil {
			return doc, err
		}
		_, doc.version, err = i.latestVersion(ctx, doc.source)
		if err != nil {
			return doc, err
		}
		doc.revision = timestampField(backing, "createdAt")
	default:
		return doc, fmt.Errorf("feedback is available for files and generated documents")
	}
	if backing == nil || boolField(backing, "archived") {
		return doc, fmt.Errorf("document is unavailable")
	}
	doc.owner = stringField(backing, "ownerUserId")
	doc.backing = backing
	if doc.owner == "" || doc.revision == "" {
		return doc, fmt.Errorf("document revision is unavailable")
	}
	return doc, nil
}

func (i *Integration) handleDocumentReview(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	if _, ok := auth.SubjectFromContext(ctx); !ok {
		return nil, fmt.Errorf("sign in to read document feedback")
	}
	doc, err := i.reviewDocument(memql.ContextWithFreshRead(ctx), asString(args["artifactId"]))
	if err != nil {
		return nil, err
	}
	raw, err := reviewstore.List(ctx, i.engine, doc.owner, doc.artifact)
	if err != nil {
		return nil, err
	}
	rows := extractRows(raw)
	more := len(rows) > 500
	if more {
		rows = rows[:500]
	}
	for _, row := range rows {
		row["id"] = memql.BareShortId(stringField(row, "id"))
		row["authorUserId"] = memql.BareShortId(stringField(row, "authorUserId"))
		row["outdated"] = stringField(row, "revision") != doc.revision
		delete(row, "ownerUserId")
		delete(row, "requestId")
	}
	return reviewResult(map[string]any{"comments": rows, "hasMore": more, "version": doc.version, "revision": doc.revision})
}

var reviewRequestID = regexp.MustCompile(`^[a-zA-Z0-9_-]{8,120}$`)

func (i *Integration) handleAddDocumentComment(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	subject, ok := auth.SubjectFromContext(ctx)
	if !ok || !auth.CapableFor(ctx, subject, auth.VerbCreate, auth.ResourceData) {
		return nil, fmt.Errorf("you do not have permission to add feedback")
	}
	requestID, body := asString(args["requestId"]), strings.TrimSpace(asString(args["body"]))
	if !reviewRequestID.MatchString(requestID) || body == "" || len(body) > 16000 {
		return nil, fmt.Errorf("feedback requires a request identifier and 1–16000 bytes of text")
	}
	anchor, err := validateReviewAnchor(args["anchor"])
	if err != nil {
		return nil, err
	}
	ctx = memql.ContextWithFreshRead(ctx)
	doc, err := i.reviewDocument(ctx, asString(args["artifactId"]))
	if err != nil {
		return nil, err
	}
	gate := i.versionGate
	if doc.kind == "file" {
		gate = i.fileVersionGate
	}
	release, err := gate(ctx, doc.source)
	if err != nil {
		return nil, err
	}
	defer release()
	doc, err = i.reviewDocument(ctx, doc.artifact)
	if err != nil {
		return nil, err
	}
	access, _ := auth.AccessFromContext(ctx)
	if access == nil || access.UserId == "" {
		return nil, fmt.Errorf("sign in to add feedback")
	}
	author := access.UserId
	commentID := string(id.New().MustFromMap(map[string]any{"artifact": doc.artifact, "author": author, "request": requestID}))
	expected, valid := intArg(args["expectedVersion"])
	revision := asString(args["expectedRevision"])
	raw, err := reviewstore.ByID(ctx, i.engine, doc.owner, commentID)
	if err != nil {
		return nil, err
	}
	if rows := extractRows(raw); len(rows) > 0 {
		oldVersion, _ := intArg(rows[0]["versionNumber"])
		if stringField(rows[0], "body") != body || stringField(rows[0], "revision") != revision || oldVersion != expected || !reflect.DeepEqual(rows[0]["anchor"], anchor) {
			return nil, fmt.Errorf("this feedback request was already used with different content")
		}
		return reviewResult(map[string]any{"commentId": commentID, "saved": true})
	}
	if !valid || expected != doc.version || revision != doc.revision {
		return nil, fmt.Errorf("the document changed; reopen its current revision before adding feedback")
	}
	if err = i.verifyReviewPassage(ctx, doc, anchor); err != nil {
		return nil, err
	}
	_, err = reviewstore.Append(ctx, i.engine, doc.owner, map[string]any{
		"commentId": commentID, "artifactId": doc.artifact, "authorUserId": author,
		"revision": doc.revision, "versionNumber": doc.version, "anchor": anchor, "body": body, "requestId": requestID,
	})
	if err != nil {
		return nil, err
	}
	return reviewResult(map[string]any{"commentId": commentID, "saved": true})
}

func validateReviewAnchor(value any) (map[string]any, error) {
	// Normalize JSON numbers so an idempotent retry compares the persisted
	// representation, not a Go int against a decoded float64.
	bytes, err := json.Marshal(value)
	if err != nil || len(bytes) > 24000 {
		return nil, fmt.Errorf("select a shorter passage")
	}
	var anchor map[string]any
	if json.Unmarshal(bytes, &anchor) != nil || anchor["kind"] != "markdown" {
		return nil, fmt.Errorf("select a Markdown passage")
	}
	start, startOK := intArg(anchor["startLine"])
	end, endOK := intArg(anchor["endLine"])
	quote, source := asString(anchor["quote"]), asString(anchor["sourceQuote"])
	if !startOK || !endOK || anchor["startLine"] != float64(start) || anchor["endLine"] != float64(end) || start < 0 || end <= start || end > 2000000 || quote == "" || len(quote) > 8000 || source == "" || len(source) > 16000 {
		return nil, fmt.Errorf("select a shorter passage")
	}
	result := map[string]any{"kind": "markdown", "startLine": float64(start), "endLine": float64(end), "quote": quote, "sourceQuote": source}
	for _, key := range []string{"startBlock", "endBlock", "startTextOffset", "endTextOffset"} {
		if value, exists := anchor[key]; exists {
			n, ok := intArg(value)
			if !ok || n < 0 || n > 2*1024*1024 || value != float64(n) {
				return nil, fmt.Errorf("invalid rendered passage position")
			}
			result[key] = float64(n)
		}
	}
	return result, nil
}

// Check saved bytes while holding the same cross-replica version lock as file
// saves. A client-supplied revision alone cannot attest a passage. Blob URLs
// come only from the authorized backing row and use the configured store.
func (i *Integration) verifyReviewPassage(ctx context.Context, doc reviewDocument, anchor map[string]any) error {
	const maxBytes = 2 * 1024 * 1024
	var content []byte
	if doc.kind == "file" {
		if i.blobFetcher == nil {
			return fmt.Errorf("document storage is unavailable on this node")
		}
		stream, err := i.blobFetcher.DownloadStreamURL(ctx, stringField(doc.backing, "blobUrl"))
		if err != nil {
			return fmt.Errorf("could not read the saved document; try again")
		}
		defer stream.Close()
		content, err = io.ReadAll(io.LimitReader(stream, maxBytes+1))
		if err != nil {
			return fmt.Errorf("could not read the saved document; try again")
		}
	} else {
		content = []byte(stringField(doc.backing, "body"))
	}
	if len(content) > maxBytes || !utf8.Valid(content) {
		return fmt.Errorf("passage feedback requires a UTF-8 document up to 2 MiB")
	}
	// Match the editor's CRLF-aware source ranges, preserving lone CR bytes.
	lines := strings.Split(strings.ReplaceAll(string(content), "\r\n", "\n"), "\n")
	start, _ := intArg(anchor["startLine"])
	end, _ := intArg(anchor["endLine"])
	if start < 0 || end <= start || end > len(lines) || strings.Join(lines[start:end], "\n") != anchor["sourceQuote"] {
		return fmt.Errorf("the selected passage does not match the saved document; select it again")
	}
	return nil
}
func reviewResult(value map[string]any) ([]memorynodes.MemoryNode, error) {
	payload, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	return []memorynodes.MemoryNode{{ID: id.NewShortId(), Concept: resultConcept, Type: memorynodes.NodeTypeObject, CreatedAt: time.Now().UTC(), Payload: payload}}, nil
}
