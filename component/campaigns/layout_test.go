package campaigns

import (
	"context"
	"encoding/json"
	"fmt"
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

// layout_test.go -- organization email layouts: the campaignSaveLayout and
// emailLayouts builtins, driven against the package's fake engine.
//
// What a fake engine can prove is the handler's own rules: the order of its
// refusals, what it writes and under which origin, that it holds the layout's
// gate and reads fresh while it does. What it cannot is the part that needs a
// database -- the exact-revision check across two replicas, the raw-write
// guard and a stranger's view of someone else's layout. Those are
// layout_db_test.go.

const (
	// testLayout is the bare id the tests save under; its organization-layout
	// id, which a picker returns, is "org:l1".
	testLayout = "l1"
	layoutOrg  = "client-a"

	// Two sentences a person reads, spelled out here and not taken from the
	// handler so that a change of wording is a change of a test.
	//
	// layoutNotAvailable answers a save that names a revision of a layout the
	// caller cannot see: it was never saved, or it belongs to an organization
	// the caller is not in. The way on is another layout or a new one.
	layoutNotAvailable = "That layout isn't available. Choose another layout or save a new one."
	// badSourceFile answers a source file id that is not the id of a Library file.
	badSourceFile = "The source file isn't a Library file. Choose a file from the Library, or leave the source empty."
)

var (
	// layoutNow is the worker's clock, and layoutStored the createdAt of a row
	// already saved: a revision, as emailLayoutById hands it to a client.
	layoutNow    = time.Date(2029, 1, 1, 0, 0, 0, 0, time.UTC)
	layoutStored = time.Date(2028, 6, 1, 12, 0, 0, 123456000, time.UTC)
)

// layoutCall is one engine call with the facts about its context that a layout
// handler promises: the write carries internal origin (both layout mutations
// are @serverOnly) and the read behind the revision check is fresh.
type layoutCall struct {
	query    string
	internal bool
	fresh    bool
}

type layoutAsk struct{ account, verb, resource string }

// layoutEngine is the package's fake engine plus a record of how it was
// called, and an organization decision a test can set.
type layoutEngine struct {
	*fakeEngine
	mu      sync.Mutex
	made    []layoutCall
	asked   []layoutAsk
	capable func(account, verb, resource string) bool // nil allows everything
}

func (e *layoutEngine) Execute(ctx context.Context, q string) (any, error) {
	e.mu.Lock()
	e.made = append(e.made, layoutCall{query: q, internal: auth.OriginFromContext(ctx).IsInternal(), fresh: memql.FreshReadFromContext(ctx)})
	e.mu.Unlock()
	return e.fakeEngine.Execute(ctx, q)
}

func (e *layoutEngine) OrganizationCapable(_ context.Context, account, verb, resource string) bool {
	e.mu.Lock()
	e.asked = append(e.asked, layoutAsk{account, verb, resource})
	e.mu.Unlock()
	return e.capable == nil || e.capable(account, verb, resource)
}

// calls returns the recorded calls whose query starts with prefix.
func (e *layoutEngine) calls(prefix string) []layoutCall {
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
func (e *layoutEngine) writes() []layoutCall { return e.calls("mutation ") }

// layoutGateLog is a layoutGate that records the keys it was asked for and how
// many releases are still owed.
type layoutGateLog struct {
	mu   sync.Mutex
	keys []string
	held int
	err  error
}

func (g *layoutGateLog) acquire(_ context.Context, key string) (func(), error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.err != nil {
		return nil, g.err
	}
	g.keys = append(g.keys, key)
	g.held++
	return func() {
		g.mu.Lock()
		defer g.mu.Unlock()
		g.held--
	}, nil
}

func (g *layoutGateLog) snapshot() (keys []string, held int) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]string(nil), g.keys...), g.held
}

// layoutFixture is a worker over a fake engine that knows one active
// organization, with a recording gate and a fixed clock.
func layoutFixture(t *testing.T) (*Worker, *layoutEngine, *layoutGateLog) {
	t.Helper()
	engine := &layoutEngine{fakeEngine: &fakeEngine{
		accounts: map[string]map[string]any{
			layoutOrg: {"id": "v1:accounts:account:" + layoutOrg, "name": "Client A", "status": "active"},
		},
		emailLayouts: map[string]map[string]any{},
	}}
	w := newTestWorker(t, engine, &recordingSender{})
	gate := &layoutGateLog{}
	w.layoutGate = gate.acquire
	w.now = func() time.Time { return layoutNow }
	return w, engine, gate
}

