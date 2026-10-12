package campaigns

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/png"
	"math"
	"math/rand"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/compose/emaildoc"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	langparser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/memql"
)

// template_test.go -- campaignSaveTemplate: an organization's email templates,
// saved, published, archived and restored from an email document.
//
// What a fake engine can prove is the handler's own rules: what a save writes
// and under which origin, which gate it holds, what it reads fresh, which
// sentences a person gets and in which order, and that a document is rendered
// as the CALLER can read its pictures. What it cannot is the part that needs a
// database -- the exact revision across two replicas, the raw-write guard, and
// the file's template found again by the real query. Those are
// template_db_test.go.
//
// The engine here keeps what the handler writes when it is asked to
// (templateEngine.stateful), so that a flow that saves, publishes, archives and
// restores reads back what it wrote, with the revision it was given. It applies
// a write the way the concept's mutations do: a field the call names replaces
// the stored one and the rest stay as stored.

const (
	templateOrg    = renderOrg
	templateSource = "art-9"

	// The sentences a person reads, spelled out here and not taken from the
	// handler so that a change of wording is a change of a test.
	chooseTemplate  = "choose an organization and a template name (up to 200 characters)"
	unknownAction   = "unknown template action"
	templateChanged = "this template changed; compare the latest revision before saving or publishing"
	unsavedTemplate = "save and review this revision before publishing"
	templateMissing = "template not found; create and save it before publishing"
	templateMoved   = "a template cannot move between organizations; create a copy instead"
	templateNeeds   = "template write permission is required in this organization"
	saveIsBusy      = "Saving is busy right now. Try again in a moment."
	heroToChoose    = `Choose a picture for "Hero picture".`
	heroUnreadable  = `The picture for "Hero picture" could not be read. Choose it again.`
	tooLargeToSend  = "This email is larger than 2 MiB once its pictures are embedded. Use fewer or smaller pictures."
)

var (
	// templateNow is the worker's clock, and templateStored the createdAt of a
	// row already saved: a revision, as templateById hands it to a client.
	templateNow    = time.Date(2029, 1, 1, 0, 0, 0, 0, time.UTC)
	templateStored = time.Date(2028, 6, 1, 12, 0, 0, 123456000, time.UTC)
)

// templateEngine is the package's fake engine plus a record of how it was
// called, an organization decision a test can set, and the owner gate of the
// Library's two named reads.
type templateEngine struct {
	*fakeEngine
	mu      sync.Mutex
	made    []layoutCall
	asked   []layoutAsk
	capable func(account, verb, resource string) bool // nil allows everything
	// pictureOwner, when set, models the owner gate of libraryArtifactById and
	// libraryFileById: only that user is shown the rows.
	pictureOwner string
	// stateful keeps what createTemplate and updateTemplate write as the stored
	// template (and as the template of its file), as the engine would.
	stateful bool
}

func (e *templateEngine) Execute(ctx context.Context, q string) (any, error) {
	e.mu.Lock()
	e.made = append(e.made, layoutCall{query: q, internal: auth.OriginFromContext(ctx).IsInternal(), fresh: memql.FreshReadFromContext(ctx)})
	e.mu.Unlock()
	switch {
	case strings.HasPrefix(q, "query templateById"):
		// The package's fake answers its one template to any id; a handler that
		// read the wrong template would not be told so.
		if row := e.fakeEngine.template; row != nil && bare(str(row, "id")) == argOf(q, "templateId") {
			return rowsEnvelope([]map[string]any{row}), nil
		}
		return rowsEnvelope(nil), nil
	case strings.HasPrefix(q, "query libraryArtifactById"), strings.HasPrefix(q, "query libraryFileById"):
		if e.pictureOwner != "" {
			if ac, _ := auth.AccessFromContext(ctx); ac == nil || ac.UserId != e.pictureOwner {
				return rowsEnvelope(nil), nil
			}
		}
	case strings.HasPrefix(q, "mutation createTemplate("), strings.HasPrefix(q, "mutation updateTemplate("):
		if e.stateful {
			e.keep(q)
		}
		return nil, nil
	}
	return e.fakeEngine.Execute(ctx, q)
}

// keep applies a rendered createTemplate or updateTemplate to the stored
// template, the way the concept's mutations do.
func (e *templateEngine) keep(q string) {
	name, args := parseRenderedCall(q)
	row := map[string]any{}
	if name == "updateTemplate" && e.fakeEngine.template != nil {
		for k, v := range e.fakeEngine.template {
			row[k] = v
		}
	} else {
		row["id"] = "v1:campaigns:template:" + fmt.Sprint(args["templateId"])
		row["ownerUserId"] = "v1:identity:user:" + testOwner
		row["accountId"] = "v1:accounts:account:" + fmt.Sprint(args["accountId"])
		row["status"] = "draft"
	}
	for _, field := range []string{"name", "subject", "textBody", "htmlBody", "preheader", "layoutId", "sourceArtifactId", "document", "status"} {
		if value, named := args[field]; named {
			row[field] = value
		}
	}
	row["createdAt"] = args["versionTime"]
	e.fakeEngine.template = row
	if source := bare(str(row, "sourceArtifactId")); source != "" {
		if e.fakeEngine.templateBySource == nil {
			e.fakeEngine.templateBySource = map[string]map[string]any{}
		}
		e.fakeEngine.templateBySource[bare(str(row, "accountId"))+"|"+source] = row
	}
}

func (e *templateEngine) OrganizationCapable(_ context.Context, account, verb, resource string) bool {
	e.mu.Lock()
	e.asked = append(e.asked, layoutAsk{account, verb, resource})
	e.mu.Unlock()
	return e.capable == nil || e.capable(account, verb, resource)
}

// calls returns the recorded calls whose query starts with prefix.
func (e *templateEngine) calls(prefix string) []layoutCall {
	e.mu.Lock()
	defer e.mu.Unlock()
	var out []layoutCall
	for _, c := range e.made {
		if strings.HasPrefix(c.query, prefix) {
			out = append(out, c)
		}
	}
	return out
}

// writes returns every mutation the handler issued.
func (e *templateEngine) writes() []layoutCall { return e.calls("mutation ") }

// parseRenderedCall reads a call langparser.RenderCall wrote back into its
// name and arguments. Every value of such a call is JSON.
func parseRenderedCall(q string) (string, map[string]any) {
	open := strings.IndexByte(q, '(')
	name := strings.TrimPrefix(strings.TrimPrefix(q[:open], "mutation "), "query ")
	rest := q[open+1:]
	args := map[string]any{}
	for {
		rest = strings.TrimLeft(rest, " ")
		if rest == "" || strings.HasPrefix(rest, ")") {
			return name, args
		}
		colon := strings.Index(rest, ": ")
		key := rest[:colon]
		decoder := json.NewDecoder(strings.NewReader(rest[colon+2:]))
		var value any
		if err := decoder.Decode(&value); err != nil {
			panic(fmt.Sprintf("the argument %q of %s does not read as JSON: %v", key, name, err))
		}
		args[key] = value
		rest = strings.TrimPrefix(rest[colon+2+int(decoder.InputOffset()):], ",")
	}
}

// requireParses fails the test unless the real parser accepts the call. A fake
// engine parses nothing, and a call the lexer refuses fails on every cluster
// while every test here stays green.
func requireParses(t *testing.T, q string) {
	t.Helper()
	if _, err := langparser.ParseExpression(q); err != nil {
		t.Fatalf("the rendered call does not parse: %v\n%.400s", err, q)
	}
}

// addPicture makes an artifact the Library shows its owner, with the bytes in
// storage.
func (tw *templateWorld) addPicture(artifact, mime string, data []byte) {
	key := "library/" + testOwner + "/file-" + artifact + "/" + artifact
	tw.engine.libraryArtifacts[artifact] = map[string]any{
		"id": "v1:library:artifact:" + artifact, "kind": "file", "sourceConceptRef": "v1:library:file:file-" + artifact, "title": artifact,
	}
	tw.engine.libraryFiles["file-"+artifact] = map[string]any{
		"id": "v1:library:file:file-" + artifact, "name": artifact, "mimeType": mime, "size": len(data), "blobUrl": key,
	}
	tw.blobs.objects[key] = data
}

// templateWorld is a worker over a fake engine that knows one active
// organization, with a recording gate, a fixed clock and storage that records
// how it is read.
type templateWorld struct {
	t      *testing.T
	w      *Worker
	engine *templateEngine
	gate   *layoutGateLog
	blobs  *recordingBlobs
}

func newTemplateWorld(t *testing.T) *templateWorld {
	t.Helper()
	engine := &templateEngine{fakeEngine: &fakeEngine{
		accounts: map[string]map[string]any{
			templateOrg: {"id": "v1:accounts:account:" + templateOrg, "name": "Acme Co", "status": "active"},
		},
		libraryArtifacts: map[string]map[string]any{},
		libraryFiles:     map[string]map[string]any{},
	}}
	w := newTestWorker(t, engine, &recordingSender{})
	gate := &layoutGateLog{}
	w.templateGate = gate.acquire
	w.now = func() time.Time { return templateNow }
	blobs := &recordingBlobs{objects: map[string][]byte{}}
	w.newBlobReader = func(context.Context) (blobReader, string, error) { return blobs, "memql", nil }
	return &templateWorld{t: t, w: w, engine: engine, gate: gate, blobs: blobs}
}

// save runs the builtin as the owner of the cluster's first test user.
func (tw *templateWorld) save(args map[string]any) ([]memorynodes.MemoryNode, error) {
	tw.t.Helper()
	return tw.w.handleSaveTemplate(importCtx(), args, 0)
}

// saved is save for a call that must succeed, and answers its receipt.
func (tw *templateWorld) saved(args map[string]any) savedTemplate {
	tw.t.Helper()
	nodes, err := tw.save(args)
	if err != nil {
		tw.t.Fatalf("campaignSaveTemplate: %v", err)
	}
	return decodeSaved(tw.t, nodes)
}

// savedTemplate is the receipt a client reads.
type savedTemplate struct {
	TemplateID string           `json:"templateId"`
	Revision   string           `json:"revision"`
	Status     string           `json:"status"`
	Saved      bool             `json:"saved"`
	Issues     []emaildoc.Issue `json:"issues"`
	raw        map[string]json.RawMessage
}

func decodeSaved(t *testing.T, nodes []memorynodes.MemoryNode) savedTemplate {
	t.Helper()
	if len(nodes) != 1 || nodes[0].ID != "campaignTemplateSaved" {
		t.Fatalf("expected one campaignTemplateSaved node, got %+v", nodes)
	}
	var out savedTemplate
	if err := json.Unmarshal(nodes[0].Payload, &out); err != nil {
		t.Fatalf("decoding the receipt: %v", err)
	}
	if err := json.Unmarshal(nodes[0].Payload, &out.raw); err != nil {
		t.Fatalf("decoding the receipt: %v", err)
	}
	return out
}

// hasIssue reports whether the receipt lists an issue of the code about the
// block ("" for any block).
func (s savedTemplate) hasIssue(code, block string) bool {
	for _, issue := range s.Issues {
		if issue.Code == code && (block == "" || issue.Block == block) {
			return true
		}
	}
	return false
}

