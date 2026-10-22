package mcp

// The app-session back-channel tools (epic memql#5096, task memql#5102,
// design D7 and D1).
//
// A delegated app reaches MemQL over MCP with a per-run credential whose
// `sub` is the OWNING USER's id and whose label names the session. These two
// tools are what that credential can do BEYOND reading the user's graph:
//
//	submit    hand back the structured answer for this session
//	nextTask  ask what this session was opened to do
//
// THEY ARE LISTED ONLY FOR AN APP-SESSION BEARER, and that gating is not
// cosmetic. `tools/list` is what a model reads to decide what it can do, so a
// browser session offered `submit` would be offered an action that can only
// fail -- and a model shown a tool it cannot use spends turns discovering
// that. The refusal in the handler is the backstop; the absence from the list
// is the mechanism.
//
// THE SESSION ID COMES FROM THE CREDENTIAL, NEVER FROM THE CALL. Neither tool
// takes a session argument, so an app cannot name somebody else's session even
// by accident. Row authz would refuse such a write anyway -- the app acts as
// its owner, and the session row is owner-tiered -- but a surface that does not
// offer the argument cannot be asked the question.
//
// WHY NOT A CROSS-NODE FORWARD. The session runs on an agent replica and this
// is the MCP node; the row is the state both can see. `submit` writes the
// answer onto the row and the runner reads it back at end, which needs no
// forward and works when the two nodes never speak. The alternative would be a
// NodeService hop whose failure mode is an app that submitted an answer nobody
// received.

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/znasllc-io/memql/component/memql"
)

const (
	toolSubmit   = "submit"
	toolNextTask = "next_task"
)

// appSessionLabelPrefix is how an app-session credential's label names its
// session. It mirrors the mint side (component/identity/http's
// mintAppSessionCredential writes `app-session:<sessionId>`), and the prefix
// is checked rather than assumed: a class=app_session token whose label is not
// this shape names no session, and treating its whole label as one would send
// a write at a row id somebody chose.
const appSessionLabelPrefix = "app-session:"

// appSessionClass is the JWT class an app-session credential carries. It is
// NOT service_account, and the distinction is the whole reason the class
// exists (memql#4857): this is the one machine class whose subject is a
// person.
const appSessionClass = "app_session"

// AppSessionFromClaims returns the session id an app-session bearer names, or
// "" for any other credential.
//
// Both halves are required. The class alone would admit a future app-session
// token minted for something other than a run; the label alone would let any
// class carrying a matching NodeId claim through. A browser's token has
// neither.
func AppSessionFromClaims(claims map[string]any) string {
	if claims == nil {
		return ""
	}
	class, _ := claims["class"].(string)
	if strings.TrimSpace(class) != appSessionClass {
		return ""
	}
	label, _ := claims["node_id"].(string)
	label = strings.TrimSpace(label)
	if !strings.HasPrefix(label, appSessionLabelPrefix) {
		return ""
	}
	sessionId := strings.TrimSpace(strings.TrimPrefix(label, appSessionLabelPrefix))
	if sessionId == "" {
		return ""
	}
	return sessionId
}

// appSessionToolDefs are the two tools, listed only when the caller IS an app
// session.
func appSessionToolDefs() []map[string]any {
	return []map[string]any{
		{
			"name": toolSubmit,
			"description": "Hand back the structured result for THIS app session. The session is " +
				"identified by your own credential, so no session id is taken. Submitting does not " +
				"end the session: the run finishes when your harness does, and the result you " +
				"submitted travels with it.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"result": map[string]any{
						"type":        "object",
						"description": "The structured answer, matching the response schema next_task reported.",
					},
				},
				"required": []any{"result"},
			},
		},
		{
			"name": toolNextTask,
			"description": "Ask what this app session was opened to do: the prompt and, when one " +
				"was requested, the JSON Schema your answer must match. Answers {idle: true} when " +
				"there is nothing outstanding.",
			"inputSchema": map[string]any{
				"type":       "object",
				"properties": map[string]any{},
			},
		},
	}
}

// isAppSessionTool reports whether name is one of the two.
func isAppSessionTool(name string) bool {
	return name == toolSubmit || name == toolNextTask
}

