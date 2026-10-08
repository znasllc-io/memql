package library

import (
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"unicode/utf8"

	workstate "github.com/znasllc-io/memql/component/work"
)

type revisionPassage struct {
	ID        string           `json:"id"`
	StartLine int              `json:"startLine"`
	EndLine   int              `json:"endLine"`
	Source    string           `json:"source"`
	Comments  []map[string]any `json:"comments"`
}
type revisionAnswer struct {
	Summary string                `json:"summary"`
	Edits   []revisionReplacement `json:"edits"`
}
type revisionReplacement struct {
	Before     string   `json:"before"`
	After      string   `json:"after"`
	Reason     string   `json:"reason"`
	CommentIDs []string `json:"commentIds"`
}

// Merge overlapping source blocks before the model sees them. Two comments
// within a paragraph must not produce competing replacements of that paragraph.
func revisionPassages(captured map[string]any) ([]revisionPassage, error) {
	raw, err := json.Marshal(captured["comments"])
	if err != nil {
		return nil, err
	}
	var comments []map[string]any
	if json.Unmarshal(raw, &comments) != nil || len(comments) == 0 || len(comments) > 100 {
		return nil, fmt.Errorf("the captured feedback is incomplete")
	}
	lines := strings.Split(strings.ReplaceAll(asString(captured["content"]), "\r\n", "\n"), "\n")
	var passages []revisionPassage
	for _, comment := range comments {
		anchor, err := validateReviewAnchor(comment["anchor"])
		if err != nil {
			return nil, err
		}
		if anchor["kind"] == "document-end" {
			passages = append(passages, revisionPassage{StartLine: len(lines), EndLine: len(lines), Comments: []map[string]any{{"id": comment["id"], "kind": "extend", "quote": "End of document", "feedback": comment["body"], "prefix": anchor["prefix"], "suffix": anchor["suffix"], "sectionPath": anchor["sectionPath"]}}})
			continue
		}
		start, _ := intArg(anchor["startLine"])
		end, _ := intArg(anchor["endLine"])
		if end > len(lines) || strings.Join(lines[start:end], "\n") != anchor["sourceQuote"] {
			return nil, fmt.Errorf("feedback no longer matches the captured source")
		}
		passages = append(passages, revisionPassage{StartLine: start, EndLine: end, Comments: []map[string]any{{"id": comment["id"], "quote": anchor["quote"], "feedback": comment["body"], "prefix": anchor["prefix"], "suffix": anchor["suffix"], "sectionPath": anchor["sectionPath"]}}})
	}
	sort.SliceStable(passages, func(a, b int) bool { return passages[a].StartLine < passages[b].StartLine })
	merged := make([]revisionPassage, 0, len(passages))
	for _, p := range passages {
		if n := len(merged); n > 0 && p.StartLine < merged[n-1].EndLine {
			prior := &merged[n-1]
			prior.EndLine = max(prior.EndLine, p.EndLine)
			prior.Comments = append(prior.Comments, p.Comments...)
		} else {
			merged = append(merged, p)
		}
	}
	for n := range merged {
		merged[n].ID = fmt.Sprintf("passage-%d", n+1)
		merged[n].Source = strings.Join(lines[merged[n].StartLine:merged[n].EndLine], "\n")
	}
	return merged, nil
}

