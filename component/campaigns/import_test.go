package campaigns

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
)

// import_test.go -- the CSV import (memql#4822, design D9).
//
// Two of these tests are about REFUSALS rather than behaviour, and they are
// the ones worth keeping: an import that silently truncates and an import
// that reads a file the caller cannot see are both invisible at every surface
// afterwards. A truncated audience looks complete; a file read under the
// engine's identity looks like a successful import.

// stubBlobs is an in-memory object store keyed by container-relative path.
type stubBlobs struct {
	objects map[string][]byte
	err     error
}

func (s stubBlobs) Download(_ context.Context, _ string, key string) ([]byte, error) {
	if s.err != nil {
		return nil, s.err
	}
	body, ok := s.objects[key]
	if !ok {
		return nil, fmt.Errorf("no object %q", key)
	}
	return body, nil
}

const testArtifact = "art-1"

func importWorker(t *testing.T, engine *fakeEngine, csv string) *Worker {
	t.Helper()
	engine.libraryArtifacts = map[string]map[string]any{
		testArtifact: {
			"id":               "v1:library:artifact:" + testArtifact,
			"kind":             "file",
			"sourceConceptRef": "v1:library:file:file-1",
			"title":            "list.csv",
		},
	}
	engine.libraryFiles = map[string]map[string]any{
		"file-1": {
			"id":       "v1:library:file:file-1",
			"name":     "list.csv",
			"mimeType": "text/csv",
			"size":     len(csv),
			"blobUrl":  "library/u-1/file-1/list.csv",
		},
	}
	w := newTestWorker(t, engine, &recordingSender{})
	w.newBlobReader = func(context.Context) (blobReader, string, error) {
		return stubBlobs{objects: map[string][]byte{"library/u-1/file-1/list.csv": []byte(csv)}}, "memql", nil
	}
	return w
}

func importCtx() context.Context {
	return auth.ContextWithUserActor(context.Background(), testOwner)
}

func runImport(t *testing.T, w *Worker, ctx context.Context) map[string]any {
	t.Helper()
	nodes, err := w.handleImportRecipients(ctx, map[string]any{
		"audienceId": testAudience, "artifactId": testArtifact,
	}, 0)
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	return decodeResult(t, nodes)
}

func TestImportAddsRowsAndCarriesEveryOtherColumn(t *testing.T) {
	engine := &fakeEngine{}
	w := importWorker(t, engine, strings.Join([]string{
		"Email,Name,Company Name,plan,2026 spend",
		"Ada@Example.test,Ada Lovelace,Acme,pro,1200",
		"bob@example.test,Bob,Beta Ltd,free,",
	}, "\n"))

	got := runImport(t, w, importCtx())

	if got["added"] != float64(2) || got["duplicates"] != float64(0) || got["invalid"] != float64(0) {
		t.Fatalf("counts = %+v, want added 2 / duplicates 0 / invalid 0", got)
	}
	// The address is stored NORMALIZED, which is what makes the suppression
	// digest and the dedup agree with the send path.
	if !wroteContaining(engine, "mutation addRecipient", `email: "ada@example.test"`) {
		t.Errorf("the address was not normalized on the way in.\ncalls:\n%s",
			strings.Join(callsWithPrefix(engine, "mutation addRecipient"), "\n"))
	}
	if !wroteContaining(engine, "mutation addRecipient", `displayName: "Ada Lovelace"`) {
		t.Error("the `Name` column was not recognized as a display name")
	}
	// EVERY other column, verbatim, under a QUOTED key -- a spreadsheet
	// header is not an identifier.
	if !wroteContaining(engine, "mutation addRecipient", `"Company Name": "Acme"`) ||
		!wroteContaining(engine, "mutation addRecipient", `"2026 spend": "1200"`) {
		t.Errorf("the extra columns did not land in `fields`.\ncalls:\n%s",
			strings.Join(callsWithPrefix(engine, "mutation addRecipient"), "\n"))
	}
	// source "import" -- the enum value that had no writer until now.
	if !wroteContaining(engine, "mutation addRecipient", `source: "import"`) {
		t.Error("imported recipients are not marked source=import")
	}
	// A consent GRANT per added recipient. Without it every imported address
	// has no consent history, which reads as "we never had permission".
	grants := callsWithPrefix(engine, "mutation recordConsentGrant")
	if len(grants) != 2 {
		t.Errorf("got %d consent grants for 2 added recipients", len(grants))
	}
	for _, g := range grants {
		if !strings.Contains(g, `source: "import"`) {
			t.Errorf("a consent grant does not name its source: %s", g)
		}
	}
}

