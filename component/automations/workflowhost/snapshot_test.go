package workflowhost

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/automations"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/core/common"
)

func sourceFixture(sources map[string]string) SourceLoader {
	return func(kind, name string) (string, error) {
		s, ok := sources[kind+":"+name]
		if !ok {
			return "", fmt.Errorf("missing %s %s", kind, name)
		}
		return s, nil
	}
}

func TestDurableSnapshotSurvivesReplicaAndInstalledSourceChange(t *testing.T) {
	sources := map[string]string{
		"automation:companySpine": "@template\nautomation companySpine { return automation companyChild() }",
		"automation:companyChild": "@template\nautomation companyChild { value := logic companyPolicy()\nreturn builtin probe(value: value) }",
		"logic:companyPolicy":     "logic companyPolicy { return \"frozen\" }",
	}
	var gotRun common.RunContext
	ops := map[string]Operation{"probe": func(ctx context.Context, args map[string]any) (any, error) {
		gotRun, _ = common.RunFromContext(ctx)
		return args["value"], nil
	}}
	snapshot, err := Capture("companySpine", "test/1", sourceFixture(sources), ops)
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Constructs) != 3 {
		t.Fatalf("missing transitive source: %+v", snapshot)
	}
	// A second replica has only the stored JSON and unrelated installed code.
	restored, err := SnapshotFromMap(snapshot.Map())
	if err != nil {
		t.Fatal(err)
	}
	sources["logic:companyPolicy"] = "logic companyPolicy { return \"new\" }"
	newSnapshot, err := Capture("companySpine", "test/1", sourceFixture(sources), ops)
	if err != nil || newSnapshot.Version == snapshot.Version {
		t.Fatalf("transitive version did not change: %v", err)
	}
	run := common.RunContext{RunId: "run-on-other-replica", OwnerUserId: "alice", GoalId: "goal"}
	opts := Options{Operations: ops, Load: func(string) (*automations.Automation, error) {
		t.Fatal("consulted installed automation")
		return nil, nil
	}, LoadLogic: func(string) (*memql.Function, error) { t.Fatal("consulted installed logic"); return nil, nil }}
	out, err := RunSnapshot(common.ContextWithRun(context.Background(), run), restored, "test/1", nil, opts)
	if err != nil || out != "frozen" || !reflect.DeepEqual(run, gotRun) {
		t.Fatalf("out=%v err=%v run=%+v", out, err, gotRun)
	}
}

func TestSnapshotRefusesInvalidClosureBeforeEffects(t *testing.T) {
	sources := map[string]string{
		"automation:entry": "@template\nautomation entry { builtin probe()\nreturn automation child() }",
		"automation:child": "@template\nautomation child { return true }",
	}
	calls := 0
	ops := map[string]Operation{"probe": func(context.Context, map[string]any) (any, error) { calls++; return true, nil }}
	valid, err := Capture("entry", "test/1", sourceFixture(sources), ops)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name     string
		mutate   func(*Snapshot)
		contract string
	}{
		{"changed_source", func(s *Snapshot) { s.Constructs[0].Source += "\n// edit" }, "test/1"},
		{"contract", func(*Snapshot) {}, "test/2"},
		{"missing_child", func(s *Snapshot) {
			for n, d := range s.Constructs {
				if d.Name == "child" {
					s.Constructs = append(s.Constructs[:n], s.Constructs[n+1:]...)
					break
				}
			}
			s.Version = s.digest()
		}, "test/1"},
		{"unbound_child", func(s *Snapshot) {
			for n, d := range s.Constructs {
				if d.Name == "child" {
					s.Constructs[n].Source = "@template\nautomation child { builtin unrestrictedWrite() }"
				}
			}
			s.Version = s.digest()
		}, "test/1"},
		{"duplicate", func(s *Snapshot) { s.Constructs = append(s.Constructs, s.Constructs[0]); s.Version = s.digest() }, "test/1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, err := SnapshotFromMap(valid.Map())
			if err != nil {
				t.Fatal(err)
			}
			tc.mutate(s)
			_, err = RunSnapshot(context.Background(), s, tc.contract, nil, Options{Operations: ops})
			if err == nil || calls != 0 {
				t.Fatalf("err=%v effects=%d", err, calls)
			}
		})
	}
	sources["automation:child"] = "@template\nautomation child { return automation entry() }"
	if _, err := Capture("entry", "test/1", sourceFixture(sources), ops); err == nil {
		t.Fatal("recursive snapshot accepted")
	}
}

func TestSnapshotAdmissionBoundsSourceAndChecksPureLogic(t *testing.T) {
	for _, body := range []string{
		"@template\nautomation entry { return true }" + strings.Repeat(" ", maxSnapshotBytes),
		"@template\nautomation entry { return logic impure() }",
		"@template\nautomation entry { return automation missing() }",
	} {
		sources := map[string]string{"automation:entry": body, "logic:impure": "logic impure { return builtin externalEffect() }"}
		if _, err := Capture("entry", "test/1", sourceFixture(sources), nil); err == nil {
			t.Fatal("invalid closure admitted")
		}
	}
}

func TestSnapshotEntryArgumentsAreCheckedBeforeAdmission(t *testing.T) {
	s, err := Capture("entry", "test/1", sourceFixture(map[string]string{
		"automation:entry": "@template\nautomation entry { args { message string! }\nreturn args.message }",
	}), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.CheckArgs(nil); err == nil {
		t.Fatal("accepted an entry that cannot run without arguments")
	}
	if err := s.CheckArgs(map[string]any{"message": "bound"}); err != nil {
		t.Fatal(err)
	}
}
