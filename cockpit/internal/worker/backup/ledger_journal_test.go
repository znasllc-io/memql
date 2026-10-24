package backup

import (
	"encoding/json"
	"os"
	"testing"
)

func TestLedgerRecoversTornTailAndDoesNotReplayOlderSnapshotRows(t *testing.T) {
	dir := t.TempDir()
	l := LoadLedger(dir, "watch-001")
	l.Put("/source/a", Record{FileID: "file-001", VersionNumber: 1})
	if err := l.Save(); err != nil {
		t.Fatal(err)
	}
	l.Put("/source/a", Record{FileID: "file-001", VersionNumber: 2})
	if err := l.Save(); err != nil {
		t.Fatal(err)
	}
	// A snapshot rename landed, but its journal truncation did not. Older records
	// cannot undo the new snapshot even if the old journal ends in a torn write.
	data, err := json.Marshal(ledgerDoc{Version: 1, Sequence: l.sequence, Files: l.files})
	if err != nil {
		t.Fatal(err)
	}
	if err = writeFileAtomic(l.path, data, 0600); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(l.path+".journal", os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString(`{"data":{"sequence":3,"path":"/source/a"`)
	f.Close()
	recovered := LoadLedger(dir, "watch-001")
	rec, _ := recovered.Get("/source/a")
	if rec.VersionNumber != 2 {
		t.Fatalf("snapshot rolled back: %+v", rec)
	}
	recovered.Put("/source/b", Record{FileID: "file-002"})
	if err = recovered.Save(); err != nil {
		t.Fatal(err)
	}
	recovered = LoadLedger(dir, "watch-001")
	if len(recovered.Paths()) != 2 {
		t.Fatal("torn tail prevented later durable receipts")
	}
	recovered.Forget("/source/a")
	if err = recovered.Save(); err != nil {
		t.Fatal(err)
	}
	if _, exists := LoadLedger(dir, "watch-001").Get("/source/a"); exists {
		t.Fatal("forgotten record resurrected")
	}
}

func TestLedgerWritesOnlyChangedPaths(t *testing.T) {
	l := LoadLedger(t.TempDir(), "watch-001")
	for n := 0; n < 1000; n++ {
		l.Put(string(rune(n+100))+"/file", Record{FileID: "file-001"})
	}
	if err := l.Save(); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(l.path + ".journal")
	if err != nil {
		t.Fatal(err)
	}
	l.Put("/one-change", Record{FileID: "file-002"})
	if err = l.Save(); err != nil {
		t.Fatal(err)
	}
	after, err := os.Stat(l.path + ".journal")
	if err != nil {
		t.Fatal(err)
	}
	if after.Size()-before.Size() > 2048 {
		t.Fatal("one changed path rewrote the whole folder")
	}
}
