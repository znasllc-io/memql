package campaigns

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/compose/emaildoc"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/memql"
)

// brandkit_test.go -- an organization's brand kit: campaignSaveBrandKit and
// campaignBrandPreview, driven against the package's fake engine.
//
// What a fake engine can prove is the handlers' own rules: the order of their
// refusals, what a save writes and under which origin and gate, that a kit's
// Library pictures are read as the CALLER reads them and kept as data:
// pictures, that contrast is a warning and never a refusal, and that a preview
// writes nothing. What needs a database -- the exact revision across two
// replicas, the refusal of a raw write, and what a colleague or a stranger
// reads -- is brandkit_db_test.go.

const (
	brandOrg = renderOrg

	// The sentences a person reads, spelled out here and not taken from the
	// handlers so that a change of wording is a change of a test.
	brandChanged    = "this brand changed; reload it before saving"
	brandNeedsWrite = "saving the brand needs permission to write templates in this organization"
	brandLocked     = "Someone else is saving this brand. Try again in a moment."
	brandBadLayout  = "choose one of the built-in layouts to preview the brand with"
	brandNoPreview  = "You need read access to this organization's data to preview its brand."
)

var (
	// brandNow is the worker's clock, and brandStored the createdAt of a kit
	// already saved: a revision, as brandKitForAccount hands it to a client.
	brandNow    = time.Date(2029, 1, 1, 0, 0, 0, 0, time.UTC)
	brandStored = time.Date(2028, 6, 1, 12, 0, 0, 123456000, time.UTC)
)

// brandEngine is the package's fake engine plus the organizations' stored kits,
// which createBrandKit and updateBrandKit write as the concept's mutations do;
// a record of how it was called; an organization decision a test can set; and
// the owner gate of the Library's two named reads.
type brandEngine struct {
	*fakeEngine
	mu      sync.Mutex
	made    []layoutCall
	asked   []layoutAsk
	capable func(account, verb, resource string) bool // nil allows everything
	// pictureOwner, when set, models the owner gate of libraryArtifactById and
	// libraryFileById: only that user is shown the rows.
	pictureOwner string
	// kits is each organization's stored kit as brandKitForAccount answers it,
	// keyed by the bare organization id the query is called with.
	kits map[string]map[string]any
	// failLibrary, when set, is what the Library's two named reads fail with:
	// the database not answering.
	failLibrary error
}

func (e *brandEngine) Execute(ctx context.Context, q string) (any, error) {
	e.mu.Lock()
	e.made = append(e.made, layoutCall{query: q, internal: auth.OriginFromContext(ctx).IsInternal(), fresh: memql.FreshReadFromContext(ctx)})
	e.mu.Unlock()
	switch {
	case strings.HasPrefix(q, "query brandKitForAccount"):
		e.mu.Lock()
		row, ok := e.kits[argOf(q, "accountId")]
		e.mu.Unlock()
		if ok {
			return rowsEnvelope([]map[string]any{row}), nil
		}
		return rowsEnvelope(nil), nil
	case strings.HasPrefix(q, "query libraryArtifactById"), strings.HasPrefix(q, "query libraryFileById"):
		if e.failLibrary != nil {
			return nil, e.failLibrary
		}
		if e.pictureOwner != "" {
			if ac, _ := auth.AccessFromContext(ctx); ac == nil || ac.UserId != e.pictureOwner {
				return rowsEnvelope(nil), nil
			}
		}
	case strings.HasPrefix(q, "mutation createBrandKit("), strings.HasPrefix(q, "mutation updateBrandKit("):
		e.keep(ctx, q)
		return nil, nil
	}
	return e.fakeEngine.Execute(ctx, q)
}

// keep applies a rendered createBrandKit or updateBrandKit to the stored kit
// the way the concept's mutations do: a create stamps the id, the owner and the
// organization; an update replaces the kit and the revision and keeps the rest.
func (e *brandEngine) keep(ctx context.Context, q string) {
	name, args := parseRenderedCall(q)
	e.mu.Lock()
	defer e.mu.Unlock()
	id := fmt.Sprint(args["brandKitId"])
	if name == "createBrandKit" {
		ac, _ := auth.AccessFromContext(ctx)
		account := fmt.Sprint(args["accountId"])
		e.kits[account] = map[string]any{
			"id": "v1:campaigns:brandKit:" + id, "ownerUserId": "v1:identity:user:" + ac.UserId,
			"accountId": "v1:accounts:account:" + account, "kit": args["kit"], "createdAt": args["versionTime"],
		}
		return
	}
	for account, row := range e.kits {
		if bare(str(row, "id")) == id {
			updated := maps.Clone(row)
			updated["kit"], updated["createdAt"] = args["kit"], args["versionTime"]
			e.kits[account] = updated
		}
	}
}

