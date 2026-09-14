package steps

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/znasllc-io/memql/component/automations"
	langparser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/memql"
)

// MutationExecutor executes MemQL mutation (insert) steps: every value is
// evaluated first, and the insert is built from values only.
type MutationExecutor struct{}

// Execute runs a mutation step.
func (e *MutationExecutor) Execute(ctx context.Context, step *automations.Step, stepCtx *Context) (*automations.StepResult, error) {
	result := &automations.StepResult{
		StepId:    step.ID,
		StartedAt: time.Now(),
	}

	if step.Mutation == nil {
		result.Status = "failed"
		result.Error = "mutation configuration is required"
		result.CompletedAt = time.Now()
		result.Duration = result.CompletedAt.Sub(result.StartedAt)
		return result, fmt.Errorf("mutation configuration is required")
	}

	if stepCtx.Engine == nil {
		result.Status = "failed"
		result.Error = "MemQL engine not configured"
		result.CompletedAt = time.Now()
		result.Duration = result.CompletedAt.Sub(result.StartedAt)
		return result, fmt.Errorf("MemQL engine not configured")
	}

	cfg := step.Mutation

	// The concept is a literal. id, parent and aliasOf are expressions
	// parsed at load, and the payload's leaves are literals or parsed
	// expressions (memql#5367). Everything evaluates to a value first; the
	// insert text is then built from values only.
	x, err := preparedExprs(step)
	if err != nil {
		result.Status = "failed"
		result.Error = err.Error()
		result.CompletedAt = time.Now()
		result.Duration = result.CompletedAt.Sub(result.StartedAt)
		return result, err
	}
	return e.execute(ctx, step, stepCtx, result, cfg.Concept, x)
}

// execute evaluates the step's values and runs the insert: id, parent and
// aliasOf are expressions evaluated to text, the payload's leaves resolve to
// values (an absent leaf is omitted, rule 30), and the insert is built from
// values only -- the payload through json.Marshal, the strings through
// QuoteString.
func (e *MutationExecutor) execute(ctx context.Context, step *automations.Step, stepCtx *Context, result *automations.StepResult, concept string, x *automations.StepExprs) (*automations.StepResult, error) {
	fail := func(what string, err error) (*automations.StepResult, error) {
		result.Status = "failed"
		result.Error = fmt.Sprintf("failed to evaluate %s: %v", what, err)
		result.CompletedAt = time.Now()
		result.Duration = result.CompletedAt.Sub(result.StartedAt)
		return result, fmt.Errorf("failed to evaluate %s: %w", what, err)
	}
	id, err := v1Text(ctx, stepCtx.Evaluator, x.ID)
	if err != nil {
		return fail("id", err)
	}
	parent, err := v1Text(ctx, stepCtx.Evaluator, x.Parent)
	if err != nil {
		return fail("parent", err)
	}
	aliasOf, err := v1Text(ctx, stepCtx.Evaluator, x.AliasOf)
	if err != nil {
		return fail("aliasOf", err)
	}
	payload, err := stepCtx.Evaluator.ResolveV1Map(ctx, step.Mutation.Payload)
	if err != nil {
		return fail("payload", err)
	}
	query := e.buildInsertQuery(concept, id, payload, parent, aliasOf)
	return e.runInsert(ctx, step, stepCtx, result, concept, query)
}

// runInsert executes a built insert and records the step.
func (e *MutationExecutor) runInsert(ctx context.Context, step *automations.Step, stepCtx *Context, result *automations.StepResult, concept, query string) (*automations.StepResult, error) {
	if stepCtx.Logger != nil {
		stepCtx.Logger.Debug("executing mutation step",
			"step", step.ID,
			"concept", concept,
			"query", query,
		)
	}

	// Execute the mutation
	execResult, err := stepCtx.Engine.Execute(ctx, query)
	if err != nil {
		result.Status = "failed"
		result.Error = err.Error()
		result.CompletedAt = time.Now()
		result.Duration = result.CompletedAt.Sub(result.StartedAt)
		return result, fmt.Errorf("mutation execution failed: %w", err)
	}

	// Extract result info
	result.Status = "success"
	result.Result = convertMutationResult(execResult)
	result.CompletedAt = time.Now()
	result.Duration = result.CompletedAt.Sub(result.StartedAt)

	// Extract item count
	itemCount := extractMutationItemCount(execResult)

	// Build result query for retrieving the inserted record
	resultQuery := buildMutationResultQuery(concept, execResult)

	result.Metadata = map[string]any{
		"concept":     concept,
		"itemCount":   itemCount,
		"resultQuery": resultQuery,
	}

	// Record step execution
	runId := ""
	if stepCtx.Execution != nil {
		runId = stepCtx.Execution.ID
	}
	stepRecordQuery := RecordStepExecution(ctx, stepCtx.Engine, StepRecordData{
		RunId:       runId,
		StepId:      step.ID,
		StepType:    "mutation",
		Status:      result.Status,
		Query:       query,
		ResultQuery: resultQuery,
		ItemCount:   itemCount,
		Duration:    float64(result.Duration.Milliseconds()),
	})

	if stepCtx.Logger != nil {
		stepCtx.Logger.Debug("mutation step completed",
			"step", step.ID,
			"concept", concept,
			"resultQuery", resultQuery,
			"itemCount", itemCount,
			"stepRecord", stepRecordQuery,
			"duration", formatDuration(result.Duration),
		)
	}

	return result, nil
}