// saveTemplateArgs is a creation call; each mutate changes the arguments.
func saveTemplateArgs(content any, mutate ...func(map[string]any)) map[string]any {
	args := map[string]any{
		"templateId":       "t1",
		"accountId":        templateOrg,
		"name":             "Welcome",
		"content":          content,
		"expectedRevision": "",
		"action":           "save",
	}
	for _, m := range mutate {
		m(args)
	}
	return args
}

// revised makes a call name the revision a client holds and the action it takes.
func revised(revision, action string) func(map[string]any) {
	return func(a map[string]any) { a["expectedRevision"], a["action"] = revision, action }
}

// pictureURI is a small picture as a data URI.
func pictureURI(t *testing.T) string {
	t.Helper()
	return "data:image/png;base64," + base64.StdEncoding.EncodeToString(previewPNG(t, 4, 3))
}

// footerLink gives the footer's link with the label an address.
func footerLink(t *testing.T, doc map[string]any, label, href string) {
	t.Helper()
	setBlock(t, doc, "footer", func(b map[string]any) {
		for _, item := range b["links"].([]any) {
			if link := item.(map[string]any); link["label"] == label {
				link["href"] = href
				return
			}
		}
		t.Fatalf("the footer has no link %q", label)
	})
}

// readyConfirmation is the built-in Confirmation with both of its pictures
// chosen and its one empty link given an address: nothing in it keeps it from
// being sent.
func readyConfirmation(t *testing.T) map[string]any {
	t.Helper()
	doc := layoutDocument(t, "builtin:confirmation")
	uri := pictureURI(t)
	for _, id := range []string{"logo", "hero"} {
		setBlock(t, doc, id, func(b map[string]any) { b["src"] = uri })
	}
	footerLink(t, doc, "Contact us", "https://acme.test/contact")
	return doc
}

// storedTemplateRow is a stored template as templateById and templateForSource
// answer: canonical ids and the version's createdAt, which is the revision a
// client holds. Without a document it is a template saved before documents
// existed.
func storedTemplateRow(id, status string, more map[string]any) map[string]any {
	row := map[string]any{
		"id":          "v1:campaigns:template:" + id,
		"ownerUserId": "v1:identity:user:" + testOwner,
		"accountId":   "v1:accounts:account:" + templateOrg,
		"name":        "Welcome",
		"subject":     "Old subject",
		"textBody":    "Old text.",
		"htmlBody":    "<p>Old text.</p>",
		"status":      status,
		"createdAt":   templateStored.Format(time.RFC3339Nano),
	}
	for k, v := range more {
		row[k] = v
	}
	return row
}

// oldContent is the content of storedTemplateRow as a client sends it.
func oldContent() map[string]any {
	return map[string]any{"subject": "Old subject", "textBody": "Old text.", "htmlBody": "<p>Old text.</p>"}
}

// canonicalDocument is the document as the handler stores it: parsed and
// written again, as the JSON object its file holds.
func canonicalDocument(t *testing.T, content any) map[string]any {
	t.Helper()
	encoded, err := json.Marshal(content)
	if err != nil {
		t.Fatal(err)
	}
	doc, err := emaildoc.Parse(encoded)
	if err != nil {
		t.Fatalf("the fixture is not a document: %v", err)
	}
	object, err := documentObject(doc)
	if err != nil {
		t.Fatal(err)
	}
	return object
}

// sameJSON reports whether two values are the same JSON.
func sameJSON(t *testing.T, a, b any) bool {
	t.Helper()
	left, err := plainJSON(a)
	if err != nil {
		t.Fatal(err)
	}
	right, err := plainJSON(b)
	if err != nil {
		t.Fatal(err)
	}
	return string(left) == string(right)
}

