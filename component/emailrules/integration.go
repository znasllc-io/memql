package emailrules

// integration.go -- the three DSL-callable operations, and how this package
// gets the two things a plug-in context cannot give it.
//
// `PluginContext` carries the ENGINE INTERFACE, which is enough for reads and
// writes and not enough for activation: `ActivateApprovedBundle` is a method on
// the concrete *MemQLEngine, and `AuthoredRuntimeDeps` is assembled from the
// live App's registry, scheduler hooks, catalog promoter and audit sink. So the
// plug-in registers itself the ordinary way and the App hands it those two
// through `Bind`, once, during the same phase that stands the authored runtime
// up.
//
// Until Bind has happened, `activate` and `retire` REFUSE with a sentence
// naming the reason rather than nil-panicking or silently doing nothing. A node
// type that does not run the authored runtime is a legitimate place for that
// refusal to be the permanent answer.

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/memql"
)

// IntegrationName is the DSL-visible namespace: integration.emailrules.*
const IntegrationName = "emailrules"

var (
	bindMu     sync.RWMutex
	boundEng   ActivationEngine
	boundDeps  func() memql.AuthoredRuntimeDeps
	boundSince time.Time
)

// Bind hands this package the concrete engine and the authored-runtime deps.
// Called once from the App during the engine phase, right after the authored
// runtime exists. Idempotent; a second call replaces the first, which is what a
// test that stands two engines up needs.
func Bind(engine ActivationEngine, deps func() memql.AuthoredRuntimeDeps) {
	bindMu.Lock()
	defer bindMu.Unlock()
	boundEng, boundDeps, boundSince = engine, deps, time.Now().UTC()
}

func bound() (ActivationEngine, func() memql.AuthoredRuntimeDeps, bool) {
	bindMu.RLock()
	defer bindMu.RUnlock()
	return boundEng, boundDeps, boundEng != nil && boundDeps != nil
}

func init() {
	memql.RegisterPlugin(IntegrationName, func(pctx memql.PluginContext) (memql.IntegrationProvider, error) {
		return &Integration{engine: pluginEngine{pctx.Engine}, logger: pctx.Logger}, nil
	})
}

// pluginEngine adapts the plug-in surface to the Execute shape this package
// reads through. The two differ only in the concrete result type -- the
// plug-in context hands back *memql.ExecuteResult while the concrete engine
// hands back `any` -- and MaterializeRows takes either. One adapter here beats
// two spellings of every read.
type pluginEngine struct{ inner memql.IntegrationEngineAccess }

func (p pluginEngine) Execute(ctx context.Context, query string) (any, error) {
	return p.inner.Execute(ctx, query)
}

// Integration is the provider the engine registers.
type Integration struct {
	engine Engine
	logger *slog.Logger
}

func (i *Integration) IntegrationName() string { return IntegrationName }

func (i *Integration) Capabilities() []memql.IntegrationCapability {
	return []memql.IntegrationCapability{
		{
			Name: "activate",
			Description: "Generate an event-email rule's automation construct and arm it immediately. " +
				"Owner or developer only: it authors an automation that sends mail on this cluster's behalf.",
			Handler: i.handleActivate,
			ArgsSchema: map[string]string{
				"emailRuleId": "string (required) - the rule to generate and arm",
			},
		},
		{
			Name: "retire",
			Description: "Retire an event-email rule's generated construct. The rule row survives with its history; " +
				"only the automation stops firing.",
			Handler: i.handleRetire,
			ArgsSchema: map[string]string{
				"emailRuleId": "string (required) - the rule whose construct should be retired",
			},
		},
		{
			Name: "fire",
			Description: "Run one event-email rule against one triggering row. The generated automation's only step; " +
				"not a client-callable operation.",
			Handler: i.handleFire,
			ArgsSchema: map[string]string{
				"emailRuleId": "string (required) - the rule that fired",
				"nodeId":      "string - the triggering row's id",
				"event":       "object - the triggering event envelope",
			},
		},
	}
}

func (i *Integration) handleActivate(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	engine, deps, ok := bound()
	if !ok {
		return nil, fmt.Errorf("emailrules.activate: the authored runtime is not wired on this node, so a rule cannot be armed here")
	}
	ruleID := strings.TrimSpace(argString(args, "emailRuleId"))
	if ruleID == "" {
		return nil, fmt.Errorf("emailrules.activate: emailRuleId is required")
	}
	owner := callerUserID(ctx)
	res, err := NewActivator(engine, deps).Activate(ctx, owner, ruleID)
	if err != nil {
		return nil, err
	}
	return resultNode("emailRuleActivated", map[string]any{
		"emailRuleId":   res.RuleID,
		"status":        res.Status,
		"bundleId":      res.BundleID,
		"constructName": res.ConstructName,
		"lane":          res.Lane,
	})
}

func (i *Integration) handleRetire(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	engine, deps, ok := bound()
	if !ok {
		return nil, fmt.Errorf("emailrules.retire: the authored runtime is not wired on this node, so a rule cannot be retired here")
	}
	ruleID := strings.TrimSpace(argString(args, "emailRuleId"))
	if ruleID == "" {
		return nil, fmt.Errorf("emailrules.retire: emailRuleId is required")
	}
	res, err := NewActivator(engine, deps).Retire(ctx, callerUserID(ctx), ruleID)
	if err != nil {
		return nil, err
	}
	return resultNode("emailRuleRetired", map[string]any{
		"emailRuleId": res.RuleID,
		"status":      res.Status,
	})
}

func (i *Integration) handleFire(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	ruleID := strings.TrimSpace(argString(args, "emailRuleId"))
	if ruleID == "" {
		return nil, fmt.Errorf("emailrules.fire: emailRuleId is required")
	}
	event, _ := args["event"].(map[string]any)
	out, err := NewFirer(i.engine).Fire(ctx, ruleID, strings.TrimSpace(argString(args, "nodeId")), event)
	if err != nil {
		return nil, err
	}
	if i.logger != nil && len(out.Refusals) > 0 {
		i.logger.Warn("email rule fired with refusals",
			"rule", out.RuleID, "lane", out.Lane, "sent", out.Sent, "refusals", out.Refusals)
	}
	return resultNode("emailRuleFired", map[string]any{
		"emailRuleId": out.RuleID,
		"lane":        out.Lane,
		"recipients":  out.Recipients,
		"sent":        out.Sent,
		"skipped":     out.Skipped,
		"refusals":    out.Refusals,
	})
}

func argString(args map[string]any, key string) string {
	if v, ok := args[key]; ok {
		if s, ok := v.(string); ok {
			return s
		}
		if v != nil {
			return fmt.Sprint(v)
		}
	}
	return ""
}

func callerUserID(ctx context.Context) string {
	if ac, ok := auth.AccessFromContext(ctx); ok && ac != nil {
		return strings.TrimSpace(ac.UserId)
	}
	return ""
}

// resultNode wraps a reply as the synthetic node the builtin surface returns.
// The concept id is deliberately outside the v{major}:{domain}:{entity} grammar
// -- it is a reply, not a row, and giving it a row-shaped id would put it in
// reach of things that walk concepts.
func resultNode(kind string, payload map[string]any) ([]memorynodes.MemoryNode, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("emailrules: marshal result: %w", err)
	}
	return []memorynodes.MemoryNode{{
		ID:        kind,
		Concept:   "integration:emailrules:" + kind,
		Type:      memorynodes.NodeTypeObject,
		CreatedAt: time.Now().UTC(),
		Payload:   body,
	}}, nil
}

var _ = boundSince
