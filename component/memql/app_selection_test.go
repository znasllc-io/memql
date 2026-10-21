package memql

import (
	"context"
	"errors"
	"github.com/znasllc-io/memql/core/common"
	"reflect"
	"testing"
	"time"
)

type selectingApps struct {
	stubApps
	calls     []AppCallRequest
	uncertain bool
}

func (s *selectingApps) SelectApp(_ context.Context, req AppCallRequest, _ bool, excluded []string) (AppSelection, error) {
	for _, app := range []string{"claude-code", "codex"} {
		found := false
		for _, x := range excluded {
			if x == app {
				found = true
			}
		}
		if !found {
			return AppSelection{app, "laptop"}, nil
		}
	}
	return AppSelection{}, ErrAppUnavailable
}
func (s *selectingApps) Call(_ context.Context, req AppCallRequest) (AppCallResult, error) {
	s.calls = append(s.calls, req)
	if req.AppId == "claude-code" {
		if s.uncertain {
			return AppCallResult{}, errors.New("connection lost after a tool ran")
		}
		return AppCallResult{}, &AppQuotaRefused{Reason: "You've hit your weekly limit"}
	}
	return AppCallResult{Content: "done", ExecutionSurface: "app:codex@laptop"}, nil
}

func TestAppFallbackPreservesContextAndNeverReplaysUncertainEffects(t *testing.T) {
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
	now := time.Now()
	rows := []map[string]any{{"app": "claude-code", "status": "failed", "endedAt": now.Format(time.RFC3339Nano), "errorMessage": "You've hit your weekly limit", "usage": map[string]any{"known": true, "inputTokens": float64(0), "outputTokens": float64(0)}}}
	if !appSignals(rows, now)["claude-code"].blockedUntil.After(now) {
		t.Fatal("quota refusal not suppressed")
	}
	if !appSignals(rows, now.Add(3*time.Hour))["claude-code"].blockedUntil.IsZero() {
		t.Fatal("old evidence blocked recovery")
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
		_, err := p.run(context.Background(), messages, []common.ToolDefinition{{Name: "tool"}}, &common.StructuredSchema{Name: "answer"})
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
