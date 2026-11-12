package library

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/uptrace/bun/driver/pgdriver"
	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/database/dbtest"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	langparser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/memql"
)

// remove_artifact_db_test.go -- the Library's one removal that removes.
//
// Archiving takes an item out of the Library's default list and destroys
// nothing: the row, its body and every earlier version stay, and a restore
// puts it back. libraryRemoveArtifact is for the engine's own use, where an
// obligation says a document must be GONE: a customer's erasure removes the
// exports her data requests filed. Against a real engine and a real Postgres,
// because "gone" is about the table: every version of every row, and what the
// Library's own reads answer afterwards.

// removalFixture is one owner's Library with a raw handle on the table.
type removalFixture struct {
	*revisionDB
	db *sql.DB
}

func newRemovalFixture(t *testing.T) *removalFixture {
	t.Helper()
	f := newRevisionDB(t)
	if f == nil {
		return nil
	}
	db := sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(dbtest.DSN())))
	t.Cleanup(func() { _ = db.Close() })
	t.Cleanup(func() {
		_, _ = db.ExecContext(context.Background(),
			`DELETE FROM "MemoryNodes" WHERE concept LIKE 'v1:library:%' AND regexp_replace(payload->>'ownerUserId', '^v1:identity:user:', '') = $1`, f.owner)
	})
	f.first.SetMaintenanceSession(sessionOf(db))
	return &removalFixture{revisionDB: f, db: db}
}

// exec runs one statement as the owner, with the engine's own origin where a
// construct is server-only.
func (f *removalFixture) exec(statement string) *memql.ExecuteResult {
	f.t.Helper()
	res, err := f.engine.Execute(auth.ContextWithInternalOrigin(f.ctx), statement)
	if err != nil {
		f.t.Fatalf("%s: %v", statement, err)
	}
	return res
}

