package automations

// value_leaves.go -- the string-typed step fields an automation carries as
// VALUE LEAVES (memql#5367).
//
// A compiled automation encodes every value position with one rule
// (compiler.EncodeValueLeaf): a literal is plain JSON, an expression is
// `{"$expr": "<canonical v1 source>"}`. That covers the args and payload maps
// and also a string-typed field that holds a value: an event's `topic`. It is
// a Go string, which JSON cannot decode an object (or a number) into, so the
// config decodes through unmarshalWithLeaves: a field whose JSON value is not
// a string is lifted into the config's unexported `leaves`, where
// PrepareExpressions reads it, and a field that is a string decodes into the
// Go field: a string literal.
//
// Marshalling writes the lifted leaves back, so a config round-trips to the
// JSON it was compiled as.

import (
	"bytes"
	"encoding/json"
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
