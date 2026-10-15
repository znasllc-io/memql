package work

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"unicode"
)

// SkillContext is a content snapshot, never a tool or permission grant.
type SkillContext struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	Version     int      `json:"version"`
	Source      string   `json:"source"`
	ReviewState string   `json:"reviewState"`
	Digest      string   `json:"digest"`
	Text        string   `json:"text"`
	Terms       []string `json:"terms"`
	Score       int      `json:"score"`
	Reason      string   `json:"reason"`
}

type SkillSelection struct {
	SchemaVersion int            `json:"schemaVersion"`
	QueryDigest   string         `json:"queryDigest"`
	Method        string         `json:"method"`
	Limit         int            `json:"limit"`
	MinimumScore  int            `json:"minimumScore"`
	MaxBytes      int            `json:"maxBytes"`
	Selected      []SkillContext `json:"selected"`
	Rejected      []SkillContext `json:"rejected"`
}

// SelectSkillContext ranks exact declared term matches. It is deliberately
// transparent about lexical evidence rather than pretending to semantic scores.
// The installed DSL supplies thresholds, count and context allocation.
func SelectSkillContext(query string, rows []map[string]any, limit, minimumScore, maxBytes int) (SkillSelection, error) {
	if limit < 1 || limit > 10 || minimumScore < 1 || maxBytes < 1 || maxBytes > 12000 || len(query) > 1024*1024 || len(rows) > 1000 {
		return SkillSelection{}, fmt.Errorf("invalid bounded skill selection")
	}
	queryHash := sha256.Sum256([]byte(query))
	selection := SkillSelection{SchemaVersion: 1, QueryDigest: hex.EncodeToString(queryHash[:]), Method: "declared-term-overlap-v1", Limit: limit, MinimumScore: minimumScore, MaxBytes: maxBytes, Selected: []SkillContext{}, Rejected: []SkillContext{}}
	queryTerms := termSet(query)
	var ranked []SkillContext
	for _, row := range rows {
		get := func(key string) string { s, _ := row[key].(string); return s }
		instructions, _ := row["instructions"].(map[string]any)
		body, _ := instructions["body"].(string)
		version := 0
		switch n := row["version"].(type) {
		case int:
			version = n
		case float64:
			if parsed, err := strconv.Atoi(strconv.FormatFloat(n, 'f', -1, 64)); err == nil {
				version = parsed
			}
		}
		c := SkillContext{ID: get("id"), Name: get("name"), Version: version, Source: "shared-catalog", ReviewState: get("status"), Text: body, Terms: []string{}}
		if row["reviewedBaseline"] == true {
			c.Source = "memql-baseline"
		}
		// Hash authored content, not changing counters, query decorations or
		// installation timestamps. The same release on a fresh installation
		// must identify the same instructions and capability bundle.
		content := map[string]any{"source": c.Source}
		for _, key := range []string{"id", "slug", "name", "description", "category", "tags", "domainIds", "toolSlugs", "liveSourceIds", "tier", "instructions", "scripts", "resources", "constructRefs", "effects", "version"} {
			content[key] = row[key]
		}
		raw, err := json.Marshal(content)
		if err != nil {
			return selection, err
		}
		h := sha256.Sum256(raw)
		c.Digest = hex.EncodeToString(h[:])
		if c.ID == "" || row["active"] != true || row["reviewedBaseline"] != true || c.ReviewState != "active" || version < 1 || strings.TrimSpace(body) == "" {
			c.Reason = "not-active-reviewed-versioned-content"
			c.Text = ""
			selection.Rejected = append(selection.Rejected, c)
			continue
		}
		terms := map[string]bool{}
		// Tags remain presentation metadata. Only the skill's explicitly
		// authored selection terms participate in automatic binding.
		{
			switch list := instructions["selectionTerms"].(type) {
			case []any:
				for _, x := range list {
					if s, ok := x.(string); ok {
						for t := range termSet(s) {
							terms[t] = true
						}
					}
				}
			case []string:
				for _, s := range list {
					for t := range termSet(s) {
						terms[t] = true
					}
				}
			}
		}
		for t := range terms {
			if queryTerms[t] {
				c.Terms = append(c.Terms, t)
			}
		}
		sort.Strings(c.Terms)
		c.Score = len(c.Terms)
		if c.Score < minimumScore {
			c.Reason = "below-declared-match-floor"
			c.Text = ""
			selection.Rejected = append(selection.Rejected, c)
			continue
		}
		ranked = append(ranked, c)
	}
	sort.Slice(ranked, func(i, j int) bool {
		if ranked[i].Score != ranked[j].Score {
			return ranked[i].Score > ranked[j].Score
		}
		return ranked[i].ID < ranked[j].ID
	})
	for _, c := range ranked {
		c.Reason = "matched-declared-terms"
		raw, _ := json.Marshal(c)
		if len(selection.Selected) >= limit {
			c.Reason = "selection-count-limit"
		} else if len(raw) > maxBytes {
			c.Reason = "context-allocation"
		} else {
			maxBytes -= len(raw)
			selection.Selected = append(selection.Selected, c)
			continue
		}
		c.Text = ""
		selection.Rejected = append(selection.Rejected, c)
	}
	sort.Slice(selection.Rejected, func(i, j int) bool { return selection.Rejected[i].ID < selection.Rejected[j].ID })
	return selection, nil
}

func termSet(s string) map[string]bool {
	out := map[string]bool{}
	for _, t := range strings.FieldsFunc(strings.ToLower(s), func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) }) {
		if len(t) > 1 {
			out[t] = true
		}
	}
	return out
}
