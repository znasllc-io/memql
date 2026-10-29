package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

func failedOnlyOptions() options {
	o := rerunOptions(failedOnlyRunID)
	o.rerunMode = rerunFailedOnly
	return o
}
func terminalFailedOnlySource() rerunSource {
	s := failedOnlySource
	s.Status, s.Conclusion, s.FinishedAt = "completed", "failure", "2026-10-29T06:00:00Z"
	return s
}
func failedOnlyStep(s rerunSource) rerunStep {
	return rerunStep{ID: "fixture-step", OwnerUserID: s.OwnerUserID, RunID: s.WorkRunID, Key: "tests.os-checks", Status: "failed"}
}

func TestFailedOnlyExplicitModeAndSourceNeverBecomeWhole(t *testing.T) {
	o := failedOnlyOptions()
	name, call, write, err := selectCall(o)
	if err != nil || name != "pipelinesRerun" || !write || call != `builtin pipelinesRerun(runId: "`+failedOnlyRunID+`", failedOnly: true)` || o.execute {
		t.Fatalf("call=%q write=%v err=%v", call, write, err)
	}
	plan := rerunPlan(o, call)
	if plan["planned"] != true || plan["failedOnly"] != true || plan["livePreflightPerformed"] != false {
		t.Fatalf("plan=%v", plan)
	}
	if !strings.Contains(plan["requires"].(string), "independently capture the newest-attempt projection") || !strings.Contains(plan["carryForward"].(string), "not an OS-check-only retry") {
		t.Fatal("plan hid external prerequisites or carry limitations")
	}
	o.op = "rerun-preflight"
	if _, _, write, err := selectCall(o); err != nil || write {
		t.Fatalf("preflight is not read-only: %v %v", write, err)
	}
	for _, mode := range []string{"", rerunWhole, "failed", "true", "unknown"} {
		o := failedOnlyOptions()
		o.rerunMode = mode
		if _, _, _, err := selectCall(o); err == nil {
			t.Fatalf("wrong mode admitted for attempt 2: %q", mode)
		}
	}
	for _, id := range []string{"", "another-run", "738bdc26767a39a926a90f9c02ccc425", "976c8c76ca9196cc41cd3f44171a6a63"} {
		o := failedOnlyOptions()
		o.run = id
		if _, _, _, err := selectCall(o); err == nil {
			t.Fatalf("unreviewed failed-only source admitted: %q", id)
		}
	}
}

func TestFailedOnlyRequiresReviewedInstalledEngine(t *testing.T) {
	o := failedOnlyOptions()
	for _, commit := range []string{failedOnlyEngineCommit, failedOnlyEngineCommit[:12]} {
		if err := checkRerunEngine(o, "0.31.7", commit); err != nil {
			t.Fatal(err)
		}
	}
	for _, pair := range [][2]string{{"", ""}, {"0.31.8", failedOnlyEngineCommit}, {"0.31.7", "565af0d"}, {"0.31.7", failedOnlyEngineCommit + "-dirty"}, {"0.31.7", strings.Repeat("a", 40)}} {
		if checkRerunEngine(o, pair[0], pair[1]) == nil {
			t.Fatalf("unreviewed installed engine accepted: %v", pair)
		}
	}
}

func TestFailedOnlyRequiresTerminalSourceAndEveryImmutableBinding(t *testing.T) {
	good := terminalFailedOnlySource()
	if err := checkFailedOnlySource(good); err != nil {
		t.Fatal(err)
	}
	cancelled := good
	cancelled.Conclusion, cancelled.CancelRequested = "cancelled", true
	if err := checkFailedOnlySource(cancelled); err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []func(*rerunSource){
		func(s *rerunSource) { s.Status = "in_progress" },
		func(s *rerunSource) { s.Status = "queued" },
		func(s *rerunSource) { s.Conclusion = "success" },
		func(s *rerunSource) { s.Conclusion = "" },
		func(s *rerunSource) { s.FinishedAt = "" },
		func(s *rerunSource) { s.FinishedAt = "not a date" },
		func(s *rerunSource) { s.FinishedAt = "2026-10-29T00:00:00Z" },
	} {
		s := good
		mutate(&s)
		if checkFailedOnlySource(s) == nil {
			t.Fatal("nonterminal or invalid source admitted")
		}
	}
	lifecycle := map[string]bool{"Status": true, "Conclusion": true, "FailureCode": true, "FinishedAt": true, "CancelRequested": true}
	for i := 0; i < reflect.TypeOf(good).NumField(); i++ {
		name := reflect.TypeOf(good).Field(i).Name
		if lifecycle[name] {
			continue
		}
		s := good
		field := reflect.ValueOf(&s).Elem().Field(i)
		if field.Kind() == reflect.String {
			field.SetString(field.String() + "changed")
		} else {
			field.SetInt(field.Int() + 1)
		}
		if checkFailedOnlySource(s) == nil {
			t.Fatalf("changed immutable %s accepted", name)
		}
	}
}

type fakeFailedOnlyReader struct {
	fakeRerunReader
	step      rerunStep
	stepErr   error
	stepReads int
}