// files files one generated output with everything the Library keeps for it:
// its index row, two more versions of its content, a snapshot in its version
// history and a comment on it. It answers the output's id and the index
// row's.
func (f *removalFixture) files(name, body string) (output, artifact string) {
	f.t.Helper()
	output = f.owner + "-" + name
	f.exec(fmt.Sprintf(`mutation createGeneratedOutput(outputId: %s, title: %s, summary: "an export for a customer", body: %s, format: "text", mimeType: "application/json", source: "derived")`, langparser.QuoteString(output), langparser.QuoteString(name+".json"), langparser.QuoteString(body)))
	ref := "v1:library:generatedOutput:" + output
	// The index row, as indexGeneratedOutputOnCreate writes it.
	f.exec(fmt.Sprintf(`mutation createArtifact(sourceConceptRef: %s, ownerUserId: %s, lens: "artifact", kind: "generated_output", source: "derived", title: %s, summary: "an export for a customer", format: "text", mimeType: "application/json", live: false, partitionId: "", agentId: "", producedByPlanId: "", producedByWorkerId: "", producedByWorkerName: "")`, langparser.QuoteString(ref), langparser.QuoteString(f.owner), langparser.QuoteString(name+".json")))
	rows := f.rows(`SELECT id FROM "MemoryNodes" WHERE concept = 'v1:library:artifact' AND payload->>'sourceConceptRef' = $1 LIMIT 1`, ref)
	if len(rows) != 1 {
		f.t.Fatalf("the fixture's output %s has no index row", name)
	}
	artifact = rows[0]
	// Two more versions of the output's own row, and of its index row.
	for _, table := range []string{"v1:library:generatedOutput:" + output, artifact} {
		for n := 1; n <= 2; n++ {
			if _, err := f.db.ExecContext(context.Background(),
				`INSERT INTO "MemoryNodes" (id, concept, type, "createdBy", "createdAt", payload, schema, metadata, provenance)
				 SELECT id, concept, type, "createdBy", "createdAt" + ($2 || ' seconds')::interval, payload || jsonb_build_object('summary', 'edited ' || $2), schema, metadata, provenance
				   FROM "MemoryNodes" WHERE id = $1 ORDER BY "createdAt" LIMIT 1`, table, fmt.Sprint(n)); err != nil {
				f.t.Fatalf("write another version of %s: %v", table, err)
			}
		}
	}
	// What hangs from it: two snapshots of its history and a comment, owned
	// as the engine stamped the output itself.
	owners := f.rows(`SELECT payload->>'ownerUserId' FROM "MemoryNodes" WHERE id = $1 LIMIT 1`, ref)
	if len(owners) != 1 {
		f.t.Fatalf("the fixture's output %s has no row", name)
	}
	owner := owners[0]
	for id, row := range map[string]struct{ concept, payload string }{
		"v1:library:documentVersion:" + output + "-v0": {"v1:library:documentVersion", fmt.Sprintf(`{"ownerUserId": %q, "documentId": %q, "versionNumber": 0, "body": %q}`, owner, output, body)},
		"v1:library:documentVersion:" + output + "-v1": {"v1:library:documentVersion", fmt.Sprintf(`{"ownerUserId": %q, "documentId": %q, "versionNumber": 1, "body": %q}`, owner, ref, body)},
		"v1:library:documentComment:" + output + "-c1": {"v1:library:documentComment", fmt.Sprintf(`{"ownerUserId": %q, "artifactId": %q, "requestId": "r1", "purpose": "feedback", "removed": false, "body": "a note about %s"}`, owner, memql.BareShortId(artifact), name)},
	} {
		if _, err := f.db.ExecContext(context.Background(),
			`INSERT INTO "MemoryNodes" (id, concept, type, "createdBy", "createdAt", payload, schema, metadata, provenance)
			 VALUES ($1, $2, 'object', $3, now(), $4::jsonb, '{}'::jsonb, '{}'::jsonb, '{"kind":"system","name":"test"}'::jsonb)`, id, row.concept, f.owner, row.payload); err != nil {
			f.t.Fatalf("write %s: %v", id, err)
		}
	}
	return output, artifact
}

func (f *removalFixture) rows(query string, args ...any) []string {
	f.t.Helper()
	rows, err := f.db.QueryContext(context.Background(), query, args...)
	if err != nil {
		f.t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			f.t.Fatal(err)
		}
		out = append(out, v)
	}
	return out
}

// held is every row version the table holds that mentions a word, as
// "concept id".
func (f *removalFixture) held(word string) []string {
	f.t.Helper()
	return f.rows(`SELECT concept || ' ' || id FROM "MemoryNodes" WHERE concept LIKE 'v1:library:%' AND (payload::text LIKE '%' || $1 || '%' OR id LIKE '%' || $1 || '%') ORDER BY concept, id, "createdAt"`, word)
}

// reads is how many rows each of the Library's reads of an artifact answers
// its owner.
func (f *removalFixture) reads(output, artifact string) map[string]int {
	f.t.Helper()
	out := map[string]int{}
	for name, statement := range map[string]string{
		"generatedOutputById":               fmt.Sprintf(`query generatedOutputById(outputId: %s)`, langparser.QuoteString(output)),
		"libraryArtifactById":               fmt.Sprintf(`query libraryArtifactById(artifactId: %s)`, langparser.QuoteString(memql.BareShortId(artifact))),
		"libraryArtifactBySourceConceptRef": fmt.Sprintf(`query libraryArtifactBySourceConceptRef(sourceConceptRef: %s)`, langparser.QuoteString("v1:library:generatedOutput:"+output)),
		"documentVersions":                  fmt.Sprintf(`query documentVersions(documentId: %s)`, langparser.QuoteString(output)),
		"documentCommentsForArtifact":       fmt.Sprintf(`query documentCommentsForArtifact(artifactId: %s)`, langparser.QuoteString(memql.BareShortId(artifact))),
	} {
		// Some of these reads are server-only: they are asked as the owner,
		// by the engine.
		res, err := f.engine.Execute(auth.ContextWithInternalOrigin(memql.ContextWithFreshRead(f.ctx)), statement)
		if err != nil {
			f.t.Fatalf("%s: %v", statement, err)
		}
		out[name] = len(memql.MaterializeRows(res))
	}
	return out
}

