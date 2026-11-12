package campaigns

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"maps"
	"math/rand"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/znasllc-io/memql/component/compose/emaildoc"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/integrations/compose"
)

// compose_tools_test.go -- the three tools of an email composition run
// (emailComposeContext, emailComposeRender and emailComposeSubmit, through the
// campaignCompose* builtins) and the material they read, driven against the
// package's fake engine.
//
// The engine here keeps every template a test writes, by id, so a submit that
// saves a copy beside a template that changed can be read back on both sides.
// What needs a database (the exact revision across replicas, the account tier
// of templateById) is template_db_test.go's.

const (
	// The sentences a person (or the composer) reads, spelled out here and not
	// taken from the handlers so that a change of wording is a change of a test.
	composeNotWriting   = "This composition is no longer running."
	composeNoTemplate   = "These tools work only inside an email composition."
	composeMissing      = "The template this composition writes can't be found."
	composeNotComposing = "MemQL is not writing this template."
	composeCopyName     = "Spring news (MemQL's version)"
	composeChanged      = "The template changed, so MemQL saved a copy."
	composeNoOrg        = "organization is not active or accessible"
)

// composeEngine is the template engine with every stored template kept by its
// bare id: createTemplate and updateTemplate apply as the concept's mutations
// do, and templateById answers the row with the id it is asked for.
type composeEngine struct {
	*templateEngine
	rowsMu sync.Mutex
	rows   map[string]map[string]any
}

func (e *composeEngine) Execute(ctx context.Context, q string) (any, error) {
	answer, err := e.templateEngine.Execute(ctx, q) // records the call
	switch {
	case strings.HasPrefix(q, "query templateById"):
		e.rowsMu.Lock()
		defer e.rowsMu.Unlock()
		if row, ok := e.rows[argOf(q, "templateId")]; ok {
			return rowsEnvelope([]map[string]any{maps.Clone(row)}), nil
		}
		return rowsEnvelope(nil), nil
	case strings.HasPrefix(q, "mutation createTemplate("), strings.HasPrefix(q, "mutation updateTemplate("):
		if err != nil {
			return nil, err
		}
		e.keep(q)
		return nil, nil
	}
	return answer, err
}

func (e *composeEngine) keep(q string) {
	name, args := parseRenderedCall(q)
	e.rowsMu.Lock()
	defer e.rowsMu.Unlock()
	id := fmt.Sprint(args["templateId"])
	row := map[string]any{}
	if name == "updateTemplate" {
		maps.Copy(row, e.rows[id])
	} else {
		row["id"] = "v1:campaigns:template:" + id
		row["ownerUserId"] = "v1:identity:user:" + testOwner
		row["accountId"] = "v1:accounts:account:" + fmt.Sprint(args["accountId"])
		row["status"] = "draft"
	}
	for _, field := range []string{"name", "subject", "textBody", "htmlBody", "preheader", "layoutId", "sourceArtifactId", "document", "composition", "status"} {
		if value, named := args[field]; named {
			row[field] = value
		}
	}
	row["createdAt"] = args["versionTime"]
	e.rows[id] = row
}

// row is the stored template with the id, as a copy.
func (e *composeEngine) row(id string) map[string]any {
	e.rowsMu.Lock()
	defer e.rowsMu.Unlock()
	return maps.Clone(e.rows[id])
}

// edit changes the stored template with the id, as another writer would.
func (e *composeEngine) edit(id string, change func(row map[string]any)) {
	e.rowsMu.Lock()
	defer e.rowsMu.Unlock()
	change(e.rows[id])
}

// composeWorld is a worker over that engine: one active organization, a
// recording template gate, a fixed clock and storage that records its reads.
type composeWorld struct {
	t      *testing.T
	w      *Worker
	engine *composeEngine
	gate   *layoutGateLog
	blobs  *recordingBlobs
}

func newComposeWorld(t *testing.T) *composeWorld {
	t.Helper()
	engine := &composeEngine{templateEngine: &templateEngine{fakeEngine: &fakeEngine{
		accounts: map[string]map[string]any{
			templateOrg: {"id": "v1:accounts:account:" + templateOrg, "name": "Acme Co", "status": "active"},
		},
		libraryArtifacts: map[string]map[string]any{},
		libraryFiles:     map[string]map[string]any{},
	}}, rows: map[string]map[string]any{}}
	w := newTestWorker(t, engine, &recordingSender{})
	gate := &layoutGateLog{}
	w.templateGate = gate.acquire
	w.now = func() time.Time { return templateNow }
	blobs := &recordingBlobs{objects: map[string][]byte{}}
	w.newBlobReader = func(context.Context) (blobReader, string, error) { return blobs, "memql", nil }
	return &composeWorld{t: t, w: w, engine: engine, gate: gate, blobs: blobs}
}

// withKit gives the organization the test brand kit and answers it as it reads.
func (cw *composeWorld) withKit() emaildoc.BrandKit {
	cw.t.Helper()
	cw.engine.brandKit = storedBrandRow(cw.t, testBrandKit(cw.t))
	kit, _, ok, err := cw.w.brandKitFor(importCtx(), templateOrg)
	if !ok || err != nil {
		cw.t.Fatalf("brandKitFor: ok=%v err=%v", ok, err)
	}
	return kit
}

// addFile puts a file in the caller's Library, with its bytes in storage, and
// answers its id.
func (cw *composeWorld) addFile(name, mime string, data []byte) string {
	cw.t.Helper()
	id := fmt.Sprintf("file-%d", len(cw.engine.libraryFiles)+1)
	key := "library/" + testOwner + "/" + id + "/blob"
	cw.engine.libraryFiles[id] = map[string]any{
		"id": "v1:library:file:" + id, "ownerUserId": testOwner, "name": name, "mimeType": mime, "size": len(data), "blobUrl": key,
	}
	cw.blobs.objects[key] = data
	return id
}

// start makes a template from the layout whose composition is writing, the
// way campaignComposeEmail starts one: the template is created, then the goal
// is recorded with the revision the composition starts from.
func (cw *composeWorld) start(layoutID string, fileIDs []string, reference string) string {
	cw.t.Helper()
	c := composition{GoalID: composeGoal, State: "writing", StartedAt: templateNow,
		Request: compositionRequest{LayoutID: layoutID, Name: "Spring news", Description: "Tell them about spring.", FileIDs: fileIDs, ReferenceFileID: reference}}
	id, revision, _, err := cw.w.createTemplateFromLayout(importCtx(), templateOrg, "Spring news", layoutID, &c)
	if err != nil {
		cw.t.Fatalf("createTemplateFromLayout: %v", err)
	}
	c.BaseRevision = revision
	if _, err := cw.w.writeComposition(importCtx(), templateOrg, id, c); err != nil {
		cw.t.Fatalf("writeComposition: %v", err)
	}
	return id
}

// setState writes the template's composition with another state, as cancel
// and the end of a run do.
func (cw *composeWorld) setState(id, state string) {
	cw.t.Helper()
	c, ok := compositionFromRow(cw.engine.row(id))
	if !ok {
		cw.t.Fatal("the template has no composition")
	}
	c.State = state
	if _, err := cw.w.writeComposition(importCtx(), templateOrg, id, c); err != nil {
		cw.t.Fatal(err)
	}
}

