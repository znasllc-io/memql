package memql

import (
	"encoding/json"
	"sort"
	"strings"
	"time"
	_ "time/tzdata" // Clock grounding must also work in minimal engine images.
)

// Source-local wall clocks prevent a model from guessing a timezone conversion
// when deciding whether to reuse a receipt. Only explicit JSON timezone fields
// are considered; neither a place name nor prose supplies a timezone. These are
// current clocks, not rewritten observation timestamps or freshness verdicts.
func workSourceClock(sources []any, now time.Time) map[string]any {
	clocks := map[string]string{}
	seen := map[string]bool{}
	visits, candidates := 2048, 16
	var walk func(any, int)
	walk = func(value any, depth int) {
		if depth > 10 || visits <= 0 || candidates <= 0 || len(clocks) >= 8 {
			return
		}
		visits--
		switch value := value.(type) {
		case map[string]any:
			keys := make([]string, 0, len(value))
			for key := range value {
				keys = append(keys, key)
			}
			sort.Strings(keys)
			for _, key := range keys {
				if strings.EqualFold(strings.ReplaceAll(key, "_", ""), "timezone") {
					zone, ok := value[key].(string)
					if ok && len(zone) <= 80 && !seen[zone] && candidates > 0 && len(clocks) < 8 {
						seen[zone] = true
						candidates--
						// Local is the node's timezone, not a source declaration.
						// Abbreviations are ambiguous; accept UTC or named IANA zones.
						if zone == "UTC" || strings.Contains(zone, "/") {
							if location, err := time.LoadLocation(zone); err == nil {
								clocks[zone] = now.In(location).Format(time.RFC3339)
							}
						}
					}
				}
				walk(value[key], depth+1)
			}
		case []any:
			for _, child := range value {
				walk(child, depth+1)
			}
		case string:
			// Tool envelopes often contain JSON stdout as a string. Decode whole
			// JSON values only, never snippets extracted from arbitrary prose.
			text := strings.TrimSpace(value)
			if len(text) > 8000 || len(text) < 2 || (text[0] != '{' && text[0] != '[') {
				return
			}
			var nested any
			if json.Unmarshal([]byte(text), &nested) == nil {
				walk(nested, depth+1)
			}
		}
	}
	for _, source := range sources {
		if receipt, ok := source.(map[string]any); ok {
			walk(receipt["value"], 0)
		}
	}
	return map[string]any{
		"currentTimeUTC":   now.UTC().Format(time.RFC3339),
		"sourceZoneClocks": clocks,
		"note":             "Server-calculated clocks at this context read, all representing the same instant. Zone names come only from explicit timezone fields in recorded JSON source values. These clocks do not change source timestamps, establish their timezone, or prove observation freshness. Preserve the source's own timestamp and stated zone; do not invent relative ages or stale-evidence claims from unverified time arithmetic.",
	}
}
