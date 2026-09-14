package automations

// value_leaves.go -- the string-typed step fields a v1 automation carries as
// VALUE LEAVES (memql#5367).
//
// A v1 automation encodes every value position with one rule
// (compiler.EncodeValueLeaf): a literal is plain JSON, an expression is
// `{"$expr": "<canonical v1 source>"}`. That covers the args and payload maps
// and also the string-typed fields that hold a value: an event's `topic`, a
// webhook's `url` and header values, a mutation's `id` / `parent` /
// `aliasOf`, and a concept card's `cardType` / `partitionId` / `conceptRef`.
// Those fields are Go strings, which JSON cannot decode an object (or a
// number) into, so each config type below decodes through
// unmarshalWithLeaves: a field whose JSON value is not a string is lifted into
// the config's unexported `leaves`, where PrepareExpressions reads it, and a
// field that is a string decodes into the Go field exactly as before. A legacy
// automation's compiled JSON only ever holds strings there, so its decoding is
// unchanged -- and PrepareExpressions refuses a legacy automation that carries
// a lifted leaf, rather than letting the string evaluator read an empty
// field.
//
// Marshalling writes the lifted leaves back, so a config round-trips to the
// JSON it was compiled as.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// leafFields holds a config's lifted value leaves, keyed by JSON field name;
// a header is keyed "headers.<name>".
type leafFields map[string]json.RawMessage

// isJSONString reports whether raw is a JSON string.
func isJSONString(raw json.RawMessage) bool {
	t := bytes.TrimSpace(raw)
	return len(t) > 0 && t[0] == '"'
}

// unmarshalWithLeaves decodes data into v (a pointer to the config's plain
// alias type), first lifting every named field -- and every entry of each
// named map field -- whose JSON value is not a string.
func unmarshalWithLeaves(data []byte, v any, fields, mapFields []string) (leafFields, error) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		// Not an object: let the plain decode report it in its own words.
		return nil, json.Unmarshal(data, v)
	}
	var leaves leafFields
	lift := func(key string, r json.RawMessage) {
		if leaves == nil {
			leaves = leafFields{}
		}
		leaves[key] = r
	}
	for _, f := range fields {
		if r, ok := raw[f]; ok && !isJSONString(r) && string(bytes.TrimSpace(r)) != "null" {
			lift(f, r)
			delete(raw, f)
		}
	}
	for _, f := range mapFields {
		r, ok := raw[f]
		if !ok {
			continue
		}
		var m map[string]json.RawMessage
		if json.Unmarshal(r, &m) != nil {
			continue // the plain decode reports a non-object
		}
		changed := false
		for k, entry := range m {
			if !isJSONString(entry) {
				lift(f+"."+k, entry)
				delete(m, k)
				changed = true
			}
		}
		if changed {
			b, err := json.Marshal(m)
			if err != nil {
				return nil, err
			}
			raw[f] = b
		}
	}
	if leaves == nil {
		return nil, json.Unmarshal(data, v)
	}
	cleaned, err := json.Marshal(raw)
	if err != nil {
		return nil, err
	}
	return leaves, json.Unmarshal(cleaned, v)
}

