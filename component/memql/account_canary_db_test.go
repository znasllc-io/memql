package memql

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
)

// THE CANARY SWEEP (design record
// docs/superpowers/specs/2026-11-12-account-row-ownership-design.md, section C).
//
// Every engine-managed concept in the loaded registry is written twice, once in
// each of two accounts, through the ordinary write path with the call's account
// stated, and each stored row must record the account it was written in. The
// list is read from the registry rather than written here, so a concept added
// later is covered with nothing to change in this file, and one whose writes
// lose the account (a before-write hook that rebuilds the payload, a guard that
// strips unknown keys, a table of its own the stamp never reaches) fails here
// by name.
//
// A payload is generated from the concept's own definition schema. A concept is
// SKIPPED, and listed with the reason, only when no valid payload can be
// generated for it or a guard of its own refuses the generated write. A sweep
// that silently skipped its way to green would prove nothing, so it also fails
// below a floor of exercised concepts.

// canaryMinimumExercised is the floor. The tree has well over twice this many
// engine-managed concepts; most of the rest are mirror rows written by their
// connector. A drop below it means the generator or a shared guard has started
// refusing whole families, which is a change to look at, not to absorb.
const canaryMinimumExercised = 60

func TestAccountCanarySweep(t *testing.T) {
	f := newStampFixture(t, "stamp-canary")
	accountA, accountB := "canary-a-"+f.suffix, "canary-b-"+f.suffix

	var exercised []string
	skipped := map[string]string{}
	for _, c := range engineManagedConcepts() {
		if _, ok := canaryPayload(c, f.suffix); !ok {
			skipped[c.Name] = "no valid payload can be generated from its schema"
			continue
		}
		rows := map[string]string{}
		var refused error
		for _, account := range []string{accountA, accountB} {
			// A payload of its own per write: a concept whose slug or name
			// must be unique would otherwise refuse the second row.
			id := f.id("canary")
			payload, _ := canaryPayload(c, id)
			ctx := canaryContext(c, f.trusted(account))
			id, err := f.insert(ctx, c.Name, id, payload)
			if err != nil {
				refused = err
				break
			}
			rows[account] = id
		}
		if refused != nil {
			skipped[c.Name] = "its write is refused: " + firstLine(refused.Error())
			continue
		}
		for account, id := range rows {
			stored, exists, err := f.engine.loadPriorPayload(f.trusted(""), c, id)
			require.NoError(t, err, "%s: reading back %s", c.Name, id)
			require.True(t, exists, "%s: the row written as %s is not stored under that id", c.Name, id)
			require.Equal(t, account, bareAccount(stored["accountId"]),
				"%s: a row written in account %s records %v", c.Name, account, stored["accountId"])
		}
		exercised = append(exercised, c.Name)
	}

	names := make([]string, 0, len(skipped))
	for name := range skipped {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		t.Logf("SKIPPED %s: %s", name, skipped[name])
	}
	t.Logf("exercised %d engine-managed concepts, skipped %d", len(exercised), len(skipped))
	require.GreaterOrEqual(t, len(exercised), canaryMinimumExercised,
		"the canary exercised only %d concepts; the skips are listed above", len(exercised))
}

