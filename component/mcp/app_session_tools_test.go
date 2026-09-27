package mcp

// The app-session back-channel, gated on the CREDENTIAL (epic memql#5096,
// task memql#5102, design D7).

import (
	"context"
	"strings"
	"testing"
)

func appSessionClaims(sessionId string) map[string]any {
	return map[string]any{"class": "app_session", "node_id": "app-session:" + sessionId, "sub": "alice"}
}

func TestOnlyAnAppSessionCredentialNamesASession(t *testing.T) {
	for _, tt := range []struct {
		name   string
		claims map[string]any
		want   string
	}{
		{"an app-session bearer", appSessionClaims("s1"), "s1"},
		{"no claims at all", nil, ""},
		{"a browser session", map[string]any{"sub": "alice", "role": "owner"}, ""},
		// A service account carrying the same label shape is still NOT an app
		// session. The class is what says whose subject the token names, and
		// it is the whole reason app_session exists as its own class
		// (memql#4857).
		{"a service account with a lookalike label",
			map[string]any{"class": "service_account", "node_id": "app-session:s1"}, ""},
		// And an app-session class whose label is some OTHER shape names no
		// session. Treating the whole label as a session id would send a write
		// at a row id somebody else chose.
		{"an app-session class with an unrelated label",
			map[string]any{"class": "app_session", "node_id": "deploy-gate-staging"}, ""},
		{"an app-session class with an empty session id",
			map[string]any{"class": "app_session", "node_id": "app-session:"}, ""},
	} {
		if got := AppSessionFromClaims(tt.claims); got != tt.want {
			t.Errorf("%s: AppSessionFromClaims = %q, want %q", tt.name, got, tt.want)
		}
	}
}

// THE LIST IS THE GATE. tools/list is what a model reads to decide what it can
// do, so offering `submit` to a browser session offers an action that can only
// fail -- and a model shown a tool it cannot use spends turns discovering that.
func TestTheBackChannelToolsAreListedOnlyForAnAppSession(t *testing.T) {
	eng := newFakeEngine()

	browser := toolNames(listMCPTools(asPerson("owner"), eng, "owner", TierInline, ""))
	for _, name := range []string{toolSubmit, toolNextTask} {
		if browser[name] {
			t.Errorf("%q must not be listed for a session that is not an app: it can only fail", name)
		}
	}

	app := toolNames(listMCPTools(asPerson("owner"), eng, "owner", TierInline, "s1"))
	for _, name := range []string{toolSubmit, toolNextTask} {
		if !app[name] {
			t.Errorf("%q must be listed for an app-session bearer; got %v", name, app)
		}
	}

	// The tools take NO session argument. An app cannot name somebody else's
	// session even by accident, because the surface does not offer the
	// question.
	for _, def := range appSessionToolDefs() {
		schema, _ := def["inputSchema"].(map[string]any)
		props, _ := schema["properties"].(map[string]any)
		for key := range props {
			if strings.Contains(strings.ToLower(key), "session") {
				t.Errorf("%v declares the argument %q; the session comes from the credential",
					def["name"], key)
			}
		}
	}
}

// The handler refuses too. The list gate is the mechanism; this is the
// backstop, and it says WHY rather than failing generically -- a browser
// session reaching here is a gating bug, and the message is what tells the
// reader that.
func TestTheBackChannelHandlersRefuseANonAppCaller(t *testing.T) {
	eng := newFakeEngine()
	for _, name := range []string{toolSubmit, toolNextTask} {
		res := callMCPTool(context.Background(), eng, "owner", TierInline, "", name,
			map[string]any{"result": map[string]any{"ok": true}})
		if !isError(res) {
			t.Fatalf("%s with no app session must be refused, got %v", name, res)
		}
		if !strings.Contains(resultText(res), "delegated app session") {
			t.Errorf("%s refusal must name the reason: %q", name, resultText(res))
		}
	}
}

func TestSubmitWritesTheResultOntoTheCallersOwnSession(t *testing.T) {
	eng := newFakeEngine()
	res := callMCPTool(context.Background(), eng, "owner", TierInline, "s1", toolSubmit,
		map[string]any{"result": map[string]any{"summary": "done", "files": 2}})
	if isError(res) {
		t.Fatalf("submit: %v", resultText(res))
	}
	call := eng.query
	if call == "" {
		t.Fatal("submit must reach the engine")
	}
	if !strings.HasPrefix(call, "submitAppSessionResult(") {
		t.Fatalf("call = %q", call)
	}
	// THE SESSION ID COMES FROM THE CREDENTIAL. Asserted on the rendered call
	// rather than trusted, because this is the one argument an app must never
	// be able to choose.
	if !strings.Contains(call, `sessionId: "s1"`) {
		t.Errorf("the call must name the credential's session: %q", call)
	}
	if !strings.Contains(call, `"summary"`) {
		t.Errorf("the result must reach the mutation: %q", call)
	}
	if !strings.Contains(call, "submittedAt:") {
		t.Errorf("the write must stamp a server timestamp: %q", call)
	}
}

func TestSubmitRefusesAnEmptyResult(t *testing.T) {
	eng := newFakeEngine()
	for _, args := range []map[string]any{
		{},
		{"result": map[string]any{}},
		{"result": "not an object"},
	} {
		res := callMCPTool(context.Background(), eng, "owner", TierInline, "s1", toolSubmit, args)
		if !isError(res) {
			t.Errorf("submit(%v) must be refused: an empty answer recorded as an answer is worse "+
				"than none, because the run then reads as having produced one", args)
		}
	}
	if eng.query != "" {
		t.Errorf("a refused submit must write nothing, got %q", eng.query)
	}
}

func TestNextTaskReadsTheCallersOwnSession(t *testing.T) {
	eng := newFakeEngine()
	res := callMCPTool(context.Background(), eng, "owner", TierInline, "s1", toolNextTask, nil)
	if isError(res) {
		t.Fatalf("next_task: %v", resultText(res))
	}
	if !strings.HasPrefix(eng.query, "appSessionById(") {
		t.Fatalf("next_task must read through the caller-scoped query, got %q", eng.query)
	}
	if !strings.Contains(eng.query, `sessionId: "s1"`) {
		t.Errorf("the read must name the credential's session: %q", eng.query)
	}
}