// marshalWithLeaves marshals v (the config's plain alias) and writes its
// lifted leaves back in place.
func marshalWithLeaves(v any, leaves leafFields) ([]byte, error) {
	b, err := json.Marshal(v)
	if err != nil || len(leaves) == 0 {
		return b, err
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(b, &raw); err != nil {
		return nil, err
	}
	nested := map[string]map[string]json.RawMessage{}
	for key, r := range leaves {
		field, entry, isEntry := strings.Cut(key, ".")
		if !isEntry {
			raw[key] = r
			continue
		}
		m, ok := nested[field]
		if !ok {
			m = map[string]json.RawMessage{}
			if existing, has := raw[field]; has {
				_ = json.Unmarshal(existing, &m)
			}
			nested[field] = m
		}
		m[entry] = r
	}
	for field, m := range nested {
		mb, err := json.Marshal(m)
		if err != nil {
			return nil, err
		}
		raw[field] = mb
	}
	return json.Marshal(raw)
}

// leafKeys lists a config's lifted leaves, sorted, for a refusal message.
func (l leafFields) keys() []string {
	out := make([]string, 0, len(l))
	for k := range l {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// UnmarshalJSON decodes the config, lifting a non-string `topic`.
func (c *EventStepConfig) UnmarshalJSON(data []byte) error {
	type plain EventStepConfig
	var p plain
	leaves, err := unmarshalWithLeaves(data, &p, []string{"topic"}, nil)
	if err != nil {
		return err
	}
	*c = EventStepConfig(p)
	c.leaves = leaves
	return nil
}

// MarshalJSON writes the config with its lifted leaves.
func (c EventStepConfig) MarshalJSON() ([]byte, error) {
	type plain EventStepConfig
	return marshalWithLeaves(plain(c), c.leaves)
}

// UnmarshalJSON decodes the config, lifting a non-string `url` or header.
func (c *WebhookStepConfig) UnmarshalJSON(data []byte) error {
	type plain WebhookStepConfig
	var p plain
	leaves, err := unmarshalWithLeaves(data, &p, []string{"url"}, []string{"headers"})
	if err != nil {
		return err
	}
	*c = WebhookStepConfig(p)
	c.leaves = leaves
	return nil
}

// MarshalJSON writes the config with its lifted leaves.
func (c WebhookStepConfig) MarshalJSON() ([]byte, error) {
	type plain WebhookStepConfig
	return marshalWithLeaves(plain(c), c.leaves)
}

// UnmarshalJSON decodes the config, lifting a non-string `id`, `parent` or
// `aliasOf`.
func (c *MutationStepConfig) UnmarshalJSON(data []byte) error {
	type plain MutationStepConfig
	var p plain
	leaves, err := unmarshalWithLeaves(data, &p, []string{"id", "parent", "aliasOf"}, nil)
	if err != nil {
		return err
	}
	*c = MutationStepConfig(p)
	c.leaves = leaves
	return nil
}

// MarshalJSON writes the config with its lifted leaves.
func (c MutationStepConfig) MarshalJSON() ([]byte, error) {
	type plain MutationStepConfig
	return marshalWithLeaves(plain(c), c.leaves)
}

// UnmarshalJSON decodes the config, lifting a non-string `cardType`,
// `partitionId` or `conceptRef`.
func (c *EmitConceptCardStepConfig) UnmarshalJSON(data []byte) error {
	type plain EmitConceptCardStepConfig
	var p plain
	leaves, err := unmarshalWithLeaves(data, &p, []string{"cardType", "partitionId", "conceptRef"}, nil)
	if err != nil {
		return err
	}
	*c = EmitConceptCardStepConfig(p)
	c.leaves = leaves
	return nil
}

// MarshalJSON writes the config with its lifted leaves.
func (c EmitConceptCardStepConfig) MarshalJSON() ([]byte, error) {
	type plain EmitConceptCardStepConfig
	return marshalWithLeaves(plain(c), c.leaves)
}

// stepLeaves returns the lifted value leaves of a step's config, if any.
func stepLeaves(s *Step) leafFields {
	switch {
	case s.Event != nil:
		return s.Event.leaves
	case s.Webhook != nil:
		return s.Webhook.leaves
	case s.Mutation != nil:
		return s.Mutation.leaves
	case s.EmitConceptCard != nil:
		return s.EmitConceptCard.leaves
	}
	return nil
}

// refuseLegacyLeaves refuses a legacy automation whose compiled JSON carries a
// value leaf the string evaluator cannot read (an `{"$expr"}` object, or a
// number where a string belongs), naming the first one. Without it the Go
// field would decode empty and the step would run with, say, an empty topic.
func refuseLegacyLeaves(a *Automation) error {
	var walk func(steps []*Step) error
	walk = func(steps []*Step) error {
		for _, s := range steps {
			if s == nil {
				continue
			}
			if l := stepLeaves(s); len(l) > 0 {
				return fmt.Errorf("automation %q: step %q carries a v1 value leaf in %s, but the automation is not marked `\"expressions\": \"v1\"`", a.Name, s.ID, strings.Join(l.keys(), ", "))
			}
			if s.ForEach != nil {
				if err := walk(s.ForEach.Do); err != nil {
					return err
				}
			}
			if s.Parallel != nil {
				if err := walk(s.Parallel.Branches); err != nil {
					return err
				}
			}
			if s.Switch != nil {
				for _, k := range sortedCaseKeys(s.Switch.Cases) {
					if err := walk(caseSteps(s.Switch.Cases[k])); err != nil {
						return err
					}
				}
				if err := walk(caseSteps(s.Switch.Default)); err != nil {
					return err
				}
			}
		}
		return nil
	}
	if err := walk(a.Steps); err != nil {
		return err
	}
	return walk([]*Step{a.OnComplete, a.OnError})
}
