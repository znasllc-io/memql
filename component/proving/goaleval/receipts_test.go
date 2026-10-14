package goaleval

import (
	"context"
	"testing"
)

func TestRunEvidenceIncludesChildrenAndRefusesIncompleteFamilies(t *testing.T) {
	for _, variant := range []string{"complete", "foreign-parent", "lost-ceiling", "still-running", "missing-child"} {
		t.Run(variant, func(t *testing.T) {
			read := func(_ context.Context, kind, id string) ([]map[string]any, error) {
				switch kind {
				case "run":
					row := map[string]any{"id": id, "status": "succeeded", "routing": map[string]any{"source": "fleet:test", "noPaidProviders": true}}
					if id == "child" {
						row["parentRunId"] = "root"
						switch variant {
						case "foreign-parent":
							row["parentRunId"] = "somebody-else"
						case "lost-ceiling":
							row["routing"] = map[string]any{"source": "fleet:test"}
						case "still-running":
							row["status"] = "running"
						case "missing-child":
							return nil, nil
						}
					}
					return []map[string]any{row}, nil
				case "steps":
					if id == "root" {
						return []map[string]any{{"childRunId": "child"}, {"childRunId": "child"}}, nil
					}
				case "modelCalls", "observations":
					return []map[string]any{{"id": kind + "-" + id}}, nil
				}
				return nil, nil
			}
			e, err := CollectRunEvidence(context.Background(), "root", "fleet:test", read)
			if variant == "complete" {
				if err != nil || len(e.Runs) != 2 || len(e.ModelCalls) != 2 || len(e.Observations) != 2 {
					t.Fatalf("incomplete evidence: %+v, %v", e, err)
				}
			} else if err == nil {
				t.Fatal("incomplete or unauthorized family accepted")
			}
		})
	}
}
