package library

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/pgdialect"
	"github.com/uptrace/bun/driver/pgdriver"
	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/database/dbtest"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	langparser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/memql"
)

func TestReviewCommentsAcrossReplicasAndDocumentRevisions(t *testing.T) {
	reachable, err := dbtest.EnsureSchema(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !reachable {
		dbtest.Unreachable(t, "document reviews", dbtest.DSN(), nil)
		return
	}
	if _, err := memql.LoadUnifiedConcepts(nil); err != nil {
		t.Fatal(err)
	}
	open := func() (*Integration, *memql.MemQLEngine) {
		db := bun.NewDB(sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(dbtest.DSN()))), pgdialect.New())
		t.Cleanup(func() { _ = db.Close() })
		engine, err := memql.New(db)
		if err != nil {
			t.Fatal(err)
		}
		if err = engine.Init(memorynodes.DefaultRegistry()); err != nil {
			t.Fatal(err)
		}
		return NewIntegration(engine, func() *sql.DB { return db.DB }), engine
	}
	first, engine := open()
	second, _ := open()
	documentID := fmt.Sprintf("review-%d", time.Now().UnixNano())
	owner := documentID + "-owner"
	actor := func(user string, role auth.Role) context.Context {
		return auth.ContextWithAccess(auth.ContextWithToken(context.Background(), &auth.TokenInfo{Subject: user}), &auth.AccessContext{UserId: user, Role: role})
	}
	ctx := actor(owner, auth.RoleWriter)
	_, err = engine.Execute(ctx, fmt.Sprintf(`mutation createGeneratedOutput(outputId: %s, title: "Markdown", body: "# Heading\n\nA paragraph.", format: "markdown", source: "user_created")`, langparser.QuoteString(documentID)))
	if err != nil {
		t.Fatal(err)
	}
	raw, err := engine.Execute(ctx, fmt.Sprintf(`mutation createArtifact(sourceConceptRef: %s, ownerUserId: %s, lens: "artifact", kind: "generated_output", source: "user_created", title: "Markdown", format: "markdown")`, langparser.QuoteString(documentID), langparser.QuoteString(owner)))
	if err != nil {
		t.Fatal(err)
	}
	artifacts := extractRows(raw)
	if len(artifacts) != 1 {
		t.Fatalf("artifact seed: %#v", artifacts)
	}
	artifactID := stringField(artifacts[0], "id")
	base, err := first.reviewDocument(ctx, artifactID)
	if err != nil {
		t.Fatal(err)
	}
	args := map[string]any{"artifactId": artifactID, "expectedVersion": 0, "expectedRevision": base.revision, "body": "Please clarify this paragraph.", "requestId": "same-review-request", "anchor": map[string]any{"kind": "markdown", "startLine": 2, "endLine": 3, "sourceQuote": "A paragraph.", "quote": "paragraph"}}
	// Two processes have separate caches and one shared DB lock. Retrying the
	// same submission on another replica must write exactly one comment.
	var wait sync.WaitGroup
	failures := make(chan error, 2)
	for _, replica := range []*Integration{first, second} {
		wait.Add(1)
		go func(i *Integration) {
			defer wait.Done()
			_, err := i.handleAddDocumentComment(ctx, args, 0)
			failures <- err
		}(replica)
	}
	wait.Wait()
	close(failures)
	for err := range failures {
		if err != nil {
			t.Fatal(err)
		}
	}
	review := func() []map[string]any {
		nodes, err := second.handleDocumentReview(ctx, map[string]any{"artifactId": artifactID}, 0)
		if err != nil {
			t.Fatal(err)
		}
		result := extractRows(map[string]memorynodes.MemoryNode{"result": nodes[0]})[0]
		comments, _ := result["comments"].([]any)
		out := make([]map[string]any, 0, len(comments))
		for _, row := range comments {
			out = append(out, row.(map[string]any))
		}
		return out
	}
	rows := review()
	if len(rows) != 1 || rows[0]["outdated"] != false || rows[0]["authorUserId"] != owner {
		t.Fatalf("saved review: %#v", rows)
	}
	// The source moved. Its old comments remain readable and explicitly old.
	if _, err = second.handleEditDocument(ctx, map[string]any{"documentId": documentID, "content": "# Heading\n\nChanged.", "expectedVersion": 0, "expectedRevision": base.revision}, 0); err != nil {
		t.Fatal(err)
	}
	if rows = review(); len(rows) != 1 || rows[0]["outdated"] != true {
		t.Fatalf("old anchor was silently moved: %#v", rows)
	}
	if _, err = first.handleAddDocumentComment(ctx, args, 0); err != nil {
		t.Fatalf("lost response retry after edit: %v", err)
	}
	args["requestId"] = "new-stale-request"
	if _, err = first.handleAddDocumentComment(ctx, args, 0); err == nil || !strings.Contains(err.Error(), "changed") {
		t.Fatalf("stale feedback accepted: %v", err)
	}
	for _, denied := range []context.Context{actor(documentID+"-other", auth.RoleWriter), actor(owner, auth.RoleReader)} {
		if _, err = first.handleAddDocumentComment(denied, args, 0); err == nil {
			t.Fatal("unauthorized feedback was accepted")
		}
	}
	if _, err = second.handleDocumentReview(actor(documentID+"-other", auth.RoleWriter), map[string]any{"artifactId": artifactID}, 0); err == nil {
		t.Fatal("unrelated user read comments")
	}
	_, err = engine.Execute(ctx, `insert("v1:library:documentComment", id="forged-raw", payload={"body":"fake"})`)
	if err == nil || !strings.Contains(err.Error(), "internal origin") {
		t.Fatalf("raw feedback forgery accepted or not tested: %v", err)
	}
	for _, call := range []string{`query documentCommentsForArtifact(artifactId: "anything")`, `mutation appendDocumentComment(commentId: "forged")`} {
		if _, err = engine.Execute(ctx, call); err == nil || !strings.Contains(strings.ToLower(err.Error()), "server") {
			t.Fatalf("direct review store call accepted: %v", err)
		}
	}
}