func (f *fakeFailedOnlyReader) failedStep(_ context.Context, id string) (rerunStep, error) {
	f.stepReads++
	if id != f.first.WorkRunID {
		return rerunStep{}, errors.New("wrong Work run")
	}
	return f.step, f.stepErr
}
func TestFailedOnlyPreflightReadsExactFailedReceiptThenReobservesSource(t *testing.T) {
	s := terminalFailedOnlySource()
	for _, variant := range []string{"valid", "source-running", "wrong-step-run", "wrong-step-owner", "wrong-step-key", "step-passed", "missing-step", "read-error", "source-changed", "source-lifecycle-changed"} {
		t.Run(variant, func(t *testing.T) {
			f := &fakeFailedOnlyReader{fakeRerunReader: fakeRerunReader{first: s, last: s, p: goodRerunPipeline(s)}, step: failedOnlyStep(s)}
			switch variant {
			case "source-running":
				f.first.Status = "in_progress"
			case "wrong-step-run":
				f.step.RunID = "different"
			case "wrong-step-owner":
				f.step.OwnerUserID = "different"
			case "wrong-step-key":
				f.step.Key = "different"
			case "step-passed":
				f.step.Status = "done"
			case "missing-step":
				f.step = rerunStep{}
			case "read-error":
				f.stepErr = errors.New("unavailable")
			case "source-changed":
				f.last.SHA = strings.Repeat("a", 40)
			case "source-lifecycle-changed":
				f.last.FailureCode = "changed"
			}
			_, err := preflightRerun(context.Background(), f, failedOnlyOptions())
			if (err == nil) != (variant == "valid") {
				t.Fatalf("err=%v", err)
			}
			if variant == "valid" && (f.reads != 2 || f.stepReads != 1) {
				t.Fatal("source was not reobserved")
			}
			if variant == "source-running" && f.stepReads != 0 {
				t.Fatal("nonterminal source proceeded")
			}
		})
	}
}

func TestFailedOnlyUncertainResponseCannotBeSubmittedAgain(t *testing.T) {
	o := failedOnlyOptions()
	o.execute = true
	dir := t.TempDir()
	calls := 0
	invoke := func() (any, error) { calls++; return nil, errors.New("uncertain transport") }
	if _, err := executeFailedOnlyOnce(context.Background(), dir, o, invoke); err == nil {
		t.Fatal("lost failure")
	}
	if _, err := executeFailedOnlyOnce(context.Background(), dir, o, invoke); err == nil {
		t.Fatal("replay admitted")
	}
	if calls != 1 {
		t.Fatalf("calls=%d", calls)
	}
	body, err := os.ReadFile(filepath.Join(dir, o.run+".json"))
	if err != nil {
		t.Fatal(err)
	}
	var marker map[string]any
	if json.Unmarshal(body, &marker) != nil || marker["failedOnly"] != true || marker["sourceRunId"] != o.run {
		t.Fatal("missing durable exact intent")
	}
}
func TestFailedOnlyConcurrentInvocationsHaveOneLocalIntent(t *testing.T) {
	o := failedOnlyOptions()
	o.execute = true
	dir := t.TempDir()
	var calls atomic.Int32
	var wg sync.WaitGroup
	for range 12 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = executeFailedOnlyOnce(context.Background(), dir, o, func() (any, error) { calls.Add(1); return "receipt", nil })
		}()
	}
	wg.Wait()
	if calls.Load() != 1 {
		t.Fatalf("calls=%d", calls.Load())
	}
}
func TestFailedOnlyPlanExpiredContextAndPreexistingIntentNeverInvoke(t *testing.T) {
	for _, variant := range []string{"plan", "expired", "existing-intent"} {
		t.Run(variant, func(t *testing.T) {
			o := failedOnlyOptions()
			o.execute = variant != "plan"
			dir := t.TempDir()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if variant == "expired" {
				cancel()
			}
			if variant == "existing-intent" {
				if err := os.WriteFile(filepath.Join(dir, o.run+".json"), nil, 0600); err != nil {
					t.Fatal(err)
				}
			}
			_, err := executeFailedOnlyOnce(ctx, dir, o, func() (any, error) { t.Fatal("invoked"); return nil, nil })
			if err == nil {
				t.Fatal("refusal missing")
			}
			files, err := os.ReadDir(dir)
			if err != nil {
				t.Fatal(err)
			}
			if variant != "existing-intent" && len(files) != 0 {
				t.Fatal("pre-execution refusal wrote an intent")
			}
		})
	}
}

func TestFailedOnlyFirstIntentPersistsParentAndRefusesAnyDirectorySyncFailure(t *testing.T) {
	for _, failAt := range []int{0, 1, 2} {
		t.Run(string(rune('0'+failAt)), func(t *testing.T) {
			parent := t.TempDir()
			dir := filepath.Join(parent, "rerun-intents")
			o := failedOnlyOptions()
			o.execute = true
			var synced []string
			calls := 0
			_, err := executeFailedOnlyWithSync(context.Background(), dir, o, func() (any, error) {
				calls++
				return "receipt", nil
			}, func(path string) error {
				synced = append(synced, path)
				if len(synced) == failAt {
					return errors.New("persistence unavailable")
				}
				return syncIntentDirectory(path)
			})
			if failAt == 0 {
				if err != nil || calls != 1 || !reflect.DeepEqual(synced, []string{parent, dir}) {
					t.Fatalf("calls=%d synced=%v err=%v", calls, synced, err)
				}
			} else if err == nil || calls != 0 || len(synced) != failAt {
				t.Fatalf("persistence failure did not refuse: calls=%d synced=%v err=%v", calls, synced, err)
			}
		})
	}
}
