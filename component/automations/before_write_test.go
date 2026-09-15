package automations

import (
	"context"
	"github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/events"
	"github.com/znasllc-io/memql/component/memql"
	"strings"
	"testing"
)

func TestBeforeWriteCompilesAndRefusesWrites(t *testing.T) {
	loader := NewLoader(LoaderOptions{})
	a, err := loader.CompileSource(`@trigger(before="create", concept="v1:probe:ticket")
automation adjust {
 row.status = "queued"
 if row.status == "queued" {
   row.label = row.status
 }
}`, "test")
	if err != nil {
		t.Fatal(err)
	}
	if a.BeforeWrite == nil || a.BeforeWrite.On != "create" || a.IsEventTriggered() || len(a.Steps) != 3 || a.Steps[0].FieldWrite.Field != "status" {
		t.Fatalf("unexpected compiled body: %+v", a)
	}
	for _, tc := range []struct{ source, code string }{
		{`@trigger(before="create", concept="v1:probe:ticket")
automation bad { mutation other() }`, "before_write_writes"},
		{`@trigger(before="create", concept="v1:probe:ticket")
automation bad { row.id = "other" }`, "before_write_field"},
		{`@trigger(event="probe")
automation bad { row.status = "other" }`, "before_write_outside"},
		{`@trigger(before="create", event="probe", concept="v1:probe:ticket")
automation bad { row.status = "other" }`, "before_write_trigger"},
		{`@trigger(before="create", concept="v1:probe:ticket")
automation bad { row.nested.status = "other" }`, "before_write_field"},
		{`@trigger(before="create", concept="v1:probe:ticket")
automation bad { publish "other" {} }`, "before_write_writes"},
	} {
		_, err := loader.CompileSource(tc.source, "test")
		if err == nil || !strings.Contains(err.Error(), tc.code) {
			t.Errorf("want %s, got %v", tc.code, err)
		}
	}
}

func TestBeforeWriteRetainsBranchAndReturnsWithoutAnotherWrite(t *testing.T) {
	loader := NewLoader(LoaderOptions{})
	a, err := loader.CompileSource(`@trigger(before="write", concept="v1:probe:ticket")
automation adjust {
 if row.status == "submitted" {
  row.status = "queued"
  row.label = row.status
 } else {
  row.label = "wrong branch"
 }
 return
 row.label = "past return"
}`, "test")
	if err != nil {
		t.Fatal(err)
	}
	row := map[string]any{"status": "submitted"}
	hooks := BuildBeforeWriteHooks(&memql.MemQLEngine{}, []*Automation{a}, nil, nil)
	if err := hooks[a.BeforeWrite.Concept][0].Apply(context.Background(), row); err != nil {
		t.Fatal(err)
	}
	if row["status"] != "queued" || row["label"] != "queued" {
		t.Fatal(row)
	}
}

func TestBeforeWriteChecksTransitiveLogicCalls(t *testing.T) {
	fns := newTestFunctionRegistry()
	for _, fn := range []*memql.Function{
		{Name: "pure", FunctionKind: "logic", Origin: "unified:probe/logic.memql", LogicBody: []map[string]any{{"id": "return", "type": "return", "return": map[string]any{"value": "args.status"}}}},
		{Name: "impure", FunctionKind: "logic", Origin: "unified:probe/logic.memql", LogicBody: []map[string]any{{"id": "write", "type": "function", "function": map[string]any{"name": "write", "kind": "mutation"}}}},
		{Name: "write", FunctionKind: "mutation", Origin: "unified:probe/mutations.memql"},
	} {
		if err := fns.Upsert(fn); err != nil {
			t.Fatal(err)
		}
	}
	loader := NewLoader(LoaderOptions{Functions: fns})
	for _, name := range []string{"pure", "impure", "missing"} {
		_, err := loader.CompileSource(`@trigger(before="write", concept="v1:probe:ticket")
automation adjust {
 chosen := logic `+name+`(status: row.status)
 row.status = chosen
}`, "test")
		if name == "pure" && err != nil {
			t.Fatal(err)
		}
		if name != "pure" && (err == nil || !strings.Contains(err.Error(), "before_write_writes")) {
			t.Fatalf("%s: %v", name, err)
		}
	}
}

func TestBeforeWriteFilterBindsItsOwnParameter(t *testing.T) {
	a, err := NewLoader(LoaderOptions{}).CompileSource(`@trigger(before="write", concept="v1:probe:ticket")
@filter(incoming => incoming.status == "submitted")
automation adjust { row.status = "queued" }`, "test")
	if err != nil {
		t.Fatal(err)
	}
	hook := BuildBeforeWriteHooks(&memql.MemQLEngine{}, []*Automation{a}, nil, nil)[a.BeforeWrite.Concept][0]
	for _, status := range []string{"submitted", "unchanged"} {
		row := map[string]any{"status": status}
		if err := hook.Apply(context.Background(), row); err != nil {
			t.Fatal(err)
		}
		want := status
		if status == "submitted" {
			want = "queued"
		}
		if row["status"] != want {
			t.Fatal(row)
		}
	}
}