// engineManagedConcepts is every concept whose account the engine decides, in a
// stable order so a failure reproduces.
func engineManagedConcepts() []*memorynodes.Concept {
	var out []*memorynodes.Concept
	for _, c := range memorynodes.List() {
		if c != nil && c.BelongsTo.EngineManaged() {
			out = append(out, c)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// declaresAccountRelationship reports an outgoing relationship on accountId.
func declaresAccountRelationship(c *memorynodes.Concept) bool {
	for _, rel := range c.Relationships {
		if strings.TrimSpace(rel.Field) == accountField && !strings.EqualFold(strings.TrimSpace(rel.Direction), relationshipDirectionIncoming) {
			return true
		}
	}
	return false
}

// canaryContext is who may write the concept at all. A mirror concept is
// written only by the connector its @origin names (mirror_write_guard.go), so
// its rows are written as that connector, with the account still stated: that
// is how a sync run reaches them.
func canaryContext(c *memorynodes.Concept, base context.Context) context.Context {
	if c.IsMirror() {
		return auth.ContextWithConnectorActor(base, c.EffectiveOrigin())
	}
	return base
}

// canaryPayload builds the required fields of a concept from its definition
// schema. The `via` field, when the schema does not require it, is left out: a
// row with no parent takes the call's account, which is what this sweep checks.
// A required `via` field gets a value naming no row, for the same reason.
func canaryPayload(c *memorynodes.Concept, suffix string) (map[string]any, bool) {
	raw, err := c.DefinitionSchema()
	if err != nil || len(raw) == 0 {
		return nil, false
	}
	var schema map[string]any
	if json.Unmarshal(raw, &schema) != nil {
		return nil, false
	}
	value, ok := canaryValue(schema, "canary-"+suffix, 0)
	if !ok {
		return nil, false
	}
	payload, ok := value.(map[string]any)
	if !ok {
		return nil, false
	}
	delete(payload, accountField)
	delete(payload, accountListField)
	return payload, true
}

// canaryValue is a plausible value for one schema node, and false when none can
// be made.
func canaryValue(schema map[string]any, seed string, depth int) (any, bool) {
	if depth > 8 || schema == nil {
		return nil, false
	}
	if v, ok := schema["const"]; ok {
		return v, true
	}
	if enum, ok := schema["enum"].([]any); ok {
		for _, v := range enum {
			if v != nil {
				return v, true
			}
		}
		return nil, false
	}
	for _, key := range []string{"oneOf", "anyOf", "allOf"} {
		options, ok := schema[key].([]any)
		if !ok {
			continue
		}
		for _, option := range options {
			sub, _ := option.(map[string]any)
			if schemaType(sub) == "null" {
				continue
			}
			if v, ok := canaryValue(sub, seed, depth+1); ok {
				return v, true
			}
		}
		return nil, false
	}

	switch schemaType(schema) {
	case "string":
		return canaryString(schema, seed)
	case "integer":
		return canaryNumber(schema, true), true
	case "number":
		return canaryNumber(schema, false), true
	case "boolean":
		return false, true
	case "array":
		minItems := intOf(schema["minItems"])
		items, _ := schema["items"].(map[string]any)
		out := make([]any, 0, minItems)
		for i := 0; i < minItems; i++ {
			v, ok := canaryValue(items, fmt.Sprintf("%s-%d", seed, i), depth+1)
			if !ok {
				return nil, false
			}
			out = append(out, v)
		}
		return out, true
	case "object", "":
		properties, _ := schema["properties"].(map[string]any)
		out := map[string]any{}
		required, _ := schema["required"].([]any)
		for _, r := range required {
			name, _ := r.(string)
			sub, _ := properties[name].(map[string]any)
			if sub == nil {
				// Required and undescribed: any string is as good as another.
				out[name] = seed
				continue
			}
			v, ok := canaryValue(sub, seed, depth+1)
			if !ok {
				return nil, false
			}
			out[name] = v
		}
		return out, true
	}
	return nil, false
}

func schemaType(schema map[string]any) string {
	switch t := schema["type"].(type) {
	case string:
		return t
	case []any:
		for _, v := range t {
			if s, _ := v.(string); s != "" && s != "null" {
				return s
			}
		}
		return "null"
	}
	return ""
}

func canaryString(schema map[string]any, seed string) (any, bool) {
	switch schema["format"] {
	case "date-time":
		return time.Now().UTC().Format(time.RFC3339), true
	case "date":
		return time.Now().UTC().Format("2006-01-02"), true
	case "email":
		return "canary@example.test", true
	case "uri", "url":
		return "https://example.test/canary", true
	case "uuid":
		return "00000000-0000-4000-8000-000000000001", true
	}
	candidates := []string{seed, "canary", "a", "x1", "0", "1", "A", "abc", "https://example.test/canary",
		time.Now().UTC().Format(time.RFC3339), "v1", "canary@example.test", "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"}
	minLen, maxLen := intOf(schema["minLength"]), intOf(schema["maxLength"])
	fits := func(s string) bool {
		return len(s) >= minLen && (maxLen == 0 || len(s) <= maxLen)
	}
	pattern, _ := schema["pattern"].(string)
	var re *regexp.Regexp
	if pattern != "" {
		var err error
		if re, err = regexp.Compile(pattern); err != nil {
			return nil, false
		}
	}
	for _, s := range candidates {
		if maxLen > 0 && len(s) > maxLen && re == nil {
			s = s[:maxLen]
		}
		for len(s) < minLen && re == nil {
			s += "x"
		}
		if fits(s) && (re == nil || re.MatchString(s)) {
			return s, true
		}
	}
	return nil, false
}

func canaryNumber(schema map[string]any, integer bool) any {
	v := 1.0
	if min, ok := schema["minimum"].(float64); ok && v < min {
		v = min
	}
	if max, ok := schema["maximum"].(float64); ok && v > max {
		v = max
	}
	if integer {
		return int(v)
	}
	return v
}

func intOf(v any) int {
	if f, ok := v.(float64); ok {
		return int(f)
	}
	return 0
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > 240 {
		s = s[:240] + "..."
	}
	return s
}