func TestSaveTemplateRendersALayoutDocument(t *testing.T) {
	tw := newTemplateWorld(t)
	doc := readyConfirmation(t)

	receipt := tw.saved(saveTemplateArgs(doc))

	if receipt.TemplateID != "t1" || receipt.Status != "draft" || !receipt.Saved || receipt.Revision != templateNow.Format(time.RFC3339Nano) {
		t.Errorf("receipt = %+v", receipt)
	}
	// An empty list and not a missing key or null: a client reads issues.length.
	if got := string(receipt.raw["issues"]); got != "[]" {
		t.Errorf("issues = %s, want []", got)
	}
	writes := tw.engine.writes()
	if len(writes) != 1 {
		t.Fatalf("expected one write, got %v", writes)
	}
	q := writes[0].query
	for _, want := range []string{
		"mutation createTemplate(",
		`htmlBody: "<!DOCTYPE html`,
		`layoutId: "builtin:confirmation"`,
		"document: {",
		"Confirm my address: {{confirmUrl}}",
	} {
		if !strings.Contains(q, want) {
			t.Errorf("the write lacks %q:\n%.600s", want, q)
		}
	}
	requireParses(t, q)
	if !writes[0].internal {
		t.Error("createTemplate is @serverOnly and ran without internal origin")
	}

	// The three parts are what the send-mode render makes of the document, and
	// the document is the one that was sent, written as its file is.
	parsed, err := emaildoc.Parse(mustJSON(t, doc))
	if err != nil {
		t.Fatal(err)
	}
	want, err := emaildoc.Render(context.Background(), parsed, nil, emaildoc.RenderOptions{})
	if err != nil {
		t.Fatal(err)
	}
	_, args := parseRenderedCall(q)
	if args["htmlBody"] != want.HTML || args["textBody"] != want.Text {
		t.Error("the stored parts are not the render of the document")
	}
	if strings.Contains(want.HTML, "data-block=") {
		t.Fatal("the render the test compares with is a preview")
	}
	if args["subject"] != doc["subject"] || args["preheader"] != doc["preheader"] || args["name"] != "Welcome" || args["accountId"] != templateOrg {
		t.Errorf("subject, preheader, name or organization wrong: %v %v %v %v", args["subject"], args["preheader"], args["name"], args["accountId"])
	}
	if !sameJSON(t, args["document"], canonicalDocument(t, doc)) {
		t.Error("the stored document is not the document that was sent")
	}
	if args["sourceArtifactId"] != "" || args["versionTime"] != templateNow.Format(time.RFC3339Nano) {
		t.Errorf("sourceArtifactId or versionTime wrong: %v %v", args["sourceArtifactId"], args["versionTime"])
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	encoded, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}

func TestSaveTemplateKeepsCustomTemplatesWorking(t *testing.T) {
	tw := newTemplateWorld(t)
	// What the Campaigns form and the old editors send: the two bodies and a
	// subject, with markup and merge tags in them.
	const html = `<p>Tom & <b>Jerry</b>, here is your {{fields.plan}} plan.</p>`
	content := map[string]any{"subject": "Hello {{displayName}}", "textBody": "Plain & simple.", "htmlBody": html}

	receipt := tw.saved(saveTemplateArgs(content))

	if receipt.Status != "draft" || len(receipt.Issues) != 0 || string(receipt.raw["issues"]) != "[]" {
		t.Errorf("receipt = %+v", receipt)
	}
	writes := tw.engine.writes()
	if len(writes) != 1 {
		t.Fatalf("expected one write, got %v", writes)
	}
	q := writes[0].query
	// The HTML is the author's, byte for byte, and written as it is.
	for _, want := range []string{`htmlBody: "` + html + `"`, `textBody: "Plain & simple."`, `layoutId: ""`, `preheader: ""`, `subject: "Hello {{displayName}}"`} {
		if !strings.Contains(q, want) {
			t.Errorf("the write lacks %q:\n%.600s", want, q)
		}
	}
	requireParses(t, q)
	_, args := parseRenderedCall(q)
	document, _ := args["document"].(map[string]any)
	if document["htmlBody"] != html || document["textBody"] != "Plain & simple." || document["blocks"] != nil || document["layout"] != nil {
		t.Errorf("the stored document is not the custom document: %v", document)
	}
}

func TestPublishRefusesDocumentsWithIssues(t *testing.T) {
	tw := newTemplateWorld(t)
	tw.engine.stateful = true
	doc := readyConfirmation(t)
	setBlock(t, doc, "hero", func(b map[string]any) { delete(b, "src") })

	saved := tw.saved(saveTemplateArgs(doc))

	// A draft is a draft: it saves, and says what is left to do.
	if len(saved.Issues) != 1 || !saved.hasIssue(emaildoc.IssueImageMissing, "hero") || saved.Issues[0].Message != heroToChoose {
		t.Fatalf("issues = %+v, want the one for the hero picture", saved.Issues)
	}
	_, err := tw.save(saveTemplateArgs(doc, revised(saved.Revision, "publish")))
	if err == nil || err.Error() != "fix these before publishing: "+heroToChoose {
		t.Fatalf("publishing error = %v", err)
	}
	if got := len(tw.engine.writes()); got != 1 {
		t.Fatalf("a refused publication wrote: %v", tw.engine.writes())
	}

	// Once the picture is chosen the same call publishes.
	setBlock(t, doc, "hero", func(b map[string]any) { b["src"] = pictureURI(t) })
	saved = tw.saved(saveTemplateArgs(doc, revised(saved.Revision, "save")))
	if len(saved.Issues) != 0 {
		t.Fatalf("issues = %+v", saved.Issues)
	}
	published := tw.saved(saveTemplateArgs(doc, revised(saved.Revision, "publish")))
	if published.Status != "ready" || len(published.Issues) != 0 || string(published.raw["issues"]) != "[]" {
		t.Errorf("publication = %+v", published)
	}
}

// The footer's "Contact us" is empty in every built-in layout, and an empty
// link is an issue a person must resolve: it is the one that is left when
// the pictures are chosen.
func TestPublishNamesEveryIssueThereIs(t *testing.T) {
	tw := newTemplateWorld(t)
	tw.engine.stateful = true
	doc := layoutDocument(t, "builtin:confirmation")
	uri := pictureURI(t)
	setBlock(t, doc, "logo", func(b map[string]any) { b["src"] = uri })

	saved := tw.saved(saveTemplateArgs(doc))

	if !saved.hasIssue(emaildoc.IssueImageMissing, "hero") || !saved.hasIssue(emaildoc.IssueLinkMissing, "footer") || len(saved.Issues) != 2 {
		t.Fatalf("issues = %+v", saved.Issues)
	}
	_, err := tw.save(saveTemplateArgs(doc, revised(saved.Revision, "publish")))
	want := "fix these before publishing: " + heroToChoose + ` Add a link to "Contact us".`
	if err == nil || err.Error() != want {
		t.Fatalf("publishing error = %v, want %q", err, want)
	}
}

// Review focus 3: a person who publishes a document naming a picture they
// cannot read gets a draft that says so, and no publication. The picture is
// read as the caller, so another member's private file is the same answer as
// one that does not exist.
func TestPublishRefusesAPictureTheCallerCannotRead(t *testing.T) {
	picture := previewPNG(t, 4, 3)
	for _, tc := range []struct {
		name  string
		setup func(tw *templateWorld)
	}{
		{"an artifact the engine does not show", func(*templateWorld) {}},
		{"an artifact that belongs to someone else", func(tw *templateWorld) {
			tw.addPicture("art-secret", "image/png", picture)
			tw.engine.pictureOwner = "someone-else"
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tw := newTemplateWorld(t)
			tw.engine.stateful = true
			tc.setup(tw)
			doc := readyConfirmation(t)
			setBlock(t, doc, "hero", func(b map[string]any) { b["src"] = "artifact:art-secret" })

			saved := tw.saved(saveTemplateArgs(doc))

			if len(saved.Issues) != 1 || !saved.hasIssue(emaildoc.IssueImageUnavailable, "hero") || saved.Issues[0].Message != heroUnreadable {
				t.Fatalf("issues = %+v, want the one for the hero picture", saved.Issues)
			}
			_, err := tw.save(saveTemplateArgs(doc, revised(saved.Revision, "publish")))
			if err == nil || err.Error() != "fix these before publishing: "+heroUnreadable {
				t.Fatalf("publishing error = %v", err)
			}
			// Nothing of the picture was fetched from storage for them.
			if limited, unbounded := tw.blobs.reads(); limited+unbounded != 0 {
				t.Errorf("storage was read %d times for a picture the caller cannot see", limited+unbounded)
			}
			if got := len(tw.engine.writes()); got != 1 {
				t.Errorf("writes = %d, want the draft alone", got)
			}
		})
	}
}

func TestSaveTemplateEmbedsPicturesTheCallerCanRead(t *testing.T) {
	tw := newTemplateWorld(t)
	tw.engine.pictureOwner = testOwner
	data := previewPNG(t, 5, 3) // not the picture of the logo, which is 4 by 3
	tw.addPicture("art-hero", "image/png", data)
	doc := readyConfirmation(t)
	setBlock(t, doc, "hero", func(b map[string]any) { b["src"] = "artifact:art-hero" })

	saved := tw.saved(saveTemplateArgs(doc))

	if len(saved.Issues) != 0 {
		t.Fatalf("issues = %+v", saved.Issues)
	}
	_, args := parseRenderedCall(tw.engine.writes()[0].query)
	embedded := "data:image/png;base64," + base64.StdEncoding.EncodeToString(data)
	if html, _ := args["htmlBody"].(string); !strings.Contains(html, embedded) || strings.Contains(html, "artifact:") {
		t.Error("the HTML does not carry the picture itself")
	}
	// The document keeps the reference, so whoever opens it next embeds their own
	// readable picture and the file stays small.
	stored := fmt.Sprint(args["document"])
	if !strings.Contains(stored, "artifact:art-hero") || strings.Contains(stored, embedded) {
		t.Error("the stored document does not keep the artifact reference")
	}
	if limited, unbounded := tw.blobs.reads(); limited != 1 || unbounded != 0 {
		t.Errorf("storage reads = %d limited, %d unlimited, want one limited", limited, unbounded)
	}
}

func TestPublishingAFileAgainUpdatesItsTemplate(t *testing.T) {
	derived := func(account, source string) string {
		sum := sha256.Sum256([]byte("memql/campaigns/template-source\x00" + account + "\x00" + source))
		return hex.EncodeToString(sum[:])[:32]
	}

	t.Run("the template of the file is the one saved, whatever id the call carries", func(t *testing.T) {
		tw := newTemplateWorld(t)
		row := storedTemplateRow("t1", "draft", nil)
		tw.engine.fakeEngine.template = row
		tw.engine.fakeEngine.templateBySource = map[string]map[string]any{templateOrg + "|" + templateSource: row}

		saved := tw.saved(saveTemplateArgs(readyConfirmation(t), func(a map[string]any) {
			a["templateId"] = "other"
			a["sourceArtifactId"] = templateSource
			a["expectedRevision"] = templateStored.Format(time.RFC3339Nano)
		}))

		if saved.TemplateID != "t1" || saved.Status != "draft" {
			t.Errorf("receipt = %+v, want the template of the file", saved)
		}
		writes := tw.engine.writes()
		if len(writes) != 1 || !strings.HasPrefix(writes[0].query, "mutation updateTemplate(") {
			t.Fatalf("writes = %v", writes)
		}
		q := writes[0].query
		if !strings.Contains(q, `templateId: "t1"`) || strings.Contains(q, "other") || !strings.Contains(q, `sourceArtifactId: "`+templateSource+`"`) {
			t.Errorf("the write is not an update of t1 from the file:\n%.500s", q)
		}
		requireParses(t, q)
		// The file's gate, then the template's own: a save from the file and a
		// save that names only the template must not both pass one revision.
		if keys, held := tw.gate.snapshot(); fmt.Sprint(keys) != "[source:acme:art-9 t1]" || held != 0 {
			t.Errorf("gate keys %v, still held %d", keys, held)
		}
		if reads := tw.engine.calls("query templateForSource"); len(reads) != 1 || !reads[0].fresh ||
			!strings.Contains(reads[0].query, `accountId: "acme"`) || !strings.Contains(reads[0].query, `sourceArtifactId: "art-9"`) {
			t.Errorf("the file's template must be read fresh, in the organization: %+v", reads)
		}
	})

	t.Run("the first publication of a file creates the template Rulings 14 names", func(t *testing.T) {
		tw := newTemplateWorld(t)
		want := derived(templateOrg, templateSource)

		saved := tw.saved(saveTemplateArgs(readyConfirmation(t), func(a map[string]any) {
			a["templateId"] = ""
			a["sourceArtifactId"] = templateSource
		}))

		if saved.TemplateID != want || len(want) != 32 {
			t.Errorf("template id = %q, want %q", saved.TemplateID, want)
		}
		writes := tw.engine.writes()
		if len(writes) != 1 || !strings.HasPrefix(writes[0].query, "mutation createTemplate(") || !strings.Contains(writes[0].query, `templateId: "`+want+`"`) {
			t.Fatalf("writes = %v", writes)
		}
		if keys, _ := tw.gate.snapshot(); fmt.Sprint(keys) != "[source:acme:art-9 "+want+"]" {
			t.Errorf("gate keys %v", keys)
		}
		// The same file in another organization is another template.
		if derived("other-org", templateSource) == want || derived(templateOrg, "art-10") == want {
			t.Error("the id does not depend on the organization and the file")
		}
	})

	t.Run("a caller's own id names the template of a file that has none", func(t *testing.T) {
		tw := newTemplateWorld(t)
		saved := tw.saved(saveTemplateArgs(readyConfirmation(t), func(a map[string]any) {
			a["templateId"] = "mine"
			a["sourceArtifactId"] = templateSource
		}))
		if saved.TemplateID != "mine" || !strings.Contains(tw.engine.writes()[0].query, `templateId: "mine"`) {
			t.Errorf("receipt = %+v, want the id the caller named", saved)
		}
	})

	t.Run("a canonical file id is the bare one", func(t *testing.T) {
		tw := newTemplateWorld(t)
		tw.saved(saveTemplateArgs(readyConfirmation(t), func(a map[string]any) {
			a["templateId"] = ""
			a["sourceArtifactId"] = "v1:library:artifact:" + templateSource
		}))
		if got := tw.engine.calls("query templateForSource"); len(got) != 1 || !strings.Contains(got[0].query, `sourceArtifactId: "`+templateSource+`"`) {
			t.Errorf("the lookup is %+v", got)
		}
		if keys, _ := tw.gate.snapshot(); keys[0] != "source:acme:art-9" {
			t.Errorf("gate keys %v", keys)
		}
	})

	t.Run("a second save from the file finds the template the first one made", func(t *testing.T) {
		tw := newTemplateWorld(t)
		tw.engine.stateful = true
		first := tw.saved(saveTemplateArgs(readyConfirmation(t), func(a map[string]any) {
			a["templateId"] = ""
			a["sourceArtifactId"] = templateSource
		}))
		edited := readyConfirmation(t)
		edited["subject"] = "A second subject"
		second := tw.saved(saveTemplateArgs(edited, func(a map[string]any) {
			a["templateId"] = ""
			a["sourceArtifactId"] = templateSource
			a["expectedRevision"] = first.Revision
		}))
		if second.TemplateID != first.TemplateID || second.Revision == first.Revision {
			t.Errorf("first %+v, second %+v: want one template and a newer revision", first, second)
		}
		writes := tw.engine.writes()
		if len(writes) != 2 || !strings.HasPrefix(writes[1].query, "mutation updateTemplate(") {
			t.Fatalf("writes = %v", writes)
		}
	})
}

func TestArchiveNeedsOnlyTheRevision(t *testing.T) {
	published := func(t *testing.T) (*templateWorld, savedTemplate, map[string]any) {
		tw := newTemplateWorld(t)
		tw.engine.stateful = true
		content := readyConfirmation(t)
		saved := tw.saved(saveTemplateArgs(content))
		ready := tw.saved(saveTemplateArgs(content, revised(saved.Revision, "publish")))
		return tw, ready, content
	}
	// Content that, if it were rendered, would look a picture up.
	other := layoutDocument(t, "builtin:letter")
	setBlock(t, other, "logo", func(b map[string]any) { b["src"] = "artifact:never-read" })

	for _, tc := range []struct {
		name    string
		content any
		mutate  func(map[string]any)
	}{
		{"different content", other, nil},
		{"content that is not a document", map[string]any{"subject": ""}, nil},
		{"no content at all", nil, nil},
		{"no name either", other, func(a map[string]any) { delete(a, "name") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tw, ready, _ := published(t)
			before := len(tw.engine.writes())

			archived := tw.saved(saveTemplateArgs(tc.content, func(a map[string]any) {
				a["expectedRevision"], a["action"] = ready.Revision, "archive"
				if tc.mutate != nil {
					tc.mutate(a)
				}
			}))

			if archived.Status != "archived" || archived.Revision == ready.Revision || len(archived.Issues) != 0 || string(archived.raw["issues"]) != "[]" {
				t.Errorf("receipt = %+v", archived)
			}
			writes := tw.engine.writes()
			if len(writes) != before+1 {
				t.Fatalf("writes = %d, want one more than %d", len(writes), before)
			}
			// The stored content is written back as it is.
			_, published := parseRenderedCall(writes[before-1].query)
			name, args := parseRenderedCall(writes[before].query)
			if name != "updateTemplate" || args["status"] != "archived" {
				t.Fatalf("the write is %s with status %v", name, args["status"])
			}
			for _, field := range []string{"name", "subject", "textBody", "htmlBody", "preheader", "layoutId", "sourceArtifactId", "document"} {
				if !sameJSON(t, args[field], published[field]) {
					t.Errorf("%s was changed by archiving", field)
				}
			}
			requireParses(t, writes[before].query)
			if got := tw.engine.calls("query libraryArtifactById"); len(got) != 0 {
				t.Errorf("archiving looked at pictures: %v", got)
			}
		})
	}

	t.Run("it still needs the exact revision", func(t *testing.T) {
		tw, _, _ := published(t)
		before := len(tw.engine.writes())
		for name, revision := range map[string]string{
			"an old one": templateStored.Format(time.RFC3339Nano), "none": "", "not a time": "yesterday",
		} {
			_, err := tw.save(saveTemplateArgs(nil, revised(revision, "archive")))
			if err == nil || err.Error() != templateChanged {
				t.Errorf("archiving with %s: %v", name, err)
			}
		}
		if got := len(tw.engine.writes()); got != before {
			t.Errorf("a refused archive wrote: %v", tw.engine.writes()[before:])
		}
	})
}

func TestRestoreReturnsATemplateToDraft(t *testing.T) {
	archived := func(t *testing.T) (*templateWorld, savedTemplate) {
		tw := newTemplateWorld(t)
		tw.engine.stateful = true
		content := readyConfirmation(t)
		saved := tw.saved(saveTemplateArgs(content))
		ready := tw.saved(saveTemplateArgs(content, revised(saved.Revision, "publish")))
		return tw, tw.saved(saveTemplateArgs(nil, revised(ready.Revision, "archive")))
	}

	t.Run("an archived template is a draft again, with the content it had", func(t *testing.T) {
		tw, away := archived(t)
		before := len(tw.engine.writes())
		back := tw.saved(saveTemplateArgs(map[string]any{"ignored": true}, revised(away.Revision, "restore")))
		if back.Status != "draft" || back.Revision == away.Revision || string(back.raw["issues"]) != "[]" {
			t.Errorf("receipt = %+v", back)
		}
		writes := tw.engine.writes()
		_, was := parseRenderedCall(writes[before-1].query)
		_, args := parseRenderedCall(writes[before].query)
		if args["status"] != "draft" || args["subject"] != was["subject"] || args["htmlBody"] != was["htmlBody"] || !sameJSON(t, args["document"], was["document"]) {
			t.Errorf("restoring changed the content or did not draft it: %v", args["status"])
		}
		// It is a draft, so it must be published again.
		if _, err := tw.save(saveTemplateArgs(readyConfirmation(t), revised(back.Revision, "publish"))); err != nil {
			t.Errorf("publishing a restored template: %v", err)
		}
	})

	t.Run("it needs the exact revision", func(t *testing.T) {
		tw, _ := archived(t)
		_, err := tw.save(saveTemplateArgs(nil, revised(templateStored.Format(time.RFC3339Nano), "restore")))
		if err == nil || err.Error() != templateChanged {
			t.Errorf("error = %v", err)
		}
	})

	t.Run("a retry of what is already done answers with the receipt it already has", func(t *testing.T) {
		tw, away := archived(t)
		back := tw.saved(saveTemplateArgs(nil, revised(away.Revision, "restore")))
		before := len(tw.engine.writes())
		// The same call again, and a call with no revision at all: neither writes.
		again := tw.saved(saveTemplateArgs(nil, revised(away.Revision, "restore")))
		blind := tw.saved(saveTemplateArgs(nil, revised("", "restore")))
		for _, got := range []savedTemplate{again, blind} {
			if got.Revision != back.Revision || got.Status != "draft" {
				t.Errorf("retry receipt = %+v, want %+v", got, back)
			}
		}
		if got := len(tw.engine.writes()); got != before {
			t.Errorf("a retry wrote %d more times", got-before)
		}
		// The same goes for archiving what is archived.
		gone := tw.saved(saveTemplateArgs(nil, revised(back.Revision, "archive")))
		before = len(tw.engine.writes())
		for _, revision := range []string{back.Revision, gone.Revision, ""} {
			got := tw.saved(saveTemplateArgs(nil, revised(revision, "archive")))
			if got.Revision != gone.Revision || got.Status != "archived" {
				t.Errorf("archive retry with %q = %+v, want %+v", revision, got, gone)
			}
		}
		if got := len(tw.engine.writes()); got != before {
			t.Errorf("an archive retry wrote %d more times", got-before)
		}
	})

	t.Run("a template that is not archived is as restored as it can be", func(t *testing.T) {
		tw := newTemplateWorld(t)
		tw.engine.fakeEngine.template = storedTemplateRow("t1", "ready", nil)
		got := tw.saved(saveTemplateArgs(nil, revised("", "restore")))
		if got.Status != "ready" || got.Revision != templateStored.Format(time.RFC3339Nano) || len(tw.engine.writes()) != 0 {
			t.Errorf("receipt = %+v, writes %v: restoring must not unpublish a template", got, tw.engine.writes())
		}
	})
}

func TestEditingAnArchivedTemplateReturnsItToDraft(t *testing.T) {
	tw := newTemplateWorld(t)
	tw.engine.fakeEngine.template = storedTemplateRow("t1", "archived", nil)
	saved := tw.saved(saveTemplateArgs(oldContent(), func(a map[string]any) {
		a["expectedRevision"] = templateStored.Format(time.RFC3339Nano)
		a["content"] = map[string]any{"subject": "Revived", "textBody": "Back again.", "htmlBody": ""}
	}))
	if saved.Status != "draft" {
		t.Errorf("status = %q, want draft: editing always returns a template to draft", saved.Status)
	}
}

func TestSavingTheSameContentAgainIsInert(t *testing.T) {
	tw := newTemplateWorld(t)
	tw.engine.stateful = true
	doc := readyConfirmation(t)
	first := tw.saved(saveTemplateArgs(doc))

	// An uncertain response is retried with the same call, and an autosave of
	// unchanged bytes is the same call again: neither writes another version,
	// and each says what the first one did.
	for name, revision := range map[string]string{"the same call": "", "the revision it was given": first.Revision, "a stale revision": "2020-01-01T00:00:00Z"} {
		again := tw.saved(saveTemplateArgs(doc, func(a map[string]any) { a["expectedRevision"] = revision }))
		if again.Revision != first.Revision || again.Status != "draft" || !again.Saved {
			t.Errorf("%s: receipt = %+v, want %+v", name, again, first)
		}
	}
	if got := len(tw.engine.writes()); got != 1 {
		t.Errorf("writes = %d, want the first alone", got)
	}

	// Publishing twice is the same: the second answers with the first's receipt.
	ready := tw.saved(saveTemplateArgs(doc, revised(first.Revision, "publish")))
	again := tw.saved(saveTemplateArgs(doc, revised(first.Revision, "publish")))
	if again.Revision != ready.Revision || again.Status != "ready" || len(tw.engine.writes()) != 2 {
		t.Errorf("second publication = %+v (first %+v), writes %d", again, ready, len(tw.engine.writes()))
	}

	// The receipt of an inert save still lists the issues of the email.
	draft := readyConfirmation(t)
	setBlock(t, draft, "hero", func(b map[string]any) { delete(b, "src") })
	tw2 := newTemplateWorld(t)
	tw2.engine.stateful = true
	one := tw2.saved(saveTemplateArgs(draft))
	two := tw2.saved(saveTemplateArgs(draft))
	if two.Revision != one.Revision || len(two.Issues) != 1 || !two.hasIssue(emaildoc.IssueImageMissing, "hero") {
		t.Errorf("an inert save lost its issues: %+v", two)
	}
}

func TestPublishNeedsTheExactRevisionAndTheExactContent(t *testing.T) {
	tw := newTemplateWorld(t)
	tw.engine.stateful = true
	doc := readyConfirmation(t)
	saved := tw.saved(saveTemplateArgs(doc))
	before := len(tw.engine.writes())

	// Stale, blank or unreadable revisions are all "changed".
	for name, revision := range map[string]string{
		"an old revision": templateStored.Format(time.RFC3339Nano), "no revision": "", "a revision that is not a time": "latest",
	} {
		_, err := tw.save(saveTemplateArgs(doc, revised(revision, "publish")))
		if err == nil || err.Error() != templateChanged {
			t.Errorf("publishing with %s: %v", name, err)
		}
	}
	// The right revision is not enough for content nobody saved.
	edited := readyConfirmation(t)
	edited["subject"] = "Not the subject that was reviewed"
	_, err := tw.save(saveTemplateArgs(edited, revised(saved.Revision, "publish")))
	if err == nil || err.Error() != unsavedTemplate {
		t.Errorf("publishing unsaved content: %v", err)
	}
	// Nor is changing only the preheader a way past it: a preheader is content.
	preheader := readyConfirmation(t)
	preheader["preheader"] = "A different line"
	if _, err := tw.save(saveTemplateArgs(preheader, revised(saved.Revision, "publish"))); err == nil || err.Error() != unsavedTemplate {
		t.Errorf("publishing a changed preheader: %v", err)
	}
	if got := len(tw.engine.writes()); got != before {
		t.Fatalf("a refused publication wrote: %v", tw.engine.writes()[before:])
	}
	// Only the exact saved content, at the exact revision: a copy of the document
	// that was saved is it, wherever it came from.
	if ready := tw.saved(saveTemplateArgs(readyConfirmation(t), revised(saved.Revision, "publish"))); ready.Status != "ready" {
		t.Errorf("publication = %+v", ready)
	}
}

// Everything a template stores is content: a rename is a change, and so is a
// picture whose bytes changed under an artifact reference that did not, since
// the HTML that is sent has the picture in it. Publishing the reviewed content
// means the content as it renders now.
func TestRenamingAndRePicturingAreChanges(t *testing.T) {
	t.Run("a rename", func(t *testing.T) {
		tw := newTemplateWorld(t)
		tw.engine.stateful = true
		doc := readyConfirmation(t)
		first := tw.saved(saveTemplateArgs(doc))

		renamed := tw.saved(saveTemplateArgs(doc, revised(first.Revision, "save"), func(a map[string]any) { a["name"] = "Renamed" }))

		if renamed.Revision == first.Revision || len(tw.engine.writes()) != 2 {
			t.Fatalf("a rename wrote nothing: %+v", renamed)
		}
		if _, args := parseRenderedCall(tw.engine.writes()[1].query); args["name"] != "Renamed" {
			t.Errorf("name = %v", args["name"])
		}
	})

	// The document is what an editor opens again: a change to it that does not
	// change the email, such as a block's id (a preview marks blocks by it, a
	// send does not), is still saved.
	t.Run("a change only the document shows", func(t *testing.T) {
		tw := newTemplateWorld(t)
		tw.engine.stateful = true
		doc := readyConfirmation(t)
		first := tw.saved(saveTemplateArgs(doc))
		setBlock(t, doc, "note", func(b map[string]any) { b["id"] = "reassurance" })

		second := tw.saved(saveTemplateArgs(doc, revised(first.Revision, "save")))

		if second.Revision == first.Revision || len(tw.engine.writes()) != 2 {
			t.Fatalf("a change of the document alone wrote nothing: %+v", second)
		}
		_, was := parseRenderedCall(tw.engine.writes()[0].query)
		_, args := parseRenderedCall(tw.engine.writes()[1].query)
		if args["htmlBody"] != was["htmlBody"] || args["textBody"] != was["textBody"] {
			t.Error("the fixture changes the email, not only its document")
		}
		if !strings.Contains(fmt.Sprint(args["document"]), "reassurance") {
			t.Error("the new document was not stored")
		}
	})

	t.Run("a picture that changed under the same reference", func(t *testing.T) {
		tw := newTemplateWorld(t)
		tw.engine.stateful = true
		tw.engine.pictureOwner = testOwner
		before, after := previewPNG(t, 5, 3), previewPNG(t, 6, 3)
		tw.addPicture("art-hero", "image/png", before)
		doc := readyConfirmation(t)
		setBlock(t, doc, "hero", func(b map[string]any) { b["src"] = "artifact:art-hero" })
		first := tw.saved(saveTemplateArgs(doc))

		// The file is replaced; the document is not.
		tw.addPicture("art-hero", "image/png", after)
		_, err := tw.save(saveTemplateArgs(doc, revised(first.Revision, "publish")))
		if err == nil || err.Error() != unsavedTemplate {
			t.Fatalf("publishing what the picture used to be: %v", err)
		}
		second := tw.saved(saveTemplateArgs(doc, revised(first.Revision, "save")))
		if second.Revision == first.Revision || len(tw.engine.writes()) != 2 {
			t.Fatalf("a changed picture wrote nothing: %+v", second)
		}
		_, args := parseRenderedCall(tw.engine.writes()[1].query)
		if html, _ := args["htmlBody"].(string); !strings.Contains(html, base64.StdEncoding.EncodeToString(after)) || strings.Contains(html, base64.StdEncoding.EncodeToString(before)) {
			t.Error("the new version does not carry the new picture")
		}
		if ready := tw.saved(saveTemplateArgs(doc, revised(second.Revision, "publish"))); ready.Status != "ready" {
			t.Errorf("publication = %+v", ready)
		}
	})
}

// A stored copy that differs from what saving the document would write now, in
// any field, is rewritten: the renderer improves, a picture is replaced, a
// migration changes a column. Only a copy that is the same in every field is
// left alone, so a save that finds one stored is a retry and not a change.
func TestAStoredCopyThatDiffersInAnyFieldIsRewritten(t *testing.T) {
	for _, field := range []string{"name", "subject", "textBody", "htmlBody", "preheader", "layoutId", "sourceArtifactId", "document"} {
		t.Run(field, func(t *testing.T) {
			tw := newTemplateWorld(t)
			tw.engine.stateful = true
			doc := readyConfirmation(t)
			save := func(revision string) savedTemplate {
				return tw.saved(saveTemplateArgs(doc, revised(revision, "save"), func(a map[string]any) { a["sourceArtifactId"] = templateSource }))
			}
			first := save("")
			stored := tw.engine.fakeEngine.template
			if field == "document" {
				stored["document"] = map[string]any{"subject": "Older", "textBody": "Older.", "htmlBody": ""}
			} else {
				stored[field] = "older " + field
			}

			again := save(first.Revision)

			if again.Revision == first.Revision || len(tw.engine.writes()) != 2 {
				t.Fatalf("a stored %s that differs was not rewritten: %+v", field, again)
			}
			_, was := parseRenderedCall(tw.engine.writes()[0].query)
			_, now := parseRenderedCall(tw.engine.writes()[1].query)
			if !sameJSON(t, now[field], was[field]) {
				t.Errorf("the rewritten %s is %v, not what the document makes (%v)", field, now[field], was[field])
			}
		})
	}
}

// A client reads issues.length, so the receipt never says null, whatever hands
// it a list.
func TestTemplateReceiptIssuesAreNeverNull(t *testing.T) {
	for name, issues := range map[string][]emaildoc.Issue{"nil": nil, "empty": {}} {
		nodes, err := templateReceipt("t1", templateNow, "draft", issues)
		if err != nil {
			t.Fatal(err)
		}
		if got := string(decodeSaved(t, nodes).raw["issues"]); got != "[]" {
			t.Errorf("%s: issues = %s, want []", name, got)
		}
	}
}

// lockingGates is the template gate as the database makes it: a key is held by one
// caller at a time, and the others wait for it (or for their own deadline). It
// records what is held now and what was ever taken, in order.
type lockingGates struct {
	mu    sync.Mutex
	held  map[string]chan struct{}
	taken []string
}

func newLockingGates() *lockingGates { return &lockingGates{held: map[string]chan struct{}{}} }

func (g *lockingGates) acquire(ctx context.Context, key string) (func(), error) {
	for {
		g.mu.Lock()
		if wait, busy := g.held[key]; busy {
			g.mu.Unlock()
			select {
			case <-wait:
				continue
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		release := make(chan struct{})
		g.held[key] = release
		g.taken = append(g.taken, key)
		g.mu.Unlock()
		return func() {
			g.mu.Lock()
			delete(g.held, key)
			g.mu.Unlock()
			close(release)
		}, nil
	}
}

func (g *lockingGates) holding() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	keys := make([]string, 0, len(g.held))
	for key := range g.held {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func (g *lockingGates) everTaken() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]string(nil), g.taken...)
}

// parkedRender makes the render of the next save stop at a picture it is reading,
// as a slow storage does: entered says that a read has begun, and closing release
// lets it go on. The picture is the hero of readyConfirmation, readable by the
// caller.
type parkedRender struct {
	entered chan struct{}
	release chan struct{}
}

func (tw *templateWorld) parkTheRender() *parkedRender {
	tw.engine.pictureOwner = testOwner
	tw.addPicture("art-hero", "image/png", previewPNG(tw.t, 5, 3))
	tw.blobs.hold = make(chan struct{})
	tw.blobs.entered = make(chan struct{}, 4)
	return &parkedRender{entered: tw.blobs.entered, release: tw.blobs.hold}
}

// slowDocument is readyConfirmation with its hero picture read from the Library.
func slowDocument(t *testing.T) map[string]any {
	doc := readyConfirmation(t)
	setBlock(t, doc, "hero", func(b map[string]any) { b["src"] = "artifact:art-hero" })
	return doc
}

// A render reads nothing a lock protects (the document and the caller's own
// pictures), and it can take a long time: a slot to wait for, then nine reads of
// storage, each allowed ten seconds. A lock is a transaction left open on the
// request pool, and the pool's backends end a transaction that has been idle for
// a minute and cancel a statement that waits for eight seconds. So a template is
// rendered BEFORE it takes its locks, and a slow render holds none.
func TestASlowRenderDoesNotHoldTheLocks(t *testing.T) {
	tw := newTemplateWorld(t)
	gates := newLockingGates()
	tw.w.templateGate = gates.acquire
	parked := tw.parkTheRender()

	done := make(chan error, 1)
	go func() {
		_, err := tw.save(saveTemplateArgs(slowDocument(t), func(a map[string]any) {
			a["templateId"] = ""
			a["sourceArtifactId"] = templateSource
		}))
		done <- err
	}()

	select {
	case <-parked.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the render never read the picture")
	}
	if held := gates.holding(); len(held) != 0 {
		t.Errorf("a render that is waiting for storage holds %v", held)
	}
	if taken := gates.everTaken(); len(taken) != 0 {
		t.Errorf("a render that is waiting for storage had already taken %v", taken)
	}

	close(parked.release)
	if err := <-done; err != nil {
		t.Fatalf("the save: %v", err)
	}
	// The locks are taken for the part that decides, in their order: the file's,
	// then the template's.
	if taken := gates.everTaken(); len(taken) != 2 || taken[0] != "source:acme:art-9" || len(taken[1]) != 32 {
		t.Errorf("locks taken = %v", taken)
	}
	if held := gates.holding(); len(held) != 0 {
		t.Errorf("locks still held: %v", held)
	}
}

// While one save renders slowly another may save the template, and the slow one
// must not then pass the revision it read: it takes its locks after its render,
// reads the template again under them and holds the caller to the revision.
func TestASaveDuringASlowRenderCannotPassTheSameRevision(t *testing.T) {
	tw := newTemplateWorld(t)
	tw.engine.stateful = true
	gates := newLockingGates()
	tw.w.templateGate = gates.acquire
	first := tw.saved(saveTemplateArgs(readyConfirmation(t)))
	before := len(tw.engine.writes())
	parked := tw.parkTheRender()

	slow := make(chan error, 1)
	go func() {
		_, err := tw.save(saveTemplateArgs(slowDocument(t), revised(first.Revision, "save")))
		slow <- err
	}()
	select {
	case <-parked.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the render never read the picture")
	}

	// A quick save of the same revision lands in the meantime, behind no lock.
	quick, cancel := context.WithTimeout(importCtx(), 3*time.Second)
	defer cancel()
	edited := readyConfirmation(t)
	edited["subject"] = "Saved while the other render waited"
	nodes, err := tw.w.handleSaveTemplate(quick, saveTemplateArgs(edited, revised(first.Revision, "save")), 0)
	if err != nil {
		t.Fatalf("a save during a slow render was held up: %v", err)
	}
	landed := decodeSaved(t, nodes)

	close(parked.release)
	if err := <-slow; err == nil || err.Error() != templateChanged {
		t.Fatalf("the slow save passed a revision that was no longer the template's: %v", err)
	}
	if got := len(tw.engine.writes()); got != before+1 {
		t.Fatalf("writes = %d, want the quick save's alone after the first (%d)", got, before+1)
	}
	if row := tw.engine.fakeEngine.template; templateTime(row["createdAt"]).Format(time.RFC3339Nano) != landed.Revision || row["subject"] != edited["subject"] {
		t.Errorf("the stored template is not the quick save's: %v", row)
	}
}

// A gate that cannot be taken (the database cancels a statement that waits for
// eight seconds, or a lock is wanted for longer than the call may wait) is not an
// error text for a person to read: it says what happened and what to do.
func TestAGateThatCannotBeTakenIsASentence(t *testing.T) {
	const sentence = "Someone else is saving this template. Try again in a moment."
	timeout := errors.New("pq: canceling statement due to statement timeout")

	t.Run("the first lock", func(t *testing.T) {
		tw := newTemplateWorld(t)
		tw.w.templateGate = func(context.Context, string) (func(), error) { return nil, timeout }
		_, err := tw.save(saveTemplateArgs(readyConfirmation(t)))
		if err == nil || err.Error() != sentence {
			t.Fatalf("error = %v, want %q", err, sentence)
		}
		if strings.Contains(err.Error(), "statement") {
			t.Error("the database's own words reached the person")
		}
		if len(tw.engine.writes()) != 0 {
			t.Error("wrote without the lock")
		}
	})

	t.Run("the template's own lock, after the file's", func(t *testing.T) {
		tw := newTemplateWorld(t)
		gates := newLockingGates()
		taken := 0
		tw.w.templateGate = func(ctx context.Context, key string) (func(), error) {
			taken++
			if taken == 2 {
				return nil, timeout
			}
			return gates.acquire(ctx, key)
		}
		_, err := tw.save(saveTemplateArgs(readyConfirmation(t), func(a map[string]any) { a["sourceArtifactId"] = templateSource }))
		if err == nil || err.Error() != sentence {
			t.Fatalf("error = %v, want %q", err, sentence)
		}
		if held := gates.holding(); len(held) != 0 {
			t.Errorf("the file's lock was kept: %v", held)
		}
		if len(tw.engine.writes()) != 0 {
			t.Error("wrote without the lock")
		}
	})

	t.Run("a caller who stops waiting", func(t *testing.T) {
		tw := newTemplateWorld(t)
		gates := newLockingGates()
		tw.w.templateGate = gates.acquire
		release, err := gates.acquire(context.Background(), "t1") // somebody else's save of t1
		if err != nil {
			t.Fatal(err)
		}
		defer release()
		waiting, cancel := context.WithTimeout(importCtx(), 100*time.Millisecond)
		defer cancel()
		if _, err := tw.w.handleSaveTemplate(waiting, saveTemplateArgs(readyConfirmation(t)), 0); err == nil || err.Error() != sentence {
			t.Fatalf("error = %v, want %q", err, sentence)
		}
	})
}

// A person who may not save here costs no render and takes no lock: the
// organization and the permission are looked at before either.
func TestSomeoneWhoMayNotSaveCostsNoRenderAndNoLock(t *testing.T) {
	for name, setup := range map[string]func(tw *templateWorld){
		"outside the organization": func(tw *templateWorld) { tw.engine.accounts = map[string]map[string]any{} },
		"an organization that is archived": func(tw *templateWorld) {
			tw.engine.accounts = map[string]map[string]any{templateOrg: {"id": "v1:accounts:account:" + templateOrg, "status": "archived"}}
		},
		"no permission to write": func(tw *templateWorld) { tw.engine.capable = func(string, string, string) bool { return false } },
	} {
		t.Run(name, func(t *testing.T) {
			tw := newTemplateWorld(t)
			setup(tw)
			parked := tw.parkTheRender()
			_ = parked
			_, err := tw.save(saveTemplateArgs(slowDocument(t)))
			if err == nil {
				t.Fatal("saved")
			}
			if keys, _ := tw.gate.snapshot(); len(keys) != 0 {
				t.Errorf("took the locks %v", keys)
			}
			if got := tw.engine.calls("query libraryArtifactById"); len(got) != 0 {
				t.Errorf("rendered: %v", got)
			}
			if limited, unbounded := tw.blobs.reads(); limited+unbounded != 0 {
				t.Error("storage was read")
			}
		})
	}
}

func TestSaveTemplateRefusalsComeInOrder(t *testing.T) {
	for _, tc := range []struct {
		name   string
		ctx    context.Context
		mutate func(map[string]any)
		want   string
	}{
		{"no template id and no file", importCtx(), func(a map[string]any) { a["templateId"] = "" }, chooseTemplate},
		{"an id with a space", importCtx(), func(a map[string]any) { a["templateId"] = "my template" }, chooseTemplate},
		{"an id over 160 characters", importCtx(), func(a map[string]any) { a["templateId"] = strings.Repeat("a", 161) }, chooseTemplate},
		{"a bad id even with a file", importCtx(), func(a map[string]any) { a["templateId"] = "my template"; a["sourceArtifactId"] = templateSource }, chooseTemplate},
		{"no organization", importCtx(), func(a map[string]any) { a["accountId"] = " " }, chooseTemplate},
		{"nobody is signed in", context.Background(), func(map[string]any) {}, chooseTemplate},
		{"no name", importCtx(), func(a map[string]any) { a["name"] = "  " }, chooseTemplate},
		{"a name over 200 characters", importCtx(), func(a map[string]any) { a["name"] = strings.Repeat("é", 201) }, chooseTemplate},
		{"a name with a line break", importCtx(), func(a map[string]any) { a["name"] = "Two\nlines" }, chooseTemplate},
		{"no name when publishing", importCtx(), func(a map[string]any) { a["name"] = ""; a["action"] = "publish" }, chooseTemplate},
		{"a source that is not a Library file id", importCtx(), func(a map[string]any) { a["sourceArtifactId"] = "not a file id!" }, badSourceFile},
		{"an unknown action", importCtx(), func(a map[string]any) { a["action"] = "send" }, unknownAction},
		// The id and the organization come before the action.
		{"an unknown action without an organization", importCtx(), func(a map[string]any) { a["action"] = "send"; a["accountId"] = "" }, chooseTemplate},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tw := newTemplateWorld(t)
			_, err := tw.w.handleSaveTemplate(tc.ctx, saveTemplateArgs(readyConfirmation(t), tc.mutate), 0)
			if err == nil || err.Error() != tc.want {
				t.Fatalf("error = %v, want %q", err, tc.want)
			}
			if keys, _ := tw.gate.snapshot(); len(keys) != 0 || len(tw.engine.made) != 0 {
				t.Fatalf("a refusal before the gate still took it or called the engine: gate %v, calls %v", keys, tw.engine.made)
			}
		})
	}

	t.Run("a document the parser refuses, in its own words", func(t *testing.T) {
		for name, content := range map[string]any{
			"an empty subject":                   map[string]any{"subject": ""},
			"a field the format does not have":   map[string]any{"subject": "Hi", "blocks": []any{}, "mystery": true},
			"no content":                         nil,
			"content that is not an object":      "just text",
			"both modes at once":                 map[string]any{"subject": "Hi", "textBody": "x", "htmlBody": "", "blocks": []any{}},
			"a control character in the subject": map[string]any{"subject": "Hi\u0000there", "textBody": "x", "htmlBody": ""},
		} {
			encoded, err := json.Marshal(content)
			if err != nil {
				t.Fatal(err)
			}
			_, parseErr := emaildoc.Parse(encoded)
			if parseErr == nil {
				continue
			}
			tw := newTemplateWorld(t)
			_, err = tw.save(saveTemplateArgs(content))
			if err == nil || err.Error() != "template: "+parseErr.Error() {
				t.Errorf("%s: error = %v, want template: %v", name, err, parseErr)
			}
			if keys, _ := tw.gate.snapshot(); len(keys) != 0 || len(tw.engine.made) != 0 {
				t.Errorf("%s: a document that could never be saved took the gate or reached the engine: %v %v", name, keys, tw.engine.made)
			}
		}
	})

	t.Run("a name of 200 characters is 200 characters, whatever they weigh", func(t *testing.T) {
		tw := newTemplateWorld(t)
		if _, err := tw.save(saveTemplateArgs(readyConfirmation(t), func(a map[string]any) { a["name"] = strings.Repeat("é", 200) })); err != nil {
			t.Fatalf("a name of 200 accented letters was refused: %v", err)
		}
	})

	t.Run("archiving and restoring need no name, no content and no file", func(t *testing.T) {
		for _, action := range []string{"archive", "restore"} {
			tw := newTemplateWorld(t)
			status := map[string]string{"archive": "ready", "restore": "archived"}[action]
			tw.engine.fakeEngine.template = storedTemplateRow("t1", status, nil)
			_, err := tw.w.handleSaveTemplate(importCtx(), map[string]any{
				"templateId": "t1", "accountId": templateOrg, "action": action, "expectedRevision": templateStored.Format(time.RFC3339Nano),
			}, 0)
			if err != nil {
				t.Errorf("%s with only the revision: %v", action, err)
			}
		}
	})
}