func TestBeforeWriteAuthoredActivationHasNoBusSubscription(t *testing.T) {
	bus := events.NewBus()
	defer bus.Close()
	engine := &memql.MemQLEngine{}
	concept, err := memorynodes.ParseConceptMemQL([]byte(`concept ticket { title string }`), "v1/forge/request")
	if err != nil {
		t.Fatal(err)
	}
	registry := memorynodes.NewRegistry(map[string]*memorynodes.Concept{"v1:forge:request": concept})
	scheduler, err := NewAuthoredScheduler(AuthoredSchedulerOptions{Loader: NewLoader(LoaderOptions{Registry: registry}), EventBus: bus, BeforeWriteEngine: engine, Run: func(context.Context, *Automation, *events.Event) error {
		t.Fatal("before-write invoked event runner")
		return nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	defer scheduler.Stop()
	construct := &memql.AuthoredConstruct{OwnerUserId: "owner-a", Name: "adjust", Kind: "automation", Source: `@trigger(before="write", concept="v1:forge:request")
automation adjust { row.title = "adjusted" }`}
	if err := scheduler.Activate(construct); err != nil {
		t.Fatal(err)
	}
	if !scheduler.IsActive("owner-a", "adjust") {
		t.Fatal("not activated")
	}
	scheduler.Deactivate("owner-a", "adjust")
	if scheduler.IsActive("owner-a", "adjust") {
		t.Fatal("not deactivated")
	}
}

type beforeWriteOriginProbe struct {
	t      *testing.T
	called bool
}

func (p *beforeWriteOriginProbe) Execute(ctx context.Context, s *Step, _ *StepContext) (*StepResult, error) {
	p.called = true
	if auth.OriginFromContext(ctx).IsInternal() {
		p.t.Fatal("untrusted hook borrowed internal origin")
	}
	return &StepResult{StepId: s.ID, Status: "success", Result: "ready"}, nil
}
func TestBeforeWriteUntrustedSourceCannotBorrowInternalOrigin(t *testing.T) {
	fns := newTestFunctionRegistry()
	if err := fns.Upsert(&memql.Function{Name: "read", FunctionKind: "query", Origin: "unified:probe/queries.memql"}); err != nil {
		t.Fatal(err)
	}
	a, err := NewLoader(LoaderOptions{Functions: fns}).CompileSource(`@trigger(before="write", concept="v1:probe:ticket")
automation adjust { value := query read() }`, "authored:owner:adjust")
	if err != nil {
		t.Fatal(err)
	}
	registry := &beforeWriteOriginProbe{t: t}
	hook := BuildBeforeWriteHooks(&memql.MemQLEngine{}, []*Automation{a}, registry, nil)[a.BeforeWrite.Concept][0]
	if err := hook.Apply(auth.ContextWithInternalOrigin(AuthorContext(context.Background(), "owner")), map[string]any{}); err != nil {
		t.Fatal(err)
	}
	if !registry.called {
		t.Fatal("query did not execute")
	}
}

func TestAuthoredBeforeWriteCannotChangeServerOrOwnershipFields(t *testing.T) {
	c, err := memorynodes.ParseConceptMemQL([]byte(`@rowAuthz(owner="ownerUserId", account="accountId")
concept ticket {
 title string
 ownerUserId string
 secret string @internal
 server string @serverSet
 accountId string
}`), "v1/probe/ticket")
	if err != nil {
		t.Fatal(err)
	}
	s := &AuthoredScheduler{loader: NewLoader(LoaderOptions{Registry: memorynodes.NewRegistry(map[string]*memorynodes.Concept{"v1:probe:ticket": c})})}
	for _, field := range []string{"title", "ownerUserId", "secret", "server", "accountId"} {
		a := &Automation{Name: "adjust", BeforeWrite: &BeforeWriteConfig{On: "write", Concept: "v1:probe:ticket"}, Steps: []*Step{{Type: StepTypeFieldWrite, FieldWrite: &FieldWriteConfig{Field: field, Value: "other"}}}}
		err := s.validateBeforeWriteFields(a)
		if field == "title" && err != nil {
			t.Fatal(err)
		}
		if field != "title" && err == nil {
			t.Fatalf("authored hook may change %s", field)
		}
	}
}
