package planner

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/znasllc-io/memql/component/automations"
	workintegration "github.com/znasllc-io/memql/integrations/work"
)

// Existing sealed programs are repaired by exact edits: re-emitting an entire
// large plan can itself exhaust the model call that should repair one step.
// The DSL chooses the changes; this codec enforces unambiguous application to
// the original bytes. Compilation, prefix validation and Gate 1 still follow.
type replanSourceEdit struct {
	Find    string `json:"find"`
	Replace string `json:"replace"`
}

var replanEditsSchema = json.RawMessage(`{
 "type":"object","additionalProperties":false,
 "required":["edits","goalAlreadyServed","abandonedAssumption"],
 "properties":{
  "edits":{"type":"array","maxItems":16,"items":{
   "type":"object","additionalProperties":false,"required":["find","replace"],
   "properties":{"find":{"type":"string","minLength":1},"replace":{"type":"string"}}
  }},
  "goalAlreadyServed":{"type":"boolean"},
  "abandonedAssumption":{"type":"string"}
 }
}`)

func replanHeadline(rc workintegration.ReplanContext) string {
	for _, construct := range rc.Template {
		if construct.Kind == "automation" && construct.Name == rc.TemplateName {
			return construct.Source
		}
	}
	return ""
}

func applyReplanEdits(source string, edits []replanSourceEdit) (string, error) {
	if len(edits) == 0 || len(edits) > 16 {
		return "", fmt.Errorf("replan requires between 1 and 16 exact source edits")
	}
	type replacement struct {
		start, end int
		text       string
	}
	spans := make([]replacement, 0, len(edits))
	for i, edit := range edits {
		start := strings.Index(source, edit.Find)
		// Search one byte after the match as well, so overlapping occurrences
		// (e.g. 'aa' in 'aaa') cannot masquerade as a unique target.
		if edit.Find == "" || start < 0 || strings.Contains(source[start+1:], edit.Find) {
			return "", fmt.Errorf("replan edit %d must find exactly one original source occurrence", i+1)
		}
		spans = append(spans, replacement{start, start + len(edit.Find), edit.Replace})
	}
	sort.Slice(spans, func(i, j int) bool { return spans[i].start < spans[j].start })
	var out strings.Builder
	at := 0
	for _, span := range spans {
		if span.start < at {
			return "", fmt.Errorf("replan source edits overlap")
		}
		out.WriteString(source[at:span.start])
		out.WriteString(span.text)
		at = span.end
	}
	out.WriteString(source[at:])
	if out.String() == source {
		return "", fmt.Errorf("replan edits do not change the source")
	}
	return out.String(), nil
}

// Reusing a completed result under a changed call is as unsafe as running that
// effect again. Compare authored executable definitions (JSON excludes parsed
// expression caches) as well as the identity/order check in replanKeepsPrefix.
func replanPreservesDefinitions(original, revised *automations.Automation, rc workintegration.ReplanContext) error {
	for i, step := range original.Steps {
		status := rc.Recorded[step.ID]
		if status != "done" && status != "skipped" && !(status == "failed" && step.OnError == automations.ErrorStrategyContinue) {
			break
		}
		if i >= len(revised.Steps) {
			return fmt.Errorf("replan removed the recorded prefix step %q", step.ID)
		}
		before, err := json.Marshal(step)
		if err != nil {
			return err
		}
		after, err := json.Marshal(revised.Steps[i])
		if err != nil {
			return err
		}
		if !bytes.Equal(before, after) {
			return fmt.Errorf("replan changed the definition or position of recorded prefix step %q", step.ID)
		}
	}
	return nil
}