// layoutDocument is a valid layout-mode document as a client sends it: a
// built-in layout, as the JSON object its file holds.
func layoutDocument(t *testing.T, id string) map[string]any {
	t.Helper()
	doc, ok := emaildoc.Layout(id)
	if !ok {
		t.Fatalf("no built-in layout %s", id)
	}
	raw, err := doc.JSON()
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

// saveLayoutArgs is a creation call; each mutate changes the arguments.
func saveLayoutArgs(t *testing.T, mutate ...func(map[string]any)) map[string]any {
	t.Helper()
	args := map[string]any{
		"layoutId":         testLayout,
		"accountId":        layoutOrg,
		"name":             "Acme letter",
		"description":      "A personal note with a signature band.",
		"document":         layoutDocument(t, "builtin:letter"),
		"sourceArtifactId": "art-1",
		"expectedRevision": "",
		"action":           "save",
	}
	for _, m := range mutate {
		m(args)
	}
	return args
}

// storedLayout is the row emailLayoutById answers for a layout saved from
// saveLayoutArgs, as a stored version: canonical ids, and the version's
// createdAt, which is the revision a client holds.
func storedLayout(t *testing.T, status string) map[string]any {
	t.Helper()
	return map[string]any{
		"id":               "v1:campaigns:emailLayout:" + testLayout,
		"ownerUserId":      "v1:identity:user:" + testOwner,
		"accountId":        "v1:accounts:account:" + layoutOrg,
		"name":             "Acme letter",
		"description":      "A personal note with a signature band.",
		"document":         layoutDocument(t, "builtin:letter"),
		"sourceArtifactId": "art-1",
		"status":           status,
		"createdAt":        layoutStored.Format(time.RFC3339Nano),
	}
}

func nodePayloads(t *testing.T, nodes []memorynodes.MemoryNode) []map[string]any {
	t.Helper()
	out := make([]map[string]any, 0, len(nodes))
	for _, n := range nodes {
		var row map[string]any
		if err := json.Unmarshal(n.Payload, &row); err != nil {
			t.Fatalf("decoding the payload of %s: %v", n.ID, err)
		}
		out = append(out, row)
	}
	return out
}

func TestSaveLayoutRefusesCustomDocuments(t *testing.T) {
	for _, tc := range []struct {
		name     string
		document any
		want     string
	}{
		{
			"custom HTML",
			map[string]any{"subject": "Hello", "preheader": "", "textBody": "Hello there.", "htmlBody": "<p>Hello there.</p>"},
			"a layout needs blocks; save custom HTML as a template instead",
		},
		{
			// The parser's own sentence, with the prefix a person reads it under.
			"a document the parser refuses",
			map[string]any{"subject": "Hello", "blocks": []any{}, "mystery": true},
			`layout: unknown field "mystery"`,
		},
		{"no document at all", nil, "layout: email document must be a JSON object"},
		{"a document that is not an object", "just text", "layout: email document must be a JSON object"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w, engine, gate := layoutFixture(t)
			_, err := w.handleSaveLayout(importCtx(), saveLayoutArgs(t, func(a map[string]any) { a["document"] = tc.document }), 0)
			if err == nil || err.Error() != tc.want {
				t.Fatalf("error = %v, want %q", err, tc.want)
			}
			if keys, _ := gate.snapshot(); len(keys) != 0 {
				t.Fatalf("the gate was taken for a document that could never be saved: %v", keys)
			}
			if len(engine.made) != 0 {
				t.Fatalf("a refused document reached the engine: %v", engine.made)
			}
		})
	}
}

// The refusals come in the order the brief gives, so a person who got two
// things wrong is told the same one first on every replica.
func TestSaveLayoutRefusalsComeInOrder(t *testing.T) {
	const identifyIt = "choose an organization and a layout name (up to 200 characters)"
	for _, tc := range []struct {
		name   string
		ctx    context.Context
		mutate func(map[string]any)
		want   string
	}{
		{"no layout id", importCtx(), func(a map[string]any) { a["layoutId"] = "" }, identifyIt},
		{"an id with a space", importCtx(), func(a map[string]any) { a["layoutId"] = "my layout" }, identifyIt},
		// The opaque id a picker shows is not the id this takes.
		{"the opaque picker id", importCtx(), func(a map[string]any) { a["layoutId"] = "org:l1" }, identifyIt},
		{"an id over 160 characters", importCtx(), func(a map[string]any) { a["layoutId"] = strings.Repeat("a", 161) }, identifyIt},
		{"no organization", importCtx(), func(a map[string]any) { a["accountId"] = " " }, identifyIt},
		{"nobody is signed in", context.Background(), func(map[string]any) {}, identifyIt},
		{"no name", importCtx(), func(a map[string]any) { a["name"] = "  " }, identifyIt},
		{"a name over 200 characters", importCtx(), func(a map[string]any) { a["name"] = strings.Repeat("é", 201) }, identifyIt},
		{"a name with a line break", importCtx(), func(a map[string]any) { a["name"] = "Two\nlines" }, identifyIt},
		{
			"a description over 500 characters", importCtx(),
			func(a map[string]any) { a["description"] = strings.Repeat("d", 501) },
			"a layout description is one line of up to 500 characters",
		},
		{
			"a description with a line break", importCtx(),
			func(a map[string]any) { a["description"] = "first\nsecond" },
			"a layout description is one line of up to 500 characters",
		},
		{"an unknown action", importCtx(), func(a map[string]any) { a["action"] = "publish" }, "unknown layout action"},
		// An unknown action is named before the name is checked: the name is
		// only looked at when saving.
		{"an unknown action without a name", importCtx(), func(a map[string]any) { a["action"] = "delete"; a["name"] = "" }, "unknown layout action"},
		// The name comes before the document, which comes after the action.
		{"no name and an invalid document", importCtx(), func(a map[string]any) { a["name"] = ""; a["document"] = "x" }, identifyIt},
		{"a bad source file id", importCtx(), func(a map[string]any) { a["sourceArtifactId"] = "not a file id!" }, badSourceFile},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w, engine, gate := layoutFixture(t)
			_, err := w.handleSaveLayout(tc.ctx, saveLayoutArgs(t, tc.mutate), 0)
			if err == nil || err.Error() != tc.want {
				t.Fatalf("error = %v, want %q", err, tc.want)
			}
			if keys, _ := gate.snapshot(); len(keys) != 0 || len(engine.made) != 0 {
				t.Fatalf("a refusal before the gate still took it or called the engine: gate %v, calls %v", keys, engine.made)
			}
		})
	}

	t.Run("archiving needs neither a name nor a document", func(t *testing.T) {
		w, engine, _ := layoutFixture(t)
		engine.emailLayouts[testLayout] = storedLayout(t, "active")
		_, err := w.handleSaveLayout(importCtx(), map[string]any{
			"layoutId": testLayout, "accountId": layoutOrg, "action": "archive",
			"expectedRevision": layoutStored.Format(time.RFC3339Nano),
		}, 0)
		if err != nil {
			t.Fatalf("archive with only the revision: %v", err)
		}
	})
}

