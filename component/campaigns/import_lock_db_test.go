package campaigns

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"
)

// import_lock_db_test.go -- an import takes its audience's lock one batch at a
// time, against a real engine and the real gate pool. While a batch is written
// the audience is the import's, and anyone else adding a person is told so;
// between batches it is free. Under each batch's lock every row is looked up
// again before it is written, so an address somebody added meanwhile is counted
// as a duplicate and not added twice.

// uploadedCSV is a real engine with one Library file in it: the import's two
// Library reads answer with a CSV the owner uploaded, and every other statement
// goes to the engine. during, when set, runs once, right after the import writes
// its first person.
type uploadedCSV struct {
	Engine
	owner  string
	size   int
	during func()
	once   sync.Once
}

func (e *uploadedCSV) Execute(ctx context.Context, statement string) (any, error) {
	switch {
	case strings.HasPrefix(statement, "query libraryArtifactById"):
		return rowsEnvelope([]map[string]any{{"id": "v1:library:artifact:" + testArtifact, "kind": "file", "sourceConceptRef": "v1:library:file:file-1", "title": "list.csv"}}), nil
	case strings.HasPrefix(statement, "query libraryFileById"):
		return rowsEnvelope([]map[string]any{{"id": "v1:library:file:file-1", "name": "list.csv", "mimeType": "text/csv", "size": e.size, "ownerUserId": "v1:identity:user:" + e.owner, "blobUrl": "library/" + e.owner + "/file-1/list.csv"}}), nil
	}
	result, err := e.Engine.Execute(ctx, statement)
	if strings.HasPrefix(statement, "mutation addRecipient") && e.during != nil {
		e.once.Do(e.during)
	}
	return result, err
}

func TestAnImportHoldsItsAudienceOneBatchAtATime(t *testing.T) {
	n := newDigestFixture(t).newEmptyNewsletter(t, "import-lock")
	wait, size := newsletterGateWait, importBatchRows
	newsletterGateWait, importBatchRows = 300*time.Millisecond, 2
	defer func() { newsletterGateWait, importBatchRows = wait, size }()
	address := func(name string) string { return name + "@" + n.suffix + ".example.test" }
	csv := "email\n" + strings.Join([]string{address("one"), address("two"), address("three"), address("four"), address("five")}, "\n") + "\n"

	// The operator works on another node, through the builtin they press.
	operator := newGatedWorker(t, n.engine, n.db, &recordingSender{})
	add := func(name string) error {
		_, err := operator.handleAddRecipient(n.owner, map[string]any{"recipientId": n.suffix + "-" + name, "audienceId": n.audience, "email": address(name)}, 0)
		return err
	}
	engine := &uploadedCSV{Engine: n.worker.store.engine, owner: n.user, size: len(csv)}
	var during error
	// While a batch is being written the audience is the import's: an add waits
	// for the lock, then is told the audience is busy, in an operator's words.
	engine.during = func() { during = add("during") }
	n.worker.store.engine = engine
	n.worker.newBlobReader = func(context.Context) (blobReader, string, error) {
		return stubBlobs{objects: map[string][]byte{"library/" + n.user + "/file-1/list.csv": []byte(csv)}}, "memql", nil
	}
	// Between two batches the audience is free: before the second batch takes the
	// lock, the operator adds the fourth address of the file.
	gate := n.worker.newsletterGate
	batches := 0
	var between error
	n.worker.newsletterGate = func(ctx context.Context, keys ...string) (gateHold, error) {
		batches++
		if batches == 2 {
			between = add("four")
		}
		return gate(ctx, keys...)
	}

	nodes, err := n.worker.handleImportRecipients(n.owner, map[string]any{"audienceId": n.audience, "artifactId": testArtifact}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if during == nil || during.Error() != audienceBusy.Error() {
		t.Errorf("an add while a batch was written: %v, want %q", during, audienceBusy.Error())
	}
	if between != nil {
		t.Errorf("an add between two batches: %v", between)
	}
	got := decodeResult(t, nodes)
	if got["added"] != float64(4) || got["duplicates"] != float64(1) || batches != 3 {
		t.Errorf("the import added %v and counted %v duplicates in %d batches, want 4, 1 and 3", got["added"], got["duplicates"], batches)
	}
	// Every address is in the audience once: four from the file, the fourth by hand.
	held := map[string]int{}
	for _, person := range n.roster(t) {
		held[NormalizeEmail(person.Email)]++
	}
	for _, name := range []string{"one", "two", "three", "four", "five"} {
		if held[address(name)] != 1 {
			t.Fatalf("the audience holds %s %d times: %v", address(name), held[address(name)], held)
		}
	}
	if len(held) != 5 {
		t.Fatalf("the audience holds %v, want the five addresses of the file", held)
	}
}