// What decides a save is looked at again under the locks; the organization and the
// permission are looked at before the render as well (see
// TestSomeoneWhoMayNotSaveCostsNoRenderAndNoLock).
func TestSaveTemplateRefusalsInsideTheGate(t *testing.T) {
	t.Run("an inactive or unreadable organization", func(t *testing.T) {
		for name, accounts := range map[string]map[string]map[string]any{
			"unknown":  {},
			"archived": {templateOrg: {"id": "v1:accounts:account:" + templateOrg, "name": "Acme Co", "status": "archived"}},
		} {
			tw := newTemplateWorld(t)
			tw.engine.accounts = accounts
			_, err := tw.save(saveTemplateArgs(readyConfirmation(t)))
			if err == nil || err.Error() != notAccessible {
				t.Fatalf("%s: error = %v", name, err)
			}
			if len(tw.engine.writes()) != 0 {
				t.Fatalf("%s: wrote for an organization the caller cannot use", name)
			}
			if _, held := tw.gate.snapshot(); held != 0 {
				t.Fatalf("%s: the gate was not released", name)
			}
			if got := tw.engine.calls("query templateForSource"); len(got) != 0 {
				t.Fatalf("%s: looked for a template in an organization the caller cannot use", name)
			}
		}
	})

	// Before the render it is not known whether the template is new, so somebody who
	// can neither create nor update is refused there (no render, no lock). Which of
	// the two a save needs is asked under the locks, on organization data.
	t.Run("creating asks for create, editing asks for update, both on organization data", func(t *testing.T) {
		tw := newTemplateWorld(t)
		// Somebody who may update but not create.
		tw.engine.capable = func(_, verb, _ string) bool { return verb == auth.VerbUpdate }
		if _, err := tw.save(saveTemplateArgs(readyConfirmation(t))); err == nil || err.Error() != templateNeeds {
			t.Fatalf("create without permission: %v", err)
		}
		if last := tw.engine.asked[len(tw.engine.asked)-1]; last != (layoutAsk{templateOrg, auth.VerbCreate, auth.ResourceData}) {
			t.Errorf("a new template was decided on %+v, want create on organization data", last)
		}
		// Somebody who may create but not update.
		tw.engine.asked = nil
		tw.engine.capable = func(_, verb, _ string) bool { return verb == auth.VerbCreate }
		tw.engine.fakeEngine.template = storedTemplateRow("t1", "draft", nil)
		args := saveTemplateArgs(readyConfirmation(t), func(a map[string]any) { a["expectedRevision"] = templateStored.Format(time.RFC3339Nano) })
		if _, err := tw.save(args); err == nil || err.Error() != templateNeeds {
			t.Fatalf("edit without permission: %v", err)
		}
		if last := tw.engine.asked[len(tw.engine.asked)-1]; last != (layoutAsk{templateOrg, auth.VerbUpdate, auth.ResourceData}) {
			t.Errorf("an existing template was decided on %+v, want update on organization data", last)
		}
		if len(tw.engine.writes()) != 0 {
			t.Fatalf("a refused save wrote: %v", tw.engine.writes())
		}
	})

	t.Run("a reader cannot save", func(t *testing.T) {
		// The package's ordinary fake decides from the caller's role.
		engine := &fakeEngine{accounts: map[string]map[string]any{templateOrg: {"id": "v1:accounts:account:" + templateOrg, "status": "active"}}}
		w := newTestWorker(t, engine, &recordingSender{})
		w.templateGate = (&layoutGateLog{}).acquire
		reader := auth.ContextWithToken(
			auth.ContextWithAccess(context.Background(), &auth.AccessContext{UserId: "reader-1", Role: auth.RoleReader}),
			&auth.TokenInfo{Subject: "reader-1"})
		if _, err := w.handleSaveTemplate(reader, saveTemplateArgs(readyConfirmation(t)), 0); err == nil || err.Error() != templateNeeds {
			t.Fatalf("reader: %v", err)
		}
	})

	t.Run("a template cannot move between organizations", func(t *testing.T) {
		tw := newTemplateWorld(t)
		tw.engine.fakeEngine.template = storedTemplateRow("t1", "draft", map[string]any{"accountId": "v1:accounts:account:other-org"})
		for _, action := range []string{"save", "archive"} {
			_, err := tw.save(saveTemplateArgs(readyConfirmation(t), revised(templateStored.Format(time.RFC3339Nano), action)))
			if err == nil || err.Error() != templateMoved {
				t.Errorf("%s: error = %v", action, err)
			}
		}
		if len(tw.engine.writes()) != 0 {
			t.Fatalf("wrote: %v", tw.engine.writes())
		}
	})

	t.Run("what was never saved cannot be revised, published, archived or restored", func(t *testing.T) {
		for _, tc := range []struct{ action, revision string }{
			{"save", templateStored.Format(time.RFC3339Nano)}, {"publish", ""}, {"archive", ""}, {"restore", ""},
		} {
			tw := newTemplateWorld(t)
			doc := readyConfirmation(t)
			setBlock(t, doc, "hero", func(b map[string]any) { b["src"] = "artifact:art-1" }) // a render would look it up
			_, err := tw.save(saveTemplateArgs(doc, revised(tc.revision, tc.action)))
			if err == nil || err.Error() != templateMissing {
				t.Errorf("%s: error = %v", tc.action, err)
			}
			if len(tw.engine.writes()) != 0 {
				t.Errorf("%s: wrote", tc.action)
			}
			// Archive and restore render nothing. A save or a publish is rendered
			// before the locks, when it is not yet known that the template is not
			// there, so it is the locks that refuse it.
			rendered := len(tw.engine.calls("query libraryArtifactById")) != 0
			if rendered != (tc.action == "save" || tc.action == "publish") {
				t.Errorf("%s: rendered = %v", tc.action, rendered)
			}
		}
	})

	t.Run("the revision must be the stored one, to the microsecond", func(t *testing.T) {
		tw := newTemplateWorld(t)
		tw.engine.fakeEngine.template = storedTemplateRow("t1", "draft", nil)
		for name, revision := range map[string]string{
			"an earlier one":    templateStored.Add(-time.Second).Format(time.RFC3339Nano),
			"a microsecond off": templateStored.Add(time.Microsecond).Format(time.RFC3339Nano),
			"none":              "",
		} {
			if _, err := tw.save(saveTemplateArgs(readyConfirmation(t), revised(revision, "save"))); err == nil || err.Error() != templateChanged {
				t.Errorf("%s: error = %v", name, err)
			}
		}
		if _, err := tw.save(saveTemplateArgs(readyConfirmation(t), revised(templateStored.Format(time.RFC3339Nano), "save"))); err != nil {
			t.Errorf("the stored revision: %v", err)
		}
	})
}