func (e *brandEngine) OrganizationCapable(_ context.Context, account, verb, resource string) bool {
	e.mu.Lock()
	e.asked = append(e.asked, layoutAsk{account, verb, resource})
	e.mu.Unlock()
	return e.capable == nil || e.capable(account, verb, resource)
}

// calls returns the recorded calls whose query starts with prefix.
func (e *brandEngine) calls(prefix string) []layoutCall {
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

// writes returns every mutation the handlers issued.
func (e *brandEngine) writes() []layoutCall { return e.calls("mutation ") }

// libraryReadsBy returns the actors the Library's two named reads ran under.
func (e *brandEngine) libraryReadsBy() []string {
	e.fakeEngine.mu.Lock()
	defer e.fakeEngine.mu.Unlock()
	var actors []string
	for _, c := range e.fakeEngine.calls {
		if strings.HasPrefix(c.query, "query libraryArtifactById") || strings.HasPrefix(c.query, "query libraryFileById") {
			actors = append(actors, c.actorID)
		}
	}
	return actors
}

// brandWorld is a worker over a fake engine that knows one active
// organization, with a recording gate, a fixed clock and storage that records
// how it is read.
type brandWorld struct {
	t      *testing.T
	w      *Worker
	engine *brandEngine
	gate   *layoutGateLog
	blobs  *recordingBlobs
}

func newBrandWorld(t *testing.T) *brandWorld {
	t.Helper()
	engine := &brandEngine{fakeEngine: &fakeEngine{
		accounts: map[string]map[string]any{
			brandOrg: {"id": "v1:accounts:account:" + brandOrg, "name": "Acme Co", "status": "active"},
		},
		libraryArtifacts: map[string]map[string]any{},
		libraryFiles:     map[string]map[string]any{},
	}, kits: map[string]map[string]any{}}
	w := newTestWorker(t, engine, &recordingSender{})
	gate := &layoutGateLog{}
	w.brandGate = gate.acquire
	w.now = func() time.Time { return brandNow }
	blobs := &recordingBlobs{objects: map[string][]byte{}}
	w.newBlobReader = func(context.Context) (blobReader, string, error) { return blobs, "memql", nil }
	return &brandWorld{t: t, w: w, engine: engine, gate: gate, blobs: blobs}
}

// addPicture makes a PNG artifact the Library shows its owner, the caller of
// these tests, with the bytes in storage.
func (bw *brandWorld) addPicture(artifact string, data []byte) {
	key := "library/" + testOwner + "/file-" + artifact + "/" + artifact
	bw.engine.libraryArtifacts[artifact] = map[string]any{
		"id": "v1:library:artifact:" + artifact, "kind": "file", "sourceConceptRef": "v1:library:file:file-" + artifact, "title": artifact,
	}
	bw.engine.libraryFiles["file-"+artifact] = map[string]any{
		"id": "v1:library:file:file-" + artifact, "ownerUserId": testOwner, "name": artifact, "mimeType": "image/png", "size": len(data), "blobUrl": key,
	}
	bw.blobs.objects[key] = data
}

// save runs campaignSaveBrandKit as the cluster's first test user.
func (bw *brandWorld) save(kit any, expected string) ([]memorynodes.MemoryNode, error) {
	bw.t.Helper()
	return bw.w.handleSaveBrandKit(importCtx(), map[string]any{"accountId": brandOrg, "kit": kit, "expectedRevision": expected}, 0)
}

// saved is save for a call that must succeed, and answers its receipt.
func (bw *brandWorld) saved(kit any, expected string) brandAnswer {
	bw.t.Helper()
	nodes, err := bw.save(kit, expected)
	if err != nil {
		bw.t.Fatalf("campaignSaveBrandKit: %v", err)
	}
	return decodeBrandAnswer(bw.t, nodes, "campaignBrandKitSaved")
}

// preview runs campaignBrandPreview for the organization; args adds to the call.
func (bw *brandWorld) preview(kit any, args map[string]any) (brandAnswer, error) {
	bw.t.Helper()
	call := map[string]any{"accountId": brandOrg, "kit": kit}
	maps.Copy(call, args)
	nodes, err := bw.w.handleBrandPreview(importCtx(), call, 0)
	if err != nil {
		return brandAnswer{}, err
	}
	return decodeBrandAnswer(bw.t, nodes, "campaignBrandPreview"), nil
}

// writtenKit is the kit the n-th mutation wrote.
func (bw *brandWorld) writtenKit(n int) map[string]any {
	bw.t.Helper()
	writes := bw.engine.writes()
	if n >= len(writes) {
		bw.t.Fatalf("there are %d writes, not %d", len(writes), n+1)
	}
	_, args := parseRenderedCall(writes[n].query)
	kit, ok := args["kit"].(map[string]any)
	if !ok {
		bw.t.Fatalf("write %d carries no kit object: %.300s", n, writes[n].query)
	}
	return kit
}

// brandAnswer is the answer of either builtin as a client reads it: a save's
// {revision, warnings} or a preview's {html, warnings}.
type brandAnswer struct {
	Revision string `json:"revision"`
	HTML     string `json:"html"`
	Warnings []struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"warnings"`
	raw map[string]json.RawMessage
}

func decodeBrandAnswer(t *testing.T, nodes []memorynodes.MemoryNode, kind string) brandAnswer {
	t.Helper()
	if len(nodes) != 1 || nodes[0].ID != kind {
		t.Fatalf("expected one %s node, got %+v", kind, nodes)
	}
	var out brandAnswer
	if err := json.Unmarshal(nodes[0].Payload, &out); err != nil {
		t.Fatalf("decoding the answer: %v", err)
	}
	if err := json.Unmarshal(nodes[0].Payload, &out.raw); err != nil {
		t.Fatalf("decoding the answer: %v", err)
	}
	return out
}

// codes is the warning codes of an answer, in order.
func (a brandAnswer) codes() []string {
	var out []string
	for _, w := range a.Warnings {
		out = append(out, w.Code)
	}
	return out
}

// embedded is a PNG as a data: picture.
func embedded(data []byte) string {
	return "data:image/png;base64," + base64.StdEncoding.EncodeToString(data)
}

// testBrandKit is a kit as the Brand page sends it: every part filled, the
// pictures embedded, in a theme every built-in layout draws readably.
func testBrandKit(t *testing.T) map[string]any {
	t.Helper()
	logo, photo := embedded(previewPNG(t, 4, 3)), embedded(previewPNG(t, 5, 5))
	return map[string]any{
		"theme": map[string]any{
			"page": "#eef2f6", "canvas": "#ffffff", "primary": "#12355b", "secondary": "#1d4f7c", "accent": "#c0392b",
			"tint": "#f3f6f9", "text": "#1a1a1a", "muted": "#5a5a5a", "headingFont": "Georgia", "bodyFont": "Arial",
		},
		"logo":      map[string]any{"src": logo, "alt": "Acme", "href": "https://acme.test"},
		"signature": map[string]any{"photo": photo, "name": map[string]any{"text": "Ada Lovelace"}, "title": "Founder of Acme"},
		"follow":    "Find us online",
		"social": []any{
			map[string]any{"network": "Instagram", "href": "https://instagram.com/acme", "icon": logo},
		},
		"footer": map[string]any{
			"lines": []any{"ACME CO", "1 Harbour Road"},
			"links": []any{
				map[string]any{"label": "Contact", "href": "mailto:hi@acme.test"},
				map[string]any{"label": "Unsubscribe", "href": "{{unsubscribeUrl}}"},
			},
		},
		"reply": map[string]any{"heading": "Write back", "button": map[string]any{"label": "Reply", "href": "mailto:hi@acme.test"}, "note": "We read everything."},
	}
}

// part is one object of a kit, to change it in place.
func part(kit map[string]any, key string) map[string]any { return kit[key].(map[string]any) }

// canonicalKit is the kit as a save stores it: parsed and written again, as
// the JSON object its file holds.
func canonicalKit(t *testing.T, kit map[string]any) map[string]any {
	t.Helper()
	raw, err := json.Marshal(kit)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := emaildoc.ParseBrandKit(raw)
	if err != nil {
		t.Fatalf("the fixture is not a kit: %v", err)
	}
	canonical, err := parsed.JSON()
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err := json.Unmarshal(canonical, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

// storedBrandRow is the organization's kit as brandKitForAccount answers it:
// canonical ids, the kit as it was saved and the version's createdAt, which is
// the revision a client holds.
func storedBrandRow(t *testing.T, kit map[string]any) map[string]any {
	t.Helper()
	return map[string]any{
		"id":          "v1:campaigns:brandKit:" + brandKitID(brandOrg),
		"ownerUserId": "v1:identity:user:" + testOwner,
		"accountId":   "v1:accounts:account:" + brandOrg,
		"kit":         canonicalKit(t, kit),
		"createdAt":   brandStored.Format(time.RFC3339Nano),
	}
}

func TestSaveBrandKitCreatesThenUpdatesWithRevision(t *testing.T) {
	bw := newBrandWorld(t)
	fresh := memql.ContextWithFreshRead(importCtx())
	if _, _, ok, err := bw.w.brandKitFor(fresh, brandOrg); ok || err != nil {
		t.Fatalf("an organization without a kit read as ok=%v err=%v", ok, err)
	}

	kit := testBrandKit(t)
	first := bw.saved(kit, "")
	if first.Revision != brandNow.Format(time.RFC3339Nano) {
		t.Fatalf("created at %q; want the clock, %s", first.Revision, brandNow.Format(time.RFC3339Nano))
	}
	writes := bw.engine.writes()
	if len(writes) != 1 || !strings.HasPrefix(writes[0].query, "mutation createBrandKit(") || !writes[0].internal {
		t.Fatalf("the first save wrote %+v; want one createBrandKit with internal origin", writes)
	}
	requireParses(t, writes[0].query)
	_, args := parseRenderedCall(writes[0].query)
	// One kit per organization: its id is made from the organization's.
	sum := sha256.Sum256([]byte("brand-kit:" + brandOrg))
	if args["brandKitId"] != hex.EncodeToString(sum[:]) || args["accountId"] != brandOrg || args["versionTime"] != first.Revision {
		t.Fatalf("createBrandKit named brandKitId=%v accountId=%v versionTime=%v", args["brandKitId"], args["accountId"], args["versionTime"])
	}
	if !sameJSON(t, args["kit"], canonicalKit(t, kit)) {
		t.Fatalf("the stored kit is not the kit as parsed:\n%v", args["kit"])
	}

	read, revision, ok, err := bw.w.brandKitFor(fresh, brandOrg)
	if err != nil || !ok || !revision.Equal(brandNow) || read.Follow != "Find us online" || read.Logo.Alt != "Acme" {
		t.Fatalf("brandKitFor answered ok=%v revision=%v err=%v follow=%q", ok, revision, err, read.Follow)
	}

	// An edit names the revision it read, and is stamped after it although
	// the clock has not moved. Its words carry what a rendered call must
	// quote: quotes, a backslash, an apostrophe and non-ASCII.
	const follow = `Follow "along" \ with us, it's été`
	kit["follow"] = follow
	second := bw.saved(kit, first.Revision)
	secondAt, err := time.Parse(time.RFC3339Nano, second.Revision)
	if err != nil || !secondAt.After(brandNow) {
		t.Fatalf("the edit was stamped %q, not after %s", second.Revision, first.Revision)
	}
	writes = bw.engine.writes()
	if len(writes) != 2 || !strings.HasPrefix(writes[1].query, "mutation updateBrandKit(") || !writes[1].internal {
		t.Fatalf("the edit wrote %+v; want an updateBrandKit with internal origin", writes)
	}
	requireParses(t, writes[1].query)
	_, args = parseRenderedCall(writes[1].query)
	if _, named := args["accountId"]; named {
		t.Error("the update names the organization; a kit stays where it was created")
	}
	if args["brandKitId"] != hex.EncodeToString(sum[:]) || args["versionTime"] != second.Revision {
		t.Fatalf("updateBrandKit named brandKitId=%v versionTime=%v", args["brandKitId"], args["versionTime"])
	}
	read, revision, ok, err = bw.w.brandKitFor(fresh, brandOrg)
	if err != nil || !ok || !revision.Equal(secondAt) || read.Follow != follow {
		t.Fatalf("after the edit brandKitFor answered ok=%v revision=%v err=%v follow=%q", ok, revision, err, read.Follow)
	}

	// A retry whose answer was lost: the same kit, with the revision it was
	// saved from, answers with the revision it has and writes nothing.
	again := bw.saved(kit, first.Revision)
	if again.Revision != second.Revision || len(bw.engine.writes()) != 2 {
		t.Fatalf("a retry answered %q after %d writes; want %q and no write", again.Revision, len(bw.engine.writes()), second.Revision)
	}

	// Every save held the organization's gate and gave it back, and the read
	// behind each revision check was fresh.
	keys, held := bw.gate.snapshot()
	if strings.Join(keys, ",") != strings.Join([]string{brandOrg, brandOrg, brandOrg}, ",") || held != 0 {
		t.Fatalf("gate keys %v, %d still held", keys, held)
	}
	for _, c := range bw.engine.calls("query brandKitForAccount") {
		if !c.fresh {
			t.Errorf("a read of the kit was not fresh: %s", c.query)
		}
	}
}

func TestSaveBrandKitRefusesAStaleRevision(t *testing.T) {
	bw := newBrandWorld(t)
	bw.engine.kits[brandOrg] = storedBrandRow(t, testBrandKit(t))
	changed := testBrandKit(t)
	changed["follow"] = "Somebody else's line"
	for name, expected := range map[string]string{
		"an older revision":        brandStored.Add(-time.Millisecond).Format(time.RFC3339Nano),
		"a newer revision":         brandStored.Add(time.Millisecond).Format(time.RFC3339Nano),
		"no revision, as a create": "",
		"not a revision":           "yesterday",
	} {
		if _, err := bw.save(changed, expected); err == nil || err.Error() != brandChanged {
			t.Errorf("%s: %v; want %q", name, err, brandChanged)
		}
	}
	if writes := bw.engine.writes(); len(writes) != 0 {
		t.Fatalf("a stale save wrote %+v", writes)
	}
	if _, held := bw.gate.snapshot(); held != 0 {
		t.Fatalf("a refused save kept %d gates", held)
	}

	// The revision that is stored saves.
	bw.saved(changed, brandStored.Format(time.RFC3339Nano))
	if got := bw.writtenKit(0); got["follow"] != "Somebody else's line" {
		t.Fatalf("the save with the current revision wrote %v", got["follow"])
	}

	// An organization without a kit has no revision: a save that names one read
	// a kit that is not there.
	empty := newBrandWorld(t)
	if _, err := empty.save(changed, brandStored.Format(time.RFC3339Nano)); err == nil || err.Error() != brandChanged {
		t.Fatalf("a revision of a kit that is not there: %v; want %q", err, brandChanged)
	}
	if writes := empty.engine.writes(); len(writes) != 0 {
		t.Fatalf("it wrote %+v", writes)
	}
}

func TestSaveBrandKitStoresArtifactPicturesAsData(t *testing.T) {
	bw := newBrandWorld(t)
	bw.engine.pictureOwner = testOwner
	pictures := map[string][]byte{
		"logo-art": previewPNG(t, 8, 4), "reversed-art": previewPNG(t, 8, 5), "wide-art": previewPNG(t, 30, 8),
		"photo-art": previewPNG(t, 6, 6), "name-art": previewPNG(t, 12, 3), "signed-art": previewPNG(t, 20, 6),
		"icon-art": previewPNG(t, 4, 4),
	}
	for artifact, data := range pictures {
		bw.addPicture(artifact, data)
	}
	kit := testBrandKit(t)
	kit["logo"] = map[string]any{"src": "artifact:logo-art", "onColour": "artifact:reversed-art", "wide": "artifact:wide-art", "alt": "Acme"}
	kit["signature"] = map[string]any{"photo": "artifact:photo-art", "name": map[string]any{"src": "artifact:name-art"}, "title": "Founder", "picture": "artifact:signed-art"}
	kit["social"] = []any{
		map[string]any{"network": "Instagram", "href": "https://instagram.com/acme", "icon": "artifact:icon-art"},
		// The same picture twice is read once.
		map[string]any{"network": "YouTube", "href": "https://youtube.com/@acme", "icon": "artifact:icon-art"},
	}
	bw.saved(kit, "")

	stored := bw.writtenKit(0)
	logo, signature := part(stored, "logo"), part(stored, "signature")
	social := stored["social"].([]any)
	for field, got := range map[string]any{
		"logo.src": logo["src"], "logo.onColour": logo["onColour"], "logo.wide": logo["wide"],
		"signature.photo": signature["photo"], "signature.name.src": part(signature, "name")["src"], "signature.picture": signature["picture"],
		"social[0].icon": social[0].(map[string]any)["icon"], "social[1].icon": social[1].(map[string]any)["icon"],
	} {
		want := map[string]string{
			"logo.src": "logo-art", "logo.onColour": "reversed-art", "logo.wide": "wide-art", "signature.photo": "photo-art",
			"signature.name.src": "name-art", "signature.picture": "signed-art", "social[0].icon": "icon-art", "social[1].icon": "icon-art",
		}[field]
		if got != embedded(pictures[want]) {
			t.Errorf("%s was stored as %.60v; want %s as a data: picture", field, got, want)
		}
	}
	// Read as the caller, each once, and with a limit.
	for _, actor := range bw.engine.libraryReadsBy() {
		if actor != testOwner {
			t.Fatalf("a Library picture was read as %q, not as the caller", actor)
		}
	}
	if limited, unbounded := bw.blobs.reads(); limited != len(pictures) || unbounded != 0 {
		t.Fatalf("storage was read %d times with a limit and %d without; want %d and 0", limited, unbounded, len(pictures))
	}

	// A picture the caller cannot read (somebody else's upload) is refused with
	// the field it is in, and nothing is written.
	other := newBrandWorld(t)
	other.engine.pictureOwner = "someone-else"
	other.addPicture("wide-art", pictures["wide-art"])
	kit = testBrandKit(t)
	part(kit, "logo")["wide"] = "artifact:wide-art"
	_, err := other.save(kit, "")
	if err == nil || !strings.HasPrefix(err.Error(), "logo.wide: ") || !strings.Contains(err.Error(), "could not be read") {
		t.Fatalf("somebody else's picture: %v; want a refusal that names logo.wide", err)
	}
	if writes := other.engine.writes(); len(writes) != 0 {
		t.Fatalf("it wrote %+v", writes)
	}
}

// A picture that cannot be read just now -- storage or the database did not
// answer -- says so, and that trying again is the way on. It is not a picture
// to choose again, which is what a picture that is not a PNG, JPEG or GIF of up
// to 1 MiB of the caller's own is told. Nothing is written either way, and the
// cause is never shown.
func TestSaveBrandKitTellsATransientPictureReadApart(t *testing.T) {
	const (
		justNow = "logo.wide: the picture could not be read just now. Try again."
		choose  = "logo.wide: the picture could not be read. Choose a PNG, JPEG or GIF of up to 1 MiB that you uploaded"
		cause   = "dial tcp 10.0.0.9:443: i/o timeout"
	)
	for _, tc := range []struct {
		name  string
		setup func(bw *brandWorld)
		want  string
	}{
		{"storage did not answer", func(bw *brandWorld) { bw.blobs.err = errors.New(cause) }, justNow},
		{"the database did not answer", func(bw *brandWorld) { bw.engine.failLibrary = errors.New(cause) }, justNow},
		{"somebody else's upload", func(bw *brandWorld) { bw.engine.pictureOwner = "someone-else" }, choose},
		{"not a picture an email can carry", func(bw *brandWorld) {
			bw.blobs.objects["library/"+testOwner+"/file-wide-art/wide-art"] = []byte("not a picture")
		}, choose},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bw := newBrandWorld(t)
			bw.addPicture("wide-art", previewPNG(t, 6, 2))
			tc.setup(bw)
			kit := testBrandKit(t)
			part(kit, "logo")["wide"] = "artifact:wide-art"
			_, err := bw.save(kit, "")
			if err == nil || err.Error() != tc.want {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
			if strings.Contains(err.Error(), cause) {
				t.Error("the cause reached the person")
			}
			if writes := bw.engine.writes(); len(writes) != 0 {
				t.Fatalf("it wrote %+v", writes)
			}
		})
	}
}

