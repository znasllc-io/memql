package memql

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/work"
	"github.com/znasllc-io/memql/core/common"
)

// ParseExecutionDestination is shared by Ask, voice and direct goal admission.
// It is a tool-execution constraint, independent of model/provider selection.
func ParseExecutionDestination(value string) (string, error) {
	value = strings.TrimSpace(value)
	switch value {
	case "", "auto":
		return "", nil
	case "workbench", "computer":
		return value, nil
	default:
		return "", &RouteChoiceError{Code: "execution_destination_invalid", Field: "destination", Value: value, Reason: "choose Auto, Workbench or Computer"}
	}
}

type ExecutionDestinationRequired struct{ From, To string }

func (e *ExecutionDestinationRequired) Error() string {
	return fmt.Sprintf("This goal is set to %s. Explain why %s is needed and request permission before moving this work.", e.From, e.To)
}

func (e *ExecutionDestinationRequired) Subject() map[string]any {
	return map[string]any{"from": e.From, "to": e.To}
}

// CheckExecutionDestination reads authority from the owned run on the serving
// replica. A forwarded request or model argument cannot replace the choice.
// Approval changes only destination eligibility; computer scopes still apply.
func CheckExecutionDestination(ctx context.Context, engine interface {
	Execute(context.Context, string) (*ExecuteResult, error)
}, runID, target string) error {
	if target != "workbench" && target != "computer" {
		return fmt.Errorf("invalid execution destination")
	}
	if rc, ok := common.RunFromContext(ctx); ok && rc.RunId != "" && BareShortId(rc.RunId) != BareShortId(runID) {
		return fmt.Errorf("execution destination belongs to another run")
	}
	q, err := parser.RenderCall("workRunForOwner", map[string]any{"runId": runID})
	if err != nil {
		return err
	}
	result, err := engine.Execute(ContextWithFreshRead(ctx), "query "+q)
	if err != nil {
		return err
	}
	rows := MaterializeRows(result)
	if len(rows) != 1 {
		return fmt.Errorf("execution destination requires an accessible work run")
	}
	run := rows[0]
	if run["cancelRequested"] == true {
		return fmt.Errorf("this work was cancelled before execution")
	}
	switch run["status"] {
	case "succeeded", "failed", "cancelled", "abandoned":
		return fmt.Errorf("this work finished before execution")
	}
	choice := common.RouteChoiceFrom(run["routing"])
	destination, err := ParseExecutionDestination(choice.Destination)
	if err != nil {
		return err
	}
	if destination == target || (destination == "" && target == "workbench") {
		return nil
	}
	// Auto can use the hosted sandbox directly, but choosing someone's own
	// computer still needs this goal's permission, even with a saved command
	// grant. Destination consent and access scope answer different questions.
	if destination == "" {
		destination = "auto"
	}
	subject, ok := auth.SubjectFromContext(ctx)
	owner, _ := run["ownerUserId"].(string)
	if !ok || !sameUserId(subject.UserId, owner) {
		return fmt.Errorf("execution destination requires the work's owner")
	}
	q, err = parser.RenderCall("workComputerScopesForOwnerRun", map[string]any{"runId": runID})
	if err != nil {
		return err
	}
	result, err = engine.Execute(ContextWithFreshRead(ctx), "query "+q)
	if err != nil {
		return err
	}
	for _, approval := range MaterializeRows(result) {
		if executionDestinationReceipt(approval, owner, runID, destination, target, time.Now()) {
			return nil
		}
	}
	return &ExecutionDestinationRequired{From: destination, To: target}
}

func executionDestinationReceipt(row map[string]any, owner, runID, from, to string, now time.Time) bool {
	text := func(key string) string { value, _ := row[key].(string); return value }
	if text("kind") != "scopeElevation" || text("decision") != "approved" || !sameUserId(text("decidedBy"), owner) || !sameUserId(text("ownerUserId"), owner) || BareShortId(text("runId")) != BareShortId(runID) || text("permissionRevokedAt") != "" {
		return false
	}
	expires, err := time.Parse(time.RFC3339Nano, text("expiresAt"))
	if err != nil || !now.Before(expires) {
		return false
	}
	subject, _ := row["subject"].(map[string]any)
	destination, _ := subject["executionDestination"].(map[string]any)
	return destination["from"] == from && destination["to"] == to && text("artifactHash") == work.ArtifactHash(subject)
}
