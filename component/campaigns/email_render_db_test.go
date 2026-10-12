package campaigns

import (
	"context"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/compose/emaildoc"
	langparser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/memql"
)

// email_render_db_test.go -- the emailRender builtin against real engines: what
// email_render_test.go's fake can only imitate.
//
// A picture in a preview is found by two Library reads that gate on the file's
// owner in the query itself, and the organization is decided by the engine's
// grants. A preview is an act of the person who asks for it, so the proof that
// matters is the one the fake cannot give: that the person who uploaded a
// picture sees it, and that nobody else does, not a colleague in the same
// organization and not the owner of the cluster, because every read ran as the
// caller. It also runs the builtin as a client calls it, so a mismatch between
// the DSL declaration and the handler (the executor's name, the arguments, the
// shape of the answer) fails here.

func TestEmailRenderBuiltinReadsPicturesAsTheCallerThroughTheEngine(t *testing.T) {
	w := newLayoutWorld(t)
	previousGrants, previousMembers := auth.InstalledGrantSource(), auth.InstalledMembershipSource()
	w.engines[0].InstallGrantResolution()
	t.Cleanup(func() { auth.SetGrantSource(previousGrants); auth.SetMembershipSource(previousMembers) })

	account := w.suffix + "-org"
	w.createOrganization(t, account, "Preview client")
	uploaderID, colleagueID, outsiderID := w.suffix+"-uploader", w.suffix+"-colleague", w.suffix+"-outsider"
	for _, user := range []string{uploaderID, colleagueID, outsiderID} {
		w.seedUser(t, user, "writer")
	}
	w.seedMember(t, uploaderID, account)
	w.seedMember(t, colleagueID, account)
	uploader, colleague, outsider := layoutActor(uploaderID, auth.RoleWriter), layoutActor(colleagueID, auth.RoleWriter), layoutActor(outsiderID, auth.RoleWriter)

	// An upload, as the uploader's own act: the bytes in storage, the file row
	// and the Library's index row for it.
	data := previewPNG(t, 6, 4)
	file := w.suffix + "-file"
	blobURL := "library/" + uploaderID + "/" + file + "/logo.png"
	blobs := &recordingBlobs{objects: map[string][]byte{blobURL: data}}
	for _, worker := range w.workers {
		worker.newBlobReader = func(context.Context) (blobReader, string, error) { return blobs, "memql", nil }
	}
	w.run(t, uploader, call("mutation", "createLibraryFile", arg{"fileId", file}, arg{"name", "logo.png"}, arg{"mimeType", "image/png"},
		arg{"size", len(data)}, arg{"blobUrl", blobURL}, arg{"source", "uploaded"}, arg{"format", "image"}))
	created, ok := w.run(t, uploader, call("mutation", "createArtifact", arg{"sourceConceptRef", "v1:library:file:" + file}, arg{"ownerUserId", uploaderID},
		arg{"lens", "artifact"}, arg{"kind", "file"}, arg{"source", "uploaded"}, arg{"title", "logo.png"}, arg{"format", "image"}, arg{"mimeType", "image/png"})).(*memql.ExecuteResult)
	if !ok || created.Bundle == nil || len(created.Bundle.Nodes) == 0 {
		t.Fatal("createArtifact answered no row")
	}
	artifact := bare(created.Bundle.Nodes[0].Id)

	doc := layoutDocument(t, "builtin:letter")
	setBlock(t, doc, "logo", func(b map[string]any) { b["src"] = "artifact:" + artifact })
	previewOn := func(replica int, ctx context.Context, organization string) (map[string]any, error) {
		rendered, err := langparser.RenderCall("emailRender", map[string]any{
			"document": doc, "accountId": organization,
			"sample": map[string]any{"displayName": "Ada", "fields": map[string]any{"plan": "pro", "seats": 5}},
		})
		if err != nil {
			t.Fatal(err)
		}
		// The engine's cache is per engine and nothing carries one's evictions to
		// the other: read what the database says.
		res, err := w.engines[replica].Execute(memql.ContextWithFreshRead(ctx), "builtin "+rendered)
		if err != nil {
			return nil, err
		}
		rows := memql.MaterializeRows(res)
		if len(rows) != 1 {
			t.Fatalf("emailRender answered %d rows: %v", len(rows), rows)
		}
		return rows[0], nil
	}
	preview := func(ctx context.Context, organization string) (map[string]any, error) {
		return previewOn(0, ctx, organization)
	}
	issueFor := func(row map[string]any, code, block string) bool {
		issues, _ := row["issues"].([]any)
		for _, item := range issues {
			if issue, _ := item.(map[string]any); issue["code"] == code && issue["block"] == block {
				return true
			}
		}
		return false
	}

	t.Run("the person who uploaded the picture sees it", func(t *testing.T) {
		row, err := preview(uploader, account)
		if err != nil {
			t.Fatal(err)
		}
		html, _ := row["html"].(string)
		if !strings.Contains(html, "data:image/png;base64,") || !strings.Contains(html, `data-block="logo"`) {
			t.Fatalf("the picture was not embedded in the preview:\n%.500s", html)
		}
		if issueFor(row, emaildoc.IssueImageUnavailable, "logo") || issueFor(row, emaildoc.IssueImageMissing, "logo") {
			t.Errorf("the logo has an issue although its picture was read: %v", row["issues"])
		}
		if row["subject"] != "A note from our founder" {
			t.Errorf("subject = %v", row["subject"])
		}
		if text, _ := row["text"].(string); strings.TrimSpace(text) == "" {
			t.Error("the plain-text part is empty")
		}
	})

	// Any replica answers alike: nothing a preview needs lives on the node that
	// first saw the upload.
	t.Run("the other replica answers the same", func(t *testing.T) {
		row, err := previewOn(1, uploader, account)
		if err != nil {
			t.Fatal(err)
		}
		if html, _ := row["html"].(string); !strings.Contains(html, "data:image/png;base64,") {
			t.Fatalf("the picture was not embedded by the other replica:\n%.500s", html)
		}
		other, err := previewOn(1, colleague, account)
		if err != nil {
			t.Fatal(err)
		}
		if html, _ := other["html"].(string); strings.Contains(html, "data:image") {
			t.Fatal("the other replica embedded somebody else's picture")
		}
	})

	// Nobody else: not a colleague in the same organization, who may preview its
	// emails, and not the owner of the cluster, who may do almost anything. The
	// Library says a file is its uploader's, and every read ran as the caller.
	for name, ctx := range map[string]context.Context{"a colleague in the same organization": colleague, "the owner of the cluster": w.owner} {
		t.Run(name+" does not", func(t *testing.T) {
			before, _ := blobs.reads()
			row, err := preview(ctx, account)
			if err != nil {
				t.Fatal(err)
			}
			if html, _ := row["html"].(string); strings.Contains(html, "data:image") {
				t.Fatal("somebody else's picture was embedded in the preview")
			}
			if !issueFor(row, emaildoc.IssueImageUnavailable, "logo") {
				t.Errorf("the logo is not reported as unavailable: %v", row["issues"])
			}
			if after, _ := blobs.reads(); after != before {
				t.Error("storage was read for a picture the caller cannot see")
			}
		})
	}

	t.Run("a person outside the organization is refused", func(t *testing.T) {
		before, _ := blobs.reads()
		if _, err := preview(outsider, account); err == nil || !strings.Contains(err.Error(), "organization is not active or accessible") {
			t.Fatalf("an outsider's preview: %v", err)
		}
		if after, _ := blobs.reads(); after != before {
			t.Error("storage was read for someone outside the organization")
		}
	})

	t.Run("an organization that does not exist is refused", func(t *testing.T) {
		if _, err := preview(uploader, w.suffix+"-nowhere"); err == nil || !strings.Contains(err.Error(), "organization is not active or accessible") {
			t.Fatalf("a preview for a made-up organization: %v", err)
		}
	})
}
