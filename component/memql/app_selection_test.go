package memql

import (
	"context"
	"errors"
	"fmt"
	"github.com/znasllc-io/memql/core/common"
	"reflect"
	"testing"
)

type selectingApps struct {
	stubApps
	calls     []AppCallRequest
	uncertain bool
	// refuse answers the first call to each app; nil is the weekly limit.
	refuse map[string]error
}

func (s *selectingApps) SelectApp(_ context.Context, req AppCallRequest, _ bool, excluded []string) (AppSelection, error) {
	for _, app := range []string{"claude-code", "codex"} {
		if !AppSelectionExcluded(excluded, app, "laptop") && (req.AppId == "*" || req.AppId == app) {
			return AppSelection{app, "laptop"}, nil
		}
	}
	return AppSelection{}, ErrAppUnavailable
}
func (s *selectingApps) Call(_ context.Context, req AppCallRequest) (AppCallResult, error) {
	s.calls = append(s.calls, req)
	if refusal, ok := s.refuse[req.AppId]; ok {
		return AppCallResult{}, refusal
	}
	if req.AppId == "claude-code" {
		if s.uncertain {
			return AppCallResult{}, errors.New("connection lost after a tool ran")
		}
		return AppCallResult{}, &AppQuotaRefused{Reason: "You've hit your weekly limit"}
	}
	return AppCallResult{Content: "done", ExecutionSurface: "app:codex@laptop"}, nil
}

func TestAppFallbackPreservesContextAndNeverReplaysUncertainEffects(t *testing.T) {
	// Earlier app tests can exhaust the shared guard; this test asserts fallback,
	// context preservation and uncertain-effect handling, not rate limits.
	offTheSharedRateCeiling(t)
	for _, uncertain := range []bool{false, true} {
		t.Run(map[bool]string{false: "quota", true: "uncertain"}[uncertain], func(t *testing.T) {
			a := &selectingApps{uncertain: uncertain}
			r := &ProviderRegistry{}
			r.SetAppInference(a)
			p := &appProvider{registry: r, appId: "*", wildcard: true, actingUserId: "alice", level: "reasoning"}
			messages := []common.ChatMessage{{Role: "system", Content: "Keep constraints"}, {Role: "user", Content: "Initial task"}, {Role: "assistant", Content: "Earlier result"}, {Role: "user", Content: "Correction: use cents"}}
			req := AppCallRequest{Messages: messages, Schema: &common.StructuredSchema{Name: "answer"}, Inputs: []string{"file-a"}, RunId: "run", StepId: "step"}
			_, err := p.call(context.Background(), req)
			if uncertain {
				if err == nil || len(a.calls) != 1 {
					t.Fatalf("replayed uncertain session: %v %+v", err, a.calls)
				}
				return
			}
			if err != nil || len(a.calls) != 2 {
				t.Fatalf("no fallback: %v %+v", err, a.calls)
			}
			first, second := a.calls[0], a.calls[1]
			first.AppId, first.RegistrationId = second.AppId, second.RegistrationId
			if !reflect.DeepEqual(first, second) || !reflect.DeepEqual(second.Messages, messages) {
				t.Fatal("fallback lost context, artifacts, schema or attribution")
			}
		})
	}
}

func TestAppAutoUsesObservedUsageSpeedAndReliability(t *testing.T) {
	light := appSignal{tokens: 100, duration: 1000, samples: 1, successes: 1, usageSamples: 1}
	heavy := appSignal{tokens: 1000, duration: 100, samples: 1, successes: 1, usageSamples: 1}
	if appScore(AppRoutingPolicy{UsageWeight: 100}, light, 1000, 100) <= appScore(AppRoutingPolicy{UsageWeight: 100}, heavy, 1000, 100) {
		t.Fatal("usage balance ignored")
	}
	if appScore(AppRoutingPolicy{SpeedWeight: 100}, light, 1000, 100) >= appScore(AppRoutingPolicy{SpeedWeight: 100}, heavy, 1000, 100) {
		t.Fatal("speed balance ignored")
	}
	if got := appScore(AppRoutingPolicy{UsageWeight: 100}, appSignal{}, 1000, 100); got != 50 {
		t.Fatalf("unknown usage must be neutral, got %v", got)
	}
	heavy.successes = 0
	if appScore(AppRoutingPolicy{ReliabilityWeight: 100}, light, 1000, 100) <= appScore(AppRoutingPolicy{ReliabilityWeight: 100}, heavy, 1000, 100) {
		t.Fatal("reliability ignored")
	}

}

type quotaSessionDelegate struct {
	calls     []AppSessionHandover
	uncertain bool
}

