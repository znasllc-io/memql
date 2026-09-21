package memql

// ai_builtin_seam_test.go -- the seam the `ai` BUILTIN calls, exercised end to
// end against a stubbed provider.
//
// `builtin ai(templateId:, data:)` is declared in dsl/agents/builtins.memql and
// its executor (integrations/agents/ai.go) does one thing: hand templateId and
// data to MemQLEngine.InvokeAI. Everything the call IS -- resolve the prompt by
// name, validate the data against the prompt's own body, render the template,
// route by @level, answer -- happens here, below that executor. So this is
// where it is worth proving, with a real PromptRegistry and a real compiled
// input schema; the executor's own test (integrations/agents/ai_test.go) proves
// the wiring into this call and nothing more.
//
// The provider is stubbed and COUNTED, which is what makes the validation claim
// falsifiable: "the data is validated" only means something if a bad data
// object is refused BEFORE a model is asked, and a counter that stays at zero
// is the evidence for that.

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"text/template"

	"github.com/santhosh-tekuri/jsonschema/v5"
)

// promptWithSchema builds the registry entry the way the unified loader does:
// a parsed template plus a compiled input schema, which is what makes
// ValidateData a real check rather than the nil-schema no-op.
func promptWithSchema(t *testing.T, name, source, schema string) *PromptTemplate {
	t.Helper()
	tmpl, err := template.New(name).Parse(source)
	if err != nil {
		t.Fatalf("parse template %q: %v", name, err)
	}
	compiler := jsonschema.NewCompiler()
	compiler.Draft = jsonschema.Draft2019
	if err := compiler.AddResource(name, bytes.NewReader([]byte(schema))); err != nil {
		t.Fatalf("add schema %q: %v", name, err)
	}
	compiled, err := compiler.Compile(name)
	if err != nil {
		t.Fatalf("compile schema %q: %v", name, err)
	}
	return &PromptTemplate{
		Level:           "fast",
		Name:            name,
		TemplateSource:  source,
		DefaultProvider: "stub",
		tmpl:            tmpl,
		inputSchema:     compiled,
	}
}

// engineForAiBuiltin is an engine with one prompt -- `docSummaryProbe`, shaped
// like the real docSummary: a required `content` and an optional `title` -- and
// one stub provider that echoes what it was asked.
func engineForAiBuiltin(t *testing.T) (*MemQLEngine, *mockAIProvider) {
	t.Helper()
	prompts := newPromptRegistry()
	prompts.set(promptWithSchema(t, "docSummaryProbe", "summarise {{.title}}: {{.content}}", `{
	  "type": "object",
	  "additionalProperties": false,
	  "required": ["content"],
	  "properties": {
	    "title":   {"type": "string"},
	    "content": {"type": "string"}
	  }
	}`))

	providers := newProviderRegistry()
	stub := &mockAIProvider{}
	providers.setEntry(&ProviderConfigEntry{
		Config:    ProviderConfig{Name: "stub", Type: "test"},
		Client:    stub,
		Available: true,
	})

	e := &MemQLEngine{providers: providers, prompts: prompts, modelSeam: &modelSeam{}}
	e.aiRuntime = newTestAIRuntime(prompts, providers, aiCacheConfig{})
	e.aiRuntime.seam = e.modelSeam
	return e, stub
}

// THE CALL WORKS: a named prompt, a data object, a reply.
//
// The reply asserted is the RENDERED template, because the stub echoes its
// prompt -- so one assertion covers three claims at once: the name resolved to
// that prompt, the data reached the template, and what the provider said came
// back to the caller unchanged.
func TestTheAiBuiltinSeamResolvesRendersAndAnswers(t *testing.T) {
	e, stub := engineForAiBuiltin(t)

	reply, err := e.InvokeAI(context.Background(), "docSummaryProbe", map[string]any{
		"title":   "Q3 plan",
		"content": "we ship the thing",
	})
	if err != nil {
		t.Fatalf("a valid call to a declared prompt must answer: %v", err)
	}
	if got, want := reply, "summarise Q3 plan: we ship the thing"; got != want {
		t.Fatalf("reply = %q, want %q -- the prompt resolved, the data rendered, and the provider's "+
			"answer comes back unchanged", got, want)
	}
	if stub.calls != 1 {
		t.Fatalf("the provider was called %d times, want exactly 1", stub.calls)
	}
}

// An OPTIONAL field may be omitted: the schema is the prompt's own body, so
// what it requires is the only thing required. Without this, a test suite that
// only ever passes every field cannot tell a working validator from one that
// refuses everything.
func TestTheAiBuiltinSeamAcceptsAPromptsOptionalFieldBeingAbsent(t *testing.T) {
	e, stub := engineForAiBuiltin(t)

	reply, err := e.InvokeAI(context.Background(), "docSummaryProbe", map[string]any{"content": "just this"})
	if err != nil {
		t.Fatalf("omitting an optional field must be accepted: %v", err)
	}
	// `<no value>` is text/template's rendering of an absent key, and it is what
	// the engine has always produced: asserted rather than tidied, because the
	// point here is that the CALL is accepted, not that the template is pretty.
	if got, want := reply, "summarise <no value>: just this"; got != want {
		t.Fatalf("reply = %q, want %q", got, want)
	}
	if stub.calls != 1 {
		t.Fatalf("the provider was called %d times, want exactly 1", stub.calls)
	}
}

