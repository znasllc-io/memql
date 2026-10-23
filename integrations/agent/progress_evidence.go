package agent

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"slices"
	"strings"
)

// A new argument map is not progress. Only returned substantive source content
// can supply this conservative automatic-read signal. Opaque exec output,
// statuses, counters, timestamps, echoed queries and model-written journal notes
// are deliberately not a generic success signal. Other effect protocols need
// their own verified native evidence or scoped owner feedback.
func sourceReadEvidence(result string) string {
	if len(result) == 0 || len(result) > 64<<10 {
		return ""
	}
	var value any
	if json.Unmarshal([]byte(result), &value) != nil {
		return ""
	}
	var facts []string
	var visit func(any, int)
	visit = func(v any, depth int) {
		if depth > 8 || len(facts) > 100 {
			return
		}
		switch row := v.(type) {
		case []any:
			for _, item := range row {
				visit(item, depth+1)
			}
		case map[string]any:
			if row["isError"] == true || row["error"] != nil {
				return
			}
			// ExecuteToolByName returns the native content envelope, whose text
			// is the JSON produced by executeResultToToolJSON. Decode that exact
			// layer; treating its serialized bytes as source prose would credit
			// wrappers and would miss the authorized rows inside them.
			if content, ok := row["content"].([]any); ok {
				for _, item := range content {
					block, ok := item.(map[string]any)
					if !ok || block["type"] != "text" {
						continue
					}
					text, _ := block["text"].(string)
					var inner any
					if json.Unmarshal([]byte(text), &inner) == nil {
						visit(inner, depth+1)
					}
				}
			}
			concept, _ := row["concept"].(string)
			if strings.HasPrefix(concept, "v1:work:") {
				return
			}
			if payload, ok := row["payload"].(map[string]any); ok {
				// Native node envelopes carry the identity outside their payload.
				if id, ok := row["id"].(string); ok && payload["id"] == nil {
					payload["id"] = id
				}
			}
			// Runtime/work journals and generated self-reports are not sources
			// that can renew their own execution allowance.
			if row["runId"] != nil || row["stepKey"] != nil || row["kind"] == "note" {
				return
			}
			id, _ := row["id"].(string)
			if id != "" && !strings.HasPrefix(id, "v1:work:") {
				for _, key := range []string{"text", "body", "description"} {
					text, _ := row[key].(string)
					text = strings.Join(strings.Fields(text), " ")
					if len(text) >= 24 {
						facts = append(facts, text)
					}
				}
			}
			for _, key := range []string{"data", "nodes", "rows", "results", "payload"} {
				if child, ok := row[key]; ok {
					visit(child, depth+1)
				}
			}
			// Shaped data uses the concept name as its row wrapper. Work rows
			// remain excluded even when their wire identity has been bare-ified.
			for key, child := range row {
				if strings.HasPrefix(key, "v1:") && !strings.HasPrefix(key, "v1:work:") {
					visit(child, depth+1)
				}
			}
		}
	}
	visit(value, 0)
	if len(facts) == 0 || len(facts) > 100 {
		return ""
	}
	// The set of actual facts, independent of row order, source aliases or
	// pagination metadata. A repeated equivalent answer cannot buy a window.
	return sourceFactDigest(facts)
}

func sourceFactDigest(facts []string) string {
	set := map[string]bool{}
	for _, fact := range facts {
		digest := sha256.Sum256([]byte(fact))
		set[hex.EncodeToString(digest[:])] = true
	}
	keys := make([]string, 0, len(set))
	for key := range set {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	raw, _ := json.Marshal(keys)
	return string(raw)
}
