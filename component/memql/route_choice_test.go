package memql

// The Ask route picker's choice (design brief section 6): its grammar, and
// what it does to every model call a run makes.

import (
	"context"
	"errors"
	"testing"

	"github.com/znasllc-io/memql/core/airoute"
	"github.com/znasllc-io/memql/core/common"
)

func TestParseRouteChoiceAcceptsExactlyThePickersVocabulary(t *testing.T) {
	for _, source := range []string{
		"", "app:claude-code", "app:codex", "app:*",
		"fleet:strongest", "fleet:fastest", "fleet:qwen3.5:4b",
		"federation:cheapest", "federation:strongest",
		"policy:localFirst",
	} {
		for _, level := range []string{"", "fast", "strong", "reasoning"} {
			got, err := ParseRouteChoice(" "+source+" ", level)
			if err != nil {
				t.Fatalf("ParseRouteChoice(%q, %q): %v", source, level, err)
			}
			if got.Source != source || got.Level != level {
				t.Fatalf("ParseRouteChoice(%q, %q) = %+v", source, level, got)
			}
			if got.By != "" {
				t.Fatalf("the grammar stamped By=%q; only the server stamps who chose", got.By)
			}
		}
	}
}

func TestParseRouteChoiceRefusesWithACode(t *testing.T) {
	cases := []struct{ source, level, code, field string }{
		{"app:cursor", "", RouteSourceInvalid, "source"},           // not an app the engine drives
		{"app:claude-code:opus", "", RouteSourceInvalid, "source"}, // a model pin is not a picker choice
		{"app:", "", RouteSourceInvalid, "source"},
		{"fleet:*", "", RouteSourceInvalid, "source"}, // the retired wildcard
		{"fleet:", "", RouteSourceInvalid, "source"},
		{"fleet:has space", "", RouteSourceInvalid, "source"},
		{"federation:fastest", "", RouteSourceInvalid, "source"},
		{"streamClaudeSonnet", "", RouteSourceInvalid, "source"}, // a bare provider record
		{"policy:", "", RouteSourceInvalid, "source"},
		{"policy:local first", "", RouteSourceInvalid, "source"},
		{"auto", "", RouteSourceInvalid, "source"},
		{"", "embeddings", RouteLevelInvalid, "level"}, // a level a call declares, never one a person picks
		{"", "Strong", RouteLevelInvalid, "level"},
		{"", "max", RouteLevelInvalid, "level"},
	}
	for _, tc := range cases {
		_, err := ParseRouteChoice(tc.source, tc.level)
		var refusal *RouteChoiceError
		if !errors.As(err, &refusal) {
			t.Fatalf("ParseRouteChoice(%q, %q) = %v, want a *RouteChoiceError", tc.source, tc.level, err)
		}
		if refusal.Code != tc.code || refusal.Field != tc.field {
			t.Fatalf("ParseRouteChoice(%q, %q) refused %s/%s, want %s/%s", tc.source, tc.level, refusal.Code, refusal.Field, tc.code, tc.field)
		}
		if got := err.Error(); len(got) < len(tc.code) || got[:len(tc.code)] != tc.code {
			t.Fatalf("the refusal must LEAD with its code: %q", got)
		}
	}
}

// stepOf is a context for a step of a run whose owner chose routing.
func stepOf(stepKey string, routing common.RouteChoice, ov *common.StepOverride) context.Context {
	return common.ContextWithRun(context.Background(), common.RunContext{
		RunId: "v1:work:run:r1", GoalId: "v1:work:goal:g1", OwnerUserId: "alice",
		StepKey: stepKey, Mode: common.RunModeLive, Routing: routing, Override: ov,
	})
}

func TestAPinnedSourceBecomesTheOwnersPinOnEveryTextCall(t *testing.T) {
	choice := common.RouteChoice{Source: "app:claude-code", By: "alice"}
	for _, mod := range []airoute.Modality{
		airoute.ModalityChat, airoute.ModalityStreamingChat, airoute.ModalityTools,
		airoute.ModalityStreamingTools, airoute.ModalityStructured,
	} {
		// A prompt author's pin is overruled: the person chose where their
		// conversation is served, and an @defaultProvider is nobody's choice.
		req := airoute.ResolveRequest{Level: airoute.LevelFast, Modality: mod, ExplicitProvider: "authorsDefault"}
		for _, ctx := range []context.Context{stepOf("reason", choice, nil), stepOf("", choice, nil)} {
			got, err := ApplyRunRouting(ctx, req)
			if err != nil {
				t.Fatal(err)
			}
			if got.ExplicitProvider != "app:claude-code" || got.PinnedBy != "alice" || got.Route != "" {
				t.Fatalf("%s: pinned %q by %q route %q, want the owner's pin", mod, got.ExplicitProvider, got.PinnedBy, got.Route)
			}
		}
	}
}