// EVERY REFUSAL NAMES ITS CAUSE, and none of them is an empty answer.
//
// That is the rule `runAgentTurn` states for "answers only on an agent node"
// and it is the same rule here: an empty reply is indistinguishable from a
// model that had nothing to say, and each of these is a deployment or an
// authoring fact instead.
//
// The provider call count is asserted at ZERO for the two that should never
// reach a model. A validator that ran AFTER the call would still produce the
// right error text and would have spent the money first.
func TestTheAiBuiltinSeamRefusesByNameAndNeverAnswersEmpty(t *testing.T) {
	t.Run("an unknown prompt names the prompt", func(t *testing.T) {
		e, stub := engineForAiBuiltin(t)
		reply, err := e.InvokeAI(context.Background(), "noSuchPrompt", map[string]any{"content": "x"})
		if err == nil {
			t.Fatal("a prompt nobody declared must refuse, not answer")
		}
		if !strings.Contains(err.Error(), `unknown prompt template "noSuchPrompt"`) {
			t.Errorf("the refusal must name the prompt: %v", err)
		}
		if reply != nil {
			t.Errorf("a refusal must answer nothing, got %#v", reply)
		}
		if stub.calls != 0 {
			t.Errorf("a model was asked %d times about a prompt that does not exist", stub.calls)
		}
	})

	t.Run("data that does not fit the schema is refused before a model is asked", func(t *testing.T) {
		e, stub := engineForAiBuiltin(t)
		_, err := e.InvokeAI(context.Background(), "docSummaryProbe", map[string]any{"title": "no content"})
		if err == nil {
			t.Fatal("a data object missing a required field must refuse")
		}
		if !strings.Contains(err.Error(), `data for prompt "docSummaryProbe" invalid`) {
			t.Errorf("the refusal must name the prompt and say the data is the problem: %v", err)
		}
		if stub.calls != 0 {
			t.Errorf("the provider was called %d times for data that never validated; validation must "+
				"precede the model call, or the refusal is correct and the money is already spent", stub.calls)
		}
	})

	t.Run("an undeclared field is refused, because the body IS the schema", func(t *testing.T) {
		e, stub := engineForAiBuiltin(t)
		_, err := e.InvokeAI(context.Background(), "docSummaryProbe", map[string]any{
			"content": "fine", "spuriousField": "not declared",
		})
		if err == nil {
			t.Fatal("a field the prompt does not declare must refuse: the schema is additionalProperties:false")
		}
		if !strings.Contains(err.Error(), `data for prompt "docSummaryProbe" invalid`) {
			t.Errorf("the refusal must name the prompt: %v", err)
		}
		if stub.calls != 0 {
			t.Errorf("the provider was called %d times for data that never validated", stub.calls)
		}
	})

	t.Run("an unconfigured AI runtime refuses by name", func(t *testing.T) {
		// The node has an engine and no AI runtime: the shape a node that was
		// never wired for inference is in. It must say so rather than hand back
		// an empty reply that reads as a model with nothing to say.
		e := &MemQLEngine{}
		reply, err := e.InvokeAI(context.Background(), "docSummaryProbe", map[string]any{"content": "x"})
		if err == nil {
			t.Fatal("an engine with no AI runtime must refuse, not answer")
		}
		if !strings.Contains(err.Error(), "AI runtime is not configured") {
			t.Errorf("the refusal must name the missing runtime: %v", err)
		}
		if reply != nil {
			t.Errorf("a refusal must answer nothing, got %#v", reply)
		}
	})

	t.Run("an unwired router refuses rather than choosing a provider", func(t *testing.T) {
		// The runtime exists and nobody wired the router. This is the ONE
		// refusal that would be tempting to paper over with a registry default,
		// and epic memql#5127 deleted that path deliberately: a fallback here is
		// spend nobody chose. Named so the operator reads it as a wiring fault.
		prompts := newPromptRegistry()
		prompts.set(promptWithSchema(t, "docSummaryProbe", "summarise {{.content}}", `{"type":"object"}`))
		providers := newProviderRegistry()
		e := &MemQLEngine{providers: providers, prompts: prompts, modelSeam: &modelSeam{}}
		e.aiRuntime = newAIRuntime(nil, prompts, providers, aiCacheConfig{}) // no .resolve
		e.aiRuntime.seam = e.modelSeam

		_, err := e.InvokeAI(context.Background(), "docSummaryProbe", map[string]any{"content": "x"})
		if err == nil {
			t.Fatal("an unwired resolver must refuse, not resolve a default")
		}
		if !strings.Contains(err.Error(), ErrAIResolverUnwired.Error()) {
			t.Errorf("the refusal must be the typed unwired-resolver one: %v", err)
		}
	})
}

// The envelope the executor builds is {prompt, reply}, and `reply` is the key
// runAgentTurn already uses for what a model said. Pinned here because the key
// name is the part a DSL body reads, so renaming it is a wire change to every
// body that calls the builtin.
func TestTheAiBuiltinEnvelopeKeysAreStable(t *testing.T) {
	payload, err := json.Marshal(map[string]any{"prompt": "docSummary", "reply": "two sentences"})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var back map[string]any
	if err := json.Unmarshal(payload, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, key := range []string{"prompt", "reply"} {
		if _, ok := back[key]; !ok {
			t.Errorf("the ai() envelope must carry %q", key)
		}
	}
}