func TestTemplateWritesAreServerOnlyAndReadsAreFresh(t *testing.T) {
	tw := newTemplateWorld(t)
	tw.engine.stateful = true
	doc := readyConfirmation(t)
	first := tw.saved(saveTemplateArgs(doc, func(a map[string]any) { a["sourceArtifactId"] = templateSource }))
	tw.saved(saveTemplateArgs(doc, revised(first.Revision, "publish"), func(a map[string]any) { a["sourceArtifactId"] = templateSource }))

	for _, w := range tw.engine.writes() {
		if !w.internal {
			t.Errorf("%.40s ran without internal origin: both template mutations are @serverOnly", w.query)
		}
		requireParses(t, w.query)
	}
	for _, prefix := range []string{"query templateById", "query templateForSource", "query clientAccountById"} {
		reads := tw.engine.calls(prefix)
		if len(reads) == 0 {
			t.Errorf("%s was never read", prefix)
		}
		for _, r := range reads {
			if !r.fresh {
				t.Errorf("%s: the revision check must read fresh", prefix)
			}
		}
	}
	// The gate is released when the call is done, whichever way it ends.
	if _, held := tw.gate.snapshot(); held != 0 {
		t.Errorf("%d gates still held", held)
	}
	tw.gate.err = fmt.Errorf("gate unavailable")
	if _, err := tw.save(saveTemplateArgs(doc)); err == nil || err.Error() != "Someone else is saving this template. Try again in a moment." {
		t.Errorf("a gate that cannot be taken: %v", err)
	}
	tw.w.templateGate = nil
	if _, err := tw.save(saveTemplateArgs(doc)); err == nil || err.Error() != "template write coordination is unavailable" {
		t.Errorf("no gate configured: %v", err)
	}
}