func TestARouteReplacesTheRulesChainAndClearsAnAuthorsPin(t *testing.T) {
	req := airoute.ResolveRequest{Level: airoute.LevelStrong, Modality: airoute.ModalityStructured,
		ExplicitProvider: "authorsDefault", PinnedBy: ""}
	got, err := ApplyRunRouting(stepOf("", common.RouteChoice{Source: "policy:localFirst", By: "alice"}, nil), req)
	if err != nil {
		t.Fatal(err)
	}
	if got.Route != "localFirst" || got.ExplicitProvider != "" || got.PinnedBy != "" {
		t.Fatalf("route=%q pin=%q by=%q, want the route alone", got.Route, got.ExplicitProvider, got.PinnedBy)
	}
}

// Triage and compile run while the run is COMPILING, on the planner, with no
// step; the failure classifier runs on the run's own context, with no step.
// They keep their own level and still go where the person said.
func TestTheLevelBindsTheRunsStepsAndNothingElse(t *testing.T) {
	choice := common.RouteChoice{Source: "fleet:strongest", Level: "reasoning", By: "alice"}
	req := airoute.ResolveRequest{Level: airoute.LevelFast, Modality: airoute.ModalityStructured, PromptName: "goalComplexityTriage"}

	compile, err := ApplyRunRouting(stepOf("", choice, nil), req)
	if err != nil {
		t.Fatal(err)
	}
	if compile.Level != airoute.LevelFast || compile.ExplicitProvider != "fleet:strongest" {
		t.Fatalf("a compile-time call ran at %q on %q, want its own level on the chosen source", compile.Level, compile.ExplicitProvider)
	}
	step, err := ApplyRunRouting(stepOf("reason", choice, nil), req)
	if err != nil {
		t.Fatal(err)
	}
	if step.Level != airoute.LevelReasoning {
		t.Fatalf("a step's call ran at %q, want the chosen level", step.Level)
	}
}

// A PERSON'S OVERRIDE FOR ONE STEP is more specific than their choice for the
// whole conversation, so it wins -- knob by knob.
func TestAStepOverrideOutranksTheRunsChoice(t *testing.T) {
	choice := common.RouteChoice{Source: "app:claude-code", Level: "fast", By: "alice"}
	req := airoute.ResolveRequest{Level: airoute.LevelStrong, Modality: airoute.ModalityTools}

	overridden, err := ApplyStepOverride(stepOf("reason", choice, &common.StepOverride{Model: "fleet:llama", Level: "reasoning", RequestedBy: "alice"}), req)
	if err != nil {
		t.Fatal(err)
	}
	got, err := ApplyRunRouting(stepOf("reason", choice, &common.StepOverride{Model: "fleet:llama", Level: "reasoning", RequestedBy: "alice"}), overridden)
	if err != nil {
		t.Fatal(err)
	}
	if got.ExplicitProvider != "fleet:llama" || got.Level != airoute.LevelReasoning {
		t.Fatalf("pin=%q level=%q, want the step override's", got.ExplicitProvider, got.Level)
	}

	// An override that names only an effort changes neither knob, so the
	// run's choice still applies to both.
	got, err = ApplyRunRouting(stepOf("reason", choice, &common.StepOverride{Effort: "high"}), req)
	if err != nil {
		t.Fatal(err)
	}
	if got.ExplicitProvider != "app:claude-code" || got.Level != airoute.LevelFast {
		t.Fatalf("pin=%q level=%q, want the run's choice", got.ExplicitProvider, got.Level)
	}
}

// An embedding must come from the embedder of the index it is written into
// (design D10); vision and audio have capability floors a conversation's
// source says nothing about.
func TestTheChoiceLeavesEmbeddingsVisionAndAudioAlone(t *testing.T) {
	choice := common.RouteChoice{Source: "app:claude-code", Level: "reasoning", By: "alice"}
	for _, req := range []airoute.ResolveRequest{
		{Level: airoute.LevelEmbeddings, Modality: airoute.ModalityEmbedding},
		{Level: airoute.LevelStrong, Modality: airoute.ModalityVision},
		{Level: airoute.LevelFast, Modality: airoute.ModalitySpeech},
		{Level: airoute.LevelFast, Modality: airoute.ModalityTranscribe},
	} {
		got, err := ApplyRunRouting(stepOf("reason", choice, nil), req)
		if err != nil {
			t.Fatal(err)
		}
		if got.ExplicitProvider != "" || got.Level != req.Level || got.Route != "" {
			t.Fatalf("%s call was re-routed to %q at %q", req.Modality, got.ExplicitProvider, got.Level)
		}
	}
}

