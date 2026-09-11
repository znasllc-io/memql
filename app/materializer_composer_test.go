package app

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	pure "github.com/znasllc-io/memql/component/compose"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/router"
	"github.com/znasllc-io/memql/core/airoute"
	"github.com/znasllc-io/memql/core/common"
	composeint "github.com/znasllc-io/memql/integrations/compose"
)

type materializerFleet struct {
	online bool
	calls  []memql.FleetCallRequest
	answer string
	actor  string
}

func (f *materializerFleet) Catalog(ctx context.Context, user string) ([]memql.FleetModel, error) {
	access, ok := auth.AccessFromContext(ctx)
	if !ok || access.UserId != "alice" || user != "alice" {
		return nil, nil
	}
	return []memql.FleetModel{{ModelId: "writer:27b", Params: 27_000_000_000, ContextWindow: 131072, StructuredOutput: true,
		Machines: []memql.FleetMachine{{RegistrationId: "remote-laptop", Online: f.online}}}}, nil
}
func (*materializerFleet) ModelPreference(context.Context, string) ([]string, error) { return nil, nil }
func (f *materializerFleet) Call(ctx context.Context, req memql.FleetCallRequest) (memql.FleetCallResult, error) {
	f.calls = append(f.calls, req)
	if access, ok := auth.AccessFromContext(ctx); ok {
		f.actor = access.UserId
	}
	return memql.FleetCallResult{Content: f.answer, Usage: memql.FleetUsage{InputTokens: 11, OutputTokens: 17, Model: "writer:27b", Known: true}}, nil
}

func materializerTestEngine(t *testing.T, f *materializerFleet) *memql.MemQLEngine {
	t.Helper()
	providers := memql.NewProviderRegistryForTest()
	providers.SetFleetInference(f)
	rules := memql.NewRuleRegistry()
	if err := rules.Register(&memql.RuleConfig{Name: memql.DefaultRuleName, Policy: "localFirst", Locked: true,
		When: memql.RuleWhen{Present: map[string]bool{}}, OnUnavailable: memql.OnUnavailableDegrade}); err != nil {
		t.Fatal(err)
	}
	if err := rules.Finalize(); err != nil {
		t.Fatal(err)
	}
	r := router.New(providers, memql.NewPolicyRegistryForTest(map[string][]string{"localFirst": {"fleet:strongest"}}), rules, nil, nil)
	e := &memql.MemQLEngine{}
	e.SetAIResolver(r)
	return e
}

func TestMaterializerComposerUsesCallerFleetForEveryFormat(t *testing.T) {
	for _, format := range pure.Formats() {
		t.Run(string(format), func(t *testing.T) {
			f := &materializerFleet{online: true, answer: `{"title":"Quarterly report","body":"Revenue increased.","header":["quarter","revenue"],"rows":[["Q1",42]]}`}
			composer := materializerComposer{engine: materializerTestEngine(t, f)}
			ctx := auth.ContextWithUserActor(context.Background(), "alice")
			ctx = common.ContextWithFleetRegistration(ctx, "remote-laptop")
			reply, err := composer.Compose(ctx, composeint.ComposeRequest{Statement: "Summarize the quarter", Format: format, Draft: "Previous draft", TemplateName: "Annual style", TemplateBody: "Use short paragraphs"})
			if err != nil {
				t.Fatal(err)
			}
			if len(f.calls) != 1 || f.actor != "alice" {
				t.Fatalf("fleet calls=%d actor=%q", len(f.calls), f.actor)
			}
			call := f.calls[0]
			if call.RegistrationId != "remote-laptop" || call.ModelId != "writer:27b" || call.Schema == nil || !call.Schema.Strict {
				t.Fatalf("model, machine pin, or structured schema lost: %+v", call)
			}
			if len(reply.Models) != 1 || reply.Models[0].Model != "writer:27b" || reply.Models[0].Provider != "fleet:writer:27b" || reply.Models[0].Tokens != 28 || reply.Models[0].Calls != 1 {
				t.Fatalf("wrong model provenance: %+v", reply.Models)
			}
			if reply.Draft.Body != "Revenue increased." || reply.Draft.Rows[0]["revenue"] != json.Number("42") {
				t.Fatalf("draft lost prose or typed rows: %+v", reply.Draft)
			}
			if _, err := pure.Render(format, reply.Draft, pure.Provenance{Models: reply.Models}); err != nil {
				t.Fatalf("draft cannot render as %s: %v", format, err)
			}
			input := call.Messages[len(call.Messages)-1].Content
			if !strings.Contains(input, "Previous draft") || !strings.Contains(input, "Use short paragraphs") {
				t.Fatalf("caller draft/template were omitted: %s", input)
			}
			f.online = false
			if _, err := composer.Compose(ctx, composeint.ComposeRequest{Statement: "Write again", Format: format}); err == nil {
				t.Fatal("composer reused a provider after its machine went offline")
			}
			if len(f.calls) != 1 {
				t.Fatal("unavailable call dispatched inference")
			}
		})
	}
}

