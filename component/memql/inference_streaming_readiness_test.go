package memql

import (
	"context"
	"errors"
	"testing"
)

func streamingReady(t *testing.T, r *ProviderRegistry, ctx context.Context) bool {
	t.Helper()
	rows, err := (&MemQLEngine{providers: r}).evaluateInferenceStatusExpression(ctx)
	if err != nil || len(rows) != 1 {
		t.Fatalf("inferenceStatus: %v, %v", rows, err)
	}
	status := decodePayload(t, rows[0].Payload)
	ready, ok := status["streamingChatEligible"].(bool)
	if !ok {
		t.Fatalf("streaming chat readiness missing from status: %v", status)
	}
	return ready
}

func TestInferenceStatusStreamingFleetCapability(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*FleetModel)
		want   bool
	}{
		{"plain chat without structured output", func(m *FleetModel) { m.StructuredOutput = false }, true},
		{"embedding only despite structured flag", func(m *FleetModel) { m.Embeddings = true }, false},
		{"offline", func(m *FleetModel) { m.Machines[0].Online = false }, false},
		{"below context floor", func(m *FleetModel) { m.ContextWindow = MinimumContextWindow - 1 }, false},
		{"at context floor", func(m *FleetModel) { m.ContextWindow = MinimumContextWindow }, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := capable("chat-model")
			tc.change(&m)
			r := newProviderRegistry()
			// BFF has the shared catalog, not a local worker dispatcher.
			r.SetFleetCatalog(&stubFleet{models: []FleetModel{m}})
			if got := streamingReady(t, r, userCtx("alice")); got != tc.want {
				t.Fatalf("streamingChatEligible = %v, want %v", got, tc.want)
			}
		})
	}
	for _, catalog := range []FleetCatalogReader{&stubFleet{}, streamingOwnerCatalog{err: errors.New("catalog unavailable")}} {
		r := newProviderRegistry()
		r.SetFleetCatalog(catalog)
		if streamingReady(t, r, userCtx("alice")) {
			t.Fatal("installing a catalog without an eligible model must not enable Ask")
		}
	}
}

// streamingOwnerCatalog is alice's machine, optionally lent to everyone. Lent
// to everyone, it is in EVERY catalog -- each person's and system work's --
// which is FleetCatalogReader's contract (design G8, memql#5660).
type streamingOwnerCatalog struct {
	shared bool
	err    error
}

func (c streamingOwnerCatalog) Catalog(_ context.Context, actor string) ([]FleetModel, error) {
	if c.err != nil {
		return nil, c.err
	}
	if actor == "alice" || c.shared {
		return []FleetModel{capable("alice-chat")}, nil
	}
	return nil, nil
}

func TestInferenceStatusStreamingOwnerVisibility(t *testing.T) {
	for _, shared := range []bool{false, true} {
		r := newProviderRegistry()
		r.SetFleetCatalog(streamingOwnerCatalog{shared: shared})
		for _, actor := range []string{"alice", "bob", ""} {
			if got, want := streamingReady(t, r, userCtx(actor)), actor == "alice" || shared; got != want {
				t.Errorf("actor=%q shared=%v: streamingChatEligible=%v, want %v", actor, shared, got, want)
			}
		}
	}
}

func TestInferenceStatusStreamingFederationRequiresAvailablePlainStream(t *testing.T) {
	for _, tc := range []struct {
		name                       string
		client                     AIProvider
		available, federated, want bool
	}{
		{"plain stream", &openAIStreamProvider{}, true, true, true},
		{"anthropic plain stream", &anthropicStreamProvider{}, true, true, true},
		{"nonstream chat", &openAIProvider{}, true, true, false},
		{"embedding only", &readinessEmbedOnly{}, true, true, false},
		{"unavailable stream", &openAIStreamProvider{}, false, true, false},
		{"unresolved stream", &openAIStreamProvider{}, true, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newProviderRegistry()
			entry := &ProviderConfigEntry{Config: ProviderConfig{Name: "vendor", Type: "openai"}, Client: tc.client, Available: tc.available}
			if tc.federated {
				entry.Config.Auth = map[string]string{authKeyIdentityProviderID: "test-provider", authKeyOpenAIServiceAccountID: "test-account", authKeyOpenAITokenFile: "/not-read-by-readiness"}
			}
			r.setEntry(entry)
			if got := streamingReady(t, r, userCtx("alice")); got != tc.want {
				t.Fatalf("streamingChatEligible=%v, want %v", got, tc.want)
			}
		})
	}
}

type readinessEmbedOnly struct{ stubChatOnly }

func (*readinessEmbedOnly) Embed(context.Context, string) ([]float32, error) {
	panic("readiness must not dispatch")
}

func TestInferenceStatusRunnableAppDoesNotPromiseUnsupportedStreaming(t *testing.T) {
	r := newProviderRegistry()
	apps := &stubApps{doors: []AppDoor{runnableDoor(appIdClaudeCode)}}
	r.SetAppInference(apps)
	if streamingReady(t, r, userCtx("alice")) {
		t.Fatal("appProvider supports completed chat, not plain streaming chat")
	}
	rows, _ := (&MemQLEngine{providers: r}).evaluateInferenceStatusExpression(userCtx("alice"))
	if decodePayload(t, rows[0].Payload)["appEligible"] != true || apps.lastActor != "alice" || apps.lastReq.AppId != "" {
		t.Fatal("readiness must preserve owner-scoped app availability without dispatch")
	}
}