func removalAnswer(t *testing.T, nodes []memorynodes.MemoryNode) map[string]any {
	t.Helper()
	if len(nodes) != 1 {
		t.Fatalf("the removal answered %d node(s)", len(nodes))
	}
	answer := map[string]any{}
	if err := json.Unmarshal(nodes[0].Payload, &answer); err != nil {
		t.Fatal(err)
	}
	return answer
}

// A removed artifact is gone: from the table, in every version of every row
// the Library kept for it, and from every read the Library offers. Another
// artifact of the same owner is untouched. And the removal is safe to make
// twice: the second finds nothing and says so.
func TestARemovedArtifactIsGoneFromTheTableAndFromEveryRead(t *testing.T) {
	f := newRemovalFixture(t)
	if f == nil {
		return
	}
	output, artifact := f.files("hers", `{"customer":"a person's whole export"}`)
	other, otherArtifact := f.files("theirs", `{"customer":"another person's export"}`)
	if before := f.held(output); len(before) != 9 {
		t.Fatalf("the fixture's artifact is %d row version(s), want three of the output, three of its index row, two snapshots and a comment: %v", len(before), before)
	}
	untouched := f.held(other)
	if reads := f.reads(output, artifact); reads["generatedOutputById"] != 1 || reads["libraryArtifactById"] != 1 || reads["libraryArtifactBySourceConceptRef"] != 1 {
		t.Fatalf("before the removal the Library's reads answer %v", reads)
	}

	internal := auth.ContextWithInternalOrigin(context.Background())
	nodes, err := f.first.handleRemoveArtifact(internal, map[string]any{"sourceConceptRef": "v1:library:generatedOutput:" + output}, 0)
	if err != nil {
		t.Fatalf("the removal: %v", err)
	}
	if answer := removalAnswer(t, nodes); fmt.Sprint(answer["removed"]) != "9" || fmt.Sprint(answer["remaining"]) != "0" {
		t.Errorf("the removal answers %v, want the nine row versions removed and none remaining", answer)
	}
	if left := f.held(output); len(left) != 0 {
		t.Errorf("after the removal the table still holds %v", left)
	}
	for read, n := range f.reads(output, artifact) {
		if n != 0 {
			t.Errorf("after the removal %s still answers %d row(s)", read, n)
		}
	}
	if after := f.held(other); strings.Join(after, "\n") != strings.Join(untouched, "\n") {
		t.Errorf("another artifact of the same owner was touched:\n  before: %v\n  after:  %v", untouched, after)
	}
	if reads := f.reads(other, otherArtifact); reads["generatedOutputById"] != 1 || reads["libraryArtifactById"] != 1 || reads["libraryArtifactBySourceConceptRef"] != 1 || reads["documentVersions"] == 0 {
		t.Errorf("another artifact of the same owner no longer reads whole: %v", reads)
	}

	// Twice.
	nodes, err = f.first.handleRemoveArtifact(internal, map[string]any{"sourceConceptRef": "v1:library:generatedOutput:" + output}, 0)
	if err != nil {
		t.Fatalf("the removal made again: %v", err)
	}
	if answer := removalAnswer(t, nodes); fmt.Sprint(answer["removed"]) != "0" || fmt.Sprint(answer["remaining"]) != "0" {
		t.Errorf("the removal made again answers %v, want nothing removed and nothing remaining", answer)
	}
}