func TestSaveBrandKitReturnsWarningsNotRefusals(t *testing.T) {
	bw := newBrandWorld(t)
	readable := bw.saved(testBrandKit(t), "")
	if string(readable.raw["warnings"]) != "[]" {
		t.Fatalf("a readable kit answered warnings %s; want an empty list", readable.raw["warnings"])
	}

	// White on a light teal is under the WCAG minimum: the kit is saved, and
	// the answer says where.
	kit := testBrandKit(t)
	part(kit, "theme")["secondary"] = "#55b9bd"
	low := bw.saved(kit, readable.Revision)
	contrast := 0
	for _, w := range low.Warnings {
		if w.Code != "contrast" {
			t.Errorf("unexpected warning %q: %s", w.Code, w.Message)
		}
		if w.Code == "contrast" && strings.HasPrefix(w.Message, "Text in #") && strings.Contains(w.Message, "the minimum here is") {
			contrast++
		}
	}
	if contrast == 0 {
		t.Fatalf("a low-contrast kit answered %+v; want contrast warnings", low.Warnings)
	}
	if got := part(bw.writtenKit(1), "theme")["secondary"]; got != "#55b9bd" {
		t.Fatalf("the low-contrast kit was not saved as it was sent: secondary %v", got)
	}

	// Pictures past 512 KiB together are saved, and said too.
	large := testBrandKit(t)
	part(large, "logo")["src"] = embedded(noisePNG(t, 300<<10))
	part(large, "signature")["photo"] = embedded(noisePNG(t, 290<<10))
	answer := bw.saved(large, low.Revision)
	if strings.Join(answer.codes(), ",") != "pictures_large" || !strings.Contains(answer.Warnings[0].Message, "512 KiB") {
		t.Fatalf("a kit of large pictures answered %+v; want one pictures_large warning", answer.Warnings)
	}
	if writes := bw.engine.writes(); len(writes) != 3 {
		t.Fatalf("%d writes; every one of the three kits is saved", len(writes))
	}
}