// composeGoal is the goal start gives a composition, which the run stamps on
// every tool call as goalId.
const composeGoal = "goal-1"

// tool runs one of the three handlers as the cluster's first test user, with
// the template and the goal the run stamps, and decodes the node it answers.
func (cw *composeWorld) tool(name, templateID string, c map[string]any) (map[string]any, error) {
	cw.t.Helper()
	return cw.toolFor(name, templateID, composeGoal, c)
}

// toolFor is tool with the goal a session stamps given.
func (cw *composeWorld) toolFor(name, templateID, goalID string, c map[string]any) (map[string]any, error) {
	cw.t.Helper()
	args := map[string]any{"templateId": templateID, "goalId": goalID}
	if c != nil {
		args["composition"] = c
	}
	handlers := map[string]struct {
		kind string
		run  func(context.Context, map[string]any, int) ([]memorynodes.MemoryNode, error)
	}{
		"context": {"campaignComposeContext", cw.w.handleComposeContext},
		"render":  {"campaignComposeRendered", cw.w.handleComposeRender},
		"submit":  {"campaignComposeSubmitted", cw.w.handleComposeSubmit},
	}
	handler, ok := handlers[name]
	if !ok {
		cw.t.Fatalf("no tool %q", name)
	}
	nodes, err := handler.run(importCtx(), args, 0)
	if err != nil {
		return nil, err
	}
	if len(nodes) != 1 || nodes[0].ID != handler.kind {
		cw.t.Fatalf("expected one %s node, got %+v", handler.kind, nodes)
	}
	var out map[string]any
	if err := json.Unmarshal(nodes[0].Payload, &out); err != nil {
		cw.t.Fatalf("decoding the answer: %v", err)
	}
	return out, nil
}

// must is tool for a call that has to succeed.
func (cw *composeWorld) must(name, templateID string, c map[string]any) map[string]any {
	cw.t.Helper()
	out, err := cw.tool(name, templateID, c)
	if err != nil {
		cw.t.Fatalf("%s: %v", name, err)
	}
	return out
}

// zipEntry is one member of a test archive.
type zipEntry struct{ path, body string }

