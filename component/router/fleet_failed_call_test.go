package router

import (
	"context"
	"errors"
	"testing"

	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/core/common"
)

// failingFleetInference is a machine that starts every call and fails it.
type failingFleetInference struct {
	stubFleetInference
}

func (s *failingFleetInference) Call(context.Context, memql.FleetCallRequest) (memql.FleetCallResult, error) {
	s.calls++
	return memql.FleetCallResult{ExecutionSurface: "fleet:flaky", MachineLabel: "flaky"}, errors.New("the machine's runtime failed the call")
}

// ONE FAILED CALL IS ONE SAMPLE (memql#6414). A failure with a route behind it
// writes two rows: the observer's `error` and the walk's `fallback_used`, which
// exists to show the retry trail. FoldFleetHealth counts every row that names a
// machine and is not `ok`, so if both named it, the same call would count twice
// against the machine whenever a route had somewhere else to go.
func TestAFailedFleetCallWithARouteBehindItNamesItsMachineOnOneRow(t *testing.T) {
	cloud := &countingCloud{}
	providers := memql.NewProviderRegistryForTest()
	providers.SetFleetInference(&failingFleetInference{stubFleetInference{models: []memql.FleetModel{fleetModel("llama3.1:8b", true)}}})
	providers.RegisterForTest("streamClaudeSonnet", "AnthropicStream", "claude-sonnet", cloud)
	policies := memql.NewPolicyRegistryForTest(map[string][]string{"testPolicy": {"fleet:llama3.1:8b", "streamClaudeSonnet"}})
	ledger := &countingLedger{writes: make(chan string, 4)}
	r := New(providers, policies, testRules(t, defaultRule("testPolicy")), ledger, nil)

	client, _, err := r.ResolveChat(ResolveRequest{UserId: "alice"})
	if err != nil {
		t.Fatal(err)
	}
	// The background lane: the interactive budget is the rest of this package's.
	ctx := memql.ContextWithBackgroundLane(context.Background())
	answer, err := client.CallChat(ctx, []common.ChatMessage{{Role: "user", Content: "hi"}})
	if err != nil {
		t.Fatalf("the route's next source should have answered: %v", err)
	}
	if answer != "cloud answer" {
		t.Fatalf("answer = %q", answer)
	}

	for _, want := range []struct{ outcome, surface string }{
		{"error", "fleet:flaky"},
		{"fallback_used", ""},
		{"ok", ""},
	} {
		row := ledger.args(t)
		if row["outcome"] != want.outcome || row["executionSurface"] != want.surface {
			t.Fatalf("row outcome=%v executionSurface=%v, want %q on %q", row["outcome"], row["executionSurface"], want.outcome, want.surface)
		}
	}
}
