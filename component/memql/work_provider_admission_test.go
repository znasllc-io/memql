package memql

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/znasllc-io/memql/core/common"
)

type admissionFixture struct {
	calls   int
	policy  ProviderAdmission
	refusal error
}

// admittedWorkFixture wires both work guards for engine tests that exercise a
// real fleet provider with a goal-backed run. Keeping admission observable lets
// replay tests prove that a journal answer reserves no new inference attempt.
// Tests of missing wiring still use an unwired engine and must fail closed.
type admittedWorkFixture struct {
	fakeGuard
	admissionFixture
}

type admissionTransport struct{ calls int }

func (t *admissionTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	t.calls++
	return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{}`)), Request: req}, nil
}

func TestAuxiliaryHTTPInferenceReservesBeforeDispatchWithTypedRefusal(t *testing.T) {
	g := newLLMGuardFromEnv()
	g.enabled, g.rateEnabled, g.bgRateEnabled, g.scopeEnabled, g.killSwitchEnabled = false, false, false, false, false
	fixture := &admissionFixture{}
	ctx := context.WithValue(common.ContextWithRun(context.Background(), aRun()), workProviderGuardKey{}, fixture)
	base := &admissionTransport{}
	transport := &guardedTransport{base: base, guard: g}
	for _, path := range []string{"/v1/embeddings", "/v1/audio/speech", "/v1/images/generations"} {
		req, err := http.NewRequestWithContext(ctx, "POST", "https://inference.invalid"+path, strings.NewReader(`{"input":"A museum acquisition record"}`))
		require.NoError(t, err)
		response, err := transport.RoundTrip(req)
		require.NoError(t, err)
		response.Body.Close()
		require.Positive(t, fixture.policy.EstimatedTokens)
	}
	require.Equal(t, 3, fixture.calls)
	require.Equal(t, 3, base.calls)
	proof := &RunCeilingError{Breach: RunCeilingBreach{Ceiling: "modelCalls"}}
	fixture.refusal = proof
	req, _ := http.NewRequestWithContext(ctx, "POST", "https://inference.invalid/v1/embeddings", strings.NewReader(`{"input":"new"}`))
	_, err := transport.RoundTrip(req)
	require.Same(t, proof, err)
	require.Equal(t, 3, base.calls, "a refusal must precede provider dispatch")
}

func (f *admissionFixture) AdmitProvider(_ context.Context, _ common.RunContext, policy ProviderAdmission) error {
	f.policy = policy
	if f.refusal != nil {
		return f.refusal
	}
	f.calls++
	return nil
}

func TestGovernedProviderWorkUsesDurableWindowsWithoutResettingLowerUsage(t *testing.T) {
	t.Setenv("MEMQL_LLM_SCOPE_MAX_CALLS", "")
	g := newLLMGuardFromEnv()
	g.enabled, g.rateEnabled, g.bgRateEnabled = false, false, false
	g.now = time.Now
	fixture := &admissionFixture{}
	ctx := ContextWithBudgetScope(common.ContextWithRun(context.Background(), aRun()), "run:test", "goal:test")
	ctx = context.WithValue(ctx, workProviderGuardKey{}, fixture)
	for n := 0; n < 800; n++ {
		require.NoError(t, g.admitLocalCall(ctx, ""))
	}
	require.Equal(t, 800, fixture.calls)
	require.Equal(t, int64(800), g.scopes["run:test"].calls)
	require.Zero(t, fixture.policy.HardCallLimit)
	fixture.refusal = &RunCeilingError{Breach: RunCeilingBreach{Ceiling: "modelCalls"}}
	var refused *RunCeilingError
	require.ErrorAs(t, g.admitLocalCall(ctx, ""), &refused)
	require.Equal(t, int64(800), g.scopes["run:test"].calls)
	// Losing the native guard never retains its exception to the default cap.
	plain := ContextWithBudgetScope(context.Background(), "run:test")
	require.Error(t, g.admitLocalCall(plain, ""))
}

func TestExplicitProviderLimitsAndDisabledLegacyGuards(t *testing.T) {
	t.Setenv("MEMQL_LLM_SCOPE_MAX_CALLS", "600")
	g := newLLMGuardFromEnv()
	require.True(t, g.scopeCallsExplicit, "an explicit value equal to the default is still a hard operator limit")
	fixture := &admissionFixture{}
	ctx := context.WithValue(common.ContextWithRun(context.Background(), aRun()), workProviderGuardKey{}, fixture)
	_, err := g.admitWorkProvider(ctx, 0)
	require.NoError(t, err)
	require.Equal(t, 600, fixture.policy.HardCallLimit)
	g.enabled, g.rateEnabled, g.bgRateEnabled, g.scopeEnabled, g.killSwitchEnabled = false, false, false, false, false
	require.NoError(t, g.admitLocalCall(ctx, ""))
	require.Equal(t, 2, fixture.calls, "disabling process guards cannot disable the owned work reservation")
	_, err = g.admitWorkProvider(common.ContextWithRun(context.Background(), aRun()), 0)
	var unwired *RunCeilingError
	require.ErrorAs(t, err, &unwired)
	require.Equal(t, "unevaluated", unwired.Breach.Ceiling)
	proof := &RunCeilingError{Breach: RunCeilingBreach{Ceiling: "cost"}}
	require.Same(t, proof, sanitizeEmbeddingCallError(fmt.Errorf("transport: %w", proof)))
	require.False(t, errors.Is(sanitizeEmbeddingCallError(errors.New("vendor secret")), proof))
}