// A selection anchors the person's intent, not the possible destinations of a
// move. Validate exact, nonoverlapping source replacements across the document;
// every changed location is included in the immutable proposal for approval.
func buildRevisionProposal(captured map[string]any, response any) (map[string]any, error) {
	var raw []byte
	if text, ok := response.(string); ok {
		raw = []byte(text)
	} else {
		var err error
		raw, err = json.Marshal(response)
		if err != nil {
			return nil, err
		}
	}
	if len(raw) > 512*1024 || !utf8.Valid(raw) {
		return nil, fmt.Errorf("the proposed edits exceed the document review limit")
	}
	var answer revisionAnswer
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&answer); err != nil {
		return nil, fmt.Errorf("AI did not return valid document edits: %w", err)
	}
	if decoder.Decode(new(any)) != io.EOF {
		return nil, fmt.Errorf("AI returned extra content after the proposed edits")
	}
	if strings.TrimSpace(answer.Summary) == "" || len(answer.Summary) > 8000 || len(answer.Edits) > 100 {
		return nil, fmt.Errorf("the proposed edits need a concise summary and at most 100 changes")
	}
	passages, err := revisionPassages(captured)
	if err != nil {
		return nil, err
	}
	allowed := map[string]bool{}
	extensions := map[string]bool{}
	for _, p := range passages {
		for _, c := range p.Comments {
			allowed[asString(c["id"])] = true
			extensions[asString(c["id"])] = c["kind"] == "extend"
		}
	}
	original := asString(captured["content"])
	normalized, offsets := normalizedRevisionSource(original)
	type located struct {
		revisionReplacement
		start, end int
	}
	replacements := make([]located, 0, len(answer.Edits))
	for _, edit := range answer.Edits {
		edit.Before = strings.ReplaceAll(edit.Before, "\r\n", "\n")
		edit.After = strings.ReplaceAll(edit.After, "\r\n", "\n")
		if (edit.Before == "" && (normalized != "" || len(answer.Edits) != 1)) || len(edit.Before) > revisionContentLimit || len(edit.After) > revisionContentLimit || strings.TrimSpace(edit.Reason) == "" || len(edit.Reason) > 4000 || len(edit.CommentIDs) == 0 {
			return nil, fmt.Errorf("each proposed edit needs an exact source, feedback reference and explanation")
		}
		if edit.Before != "" && (strings.Index(normalized, edit.Before) < 0 || strings.Index(normalized, edit.Before) != strings.LastIndex(normalized, edit.Before)) {
			return nil, fmt.Errorf("a proposed edit does not identify a unique passage in the saved document")
		}
		seen := map[string]bool{}
		extensionOnly := true
		for _, id := range edit.CommentIDs {
			if !allowed[id] || seen[id] {
				return nil, fmt.Errorf("a proposed edit references unknown or repeated feedback")
			}
			seen[id] = true
			extensionOnly = extensionOnly && extensions[id]
		}
		start := strings.Index(normalized, edit.Before)
		prefix, beforeEnd, _ := revisionChangeBounds(edit.Before, edit.After)
		if extensionOnly && (prefix != beforeEnd || strings.TrimSpace(normalized[start+beforeEnd:]) != "") {
			return nil, fmt.Errorf("an extension may only add content at the end of the document; existing content must remain unchanged")
		}
		replacements = append(replacements, located{edit, start, start + len(edit.Before)})
	}
	sort.SliceStable(replacements, func(a, b int) bool { return replacements[a].start < replacements[b].start })
	var result strings.Builder
	cursor := 0
	edits := make([]any, 0, len(replacements))
	for _, edit := range replacements {
		start, end := offsets[edit.start], offsets[edit.end]
		if start < cursor {
			return nil, fmt.Errorf("the proposed changes overlap; they cannot be applied safely")
		}
		prefix, beforeEnd, afterEnd := revisionChangeBounds(edit.Before, edit.After)
		changeStart, changeEnd := offsets[edit.start+prefix], offsets[edit.start+beforeEnd]
		replacement := edit.After[prefix:afterEnd]
		if strings.Contains(original[changeStart:changeEnd], "\r\n") || (!strings.Contains(original[changeStart:changeEnd], "\n") && strings.Contains(original, "\r\n")) {
			replacement = strings.ReplaceAll(replacement, "\n", "\r\n")
		}
		result.WriteString(original[cursor:start])
		result.WriteString(original[start:changeStart])
		result.WriteString(replacement)
		result.WriteString(original[changeEnd:end])
		cursor = end
		edits = append(edits, map[string]any{"startLine": strings.Count(normalized[:edit.start], "\n"), "endLine": strings.Count(normalized[:edit.end], "\n") + 1, "before": edit.Before, "after": edit.After, "reason": edit.Reason, "commentIds": edit.CommentIDs})
	}
	result.WriteString(original[cursor:])
	if result.Len() > revisionContentLimit {
		return nil, fmt.Errorf("the proposed document exceeds 128 KiB")
	}
	proposal := make(map[string]any, len(captured)+3)
	for k, v := range captured {
		proposal[k] = v
	}
	proposal["summary"], proposal["edits"], proposal["revisedContent"] = answer.Summary, edits, result.String()
	return proposal, nil
}

// Context only disambiguates a replacement. Copy matching context from the
// saved bytes, including mixed line endings, rather than regenerating it from
// model output. Keep boundaries on complete UTF-8 characters.
func revisionChangeBounds(before, after string) (prefix, beforeEnd, afterEnd int) {
	for prefix < len(before) && prefix < len(after) && before[prefix] == after[prefix] {
		prefix++
	}
	for prefix > 0 && ((prefix < len(before) && !utf8.RuneStart(before[prefix])) || (prefix < len(after) && !utf8.RuneStart(after[prefix]))) {
		prefix--
	}
	beforeEnd, afterEnd = len(before), len(after)
	for beforeEnd > prefix && afterEnd > prefix && before[beforeEnd-1] == after[afterEnd-1] {
		beforeEnd--
		afterEnd--
	}
	for (beforeEnd < len(before) && !utf8.RuneStart(before[beforeEnd])) || (afterEnd < len(after) && !utf8.RuneStart(after[afterEnd])) {
		beforeEnd++
		afterEnd++
	}
	return
}

// Map normalized UTF-8 byte boundaries back to the original source so Windows
// line endings outside the replacements remain byte-for-byte unchanged.
func normalizedRevisionSource(source string) (string, []int) {
	var text strings.Builder
	offsets := make([]int, 0, len(source)+1)
	for n := 0; n < len(source); n++ {
		offsets = append(offsets, n)
		if source[n] == '\r' && n+1 < len(source) && source[n+1] == '\n' {
			n++
		}
		text.WriteByte(source[n])
	}
	return text.String(), append(offsets, len(source))
}

func validateRevisionResult(captured, proposal map[string]any) error {
	raw, err := json.Marshal(proposal["edits"])
	if err != nil {
		return err
	}
	var edits []map[string]any
	if json.Unmarshal(raw, &edits) != nil {
		return fmt.Errorf("the proposed changes are incomplete")
	}
	answer := revisionAnswer{Summary: asString(proposal["summary"])}
	for _, edit := range edits {
		var ids []string
		encoded, _ := json.Marshal(edit["commentIds"])
		if json.Unmarshal(encoded, &ids) != nil {
			return fmt.Errorf("the proposed feedback references are incomplete")
		}
		answer.Edits = append(answer.Edits, revisionReplacement{Before: asString(edit["before"]), After: asString(edit["after"]), Reason: asString(edit["reason"]), CommentIDs: ids})
	}
	rebuilt, err := buildRevisionProposal(captured, answer)
	if err != nil {
		return err
	}
	if workstate.ArtifactHash(rebuilt) != workstate.ArtifactHash(proposal) {
		return fmt.Errorf("the proposed changes differ from the captured source or validated replacements")
	}
	return nil
}