// The limits are in characters, as the sentence says: two hundred letters with
// accents are two hundred characters whatever they weigh in bytes.
func TestSaveLayoutLimitsCountCharactersNotBytes(t *testing.T) {
	w, _, _ := layoutFixture(t)
	if _, err := w.handleSaveLayout(importCtx(), saveLayoutArgs(t, func(a map[string]any) {
		a["name"] = strings.Repeat("é", 200)
		a["description"] = strings.Repeat("é", 500)
	}), 0); err != nil {
		t.Fatalf("a name of 200 characters and a description of 500 were refused: %v", err)
	}
}

func TestSaveLayoutRequiresOrganizationWrite(t *testing.T) {
	t.Run("an inactive or unreadable organization", func(t *testing.T) {
		for name, accounts := range map[string]map[string]map[string]any{
			"unknown":  {},
			"archived": {layoutOrg: {"id": "v1:accounts:account:" + layoutOrg, "name": "Client A", "status": "archived"}},
		} {
			w, engine, gate := layoutFixture(t)
			engine.accounts = accounts
			_, err := w.handleSaveLayout(importCtx(), saveLayoutArgs(t), 0)
			if err == nil || err.Error() != "organization is not active or accessible" {
				t.Fatalf("%s: error = %v", name, err)
			}
			if len(engine.writes()) != 0 {
				t.Fatalf("%s: wrote for an organization the caller cannot use", name)
			}
			if _, held := gate.snapshot(); held != 0 {
				t.Fatalf("%s: the gate was not released", name)
			}
		}
	})

	t.Run("creating asks for create, editing asks for update, both on organization data", func(t *testing.T) {
		w, engine, _ := layoutFixture(t)
		engine.capable = func(string, string, string) bool { return false }
		if _, err := w.handleSaveLayout(importCtx(), saveLayoutArgs(t), 0); err == nil || err.Error() != "layout write permission is required in this organization" {
			t.Fatalf("create without permission: %v", err)
		}
		engine.emailLayouts[testLayout] = storedLayout(t, "active")
		args := saveLayoutArgs(t, func(a map[string]any) {
			a["expectedRevision"] = layoutStored.Format(time.RFC3339Nano)
			a["name"] = "Acme letter, second edition"
		})
		if _, err := w.handleSaveLayout(importCtx(), args, 0); err == nil || err.Error() != "layout write permission is required in this organization" {
			t.Fatalf("edit without permission: %v", err)
		}
		want := []layoutAsk{
			{layoutOrg, auth.VerbCreate, auth.ResourceData},
			{layoutOrg, auth.VerbUpdate, auth.ResourceData},
		}
		if len(engine.asked) != len(want) || engine.asked[0] != want[0] || engine.asked[1] != want[1] {
			t.Fatalf("asked %+v, want %+v", engine.asked, want)
		}
		if len(engine.writes()) != 0 {
			t.Fatalf("a refused save wrote: %v", engine.writes())
		}
	})

	t.Run("archiving and restoring need update", func(t *testing.T) {
		for _, action := range []string{"archive", "restore"} {
			w, engine, _ := layoutFixture(t)
			status := map[string]string{"archive": "active", "restore": "archived"}[action]
			engine.emailLayouts[testLayout] = storedLayout(t, status)
			engine.capable = func(_, verb, _ string) bool { return verb != auth.VerbUpdate }
			_, err := w.handleSaveLayout(importCtx(), map[string]any{
				"layoutId": testLayout, "accountId": layoutOrg, "action": action,
				"expectedRevision": layoutStored.Format(time.RFC3339Nano),
			}, 0)
			if err == nil || err.Error() != "layout write permission is required in this organization" {
				t.Fatalf("%s without permission: %v", action, err)
			}
		}
	})

	t.Run("a reader cannot save", func(t *testing.T) {
		// The package's ordinary fake decides from the caller's role.
		engine := &fakeEngine{
			accounts:     map[string]map[string]any{layoutOrg: {"id": "v1:accounts:account:" + layoutOrg, "status": "active"}},
			emailLayouts: map[string]map[string]any{},
		}
		w := newTestWorker(t, engine, &recordingSender{})
		w.layoutGate = (&layoutGateLog{}).acquire
		reader := auth.ContextWithToken(
			auth.ContextWithAccess(context.Background(), &auth.AccessContext{UserId: "reader-1", Role: auth.RoleReader}),
			&auth.TokenInfo{Subject: "reader-1"})
		_, err := w.handleSaveLayout(reader, saveLayoutArgs(t), 0)
		if err == nil || err.Error() != "layout write permission is required in this organization" {
			t.Fatalf("reader: %v", err)
		}
	})
}

