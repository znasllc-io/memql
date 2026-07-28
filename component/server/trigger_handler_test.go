package server

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/automations"
	"github.com/znasllc-io/memql/component/events"
)

type mockScheduler struct {
	exec *automations.AutomationExecution
	err  error
}

func (m *mockScheduler) TriggerAutomation(_ context.Context, _ string) (*automations.AutomationExecution, error) {
	return m.exec, m.err
}

func (m *mockScheduler) TriggerAutomationWithEvent(_ context.Context, _ string, _ *events.Event) (*automations.AutomationExecution, error) {
	return m.exec, m.err
}

func (m *mockScheduler) GetAutomations() []*automations.Automation {
	return nil
}

func (m *mockScheduler) ResumeAutomation(_ context.Context, _ string, _ string) (*automations.AutomationExecution, error) {
	return m.exec, m.err
}

type mockLoader struct {
	automation *automations.Automation
	err        error
}

func (m *mockLoader) LoadAll() ([]*automations.Automation, error) {
	return nil, m.err
}

func (m *mockLoader) LoadByName(_ string) (*automations.Automation, error) {
	if m.err != nil {
		return nil, m.err
	}
	return m.automation, nil
}

func TestPostAutomationTriggerSuccess(t *testing.T) {
	sched := &mockScheduler{
		exec: automations.NewExecution("demo", "manual"),
	}
	s := &Server{
		automationScheduler: sched,
	}

	resp, err := s.PostAutomationTrigger(ownerCtx(), PostAutomationTriggerRequestObject{AutomationName: "demo"})
	require.NoError(t, err)

	success, ok := resp.(PostAutomationTrigger202JSONResponse)
	require.True(t, ok)
	assert.Equal(t, sched.exec.ID, success.ExecutionId)
	assert.Equal(t, "queued", success.Status)
}

func TestPostAutomationTriggerDisabled(t *testing.T) {
	disabled := false
	sched := &mockScheduler{
		err: errors.New("not loaded"),
	}
	loader := &mockLoader{
		automation: &automations.Automation{
			Name:    "demo",
			Enabled: &disabled,
		},
	}
	s := &Server{
		automationScheduler: sched,
		automationLoader:    loader,
	}

	resp, err := s.PostAutomationTrigger(ownerCtx(), PostAutomationTriggerRequestObject{AutomationName: "demo"})
	require.NoError(t, err)

	disabledResp, ok := resp.(PostAutomationTrigger409JSONResponse)
	require.True(t, ok)
	assert.Contains(t, disabledResp.Error, "disabled")
}

func TestPostAutomationTriggerNotFound(t *testing.T) {
	// Scheduler returns nil execution and error = not found
	sched := &mockScheduler{
		exec: nil,
		err:  errors.New("missing"),
	}
	// Loader also returns not found
	loader := &mockLoader{
		err: errors.New("not found"),
	}
	s := &Server{
		automationScheduler: sched,
		automationLoader:    loader,
	}

	resp, err := s.PostAutomationTrigger(ownerCtx(), PostAutomationTriggerRequestObject{AutomationName: "missing"})
	require.NoError(t, err)

	notFound, ok := resp.(PostAutomationTrigger404JSONResponse)
	require.True(t, ok)
	assert.Contains(t, notFound.Error, "not found")
}

func TestPostAutomationTriggerExecutionFailedReturns202(t *testing.T) {
	// The automation is found and triggered, but execution fails
	// This should return 202 (triggered) not 404 (not found)
	enabledTrue := true
	exec := automations.NewExecution("demo", "manual")
	sched := &mockScheduler{
		exec: exec,
		err:  errors.New("step execution failed: AI validation error"),
	}
	loader := &mockLoader{
		automation: &automations.Automation{
			Name:    "demo",
			Enabled: &enabledTrue,
		},
	}
	s := &Server{
		automationScheduler: sched,
		automationLoader:    loader,
	}

	resp, err := s.PostAutomationTrigger(ownerCtx(), PostAutomationTriggerRequestObject{AutomationName: "demo"})
	require.NoError(t, err)

	// Should still return 202 because the automation was found and triggered
	// even though execution had an error
	success, ok := resp.(PostAutomationTrigger202JSONResponse)
	require.True(t, ok, "expected 202 response when automation is found but execution fails, got %T", resp)
	assert.Equal(t, exec.ID, success.ExecutionId)
	assert.Equal(t, "queued", success.Status)
}

func TestPostAutomationTriggerMissingName(t *testing.T) {
	s := &Server{}

	resp, err := s.PostAutomationTrigger(ownerCtx(), PostAutomationTriggerRequestObject{})
	require.NoError(t, err)

	notFound, ok := resp.(PostAutomationTrigger404JSONResponse)
	require.True(t, ok)
	assert.Contains(t, notFound.Error, "required")
}