// The removal is the engine's own. A builtin carries no wire bar of its own,
// so without the handler's refusal any signed-in user could remove any
// artifact by naming it; the owner of the artifact is refused like anybody
// else, before a row is read.
func TestTheRemovalRefusesACallThatIsNotTheEnginesOwn(t *testing.T) {
	f := newRemovalFixture(t)
	if f == nil {
		return
	}
	output, _ := f.files("kept", `{"customer":"a person's whole export"}`)
	before := f.held(output)
	// No session at all: a refusal that came after a read would fail as a read.
	f.first.SetMaintenanceSession(nil)
	for who, ctx := range map[string]context.Context{
		"its owner":                   f.ctx,
		"its owner, at client origin": auth.ContextWithClientOrigin(f.ctx),
		"a cluster owner":             revisionActor("somebody-else", auth.RoleOwner),
		"nobody":                      context.Background(),
	} {
		_, err := f.first.handleRemoveArtifact(ctx, map[string]any{"sourceConceptRef": "v1:library:generatedOutput:" + output}, 0)
		if err == nil || !strings.Contains(err.Error(), "refuses") {
			t.Errorf("a removal asked by %s answered %v, want it refused", who, err)
		}
	}
	if after := f.held(output); strings.Join(after, "\n") != strings.Join(before, "\n") {
		t.Errorf("a refused removal changed the table:\n  before: %v\n  after:  %v", before, after)
	}
	// And through the engine, as a client reaches a builtin.
	if _, err := f.engine.Execute(f.ctx, fmt.Sprintf(`builtin libraryRemoveArtifact(sourceConceptRef: %s)`, langparser.QuoteString("v1:library:generatedOutput:"+output))); err == nil || !strings.Contains(err.Error(), "refuses") {
		t.Errorf("the builtin called by the artifact's owner answered %v, want it refused", err)
	}
}

// What the removal cannot remove whole it does not touch. A file's bytes are
// in blob storage, and a document, a note or a live source is another
// domain's row: removing the Library's rows for one would leave its content
// where it is and say it was gone.
func TestTheRemovalRefusesAnArtifactWhoseContentIsElsewhere(t *testing.T) {
	f := newRemovalFixture(t)
	if f == nil {
		return
	}
	ref := "v1:library:file:" + f.owner + "-a-file"
	f.exec(fmt.Sprintf(`mutation createArtifact(sourceConceptRef: %s, ownerUserId: %s, lens: "artifact", kind: "file", source: "uploaded", title: "a file", summary: "", format: "pdf", mimeType: "application/pdf", live: false, partitionId: "", agentId: "", producedByPlanId: "", producedByWorkerId: "", producedByWorkerName: "")`, langparser.QuoteString(ref), langparser.QuoteString(f.owner)))
	before := f.held(f.owner + "-a-file")
	if len(before) == 0 {
		t.Fatal("the fixture's file has no index row")
	}
	_, err := f.first.handleRemoveArtifact(auth.ContextWithInternalOrigin(context.Background()), map[string]any{"sourceConceptRef": ref}, 0)
	if err == nil || !strings.Contains(err.Error(), "nothing was removed") {
		t.Errorf("the removal of a file answered %v, want it refused with nothing removed", err)
	}
	if after := f.held(f.owner + "-a-file"); strings.Join(after, "\n") != strings.Join(before, "\n") {
		t.Errorf("a refused removal changed the table: %v", after)
	}
}

// sessionOf is the maintenance session of a test: one connection of the
// test's own handle, held for the removal and returned after it.
func sessionOf(db *sql.DB) func(context.Context) (*sql.Conn, func(), error) {
	return func(ctx context.Context) (*sql.Conn, func(), error) {
		conn, err := db.Conn(ctx)
		if err != nil {
			return nil, nil, err
		}
		return conn, func() { _ = conn.Close() }, nil
	}
}