func TestImportDedupsAgainstTheAudienceAndWithinTheFile(t *testing.T) {
	engine := &fakeEngine{roster: []map[string]any{
		recipientRow("r-existing", "Held@Example.test", "subscribed"),
	}}
	w := importWorker(t, engine, strings.Join([]string{
		"email,name",
		"held@example.test,Already Here", // duplicate of the audience
		"new@example.test,First",         // added
		"NEW@example.test,Second",        // duplicate within the file
	}, "\n"))

	got := runImport(t, w, importCtx())

	if got["added"] != float64(1) || got["duplicates"] != float64(2) {
		t.Fatalf("counts = %+v, want added 1 / duplicates 2", got)
	}
	// FIRST occurrence wins: a CSV's later rows are appended corrections of
	// unknown provenance, so taking the last would let a stale export at the
	// end of a file silently overwrite a fresher entry.
	if !wroteContaining(engine, "mutation addRecipient", `displayName: "First"`) {
		t.Errorf("the SECOND occurrence won the within-file dedup.\ncalls:\n%s",
			strings.Join(callsWithPrefix(engine, "mutation addRecipient"), "\n"))
	}
}

func TestImportReportsInvalidLinesWithTheirNumbers(t *testing.T) {
	engine := &fakeEngine{}
	w := importWorker(t, engine, strings.Join([]string{
		"email,name",
		"good@example.test,Good",
		"not-an-address,Bad",
		"",                     // a trailing blank line is NOT an error
		"also bad@example,Bad", // a space and no dotted domain
		"user@localhost,Bad",   // cannot receive mail from outside this machine
	}, "\n"))

	got := runImport(t, w, importCtx())

	if got["added"] != float64(1) {
		t.Fatalf("added = %v, want 1", got["added"])
	}
	if got["invalid"] != float64(3) {
		t.Fatalf("invalid = %v, want 3 (a blank line is not an invalid row -- every spreadsheet "+
			"export has them, and counting them would put a permanent failure on a clean file)", got["invalid"])
	}
	lines, _ := got["invalidLines"].([]any)
	if len(lines) != 3 {
		t.Fatalf("got %d sample lines, want 3", len(lines))
	}
	first, _ := lines[0].(map[string]any)
	if first["line"] != float64(3) {
		t.Errorf("the first invalid row is reported at line %v, want 3. A count with no line number "+
			"sends somebody hunting through a spreadsheet", first["line"])
	}
	if v, _ := first["value"].(string); strings.Contains(v, "not-an-address") {
		t.Errorf("the rejected value was echoed in full: %q. An import report is read in a log", v)
	}
}

// TestImportRefusesTheWholeFileOverTheCeiling is the truncation refusal.
func TestImportRefusesTheWholeFileOverTheCeiling(t *testing.T) {
	engine := &fakeEngine{}
	rows := []string{"email"}
	for i := 0; i < 5; i++ {
		rows = append(rows, fmt.Sprintf("person%d@example.test", i))
	}
	w := importWorker(t, engine, strings.Join(rows, "\n"))
	w.cfg.MaxAudience = 3

	_, err := w.handleImportRecipients(importCtx(), map[string]any{
		"audienceId": testAudience, "artifactId": testArtifact,
	}, 0)
	if err == nil {
		t.Fatal("an import over the ceiling was accepted")
	}
	if !strings.Contains(err.Error(), "refused WHOLE") {
		t.Errorf("the refusal does not say it is whole-file: %v", err)
	}
	if n := len(callsWithPrefix(engine, "mutation addRecipient")); n != 0 {
		t.Errorf("%d rows were written before the ceiling was checked. A partially imported list is one "+
			"nobody knows is partial -- the audience looks complete and the missing tail is invisible", n)
	}
}

// TestTheCeilingIsMeasuredWithACountNotAPage: the existing membership comes
// from the server-side count, never from the length of a roster read.
// Measuring a page and calling it a total is how 5000 came to be a ceiling.
func TestTheCeilingIsMeasuredWithACountNotAPage(t *testing.T) {
	engine := &fakeEngine{}
	w := importWorker(t, engine, "email\nnew@example.test")
	runImport(t, w, importCtx())

	if len(callsWithPrefix(engine, "query audienceRosterSize")) == 0 {
		t.Error("the import did not ask for the audience's server-side count")
	}
}