// handleSubmit records the app's structured answer on its own session row.
func handleSubmit(ctx context.Context, eng Engine, sessionId string, args map[string]any) map[string]any {
	if sessionId == "" {
		// The list gate should have kept us out of here. Said plainly rather
		// than as a generic failure: a browser session reaching this handler
		// is a gating bug, and "submit is only available to a delegated app"
		// is what tells the reader that.
		return errorResult("submit is only available to a delegated app session: this credential names none")
	}
	result, ok := args["result"].(map[string]any)
	if !ok || len(result) == 0 {
		return errorResult("submit requires a non-empty `result` object")
	}
	raw, err := json.Marshal(result)
	if err != nil {
		return errorResult(fmt.Sprintf("submit: the result could not be encoded: %v", err))
	}

	call, err := namedCallQuery("submitAppSessionResult", map[string]any{
		"sessionId": sessionId,
		"result":    result,
		// Server time, not the app's. A timestamp a caller supplies is a
		// claim; this row is evidence about when an answer arrived.
		"submittedAt": time.Now().UTC().Format(time.RFC3339Nano),
	})
	if err != nil {
		return errorResult(fmt.Sprintf("submit: %v", err))
	}
	if _, err := executeForSession(ctx, eng, call); err != nil {
		// The mutation is owner-tiered and the app acts AS its owner, so a
		// refusal here means the session is not this caller's -- which the
		// credential makes impossible unless the row was deleted underneath
		// it. Reported as-is rather than translated: guessing which of those
		// happened would put the wrong sentence in front of the reader.
		return errorResult(fmt.Sprintf("submit: %v", err))
	}
	return toolResultFromJSON(fmt.Sprintf(`{"submitted":true,"sessionId":%q,"bytes":%d}`, sessionId, len(raw)))
}

// handleNextTask answers what this session was opened to do.
//
// It reads the session's OWN row through the caller-scoped query, so the
// answer is bounded by the same row admission every other read on this
// credential passes. A session that has ended, or has already been answered,
// reports `idle` rather than handing the prompt out again: a harness that
// re-ran a finished task would duplicate whatever side effects it had.
func handleNextTask(ctx context.Context, eng Engine, sessionId string) map[string]any {
	if sessionId == "" {
		return errorResult("next_task is only available to a delegated app session: this credential names none")
	}
	call, err := namedCallQuery("appSessionById", map[string]any{"sessionId": sessionId})
	if err != nil {
		return errorResult(fmt.Sprintf("next_task: %v", err))
	}
	res, err := executeForSession(ctx, eng, call)
	if err != nil {
		return errorResult(fmt.Sprintf("next_task: %v", err))
	}
	row := firstRow(res)
	if row == nil {
		return toolResultFromJSON(`{"idle":true,"reason":"no session row is readable for this credential"}`)
	}

	status, _ := row["status"].(string)
	switch status {
	case "ended", "failed", "cancelled":
		return toolResultFromJSON(fmt.Sprintf(`{"idle":true,"reason":"this session is %s"}`, status))
	}
	if submitted, _ := row["resultSubmittedAt"].(string); strings.TrimSpace(submitted) != "" {
		return toolResultFromJSON(`{"idle":true,"reason":"a result was already submitted for this session"}`)
	}
	prompt, _ := row["prompt"].(string)
	if strings.TrimSpace(prompt) == "" {
		return toolResultFromJSON(`{"idle":true,"reason":"this session carries no prompt"}`)
	}

	answer := map[string]any{
		"sessionId": sessionId,
		"prompt":    prompt,
	}
	for _, field := range []string{"runId", "stepId"} {
		if value := stringValue(row[field]); value != "" {
			answer[field] = value
		}
	}
	if schema, _ := row["responseSchema"].(string); strings.TrimSpace(schema) != "" {
		answer["responseSchema"] = schema
	}
	if workspace, _ := row["workspace"].(string); strings.TrimSpace(workspace) != "" {
		answer["workspace"] = workspace
	}
	raw, err := json.Marshal(answer)
	if err != nil {
		return errorResult(fmt.Sprintf("next_task: %v", err))
	}
	return toolResultFromJSON(string(raw))
}

// firstRow pulls the first row out of an execute result, or nil.
func firstRow(res *memql.ExecuteResult) map[string]any {
	if res == nil || res.Bundle == nil {
		return nil
	}
	for _, node := range res.Bundle.Nodes {
		if node == nil || node.Payload == nil {
			continue
		}
		return node.Payload.AsMap()
	}
	return nil
}
