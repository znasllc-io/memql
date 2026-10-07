package router

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/znasllc-io/memql/component/automations/workflowhost"
	"github.com/znasllc-io/memql/component/memql"
	routerlib "github.com/znasllc-io/memql/component/router"
	"github.com/znasllc-io/memql/core/num"
	"time"
)

// The rule source and its hash are created together after DSL chooses a form.
// This callback can render a proposal, never activate a routing rule.
func proposeEvidence(ctx context.Context, w routerlib.Window, direction string, validity float64, measured bool, render routerlib.Renderer) (routerlib.Proposal, bool, error) {
	var proposal routerlib.Proposal
	if render == nil {
		return proposal, false, fmt.Errorf("router: an evidence proposal needs a rule renderer")
	}
	_, err := workflowhost.Run(ctx, "routingEvidenceProposal", map[string]any{
		"valid": w.Valid(), "model": w.ModelId, "level": w.Level, "week": w.Week, "calls": w.Calls, "failures": w.StructuredFailures,
		"callsText": fmt.Sprint(w.Calls), "failuresText": fmt.Sprint(w.StructuredFailures), "failureRate": w.FailureRate(), "percent": fmt.Sprintf("%.0f", w.FailureRate()*100), "direction": direction,
		"measured": measured, "validity": validity, "validityPercent": fmt.Sprintf("%.0f", validity*100),
	}, workflowhost.Options{Operations: map[string]workflowhost.Operation{
		"routingRenderEvidenceProposal": func(_ context.Context, a map[string]any) (any, error) {
			raw, err := json.Marshal(a["form"])
			if err != nil {
				return nil, err
			}
			var form routerlib.Form
			if err = json.Unmarshal(raw, &form); err != nil {
				return nil, err
			}
			// The selected window, not DSL-supplied identity, binds the approval.
			form.Name = routerlib.RuleName(direction, w.ModelId, w.Level)
			form.Level = w.Level
			form.When = map[string]string{"level": w.Level}
			source, err := render(form)
			if err != nil {
				return nil, fmt.Errorf("router: render the proposed rule: %w", err)
			}
			proposal = routerlib.Proposal{ModelId: w.ModelId, Level: w.Level, Week: w.Week, Direction: direction, RuleName: form.Name, RuleSource: source, Hash: routerlib.ProposalHash(source), Reason: stringOf(a["reason"])}
			return nil, nil
		},
	}})
	return proposal, err == nil && proposal.Hash != "", err
}

func (i *Integration) evidenceWorkflow(ctx context.Context, now time.Time) (map[string]any, error) {
	selected := map[string]routerlib.Window{}
	proposals := map[string]routerlib.Proposal{}
	type outcome struct{ proposal, approval, hash string }
	outcomes := map[string]outcome{}
	result := map[string]any{"calls": 0, "callsWithoutALevel": 0, "evidenceWritten": 0, "proposalsOpened": 0, "foldedAt": now.Format(time.RFC3339)}
	written, opened := 0, 0
	_, err := workflowhost.Run(ctx, "routingEvidenceWorkflow", nil, workflowhost.Options{Logger: i.logger, Operations: map[string]workflowhost.Operation{
		"routingReadEvidenceWindows": func(ctx context.Context, a map[string]any) (any, error) {
			days := num.Float64OrZero(numberArg(a["days"]))
			if days < 1 || days > 90 {
				return nil, fmt.Errorf("evidence window must be between 1 and 90 days")
			}
			since := now.AddDate(0, 0, -days)
			week := isoWeek(since)
			rows, err := i.readCallsInWindow(ctx, since, now)
			if err != nil {
				return nil, err
			}
			categories := []string{}
			items, ok := a["failureCategories"].([]any)
			if !ok {
				return nil, fmt.Errorf("failure categories must be a list")
			}
			for _, v := range items {
				categories = append(categories, stringOf(v))
			}
			windows, without := foldWindows(rows, week, categories)
			routerlib.SortWindows(windows)
			result["week"], result["calls"], result["callsWithoutALevel"] = week, len(rows), without
			keys := []string{}
			for _, w := range windows {
				key := evidenceRowShortId(w)
				keys = append(keys, key)
				selected[key] = w
			}
			if i.logger != nil {
				i.logger.Info("routingEvidenceFold: folded calls", "week", week, "calls", len(rows), "windows", len(windows), "calls_without_a_level", without)
			}
			return keys, nil
		},
		"routingEvaluateEvidence": func(ctx context.Context, a map[string]any) (any, error) {
			key := stringOf(a["key"])
			w, ok := selected[key]
			if !ok {
				return nil, fmt.Errorf("evidence outside selected window")
			}
			p, ok, err := proposeEvidence(ctx, w, "demotion", 0, false, i.renderRule)
			if err == nil && ok {
				proposals[key] = p
			}
			return ok, err
		},
		"routingPriorEvidence": func(ctx context.Context, a map[string]any) (any, error) {
			w, ok := selected[stringOf(a["key"])]
			if !ok {
				return nil, fmt.Errorf("evidence outside selected window")
			}
			return i.priorEvidence(ctx, w)
		},
		"routingOpenEvidenceReview": func(ctx context.Context, a map[string]any) (any, error) {
			key := stringOf(a["key"])
			p, ok := proposals[key]
			if !ok {
				return nil, fmt.Errorf("review requires a rendered proposal")
			}
			days := numberArg(a["days"])
			if days < 1 || days > 90 {
				return nil, fmt.Errorf("review lifetime must be between 1 and 90 days")
			}
			id, err := i.openRoutingReview(ctx, selected[key], p, now, time.Duration(days*float64(24*time.Hour)))
			if err == nil {
				outcomes[key] = outcome{p.Direction, id, p.Hash}
				opened++
			}
			return nil, err
		},
		"routingWriteEvidence": func(ctx context.Context, a map[string]any) (any, error) {
			key := stringOf(a["key"])
			w, ok := selected[key]
			if !ok {
				return nil, fmt.Errorf("evidence outside selected window")
			}
			o := outcomes[key]
			if o.proposal == "" {
				o.proposal = stringOf(a["outcome"])
			}
			if err := i.writeEvidence(ctx, w, o.proposal, o.approval, o.hash, now); err != nil {
				return nil, err
			}
			written++
			return nil, nil
		},
	}})
	result["evidenceWritten"], result["proposalsOpened"] = written, opened
	return result, err
}

func numberArg(v any) float64 {
	switch n := v.(type) {
	case float64:
		return n
	case int:
		return float64(n)
	case int64:
		return float64(n)
	}
	return 0
}

func evidenceScopeCapabilities() []memql.IntegrationCapability {
	return workflowhost.ScopedCapabilities(map[string]workflowhost.Operation{
		"routingReadEvidenceWindows": nil, "routingEvaluateEvidence": nil, "routingPriorEvidence": nil, "routingOpenEvidenceReview": nil, "routingWriteEvidence": nil, "routingRenderEvidenceProposal": nil,
	})
}