func TestImportRefusesAnArtifactTheCallerCannotRead(t *testing.T) {
	engine := &fakeEngine{}
	w := importWorker(t, engine, "email\na@example.test")
	// The artifact is simply absent from what this caller can read, which is
	// what an owner-gated query returns for somebody else's upload.
	engine.libraryArtifacts = nil

	_, err := w.handleImportRecipients(importCtx(), map[string]any{
		"audienceId": testAudience, "artifactId": testArtifact,
	}, 0)
	if err == nil {
		t.Fatal("an artifact this caller cannot read was imported. The artifact id is not a capability: " +
			"reading under the engine's own identity would turn one id into a read primitive for every " +
			"upload in the cluster")
	}
	if !strings.Contains(err.Error(), "visible to this caller") {
		t.Errorf("the refusal does not name the reason: %v", err)
	}
}

func TestImportRefusesANonFileArtifact(t *testing.T) {
	engine := &fakeEngine{}
	w := importWorker(t, engine, "email\na@example.test")
	engine.libraryArtifacts[testArtifact]["kind"] = "note"

	if _, err := w.handleImportRecipients(importCtx(), map[string]any{
		"audienceId": testAudience, "artifactId": testArtifact,
	}, 0); err == nil {
		t.Fatal("a note artifact was imported as a CSV")
	}
}

// TestImportRefusesAFileWithNoEmailColumn: the column mapping IS the header,
// and guessing which column holds addresses is how an import mails the wrong
// list.
func TestImportRefusesAFileWithNoEmailColumn(t *testing.T) {
	engine := &fakeEngine{}
	w := importWorker(t, engine, "name,company\nAda,Acme")

	_, err := w.handleImportRecipients(importCtx(), map[string]any{
		"audienceId": testAudience, "artifactId": testArtifact,
	}, 0)
	if err == nil {
		t.Fatal("a headerless-in-effect file was imported")
	}
	if !strings.Contains(err.Error(), "`email` column") {
		t.Errorf("the refusal does not name the missing column: %v", err)
	}
}

func TestImportRefusesHasHeaderFalse(t *testing.T) {
	engine := &fakeEngine{}
	w := importWorker(t, engine, "a@example.test")

	if _, err := w.handleImportRecipients(importCtx(), map[string]any{
		"audienceId": testAudience, "artifactId": testArtifact, "hasHeader": false,
	}, 0); err == nil {
		t.Fatal("hasHeader=false was accepted")
	}
}

func TestImportStripsExcelsByteOrderMark(t *testing.T) {
	engine := &fakeEngine{}
	// Excel writes a BOM at the start of a UTF-8 CSV. Left on the first
	// header cell it makes `email` match nothing and the import refuses a
	// file whose header is visibly correct.
	w := importWorker(t, engine, utf8BOM+"email,name\na@example.test,Ada")

	got := runImport(t, w, importCtx())
	if got["added"] != float64(1) {
		t.Fatalf("a BOM-prefixed header was not recognized: %+v", got)
	}
}

func TestImportSurfacesAStorageFailure(t *testing.T) {
	engine := &fakeEngine{}
	w := importWorker(t, engine, "email\na@example.test")
	w.newBlobReader = func(context.Context) (blobReader, string, error) {
		return stubBlobs{err: errors.New("container unreachable")}, "memql", nil
	}

	if _, err := w.handleImportRecipients(importCtx(), map[string]any{
		"audienceId": testAudience, "artifactId": testArtifact,
	}, 0); err == nil {
		t.Fatal("an unreadable blob produced no error")
	}
}

func TestPlausibleAddressShapeRules(t *testing.T) {
	valid := []string{"a@b.co", "first.last+tag@sub.example.test", "x_y@a-b.example.com"}
	invalid := []string{"", "a@b", "a@localhost", "a@@b.com", "a b@c.com", "a@b..com", "a@.com", "a@b."}
	for _, v := range valid {
		if !plausibleAddress(NormalizeEmail(v)) {
			t.Errorf("%q was refused; a stricter check refuses addresses that deliver", v)
		}
	}
	for _, v := range invalid {
		if plausibleAddress(NormalizeEmail(v)) {
			t.Errorf("%q was accepted", v)
		}
	}
}

// decodeResult unwraps a builtin's synthetic reply node.
//
// Local to this file rather than a package helper: the reply shape is
// resultNode's, and a helper on the shared fixture would be one more thing
// every other test file inherits without needing it.
func decodeResult(t *testing.T, nodes []memorynodes.MemoryNode) map[string]any {
	t.Helper()
	if len(nodes) != 1 {
		t.Fatalf("expected one result node, got %d", len(nodes))
	}
	var out map[string]any
	if err := json.Unmarshal(nodes[0].Payload, &out); err != nil {
		t.Fatalf("decoding the result payload: %v", err)
	}
	return out
}
