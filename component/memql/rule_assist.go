package memql

import (
	"context"
	"encoding/json"
	"fmt"

	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
)

// Compile a reviewable rule through the shipped local-only compiler prompt.
// Its result takes the same validation and revision path as the fields editor.
func (e *MemQLEngine) routingRuleDescribeBuiltin(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	if _, err := configurationAuthor(ctx); err != nil {
		return nil, err
	}
	sentence := stringArg(args, "sentence")
	if len(sentence) == 0 || len(sentence) > 12000 {
		return nil, fmt.Errorf("describe a rule in 1 to 12000 characters")
	}
	catalog, err := e.policies.Catalog(ctx)
	if err != nil {
		return nil, err
	}
	rawCatalog, err := json.Marshal(catalog)
	if err != nil {
		return nil, err
	}
	var policies []map[string]any
	if err := json.Unmarshal(rawCatalog, &policies); err != nil {
		return nil, err
	}
	schema := json.RawMessage(`{"type":"object","required":["name","when","level","policy","precedence","onUnavailable","excludes","restatement"],"additionalProperties":false,"properties":{"name":{"type":"string"},"when":{"type":"object","additionalProperties":{"type":"string"}},"level":{"type":"string"},"policy":{"type":"string"},"precedence":{"type":"integer"},"onUnavailable":{"type":"string"},"excludes":{"type":"array","items":{"type":"string"}},"restatement":{"type":"string"}}}`)
	result, err := e.InvokeAIStructured(ctx, "compileRule", map[string]any{"sentence": sentence, "whenKeys": RuleWhenKeys, "policies": policies, "levels": []string{"fast", "strong", "reasoning", "embeddings"}}, "routing_rule_proposal", schema, false)
	if err != nil {
		return nil, err
	}
	var proposal map[string]any
	if err := json.Unmarshal([]byte(result), &proposal); err != nil {
		return nil, fmt.Errorf("unreadable rule proposal: %w", err)
	}
	proposal["conditions"] = proposal["when"]
	proposal["description"] = sentence
	rule, doc, err := e.ruleFromConfigurationArgs(ctx, proposal)
	if err != nil {
		return nil, fmt.Errorf("the proposed rule was refused: %w", err)
	}
	if len(catalog) > 0 && doc.Revision != catalog[0].Revision {
		return nil, fmt.Errorf("routing changed while composing; ask again")
	}
	// A natural language creation must not silently replace an existing rule.
	current, _, err := e.policies.configuration(ctx)
	if err != nil {
		return nil, err
	}
	if _, exists := current.Rules[rule.Name]; exists {
		return nil, fmt.Errorf("custom rule %s already exists; edit it using its fields", rule.Name)
	}
	wire := ruleWire(rule, doc.Revision)
	wire["restatement"] = proposal["restatement"]
	return singleVirtualRow("v1:router:ruleCatalog", "rule-proposal", wire)
}
