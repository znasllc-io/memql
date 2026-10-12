package campaigns

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/pgdialect"
	"github.com/uptrace/bun/driver/pgdriver"
	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/compose/emaildoc"
	"github.com/znasllc-io/memql/component/database/dbtest"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	langparser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/memql"
)

func TestTemplatePublicationChecksExactRevisionAcrossReplicas(t *testing.T) {
	db := bun.NewDB(sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(dbtest.DSN()))), pgdialect.New())
	defer db.Close()
	ping, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := db.PingContext(ping); err != nil {
		dbtest.Unreachable(t, "template versions", dbtest.DSN(), err)
	}
	if _, err := memql.LoadUnifiedConcepts(nil); err != nil {
		t.Fatal(err)
	}
	engines := make([]*memql.MemQLEngine, 2)
	workers := make([]*Worker, 2)
	for n := range engines {
		e, err := memql.New(db)
		if err != nil {
			t.Fatal(err)
		}
		e.Logger = quietLogger()
		if err := e.Init(memorynodes.DefaultRegistry()); err != nil {
			t.Fatal(err)
		}
		engines[n] = e
		workers[n] = NewWorker(deliveryDBEngine{e}, nil, nil, quietLogger(), func() *sql.DB { return db.DB })
	}
	workers[0].now = func() time.Time { return time.Date(2029, 1, 1, 0, 0, 0, 0, time.UTC) }
	workers[1].now = func() time.Time { return time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC) }
	suffix := fmt.Sprintf("template-%d", time.Now().UnixNano())
	user, account, template := suffix+"-user", suffix+"-org", suffix+"-template"
	owner := auth.ContextWithToken(auth.ContextWithAccess(context.Background(), &auth.AccessContext{UserId: user, Role: auth.RoleOwner}), &auth.TokenInfo{Subject: user})
	run := func(ctx context.Context, q string) {
		t.Helper()
		if _, err := engines[0].Execute(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	system := auth.ContextWithInternalOrigin(auth.ContextWithAccess(context.Background(), auth.SystemActor("template-tests")))
	system = auth.ContextWithToken(system, &auth.TokenInfo{Subject: "template-tests"})
	run(system, fmt.Sprintf(`insert("v1:identity:user", id=%s, payload={"primaryEmail":"%s@example.test","displayName":"Template owner","role":"owner","active":true})`, langparser.QuoteString(user), user))
	run(owner, fmt.Sprintf(`mutation createClientAccount(accountId: %s, name: "Template client")`, langparser.QuoteString(account)))
	content := func(subject string) map[string]any {
		return map[string]any{"subject": subject, "textBody": "Welcome", "htmlBody": "<p>Welcome</p>"}
	}
	args := func(revision, subject, action string) map[string]any {
		return map[string]any{"templateId": template, "accountId": account, "name": "Welcome", "content": content(subject), "expectedRevision": revision, "action": action}
	}
	save := func(w *Worker, data map[string]any) string {
		t.Helper()
		rows, err := w.handleSaveTemplate(owner, data, 0)
		if err != nil {
			t.Fatal(err)
		}
		if len(rows) != 1 {
			t.Fatalf("receipt: %v", rows)
		}
		var decoded map[string]any
		if err := json.Unmarshal(rows[0].Payload, &decoded); err != nil {
			t.Fatal(err)
		}
		return str(decoded, "revision")
	}
	first := save(workers[0], args("", "First", "save"))
	if first == "" {
		t.Fatal("missing revision")
	}
	if retry := save(workers[1], args("", "First", "save")); retry != first {
		t.Fatal("creation retry rewrote the draft")
	}
	if _, err := engines[0].Execute(owner, fmt.Sprintf(`mutation updateTemplate(templateId: %s, name: "Bypass", subject: "No", textBody: "No", status: "ready", versionTime: "2031-01-01T00:00:00Z")`, langparser.QuoteString(template))); err == nil {
		t.Fatal("client bypassed capability")
	}
	if _, err := engines[0].Execute(owner, fmt.Sprintf(`insert("v1:campaigns:template", id=%s, payload={"name":"Bypass","subject":"No","textBody":"No","status":"ready","accountId":%s})`, langparser.QuoteString(template), langparser.QuoteString(account))); err == nil {
		t.Fatal("raw template write bypassed revision gate")
	}
	var wg sync.WaitGroup
	errors := make(chan error, 2)
	for n, w := range workers {
		wg.Add(1)
		go func(n int, w *Worker) {
			defer wg.Done()
			_, err := w.handleSaveTemplate(owner, args(first, fmt.Sprintf("Replica %d", n), "save"), 0)
			errors <- err
		}(n, w)
	}
	wg.Wait()
	close(errors)
	successes, conflicts := 0, 0
	for err := range errors {
		if err == nil {
			successes++
		} else if strings.Contains(err.Error(), "changed") {
			conflicts++
		} else {
			t.Fatal(err)
		}
	}
	if successes != 1 || conflicts != 1 {
		t.Fatalf("got %d successes, %d conflicts", successes, conflicts)
	}
	rows, err := workers[1].store.rows(memql.ContextWithFreshRead(owner), call("query", "templateById", arg{"templateId", template}))
	if err != nil || len(rows) != 1 {
		t.Fatalf("read: %v %v", rows, err)
	}
	row := rows[0]
	current := templateTime(row["createdAt"]).Format(time.RFC3339Nano)
	subject := str(row, "subject")
	if !templateTime(row["createdAt"]).After(templateTime(first)) {
		t.Fatal("lagging replica wrote behind its read")
	}
	if _, err := workers[1].handleSaveTemplate(owner, args(first, subject, "publish"), 0); err == nil {
		t.Fatal("published an unreviewed revision")
	}
	if _, err := workers[1].handleSaveTemplate(owner, args(current, "Unsaved change", "publish"), 0); err == nil {
		t.Fatal("published unsaved content")
	}
	published := save(workers[1], args(current, subject, "publish"))
	edited := save(workers[0], args(published, "Revised", "save"))
	if edited == published {
		t.Fatal("edit did not advance revision")
	}
	wrongOrg := args(edited, "Revised", "save")
	wrongOrg["accountId"] = "missing-organization"
	if _, err := workers[0].handleSaveTemplate(owner, wrongOrg, 0); err == nil {
		t.Fatal("moved a template across organizations")
	}
	denied := auth.ContextWithAccess(context.Background(), &auth.AccessContext{UserId: suffix + "-outsider", Role: auth.RoleWriter})
	if _, err := workers[1].handleSaveTemplate(denied, args(edited, "Stolen", "save"), 0); err == nil {
		t.Fatal("unrelated writer edited client template")
	}
}

// A template made from an email document, against real engines: the parts
// template_test.go's fake cannot say. Two engines share one database, as two
// replicas do. The email is a built-in layout with its pictures chosen and every
// link given an address, so nothing keeps it from being published; it is saved
// from a Library file, so the file says which template it is.
func TestTemplateSaveRendersDocumentsAndPublishesBySource(t *testing.T) {
	w := newLayoutWorld(t)
	previousGrants, previousMembers := auth.InstalledGrantSource(), auth.InstalledMembershipSource()
	w.engines[0].InstallGrantResolution()
	t.Cleanup(func() { auth.SetGrantSource(previousGrants); auth.SetMembershipSource(previousMembers) })

	account, other := w.suffix+"-org", w.suffix+"-other-org"
	source := "art-" + w.suffix
	w.createOrganization(t, account, "Template client")
	w.createOrganization(t, other, "Another client")
	stranger := w.suffix + "-stranger"
	w.seedUser(t, stranger, "writer")
	strangerCtx := layoutActor(stranger, auth.RoleWriter)
	fresh := func(ctx context.Context) context.Context { return memql.ContextWithFreshRead(ctx) }

	document := readyConfirmation(t)
	// args is a save from the file with no template id, which the file makes.
	args := func(accountID, revision, action string, mutate ...func(map[string]any)) map[string]any {
		call := map[string]any{
			"accountId": accountID, "name": "Welcome email", "content": document, "sourceArtifactId": source,
			"expectedRevision": revision, "action": action,
		}
		for _, m := range mutate {
			m(call)
		}
		return call
	}
	save := func(replica int, ctx context.Context, call map[string]any) savedTemplate {
		t.Helper()
		nodes, err := w.workers[replica].handleSaveTemplate(ctx, call, 0)
		if err != nil {
			t.Fatalf("%v on replica %d: %v", call["action"], replica, err)
		}
		return decodeSaved(t, nodes)
	}
	templatesOfTheFile := func(accountID string) int {
		t.Helper()
		var n int
		// The organization is stored as the canonical id the engine makes of it.
		err := w.db.QueryRowContext(context.Background(),
			`SELECT count(DISTINCT id) FROM "MemoryNodes" WHERE concept = 'v1:campaigns:template' AND payload->>'accountId' IN (?, ?) AND payload->>'sourceArtifactId' = ?`,
			accountID, "v1:accounts:account:"+accountID, source).Scan(&n)
		if err != nil {
			t.Fatal(err)
		}
		return n
	}
	// ofTheFile is the template of the file, whole: templateForSource says which
	// template it is and at which revision, and templateById reads it.
	ofTheFile := func(replica int, ctx context.Context, accountID string) (map[string]any, bool) {
		t.Helper()
		ref, found, err := w.workers[replica].store.TemplateForSource(fresh(ctx), accountID, source)
		if err != nil {
			t.Fatalf("templateForSource on replica %d: %v", replica, err)
		}
		if !found {
			return nil, false
		}
		rows, err := w.workers[replica].store.rows(fresh(ctx), call("query", "templateById", arg{"templateId", bare(str(ref, "id"))}))
		if err != nil || len(rows) != 1 {
			t.Fatalf("templateById on replica %d: %v %v", replica, rows, err)
		}
		if templateTime(rows[0]["createdAt"]) != templateTime(ref["createdAt"]) {
			t.Fatalf("the lookup says revision %v and the template is at %v", ref["createdAt"], rows[0]["createdAt"])
		}
		return rows[0], true
	}

	// Nothing is published from the file yet, here or in the other organization.
	if _, found := ofTheFile(1, w.owner, account); found {
		t.Fatal("a template was found for a file nobody published")
	}

	// 1. The first save, with no template id: the file makes the template, under
	// the id Rulings 14 names, and the other replica sees it.
	sum := sha256.Sum256([]byte("memql/campaigns/template-source\x00" + account + "\x00" + source))
	derived := hex.EncodeToString(sum[:])[:32]
	first := save(0, w.owner, args(account, "", "save"))
	if first.TemplateID != derived || first.Status != "draft" || first.Revision == "" || len(first.Issues) != 0 || string(first.raw["issues"]) != "[]" {
		t.Fatalf("first save = %+v, want a draft of template %s with no issues", first, derived)
	}
	row, found := ofTheFile(1, w.owner, account)
	if !found || bare(str(row, "id")) != derived {
		t.Fatalf("templateForSource on the other replica = %v, %v", row, found)
	}
	if templateTime(row["createdAt"]).Format(time.RFC3339Nano) != first.Revision {
		t.Errorf("the revision of the row is %v, not the receipt's %s", row["createdAt"], first.Revision)
	}
	html := str(row, "htmlBody")
	if !strings.HasPrefix(html, "<!DOCTYPE html") || strings.Contains(html, "data-block=") || !strings.Contains(html, "data:image/png;base64,") {
		t.Errorf("the stored HTML is not the send-mode render with its pictures embedded:\n%.300s", html)
	}
	if !strings.Contains(str(row, "textBody"), "Confirm my address: {{confirmUrl}}") ||
		str(row, "layoutId") != "builtin:confirmation" || str(row, "preheader") != document["preheader"] ||
		str(row, "subject") != document["subject"] || bare(str(row, "sourceArtifactId")) != source || str(row, "status") != "draft" {
		t.Errorf("the stored template is not what the document makes: %v", row)
	}
	stored, _ := row["document"].(map[string]any)
	if stored == nil || stored["layout"] != "builtin:confirmation" || !sameJSON(t, stored, canonicalDocument(t, document)) {
		t.Errorf("the stored document is not the document that was sent: %v", stored)
	}

	// 2. A client that holds the id and a stale revision is told to look again;
	// publishing needs the exact revision and the exact content.
	if _, err := w.workers[1].handleSaveTemplate(w.owner, args(account, "2020-01-01T00:00:00Z", "publish"), 0); err == nil || !strings.Contains(err.Error(), "changed") {
		t.Fatalf("a stale revision: %v", err)
	}
	reviewed := readyConfirmation(t)
	reviewed["subject"] = "A subject nobody saved"
	if _, err := w.workers[1].handleSaveTemplate(w.owner, args(account, first.Revision, "publish", func(c map[string]any) { c["content"] = reviewed }), 0); err == nil || !strings.Contains(err.Error(), "save and review") {
		t.Fatalf("publishing unsaved content: %v", err)
	}
	ready := save(1, w.owner, args(account, first.Revision, "publish"))
	if ready.TemplateID != derived || ready.Status != "ready" || ready.Revision == first.Revision || len(ready.Issues) != 0 {
		t.Fatalf("publication = %+v", ready)
	}
	if row, found := ofTheFile(0, w.owner, account); !found || str(row, "status") != "ready" {
		t.Fatalf("the template of the file after publishing: %v, %v", row, found)
	}

	// 3. Publishing the file again updates its template. The call carries no id
	// at all, and a second one that carries somebody else's makes no difference.
	document["subject"] = "Welcome, second edition"
	second := save(0, w.owner, args(account, ready.Revision, "save"))
	if second.TemplateID != derived || second.Status != "draft" || second.Revision == ready.Revision {
		t.Fatalf("second save = %+v, want the same template, a draft, at a newer revision", second)
	}
	third := save(1, w.owner, args(account, second.Revision, "save", func(c map[string]any) {
		c["templateId"] = w.suffix + "-somebody-elses-template"
		c["name"] = "Welcome email, renamed"
	}))
	if third.TemplateID != derived {
		t.Fatalf("a call that named another template wrote %s", third.TemplateID)
	}
	row, found = ofTheFile(0, w.owner, account)
	if !found || bare(str(row, "id")) != derived || str(row, "subject") != "Welcome, second edition" || str(row, "name") != "Welcome email, renamed" {
		t.Fatalf("templateForSource after the second and third saves: %v", row)
	}
	if n := templatesOfTheFile(account); n != 1 {
		t.Fatalf("%d templates were made from one file in one organization, want 1", n)
	}
	// The rows of the template are its versions, newest first, and the other
	// template id was never written.
	versions := func(id string) int {
		t.Helper()
		var n int
		if err := w.db.QueryRowContext(context.Background(), `SELECT count(*) FROM "MemoryNodes" WHERE concept = 'v1:campaigns:template' AND id = ?`, "v1:campaigns:template:"+id).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if got, none := versions(derived), versions(w.suffix+"-somebody-elses-template"); got != 4 || none != 0 {
		t.Errorf("versions of the template = %d (want 4: save, publish, save, save), of the other id = %d (want 0)", got, none)
	}

	// 4. The same file in another organization is another template, and the
	// first organization's stays as it was.
	elsewhere := save(1, w.owner, args(other, "", "save"))
	sum = sha256.Sum256([]byte("memql/campaigns/template-source\x00" + other + "\x00" + source))
	if elsewhere.TemplateID != hex.EncodeToString(sum[:])[:32] || elsewhere.TemplateID == derived {
		t.Errorf("the other organization's template is %s", elsewhere.TemplateID)
	}
	if templatesOfTheFile(account) != 1 || templatesOfTheFile(other) != 1 {
		t.Errorf("templates per organization: %d and %d, want one each", templatesOfTheFile(account), templatesOfTheFile(other))
	}
	// A template cannot be moved there by naming it.
	if _, err := w.workers[0].handleSaveTemplate(w.owner, args(other, third.Revision, "save", func(c map[string]any) { c["templateId"] = derived; c["sourceArtifactId"] = "" }), 0); err == nil || !strings.Contains(err.Error(), "a template cannot move between organizations") {
		t.Fatalf("a template moved to another organization: %v", err)
	}

	// 5. Archive and restore need the revision and nothing else, on either replica.
	away := save(1, w.owner, map[string]any{"templateId": derived, "accountId": account, "name": "ignored", "content": map[string]any{}, "expectedRevision": third.Revision, "action": "archive"})
	if away.Status != "archived" {
		t.Fatalf("archive = %+v", away)
	}
	// The subject is not in the HTML, so the HTML is still the first save's.
	if row, _ := ofTheFile(0, w.owner, account); str(row, "status") != "archived" || str(row, "subject") != "Welcome, second edition" || row["document"] == nil || str(row, "htmlBody") != html {
		t.Fatalf("archiving changed the content: %v", row)
	}
	back := save(0, w.owner, map[string]any{"templateId": derived, "accountId": account, "name": "ignored", "content": map[string]any{}, "expectedRevision": away.Revision, "action": "restore"})
	if back.Status != "draft" || back.Revision == away.Revision {
		t.Fatalf("restore = %+v", back)
	}
	if again := save(1, w.owner, map[string]any{"templateId": derived, "accountId": account, "name": "ignored", "content": map[string]any{}, "expectedRevision": away.Revision, "action": "restore"}); again.Revision != back.Revision {
		t.Fatalf("a repeated restore wrote another version: %s then %s", back.Revision, again.Revision)
	}

	// 6. Two replicas publishing a file nobody has published at the same moment
	// make one template, once: the file's gate serializes them, and the second
	// finds what the first made and answers with its receipt.
	fileTwo := "art-two-" + w.suffix
	var wg sync.WaitGroup
	receipts := make([]savedTemplate, 2)
	failures := make(chan error, 2)
	for n := range w.workers {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			nodes, err := w.workers[n].handleSaveTemplate(w.owner, args(account, "", "save", func(c map[string]any) { c["sourceArtifactId"] = fileTwo }), 0)
			if err != nil {
				failures <- err
				return
			}
			receipts[n] = decodeSaved(t, nodes)
		}(n)
	}
	wg.Wait()
	close(failures)
	for err := range failures {
		t.Errorf("a concurrent first save failed: %v", err)
	}
	if receipts[0].TemplateID == "" || receipts[0].TemplateID != receipts[1].TemplateID || receipts[0].Revision != receipts[1].Revision {
		t.Errorf("concurrent first saves = %+v and %+v, want one template at one revision", receipts[0], receipts[1])
	}
	if got := versions(receipts[0].TemplateID); got != 1 {
		t.Errorf("a template made twice at once has %d versions, want 1", got)
	}

	// 7. A person outside the organization does not find the template by its
	// file, and cannot save over it: the file is not a way into somebody else's
	// templates.
	if row, found, err := w.workers[1].store.TemplateForSource(fresh(strangerCtx), account, source); err == nil && found {
		t.Errorf("a stranger found the template of a file: %v", row)
	}
	if _, err := w.workers[1].handleSaveTemplate(strangerCtx, args(account, back.Revision, "save", func(c map[string]any) { c["name"] = "Stolen" }), 0); err == nil {
		t.Error("a stranger saved over an organization's template")
	}
	if row, _ := ofTheFile(0, w.owner, account); str(row, "name") == "Stolen" {
		t.Error("the stranger's save landed")
	}

	// 8. The builtin runs the whole way through the engine, as a client calls it:
	// no template id, the file, the document, and the revision it holds. The
	// answer is the receipt, with its list of issues.
	call, err := langparser.RenderCall("campaignSaveTemplate", map[string]any{
		"accountId": account, "name": "Welcome email", "content": document, "sourceArtifactId": source,
		"expectedRevision": back.Revision, "action": "save",
	})
	if err != nil {
		t.Fatal(err)
	}
	rows := memql.MaterializeRows(w.run(t, w.owner, "builtin "+call))
	if len(rows) != 1 || rows[0]["templateId"] != derived || rows[0]["saved"] != true || rows[0]["status"] != "draft" {
		t.Fatalf("campaignSaveTemplate through the engine answered %v", rows)
	}
	if issues, ok := rows[0]["issues"].([]any); !ok || len(issues) != 0 {
		t.Errorf("issues = %#v, want an empty list", rows[0]["issues"])
	}
	published, err := langparser.RenderCall("campaignSaveTemplate", map[string]any{
		"accountId": account, "name": "Welcome email", "content": document, "sourceArtifactId": source,
		"expectedRevision": rows[0]["revision"], "action": "publish",
	})
	if err != nil {
		t.Fatal(err)
	}
	rows = memql.MaterializeRows(w.run(t, w.owner, "builtin "+published))
	if len(rows) != 1 || rows[0]["status"] != "ready" {
		t.Fatalf("publishing through the engine answered %v", rows)
	}
	// And through the engine a template that cannot be published says why.
	broken := readyConfirmation(t)
	setBlock(t, broken, "hero", func(b map[string]any) { delete(b, "src") })
	refused, err := langparser.RenderCall("campaignSaveTemplate", map[string]any{
		"accountId": account, "name": "Welcome email", "content": broken, "sourceArtifactId": source,
		"expectedRevision": rows[0]["revision"], "action": "save",
	})
	if err != nil {
		t.Fatal(err)
	}
	rows = memql.MaterializeRows(w.run(t, w.owner, "builtin "+refused))
	if len(rows) != 1 || len(rows[0]["issues"].([]any)) != 1 {
		t.Fatalf("a draft with a picture to choose answered %v", rows)
	}
	issue := rows[0]["issues"].([]any)[0].(map[string]any)
	if issue["code"] != "image_missing" || issue["block"] != "hero" || issue["message"] != heroToChoose {
		t.Errorf("issue = %v", issue)
	}
	unpublishable, err := langparser.RenderCall("campaignSaveTemplate", map[string]any{
		"accountId": account, "name": "Welcome email", "content": broken, "sourceArtifactId": source,
		"expectedRevision": rows[0]["revision"], "action": "publish",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.engines[0].Execute(w.owner, "builtin "+unpublishable); err == nil || !strings.Contains(err.Error(), "fix these before publishing: "+heroToChoose) {
		t.Fatalf("publishing through the engine a template with a picture to choose: %v", err)
	}
}

// Review focus 3, against real engines: a picture is read as the person who
// saves, so a document that names one nobody but its uploader can read is a draft
// that says so, and is not published, whoever saves it and on whichever replica;
// and the uploader publishes the same document, with the picture embedded. The
// Library's named reads decide whose picture it is, and they scope even the
// owner of the cluster to their own rows
// (component/memql/library_file_4340_db_test.go).
func TestPublishReadsPicturesAsThePublisherThroughTheEngine(t *testing.T) {
	w := newLayoutWorld(t)
	previousGrants, previousMembers := auth.InstalledGrantSource(), auth.InstalledMembershipSource()
	w.engines[0].InstallGrantResolution()
	t.Cleanup(func() { auth.SetGrantSource(previousGrants); auth.SetMembershipSource(previousMembers) })

	account := w.suffix + "-org"
	w.createOrganization(t, account, "Picture client")
	uploaderID, colleagueID := w.suffix+"-uploader", w.suffix+"-colleague"
	for _, user := range []string{uploaderID, colleagueID} {
		w.seedUser(t, user, "writer")
		w.seedMember(t, user, account)
	}
	uploader, colleague := layoutActor(uploaderID, auth.RoleWriter), layoutActor(colleagueID, auth.RoleWriter)

	data := previewPNG(t, 6, 4)
	artifact, blobs := w.uploadPicture(t, uploader, uploaderID, w.suffix+"-file", data)

	doc := readyConfirmation(t)
	setBlock(t, doc, "hero", func(b map[string]any) { b["src"] = "artifact:" + artifact })
	args := func(templateID, revision, action string) map[string]any {
		return map[string]any{"templateId": templateID, "accountId": account, "name": "Picture email", "content": doc, "expectedRevision": revision, "action": action}
	}
	saveOn := func(replica int, ctx context.Context, call map[string]any) (savedTemplate, error) {
		nodes, err := w.workers[replica].handleSaveTemplate(ctx, call, 0)
		if err != nil {
			return savedTemplate{}, err
		}
		return decodeSaved(t, nodes), nil
	}
	embedded := "data:image/png;base64," + base64.StdEncoding.EncodeToString(data)

	// Somebody who cannot read the picture, who belongs to the organization (a
	// colleague) or is the owner of the cluster, saves a draft that says so.
	for name, who := range map[string]struct {
		ctx     context.Context
		replica int
	}{"a colleague in the same organization": {colleague, 1}, "the owner of the cluster": {w.owner, 0}} {
		t.Run(name+" cannot publish it", func(t *testing.T) {
			template := w.suffix + "-template-" + strings.ReplaceAll(name, " ", "-")
			before, _ := blobs.reads()
			draft, err := saveOn(who.replica, who.ctx, args(template, "", "save"))
			if err != nil {
				t.Fatal(err)
			}
			if len(draft.Issues) != 1 || !draft.hasIssue(emaildoc.IssueImageUnavailable, "hero") || draft.Issues[0].Message != heroUnreadable {
				t.Fatalf("issues = %+v, want the one for the hero picture", draft.Issues)
			}
			if _, err := saveOn(who.replica, who.ctx, args(template, draft.Revision, "publish")); err == nil || err.Error() != "fix these before publishing: "+heroUnreadable {
				t.Fatalf("publishing error = %v", err)
			}
			if after, _ := blobs.reads(); after != before {
				t.Error("storage was read for a picture the saver cannot see")
			}
			row, found, err := w.workers[0].store.TemplateByID(memql.ContextWithFreshRead(w.owner), template)
			if err != nil || !found || row.Status != "draft" || strings.Contains(row.HTMLBody, embedded) {
				t.Errorf("the stored template is %+v (found %v, %v): a draft without the picture", row, found, err)
			}
		})
	}

	// The person who uploaded it publishes the same document, on either replica.
	t.Run("the uploader publishes it", func(t *testing.T) {
		template := w.suffix + "-template-uploader"
		draft, err := saveOn(0, uploader, args(template, "", "save"))
		if err != nil || len(draft.Issues) != 0 {
			t.Fatalf("the uploader's draft: %+v, %v", draft, err)
		}
		ready, err := saveOn(1, uploader, args(template, draft.Revision, "publish"))
		if err != nil || ready.Status != "ready" || len(ready.Issues) != 0 {
			t.Fatalf("the uploader's publication: %+v, %v", ready, err)
		}
		row, found, err := w.workers[1].store.TemplateByID(memql.ContextWithFreshRead(uploader), template)
		if err != nil || !found || row.Status != "ready" || !strings.Contains(row.HTMLBody, embedded) {
			t.Errorf("the published template does not carry the picture: %+v (found %v, %v)", row.Status, found, err)
		}
		// The document keeps the reference to the file, not the picture.
		stored, err := w.workers[1].store.rows(memql.ContextWithFreshRead(uploader), call("query", "templateById", arg{"templateId", template}))
		if err != nil || len(stored) != 1 || !strings.Contains(fmt.Sprint(stored[0]["document"]), "artifact:"+artifact) || strings.Contains(fmt.Sprint(stored[0]["document"]), embedded) {
			t.Errorf("the stored document does not keep the artifact reference: %v %v", stored, err)
		}
	})
}

// uploadPicture is an upload, as the uploader's own act: the bytes in storage
// (the recordingBlobs every worker reads), the file row and the Library's index
// row for it. It returns the artifact id and the storage.
func (w *layoutWorld) uploadPicture(t *testing.T, uploader context.Context, uploaderID, file string, data []byte) (string, *recordingBlobs) {
	t.Helper()
	blobURL := "library/" + uploaderID + "/" + file + "/hero.png"
	blobs := &recordingBlobs{objects: map[string][]byte{blobURL: data}}
	for _, worker := range w.workers {
		worker.newBlobReader = func(context.Context) (blobReader, string, error) { return blobs, "memql", nil }
	}
	w.run(t, uploader, call("mutation", "createLibraryFile", arg{"fileId", file}, arg{"name", "hero.png"}, arg{"mimeType", "image/png"},
		arg{"size", len(data)}, arg{"blobUrl", blobURL}, arg{"source", "uploaded"}, arg{"format", "image"}))
	created, ok := w.run(t, uploader, call("mutation", "createArtifact", arg{"sourceConceptRef", "v1:library:file:" + file}, arg{"ownerUserId", uploaderID},
		arg{"lens", "artifact"}, arg{"kind", "file"}, arg{"source", "uploaded"}, arg{"title", "hero.png"}, arg{"format", "image"}, arg{"mimeType", "image/png"})).(*memql.ExecuteResult)
	if !ok || created.Bundle == nil || len(created.Bundle.Nodes) == 0 {
		t.Fatal("createArtifact answered no row")
	}
	return bare(created.Bundle.Nodes[0].Id), blobs
}

// A node's request pool ends a transaction that has been idle for a minute and
// cancels a statement that waits eight seconds (component/database sets both on
// every backend), and a template gate is a transaction left open. These tests run
// under the same two limits, at a scale a test can wait for.
var productionLikeSession = map[string]any{
	"idle_in_transaction_session_timeout": "2500", // 60 s in production
	"statement_timeout":                   "1500", // 8 s in production
}

// A save that renders slowly must not lose another person's save. The reviewer's
// reproduction: a holder idle past the transaction limit is terminated, and the
// next caller takes the same lock while the first still works; the first then
// writes over the second on the revision it had read. Here the render of one save
// waits for storage for longer than the limit while another save of the same
// revision lands, and the slow one must be refused for the revision, not win.
func TestASlowRenderCannotLoseASaveThatLandsMeanwhile(t *testing.T) {
	w := newLayoutWorldWith(t, productionLikeSession)
	previousGrants, previousMembers := auth.InstalledGrantSource(), auth.InstalledMembershipSource()
	w.engines[0].InstallGrantResolution()
	t.Cleanup(func() { auth.SetGrantSource(previousGrants); auth.SetMembershipSource(previousMembers) })
	account := w.suffix + "-org"
	w.createOrganization(t, account, "Slow client")
	uploaderID := w.suffix + "-uploader"
	w.seedUser(t, uploaderID, "writer")
	w.seedMember(t, uploaderID, account)
	uploader := layoutActor(uploaderID, auth.RoleWriter)
	artifact, blobs := w.uploadPicture(t, uploader, uploaderID, w.suffix+"-file", previewPNG(t, 6, 4))
	source := "art-" + w.suffix

	fast := readyConfirmation(t)
	save := func(replica int, ctx context.Context, content map[string]any, revision string) (savedTemplate, error) {
		nodes, err := w.workers[replica].handleSaveTemplate(ctx, map[string]any{
			"accountId": account, "name": "Slow email", "content": content, "sourceArtifactId": source,
			"expectedRevision": revision, "action": "save",
		}, 0)
		if err != nil {
			return savedTemplate{}, err
		}
		return decodeSaved(t, nodes), nil
	}
	first, err := save(0, uploader, fast, "")
	if err != nil {
		t.Fatalf("the first save: %v", err)
	}

	// The slow save reads its picture from storage, which waits.
	blobs.hold = make(chan struct{})
	blobs.entered = make(chan struct{}, 4)
	slowDoc := readyConfirmation(t)
	setBlock(t, slowDoc, "hero", func(b map[string]any) { b["src"] = "artifact:" + artifact })
	slowDoc["subject"] = "The slow save"
	slow := make(chan error, 1)
	go func() {
		_, err := save(0, uploader, slowDoc, first.Revision)
		slow <- err
	}()
	select {
	case <-blobs.entered:
	case <-time.After(10 * time.Second):
		t.Fatal("the render never read the picture")
	}
	// Nothing is locked while it waits. And it waits longer than the pool lets a
	// transaction sit idle, so a lock it held would be gone by now.
	var advisory int
	if err := w.db.QueryRowContext(context.Background(),
		`SELECT count(*) FROM pg_locks WHERE locktype = 'advisory' AND classid = 1296387409 AND database = (SELECT oid FROM pg_database WHERE datname = current_database())`).Scan(&advisory); err != nil {
		t.Fatal(err)
	}
	if advisory != 0 {
		t.Errorf("%d template gates are held while a render waits for storage", advisory)
	}
	time.Sleep(3 * time.Second)

	// Another save of the same revision lands meanwhile, on the other replica.
	landing := fast
	landing["subject"] = "The save that landed meanwhile"
	landed, err := save(1, uploader, landing, first.Revision)
	if err != nil {
		t.Fatalf("a save during a slow render: %v", err)
	}

	close(blobs.hold)
	if err := <-slow; err == nil || !strings.Contains(err.Error(), "changed") {
		t.Fatalf("the slow save passed the revision it had read (it wrote over the one that landed): %v", err)
	}
	row, found, err := w.workers[0].store.TemplateForSource(memql.ContextWithFreshRead(uploader), account, source)
	if err != nil || !found {
		t.Fatalf("templateForSource: %v %v", found, err)
	}
	stored, err := w.workers[0].store.rows(memql.ContextWithFreshRead(uploader), call("query", "templateById", arg{"templateId", bare(str(row, "id"))}))
	if err != nil || len(stored) != 1 {
		t.Fatalf("templateById: %v %v", stored, err)
	}
	if str(stored[0], "subject") != landing["subject"] || templateTime(stored[0]["createdAt"]).Format(time.RFC3339Nano) != landed.Revision {
		t.Errorf("the template is %q at %v, not the save that landed (%q at %s)", stored[0]["subject"], stored[0]["createdAt"], landing["subject"], landed.Revision)
	}
}

// A template gate that cannot be taken within the statement limit is a sentence for
// a person: the pool's own words (canceling statement due to statement timeout)
// are not.
func TestAGateThatTimesOutIsASentenceForAPerson(t *testing.T) {
	w := newLayoutWorldWith(t, productionLikeSession)
	account := w.suffix + "-org"
	w.createOrganization(t, account, "Busy client")
	template := w.suffix + "-template"

	// Somebody else is saving this template: its gate is held.
	holder, err := memql.AcquireWriteGate(context.Background(), w.db.DB, "campaign-template:"+template)
	if err != nil {
		t.Fatal(err)
	}
	defer holder()

	started := time.Now()
	_, err = w.workers[1].handleSaveTemplate(w.owner, map[string]any{
		"templateId": template, "accountId": account, "name": "Busy email", "content": readyConfirmation(t), "expectedRevision": "", "action": "save",
	}, 0)
	if err == nil || err.Error() != "Someone else is saving this template. Try again in a moment." {
		t.Fatalf("error = %v", err)
	}
	if waited := time.Since(started); waited < time.Second || waited > 10*time.Second {
		t.Errorf("waited %v: the pool should cancel the wait at its statement limit (1.5 s here)", waited)
	}
}

// The three reads of a template, as a client asks them, against real rows. A list
// and the lookup of a file's template are answers to questions that need a name, a
// status and a revision, not the document and the HTML (which can weigh megabytes
// each, and a query answer is one message): only templateById carries a template
// whole.
func TestTemplateListsAreLight(t *testing.T) {
	w := newLayoutWorld(t)
	account := w.suffix + "-org"
	w.createOrganization(t, account, "Light client")
	source := "art-" + w.suffix

	doc := readyConfirmation(t)
	// A heavy template: some 600 KiB of text, which is in the document, the text
	// part and the HTML.
	setBlock(t, doc, "note", func(b map[string]any) { b["text"] = strings.Repeat("A long paragraph, said slowly. ", 20000) })
	nodes, err := w.workers[0].handleSaveTemplate(w.owner, map[string]any{
		"accountId": account, "name": "Heavy email", "content": doc, "sourceArtifactId": source, "expectedRevision": "", "action": "save",
	}, 0)
	if err != nil {
		t.Fatal(err)
	}
	saved := decodeSaved(t, nodes)
	fresh := memql.ContextWithFreshRead(w.owner)

	asKeys := func(row map[string]any) string {
		keys := make([]string, 0, len(row))
		for key := range row {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		return strings.Join(keys, ",")
	}
	size := func(row map[string]any) int {
		encoded, err := json.Marshal(row)
		if err != nil {
			t.Fatal(err)
		}
		return len(encoded)
	}

	// templates: what a list shows.
	var listed map[string]any
	for _, row := range memql.MaterializeRows(w.run(t, fresh, "query templates()")) {
		if bare(str(row, "id")) == saved.TemplateID {
			listed = row
		}
	}
	if listed == nil {
		t.Fatal("templates does not list the template")
	}
	const listKeys = "accountId,createdAt,id,layoutId,name,ownerUserId,preheader,sourceArtifactId,status,subject"
	if got := asKeys(listed); got != listKeys {
		t.Errorf("a templates row has %s, want %s", got, listKeys)
	}
	if n := size(listed); n > 4<<10 {
		t.Errorf("a templates row weighs %d bytes", n)
	}
	if listed["layoutId"] != "builtin:confirmation" || str(listed, "name") != "Heavy email" || str(listed, "status") != "draft" || bare(str(listed, "sourceArtifactId")) != source {
		t.Errorf("the list row does not say what a list shows: %v", listed)
	}

	// templateForSource: which template, and at which revision.
	ref, found, err := w.workers[1].store.TemplateForSource(fresh, account, source)
	if err != nil || !found {
		t.Fatalf("templateForSource: %v %v", found, err)
	}
	if got := asKeys(ref); got != "createdAt,id" {
		t.Errorf("a templateForSource row has %s, want createdAt,id", got)
	}
	if bare(str(ref, "id")) != saved.TemplateID || templateTime(ref["createdAt"]).Format(time.RFC3339Nano) != saved.Revision {
		t.Errorf("the row is %v, not template %s at %s", ref, saved.TemplateID, saved.Revision)
	}

	// templateById: the template whole.
	whole, err := w.workers[1].store.rows(fresh, call("query", "templateById", arg{"templateId", saved.TemplateID}))
	if err != nil || len(whole) != 1 {
		t.Fatalf("templateById: %v %v", whole, err)
	}
	for _, field := range []string{"document", "htmlBody", "textBody", "subject", "layoutId", "preheader", "sourceArtifactId", "name", "status", "accountId", "ownerUserId"} {
		if _, ok := whole[0][field]; !ok {
			t.Errorf("templateById lacks %s", field)
		}
	}
	if n := size(whole[0]); n < 1<<20 {
		t.Errorf("the template weighs %d bytes: the fixture is not heavy enough to show the difference", n)
	}
}