// The removal takes what the index row indexes, and nothing else names an
// artifact to it. Named by the index row's id it used to remove the index row
// of an output indexed by its bare id, leave the output where it was, and
// answer that nothing remained.
func TestTheRemovalTakesTheSourceReferenceAndFindsABareOne(t *testing.T) {
	f := newRemovalFixture(t)
	if f == nil {
		return
	}
	output, artifact := f.files("bare", `{"customer":"a person's whole export"}`)
	// The index row names the output by its bare id, as an older writer did.
	if _, err := f.db.ExecContext(context.Background(),
		`UPDATE "MemoryNodes" SET payload = jsonb_set(payload, '{sourceConceptRef}', to_jsonb($2::text)) WHERE id = $1`, artifact, output); err != nil {
		t.Fatal(err)
	}
	before := f.held(output)
	internal := auth.ContextWithInternalOrigin(context.Background())
	if _, err := f.first.handleRemoveArtifact(internal, map[string]any{"artifactId": memql.BareShortId(artifact)}, 0); err == nil || !strings.Contains(err.Error(), "sourceConceptRef") {
		t.Errorf("a removal named by the index row's id alone answered %v, want it refused: it takes what the row indexes", err)
	}
	if after := f.held(output); strings.Join(after, "\n") != strings.Join(before, "\n") {
		t.Fatalf("a refused removal changed the table: %v", after)
	}
	nodes, err := f.first.handleRemoveArtifact(internal, map[string]any{"sourceConceptRef": "v1:library:generatedOutput:" + output}, 0)
	if err != nil {
		t.Fatalf("the removal: %v", err)
	}
	if answer := removalAnswer(t, nodes); fmt.Sprint(answer["removed"]) != "9" || fmt.Sprint(answer["remaining"]) != "0" {
		t.Errorf("the removal answers %v, want the nine row versions and none remaining", answer)
	}
	if left := f.held(output); len(left) != 0 {
		t.Errorf("after the removal the table still holds %v", left)
	}
}

