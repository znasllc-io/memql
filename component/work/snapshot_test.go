package work

import (
	"errors"
	"strings"
	"testing"
)

func TestTheNewestEffectPerPathWins(t *testing.T) {
	snap, err := SnapshotOf([]FileEffect{
		{Order: 3, Path: "out/report.md", FileId: "f3"},
		{Order: 1, Path: "./out/report.md", FileId: "f1"},
		{Order: 2, Path: "src/main.go", FileId: "f2"},
	}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Files) != 2 || snap.Files[0] != (SnapshotFile{Path: "out/report.md", FileId: "f3"}) || snap.Files[1] != (SnapshotFile{Path: "src/main.go", FileId: "f2"}) {
		t.Errorf("Files = %+v", snap.Files)
	}
}

// "A branch from a session step whose snapshot has a contentOmitted file
// refuses naming the file" (#5415 acceptance).
func TestASnapshotWithAnOmittedFileIsRefusedNamingIt(t *testing.T) {
	_, err := SnapshotOf([]FileEffect{
		{Order: 1, Path: "src/main.go", FileId: "f1"},
		{Order: 2, Path: "data/huge.bin", Omitted: "above the per-file cap"},
	}, 0)
	var oe *SnapshotOmittedError
	if !errors.As(err, &oe) || oe.Path != "data/huge.bin" {
		t.Fatalf("err = %v, want a SnapshotOmittedError naming data/huge.bin", err)
	}
	if !strings.Contains(err.Error(), "data/huge.bin") || !strings.Contains(err.Error(), "above the per-file cap") {
		t.Errorf("the refusal must name the file and why: %q", err.Error())
	}
}

func TestAnOmittedFileLaterRewrittenIsFine(t *testing.T) {
	snap, err := SnapshotOf([]FileEffect{
		{Order: 1, Path: "data/out.csv", Omitted: "library write failed"},
		{Order: 2, Path: "data/out.csv", FileId: "f2"},
	}, 0)
	if err != nil {
		t.Fatalf("a branch needs the file's LAST state, which was recorded: %v", err)
	}
	if len(snap.Files) != 1 || snap.Files[0].FileId != "f2" {
		t.Errorf("Files = %+v", snap.Files)
	}
}

func TestUnrecordedCommandsAreCountedNotRefused(t *testing.T) {
	snap, err := SnapshotOf(nil, 3)
	if err != nil {
		t.Fatalf("a command nobody recorded has no file to name, so it cannot refuse: %v", err)
	}
	if snap.UnrecordedCommands != 3 || len(snap.Files) != 0 {
		t.Errorf("snap = %+v", snap)
	}
}

func TestTheSnapshotRoundTripsThroughItsStoredForm(t *testing.T) {
	snap := Snapshot{Files: []SnapshotFile{{Path: "a.txt", FileId: "f1"}}, UnrecordedCommands: 2}
	stored := map[string]any{
		"files":              []any{map[string]any{"path": "a.txt", "fileId": "f1"}},
		"unrecordedCommands": float64(2),
	}
	got := ParseSnapshot(stored)
	if got == nil || len(got.Files) != 1 || got.Files[0] != snap.Files[0] || got.UnrecordedCommands != 2 {
		t.Errorf("ParseSnapshot = %+v", got)
	}
	if ParseSnapshot(nil) != nil {
		t.Error("an absent snapshot is nil")
	}
	if obj := snap.Object(); obj["unrecordedCommands"] != 2 {
		t.Errorf("Object = %v", obj)
	}
}