func TestMaterializerComposerRejectsMalformedDraft(t *testing.T) {
	for _, answer := range []string{
		`not json`,
		`{"title":"x","body":"","header":[],"rows":[]}`,
		`{"title":"x","body":"","header":["a"],"rows":[[1]]}`,
		`{"title":"x","body":"ok","header":["a","a"],"rows":[[1,2]]}`,
		`{"title":"x","body":"ok","header":["a","b"],"rows":[[1]]}`,
		`{"title":"x","body":"ok","header":["a"],"rows":[[{"nested":1}]]}`,
	} {
		t.Run(answer, func(t *testing.T) {
			f := &materializerFleet{online: true, answer: answer}
			_, err := (materializerComposer{engine: materializerTestEngine(t, f)}).Compose(auth.ContextWithUserActor(context.Background(), "alice"), composeint.ComposeRequest{Format: pure.FormatMarkdown})
			if err == nil {
				t.Fatal("invalid output was accepted as a finished draft")
			}
			if len(f.calls) != 1 {
				t.Fatalf("invalid output retried %d calls", len(f.calls))
			}
		})
	}
}

func TestMaterializerComposerRefusesProseInPlaceOfGeneratedData(t *testing.T) {
	f := &materializerFleet{online: true, answer: `{"title":"Data","body":"Here is your data.","header":[],"rows":[]}`}
	_, err := (materializerComposer{engine: materializerTestEngine(t, f)}).Compose(auth.ContextWithUserActor(context.Background(), "alice"), composeint.ComposeRequest{Format: pure.FormatCSV})
	if err == nil {
		t.Fatal("a data file with no data or sources would appear successfully materialized")
	}
}

// TestTheComposerImposesNoDeadlineOfItsOwn is the reported bug, pinned as the
// property that fixes it.
//
// Compose used to wrap its one model call in `context.WithTimeout(ctx,
// 3*time.Minute)`. A page of prose returns well inside that; a site, a long
// report, or a document with dozens of sources does not, and every one of
// them died at three minutes with "context deadline exceeded" -- a sentence
// naming a limit MemQL chose that says nothing about the work.
//
// So the assertion is that the deadline the caller arrived with is the
// deadline the model call gets, unchanged. A caller with none -- which is
// what the work spine dispatches with, because a goal has no duration --
// reaches the provider with none.
func TestTheComposerImposesNoDeadlineOfItsOwn(t *testing.T) {
	f := &materializerFleet{online: true, answer: `{"title":"Site","body":"Long document.","header":[],"rows":[]}`}
	spy := &deadlineSpy{inner: materializerTestEngine(t, f)}
	composer := materializerComposer{engine: spy}
	ctx := auth.ContextWithUserActor(context.Background(), "alice")

	if _, err := composer.Compose(ctx, composeint.ComposeRequest{Statement: "Build the site", Format: pure.FormatMarkdown}); err != nil {
		t.Fatal(err)
	}
	if !spy.called {
		t.Fatal("the composer never reached the model")
	}
	if spy.hadDeadline {
		t.Fatalf("the composer set its own deadline (%s from the call); an hour-long composition dies on it, "+
			"and the run that has to explain it reads words about a limit nobody asked for", spy.remaining)
	}

	// AND A CALLER'S OWN DEADLINE IS STILL HONOURED, which is the control
	// that keeps this from reading as "deadlines are ignored here". A person
	// cancelling, and a provider's own request limit, both still reach the
	// call -- what is gone is the one this file invented.
	spy.called, spy.hadDeadline = false, false
	bounded, cancel := context.WithTimeout(ctx, time.Hour)
	defer cancel()
	if _, err := composer.Compose(bounded, composeint.ComposeRequest{Statement: "Build the site", Format: pure.FormatMarkdown}); err != nil {
		t.Fatal(err)
	}
	if !spy.hadDeadline {
		t.Fatal("the caller's deadline did not reach the model call")
	}
	if spy.remaining < 50*time.Minute {
		t.Fatalf("remaining = %s, want close to the caller's hour: something shortened it", spy.remaining)
	}
}

// deadlineSpy reports what deadline the structured call was given. It wraps a
// real engine rather than faking one so the assertion is about the composer's
// context handling and nothing else.
type deadlineSpy struct {
	inner       materializerAI
	called      bool
	hadDeadline bool
	remaining   time.Duration
}

func (s *deadlineSpy) CallAIStructured(ctx context.Context, req airoute.ResolveRequest, msgs []common.ChatMessage, schema common.StructuredSchema) (memql.StructuredAIResult, error) {
	s.called = true
	if deadline, ok := ctx.Deadline(); ok {
		s.hadDeadline = true
		s.remaining = time.Until(deadline)
	}
	return s.inner.CallAIStructured(ctx, req, msgs, schema)
}

func TestComposeIntegrationIsWiredOnAgent(t *testing.T) {
	if !strings.Contains(readAppFile(t, "transport_agent.go"), "a.wireComposeIntegration(uploader, blobContainer)") {
		t.Fatal("Materializer work runs on agent nodes; their registered integration needs its composer and uploader")
	}
}
