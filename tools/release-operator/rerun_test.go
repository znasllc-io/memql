package main

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
)

func rerunOptions(id string) options { o := baseOptions(); o.op = "rerun"; o.run = id; return o }
func goodRerunPipeline(source rerunSource) rerunPipeline {
	return rerunPipeline{ID: canonicalPipeline, OwnerUserID: source.OwnerUserID, AccountID: source.AccountID,
		PackageID: "6a97392a-6801-4ab3-b779-e02460329b65", Repository: source.Repository, Status: "active",
		DefaultBranch: "main", InstallationID: "158882932", CredentialID: "b1649763-a25a-4df7-beb4-b948c08e3294"}
}

func TestRerunOnlyBuildsTwoExplicitWholeAttemptCalls(t *testing.T) {
	for id := range rerunSources {
		o := rerunOptions(id)
		name, call, write, err := selectCall(o)
		if err != nil || name != "pipelinesRerun" || !write || call != `builtin pipelinesRerun(runId: "`+id+`", failedOnly: false)` {
			t.Fatalf("name=%s call=%s write=%v err=%v", name, call, write, err)
		}
		if o.execute {
			t.Fatal("execution was selected by default")
		}
		plan := rerunPlan(o, call)
		if plan["planned"] != true || plan["livePreflightPerformed"] != false || plan["failedOnly"] != false {
			t.Fatalf("plan=%v", plan)
		}
		o.op = "rerun-preflight"
		_, _, write, err = selectCall(o)
		if err != nil || write {
			t.Fatalf("read-only preflight: %v %v", write, err)
		}
	}
	for _, id := range []string{"", "another-run", "v1:pipelines:run:738bdc26767a39a926a90f9c02ccc425"} {
		if _, _, _, err := selectCall(rerunOptions(id)); err == nil {
			t.Fatalf("unreviewed ID accepted: %q", id)
		}
	}
}

func TestRerunRejectsReleaseAndSourceOverrides(t *testing.T) {
	base := rerunOptions("738bdc26767a39a926a90f9c02ccc425")
	for name, change := range map[string]func(*options){
		"commit": func(o *options) { o.commit = strings.Repeat("a", 40) }, "tag": func(o *options) { o.tag = "cockpit/v0.21.0" },
		"candidate": func(o *options) { o.candidate = "x" }, "approval": func(o *options) { o.approval = "x" },
		"target": func(o *options) { o.target = "x" }, "artifact": func(o *options) { o.artifact = "x" },
		"configuration": func(o *options) { o.configuration = "x" }, "output": func(o *options) { o.output = "x" },
	} {
		t.Run(name, func(t *testing.T) {
			o := base
			change(&o)
			if _, _, _, err := selectCall(o); err == nil {
				t.Fatal("override accepted")
			}
		})
	}
}

func TestRerunSourceEveryRecordedBindingMustStillMatch(t *testing.T) {
	for _, expected := range rerunSources {
		if err := checkRerunSource(expected, expected); err != nil {
			t.Fatal(err)
		}
		value := reflect.ValueOf(expected)
		for i := 0; i < value.NumField(); i++ {
			t.Run(expected.ID+"/"+value.Type().Field(i).Name, func(t *testing.T) {
				got := expected
				field := reflect.ValueOf(&got).Elem().Field(i)
				switch field.Kind() {
				case reflect.String:
					field.SetString(field.String() + "-changed")
				case reflect.Int:
					field.SetInt(field.Int() + 1)
				case reflect.Bool:
					field.SetBool(!field.Bool())
				}
				if err := checkRerunSource(expected, got); err == nil {
					t.Fatal("changed source accepted")
				}
			})
		}
		for _, status := range []string{"queued", "in_progress", "cancelled", ""} {
			got := expected
			got.Status = status
			if checkRerunSource(expected, got) == nil {
				t.Fatalf("nonterminal %s accepted", status)
			}
		}
	}
}

func TestRerunRequiresTheSameActiveSourceConnection(t *testing.T) {
	source := rerunSources["738bdc26767a39a926a90f9c02ccc425"]
	p := goodRerunPipeline(source)
	if err := checkRerunPipeline(p, source); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < reflect.TypeOf(p).NumField(); i++ {
		got := p
		field := reflect.ValueOf(&got).Elem().Field(i)
		field.SetString(field.String() + "-changed")
		if checkRerunPipeline(got, source) == nil {
			t.Fatalf("changed %s accepted", reflect.TypeOf(p).Field(i).Name)
		}
	}
}

type fakeRerunReader struct {
	first, last rerunSource
	p           rerunPipeline
	reads       int
	err         error
}

func (f *fakeRerunReader) source(context.Context, string) (rerunSource, error) {
	f.reads++
	if f.err != nil {
		return rerunSource{}, f.err
	}
	if f.reads == 1 {
		return f.first, nil
	}
	return f.last, nil
}
func (f *fakeRerunReader) pipeline(context.Context, string) (rerunPipeline, error) { return f.p, nil }
func TestRerunPreflightReobservesTheOriginalAfterOtherReads(t *testing.T) {
	for id, s := range rerunSources {
		f := &fakeRerunReader{first: s, last: s, p: goodRerunPipeline(s)}
		if got, err := preflightRerun(context.Background(), f, rerunOptions(id)); err != nil || got != s || f.reads != 2 {
			t.Fatalf("got=%v reads=%d err=%v", got, f.reads, err)
		}
		f.reads = 0
		f.last.SHA = strings.Repeat("a", 40)
		if _, err := preflightRerun(context.Background(), f, rerunOptions(id)); err == nil {
			t.Fatal("source changed during preflight")
		}
		f.reads = 0
		f.last = s
		f.err = errors.New("read unavailable")
		if _, err := preflightRerun(context.Background(), f, rerunOptions(id)); err == nil {
			t.Fatal("failed read accepted")
		}
	}
}

func TestRerunExactRowDecoderRefusesAbsentAndAmbiguousResults(t *testing.T) {
	for _, raw := range []any{nil, map[string]any{}, map[string]any{"data": []any{}}, map[string]any{"data": []any{map[string]any{}, map[string]any{}}}} {
		if _, err := decodeOneRow[rerunSource](raw); err == nil {
			t.Fatalf("ambiguous row accepted: %v", raw)
		}
	}
	source := rerunSources["738bdc26767a39a926a90f9c02ccc425"]
	got, err := decodeOneRow[rerunSource](map[string]any{"data": []any{source}})
	if err != nil || got != source {
		t.Fatalf("got=%v err=%v", got, err)
	}
}