func TestSaveBrandKitRefusesWithoutWritePermission(t *testing.T) {
	bw := newBrandWorld(t)
	bw.engine.pictureOwner = testOwner
	bw.addPicture("logo-art", previewPNG(t, 8, 4))
	kit := testBrandKit(t)
	part(kit, "logo")["src"] = "artifact:logo-art"

	// A reader may not save, and costs nothing: no picture is read, no gate is
	// taken, nothing is written.
	bw.engine.capable = func(_, verb, _ string) bool { return verb == auth.VerbRead }
	if _, err := bw.save(kit, ""); err == nil || err.Error() != brandNeedsWrite {
		t.Fatalf("a reader's save: %v; want %q", err, brandNeedsWrite)
	}
	if reads := bw.engine.libraryReadsBy(); len(reads) != 0 {
		t.Fatalf("a refused save read the Library %d times", len(reads))
	}
	if limited, unbounded := bw.blobs.reads(); limited+unbounded != 0 {
		t.Fatal("a refused save read storage")
	}
	if keys, _ := bw.gate.snapshot(); len(keys) != 0 || len(bw.engine.writes()) != 0 {
		t.Fatalf("a refused save took gates %v or wrote %+v", keys, bw.engine.writes())
	}
	for _, ask := range bw.engine.asked {
		if ask.account != brandOrg || ask.resource != auth.ResourceData {
			t.Errorf("asked %+v; want the organization's data", ask)
		}
	}

	// Somebody who may create but not update cannot change the kit that is
	// there: that is decided under the gate, once the kit is known to exist.
	stored := newBrandWorld(t)
	stored.engine.kits[brandOrg] = storedBrandRow(t, testBrandKit(t))
	stored.engine.capable = func(_, verb, _ string) bool { return verb != auth.VerbUpdate }
	changed := testBrandKit(t)
	changed["follow"] = "Changed"
	if _, err := stored.save(changed, brandStored.Format(time.RFC3339Nano)); err == nil || err.Error() != brandNeedsWrite {
		t.Fatalf("an update without update permission: %v; want %q", err, brandNeedsWrite)
	}
	if keys, held := stored.gate.snapshot(); len(keys) != 1 || held != 0 || len(stored.engine.writes()) != 0 {
		t.Fatalf("gates %v (%d held), writes %+v", keys, held, stored.engine.writes())
	}

	// An organization that is not active is refused before permission is asked.
	closed := newBrandWorld(t)
	closed.engine.accounts[brandOrg]["status"] = "archived"
	if _, err := closed.save(testBrandKit(t), ""); err == nil || err.Error() != "organization is not active or accessible" {
		t.Fatalf("an archived organization: %v", err)
	}
	if len(closed.engine.asked) != 0 {
		t.Fatalf("asked %+v about an organization the caller cannot use", closed.engine.asked)
	}
}