func TestSaveLayoutCreatesThroughTheServerOnlyMutation(t *testing.T) {
	w, engine, gate := layoutFixture(t)
	// Every string position carries something a quote, a backslash or a
	// non-ASCII letter could break out of; the document carries markup.
	awkwardName := `Acme "letter" \ it's café`
	doc := layoutDocument(t, "builtin:letter")
	doc["subject"] = `He said "hello" \ and <left>`
	args := saveLayoutArgs(t, func(a map[string]any) {
		a["name"] = awkwardName
		a["description"] = `A note for "friends" \ neighbours`
		a["document"] = doc
	})
	nodes, err := w.handleSaveLayout(importCtx(), args, 0)
	if err != nil {
		t.Fatal(err)
	}

	receipt := decodeResult(t, nodes)
	if receipt["layoutId"] != testLayout || receipt["status"] != "active" || receipt["saved"] != true || receipt["revision"] != layoutNow.Format(time.RFC3339Nano) {
		t.Fatalf("receipt = %v", receipt)
	}
	if nodes[0].ID != "campaignLayoutSaved" {
		t.Fatalf("receipt kind = %q", nodes[0].ID)
	}

	writes := engine.writes()
	if len(writes) != 1 {
		t.Fatalf("expected one write, got %v", writes)
	}
	q := writes[0].query
	if !strings.HasPrefix(q, "mutation createEmailLayout(") {
		t.Fatalf("the write is %q", q)
	}
	for _, want := range []string{"document: {", "versionTime: ", `accountId: "` + layoutOrg + `"`, `layoutId: "` + testLayout + `"`, `sourceArtifactId: "art-1"`} {
		if !strings.Contains(q, want) {
			t.Errorf("the write lacks %q:\n%s", want, q)
		}
	}
	// The mutation stamps the status and the owner; the handler does not pass them.
	for _, unwanted := range []string{"status:", "ownerUserId:"} {
		if strings.Contains(q, unwanted) {
			t.Errorf("the creation passes %q, which the mutation stamps itself:\n%s", unwanted, q)
		}
	}
	// The real parser accepts it: a fake engine parses nothing, and a call the
	// lexer refuses fails on every cluster while every test here stays green.
	if _, err := langparser.ParseExpression(q); err != nil {
		t.Fatalf("the rendered call does not parse: %v\n%s", err, q)
	}
	if !writes[0].internal {
		t.Error("createEmailLayout is @serverOnly and ran without internal origin")
	}

	// The gate is held for the layout's own id, around the read and the write.
	if keys, held := gate.snapshot(); len(keys) != 1 || keys[0] != testLayout || held != 0 {
		t.Fatalf("gate keys %v, still held %d", keys, held)
	}
	reads := engine.calls("query emailLayoutById")
	if len(reads) != 1 || !reads[0].fresh {
		t.Fatalf("the revision check must read fresh: %+v", reads)
	}
}