// A document can name tens of thousands of pictures, and each is two queries
// and a request to storage (email_render.go). Saving reads no more of them than
// a preview does, and says in one issue that the email has more than it can
// carry.
func TestSaveTemplateReadsNoMorePicturesThanAPreviewDoes(t *testing.T) {
	tw := newTemplateWorld(t)
	var html strings.Builder
	for n := 0; n < 3000; n++ {
		fmt.Fprintf(&html, `<img src="artifact:pic-%d" alt="Picture %d">`, n, n)
	}
	content := map[string]any{"subject": "Many pictures", "textBody": "See them.", "htmlBody": html.String()}

	saved := tw.saved(saveTemplateArgs(content))

	if reads := len(tw.engine.calls("query libraryArtifactById")); reads != maxPictureReads {
		t.Errorf("%d pictures were looked up, want at most %d", reads, maxPictureReads)
	}
	counts := map[string]int{}
	for _, issue := range saved.Issues {
		counts[issue.Code]++
	}
	if counts[emaildoc.IssueImageUnavailable] != maxPictureReads || counts[emaildoc.IssueImagesLimit] != 1 || len(saved.Issues) != maxPictureReads+1 {
		t.Errorf("issues by code = %v, want %d unavailable and one limit", counts, maxPictureReads)
	}
}