func TestSaveBrandKitRefusesFooterWithoutUnsubscribe(t *testing.T) {
	bw := newBrandWorld(t)
	bw.engine.pictureOwner = testOwner
	bw.addPicture("logo-art", previewPNG(t, 8, 4))
	kit := testBrandKit(t)
	kit["footer"] = map[string]any{"lines": []any{"ACME CO"}, "links": []any{map[string]any{"label": "Contact", "href": "mailto:hi@acme.test"}}}
	_, err := bw.save(kit, "")
	if err == nil || !strings.Contains(strings.ToLower(err.Error()), "unsubscribe") {
		t.Fatalf("a footer with no unsubscribe link: %v; want a refusal that says unsubscribe", err)
	}

	// A kit refused for what it says reads no picture first: its Library logo
	// is not fetched to be thrown away.
	part(kit, "logo")["src"] = "artifact:logo-art"
	if _, err := bw.save(kit, ""); err == nil || !strings.Contains(strings.ToLower(err.Error()), "unsubscribe") {
		t.Fatalf("with a Library logo: %v", err)
	}
	if reads := bw.engine.libraryReadsBy(); len(reads) != 0 {
		t.Fatalf("a kit that is refused read the Library %d times", len(reads))
	}
	if keys, _ := bw.gate.snapshot(); len(keys) != 0 || len(bw.engine.writes()) != 0 {
		t.Fatalf("a refused kit took gates %v or wrote %+v", keys, bw.engine.writes())
	}
}

