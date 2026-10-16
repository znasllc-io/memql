package goaleval

import "fmt"

// DeliveredLessonBindings joins exact Training selections to successful provider
// attempts. Selection, a failed attempt, and a configured model are insufficient.
func DeliveredLessonBindings(rows []map[string]any) []Binding {
	delivered := map[string]bool{}
	object := func(v any) map[string]any { m, _ := v.(map[string]any); return m }
	text := func(v any) string { s, _ := v.(string); return s }
	bare := func(s string) string {
		const prefix = "v1:work:observation:"
		if len(s) > len(prefix) && s[:len(prefix)] == prefix {
			return s[len(prefix):]
		}
		return s
	}
	for _, row := range rows {
		execution := object(object(row["data"])["execution"])
		call := object(execution["call"])
		if execution["kind"] != "model" || call["phase"] != "completed" || text(call["error"]) != "" {
			continue
		}
		if refs, ok := call["contextReferences"].([]any); ok {
			for _, r := range refs {
				delivered[bare(text(r))] = true
			}
		}
	}
	var out []Binding
	seen := map[string]bool{}
	for _, row := range rows {
		ref := bare(text(row["id"]))
		if !delivered[ref] {
			continue
		}
		selection := object(object(row["data"])["trainingSelection"])
		if selected, ok := selection["selected"].([]any); ok {
			for _, v := range selected {
				p := object(v)
				id, digest := text(p["lessonId"]), text(p["lessonDigest"])
				if id == "" || digest == "" || seen[id+digest] {
					continue
				}
				seen[id+digest] = true
				out = append(out, Binding{ID: id, Digest: digest, Version: fmt.Sprint(p["lessonVersion"]), Source: "observation:" + ref})
			}
		}
	}
	return out
}
