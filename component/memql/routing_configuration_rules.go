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
// routingConfigurationSource is the SourceFile every saved rule carries, so a
// duplicate or a tie names "routing configuration" beside the other rule's file.
const routingConfigurationSource = "routing configuration"

// maxRulePrecedence bounds a saved rule's precedence. The shipped rules sit far
// below it, so any order an owner wants is reachable.
const maxRulePrecedence = 1000000

func (e *MemQLEngine) ruleFromConfigurationArgs(ctx context.Context, args map[string]any) (*RuleConfig, PolicyDocument, error) {
	doc, defaults, err := e.policies.configuration(ctx)
	if err != nil {
		return nil, doc, err
	}
	r, err := ruleConfigFromArgs(args)
	if err != nil {
		return nil, doc, err
	}
	staged, _, err := e.stageConfiguredRule(doc, defaults, r)
	if err != nil {
		return nil, doc, err
	}
	return r, staged, nil
}

// ruleConfigFromArgs reads the save and validate builtins' arguments into a
// rule. It refuses only what is malformed as an argument; whether the rule may
// be saved is stageConfiguredRule's question, asked identically of an approved
// routing review (routing_review_apply.go).
//
// An unreadable precedence is carried as -1 rather than refused here, so the
// refusals keep the order they had before the split: a bad name or policy is
// reported before a bad precedence.
func ruleConfigFromArgs(args map[string]any) (*RuleConfig, error) {
	when := map[string]string{}
	if raw, ok := args["conditions"]; ok && raw != nil {
		obj, ok := raw.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("when must be an object")
		}
		for k, v := range obj {
			s, ok := v.(string)
			if !ok {
				return nil, fmt.Errorf("condition %s must be a string", k)
			}
			when[k] = s
		}
	}
	name := stringArg(args, "name")
	if name == "" {
		name = "draftRule"
	}
	var excludes []string
	if raw, ok := args["excludes"].([]any); ok {
		for _, v := range raw {
			s, ok := v.(string)
			if !ok {
				return nil, fmt.Errorf("excludes must contain strings")
			}
			excludes = append(excludes, s)
		}
	}
	precedence, err := policyExpectedRevision(map[string]any{"expectedRevision": args["precedence"]})
	if err != nil || precedence > maxRulePrecedence {
		precedence = -1
	}
	present := map[string]bool{}
	for k := range when {
		present[k] = true
	}
	return &RuleConfig{Name: name, Description: stringArg(args, "description"), Policy: stringArg(args, "policy"), Level: airoute.Level(stringArg(args, "level")), Precedence: int(precedence), OnUnavailable: stringArg(args, "onUnavailable"), Excludes: excludes, When: RuleWhen{Level: when["level"], Modality: when["modality"], Prompt: when["prompt"], Role: when["role"], ActorRole: when["actorRole"], Tag: when["tag"], Touches: when["touches"], Present: present}, SourceFile: routingConfigurationSource}, nil
}

// stageConfiguredRule is the ONE set of checks a rule passes before it enters
// the routing configuration: the shipped-name, policy-exists, closed-key, level
// and on-unavailable checks of ValidateRuleProposal against the policies this
// document makes effective, the modality, the precedence range, and the
// loader's own duplicate-name and precedence-tie rules (configuredRules).
//
// routingRuleSave and an approved routing review both call it, so a rule
// cannot be admitted by one door and refused by the other. It stages a COPY:
// the caller's document is untouched when a check refuses. It returns the
// staged document and the rule set the router would walk with it.
func (e *MemQLEngine) stageConfiguredRule(doc PolicyDocument, defaults map[string]*PolicyConfig, r *RuleConfig) (PolicyDocument, *RuleRegistry, error) {
	policies, err := policySnapshot(defaults, doc)
	if err != nil {
		return doc, nil, err
	}
	// Every key the author WROTE, known or not: an unknown key must reach
	// ValidateRuleProposal to be refused by name, and RuleWhen has no field to
	// carry its value, which the refusal does not need.
	when := map[string]string{}
	for key, present := range r.When.Present {
		if present {
			when[key] = ruleWhenValue(r.When, key)
		}
	}
	proposal := RuleProposal{Name: r.Name, When: when, Policy: r.Policy, Level: string(r.Level), OnUnavailable: r.OnUnavailable, Excludes: r.Excludes}
	if err := ValidateRuleProposal(proposal, configuredNames{rules: e.rules, policies: policies}); err != nil {
		return doc, nil, err
	}
	if modality, ok := when["modality"]; ok && modality != "" && !airoute.Modality(modality).Valid() {
		return doc, nil, fmt.Errorf("unknown call modality %q", modality)
	}
	if r.Precedence < 0 || r.Precedence > maxRulePrecedence {
		return doc, nil, fmt.Errorf("precedence must be an integer between 0 and %d", maxRulePrecedence)
	}
	if r.OnUnavailable == "" {
		r.OnUnavailable = OnUnavailableDegrade
	}
	staged := doc
	staged.Rules = make(map[string]RuleConfig, len(doc.Rules)+1)
	for name, rule := range doc.Rules {
		staged.Rules[name] = rule
	}
	staged.Rules[r.Name] = *r
	rules, err := configuredRules(e.rules, staged)
	if err != nil {
		return doc, nil, err
	}
	return staged, rules, nil
}

// ruleWhenValue is the value a rule states for one @when key.
func ruleWhenValue(w RuleWhen, key string) string {
	switch key {
	case "level":
		return w.Level
	case "modality":
		return w.Modality
	case "prompt":
		return w.Prompt
	case "role":
		return w.Role
	case "actorRole":
		return w.ActorRole
	case "tag":
		return w.Tag
	case "touches":
		return w.Touches
	}
	return ""
}

// RoutingConfigurationAuthor is the one rule for "may change routing": an
// authenticated owner or developer (auth.CanAuthor). The routing builtins and
// the routing review decision (integrations/work) both ask it, so the people
// who may save a rule by hand and the people who may approve one are the same
// people by construction.
func RoutingConfigurationAuthor(ctx context.Context) (string, error) {
	ac, ok := auth.AccessFromContext(ctx)
	if !ok || ac == nil || strings.TrimSpace(ac.UserId) == "" || !auth.CanAuthor(auth.UserContext{Role: ac.Role}) {
		return "", fmt.Errorf("routing changes require an authenticated owner or developer")
	}
	return ac.UserId, nil
}
func (e *MemQLEngine) routingRuleValidateBuiltin(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	if _, err := RoutingConfigurationAuthor(ctx); err != nil {
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
	actor, err := RoutingConfigurationAuthor(ctx)
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
	actor, err := RoutingConfigurationAuthor(ctx)
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