func TestBrandPreviewRendersTheLayoutWithTheKit(t *testing.T) {
	bw := newBrandWorld(t)
	bw.engine.pictureOwner = testOwner
	logo := previewPNG(t, 9, 4)
	bw.addPicture("logo-art", logo)
	kit := testBrandKit(t)
	part(kit, "logo")["src"] = "artifact:logo-art"

	// With no layout named, the preview is Article, in the kit's look: its
	// logo (a Library picture, read as the caller), colours, reply and footer.
	answer, err := bw.preview(kit, nil)
	if err != nil {
		t.Fatalf("campaignBrandPreview: %v", err)
	}
	for _, want := range []string{embedded(logo), "#12355b", "Write back", "We read everything.", "1 Harbour Road", "Ada Lovelace"} {
		if !strings.Contains(answer.HTML, want) {
			t.Errorf("the preview lacks %.60q", want)
		}
	}
	if string(answer.raw["warnings"]) != "[]" {
		t.Errorf("a readable kit previewed with warnings %s; want an empty list", answer.raw["warnings"])
	}
	for _, actor := range bw.engine.libraryReadsBy() {
		if actor != testOwner {
			t.Fatalf("a Library picture was read as %q, not as the caller", actor)
		}
	}

	// Another layout, named.
	letter, err := bw.preview(kit, map[string]any{"layoutId": "builtin:letter"})
	if err != nil {
		t.Fatalf("previewing Letter: %v", err)
	}
	if !strings.Contains(letter.HTML, "Founder of Acme") || letter.HTML == answer.HTML {
		t.Fatal("the Letter preview is not Letter with the kit's signature")
	}

	// A kit's warnings come with its preview.
	part(kit, "theme")["secondary"] = "#55b9bd"
	low, err := bw.preview(kit, nil)
	if err != nil {
		t.Fatalf("previewing a low-contrast kit: %v", err)
	}
	if len(low.Warnings) == 0 || low.Warnings[0].Code != "contrast" {
		t.Fatalf("a low-contrast preview warned %+v", low.Warnings)
	}

	// A preview writes nothing and takes no gate.
	if keys, _ := bw.gate.snapshot(); len(keys) != 0 || len(bw.engine.writes()) != 0 {
		t.Fatalf("a preview took gates %v or wrote %+v", keys, bw.engine.writes())
	}

	// Only a built-in layout is a brand preview.
	for _, layout := range []string{"builtin:no-such-layout", "org:l1", "letter"} {
		if _, err := bw.preview(kit, map[string]any{"layoutId": layout}); err == nil || err.Error() != brandBadLayout {
			t.Errorf("layout %q: %v; want %q", layout, err, brandBadLayout)
		}
	}

	// It needs read access to the organization's data, and refuses before any
	// picture is read.
	closed := newBrandWorld(t)
	closed.engine.pictureOwner = testOwner
	closed.addPicture("logo-art", logo)
	closed.engine.capable = func(string, string, string) bool { return false }
	if _, err := closed.preview(kit, nil); err == nil || err.Error() != brandNoPreview {
		t.Fatalf("a preview without read access: %v; want %q", err, brandNoPreview)
	}
	if reads := closed.engine.libraryReadsBy(); len(reads) != 0 {
		t.Fatalf("a refused preview read the Library %d times", len(reads))
	}
}

