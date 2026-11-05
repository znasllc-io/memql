package campaigns

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/memql"
)

// compose_tools_db_test.go -- a composition's submit against the real engine:
// the email and the finished composition land in one version through the real
// updateTemplate, and a template saved meanwhile keeps its content while the
// email goes to a copy through the real createTemplate.

func TestComposeSubmitThroughTheRealTemplateWrites(t *testing.T) {
	w := newLayoutWorld(t)
	account := w.suffix + "-org"
	w.createOrganization(t, account, "Composed client")
	fresh := memql.ContextWithFreshRead(w.owner)

	read := func(id string) map[string]any {
		t.Helper()
		rows, err := w.workers[1].store.rows(fresh, call("query", "templateById", arg{"templateId", id}))
		if err != nil || len(rows) != 1 {
			t.Fatalf("templateById(%s): %v %v", id, rows, err)
		}
		return rows[0]
	}
	// start creates a template whose composition is writing, from the revision
	// it was created at, as campaignComposeEmail does.
	start := func(name string) string {
		t.Helper()
		c := composition{GoalID: "goal-" + w.suffix, State: compositionWriting, StartedAt: time.Now().UTC().Truncate(time.Millisecond),
			Request: compositionRequest{LayoutID: "builtin:article", Name: name, Description: "Spring news"}}
		id, created, _, err := w.workers[0].createTemplateFromLayout(w.owner, account, name, "builtin:article", &c)
		if err != nil {
			t.Fatal(err)
		}
		c.BaseRevision = created
		if _, err := w.workers[0].writeComposition(w.owner, account, id, c); err != nil {
			t.Fatal(err)
		}
		return id
	}
	submit := func(id string) map[string]any {
		t.Helper()
		nodes, err := w.workers[1].handleComposeSubmit(w.owner, map[string]any{
			"templateId": id, "goalId": "goal-" + w.suffix, "composition": map[string]any{
				"subject": "Spring is here", "preheader": "Three things to plant.",
				"content": []any{map[string]any{"id": "intro", "text": "The days are longer."}}, "drop": []any{"highlight"},
			},
		}, 0)
		if err != nil {
			t.Fatalf("submit: %v", err)
		}
		var receipt map[string]any
		if err := json.Unmarshal(nodes[0].Payload, &receipt); err != nil {
			t.Fatal(err)
		}
		return receipt
	}

	// Nobody changed it: the template holds the email, as a draft, and the
	// composition is done, in the version the receipt names.
	id := start("Spring")
	receipt := submit(id)
	row := read(id)
	if receipt["templateId"] != id || receipt["revision"] != templateTime(row["createdAt"]).UTC().Format(time.RFC3339Nano) {
		t.Errorf("receipt %v, row revision %v", receipt, row["createdAt"])
	}
	if str(row, "subject") != "Spring is here" || str(row, "status") != "draft" || findBlockIn(row["document"], "highlight") != nil {
		t.Errorf("row: subject %q status %q", str(row, "subject"), str(row, "status"))
	}
	if intro := findBlockIn(row["document"], "intro"); intro == nil || intro["text"] != "The days are longer." {
		t.Errorf("intro = %v", intro)
	}
	if done, ok := compositionFromRow(row); !ok || done.State != compositionDone {
		t.Errorf("composition = %+v", done)
	}

	// Somebody saved it while MemQL wrote: theirs stays, and MemQL's is a copy.
	id = start("Autumn")
	theirs := readyConfirmation(t)
	if _, err := w.workers[0].handleSaveTemplate(w.owner, map[string]any{
		"templateId": id, "accountId": account, "name": "Autumn", "content": theirs,
		"expectedRevision": templateTime(read(id)["createdAt"]).UTC().Format(time.RFC3339Nano), "action": "save",
	}, 0); err != nil {
		t.Fatal(err)
	}
	receipt = submit(id)
	copyID, _ := receipt["templateId"].(string)
	if copyID == "" || copyID == id {
		t.Fatalf("receipt = %v", receipt)
	}
	copied := read(copyID)
	if str(copied, "name") != "Autumn (MemQL's version)" || str(copied, "status") != "draft" || str(copied, "subject") != "Spring is here" {
		t.Errorf("copy: name %q status %q subject %q", str(copied, "name"), str(copied, "status"), str(copied, "subject"))
	}
	original := read(id)
	if str(original, "layoutId") != "builtin:confirmation" || str(original, "subject") == "Spring is here" {
		t.Errorf("the original lost the save made meanwhile: layout %q subject %q", str(original, "layoutId"), str(original, "subject"))
	}
	failed, _ := compositionFromRow(original)
	if failed.State != compositionFailed || failed.Error != "The template changed while MemQL was writing; its version is saved as Autumn (MemQL's version)." {
		t.Errorf("original's composition = %+v", failed)
	}
}
