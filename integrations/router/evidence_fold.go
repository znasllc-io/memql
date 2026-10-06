package router

// The NIGHTLY EVIDENCE FOLD (epic memql#5146, design D5).
//
// ===========================================================================
// WHAT IT FOLDS AND WHAT IT REFUSES TO INVENT
// ===========================================================================
// A week of real calls says more about a model than any probe can. The fold
// counts them per (model, level, week), writes the counts down, and where the
// evidence carries a demotion it OPENS AN APPROVAL. It never applies one.
//
// It refuses to invent a level. A `v1:router:call` that did not say which level
// it was serving cannot support a demotion AT a level, and demoting at every
// level instead would be a far heavier act taken on the strength of a missing
// field. Those calls are COUNTED IN THE LOG and excluded from the fold, so a
// person reading the nightly line sees the shortfall rather than a fold that
// silently found nothing.
//
// That last sentence is load-bearing on this branch specifically: `level`
// arrives on the decision record with epic memql#5127, so until that lands the
// fold reads every call, finds no level on any of them, writes nothing, and
// SAYS SO. A sweep that quietly does nothing is indistinguishable from a fleet
// where every model is behaving, which is the one shape of silence this whole
// feature exists to break.

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	langparser "github.com/znasllc-io/memql/component/language/parser"
	routerlib "github.com/znasllc-io/memql/component/router"
	"github.com/znasllc-io/memql/component/work"
)

// EvidenceFoldResultConcept is the canonical id of the fold's answer.
const EvidenceFoldResultConcept = "v1:platform:evidenceFoldResult"

// handleEvidenceFold delegates policy to the installed recipe under the caller's authority.
func (i *Integration) handleEvidenceFold(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	result, err := i.evidenceWorkflow(ctx, time.Now().UTC())
	if err != nil {
		return nil, fmt.Errorf("routingEvidenceFold: %w", err)
	}
	return singleFoldRow(result), nil
}

// foldWindows groups call rows into per (model, level) windows.
//
// A call with no model or no level is EXCLUDED and COUNTED, never bucketed
// under an empty key: an empty-string level would collide every level's
// evidence into one row and then propose a demotion against it.
func foldWindows(rows []map[string]any, week string, categories []string) ([]routerlib.Window, int) {
	byKey := map[string]*routerlib.Window{}
	withoutLevel := 0
	for _, row := range rows {
		model := strings.TrimSpace(stringOf(row["model"]))
		level := strings.TrimSpace(stringOf(row["level"]))
		if model == "" {
			continue
		}
		if level == "" {
			withoutLevel++
			continue
		}
		key := model + "\x00" + level
		w, ok := byKey[key]
		if !ok {
			w = &routerlib.Window{ModelId: model, Level: level, Week: week}
			byKey[key] = w
		}
		w.Calls++
		if isStructuredFailure(row, categories) {
			w.StructuredFailures++
		}
	}
	out := make([]routerlib.Window, 0, len(byKey))
	for _, w := range byKey {
		out = append(out, *w)
	}
	return out, withoutLevel
}

// isStructuredFailure decides whether one call failed its structured contract.
//
// It is NARROW on purpose. A timeout, a rate limit and an auth failure are
// facts about the network and the account rather than about the model, and
// counting them would demote a model for an outage. Only an outcome of `error`
// whose category names the contract counts.
func isStructuredFailure(row map[string]any, categories []string) bool {
	if stringOf(row["outcome"]) != "error" {
		return false
	}
	category := strings.TrimSpace(stringOf(row["errorCategory"]))
	for _, c := range categories {
		if category == c {
			return true
		}
	}
	return false
}

// renderRule is the one renderer, injected into the pure layer.
//
// On this branch it is not reachable: the tree's renderer
// (component/routingrules.GenerateRule) arrives with epic memql#5127, and until
// then this REFUSES rather than rendering a rule of its own. A second renderer
// of one grammar drifts, and with the approval's hash over the output the drift
// is silent -- a person approves text A and text B is armed.
func (i *Integration) renderRule(f routerlib.Form) (string, error) {
	if i.ruleRenderer == nil {
		return "", fmt.Errorf(
			"no rule renderer is wired on this node, so the fold cannot render the demotion it would propose for %q; "+
				"the tree has exactly one renderer and this package must not become a second", f.Name)
	}
	return i.ruleRenderer(f)
}

func (i *Integration) readCallsInWindow(ctx context.Context, since, until time.Time) ([]map[string]any, error) {
	call, err := langparser.RenderCall("routerCallsInWindow", map[string]any{
		"since": since.Format(time.RFC3339),
		"until": until.Format(time.RFC3339),
	})
	if err != nil {
		return nil, err
	}
	res, err := i.engine.Execute(ctx, call)
	if err != nil {
		return nil, err
	}
	return payloadRows(res.OutputPayload()), nil
}