// A save holds the organization's gate, which another replica's save of the
// same kit holds too; a gate that cannot be taken is a sentence.
func TestSaveBrandKitTakesTheOrganizationsGate(t *testing.T) {
	bw := newBrandWorld(t)
	bw.gate.err = fmt.Errorf("%w: lock wait ended", memql.ErrWriteGateBusy)
	if _, err := bw.save(testBrandKit(t), ""); err == nil || err.Error() != brandLocked {
		t.Fatalf("a busy gate: %v; want %q", err, brandLocked)
	}
	if len(bw.engine.writes()) != 0 {
		t.Fatal("a save that could not take the gate wrote")
	}
	bw.w.brandGate = nil
	if _, err := bw.save(testBrandKit(t), ""); err == nil || !strings.Contains(err.Error(), "unavailable") {
		t.Fatalf("no gate at all: %v", err)
	}
}

func TestBrandKitCapabilitiesAreRegistered(t *testing.T) {
	bw := newBrandWorld(t)
	found := map[string]memql.IntegrationCapability{}
	for _, c := range bw.w.Capabilities() {
		found[c.Name] = c
	}
	for _, name := range []string{"saveBrandKit", "brandPreview"} {
		if found[name].Handler == nil {
			t.Errorf("%s is not registered", name)
		}
	}
}
