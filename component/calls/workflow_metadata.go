package calls

import (
	"fmt"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

// These declarations describe a Skill; they never carry captured call values,
// URLs, credentials, executable code or permission to acquire an input/output.
type WorkflowMaterial struct {
	ID          string `json:"id"`
	Label       string `json:"label"`
	Description string `json:"description,omitempty"`
}
type WorkflowField struct {
	ID          string `json:"id"`
	Label       string `json:"label"`
	Type        string `json:"type"`
	Description string `json:"description,omitempty"`
	Required    bool   `json:"required,omitempty"`
	MaxLength   *int   `json:"maxLength,omitempty"`
}

var workflowLanguage = regexp.MustCompile(`^[A-Za-z]{2,8}(-[A-Za-z0-9]{1,8}){0,5}$`)

func workflowMetadataText(value string, maximum int, required bool) bool {
	return utf8.ValidString(value) && len(value) <= maximum && (!required || strings.TrimSpace(value) != "") && strings.IndexFunc(value, func(r rune) bool { return unicode.IsControl(r) && r != '\n' && r != '\t' }) < 0
}
func workflowMetadataIssues(d WorkflowDefinition) []WorkflowIssue {
	issues := []WorkflowIssue{}
	add := func(path, message string) {
		issues = append(issues, WorkflowIssue{Path: path, Code: "invalid_metadata", Message: message})
	}
	if len(d.Languages) > 8 {
		add("languages", "Declare at most eight language tags.")
	}
	languages := map[string]bool{}
	for i, tag := range d.Languages {
		key := strings.ToLower(tag)
		if !workflowLanguage.MatchString(tag) || languages[key] {
			add(fmt.Sprintf("languages.%d", i), "Use unique bounded language tags such as en or en-US.")
		}
		languages[key] = true
	}
	if len(d.Materials) > 16 {
		add("materials", "Declare at most sixteen materials.")
	}
	materials := map[string]bool{}
	for i, m := range d.Materials {
		if !ValidWorkflowID(m.ID) || materials[m.ID] || !workflowMetadataText(m.Label, 120, true) || !workflowMetadataText(m.Description, 1000, false) {
			add(fmt.Sprintf("materials.%d", i), "Materials need unique IDs, useful labels and bounded inert descriptions.")
		}
		materials[m.ID] = true
	}
	for _, group := range []struct {
		name   string
		fields []WorkflowField
	}{{"inputs", d.Inputs}, {"outputs", d.Outputs}} {
		if len(group.fields) > 16 {
			add(group.name, "Declare at most sixteen fields.")
		}
		ids := map[string]bool{}
		for i, f := range group.fields {
			valid := ValidWorkflowID(f.ID) && !ids[f.ID] && workflowMetadataText(f.Label, 120, true) && workflowMetadataText(f.Description, 1000, false)
			switch f.Type {
			case "text":
				valid = valid && (f.MaxLength == nil || *f.MaxLength >= 1 && *f.MaxLength <= 8000)
			case "number", "boolean":
				valid = valid && f.MaxLength == nil
			default:
				valid = false
			}
			if !valid {
				add(fmt.Sprintf("%s.%d", group.name, i), "Fields need unique IDs, bounded descriptions, a text/number/boolean type, and optional text length of 1–8000.")
			}
			ids[f.ID] = true
		}
	}
	return issues
}
