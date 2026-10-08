package workflowhost

import (
	"context"
	"errors"
	"testing"

	"github.com/znasllc-io/memql/core/common"
)

func TestSnapshotActionsSurviveReplicaWithoutAmbientExecution(t *testing.T) {
	sources := map[string]string{
		"automation:entry":  "@template\nautomation entry { result := action frozenRead(path: \"original\")\nreturn result }",
		"action:frozenRead": "use capabilities.fs.{ readFile }\naction frozenRead { args { path string! } capability readFile(path: args.path) }",
	}
	var got common.RunContext
	ops := map[string]Operation{"fs.readFile": func(ctx context.Context, args map[string]any) (any, error) {
		got, _ = common.RunFromContext(ctx)
		return args["path"], nil
	}}
	snapshot, err := Capture("entry", "actions/1", sourceFixture(sources), ops)
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Constructs) != 2 {
		t.Fatal("action source not pinned")
	}
	sources["action:frozenRead"] = "use capabilities.fs.{ readFile }\naction frozenRead { args { path string! } capability readFile(path: \"changed\") }"
	restored, err := SnapshotFromMap(snapshot.Map())
	if err != nil {
		t.Fatal(err)
	}
	run := common.RunContext{RunId: "other-replica", OwnerUserId: "owner"}
	value, err := RunSnapshot(common.ContextWithRun(context.Background(), run), restored, "actions/1", nil, Options{Operations: ops})
	if err != nil || value != "original" || got.RunId != run.RunId || got.OwnerUserId != run.OwnerUserId {
		t.Fatalf("value=%v run=%+v err=%v", value, got, err)
	}
	delete(ops, "fs.readFile")
	if _, err := RunSnapshot(context.Background(), restored, "actions/1", nil, Options{Operations: ops}); err == nil {
		t.Fatal("unbound action escaped into ambient execution")
	}
}

func TestActionsPreflightAllBranchesAndPreserveRefusals(t *testing.T) {
	sources := map[string]string{
		"automation:entry":  "@template\nautomation entry { action frozenRead(path: \"ok\")\nif false { action unknownAction() } }",
		"action:frozenRead": "use capabilities.fs.{ readFile }\naction frozenRead { args { path string! } capability readFile(path: args.path) }",
	}
	calls := 0
	sentinel := errors.New("native refusal")
	ops := map[string]Operation{"fs.readFile": func(context.Context, map[string]any) (any, error) { calls++; return nil, sentinel }}
	if _, err := Capture("entry", "actions/1", sourceFixture(sources), ops); err == nil || calls != 0 {
		t.Fatalf("missing unreachable action admitted: %v calls=%d", err, calls)
	}
	sources["automation:entry"] = "@template\nautomation entry { action frozenRead(path: \"ok\") }"
	snapshot, err := Capture("entry", "actions/1", sourceFixture(sources), ops)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := RunSnapshot(context.Background(), snapshot, "actions/1", nil, Options{Operations: ops}); !errors.Is(err, sentinel) || calls != 1 {
		t.Fatalf("lost native refusal: %v calls=%d", err, calls)
	}
}
