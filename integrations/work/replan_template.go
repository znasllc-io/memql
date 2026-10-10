package work

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/znasllc-io/memql/component/automations"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/core/id"
)

// replanTemplate reads the run's owned, sealed source rather than trusting a
// planner replica's in-memory registry. A changed or missing closure must not
// become an invitation for the model to invent the program it is repairing.
func (s *store) replanTemplate(ctx context.Context, run map[string]any) ([]memql.SandboxConstruct, error) {
	constructID := rowString(run, "templateConstructId")
	if constructID == "" {
		return installedReplanTemplate(run)
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

// Installed templates have no private authoring bundle. Resolve their source
// from this replica's DSL tree, then prove it describes the exact program the
// run admitted. A rollout must not silently substitute a newer recipe. Parsing
// here installs nothing and grants no source trust to the model's later edits.
func installedReplanTemplate(run map[string]any) ([]memql.SandboxConstruct, error) {
	name, fingerprint := rowString(run, "automationName"), rowString(run, "templateFingerprint")
	if name == "" || fingerprint == "" {
		return nil, fmt.Errorf("work: the original installed replan template has no recorded identity")
	}
	source, ok := memql.DSLConstructSource(slog.Default(), "automation", name)
	if !ok {
		return nil, fmt.Errorf("work: the original installed replan template %q is unavailable", name)
	}
	auto, err := automations.NewLoader(automations.LoaderOptions{Logger: slog.Default()}).CompileSource(source, "work-replan-installed/"+name+".memql")
	if err != nil {
		return nil, fmt.Errorf("work: read the original installed replan template: %w", err)
	}
	if auto.Name != name || auto.DefinitionFingerprint(id.NewUntracked()) != fingerprint {
		return nil, fmt.Errorf("work: the original installed replan template changed since admission")
	}
	return []memql.SandboxConstruct{{Kind: "automation", Name: name, Source: source}}, nil
}