// buildInsertQuery constructs the insert query with properly escaped JSON payload.
//
// The four identifier positions render through langparser.QuoteString rather
// than fmt.Sprintf("%q"): Go's %q escape set and the lexer's do not agree, and
// the disagreement is a hard error at tokenize time, not a fallback. %q emits
// `\x00` / `\a` / `\v`; scanString implements the JSON escapes and only those.
// The id is the reachable one -- it is an evaluated expression, so it
// interpolates prior step results and event payload text, and one control
// byte in that text made the whole insert unparseable. memql#3192, the memql#3035 defect on this path.
//
// Quoted faithfully, never substituted. All four are IDENTIFIERS -- a concept
// name and three node ids -- so their bytes are load-bearing for identity in
// the strongest sense: substituting one would address a DIFFERENT row than the
// caller named, which is a worse failure than not writing at all. A NUL here
// still cannot be stored (Postgres refuses it in a text column as it does in
// jsonb), but quoting correctly is what makes that a reported storage error
// instead of a parse error that never names the offending value.
//
// The payload is a separate boundary and is deliberately untouched here: it
// goes through json.Marshal, whose escapes the lexer already accepts. Its own
// NUL-into-jsonb exposure is real but pre-dates and outlives this fix -- it is
// not a %q defect and is not silently folded into one.
func (e *MutationExecutor) buildInsertQuery(concept, id string, payload map[string]any, parent, aliasOf string) string {
	parts := []string{langparser.QuoteString(concept)}

	if id != "" {
		parts = append(parts, "id="+langparser.QuoteString(id))
	}

	if payload != nil {
		// Convert payload to JSON
		payloadJSON, err := json.Marshal(payload)
		if err != nil {
			// Fallback to empty object if marshal fails
			payloadJSON = []byte("{}")
		}
		parts = append(parts, fmt.Sprintf("payload=%s", string(payloadJSON)))
	}

	if parent != "" {
		parts = append(parts, "parent="+langparser.QuoteString(parent))
	}

	if aliasOf != "" {
		parts = append(parts, "aliasOf="+langparser.QuoteString(aliasOf))
	}

	return fmt.Sprintf("insert(%s)", strings.Join(parts, ", "))
}

// convertMutationResult converts the mutation result to an automation-friendly format.
func convertMutationResult(execResult any) any {
	if execResult == nil {
		return nil
	}
	return execResult
}

// extractMutationItemCount extracts the number of created items.
func extractMutationItemCount(execResult any) int {
	if execResult == nil {
		return 0
	}

	if er, ok := execResult.(*memql.ExecuteResult); ok {
		if er == nil || er.Bundle == nil {
			return 0
		}
		return len(er.Bundle.GetNodes())
	}

	return 1 // Assume 1 for successful insert
}

// buildMutationResultQuery constructs a query to retrieve the inserted record.
func buildMutationResultQuery(concept string, execResult any) string {
	if er, ok := execResult.(*memql.ExecuteResult); ok && er != nil && er.Bundle != nil {
		nodes := er.Bundle.GetNodes()
		if len(nodes) > 0 {
			node := nodes[0]
			id := node.GetId()
			if concept != "" && id != "" {
				// Use quoted concept and id for proper MemQL syntax
				return fmt.Sprintf(`concept==%s;id==%s`, jsonString(concept), jsonString(id))
			}
		}
	}
	return ""
}