// Another person's index row that names the export is not the export's. A
// signed-in person who learned an export's id can file an index row of their
// own, of another kind, that names it by its bare id; the removal, seeing a
// kind it does not remove, refused: an erasure blocked for ever by a
// stranger. Only the index rows of the output's own owner count; a foreign
// row is neither removed nor a reason to refuse.
//
// What a person CANNOT do is write under the id of the owner's own index row
// (the id is made from the full reference): row authorization refuses it, and
// refuses a version of the output too. The test holds both: what a person can
// file does not stop the removal and stays; and should a version under the
// owner's index row id be another's all the same, which only an engine-origin
// write could leave, it stays and the owner's versions go.
func TestAnotherPersonsIndexRowDoesNotStopTheRemoval(t *testing.T) {
	const note = `, lens: "artifact", kind: "note", source: "user_created", title: "mine", summary: "", format: "text", mimeType: "text/plain", live: false, partitionId: "", agentId: "", producedByPlanId: "", producedByWorkerId: "", producedByWorkerName: "")`
	for name, asThePerson := range map[string]bool{
		"a row a person filed, naming the export by its bare id":                    true,
		"a version under the owner's index row id that only the engine could leave": false,
	} {
		t.Run(name, func(t *testing.T) {
			f := newRemovalFixture(t)
			if f == nil {
				return
			}
			output, _ := f.files("named-by-a-stranger", `{"customer":"a person's whole export"}`)
			ref := "v1:library:generatedOutput:" + output
			stranger := f.owner + "-stranger"
			t.Cleanup(func() {
				_, _ = f.db.ExecContext(context.Background(), `DELETE FROM "MemoryNodes" WHERE concept LIKE 'v1:library:%' AND payload->>'ownerUserId' LIKE '%' || $1`, stranger)
			})
			person := revisionActor(stranger, auth.RoleWriter)
			file := func(ctx context.Context, named string) error {
				_, err := f.engine.Execute(ctx, fmt.Sprintf(`mutation createArtifact(sourceConceptRef: %s, ownerUserId: %s`+note, langparser.QuoteString(named), langparser.QuoteString(stranger)))
				return err
			}
			// Under the owner's index row id, a person is refused.
			if err := file(person, ref); err == nil || !strings.Contains(err.Error(), "row-authz") {
				t.Fatalf("a person filing under the id of another's index row answered %v, want row authorization to refuse it", err)
			}
			named := output
			if asThePerson {
				if err := file(person, output); err != nil {
					t.Fatalf("a person's own index row naming the export by its bare id: %v", err)
				}
			} else {
				named = ref
				if err := file(auth.ContextWithInternalOrigin(person), ref); err != nil {
					t.Fatalf("the engine-origin write of another person's version: %v", err)
				}
			}
			foreign := f.rows(`SELECT id FROM "MemoryNodes" WHERE concept = 'v1:library:artifact' AND payload->>'sourceConceptRef' = $1 AND payload->>'ownerUserId' LIKE '%' || $2`, named, stranger)
			if len(foreign) != 1 {
				t.Fatalf("the fixture left %d foreign index row version(s) naming the export, want the one", len(foreign))
			}

			nodes, err := f.first.handleRemoveArtifact(auth.ContextWithInternalOrigin(context.Background()), map[string]any{"sourceConceptRef": ref}, 0)
			if err != nil {
				t.Fatalf("the removal was stopped by another person's index row: %v", err)
			}
			if answer := removalAnswer(t, nodes); fmt.Sprint(answer["removed"]) != "9" || fmt.Sprint(answer["remaining"]) != "0" {
				t.Errorf("the removal answers %v, want the owner's nine row versions and none remaining", answer)
			}
			if left := f.rows(`SELECT id FROM "MemoryNodes" WHERE concept = 'v1:library:generatedOutput' AND id = $1`, ref); len(left) != 0 {
				t.Errorf("the output is still in the table")
			}
			// The foreign row is as it was, and nothing of the owner's is left
			// under its id.
			kept := f.rows(`SELECT payload->>'ownerUserId' FROM "MemoryNodes" WHERE id = $1`, foreign[0])
			if len(kept) != 1 || !strings.HasSuffix(kept[0], stranger) {
				t.Errorf("under the foreign row's id the table holds the versions of %v, want the stranger's one and none of the owner's", kept)
			}
		})
	}
}

// The removal runs on the engine's maintenance session, as the erasure's own
// statements do, and on nothing else: two of its deletes read every comment
// and every history snapshot that names the artifact, and the request pool
// ends a statement at eight seconds. With no session it refuses.
func TestTheRemovalRunsOnTheMaintenanceSession(t *testing.T) {
	f := newRemovalFixture(t)
	if f == nil {
		return
	}
	output, _ := f.files("on-a-session", `{"customer":"a person's whole export"}`)
	internal := auth.ContextWithInternalOrigin(context.Background())
	opened, closed := 0, 0
	f.first.SetMaintenanceSession(func(ctx context.Context) (*sql.Conn, func(), error) {
		conn, done, err := sessionOf(f.db)(ctx)
		opened++
		return conn, func() { closed++; done() }, err
	})
	if _, err := f.first.handleRemoveArtifact(internal, map[string]any{"sourceConceptRef": "v1:library:generatedOutput:" + output}, 0); err != nil {
		t.Fatalf("the removal: %v", err)
	}
	if opened != 1 || closed != 1 || len(f.held(output)) != 0 {
		t.Errorf("the removal opened %d session(s), closed %d and left %v; want one session, closed, and nothing left", opened, closed, f.held(output))
	}
	f.first.SetMaintenanceSession(nil)
	if _, err := f.first.handleRemoveArtifact(internal, map[string]any{"sourceConceptRef": "v1:library:generatedOutput:" + output}, 0); err == nil || !strings.Contains(err.Error(), "maintenance session") {
		t.Errorf("a removal with no session answered %v, want it refused for want of the maintenance session", err)
	}
}
