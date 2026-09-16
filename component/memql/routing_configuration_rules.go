package memql

import (
	"context"
	"fmt"
	"strings"

	"github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/core/airoute"
)

// SnapshotRouting takes policies and rules from the SAME committed revision.
func (r *PolicyRegistry) SnapshotRouting(ctx context.Context, base *RuleRegistry) (*PolicyRegistry, *RuleRegistry, error) {
	r.mu.RLock()
	attached := r.store != nil
	r.mu.RUnlock()
	if !attached {
		return r, base, nil
	}
	doc, defaults, err := r.configuration(ctx)
	if err != nil {
		return nil, nil, err
	}
	policies, err := policySnapshot(defaults, doc)
	if err != nil {
		return nil, nil, err
	}
	rules, err := configuredRules(base, doc)
	return policies, rules, err
}
func configuredRules(base *RuleRegistry, doc PolicyDocument) (*RuleRegistry, error) {
	if base == nil {
		return nil, nil
	}
	out := NewRuleRegistry()
	for _, rule := range base.Ordered() {
		c := *rule
		if err := out.Register(&c); err != nil {
			return nil, err
		}
	}
	for _, rule := range doc.Rules {
		c := rule
		if err := out.Register(&c); err != nil {
			return nil, err
		}
	}
	if err := out.Finalize(); err != nil {
		return nil, err
	}
	return out, nil
}

type configuredNames struct {
	rules    *RuleRegistry
	policies *PolicyRegistry
}

func (n configuredNames) HasRule(name string) bool {
	if n.rules == nil {
		return false
	}
	_, ok := n.rules.Lookup(name)
	return ok
}
func (n configuredNames) HasPolicy(name string) bool { _, ok := n.policies.Lookup(name); return ok }