// A document can also make a hundred thousand issues. The receipt of a save
// lists them as a preview does (each once, at most fifty of a kind), publishing
// is refused while there is any, and the refusal says the first few and how many
// more there are.
func TestPublishRefusalNamesTheFirstProblemsAndCountsTheRest(t *testing.T) {
	tw := newTemplateWorld(t)
	tw.engine.stateful = true
	doc := layoutDocument(t, "builtin:letter")
	buttons := make([]any, 0, 60)
	for n := 0; n < 60; n++ {
		buttons = append(buttons, map[string]any{"id": fmt.Sprintf("button-%d", n), "type": "button", "label": fmt.Sprintf("Button %d", n), "href": "", "color": "primary", "width": 200})
	}
	// Sixty buttons with no address, and no unsubscribe link besides.
	doc["blocks"] = buttons

	saved := tw.saved(saveTemplateArgs(doc))

	counts := map[string]int{}
	for _, issue := range saved.Issues {
		counts[issue.Code]++
	}
	if counts[emaildoc.IssueLinkMissing] != maxIssuesPerCode || counts[emaildoc.IssueUnsubscribeMissing] != 1 || len(saved.Issues) != maxIssuesPerCode+1 {
		t.Fatalf("issues by code = %v, want %d links missing and the unsubscribe link", counts, maxIssuesPerCode)
	}
	_, err := tw.save(saveTemplateArgs(doc, revised(saved.Revision, "publish")))
	if err == nil {
		t.Fatal("published an email with problems")
	}
	want := "fix these before publishing: "
	for n := 0; n < maxRefusalIssues; n++ {
		want += fmt.Sprintf(`Add a link to "Button %d". `, n)
	}
	want = strings.TrimSuffix(want, " ") + fmt.Sprintf(" And %d more.", len(saved.Issues)-maxRefusalIssues)
	if err.Error() != want {
		t.Errorf("refusal = %.400s\nwant      %.400s", err.Error(), want)
	}
}

func TestPublishRefusesAnEmailTooLargeToSend(t *testing.T) {
	t.Run("too much markup", func(t *testing.T) {
		tw := newTemplateWorld(t)
		tw.engine.stateful = true
		doc := readyConfirmation(t)
		// Each "<" is written as four bytes in the HTML, so a file of about 1.1
		// MiB is an email of more than 4 MiB.
		setBlock(t, doc, "note", func(b map[string]any) { b["text"] = strings.Repeat("<", 1150<<10) })

		saved := tw.saved(saveTemplateArgs(doc))

		if len(saved.Issues) != 1 || !saved.hasIssue(emaildoc.IssueEmailTooLarge, "") || saved.Issues[0].Message != tooLargeToSend {
			t.Fatalf("issues = %+v, want the one that says the email is too large", saved.Issues)
		}
		_, err := tw.save(saveTemplateArgs(doc, revised(saved.Revision, "publish")))
		if err == nil || err.Error() != "fix these before publishing: "+tooLargeToSend {
			t.Fatalf("publishing error = %v", err)
		}
	})

	t.Run("pictures that, embedded, do not fit", func(t *testing.T) {
		tw := newTemplateWorld(t)
		tw.engine.stateful = true
		tw.engine.pictureOwner = testOwner
		doc := readyConfirmation(t)
		// One picture of just under 1 MiB, shown twice, is two data URIs of 1.33
		// MiB in one email, though the document itself, which names it, is small
		// and the picture is within what an email may carry.
		tw.addPicture("art-big", "image/png", noisePNG(t, emaildoc.MaxImageBytes))
		for _, id := range []string{"logo", "hero"} {
			setBlock(t, doc, id, func(b map[string]any) { b["src"] = "artifact:art-big" })
		}

		saved := tw.saved(saveTemplateArgs(doc))

		if !saved.hasIssue(emaildoc.IssueEmailTooLarge, "") {
			t.Fatalf("issues = %+v, want email_too_large", saved.Issues)
		}
		_, err := tw.save(saveTemplateArgs(doc, revised(saved.Revision, "publish")))
		if err == nil || err.Error() != "fix these before publishing: "+tooLargeToSend {
			t.Fatalf("publishing error = %v", err)
		}
	})

	t.Run("the renderer's own ceiling is a sentence for a person", func(t *testing.T) {
		tw := newTemplateWorld(t)
		doc := layoutDocument(t, "builtin:letter")
		// Each "&" is five bytes of HTML ("&amp;"), so 1.9 MiB of them in a file
		// that is within 2 MiB is an email of more than 8 MiB.
		setBlock(t, doc, "body", func(b map[string]any) { b["text"] = strings.Repeat("&", 1900<<10) })
		_, err := tw.save(saveTemplateArgs(doc))
		if err == nil || err.Error() != tooLargeToWrite {
			t.Fatalf("error = %v, want %q", err, tooLargeToWrite)
		}
		if len(tw.engine.writes()) != 0 {
			t.Error("wrote an email too large to render")
		}
	})
}

// An email past the 2 MiB a send takes can never be sent, and its HTML can be
// 8 MiB: a template row (and every answer that carries it) would weigh that for
// nothing. A draft that has the email_too_large issue keeps no HTML. It keeps its
// document, from which an editor renders it on demand, and says what is wrong in
// the receipt; saving it again is still a retry, and publishing it is refused.
func TestADraftTooLargeToSendKeepsNoHTML(t *testing.T) {
	storedHTML := func(tw *templateWorld, n int) string {
		_, args := parseRenderedCall(tw.engine.writes()[n].query)
		html, _ := args["htmlBody"].(string)
		return html
	}

	for name, build := range map[string]func(tw *templateWorld) map[string]any{
		"too much markup": func(tw *templateWorld) map[string]any {
			doc := readyConfirmation(t)
			// Each "<" is four bytes of HTML: a file of about 1.1 MiB is an email of more than 4 MiB.
			setBlock(t, doc, "note", func(b map[string]any) { b["text"] = strings.Repeat("<", 1150<<10) })
			return doc
		},
		"pictures that, embedded, do not fit": func(tw *templateWorld) map[string]any {
			tw.engine.pictureOwner = testOwner
			tw.addPicture("art-big", "image/png", noisePNG(t, emaildoc.MaxImageBytes))
			doc := readyConfirmation(t)
			for _, id := range []string{"logo", "hero"} {
				setBlock(t, doc, id, func(b map[string]any) { b["src"] = "artifact:art-big" })
			}
			return doc
		},
	} {
		t.Run(name, func(t *testing.T) {
			tw := newTemplateWorld(t)
			tw.engine.stateful = true
			doc := build(tw)

			first := tw.saved(saveTemplateArgs(doc))

			if !first.hasIssue(emaildoc.IssueEmailTooLarge, "") {
				t.Fatalf("issues = %+v, want email_too_large", first.Issues)
			}
			_, args := parseRenderedCall(tw.engine.writes()[0].query)
			if html := storedHTML(tw, 0); html != "" {
				t.Errorf("the draft stored %d bytes of HTML that can never be sent", len(html))
			}
			if document, _ := args["document"].(map[string]any); document["blocks"] == nil {
				t.Errorf("the document, which the HTML is rendered from on demand, is not stored: %v", args["document"])
			}
			if text, _ := args["textBody"].(string); text == "" {
				t.Error("the plain-text part was not stored")
			}

			// A retry is a retry: the content to store is the same, so nothing is written.
			if again := tw.saved(saveTemplateArgs(doc)); again.Revision != first.Revision || len(tw.engine.writes()) != 1 {
				t.Errorf("saving it again wrote another version: %+v", again)
			}
			// It cannot be published, and says why.
			if _, err := tw.save(saveTemplateArgs(doc, revised(first.Revision, "publish"))); err == nil || err.Error() != "fix these before publishing: "+tooLargeToSend {
				t.Errorf("publishing error = %v", err)
			}
			// Archiving writes back what is stored, no HTML.
			tw.saved(saveTemplateArgs(nil, revised(first.Revision, "archive")))
			if html := storedHTML(tw, 1); html != "" || len(tw.engine.writes()) != 2 {
				t.Errorf("archiving a draft that keeps no HTML wrote %d bytes of it (%d writes)", len(html), len(tw.engine.writes()))
			}
		})
	}

	// An email that fits keeps its HTML, as ever.
	tw := newTemplateWorld(t)
	tw.saved(saveTemplateArgs(readyConfirmation(t)))
	if html := storedHTML(tw, 0); !strings.HasPrefix(html, "<!DOCTYPE html") {
		t.Errorf("an email that fits has HTML %.40q", html)
	}
}