func TestNoChoiceChangesNothing(t *testing.T) {
	req := airoute.ResolveRequest{Level: airoute.LevelStrong, Modality: airoute.ModalityTools, ExplicitProvider: "authorsDefault"}
	for _, ctx := range []context.Context{context.Background(), stepOf("reason", common.RouteChoice{}, nil)} {
		got, err := ApplyRunRouting(ctx, req)
		if err != nil {
			t.Fatal(err)
		}
		if got.ExplicitProvider != "authorsDefault" || got.Level != airoute.LevelStrong || got.Route != "" {
			t.Fatalf("an Auto run changed the request: %+v", got)
		}
	}
}

// A choice that no longer parses is a corrupted row: running anyway would
// record a call as served for a person who chose something else.
func TestAnUnreadableChoiceRefusesTheCall(t *testing.T) {
	_, err := ApplyRunRouting(stepOf("reason", common.RouteChoice{Source: "vendor:whatever", By: "alice"}, nil),
		airoute.ResolveRequest{Level: airoute.LevelStrong, Modality: airoute.ModalityTools})
	var refusal *RouteChoiceError
	if !errors.As(err, &refusal) || refusal.Code != RouteSourceInvalid {
		t.Fatalf("err = %v, want a route_source_invalid refusal", err)
	}
}

// A model call made for a person outside any run -- Ask deciding where their
// message goes, before any goal exists -- goes where they said: the choice
// stamped on the turn's context applies as a run's would. Only to that
// person's own calls, and never over a run's choice.
func TestApplyRunRoutingUsesTheTurnChoiceOutsideARun(t *testing.T) {
	req := airoute.ResolveRequest{Level: airoute.LevelFast, Modality: airoute.ModalityStructured, ExplicitProvider: "authorsDefault"}
	turn := func(user string, choice common.RouteChoice) context.Context {
		return ContextWithGoalRouteChoice(userCtx(user), choice)
	}

	got, err := ApplyRunRouting(turn("alice", common.RouteChoice{Source: "policy:local", By: "alice"}), req)
	if err != nil {
		t.Fatal(err)
	}
	if got.Route != "local" || got.ExplicitProvider != "" || got.PinnedBy != "" {
		t.Fatalf("route=%q pin=%q by=%q, want the turn's route alone", got.Route, got.ExplicitProvider, got.PinnedBy)
	}

	// A person's own route (Local, Apps, or one they made) is walked for them.
	got, err = ApplyRunRouting(turn("alice", common.RouteChoice{Source: "route:local", By: "v1:identity:user:alice"}), req)
	if err != nil {
		t.Fatal(err)
	}
	if got.PersonRoute != "local" || got.PersonRouteOwner != "v1:identity:user:alice" || got.Route != "" || got.ExplicitProvider != "" {
		t.Fatalf("person route=%q owner=%q route=%q pin=%q, want the person's route", got.PersonRoute, got.PersonRouteOwner, got.Route, got.ExplicitProvider)
	}

	// A pin is the person's own, so their app door is theirs to open.
	got, err = ApplyRunRouting(turn("alice", common.RouteChoice{Source: "app:claude-code", By: "alice"}), req)
	if err != nil {
		t.Fatal(err)
	}
	if got.ExplicitProvider != "app:claude-code" || got.PinnedBy != "alice" {
		t.Fatalf("pin=%q by=%q, want alice's pin", got.ExplicitProvider, got.PinnedBy)
	}

	// The level binds a run's steps; a call outside a run has none.
	got, err = ApplyRunRouting(turn("alice", common.RouteChoice{Level: "reasoning", By: "alice"}), req)
	if err != nil {
		t.Fatal(err)
	}
	if got.Level != airoute.LevelFast {
		t.Fatalf("a call outside a run ran at %q, want its own level", got.Level)
	}

	// No paid providers is a ceiling on every call made for the person.
	got, err = ApplyRunRouting(turn("alice", common.RouteChoice{NoPaidProviders: true, By: "alice"}), airoute.ResolveRequest{Level: airoute.LevelEmbeddings, Modality: airoute.ModalityEmbedding})
	if err != nil {
		t.Fatal(err)
	}
	if !got.NoPaidProviders {
		t.Fatal("the turn's no-paid-providers ceiling was dropped outside a run")
	}

	// Alice's choice never routes a call made for somebody else.
	got, err = ApplyRunRouting(turn("bob", common.RouteChoice{Source: "app:claude-code", By: "alice"}), req)
	if err != nil {
		t.Fatal(err)
	}
	if !sameRouting(got, req) {
		t.Fatalf("alice's choice routed bob's call: %+v", got)
	}

	// Inside a run, the run's choice wins over a stamp on the same context,
	// and an Auto run stays Auto.
	stamp := common.RouteChoice{Source: "policy:local", By: "alice"}
	got, err = ApplyRunRouting(ContextWithGoalRouteChoice(stepOf("reason", common.RouteChoice{Source: "app:codex", By: "alice"}, nil), stamp), req)
	if err != nil {
		t.Fatal(err)
	}
	if got.ExplicitProvider != "app:codex" || got.Route != "" {
		t.Fatalf("pin=%q route=%q, want the run's choice", got.ExplicitProvider, got.Route)
	}
	got, err = ApplyRunRouting(ContextWithGoalRouteChoice(stepOf("reason", common.RouteChoice{}, nil), stamp), req)
	if err != nil {
		t.Fatal(err)
	}
	if !sameRouting(got, req) {
		t.Fatalf("a stamp re-routed an Auto run's call: %+v", got)
	}

	// A stamp is an exported seam: one that does not parse refuses the call.
	_, err = ApplyRunRouting(turn("alice", common.RouteChoice{Source: "vendor:whatever", By: "alice"}), req)
	var refusal *RouteChoiceError
	if !errors.As(err, &refusal) || refusal.Code != RouteSourceInvalid {
		t.Fatalf("err = %v, want a route_source_invalid refusal", err)
	}
}