func ruleWire(r *RuleConfig, revision int64) map[string]any {
	values := map[string]string{"level": r.When.Level, "modality": r.When.Modality, "prompt": r.When.Prompt, "role": r.When.Role, "actorRole": r.When.ActorRole, "tag": r.When.Tag, "touches": r.When.Touches}
	when := map[string]string{}
	for k, v := range values {
		if r.When.Has(k) {
			when[k] = v
		}
	}
	return map[string]any{"name": r.Name, "when": when, "level": string(r.Level), "policy": r.Policy, "precedence": r.Precedence, "onUnavailable": r.OnUnavailable, "excludes": r.Excludes, "locked": r.Locked, "described": r.Description, "revision": revision}
}
func (e *MemQLEngine) routingRulesBuiltin(ctx context.Context, _ map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	ac, ok := auth.AccessFromContext(ctx)
	if !ok || ac == nil || ac.UserId == "" {
		return nil, fmt.Errorf("rule catalog requires sign-in")
	}
	doc, _, err := e.policies.configuration(ctx)
	if err != nil {
		return nil, err
	}
	registry, err := configuredRules(e.rules, doc)
	if err != nil {
		return nil, err
	}
	out := []memorynodes.MemoryNode{}
	for _, r := range registry.Ordered() {
		rows, err := singleVirtualRow("v1:router:ruleCatalog", r.Name, ruleWire(r, doc.Revision))
		if err != nil {
			return nil, err
		}
		out = append(out, rows...)
	}
	return out, nil
}
func (e *MemQLEngine) ruleFromConfigurationArgs(ctx context.Context, args map[string]any) (*RuleConfig, PolicyDocument, error) {
	doc, defaults, err := e.policies.configuration(ctx)
	if err != nil {
		return nil, doc, err
	}
	policies, err := policySnapshot(defaults, doc)
	if err != nil {
		return nil, doc, err
	}
	when := map[string]string{}
	if raw, ok := args["conditions"]; ok && raw != nil {
		obj, ok := raw.(map[string]any)
		if !ok {
			return nil, doc, fmt.Errorf("when must be an object")
		}
		for k, v := range obj {
			s, ok := v.(string)
			if !ok {
				return nil, doc, fmt.Errorf("condition %s must be a string", k)
			}
			when[k] = s
		}
	}
	name := stringArg(args, "name")
	if name == "" {
		name = "draftRule"
	}
	proposal := RuleProposal{Name: name, When: when, Policy: stringArg(args, "policy"), Level: stringArg(args, "level"), OnUnavailable: stringArg(args, "onUnavailable")}
	if raw, ok := args["excludes"].([]any); ok {
		for _, v := range raw {
			s, ok := v.(string)
			if !ok {
				return nil, doc, fmt.Errorf("excludes must contain strings")
			}
			proposal.Excludes = append(proposal.Excludes, s)
		}
	}
	if err := ValidateRuleProposal(proposal, configuredNames{rules: e.rules, policies: policies}); err != nil {
		return nil, doc, err
	}
	if modality, ok := when["modality"]; ok && modality != "" && !airoute.Modality(modality).Valid() {
		return nil, doc, fmt.Errorf("unknown call modality %q", modality)
	}
	precedence, err := policyExpectedRevision(map[string]any{"expectedRevision": args["precedence"]})
	if err != nil || precedence > 1000000 {
		return nil, doc, fmt.Errorf("precedence must be an integer between 0 and 1000000")
	}
	present := map[string]bool{}
	for k := range when {
		present[k] = true
	}
	r := &RuleConfig{Name: name, Description: stringArg(args, "description"), Policy: proposal.Policy, Level: airoute.Level(proposal.Level), Precedence: int(precedence), OnUnavailable: proposal.OnUnavailable, Excludes: proposal.Excludes, When: RuleWhen{Level: when["level"], Modality: when["modality"], Prompt: when["prompt"], Role: when["role"], ActorRole: when["actorRole"], Tag: when["tag"], Touches: when["touches"], Present: present}, SourceFile: "routing configuration"}
	if r.OnUnavailable == "" {
		r.OnUnavailable = "degrade"
	}
	if doc.Rules == nil {
		doc.Rules = map[string]RuleConfig{}
	}
	doc.Rules[name] = *r
	if _, err := configuredRules(e.rules, doc); err != nil {
		return nil, doc, err
	}
	return r, doc, nil
}
func configurationAuthor(ctx context.Context) (string, error) {
	ac, ok := auth.AccessFromContext(ctx)
	if !ok || ac == nil || strings.TrimSpace(ac.UserId) == "" || !auth.CanAuthor(auth.UserContext{Role: ac.Role}) {
		return "", fmt.Errorf("routing changes require an authenticated owner or developer")
	}
	return ac.UserId, nil
}
func (e *MemQLEngine) routingRuleValidateBuiltin(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	if _, err := configurationAuthor(ctx); err != nil {
		return nil, err
	}
	_, doc, err := e.ruleFromConfigurationArgs(ctx, args)
	if err != nil {
		return nil, err
	}
	if raw, ok := args["expectedRevision"]; ok && raw != nil {
		expected, err := policyExpectedRevision(args)
		if err != nil {
			return nil, err
		}
		if doc.Revision != expected {
			return nil, fmt.Errorf("routing changed since this draft was opened; cancel and refresh before editing")
		}
	}
	return singleVirtualRow("v1:router:ruleCatalog", "validation", map[string]any{"validationOnly": true, "revision": doc.Revision, "considered": 0, "changed": 0})
}
func (e *MemQLEngine) routingRuleSaveBuiltin(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	actor, err := configurationAuthor(ctx)
	if err != nil {
		return nil, err
	}
	expected, err := policyExpectedRevision(args)
	if err != nil {
		return nil, err
	}
	rule, doc, err := e.ruleFromConfigurationArgs(ctx, args)
	if err != nil {
		return nil, err
	}
	if doc.Revision != expected {
		return nil, fmt.Errorf("routing configuration changed; refresh and validate again")
	}
	doc.Revision++
	doc.UpdatedBy = actor
	if e.policies.store == nil {
		return nil, fmt.Errorf("persistent routing storage is unavailable")
	}
	if err := e.policies.store.CompareAndSwap(ctx, expected, doc); err != nil {
		return nil, err
	}
	return singleVirtualRow("v1:router:ruleCatalog", rule.Name, ruleWire(rule, doc.Revision))
}
func (e *MemQLEngine) routingRuleRemoveBuiltin(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	actor, err := configurationAuthor(ctx)
	if err != nil {
		return nil, err
	}
	expected, err := policyExpectedRevision(args)
	if err != nil {
		return nil, err
	}
	name := stringArg(args, "name")
	if _, ok := e.rules.Lookup(name); ok {
		return nil, fmt.Errorf("shipped rule %s cannot be removed", name)
	}
	doc, _, err := e.policies.configuration(ctx)
	if err != nil {
		return nil, err
	}
	if doc.Revision != expected {
		return nil, fmt.Errorf("routing configuration changed; refresh before removing")
	}
	if _, ok := doc.Rules[name]; !ok {
		return nil, fmt.Errorf("custom rule %s is not present", name)
	}
	delete(doc.Rules, name)
	doc.Revision++
	doc.UpdatedBy = actor
	if err := e.policies.store.CompareAndSwap(ctx, expected, doc); err != nil {
		return nil, err
	}
	return singleVirtualRow("v1:router:ruleCatalog", name, map[string]any{"name": name, "status": "removed"})
}