// priorEvidence reports what this week's evidence already decided for a window.
func (i *Integration) priorEvidence(ctx context.Context, w routerlib.Window) (string, error) {
	call, err := langparser.RenderCall("modelEvidenceForKey", map[string]any{
		"modelId": w.ModelId,
		"level":   w.Level,
		"week":    w.Week,
	})
	if err != nil {
		return "", err
	}
	res, err := i.engine.Execute(ctx, call)
	if err != nil {
		return "", err
	}
	for _, row := range payloadRows(res.OutputPayload()) {
		return strings.TrimSpace(stringOf(row["proposal"])), nil
	}
	return "", nil
}

func (i *Integration) openRoutingReview(ctx context.Context, w routerlib.Window, p routerlib.Proposal, now time.Time, approvalTTL time.Duration) (string, error) {
	approval := work.RoutingReviewApproval(work.RoutingProposal{
		ModelId:            p.ModelId,
		Level:              p.Level,
		Week:               p.Week,
		Direction:          p.Direction,
		RuleName:           p.RuleName,
		RuleSource:         p.RuleSource,
		Reason:             p.Reason,
		Calls:              w.Calls,
		StructuredFailures: w.StructuredFailures,
	}, now, approvalTTL)
	// The invariant the schema gave up when runId lost its bang, checked where
	// it can name the kind.
	if err := work.ValidateApprovalKind(approval); err != nil {
		return "", err
	}

	approvalId := "v1:work:approval:" + evidenceApprovalShortId(p)
	call, err := langparser.RenderCall("createWorkApproval", map[string]any{
		"approvalId":   approvalId,
		"runId":        "",
		"stepKey":      "",
		"kind":         approval.Kind,
		"subject":      approval.Subject,
		"artifactHash": approval.ArtifactHash,
		"question":     approval.Question,
		"options":      approval.Options,
		"evidence": map[string]any{
			"tier":   approval.Evidence.Tier,
			"reason": approval.Evidence.Reason,
			"source": approval.Evidence.Source,
		},
		"requestedAt": approval.RequestedAt.UTC().Format(time.RFC3339),
		"expiresAt":   approval.ExpiresAt.UTC().Format(time.RFC3339),
	})
	if err != nil {
		return "", err
	}
	if _, err := i.engine.Execute(ctx, call); err != nil {
		return "", err
	}
	return approvalId, nil
}

// evidenceApprovalShortId derives the approval's id from the proposal.
//
// DERIVED, so a fold that ran twice for one week writes one approval rather
// than two. The hash already identifies the exact rule; reusing it as the id
// means the second write is a new VERSION of the same question rather than a
// second copy of it in somebody's queue.
func evidenceApprovalShortId(p routerlib.Proposal) string {
	if len(p.Hash) >= 32 {
		return p.Hash[:32]
	}
	return p.Hash
}

func (i *Integration) writeEvidence(ctx context.Context, w routerlib.Window, outcome, approvalId, ruleHash string, now time.Time) error {
	evidenceId := "v1:platform:modelEvidence:" + evidenceRowShortId(w)
	call, err := langparser.RenderCall("recordModelEvidence", map[string]any{
		"evidenceId":         evidenceId,
		"modelId":            w.ModelId,
		"level":              w.Level,
		"week":               w.Week,
		"calls":              w.Calls,
		"structuredFailures": w.StructuredFailures,
		"repairs":            w.Repairs,
		"retries":            w.Retries,
		"parks":              w.Parks,
		"proposal":           outcome,
		"approvalId":         approvalId,
		"ruleHash":           ruleHash,
		"foldedAt":           now.Format(time.RFC3339),
	})
	if err != nil {
		return err
	}
	_, err = i.engine.Execute(ctx, call)
	return err
}

// evidenceRowShortId derives the evidence row's id from its key.
//
// One row per (model, level, week), so two folds of the same week land on one
// row rather than two -- which is what makes the row a record of a week rather
// than a record of how many times the fold ran.
func evidenceRowShortId(w routerlib.Window) string {
	return routerlib.ProposalHash(w.ModelId + "\x00" + w.Level + "\x00" + w.Week)[:32]
}

// isoWeek renders a time as YYYY-Www, the key form the evidence row stores.
func isoWeek(t time.Time) string {
	year, week := t.ISOWeek()
	return fmt.Sprintf("%04d-W%02d", year, week)
}

func stringOf(v any) string {
	s, _ := v.(string)
	return s
}

func payloadRows(payload any) []map[string]any {
	var raw []any
	switch v := payload.(type) {
	case []any:
		raw = v
	case map[string]any:
		if nodes, ok := v["nodes"].([]any); ok {
			raw = nodes
		}
	}
	out := make([]map[string]any, 0, len(raw))
	for _, item := range raw {
		if row, ok := item.(map[string]any); ok {
			out = append(out, row)
		}
	}
	return out
}

func singleFoldRow(payload map[string]any) []memorynodes.MemoryNode {
	raw, err := json.Marshal(payload)
	if err != nil {
		// Every value here is a string or an int, so this cannot happen -- and
		// an empty payload is still an honest row saying the fold ran.
		raw = []byte("{}")
	}
	return []memorynodes.MemoryNode{{
		ID:      EvidenceFoldResultConcept + ":current",
		Concept: EvidenceFoldResultConcept,
		Type:    memorynodes.NodeTypeObject,
		Payload: raw,
	}}
}