func (d *quotaSessionDelegate) RunStep(_ context.Context, h AppSessionHandover) (AppSessionOutcome, error) {
	d.calls = append(d.calls, h)
	if len(d.calls) == 1 {
		if d.uncertain {
			return AppSessionOutcome{}, errors.New("connection lost after writing a file")
		}
		return AppSessionOutcome{}, &AppQuotaRefused{Reason: "You've hit your weekly limit"}
	}
	return AppSessionOutcome{Content: "done", Model: "reported-model"}, nil
}
func TestSessionProviderQuotaFallbackKeepsStepContract(t *testing.T) {
	for _, uncertain := range []bool{false, true} {
		d := &quotaSessionDelegate{uncertain: uncertain}
		r := sessionRegistry(d)
		r.SetAppInference(&selectingApps{})
		p := r.SessionProvider("*", "", SessionRequest{ActingUserId: "alice", RunId: "run", StepId: "step", Level: "reasoning"}).(*sessionProvider)
		messages := []common.ChatMessage{{Role: "system", Content: "Keep constraints"}, {Role: "user", Content: "Initial task"}, {Role: "assistant", Content: "Earlier result"}, {Role: "user", Content: "Correction: use cents"}}
		_, err := p.run(delegatedProviderTestContext("alice", "run", "step"), messages, []common.ToolDefinition{{Name: "tool"}}, &common.StructuredSchema{Name: "answer"})
		if uncertain {
			if err == nil || len(d.calls) != 1 {
				t.Fatalf("replayed uncertain step: %v", err)
			}
			continue
		}
		if err != nil || len(d.calls) != 2 {
			t.Fatalf("fallback: %v %+v", err, d.calls)
		}
		a, b := d.calls[0], d.calls[1]
		a.AppId, a.RegistrationId = b.AppId, b.RegistrationId
		if !reflect.DeepEqual(a, b) || b.Prompt != flattenForSession(messages) {
			t.Fatal("step contract changed across apps")
		}
	}
}

func TestAppAuthFallbackMovesToTheNextAppWithTheSameContext(t *testing.T) {
	offTheSharedRateCeiling(t)
	a := &selectingApps{refuse: map[string]error{"claude-code": &AppAuthRefused{Reason: "Failed to authenticate: OAuth session expired and could not be refreshed"}}}
	r := &ProviderRegistry{}
	r.SetAppInference(a)
	p := &appProvider{registry: r, appId: "*", wildcard: true, actingUserId: "alice", level: "reasoning"}
	res, err := p.call(context.Background(), AppCallRequest{Messages: []common.ChatMessage{{Role: "user", Content: "Initial task"}}})
	if err != nil || res.Content != "done" {
		t.Fatalf("a signed-out app did not fail over: %v", err)
	}
	if len(a.calls) != 2 || a.calls[0].AppId != "claude-code" || a.calls[1].AppId != "codex" {
		t.Fatalf("wrong order: %+v", a.calls)
	}
}

// When every app refuses, the person reads WHY: the last refusal, not the
// selector's "nothing else is available", which would send them to Fleet to
// look for a setting that is not the problem.
func TestAppFallbackThatRunsOutKeepsTheLastRefusal(t *testing.T) {
	offTheSharedRateCeiling(t)
	refused := &AppAuthRefused{Reason: "Failed to authenticate: OAuth session expired and could not be refreshed"}
	a := &selectingApps{refuse: map[string]error{"claude-code": &AppQuotaRefused{Reason: "usage limit"}, "codex": refused}}
	r := &ProviderRegistry{}
	r.SetAppInference(a)
	p := &appProvider{registry: r, appId: "*", wildcard: true, actingUserId: "alice"}
	_, err := p.call(context.Background(), AppCallRequest{Messages: []common.ChatMessage{{Role: "user", Content: "Initial task"}}})
	var auth *AppAuthRefused
	if !errors.As(err, &auth) || !errors.Is(err, ErrAppUnavailable) {
		t.Fatalf("lost the reason: %v", err)
	}
}

// A pin names an app, not a machine: another machine with that app may answer,
// and no other app ever does.
func TestPinnedAppNeverMovesToAnotherApp(t *testing.T) {
	offTheSharedRateCeiling(t)
	a := &selectingApps{refuse: map[string]error{"claude-code": &AppAuthRefused{Reason: "Not logged in"}}}
	r := &ProviderRegistry{}
	r.SetAppInference(a)
	p := &appProvider{registry: r, appId: "claude-code", actingUserId: "alice"}
	_, err := p.call(context.Background(), AppCallRequest{Messages: []common.ChatMessage{{Role: "user", Content: "Initial task"}}})
	var auth *AppAuthRefused
	if !errors.As(err, &auth) || len(a.calls) != 1 || a.calls[0].AppId != "claude-code" {
		t.Fatalf("pin crossed apps: %v %+v", err, a.calls)
	}
}