func TestExtractAutomationNameFromPath(t *testing.T) {
	tests := []struct {
		name   string
		path   string
		prefix string
		suffix string
		want   string
	}{
		{
			name:   "valid path with simple name",
			path:   "/automations/demo/trigger",
			prefix: "/automations/",
			suffix: "/trigger",
			want:   "demo",
		},
		{
			name:   "valid path with hyphenated name",
			path:   "/automations/example-email-query-automation/trigger",
			prefix: "/automations/",
			suffix: "/trigger",
			want:   "example-email-query-automation",
		},
		{
			name:   "missing prefix",
			path:   "/other/demo/trigger",
			prefix: "/automations/",
			suffix: "/trigger",
			want:   "",
		},
		{
			name:   "missing suffix",
			path:   "/automations/demo/action",
			prefix: "/automations/",
			suffix: "/trigger",
			want:   "",
		},
		{
			name:   "empty name",
			path:   "/automations//trigger",
			prefix: "/automations/",
			suffix: "/trigger",
			want:   "",
		},
		{
			name:   "name with slash",
			path:   "/automations/foo/bar/trigger",
			prefix: "/automations/",
			suffix: "/trigger",
			want:   "",
		},
		{
			name:   "with base path",
			path:   "/memql/automations/demo/trigger",
			prefix: "/memql/automations/",
			suffix: "/trigger",
			want:   "demo",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := extractAutomationNameFromPath(tt.path, tt.prefix, tt.suffix)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestAutomationTriggerRouteIntegration(t *testing.T) {
	// Create a mock scheduler that succeeds
	sched := &mockScheduler{
		exec: automations.NewExecution("example-email-query-automation", "manual"),
	}

	// Create the server with the mock scheduler
	s := &Server{
		automationScheduler: sched,
	}

	// Build the strict handler
	strictHandler := NewStrictHandler(s, nil)

	// Build the HTTP handler with routes
	handler := HandlerWithOptions(strictHandler, StdHTTPServerOptions{
		BaseURL: "", // Empty = root
	})

	// Two servers: one that injects an authorized caller the way the verifier
	// middleware would, and one raw (no credential at all).
	//
	// The raw one is the end-to-end proof of memql#2937: these routes are
	// mounted on the identity binary, which installs NO verifier middleware and
	// is the publicly fronted listener, so "no credential" is a real wire state
	// and not a hypothetical.
	authed := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handler.ServeHTTP(w, r.WithContext(ownerCtx()))
	}))
	defer authed.Close()

	ts := authed

	rawTS := httptest.NewServer(handler)
	defer rawTS.Close()

	t.Run("unauthenticated request is refused (memql#2937)", func(t *testing.T) {
		req, err := http.NewRequest("POST", rawTS.URL+"/automations/demo/trigger", nil)
		require.NoError(t, err)
		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		assert.Equal(t, http.StatusForbidden, resp.StatusCode,
			"a request with no credential must be refused; triggering runs an automation's "+
				"action chain server-side. got %d: %s", resp.StatusCode, string(body))
	})

	// Test POST /automations/{name}/trigger
	t.Run("POST /automations/example-email-query-automation/trigger", func(t *testing.T) {
		req, err := http.NewRequest("POST", ts.URL+"/automations/example-email-query-automation/trigger", nil)
		require.NoError(t, err)

		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		defer resp.Body.Close()

		body, _ := io.ReadAll(resp.Body)
		t.Logf("Response status: %d, body: %s", resp.StatusCode, string(body))

		assert.Equal(t, http.StatusAccepted, resp.StatusCode, "Expected 202 Accepted, got %d: %s", resp.StatusCode, string(body))
	})

	// Test with a simple name
	t.Run("POST /automations/demo/trigger", func(t *testing.T) {
		sched.exec = automations.NewExecution("demo", "manual")

		req, err := http.NewRequest("POST", ts.URL+"/automations/demo/trigger", nil)
		require.NoError(t, err)

		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		defer resp.Body.Close()

		body, _ := io.ReadAll(resp.Body)
		t.Logf("Response status: %d, body: %s", resp.StatusCode, string(body))

		assert.Equal(t, http.StatusAccepted, resp.StatusCode, "Expected 202 Accepted, got %d: %s", resp.StatusCode, string(body))
	})
}

// ownerCtx is an authorized caller in the shape the real HTTP middleware
// produces -- CLAIMS, not a pre-resolved AccessContext (memql#2937).
//
// These tests predate the authorization gate and asserted the endpoint's
// behaviour for an ANONYMOUS caller. Triggering runs an automation's action
// chain server-side, so it is now owner-or-admin only; the cases below are
// about trigger's own semantics (found / disabled / missing name), which is
// what they were always testing. The refusal itself is covered by
// automation_resume_authz_test.go.
func ownerCtx() context.Context {
	return auth.ContextWithClaims(context.Background(), map[string]any{
		"sub":  "v1:identity:user:trigger-test",
		"role": string(auth.RoleOwner),
	})
}