func zipOf(t *testing.T, entries ...zipEntry) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, e := range entries {
		f, err := zw.Create(e.path)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.Write([]byte(e.body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// noisyPNG is a valid picture of more than 1 MiB: noise does not compress.
func noisyPNG(t *testing.T) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 640, 640))
	random := rand.New(rand.NewSource(7))
	for y := 0; y < 640; y++ {
		for x := 0; x < 640; x++ {
			img.Set(x, y, color.RGBA{uint8(random.Intn(256)), uint8(random.Intn(256)), uint8(random.Intn(256)), 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	if buf.Len() <= emaildoc.MaxImageBytes {
		t.Fatalf("the noisy picture is only %d bytes", buf.Len())
	}
	return buf.Bytes()
}

func dataURI(mime string, data []byte) string {
	return "data:" + mime + ";base64," + base64.StdEncoding.EncodeToString(data)
}

// composedDocument is the email a composition makes: the raw layout with the
// kit applied (the neutral layout without one), composed with the handles.
func composedDocument(t *testing.T, layoutID string, kit *emaildoc.BrandKit, c map[string]any, images map[string]string) map[string]any {
	t.Helper()
	layout, _ := emaildoc.Layout(layoutID)
	info, _ := emaildoc.LayoutMeta(layoutID)
	var err error
	if kit != nil {
		if layout, err = emaildoc.ApplyBrand(layout, *kit); err != nil {
			t.Fatal(err)
		}
	}
	raw, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	var parsed emaildoc.Composition
	if err := json.Unmarshal(raw, &parsed); err != nil {
		t.Fatal(err)
	}
	doc, err := emaildoc.Compose(layout, info, parsed, images)
	if err != nil {
		t.Fatalf("Compose: %v", err)
	}
	object, err := documentObject(doc)
	if err != nil {
		t.Fatal(err)
	}
	return object
}

// findBlockIn is the block with the id in a stored document, wherever it sits.
func findBlockIn(doc any, id string) map[string]any {
	object, _ := doc.(map[string]any)
	var walk func(blocks []any) map[string]any
	walk = func(blocks []any) map[string]any {
		for _, item := range blocks {
			b, _ := item.(map[string]any)
			if b["id"] == id {
				return b
			}
			if inner, ok := b["blocks"].([]any); ok {
				if found := walk(inner); found != nil {
					return found
				}
			}
			if columns, ok := b["columns"].([]any); ok {
				for _, column := range columns {
					if inner, ok := column.(map[string]any)["blocks"].([]any); ok {
						if found := walk(inner); found != nil {
							return found
						}
					}
				}
			}
		}
		return nil
	}
	blocks, _ := object["blocks"].([]any)
	return walk(blocks)
}

// articleComposition writes Article's banner with the handle and its intro.
func articleComposition(banner string) map[string]any {
	content := []any{map[string]any{"id": "intro", "text": "The days are longer, and the garden is waking up."}}
	if banner != "" {
		content = append(content, map[string]any{"id": "banner", "image": banner, "alt": "A spring garden"})
	}
	return map[string]any{
		"subject": "Spring is here", "preheader": "Three things to plant this week.",
		"content": content, "drop": []any{"highlight"},
	}
}

func listOf(v any) []any {
	list, _ := v.([]any)
	return list
}

func textsOf(v any) []string {
	var out []string
	for _, item := range listOf(v) {
		out = append(out, fmt.Sprint(item))
	}
	return out
}

func TestComposeContextListsPicturesWithHandles(t *testing.T) {
	cw := newComposeWorld(t)
	cw.withKit()
	hero, photo, reference := previewPNG(t, 6, 4), previewPicture(t, "jpeg"), previewPNG(t, 7, 9)
	heroID := cw.addFile("hero.png", "image/png", hero)
	photoID := cw.addFile("team.jpg", "image/jpeg", photo)
	copyID := cw.addFile("copy.txt", "text/plain", []byte("Spring is here."))
	referenceID := cw.addFile("finished.png", "image/png", reference)
	id := cw.start("builtin:article", []string{heroID, photoID, copyID}, referenceID)

	answer := cw.must("context", id, nil)

	type picture struct {
		Handle, File, MIME   string
		Bytes, Width, Height int
		Reference            bool
	}
	raw, _ := json.Marshal(answer["pictures"])
	var pictures []picture
	if err := json.Unmarshal(raw, &pictures); err != nil {
		t.Fatal(err)
	}
	want := []picture{
		{compose.EmailAssetHandle(hero), "hero.png", "image/png", len(hero), 6, 4, false},
		{compose.EmailAssetHandle(photo), "team.jpg", "image/jpeg", len(photo), 4, 3, false},
		{compose.EmailAssetHandle(reference), "finished.png", "image/png", len(reference), 7, 9, true},
	}
	if !slices.Equal(pictures, want) {
		t.Errorf("pictures = %+v\nwant %+v", pictures, want)
	}
	if answer["reference"] != "finished.png" {
		t.Errorf("reference = %v", answer["reference"])
	}
	var texts []string
	for _, f := range listOf(answer["files"]) {
		file := f.(map[string]any)
		if file["mime"] == "text/plain" {
			texts = append(texts, fmt.Sprint(file["file"]))
		}
	}
	if !slices.Equal(texts, []string{"copy.txt"}) {
		t.Errorf("text files = %v", texts)
	}

	// The budget is the email's, less the two pictures the kit puts in Article
	// (the logo and the signature photo).
	brandBytes := len(previewPNG(t, 4, 3)) + len(previewPNG(t, 5, 5))
	budget := answer["budget"].(map[string]any)
	for key, value := range map[string]float64{
		"maxPictures": 8, "maxBytes": float64(emaildoc.MaxImageBytes),
		"brandPictures": 2, "brandBytes": float64(brandBytes),
		"pictures": 6, "bytes": float64(emaildoc.MaxImageBytes - brandBytes),
	} {
		if budget[key] != value {
			t.Errorf("budget.%s = %v, want %v", key, budget[key], value)
		}
	}

	info, _ := emaildoc.LayoutMeta("builtin:article")
	if !slices.Equal(textsOf(answer["optional"]), info.Optional) {
		t.Errorf("optional = %v", answer["optional"])
	}
	if !slices.Equal(textsOf(answer["brandSlots"]), []string{"logo", "signature", "footer", "reply"}) || answer["brandKit"] != true {
		t.Errorf("brand = %v %v", answer["brandKit"], answer["brandSlots"])
	}
	var slots []string
	for _, s := range listOf(answer["slots"]) {
		slots = append(slots, fmt.Sprint(s.(map[string]any)["id"]))
	}
	if !slices.Equal(slots, []string{"banner", "lead", "intro", "highlight", "article", "closing-title", "closing-text"}) {
		t.Errorf("slots = %v", slots)
	}
	layout := answer["layout"].(map[string]any)
	if layout["id"] != "builtin:article" || layout["subject"] == "" {
		t.Errorf("layout = %v %v", layout["id"], layout["subject"])
	}
	banner := findBlockIn(layout, "banner")
	if banner == nil || banner["type"] != "image" || banner["role"] != "content" || banner["width"] != float64(552) || banner["height"] != float64(191) || banner["alt"] != "Article banner" {
		t.Errorf("banner = %v", banner)
	}
	if logo := findBlockIn(layout, "logo"); logo == nil || logo["role"] != "brand" {
		t.Errorf("logo = %v", logo)
	}
	if title := findBlockIn(layout, "closing-title"); title == nil || title["role"] != "content" || title["text"] == "" {
		t.Errorf("a block inside columns is not outlined: %v", title)
	}
	if answer["description"] != "Tell them about spring." || answer["accountName"] != "Acme Co" || answer["name"] != "Spring news" {
		t.Errorf("description %v, account %v, name %v", answer["description"], answer["accountName"], answer["name"])
	}

	// No picture passes through the model: not the brand's, not the material's.
	encoded, _ := json.Marshal(answer)
	if bytes.Contains(encoded, []byte("base64")) || bytes.Contains(encoded, []byte("data:image")) {
		t.Error("the context carries picture bytes")
	}
	if len(cw.engine.writes()) != 2 { // the create and the composition's goal, nothing more
		t.Errorf("the context wrote: %d writes", len(cw.engine.writes()))
	}
}

func TestComposeContextNamesWorkspaceFilesDistinctly(t *testing.T) {
	cw := newComposeWorld(t)
	archive := zipOf(t,
		zipEntry{"a/email.txt", "From folder a."},
		zipEntry{"b/email.txt", "From folder b."},
		zipEntry{"readme.pdf", "%PDF-1.4"},
	)
	zipID := cw.addFile("STAGE B - 1.zip", "application/zip", archive)
	plainID := cw.addFile("STAGE B - 1__a__email.txt", "text/plain", []byte("A file of its own."))
	shoutID := cw.addFile("stage b - 1__B__EMAIL.TXT", "text/plain", []byte("Named like another, in capitals."))
	id := cw.start("builtin:article", []string{zipID, plainID, shoutID}, "")

	answer := cw.must("context", id, nil)
	var names []string
	for _, f := range listOf(answer["files"]) {
		names = append(names, fmt.Sprint(f.(map[string]any)["file"]))
	}
	want := []string{"STAGE B - 1__a__email.txt", "STAGE B - 1__b__email.txt", "STAGE B - 1__a__email-2.txt", "stage b - 1__B__EMAIL-2.TXT"}
	if !slices.Equal(names, want) {
		t.Errorf("files = %q\nwant %q", names, want)
	}
	if left := textsOf(answer["leftOut"]); !slices.Equal(left, []string{"Left out STAGE B - 1__readme.pdf: not a picture or text file."}) {
		t.Errorf("leftOut = %q", left)
	}

	// Each name holds its own file's words: none overwrote another.
	c, _ := compositionFromRow(cw.engine.row(id))
	m, err := cw.w.compositionMaterial(importCtx(), c.Request)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, f := range m.Files {
		got[f.Workspace] = string(f.Data)
	}
	for name, body := range map[string]string{
		"STAGE B - 1__a__email.txt": "From folder a.", "STAGE B - 1__b__email.txt": "From folder b.",
		"STAGE B - 1__a__email-2.txt": "A file of its own.", "stage b - 1__B__EMAIL-2.TXT": "Named like another, in capitals.",
	} {
		if got[name] != body {
			t.Errorf("%s holds %q, want %q", name, got[name], body)
		}
	}
	if len(m.Skipped) != 1 || m.Skipped[0] != (compose.SkippedMember{Path: "STAGE B - 1__readme.pdf", Reason: "not a picture or text file"}) {
		t.Errorf("skipped = %+v", m.Skipped)
	}
}

func TestComposeRenderReturnsIssues(t *testing.T) {
	cw := newComposeWorld(t)
	cw.withKit()
	hero := previewPNG(t, 6, 4)
	heroID := cw.addFile("hero.png", "image/png", hero)
	id := cw.start("builtin:article", []string{heroID}, "")
	writes := len(cw.engine.writes())

	// No banner chosen: the draft says so, about the block.
	answer := cw.must("render", id, articleComposition(""))
	var missing bool
	for _, item := range listOf(answer["issues"]) {
		issue := item.(map[string]any)
		missing = missing || issue["code"] == emaildoc.IssueImageMissing && issue["block"] == "banner" && issue["message"] != ""
	}
	if !missing {
		t.Errorf("issues = %v, want image_missing on banner", answer["issues"])
	}
	if answer["subject"] != "Spring is here" || !strings.Contains(fmt.Sprint(answer["text"]), "The days are longer") {
		t.Errorf("subject %v, text %.200v", answer["subject"], answer["text"])
	}
	var slots []string
	for _, s := range listOf(answer["slots"]) {
		slots = append(slots, fmt.Sprint(s.(map[string]any)["id"]))
	}
	if slices.Contains(slots, "highlight") || !slices.Contains(slots, "intro") {
		t.Errorf("slots after dropping highlight = %v", slots)
	}

	// With the banner chosen by its handle, that issue is gone, and the HTML
	// shows pictures by their handles, never their bytes.
	answer = cw.must("render", id, articleComposition(compose.EmailAssetHandle(hero)))
	for _, item := range listOf(answer["issues"]) {
		if item.(map[string]any)["code"] == emaildoc.IssueImageMissing {
			t.Errorf("issue left after choosing the banner: %v", item)
		}
	}
	html := fmt.Sprint(answer["html"])
	if strings.Contains(html, "data:image") || strings.Contains(html, "base64") {
		t.Error("the render carries picture bytes")
	}
	for _, picture := range [][]byte{hero, previewPNG(t, 4, 3)} { // the banner and the kit's logo
		if !strings.Contains(html, compose.EmailAssetHandle(picture)) {
			t.Errorf("the HTML does not show the picture %s by its handle", compose.EmailAssetHandle(picture))
		}
	}

	// What the layout refuses comes back as its sentence.
	refused := articleComposition("")
	refused["content"] = append(listOf(refused["content"]), map[string]any{"id": "logo", "alt": "Ours"})
	if _, err := cw.tool("render", id, refused); err == nil || !strings.Contains(err.Error(), "brand block") {
		t.Errorf("a draft that writes the logo: %v", err)
	}
	if len(cw.engine.writes()) != writes {
		t.Error("a render wrote something")
	}
}

// The preview tool judges the composed email by the template's purpose, as the
// submit does: the issues the composer is shown are the ones its submit will
// answer. Every layout's brand footer carries {{unsubscribeUrl}} (a kit without
// one is refused), so a composed transactional email always names the link it
// may not carry, and a marketing one never does.
func TestThePreviewToolJudgesATemplateByItsPurpose(t *testing.T) {
	codes := func(v any) []string {
		var out []string
		for _, item := range listOf(v) {
			out = append(out, fmt.Sprint(item.(map[string]any)["code"]))
		}
		return out
	}
	for _, purpose := range []string{"", "marketing", "transactional"} {
		cw := newComposeWorld(t)
		cw.withKit()
		hero := previewPNG(t, 6, 4)
		id := cw.start("builtin:article", []string{cw.addFile("hero.png", "image/png", hero)}, "")
		if purpose != "" {
			cw.engine.edit(id, func(row map[string]any) { row["purpose"] = purpose })
		}
		c := articleComposition(compose.EmailAssetHandle(hero))
		preview := codes(cw.must("render", id, c)["issues"])
		submitted := codes(cw.must("submit", id, c)["issues"])
		if present := slices.Contains(preview, emaildoc.IssueUnsubscribePresent); present != (purpose == "transactional") {
			t.Errorf("purpose %q: the preview's issues are %q", purpose, preview)
		}
		if slices.Contains(preview, emaildoc.IssueUnsubscribeMissing) {
			t.Errorf("purpose %q: the preview misses the brand footer's link: %q", purpose, preview)
		}
		if !slices.Equal(preview, submitted) {
			t.Errorf("purpose %q: the preview says %q, the submit %q", purpose, preview, submitted)
		}
	}
}

func TestComposeSubmitSavesDraftAndMarksDone(t *testing.T) {
	cw := newComposeWorld(t)
	kit := cw.withKit()
	hero := previewPNG(t, 6, 4)
	heroID := cw.addFile("hero.png", "image/png", hero)
	zipID := cw.addFile("STAGE B - 1.zip", "application/zip", zipOf(t,
		zipEntry{"copy.txt", "Spring is here."}, zipEntry{"readme.pdf", "%PDF-1.4"}))
	id := cw.start("builtin:article", []string{heroID, zipID}, "")
	started, _ := compositionFromRow(cw.engine.row(id))
	writes := len(cw.engine.writes())

	c := articleComposition(compose.EmailAssetHandle(hero))
	receipt := cw.must("submit", id, c)

	all := cw.engine.writes()
	if len(all) != writes+1 {
		t.Fatalf("submit made %d writes, want one", len(all)-writes)
	}
	last := all[len(all)-1]
	requireParses(t, last.query)
	name, args := parseRenderedCall(last.query)
	if name != "updateTemplate" || !last.internal || args["templateId"] != id {
		t.Fatalf("write = %s %v", name, args["templateId"])
	}
	row := cw.engine.row(id)
	want := composedDocument(t, "builtin:article", &kit, c, map[string]string{compose.EmailAssetHandle(hero): dataURI("image/png", hero)})
	if !sameJSON(t, row["document"], want) {
		t.Error("the stored document is not the composition in the brand-applied layout")
	}
	if src := findBlockIn(row["document"], "banner")["src"]; src != dataURI("image/png", hero) {
		t.Errorf("banner src = %.60v", src)
	}
	if row["status"] != "draft" || row["subject"] != "Spring is here" || row["name"] != "Spring news" || row["layoutId"] != "builtin:article" {
		t.Errorf("row: status %v subject %v name %v layout %v", row["status"], row["subject"], row["name"], row["layoutId"])
	}
	done, ok := compositionFromRow(row)
	if !ok || done.State != "done" || !done.FinishedAt.Equal(templateNow) || done.Error != "" {
		t.Errorf("composition = %+v", done)
	}
	if !slices.Equal(done.Notes, []string{"Left out STAGE B - 1__readme.pdf: not a picture or text file."}) {
		t.Errorf("notes = %q", done.Notes)
	}
	if done.GoalID != started.GoalID || !done.StartedAt.Equal(started.StartedAt) || !sameJSON(t, done.Request, started.Request) {
		t.Errorf("the run's record changed: %+v", done)
	}
	if receipt["templateId"] != id || receipt["revision"] != row["createdAt"] || receipt["issues"] == nil {
		t.Errorf("receipt = %v", receipt)
	}
	if keys, held := cw.gate.snapshot(); !slices.Contains(keys, id) || held != 0 {
		t.Errorf("gate keys %v, held %d", keys, held)
	}
}

func TestComposeSubmitSavesACopyWhenTheTemplateChanged(t *testing.T) {
	cw := newComposeWorld(t)
	kit := cw.withKit()
	hero := previewPNG(t, 6, 4)
	heroID := cw.addFile("hero.png", "image/png", hero)
	pdfID := cw.addFile("brochure.pdf", "application/pdf", []byte("%PDF-1.4"))
	id := cw.start("builtin:article", []string{heroID, pdfID}, "")

	// A colleague saves the template while MemQL writes it.
	later := templateNow.Add(time.Hour).Format(time.RFC3339Nano)
	cw.engine.edit(id, func(row map[string]any) { row["subject"], row["createdAt"] = "A colleague's subject", later })
	theirs := cw.engine.row(id)
	writes := len(cw.engine.writes())

	c := articleComposition(compose.EmailAssetHandle(hero))
	receipt := cw.must("submit", id, c)

	made := cw.engine.writes()[writes:]
	if len(made) != 2 {
		t.Fatalf("submit made %d writes, want the copy and the original's composition", len(made))
	}
	copyName, copyArgs := parseRenderedCall(made[0].query)
	if copyName != "createTemplate" {
		t.Fatalf("first write = %s", copyName)
	}
	copyID := fmt.Sprint(copyArgs["templateId"])
	if copyID == id || receipt["templateId"] != copyID {
		t.Fatalf("copy %s, receipt %v", copyID, receipt["templateId"])
	}
	copied := cw.engine.row(copyID)
	if copied["name"] != composeCopyName || copied["status"] != "draft" || bare(str(copied, "accountId")) != templateOrg || str(copied, "sourceArtifactId") != "" {
		t.Errorf("copy: name %v status %v account %v source %v", copied["name"], copied["status"], copied["accountId"], copied["sourceArtifactId"])
	}
	want := composedDocument(t, "builtin:article", &kit, c, map[string]string{compose.EmailAssetHandle(hero): dataURI("image/png", hero)})
	if !sameJSON(t, copied["document"], want) {
		t.Error("the copy does not hold MemQL's version")
	}
	if done, ok := compositionFromRow(copied); !ok || done.State != "done" || !slices.Equal(done.Notes, []string{"Left out brochure.pdf: not a picture or text file."}) {
		t.Errorf("copy's composition = %+v", done)
	}
	if receipt["revision"] != copied["createdAt"] {
		t.Errorf("receipt revision %v, copy %v", receipt["revision"], copied["createdAt"])
	}

	// The original keeps the colleague's version, and says where MemQL's went.
	original := cw.engine.row(id)
	for _, field := range []string{"name", "subject", "textBody", "htmlBody", "preheader", "layoutId", "document", "status"} {
		if !sameJSON(t, original[field], theirs[field]) {
			t.Errorf("the original's %s changed", field)
		}
	}
	failed, _ := compositionFromRow(original)
	if failed.State != "failed" || failed.Error != composeChanged || !failed.FinishedAt.Equal(templateNow) {
		t.Errorf("original's composition = %+v", failed)
	}
}

func TestComposeCopyNameFitsTheNameLimit(t *testing.T) {
	if got := conflictCopyName("Spring news"); got != composeCopyName {
		t.Errorf("copy name = %q", got)
	}
	long := strings.Repeat("é", maxTemplateNameChars)
	got := conflictCopyName(long)
	if utf8.RuneCountInString(got) != maxTemplateNameChars || !strings.HasSuffix(got, " (MemQL's version)") || !utf8.ValidString(got) {
		t.Errorf("long copy name = %q (%d runes)", got, utf8.RuneCountInString(got))
	}
	// A cut that lands after a space does not leave two spaces before the suffix.
	spaced := strings.Repeat("a", maxTemplateNameChars-19) + " " + strings.Repeat("b", 18)
	if got := conflictCopyName(spaced); strings.Contains(got, "  (") || utf8.RuneCountInString(got) > maxTemplateNameChars {
		t.Errorf("spaced copy name = %q", got)
	}
}

// Every tool, not only submit, works for a composition that is writing: one
// that finished or was cancelled reads and renders nothing more.
func TestComposeSubmitRefusedUnlessWriting(t *testing.T) {
	for _, state := range []string{"done", "failed"} {
		for _, tool := range []string{"context", "render", "submit"} {
			cw := newComposeWorld(t)
			cw.withKit()
			id := cw.start("builtin:article", []string{cw.addFile("hero.png", "image/png", previewPNG(t, 6, 4))}, "")
			cw.setState(id, state)
			writes := len(cw.engine.writes())
			var c map[string]any
			if tool != "context" {
				c = articleComposition("")
			}
			_, err := cw.tool(tool, id, c)
			if !errors.Is(err, errCompositionNotWriting) || err.Error() != composeNotWriting {
				t.Errorf("%s %s: err = %v", state, tool, err)
			}
			if len(cw.engine.writes()) != writes {
				t.Errorf("%s %s: a refused call wrote something", state, tool)
			}
			if limited, unbounded := cw.blobs.reads(); limited+unbounded != 0 {
				t.Errorf("%s %s: the material was read", state, tool)
			}
		}
	}

	// Cancelled while the composition was being saved: the state is read again
	// under the template's lock.
	cw := newComposeWorld(t)
	cw.withKit()
	id := cw.start("builtin:article", nil, "")
	gate := cw.gate
	cw.w.templateGate = func(ctx context.Context, key string) (func(), error) {
		if key == id {
			cw.engine.edit(id, func(row map[string]any) {
				c, _ := compositionFromRow(row)
				c.State, c.Error = "failed", "Cancelled"
				row["composition"], _ = c.object()
			})
		}
		return gate.acquire(ctx, key)
	}
	writes := len(cw.engine.writes())
	if _, err := cw.tool("submit", id, articleComposition("")); !errors.Is(err, errCompositionNotWriting) {
		t.Errorf("cancelled at the lock: err = %v", err)
	}
	if len(cw.engine.writes()) != writes {
		t.Error("a submit refused under the lock wrote something")
	}
}

func TestComposeSubmitNeverPublishes(t *testing.T) {
	cw := newComposeWorld(t)
	cw.withKit()
	hero := previewPNG(t, 6, 4)
	heroID := cw.addFile("hero.png", "image/png", hero)
	id := cw.start("builtin:article", []string{heroID}, "")

	// An email with nothing left to fix is still saved as a draft.
	receipt := cw.must("submit", id, articleComposition(compose.EmailAssetHandle(hero)))
	if issues := listOf(receipt["issues"]); len(issues) != 0 {
		t.Fatalf("the fixture has issues: %v", issues)
	}
	if _, said := receipt["status"]; said && receipt["status"] != "draft" {
		t.Errorf("receipt status = %v", receipt["status"])
	}
	for _, write := range cw.engine.writes() {
		if _, args := parseRenderedCall(write.query); args["status"] == "ready" {
			t.Errorf("a write publishes: %.120s", write.query)
		}
	}
	if row := cw.engine.row(id); row["status"] != "draft" {
		t.Errorf("status = %v", row["status"])
	}

	// The copy saved beside a changed template is a draft too.
	cw = newComposeWorld(t)
	cw.withKit()
	heroID = cw.addFile("hero.png", "image/png", hero)
	id = cw.start("builtin:article", []string{heroID}, "")
	cw.engine.edit(id, func(row map[string]any) { row["createdAt"] = templateNow.Add(time.Hour).Format(time.RFC3339Nano) })
	receipt = cw.must("submit", id, articleComposition(compose.EmailAssetHandle(hero)))
	if copied := cw.engine.row(fmt.Sprint(receipt["templateId"])); copied["status"] != "draft" {
		t.Errorf("copy status = %v", copied["status"])
	}
}

func TestComposeToolsRefuseAnotherOwnersTemplate(t *testing.T) {
	// set answers the template and the goal the call is stamped with.
	setups := []struct {
		name string
		want string
		set  func(cw *composeWorld) (string, string)
	}{
		{"no template stamped", composeNoTemplate, func(cw *composeWorld) (string, string) { return "", composeGoal }},
		{"no goal stamped", composeNoTemplate, func(cw *composeWorld) (string, string) {
			return cw.start("builtin:article", []string{cw.addFile("hero.png", "image/png", previewPNG(cw.t, 6, 4))}, ""), ""
		}},
		{"a template the caller cannot see", composeMissing, func(cw *composeWorld) (string, string) { return "t-unseen", composeGoal }},
		{"another organization's template", composeNoOrg, func(cw *composeWorld) (string, string) {
			c := sampleComposition()
			object, _ := c.object()
			cw.engine.rows["t-other"] = map[string]any{
				"id": "v1:campaigns:template:t-other", "ownerUserId": "v1:identity:user:someone-else",
				"accountId": "v1:accounts:account:other-org", "name": "Theirs", "composition": object,
				"createdAt": templateStored.Format(time.RFC3339Nano), "status": "draft",
			}
			return "t-other", c.GoalID
		}},
		{"no write permission in the organization", templateNeeds, func(cw *composeWorld) (string, string) {
			id := cw.start("builtin:article", []string{cw.addFile("hero.png", "image/png", previewPNG(cw.t, 6, 4))}, "")
			cw.engine.capable = func(string, string, string) bool { return false }
			return id, composeGoal
		}},
		{"a template MemQL is not writing", composeNotComposing, func(cw *composeWorld) (string, string) {
			id, _, _, err := cw.w.createTemplateFromLayout(importCtx(), templateOrg, "Plain", "builtin:article", nil)
			if err != nil {
				cw.t.Fatal(err)
			}
			return id, composeGoal
		}},
	}
	for _, setup := range setups {
		for _, tool := range []string{"context", "render", "submit"} {
			t.Run(setup.name+"/"+tool, func(t *testing.T) {
				cw := newComposeWorld(t)
				cw.withKit()
				id, goal := setup.set(cw)
				writes := len(cw.engine.writes())
				var c map[string]any
				if tool != "context" {
					c = articleComposition("")
				}
				_, err := cw.toolFor(tool, id, goal, c)
				if err == nil || err.Error() != setup.want {
					t.Fatalf("err = %v, want %q", err, setup.want)
				}
				if len(cw.engine.writes()) != writes {
					t.Error("a refused call wrote something")
				}
				// Nothing of the material is read for somebody who may not write.
				if limited, unbounded := cw.blobs.reads(); limited+unbounded != 0 {
					t.Errorf("storage was read %d times", limited+unbounded)
				}
			})
		}
	}
}

func TestComposeToolsAuthorizeByOrganizationNotByLastWriter(t *testing.T) {
	cw := newComposeWorld(t)
	cw.withKit()
	hero := previewPNG(t, 6, 4)
	id := cw.start("builtin:article", []string{cw.addFile("hero.png", "image/png", hero)}, "")
	// A colleague saved last: the row's owner is the last writer, not the composer.
	cw.engine.edit(id, func(row map[string]any) { row["ownerUserId"] = "v1:identity:user:colleague-1" })
	cw.must("context", id, nil)
	cw.must("render", id, articleComposition(compose.EmailAssetHandle(hero)))
	cw.must("submit", id, articleComposition(compose.EmailAssetHandle(hero)))
}

// A session whose run was cancelled, still talking after the composition was
// retried under a new goal, acts on nothing: every tool holds the call to the
// goal of the composition that is writing.
func TestComposeToolsRefuseAStaleGoal(t *testing.T) {
	for _, tool := range []string{"context", "render", "submit"} {
		cw := newComposeWorld(t)
		cw.withKit()
		hero := previewPNG(t, 6, 4)
		id := cw.start("builtin:article", []string{cw.addFile("hero.png", "image/png", hero)}, "")
		retried, _ := compositionFromRow(cw.engine.row(id))
		retried.GoalID = "goal-2"
		if _, err := cw.w.writeComposition(importCtx(), templateOrg, id, retried); err != nil {
			t.Fatal(err)
		}
		writes := len(cw.engine.writes())
		var c map[string]any
		if tool != "context" {
			c = articleComposition(compose.EmailAssetHandle(hero))
		}
		_, err := cw.toolFor(tool, id, composeGoal, c)
		if !errors.Is(err, errCompositionNotWriting) || err.Error() != composeNotWriting {
			t.Errorf("%s with the old goal: err = %v", tool, err)
		}
		if len(cw.engine.writes()) != writes {
			t.Errorf("%s with the old goal wrote something", tool)
		}
		if limited, unbounded := cw.blobs.reads(); limited+unbounded != 0 {
			t.Errorf("%s with the old goal read the material", tool)
		}
		// The retried run's own session is answered, its goal written either way.
		if _, err := cw.toolFor(tool, id, "v1:work:goal:goal-2", c); err != nil {
			t.Errorf("%s with the new goal: %v", tool, err)
		}
	}
}

func TestComposeMaterialReadsAtMost64Files(t *testing.T) {
	cw := newComposeWorld(t)
	hero := cw.addFile("hero.png", "image/png", previewPNG(t, 6, 4))
	ids := []string{hero}
	for len(ids) < maxMaterialFiles {
		ids = append(ids, fmt.Sprintf("gone-%d", len(ids)))
	}
	// 64 files, a repeated one and the picture of the finished email are read.
	m, err := cw.w.compositionMaterial(importCtx(), compositionRequest{FileIDs: append(ids, hero), ReferenceFileID: cw.addFile("finished.png", "image/png", previewPNG(t, 7, 9))})
	if err != nil || len(m.Files) != 2 {
		t.Fatalf("64 files: %d kept, %v", len(m.Files), err)
	}
	before := len(cw.engine.calls("query libraryFileById"))
	_, err = cw.w.compositionMaterial(importCtx(), compositionRequest{FileIDs: append(ids, "one-more")})
	if err == nil || err.Error() != "An email is composed from at most 64 attached files. Attach fewer." {
		t.Errorf("65 files: %v", err)
	}
	if after := len(cw.engine.calls("query libraryFileById")); after != before {
		t.Errorf("65 files: %d Library reads before the refusal", after-before)
	}
}

// A call reads its material and renders in ONE of the render slots previews
// and saves take: none is read while every slot is taken, and a submit with a
// single slot free never needs a second.
func TestComposeToolsTakeOneRenderSlot(t *testing.T) {
	cw := newComposeWorld(t)
	cw.withKit()
	hero := previewPNG(t, 6, 4)
	id := cw.start("builtin:article", []string{cw.addFile("hero.png", "image/png", hero)}, "")
	setDuration(t, &renderSlotWait, 50*time.Millisecond)

	holdRenderSlots(t, maxConcurrentRenders)
	for _, tool := range []string{"context", "render", "submit"} {
		var c map[string]any
		if tool != "context" {
			c = articleComposition(compose.EmailAssetHandle(hero))
		}
		if _, err := cw.tool(tool, id, c); err == nil || err.Error() != "Composing is busy right now. Try again in a moment." {
			t.Errorf("%s with every slot taken: %v", tool, err)
		}
	}
	if limited, unbounded := cw.blobs.reads(); limited+unbounded != 0 {
		t.Error("the material was read without a slot")
	}

	<-renderSlots // one slot free; the cleanup takes it back
	t.Cleanup(func() { renderSlots <- struct{}{} })
	cw.must("context", id, nil)
	cw.must("render", id, articleComposition(compose.EmailAssetHandle(hero)))
	cw.must("submit", id, articleComposition(compose.EmailAssetHandle(hero)))
	if _, held := cw.gate.snapshot(); held != 0 || len(renderSlots) != maxConcurrentRenders-1 {
		t.Errorf("after the calls: %d gates and %d slots held", held, len(renderSlots))
	}
}

func TestComposeMaterialLeavesOutWhatItCannotUse(t *testing.T) {
	cw := newComposeWorld(t)
	big := noisyPNG(t)
	good := cw.addFile("hero.png", "image/png", previewPNG(t, 6, 4))
	ids := []string{
		good,
		cw.addFile("brochure.pdf", "application/pdf", []byte("%PDF-1.4")),
		cw.addFile("logo.svg", "image/svg+xml", []byte("<svg/>")),
		cw.addFile("broken.png", "image/png", []byte("not a picture")),
		cw.addFile("empty.txt", "text/plain", nil),
		cw.addFile("bad.zip", "application/zip", []byte("not an archive")),
		cw.addFile("big.png", "image/png", big),
		cw.addFile("latin1.txt", "text/plain", []byte{0xe9, 0x74, 0xe9}),
		cw.addFile("bundle.zip", "application/zip", zipOf(t, zipEntry{"brochure.pdf", "%PDF-1.4"})),
		"file-gone",
	}
	archived := cw.addFile("old.png", "image/png", previewPNG(t, 2, 2))
	cw.engine.libraryFiles[archived]["archived"] = true
	huge := cw.addFile("huge.png", "image/png", previewPNG(t, 3, 3))
	cw.engine.libraryFiles[huge]["size"] = 65 << 20
	pending := cw.addFile("pending.png", "image/png", previewPNG(t, 3, 2))
	cw.engine.libraryFiles[pending]["blobUrl"] = ""
	ids = append(ids, archived, huge, pending)
	reference := cw.addFile("finished.png", "image/png", big)

	m, err := cw.w.compositionMaterial(importCtx(), compositionRequest{LayoutID: "builtin:article", FileIDs: ids, ReferenceFileID: reference})
	if err != nil {
		t.Fatal(err)
	}
	var kept []string
	for _, f := range m.Files {
		kept = append(kept, f.Workspace)
	}
	// The picture of the finished email is kept to look at, whatever its size;
	// it is guidance, and has no handle when it cannot go in an email.
	if !slices.Equal(kept, []string{"hero.png", "finished.png"}) || m.Reference != "finished.png" {
		t.Errorf("kept %v, reference %q", kept, m.Reference)
	}
	if len(m.Handles) != 1 {
		t.Errorf("handles = %d, want the one picture an email can carry", len(m.Handles))
	}
	wantNotes := []string{
		"Left out brochure.pdf: not a picture or text file.",
		"Left out logo.svg: svg is not a picture an email can carry.",
		"Left out broken.png: not a valid picture.",
		"Left out empty.txt: empty file.",
		"Left out bad.zip: invalid ZIP: zip: not a valid zip file.",
		"Left out big.png: a picture over 1 MiB, more than an email can carry.",
		"Left out latin1.txt: not valid UTF-8 text.",
		// A ZIP member and a plain file are left out for the same reasons, in the same words.
		"Left out bundle__brochure.pdf: not a picture or text file.",
		"Left out file-gone: it is not in your Library.",
		"Left out old.png: it is archived.",
		"Left out huge.png: it is larger than 16 MiB.",
		"Left out pending.png: its contents are not stored yet.",
	}
	if notes := m.notes(); !slices.Equal(notes, wantNotes) {
		t.Errorf("notes =\n%s\nwant\n%s", strings.Join(notes, "\n"), strings.Join(wantNotes, "\n"))
	}
	// Each file is read as the caller, and with a limit. Storage is not asked
	// for a file left out for what it is, where it is or how large its row says
	// it is, a picture larger than an email can carry included (big.png): only
	// hero, broken, empty, bad.zip, latin1, bundle.zip and the reference, which
	// is read however large, are read.
	for _, c := range cw.engine.fakeEngine.calls {
		if strings.HasPrefix(c.query, "query libraryFileById") && c.actorID != testOwner {
			t.Errorf("a Library read ran as %q", c.actorID)
		}
	}
	if limited, unbounded := cw.blobs.reads(); limited != 7 || unbounded != 0 {
		t.Errorf("storage reads: %d limited, %d without a limit; want 7 and 0", limited, unbounded)
	}
}

// The attached files are at most 16 MiB together, as the New email page
// attaches them, decided from what their Library rows say before storage is
// asked for any: a file that would take them past it is left out, and the
// files after it are still taken while they fit. The picture of the finished
// email is not one of them. What storage gives back is held to the same
// budget, so a row that says less than its file holds takes nothing past it.
// A ZIP may still unpack to 64 MiB.
func TestComposeMaterialTakesAtMost16MiBOfAttachedFiles(t *testing.T) {
	cw := newComposeWorld(t)
	rowSays := func(id string, size int) { cw.engine.libraryFiles[id]["size"] = size }
	one := cw.addFile("one.txt", "text/plain", []byte("One."))
	two := cw.addFile("two.txt", "text/plain", []byte("Two."))
	three := cw.addFile("three.txt", "text/plain", []byte("Three."))
	four := cw.addFile("four.txt", "text/plain", []byte("Four."))
	rowSays(one, 7<<20)
	rowSays(two, 7<<20)
	rowSays(three, 3<<20) // past 16 MiB with the two before it
	reference := cw.addFile("finished.png", "image/png", previewPNG(t, 8, 8))
	rowSays(reference, 15<<20)

	m, err := cw.w.compositionMaterial(importCtx(), compositionRequest{LayoutID: "builtin:article", FileIDs: []string{one, two, three, four}, ReferenceFileID: reference})
	if err != nil {
		t.Fatal(err)
	}
	var kept []string
	for _, f := range m.Files {
		kept = append(kept, f.Workspace)
	}
	if want := []string{"one.txt", "two.txt", "four.txt", "finished.png"}; !slices.Equal(kept, want) {
		t.Errorf("kept %v, want %v", kept, want)
	}
	if notes, want := m.notes(), []string{"Left out three.txt: the attached files together are larger than 16 MiB."}; !slices.Equal(notes, want) {
		t.Errorf("notes = %q, want %q", notes, want)
	}
	// Storage was asked for the files that fit by their rows, and never for
	// more than 16 MiB at a time.
	cw.blobs.mu.Lock()
	limits := slices.Clone(cw.blobs.limits)
	cw.blobs.mu.Unlock()
	if len(limits) != 4 {
		t.Errorf("storage reads %d, want one, two, four and the reference", len(limits))
	}
	for _, limit := range limits {
		if limit > 16<<20 {
			t.Errorf("a read with a limit of %d bytes", limit)
		}
	}

	// Rows that say less than their files hold: what storage gives back counts.
	cw = newComposeWorld(t)
	five := cw.addFile("five.txt", "text/plain", []byte(strings.Repeat("five ", 9<<18))) // 11.25 MiB
	six := cw.addFile("six.txt", "text/plain", []byte(strings.Repeat("six! ", 1<<20)))   // 5 MiB
	seven := cw.addFile("seven.txt", "text/plain", []byte("Seven."))
	rowSays(five, 10)
	rowSays(six, 10)
	m, err = cw.w.compositionMaterial(importCtx(), compositionRequest{LayoutID: "builtin:article", FileIDs: []string{five, six, seven}})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"Left out five.txt: more than 4 MiB of text.",
		"Left out six.txt: the attached files together are larger than 16 MiB.",
	}
	if notes := m.notes(); !slices.Equal(notes, want) || len(m.Files) != 1 || m.Files[0].Workspace != "seven.txt" {
		t.Errorf("notes = %q, files %d", notes, len(m.Files))
	}
}

// A composition reads data URIs only for the pictures it puts in the email:
// the material keeps every picture an email could carry by its handle, and
// the email is made from the handles the composition names.
func TestComposeImagesAreOnlyThePicturesTheCompositionUses(t *testing.T) {
	hero, other := previewPNG(t, 6, 4), previewPNG(t, 5, 5)
	m := material{Handles: map[string]materialPicture{
		compose.EmailAssetHandle(hero):  {mime: "image/png", data: hero},
		compose.EmailAssetHandle(other): {mime: "image/png", data: other},
	}}
	banner := compose.EmailAssetHandle(hero)
	images := m.images(emaildoc.Composition{Content: []emaildoc.ContentItem{{ID: "banner", Image: &banner}, {ID: "intro"}}})
	if len(images) != 1 || images[banner] != dataURI("image/png", hero) {
		t.Errorf("images = %d entries, want the banner's alone", len(images))
	}
	if images := m.images(emaildoc.Composition{}); len(images) != 0 {
		t.Errorf("a composition with no pictures got %d", len(images))
	}
	missing := "memql-asset:nothing"
	if images := m.images(emaildoc.Composition{Content: []emaildoc.ContentItem{{ID: "banner", Image: &missing}}}); len(images) != 0 {
		t.Errorf("a handle the material does not have got %d", len(images))
	}
}

func TestComposeMaterialReadsOnlyTheCallersFiles(t *testing.T) {
	cw := newComposeWorld(t)
	theirs := cw.addFile("theirs.png", "image/png", previewPNG(t, 6, 4))
	cw.engine.pictureOwner = "colleague-1" // the Library shows the file to its owner alone
	_, err := cw.w.compositionMaterial(importCtx(), compositionRequest{FileIDs: []string{theirs}})
	if err == nil || err.Error() != "None of the attached files can be used. Left out "+theirs+": it is not in your Library." {
		t.Errorf("err = %v", err)
	}
	if limited, unbounded := cw.blobs.reads(); limited+unbounded != 0 {
		t.Error("storage was read for a file the caller cannot see")
	}
	// No files at all is a composition written from its description alone.
	if m, err := cw.w.compositionMaterial(importCtx(), compositionRequest{}); err != nil || len(m.Files) != 0 {
		t.Errorf("no files: %v %v", m.Files, err)
	}
}

func TestWriteCompositionKeepsItsBaseRevisionInStep(t *testing.T) {
	cw := newComposeWorld(t)
	id := cw.start("builtin:article", nil, "")
	row := cw.engine.row(id)
	c, _ := compositionFromRow(row)
	// A composition that started from the row as it is follows its own writes...
	if !c.BaseRevision.Equal(templateTime(row["createdAt"])) {
		t.Errorf("base %v, row %v", c.BaseRevision, row["createdAt"])
	}
	// ...and one that started from an older revision does not catch up.
	cw.engine.edit(id, func(row map[string]any) { row["createdAt"] = templateNow.Add(time.Hour).Format(time.RFC3339Nano) })
	cw.setState(id, "writing")
	after, _ := compositionFromRow(cw.engine.row(id))
	if !after.BaseRevision.Equal(c.BaseRevision) {
		t.Errorf("base moved from %v to %v over another writer's revision", c.BaseRevision, after.BaseRevision)
	}
}

func TestComposeCapabilitiesAreRegistered(t *testing.T) {
	cw := newComposeWorld(t)
	found := map[string]memql.IntegrationCapability{}
	for _, c := range cw.w.Capabilities() {
		found[c.Name] = c
	}
	for _, name := range []string{"composeContext", "composeRender", "composeSubmit"} {
		if found[name].Handler == nil {
			t.Errorf("%s is not registered", name)
		}
	}
}

// TestComposeToolsLoadWithTheirBuiltins boots the shipped DSL with this
// integration registered: each tool calls its builtin, is an agent's
// (specialist) and has its template stamped by the server, and each builtin's
// executor is a capability the integration offers (the boot audit).
func TestComposeToolsLoadWithTheirBuiltins(t *testing.T) {
	if _, err := memql.LoadUnifiedConcepts(nil); err != nil {
		t.Fatalf("LoadUnifiedConcepts: %v", err)
	}
	eng, err := memql.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	eng.Logger = quietLogger()
	if err := eng.Init(memorynodes.DefaultRegistry()); err != nil {
		t.Fatalf("engine init: %v", err)
	}
	if err := eng.RegisterIntegration(newTestWorker(t, &fakeEngine{}, &recordingSender{})); err != nil {
		t.Fatal(err)
	}
	if err := eng.AuditIntegrationExecutors(); err != nil {
		t.Fatalf("executor audit: %v", err)
	}
	for tool, capability := range map[string]string{
		"emailComposeContext": "composeContext", "emailComposeRender": "composeRender", "emailComposeSubmit": "composeSubmit",
	} {
		builtin := "campaignC" + strings.TrimPrefix(capability, "c")
		got, err := eng.Tools().Get(tool)
		if err != nil {
			t.Fatalf("tool %s: %v", tool, err)
		}
		if got.Handler == nil || got.Handler.Type != "function" || got.Handler.FunctionName != builtin {
			t.Errorf("%s handler = %+v, want function %s", tool, got.Handler, builtin)
		}
		stamped := slices.Sorted(slices.Values(got.AutoInjectedFields))
		if !slices.Equal(got.RequiresAgentRole, []string{"specialist"}) || !slices.Equal(stamped, []string{"goalId", "templateId"}) {
			t.Errorf("%s: roles %v, stamped %v", tool, got.RequiresAgentRole, got.AutoInjectedFields)
		}
		fn, err := eng.Functions().Get(builtin)
		if err != nil || fn == nil || !fn.IsBuiltin() || fn.Executor != "integration.campaigns."+capability {
			t.Errorf("builtin %s: %+v %v", builtin, fn, err)
		}
	}
}