// A login is one machine's and a usage limit is the account's, so the two rule
// out different things for the rest of a call.
func TestAppRefusalExclusionMatchesWhatTheRefusalIsAbout(t *testing.T) {
	picked := AppSelection{AppId: "claude-code", RegistrationId: "black"}
	quota, ok := appRefusalExclusion(&AppQuotaRefused{Reason: "usage limit"}, picked)
	if !ok || !AppSelectionExcluded([]string{quota}, "claude-code", "gray") {
		t.Fatalf("a usage limit must rule the app out everywhere: %q", quota)
	}
	signedOut, ok := appRefusalExclusion(fmt.Errorf("wrapped: %w", &AppAuthRefused{Reason: "Not logged in"}), picked)
	if !ok || !AppSelectionExcluded([]string{signedOut}, "claude-code", "black") || AppSelectionExcluded([]string{signedOut}, "claude-code", "gray") || AppSelectionExcluded([]string{signedOut}, "codex", "black") {
		t.Fatalf("a login must rule out that machine's app alone: %q", signedOut)
	}
	if _, ok := appRefusalExclusion(errors.New("connection lost after a tool ran"), picked); ok {
		t.Fatal("an uncertain failure may never move")
	}
}

func TestSessionProviderAuthFallbackKeepsStepContract(t *testing.T) {
	d := &refusingSessionDelegate{refusal: &AppAuthRefused{Reason: "Not logged in"}}
	r := sessionRegistry(d)
	r.SetAppInference(&selectingApps{})
	p := r.SessionProvider("*", "", SessionRequest{ActingUserId: "alice", RunId: "run", StepId: "step", Level: "reasoning"}).(*sessionProvider)
	messages := []common.ChatMessage{{Role: "user", Content: "Initial task"}}
	if _, err := p.run(delegatedProviderTestContext("alice", "run", "step"), messages, []common.ToolDefinition{{Name: "tool"}}, nil); err != nil {
		t.Fatalf("no fallback: %v", err)
	}
	if len(d.calls) != 2 || d.calls[0].AppId != "claude-code" || d.calls[1].AppId != "codex" || d.calls[0].Prompt != d.calls[1].Prompt {
		t.Fatalf("step contract changed: %+v", d.calls)
	}
}

type refusingSessionDelegate struct {
	calls   []AppSessionHandover
	refusal error
}

func (d *refusingSessionDelegate) RunStep(_ context.Context, h AppSessionHandover) (AppSessionOutcome, error) {
	d.calls = append(d.calls, h)
	if len(d.calls) == 1 {
		return AppSessionOutcome{}, d.refusal
	}
	return AppSessionOutcome{Content: "done", Model: "reported-model"}, nil
}

// The sentences each app actually printed, measured on 2026-10-29 with Claude
// Code 2.1.304 and codex-cli 0.162.1 against fabricated expired logins and
// empty homes. A phrase list that drifts from them fails here, not on the
// owner's machine.
func TestIsAppAuthRefusalNamesTheMeasuredSentences(t *testing.T) {
	for _, message := range []string{
		"harness: the claude turn exited 1: Failed to authenticate: OAuth session expired and could not be refreshed",
		"harness: the claude turn exited 1: Not logged in \u00b7 Please run /login",
		"codex app-server: the turn failed: unexpected status 401 Unauthorized: Missing bearer or basic authentication in header, url: https://api.openai.com/v1/responses, cf-ray: x",
		"codex app-server: the turn failed: workspace routing discovery unauthorized (401)",
		"Your access token could not be refreshed. Please log out and sign in again.",
		"codex account authentication required to read rate limits",
	} {
		if !IsAppAuthRefusal(message) {
			t.Errorf("not classified: %s", message)
		}
		if IsAppQuotaRefusal(message) {
			t.Errorf("read as a usage limit: %s", message)
		}
	}
	for _, message := range []string{
		"connection lost",
		"mkdir /home/znas/.local/share/memql: read-only file system",
		"You've hit your weekly limit",
		"harness: Claude Code reported MemQL's MCP server \"memql\" as needs-auth when the turn started (it did not accept the session's credential), so the turn was stopped rather than run without MemQL's tools",
		"",
	} {
		if IsAppAuthRefusal(message) {
			t.Errorf("wrongly classified as signed out: %s", message)
		}
	}
}
