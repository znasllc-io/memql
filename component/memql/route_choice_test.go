package memql

// The Ask route picker's choice (design brief section 6): its grammar, and
// what it does to every model call a run makes.

import (
	"context"
	"errors"
	"testing"

	"github.com/znasllc-io/memql/component/auth"
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

// Which account pays depends on WHO CHOSE (design record
// 2026-11-21-vendor-accounts-and-api-keys, K8 lines 4 and 5): a route the
// acting person chose, or a model they pinned, may be paid by their key; Auto
// counts only keys switched on for it. The request therefore says who chose.
func TestApplyRunRoutingNamesWhoChose(t *testing.T) {
	req := airoute.ResolveRequest{Level: airoute.LevelStrong, Modality: airoute.ModalityChat}
	for name, tc := range map[string]struct {
		choice            common.RouteChoice
		routeBy, pinnedBy string
	}{
		"a person's route": {common.RouteChoice{Source: "route:apps", By: "alice"}, "alice", ""},
		"a policy route":   {common.RouteChoice{Source: "policy:localFirst", By: "alice"}, "alice", ""},
		"a pinned source":  {common.RouteChoice{Source: "federation:strongest", By: "alice"}, "", "alice"},
		"auto":             {common.RouteChoice{Level: "strong", By: "alice"}, "", ""},
	} {
		got, err := ApplyRunRouting(stepOf("reason", tc.choice, nil), req)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if got.RouteBy != tc.routeBy || got.PinnedBy != tc.pinnedBy {
			t.Errorf("%s: route chosen by %q, pinned by %q; want %q and %q", name, got.RouteBy, got.PinnedBy, tc.routeBy, tc.pinnedBy)
		}
	}
	// A run nobody chose for names nobody.
	got, err := ApplyRunRouting(stepOf("reason", common.RouteChoice{}, nil), req)
	if err != nil || got.RouteBy != "" || got.PinnedBy != "" {
		t.Fatalf("a run nobody chose for: route by %q, pinned by %q, err %v", got.RouteBy, got.PinnedBy, err)
	}
}

// A goal a visitor's request opened runs later, as its owner's own run, on a
// node that never saw the request. The mark is on the run's routing, and every
// model call of the run carries it, whatever its modality.
func TestApplyRunRoutingCarriesAVisitorsMark(t *testing.T) {
	marked := common.RouteChoice{By: "alice", ForVisitor: true}
	for _, mod := range []airoute.Modality{airoute.ModalityChat, airoute.ModalityStreamingTools, airoute.ModalityStructured, airoute.ModalityEmbedding} {
		got, err := ApplyRunRouting(stepOf("reason", marked, nil), airoute.ResolveRequest{Level: airoute.LevelFast, Modality: mod})
		if err != nil {
			t.Fatal(err)
		}
		if !got.ForVisitor {
			t.Errorf("%s: a visitor's run reached the router as the owner's own work", mod)
		}
	}
	got, err := ApplyRunRouting(stepOf("reason", common.RouteChoice{Source: "route:apps", By: "alice"}, nil),
		airoute.ResolveRequest{Level: airoute.LevelFast, Modality: airoute.ModalityChat})
	if err != nil || got.ForVisitor {
		t.Fatalf("a person's own run was marked as a visitor's (err %v)", err)
	}
}

// A choice made through a credential that acts as a person without being them
// reaches the router marked, on every call of the run, so the account rule can
// tell it from the person's own (vendor accounts design, K8 and O4).
func TestApplyRunRoutingCarriesAChoiceMadeByAMachine(t *testing.T) {
	req := airoute.ResolveRequest{Level: airoute.LevelStrong, Modality: airoute.ModalityChat}
	for name, source := range map[string]string{"a pinned source": "federation:strongest", "a person's route": "route:apps", "a policy route": "policy:localFirst"} {
		got, err := ApplyRunRouting(stepOf("reason", common.RouteChoice{Source: source, By: "alice", ByMachine: true}, nil), req)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if !got.ChoiceByMachine {
			t.Errorf("%s: a choice an app session made reached the router as the person's own", name)
		}
		// Who it is attributed to is unchanged: an app door reached through a
		// pin still reads it as the owner's.
		if got.PinnedBy+got.RouteBy != "alice" {
			t.Errorf("%s: pinned by %q, route by %q", name, got.PinnedBy, got.RouteBy)
		}
	}
	own, err := ApplyRunRouting(stepOf("reason", common.RouteChoice{Source: "federation:strongest", By: "alice"}, nil), req)
	if err != nil || own.ChoiceByMachine {
		t.Fatalf("a person's own choice was marked as a machine's (err %v)", err)
	}

	// THE MARK DESCRIBES THE CHOICE IT TRAVELS WITH. A run's choice replaces
	// whatever the request itself named, so it replaces that pin's answer too:
	// a hint an app session put on a turn does not make the route its person
	// chose read as the app's, and a person's hint does not launder a run an
	// app session opened.
	hinted := airoute.ResolveRequest{Level: airoute.LevelStrong, Modality: airoute.ModalityChat, ExplicitProvider: "claudeSonnet", PinnedBy: "alice", ChoiceByMachine: true}
	got, err := ApplyRunRouting(stepOf("reason", common.RouteChoice{Source: "route:apps", By: "alice"}, nil), hinted)
	if err != nil || got.RouteBy != "alice" || got.ChoiceByMachine {
		t.Fatalf("a person's own route, on a turn whose hint an app made: route by %q, by machine %v, err %v", got.RouteBy, got.ChoiceByMachine, err)
	}
	hinted.ChoiceByMachine = false
	got, err = ApplyRunRouting(stepOf("reason", common.RouteChoice{Source: "route:apps", By: "alice", ByMachine: true}, nil), hinted)
	if err != nil || !got.ChoiceByMachine {
		t.Fatalf("a run an app session chose the route for read as the person's own (err %v)", err)
	}
	// With no choice on the run, the request's own pin keeps its own answer.
	hinted.ChoiceByMachine = true
	got, err = ApplyRunRouting(stepOf("reason", common.RouteChoice{}, nil), hinted)
	if err != nil || !got.ChoiceByMachine || got.PinnedBy != "alice" {
		t.Fatalf("an Auto run changed the turn's own pin: %+v (err %v)", got, err)
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

// credentialCtx is a call made as user through a credential of the given class
// ("" is a person's own signed-in session).
func credentialCtx(user, class string) context.Context {
	claims := map[string]any{"sub": user}
	if class != "" {
		claims["class"] = class
	}
	return auth.ContextWithAccess(auth.ContextWithClaims(context.Background(), claims), &auth.AccessContext{UserId: user, Role: auth.RoleDeveloper})
}

// EVERY STAMP OF A TURN'S CHOICE GOES THROUGH ONE FUNCTION, and that function
// records whose credential made it. An app session's subject is the person who
// owns the machine, so the choice names them either way; only the credential
// says whether they made it (vendor accounts design, K8 line 4, O4).
func TestTheChoiceATurnStampsSaysWhoseCredentialMadeIt(t *testing.T) {
	const ana = "v1:identity:user:ana"
	choice := common.RouteChoice{Source: "federation:strongest", By: ana}
	for name, tc := range map[string]struct {
		ctx  context.Context
		want bool
	}{
		"the person's own session":           {credentialCtx(ana, ""), false},
		"an app session on their machine":    {credentialCtx(ana, auth.ForwardedClassAppSession), true},
		"a service account":                  {credentialCtx(ana, auth.ForwardedClassServiceAccount), true},
		"server code acting on their behalf": {auth.ContextWithUserActor(context.Background(), ana), true},
	} {
		got := GoalRouteChoice(ContextWithGoalRouteChoice(tc.ctx, choice), ana)
		if got.Source != choice.Source || got.By != ana {
			t.Fatalf("%s: the stamp lost the choice: %+v", name, got)
		}
		if got.ByMachine != tc.want {
			t.Errorf("%s: ByMachine = %v, want %v", name, got.ByMachine, tc.want)
		}
	}
	// A caller that already knows the person did not choose says so, and a
	// person's session does not undo that: the product picked this route.
	kept := GoalRouteChoice(ContextWithGoalRouteChoice(credentialCtx(ana, ""), common.RouteChoice{Source: "route:apps", By: ana, ByMachine: true}), ana)
	if !kept.ByMachine {
		t.Error("a choice the caller marked as not the person's own was unmarked by their session")
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
	// The request is alice's own, in her own session: the stamp adds nothing.
	got := GoalRouteChoice(ContextWithGoalRouteChoice(credentialCtx("v1:identity:user:alice", ""), requested), "alice")
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