func TestSaveLayoutExistingRowFlows(t *testing.T) {
	stored := layoutStored.Format(time.RFC3339Nano)
	staleRevision := time.Date(2028, 5, 1, 0, 0, 0, 0, time.UTC).Format(time.RFC3339Nano)
	advanced := layoutNow.Format(time.RFC3339Nano)
	revise := func(a map[string]any) {
		a["expectedRevision"] = stored
		a["name"] = "Acme letter, second edition"
	}
	// archive and restore are called with the revision and nothing else.
	only := func(action, revision string) func(map[string]any) {
		return func(a map[string]any) {
			for _, k := range []string{"name", "description", "document", "sourceArtifactId"} {
				delete(a, k)
			}
			a["action"], a["expectedRevision"] = action, revision
		}
	}
	// Each of these changes one thing the save replaces and leaves the rest as
	// stored, so a comparison that skipped any one of them would call the edit a
	// retry and lose it.
	oneChange := func(change func(map[string]any)) func(map[string]any) {
		return func(a map[string]any) { a["expectedRevision"] = stored; change(a) }
	}
	const changed = "this layout changed; compare the latest revision before saving"
	const notFound = "layout not found; save it before archiving or restoring"
	for _, tc := range []struct {
		name        string
		stored      map[string]any // nil: nothing saved under the id
		mutate      func(map[string]any)
		wantErr     string
		wantWrite   bool   // one updateEmailLayout
		wantStatus  string // of the receipt, and of the write
		wantRev     string
		contentFree bool // the write names the status and no content
	}{
		{"an edit on the current revision", storedLayout(t, "active"), revise, "", true, "active", advanced, false},
		{"an edit on a stale revision", storedLayout(t, "active"), func(a map[string]any) { revise(a); a["expectedRevision"] = staleRevision }, changed, false, "", "", false},
		{"an edit with no revision", storedLayout(t, "active"), func(a map[string]any) { revise(a); a["expectedRevision"] = "" }, changed, false, "", "", false},
		{"an edit with a revision nobody can read", storedLayout(t, "active"), func(a map[string]any) { revise(a); a["expectedRevision"] = "yesterday" }, changed, false, "", "", false},
		{"an edit of only the document", storedLayout(t, "active"), oneChange(func(a map[string]any) {
			doc := layoutDocument(t, "builtin:letter")
			doc["subject"] = "A different subject"
			a["document"] = doc
		}), "", true, "active", advanced, false},
		{"an edit of only the description", storedLayout(t, "active"), oneChange(func(a map[string]any) { a["description"] = "Another line." }), "", true, "active", advanced, false},
		{"an edit of only the source file", storedLayout(t, "active"), oneChange(func(a map[string]any) { a["sourceArtifactId"] = "art-2" }), "", true, "active", advanced, false},
		{"an edit of only the name", storedLayout(t, "active"), oneChange(func(a map[string]any) { a["name"] = "Acme letter v2" }), "", true, "active", advanced, false},
		// A retry of a save that already landed writes nothing and answers with
		// the receipt it already has, whatever revision the retry carries.
		{"the same save again", storedLayout(t, "active"), func(a map[string]any) { a["expectedRevision"] = staleRevision }, "", false, "active", stored, false},
		{"the same save with no revision", storedLayout(t, "active"), func(map[string]any) {}, "", false, "active", stored, false},
		{"an edit of an archived layout", storedLayout(t, "archived"), revise, "restore this layout before editing it", false, "", "", false},
		{"the same content on an archived layout", storedLayout(t, "archived"), func(a map[string]any) { a["expectedRevision"] = stored }, "restore this layout before editing it", false, "", "", false},
		{"archive on the current revision", storedLayout(t, "active"), only("archive", stored), "", true, "archived", advanced, true},
		{"archive on a stale revision", storedLayout(t, "active"), only("archive", staleRevision), changed, false, "", "", false},
		{"archive again", storedLayout(t, "archived"), only("archive", staleRevision), "", false, "archived", stored, false},
		{"restore on the current revision", storedLayout(t, "archived"), only("restore", stored), "", true, "active", advanced, true},
		{"restore on a stale revision", storedLayout(t, "archived"), only("restore", staleRevision), changed, false, "", "", false},
		{"restore again", storedLayout(t, "active"), only("restore", staleRevision), "", false, "active", stored, false},
		{"archive what was never saved", nil, only("archive", stored), notFound, false, "", "", false},
		{"restore what was never saved", nil, only("restore", ""), notFound, false, "", "", false},
		// A save that names a revision is an edit, and the person was not told to
		// archive or restore anything: the layout is not one they can see (it was
		// never saved, or it is another organization's), and the way on is to
		// choose another or save a new one.
		{"an edit with a revision of what was never saved", nil, func(a map[string]any) { a["expectedRevision"] = stored }, layoutNotAvailable, false, "", "", false},
		{"another organization's layout", func() map[string]any {
			row := storedLayout(t, "active")
			row["accountId"] = "v1:accounts:account:client-b"
			return row
		}(), revise, "a layout cannot move between organizations; save a copy instead", false, "", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w, engine, gate := layoutFixture(t)
			if tc.stored != nil {
				engine.emailLayouts[testLayout] = tc.stored
			}
			nodes, err := w.handleSaveLayout(importCtx(), saveLayoutArgs(t, tc.mutate), 0)
			writes := engine.writes()
			if keys, held := gate.snapshot(); held != 0 || len(keys) != 1 || keys[0] != testLayout {
				t.Fatalf("gate keys %v, still held %d", keys, held)
			}
			if tc.wantErr != "" {
				if err == nil || err.Error() != tc.wantErr {
					t.Fatalf("error = %v, want %q", err, tc.wantErr)
				}
				if len(writes) != 0 {
					t.Fatalf("a refusal wrote: %v", writes)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			receipt := decodeResult(t, nodes)
			if receipt["status"] != tc.wantStatus || receipt["revision"] != tc.wantRev || receipt["layoutId"] != testLayout || receipt["saved"] != true {
				t.Fatalf("receipt = %v, want status %s revision %s", receipt, tc.wantStatus, tc.wantRev)
			}
			if !tc.wantWrite {
				if len(writes) != 0 {
					t.Fatalf("an idempotent retry wrote: %v", writes)
				}
				return
			}
			if len(writes) != 1 || !strings.HasPrefix(writes[0].query, "mutation updateEmailLayout(") || !writes[0].internal {
				t.Fatalf("writes = %+v, want one internal updateEmailLayout", writes)
			}
			q := writes[0].query
			if _, perr := langparser.ParseExpression(q); perr != nil {
				t.Fatalf("the rendered call does not parse: %v\n%s", perr, q)
			}
			if argOf(q, "status") != tc.wantStatus || argOf(q, "layoutId") != testLayout || !strings.Contains(q, "versionTime: ") {
				t.Fatalf("the write is not the update the receipt describes:\n%s", q)
			}
			// Archive and restore change the status and nothing else: a layout is
			// not rewritten by the act of putting it away. And no write moves a
			// layout between organizations: only a creation names one.
			for _, content := range []string{"document", "name", "description", "sourceArtifactId"} {
				if hasArg(q, content) == tc.contentFree {
					t.Fatalf("argument %s: present=%v, want %v\n%s", content, hasArg(q, content), !tc.contentFree, q)
				}
			}
			if hasArg(q, "accountId") {
				t.Fatalf("an update names the organization, which a layout cannot change:\n%s", q)
			}
		})
	}
}

// hasArg reports whether a rendered call passes the named argument.
func hasArg(call, name string) bool {
	return strings.Contains(call, "("+name+": ") || strings.Contains(call, ", "+name+": ")
}

// A save replaces the layout's name, description, source file and document
// with what it is given, so leaving the description or the source file out
// clears it. The write names all four on every save.
func TestSaveLayoutReplacesWhatItIsGiven(t *testing.T) {
	w, engine, _ := layoutFixture(t)
	engine.emailLayouts[testLayout] = storedLayout(t, "active")
	_, err := w.handleSaveLayout(importCtx(), saveLayoutArgs(t, func(a map[string]any) {
		a["expectedRevision"] = layoutStored.Format(time.RFC3339Nano)
		a["name"] = "  Acme letter, trimmed  "
		delete(a, "description")
		delete(a, "sourceArtifactId")
	}), 0)
	if err != nil {
		t.Fatal(err)
	}
	writes := engine.writes()
	if len(writes) != 1 {
		t.Fatalf("writes = %+v", writes)
	}
	q := writes[0].query
	for _, want := range []string{`name: "Acme letter, trimmed"`, `description: ""`, `sourceArtifactId: ""`, "document: {"} {
		if !strings.Contains(q, want) {
			t.Errorf("the update lacks %s:\n%s", want, q)
		}
	}
	// A canonical id for the source file is stored as the bare id clients use.
	engine.emailLayouts[testLayout] = storedLayout(t, "active")
	if _, err := w.handleSaveLayout(importCtx(), saveLayoutArgs(t, func(a map[string]any) {
		a["expectedRevision"] = layoutStored.Format(time.RFC3339Nano)
		a["sourceArtifactId"] = "v1:library:artifact:art-9"
	}), 0); err != nil {
		t.Fatal(err)
	}
	if writes = engine.writes(); len(writes) != 2 || argOf(writes[1].query, "sourceArtifactId") != "art-9" {
		t.Fatalf("the source file is not stored bare: %+v", writes)
	}
}

// A lagging clock cannot write a revision behind the one it read.
func TestSaveLayoutRevisionAlwaysAdvances(t *testing.T) {
	w, engine, _ := layoutFixture(t)
	engine.emailLayouts[testLayout] = storedLayout(t, "active")
	w.now = func() time.Time { return layoutStored.Add(-time.Hour) }
	nodes, err := w.handleSaveLayout(importCtx(), saveLayoutArgs(t, func(a map[string]any) {
		a["expectedRevision"] = layoutStored.Format(time.RFC3339Nano)
		a["name"] = "Renamed"
	}), 0)
	if err != nil {
		t.Fatal(err)
	}
	revision, err := time.Parse(time.RFC3339Nano, decodeResult(t, nodes)["revision"].(string))
	if err != nil || !revision.After(layoutStored) {
		t.Fatalf("revision %v is not after the stored %v (%v)", revision, layoutStored, err)
	}
}

func TestSaveLayoutNeedsItsGate(t *testing.T) {
	t.Run("no gate wired", func(t *testing.T) {
		w, engine, _ := layoutFixture(t)
		w.layoutGate = nil
		_, err := w.handleSaveLayout(importCtx(), saveLayoutArgs(t), 0)
		if err == nil || err.Error() != "layout write coordination is unavailable" {
			t.Fatalf("error = %v", err)
		}
		if len(engine.made) != 0 {
			t.Fatalf("worked without a gate: %v", engine.made)
		}
	})
	t.Run("the gate refuses", func(t *testing.T) {
		w, engine, gate := layoutFixture(t)
		gate.err = context.DeadlineExceeded
		_, err := w.handleSaveLayout(importCtx(), saveLayoutArgs(t), 0)
		if err == nil || !strings.Contains(err.Error(), "deadline") {
			t.Fatalf("error = %v", err)
		}
		if len(engine.made) != 0 {
			t.Fatalf("worked without the gate: %v", engine.made)
		}
	})
	t.Run("released after a refusal", func(t *testing.T) {
		w, engine, gate := layoutFixture(t)
		engine.accounts = nil
		_, _ = w.handleSaveLayout(importCtx(), saveLayoutArgs(t), 0)
		if keys, held := gate.snapshot(); len(keys) != 1 || held != 0 {
			t.Fatalf("gate keys %v, still held %d", keys, held)
		}
	})
}

func TestEmailLayoutsListsBuiltinsFirst(t *testing.T) {
	w, engine, _ := layoutFixture(t)
	engine.emailLayouts["l1"] = storedLayout(t, "active")
	// Neither another organization's layout nor an archived one is listed.
	other := storedLayout(t, "active")
	other["id"], other["accountId"] = "v1:campaigns:emailLayout:x9", "v1:accounts:account:client-b"
	engine.emailLayouts["x9"] = other
	archived := storedLayout(t, "archived")
	archived["id"] = "v1:campaigns:emailLayout:l2"
	engine.emailLayouts["l2"] = archived

	nodes, err := w.handleEmailLayouts(importCtx(), map[string]any{"accountId": layoutOrg}, 0)
	if err != nil {
		t.Fatal(err)
	}
	rows := nodePayloads(t, nodes)
	var ids []string
	for _, r := range rows {
		ids = append(ids, str(r, "id"))
	}
	if want := "builtin:confirmation, builtin:letter, builtin:article, builtin:spotlight, org:l1"; strings.Join(ids, ", ") != want {
		t.Fatalf("ids = %s\nwant %s", strings.Join(ids, ", "), want)
	}
	for i, n := range nodes {
		if n.ID != ids[i] {
			t.Errorf("node %d is keyed %q, its row says %q", i, n.ID, ids[i])
		}
	}

	for _, row := range rows[:4] {
		if row["kind"] != "builtin" || row["status"] != "active" || str(row, "name") == "" || str(row, "description") == "" {
			t.Errorf("built-in row = %v", row)
		}
		doc, ok := row["document"].(map[string]any)
		if !ok || row["document"] == nil || doc["layout"] != row["id"] {
			t.Errorf("%v: the document is not the layout's own JSON object: %v", row["id"], row["document"])
		}
		if blocks, _ := doc["blocks"].([]any); len(blocks) == 0 {
			t.Errorf("%v: the document has no blocks", row["id"])
		}
		for _, absent := range []string{"layoutId", "revision", "sourceArtifactId"} {
			if _, present := row[absent]; present {
				t.Errorf("built-in row %v carries %s", row["id"], absent)
			}
		}
	}

	org := rows[4]
	if org["layoutId"] != "l1" || org["kind"] != "organization" || org["name"] != "Acme letter" || org["status"] != "active" ||
		org["sourceArtifactId"] != "art-1" || org["revision"] != layoutStored.Format(time.RFC3339Nano) || org["description"] != "A personal note with a signature band." {
		t.Fatalf("organization row = %v", org)
	}
	if doc, ok := org["document"].(map[string]any); !ok || doc["layout"] != "builtin:letter" {
		t.Fatalf("organization document = %v", org["document"])
	}

	// Without includeArchived the read asks for active layouts only.
	reads := engine.calls("query emailLayoutsForAccount")
	if len(reads) != 1 || argOf(reads[0].query, "status") != "active" || argOf(reads[0].query, "accountId") != layoutOrg {
		t.Fatalf("reads = %+v", reads)
	}
}

func TestEmailLayoutsIncludesArchivedOnRequest(t *testing.T) {
	w, engine, _ := layoutFixture(t)
	engine.emailLayouts["l1"] = storedLayout(t, "active")
	archived := storedLayout(t, "archived")
	archived["id"] = "v1:campaigns:emailLayout:l2"
	engine.emailLayouts["l2"] = archived

	nodes, err := w.handleEmailLayouts(importCtx(), map[string]any{"accountId": layoutOrg, "includeArchived": true}, 0)
	if err != nil {
		t.Fatal(err)
	}
	rows := nodePayloads(t, nodes)
	if len(rows) != 6 || rows[4]["id"] != "org:l1" || rows[5]["id"] != "org:l2" || rows[5]["status"] != "archived" {
		t.Fatalf("rows = %v", rows)
	}
	reads := engine.calls("query emailLayoutsForAccount")
	if len(reads) != 1 || strings.Contains(reads[0].query, "status:") {
		t.Fatalf("includeArchived must not narrow by status: %+v", reads)
	}
}

func TestEmailLayoutsNeedsAnOrganizationTheCallerCanRead(t *testing.T) {
	t.Run("no organization named", func(t *testing.T) {
		w, _, _ := layoutFixture(t)
		_, err := w.handleEmailLayouts(importCtx(), map[string]any{}, 0)
		if err == nil || err.Error() != "choose an organization" {
			t.Fatalf("error = %v", err)
		}
	})
	t.Run("inactive or unreadable", func(t *testing.T) {
		w, engine, _ := layoutFixture(t)
		engine.accounts = map[string]map[string]any{layoutOrg: {"id": "v1:accounts:account:" + layoutOrg, "status": "archived"}}
		_, err := w.handleEmailLayouts(importCtx(), map[string]any{"accountId": layoutOrg}, 0)
		if err == nil || err.Error() != "organization is not active or accessible" {
			t.Fatalf("error = %v", err)
		}
	})
	t.Run("no read permission", func(t *testing.T) {
		w, engine, _ := layoutFixture(t)
		engine.capable = func(string, string, string) bool { return false }
		_, err := w.handleEmailLayouts(importCtx(), map[string]any{"accountId": layoutOrg}, 0)
		if err == nil || err.Error() != "layout read permission is required in this organization" {
			t.Fatalf("error = %v", err)
		}
		if len(engine.asked) != 1 || engine.asked[0] != (layoutAsk{layoutOrg, auth.VerbRead, auth.ResourceData}) {
			t.Fatalf("asked %+v", engine.asked)
		}
		if len(engine.calls("query emailLayoutsForAccount")) != 0 {
			t.Fatal("listed layouts for a caller who may not read them")
		}
	})
}

// pagedLayoutEngine answers emailLayoutsForAccount a page at a time with the
// engine's continuation token, the way a query that declares `paginate` does.
type pagedLayoutEngine struct {
	*fakeEngine
	pages   [][]map[string]any
	endless bool // never stops handing back a token
	cursors []string
}

func (e *pagedLayoutEngine) Execute(ctx context.Context, q string) (any, error) {
	if !strings.HasPrefix(q, "query emailLayoutsForAccount") {
		return e.fakeEngine.Execute(ctx, q)
	}
	cursor := memql.CursorFromContext(ctx)
	e.cursors = append(e.cursors, cursor)
	index := 0
	if cursor != "" {
		_, _ = fmt.Sscanf(cursor, "%d", &index)
	}
	var page []map[string]any
	if index < len(e.pages) {
		page = e.pages[index]
	}
	envelope := map[string]any{"output": toAnySlice(page)}
	if index+1 < len(e.pages) || e.endless {
		envelope["cursor"] = fmt.Sprintf("%d", index+1)
	}
	return envelope, nil
}

// An organization with more layouts than one page holds still gets all of them
// in the picker: a list that stopped at the first hundred would drop layouts
// without a word.
func TestEmailLayoutsForAccountWalksEveryPage(t *testing.T) {
	row := func(id string) map[string]any {
		r := storedLayout(t, "active")
		r["id"] = "v1:campaigns:emailLayout:" + id
		return r
	}
	engine := &pagedLayoutEngine{fakeEngine: &fakeEngine{}, pages: [][]map[string]any{{row("a"), row("b")}, {row("c")}, {row("d")}}}
	rows, err := NewStore(engine).EmailLayoutsForAccount(context.Background(), layoutOrg, false)
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, r := range rows {
		ids = append(ids, bare(str(r, "id")))
	}
	if strings.Join(ids, ",") != "a,b,c,d" {
		t.Fatalf("ids = %v", ids)
	}
	// Each page continues from the token the one before it returned.
	if strings.Join(engine.cursors, "|") != "|1|2" {
		t.Fatalf("cursors = %q", engine.cursors)
	}

	endless := &pagedLayoutEngine{fakeEngine: &fakeEngine{}, pages: [][]map[string]any{{row("a")}}, endless: true}
	if _, err := NewStore(endless).EmailLayoutsForAccount(context.Background(), layoutOrg, false); err == nil {
		t.Fatal("a listing that never ended was returned as if it were whole")
	}
	if len(endless.cursors) != layoutListPages {
		t.Fatalf("read %d pages before giving up, want %d", len(endless.cursors), layoutListPages)
	}
}

func TestResultRowsAreOneNodePerRowKeyedByTheRowsID(t *testing.T) {
	rows := []map[string]any{
		{"id": "builtin:letter", "name": "Letter", "document": map[string]any{"blocks": []any{}}},
		{"id": "org:l1", "name": "Mine"},
	}
	nodes, err := resultRows("emailLayout", rows)
	if err != nil {
		t.Fatal(err)
	}
	if len(nodes) != 2 || nodes[0].ID != "builtin:letter" || nodes[1].ID != "org:l1" {
		t.Fatalf("nodes = %+v", nodes)
	}
	for i, n := range nodes {
		// A made-up concept outside the graph's grammar, like resultNode's: no
		// row-authorization tier can mistake it for stored state.
		if n.Concept != "integration:campaigns:emailLayout" || n.Type != memorynodes.NodeTypeObject {
			t.Errorf("node %d concept/type = %q/%q", i, n.Concept, n.Type)
		}
		var back map[string]any
		if err := json.Unmarshal(n.Payload, &back); err != nil {
			t.Fatal(err)
		}
		if back["id"] != rows[i]["id"] || back["name"] != rows[i]["name"] {
			t.Errorf("payload %d = %v", i, back)
		}
	}

	if got, err := resultRows("emailLayout", nil); err != nil || len(got) != 0 {
		t.Errorf("no rows = %v, %v", got, err)
	}
	// The engine drops a node with a blank id and keeps one of two with the
	// same id, so either would hide a row without saying so.
	if _, err := resultRows("emailLayout", []map[string]any{{"name": "no id"}}); err == nil {
		t.Error("a row without an id was accepted")
	}
	if _, err := resultRows("emailLayout", []map[string]any{{"id": "a"}, {"id": "a"}}); err == nil {
		t.Error("two rows with one id were accepted")
	}
}

func TestLayoutCapabilitiesAreRegistered(t *testing.T) {
	w, _, _ := layoutFixture(t)
	found := map[string]memql.IntegrationCapability{}
	for _, c := range w.Capabilities() {
		found[c.Name] = c
	}
	save, list := found["saveLayout"], found["emailLayouts"]
	if save.Handler == nil || list.Handler == nil {
		t.Fatalf("saveLayout / emailLayouts are not registered: %v", found)
	}
	// The picker is ordered -- the built-ins in catalogue order, then the
	// organization's layouts -- and a node map loses a handler's order unless
	// the capability asks for it to be kept.
	if !list.PreserveOrder {
		t.Error("emailLayouts must PreserveOrder or the built-ins stop coming first on the wire")
	}
	if save.PreserveOrder {
		t.Error("saveLayout answers one receipt; there is no order to preserve")
	}
}
