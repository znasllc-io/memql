package compose

import (
	"context"
	"fmt"
	"math"
	"strings"
	"time"

	pure "github.com/znasllc-io/memql/component/compose"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/core/common"
)

// The DSL selects the completed steps and their order. This codec captures
// owner-scoped receipts as immutable render input; it never writes new prose.
type sectionAssembly struct {
	Body    string                   `json:"body"`
	Models  []pure.ModelContribution `json:"models"`
	Sources []pure.Source            `json:"sources"`
}

func validateSectionAssembly(a materializeArgs, rc common.RunContext, nested bool, owner string) error {
	if len(a.SectionKeys) == 0 {
		return nil
	}
	if !nested || !sameRowID(rc.OwnerUserId, owner) {
		return fmt.Errorf("compose: sectionKeys require the caller's current work run")
	}
	if len(a.SectionKeys) > 64 || a.Draft != "" || a.OutputKind != "" || a.DeployableKind != "" || a.RecipeId != "" || a.Format == pure.FormatCSV || a.Format == pure.FormatJSON {
		return fmt.Errorf("compose: sectionKeys require at most 64 completed prose sections, without another draft or output kind")
	}
	seen := map[string]bool{}
	for _, key := range a.SectionKeys {
		if strings.TrimSpace(key) == "" || key == rc.StepKey || seen[key] {
			return fmt.Errorf("compose: sectionKeys contain a blank, duplicate or current step")
		}
		seen[key] = true
	}
	return nil
}

func (i *Integration) captureSections(ctx context.Context, rc common.RunContext, keys []string) (*sectionAssembly, error) {
	out := &sectionAssembly{}
	var parts []string
	total := 0
	for _, key := range keys {
		args := map[string]any{"runId": rc.RunId, "stepKey": key}
		rows, err := i.store().query(ctx, "query "+call("workStepForOwnerRun", args))
		if err != nil {
			return nil, err
		}
		if len(rows) != 1 || rows[0]["status"] != "done" || !sameRowID(stringOf(rows[0]["ownerUserId"]), rc.OwnerUserId) || !sameRowID(stringOf(rows[0]["runId"]), rc.RunId) || rows[0]["key"] != key {
			return nil, fmt.Errorf("compose: section %q has no completed receipt in this run", key)
		}
		row := rows[0]
		result, _ := row["result"].(map[string]any)
		body, ok := result["value"].(string)
		if !ok {
			value, _ := result["value"].(map[string]any)
			body, ok = value["reply"].(string)
		}
		if !ok || strings.TrimSpace(body) == "" {
			return nil, fmt.Errorf("compose: section %q has no complete text reply", key)
		}
		if len(parts) > 0 {
			total += 2
		}
		if len(body) > (8<<20)-total {
			return nil, fmt.Errorf("compose: completed section text exceeds the 8 MiB render input limit")
		}
		total += len(body)
		parts = append(parts, body)
		at, err := time.Parse(time.RFC3339Nano, stringOf(row["finishedAt"]))
		if err != nil || stringOf(row["id"]) == "" {
			return nil, fmt.Errorf("compose: section %q has no verifiable completion identity", key)
		}
		out.Sources = append(out.Sources, pure.Source{Kind: "concept_row", Ref: memql.BareShortId(stringOf(row["id"])), Label: key, CapturedAt: at})
		receipts, err := i.store().query(ctx, "query "+call("workModelCallsForOwnerStep", args))
		if err != nil {
			return nil, err
		}
		for _, receipt := range receipts {
			if !sameRowID(stringOf(receipt["ownerUserId"]), rc.OwnerUserId) || !sameRowID(stringOf(receipt["runId"]), rc.RunId) || receipt["stepKey"] != key {
				return nil, fmt.Errorf("compose: section model receipt does not belong to this run")
			}
			if stringOf(receipt["error"]) != "" || stringOf(receipt["model"]) == "" {
				continue
			}
			tokens, output := max(0, runCountOf(receipt["inputTokens"])), max(0, runCountOf(receipt["outputTokens"]))
			if output > math.MaxInt-tokens {
				tokens = math.MaxInt
			} else {
				tokens += output
			}
			out.Models = append(out.Models, pure.ModelContribution{Provider: stringOf(receipt["provider"]), Model: stringOf(receipt["model"]), Calls: 1, Tokens: tokens})
		}
	}
	out.Body = strings.Join(parts, "\n\n")
	return out, nil
}