// sameRouting reports whether a request still goes where it went before a
// choice was applied: the same source, route, level and ceiling.
func sameRouting(a, b airoute.ResolveRequest) bool {
	return a.ExplicitProvider == b.ExplicitProvider && a.PinnedBy == b.PinnedBy && a.Route == b.Route &&
		a.PersonRoute == b.PersonRoute && a.PersonRouteOwner == b.PersonRouteOwner && a.Level == b.Level &&
		a.NoPaidProviders == b.NoPaidProviders
}

// A goal opened by a request carries the caller's choice; a goal opened by one
// of a run's steps carries that run's choice. Either only for the person who
// made it -- a choice is never lent to somebody else's goal.
func TestAGoalTakesTheChoiceOfTheRequestOrOfTheRunThatOpenedIt(t *testing.T) {
	requested := common.RouteChoice{Source: "fleet:fastest", Level: "fast", By: "v1:identity:user:alice"}
	got := GoalRouteChoice(ContextWithGoalRouteChoice(context.Background(), requested), "alice")
	if got != requested {
		t.Fatalf("requested choice = %+v, want %+v", got, requested)
	}
	if got := GoalRouteChoice(ContextWithGoalRouteChoice(context.Background(), requested), "mallory"); !got.IsZero() {
		t.Fatalf("alice's choice reached mallory's goal: %+v", got)
	}
	// A stamp naming nobody is no stamp: who chose is the server's to say.
	if got := GoalRouteChoice(ContextWithGoalRouteChoice(context.Background(), common.RouteChoice{Source: "app:codex"}), "alice"); !got.IsZero() {
		t.Fatalf("an unattributed stamp applied: %+v", got)
	}

	inRun := stepOf("reason", common.RouteChoice{Source: "app:codex", By: "v1:identity:user:alice"}, nil)
	if got := GoalRouteChoice(inRun, "alice"); got.Source != "app:codex" {
		t.Fatalf("a goal opened by the owner's run = %+v, want the run's choice", got)
	}
	if got := GoalRouteChoice(inRun, "bob"); !got.IsZero() {
		t.Fatalf("a goal for somebody else inherited %+v", got)
	}
	// The request's own choice wins over the run it was made from.
	both := ContextWithGoalRouteChoice(inRun, common.RouteChoice{Source: "policy:localFirst", By: "alice"})
	if got := GoalRouteChoice(both, "alice"); got.Source != "policy:localFirst" {
		t.Fatalf("got %+v, want the request's choice", got)
	}
	if got := GoalRouteChoice(context.Background(), "alice"); !got.IsZero() {
		t.Fatalf("no choice anywhere = %+v, want Auto", got)
	}
}
