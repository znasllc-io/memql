package work

import (
	"context"
	"fmt"
	"strings"

	"github.com/znasllc-io/memql/component/memql"
)

// replanTemplate reads the run's owned, sealed source rather than trusting a
// planner replica's in-memory registry. A changed or missing closure must not
// become an invitation for the model to invent the program it is repairing.
func (s *store) replanTemplate(ctx context.Context, run map[string]any) ([]memql.SandboxConstruct, error) {
	constructID := rowString(run, "templateConstructId")
	if constructID == "" {
		return nil, nil // Installed templates have no private source bundle.
	}
	headline, err := one(s.query(ctx, "query "+call("authoringConstructById", map[string]any{"constructId": constructID})))
	if err != nil {
		return nil, err
	}
	if headline == nil || rowString(headline, "kind") != "automation" || rowString(headline, "name") != rowString(run, "automationName") || rowString(headline, "bundleId") == "" {
		return nil, fmt.Errorf("work: the original replan template is not readable as its owner")
	}
	rows, err := s.query(ctx, "query "+call("authoringConstructsForBundle", map[string]any{"bundleId": rowString(headline, "bundleId")}))
	if err != nil {
		return nil, err
	}
	var source []memql.SandboxConstruct
	hasHeadline := false
	for _, row := range rows {
		// Source is sealed byte-for-byte, including its trailing newline.
		// rowString trims display/identity fields and must not read source.
		text, _ := row["source"].(string)
		if strings.TrimSpace(text) == "" || rowString(row, "status") == "retired" {
			return nil, fmt.Errorf("work: the original replan template has missing or retired source")
		}
		source = append(source, memql.SandboxConstruct{Kind: rowString(row, "kind"), Name: rowString(row, "name"), Source: text})
		hasHeadline = hasHeadline || (rowString(row, "kind") == "automation" && rowString(row, "name") == rowString(run, "automationName"))
	}
	if !hasHeadline || (rowString(run, "templateVersion") != "" && memql.WorkBundleVersion(source) != rowString(run, "templateVersion")) {
		return nil, fmt.Errorf("work: the original replan template source changed since compilation")
	}
	return source, nil
}