// noisePNG is a picture of random pixels, which no compression helps, of at
// most limit bytes and not much fewer.
func noisePNG(t *testing.T, limit int) []byte {
	t.Helper()
	// Three bytes a pixel, and a little for the rows' filters and the framing.
	for side := int(math.Sqrt(float64(limit-2048) / 3)); side > 8; side -= 2 {
		img := image.NewNRGBA(image.Rect(0, 0, side, side))
		rand.New(rand.NewSource(7)).Read(img.Pix)
		for i := 3; i < len(img.Pix); i += 4 {
			img.Pix[i] = 255 // opaque, so that it is written as three bytes a pixel
		}
		var buf bytes.Buffer
		if err := png.Encode(&buf, img); err != nil {
			t.Fatal(err)
		}
		if buf.Len() <= limit-1024 {
			return buf.Bytes()
		}
	}
	t.Fatal("no picture fits")
	return nil
}

// A node is shared by whoever saves, previews and sends. Saving takes one of the
// slots a preview takes, around the render, so that a flood of saves cannot cost
// a node more than a flood of previews; archiving renders nothing, so it does
// not wait for one.
func TestSavingWaitsForARenderSlotAndArchivingDoesNot(t *testing.T) {
	tw := newTemplateWorld(t)
	tw.engine.stateful = true
	doc := readyConfirmation(t)
	saved := tw.saved(saveTemplateArgs(doc))
	holdRenderSlots(t, maxConcurrentRenders)
	setDuration(t, &renderSlotWait, 50*time.Millisecond)
	before := len(tw.engine.writes())

	_, err := tw.save(saveTemplateArgs(doc, func(a map[string]any) { a["name"] = "Renamed" }))
	if err == nil || err.Error() != saveIsBusy {
		t.Fatalf("saving with every slot taken: %v, want %q", err, saveIsBusy)
	}
	if _, err := tw.save(saveTemplateArgs(doc, revised(saved.Revision, "publish"))); err == nil || err.Error() != saveIsBusy {
		t.Fatalf("publishing with every slot taken: %v", err)
	}
	if got := len(tw.engine.writes()); got != before {
		t.Fatalf("a refused save wrote")
	}
	if _, held := tw.gate.snapshot(); held != 0 {
		t.Fatal("a save that waited in vain kept its gate")
	}
	archived := tw.saved(saveTemplateArgs(nil, revised(saved.Revision, "archive")))
	if archived.Status != "archived" {
		t.Errorf("archiving with every slot taken: %+v", archived)
	}
}

func TestSaveTemplateMeasuresTheFileNotItsEscapedForm(t *testing.T) {
	// A JSON encoder's default writes each "<" as six bytes, and a custom email
	// of 1.5 MiB of them would be measured as nine: refused here although the
	// preview and a layout of the same size are not.
	tw := newTemplateWorld(t)
	html := strings.Repeat("<", emaildoc.MaxBytes*3/4)
	content := map[string]any{"subject": "Large", "textBody": "Large.", "htmlBody": html}

	receipt := tw.saved(saveTemplateArgs(content))

	if receipt.Status != "draft" {
		t.Errorf("receipt = %+v", receipt)
	}
	if got := tw.engine.writes(); len(got) != 1 {
		t.Errorf("writes = %d, want the one", len(got))
	}
	// A file that is larger than a file may be is still refused, in plain words,
	// before anything is looked up.
	tw = newTemplateWorld(t)
	content["htmlBody"] = strings.Repeat("<", emaildoc.MaxBytes+1)
	if _, err := tw.save(saveTemplateArgs(content)); err == nil || err.Error() != "template: "+fileTooLarge {
		t.Errorf("a file over 2 MiB: %v", err)
	}
	if len(tw.engine.made) != 0 {
		t.Error("a file over 2 MiB reached the engine")
	}
}

// A template saved before documents existed is a custom template by
// construction. Its subject and bodies are the whole of it, and publishing them
// unchanged is what it was before: it needs no second save to store a document.
func TestATemplateSavedBeforeDocumentsStaysPublishable(t *testing.T) {
	t.Run("publishing it unchanged", func(t *testing.T) {
		tw := newTemplateWorld(t)
		tw.engine.fakeEngine.template = storedTemplateRow("t1", "draft", nil)

		ready := tw.saved(saveTemplateArgs(oldContent(), revised(templateStored.Format(time.RFC3339Nano), "publish")))

		if ready.Status != "ready" {
			t.Errorf("receipt = %+v", ready)
		}
		name, args := parseRenderedCall(tw.engine.writes()[0].query)
		if name != "updateTemplate" || args["status"] != "ready" {
			t.Fatalf("the write is %s with status %v", name, args["status"])
		}
		// The publication stores the document the old parts make.
		if !sameJSON(t, args["document"], map[string]any{"subject": "Old subject", "textBody": "Old text.", "htmlBody": "<p>Old text.</p>"}) {
			t.Errorf("document = %v", args["document"])
		}
	})

	t.Run("saving it unchanged writes nothing", func(t *testing.T) {
		tw := newTemplateWorld(t)
		tw.engine.fakeEngine.template = storedTemplateRow("t1", "draft", nil)
		got := tw.saved(saveTemplateArgs(oldContent(), revised("", "save")))
		if got.Revision != templateStored.Format(time.RFC3339Nano) || got.Status != "draft" || len(tw.engine.writes()) != 0 {
			t.Errorf("receipt = %+v, writes %v", got, tw.engine.writes())
		}
	})

	t.Run("publishing something else needs a save first", func(t *testing.T) {
		tw := newTemplateWorld(t)
		tw.engine.fakeEngine.template = storedTemplateRow("t1", "draft", nil)
		other := oldContent()
		other["subject"] = "New subject"
		_, err := tw.save(saveTemplateArgs(other, revised(templateStored.Format(time.RFC3339Nano), "publish")))
		if err == nil || err.Error() != unsavedTemplate {
			t.Errorf("error = %v", err)
		}
	})

	t.Run("archiving it writes its parts back and no document", func(t *testing.T) {
		tw := newTemplateWorld(t)
		tw.engine.fakeEngine.template = storedTemplateRow("t1", "ready", nil)
		tw.saved(saveTemplateArgs(nil, revised(templateStored.Format(time.RFC3339Nano), "archive")))
		_, args := parseRenderedCall(tw.engine.writes()[0].query)
		if _, named := args["document"]; named || args["subject"] != "Old subject" || args["status"] != "archived" {
			t.Errorf("the archive call is %v", args)
		}
	})

	t.Run("a layout document is not the same content as a custom one with the same subject", func(t *testing.T) {
		tw := newTemplateWorld(t)
		doc := readyConfirmation(t)
		tw.engine.fakeEngine.template = storedTemplateRow("t1", "draft", map[string]any{"subject": doc["subject"]})
		_, err := tw.save(saveTemplateArgs(doc, revised(templateStored.Format(time.RFC3339Nano), "publish")))
		if err == nil || err.Error() != unsavedTemplate {
			t.Errorf("error = %v", err)
		}
	})
}

// Saving a template from another file moves it there; saving it without naming
// a file keeps the one it has.
func TestTheFileOfATemplateFollowsTheCall(t *testing.T) {
	tw := newTemplateWorld(t)
	tw.engine.stateful = true
	doc := readyConfirmation(t)
	first := tw.saved(saveTemplateArgs(doc, func(a map[string]any) { a["sourceArtifactId"] = "art-1" }))

	// Nothing else changed, and no file is named: nothing is written.
	if again := tw.saved(saveTemplateArgs(doc)); again.Revision != first.Revision || len(tw.engine.writes()) != 1 {
		t.Fatalf("saving without a file rewrote the template: %+v", again)
	}
	// The same content from another file is a change.
	moved := tw.saved(saveTemplateArgs(doc, revised(first.Revision, "save"), func(a map[string]any) { a["sourceArtifactId"] = "art-2" }))
	if moved.Revision == first.Revision || len(tw.engine.writes()) != 2 {
		t.Fatalf("saving from another file wrote nothing: %+v", moved)
	}
	_, args := parseRenderedCall(tw.engine.writes()[1].query)
	if args["sourceArtifactId"] != "art-2" {
		t.Errorf("sourceArtifactId = %v", args["sourceArtifactId"])
	}
	// An edit that names no file keeps the file the template has.
	edited := readyConfirmation(t)
	edited["subject"] = "A later subject"
	tw.saved(saveTemplateArgs(edited, revised(moved.Revision, "save")))
	_, args = parseRenderedCall(tw.engine.writes()[2].query)
	if args["sourceArtifactId"] != "art-2" {
		t.Errorf("an edit without a file changed the file to %q", args["sourceArtifactId"])
	}
}

func TestTemplateForSourceAsksForTheFileInTheOrganization(t *testing.T) {
	engine := &fakeEngine{templateBySource: map[string]map[string]any{
		"acme|art-9": storedTemplateRow("t1", "ready", nil),
	}}
	store := NewStore(engine)
	row, found, err := store.TemplateForSource(context.Background(), "acme", "art-9")
	if err != nil || !found || bare(str(row, "id")) != "t1" {
		t.Fatalf("TemplateForSource = %v, %v, %v", row, found, err)
	}
	for _, ask := range [][2]string{{"acme", "art-10"}, {"other", "art-9"}} {
		if _, found, err := store.TemplateForSource(context.Background(), ask[0], ask[1]); err != nil || found {
			t.Errorf("%v: found = %v, err = %v, want neither", ask, found, err)
		}
	}
	// The call the engine is asked is the query's own, named arguments.
	q := engine.calls[0].query
	if q != `query templateForSource(accountId: "acme", sourceArtifactId: "art-9")` {
		t.Errorf("the call is %s", q)
	}
	requireParses(t, q)
}
