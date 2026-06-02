package deploycontrol

import (
	"encoding/json"
	"fmt"
	"sort"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
)

// This file holds the pure mappers from raw kubectl `-o json` bytes
// into the proto status types. They take []byte (never a live
// cluster) so they are fully unit-testable with fixture JSON.

// -----------------------------------------------------------------------------
// Argo CD application status
// -----------------------------------------------------------------------------

// argoAppJSON mirrors the slice of `kubectl -n argocd get app memql
// -o json` we consume.
type argoAppJSON struct {
	Status struct {
		Sync struct {
			Status   string `json:"status"`
			Revision string `json:"revision"`
		} `json:"sync"`
		Health struct {
			Status string `json:"status"`
		} `json:"health"`
		OperationState struct {
			Phase      string `json:"phase"`
			FinishedAt string `json:"finishedAt"`
		} `json:"operationState"`
	} `json:"status"`
}

// MapArgoStatus maps the raw `kubectl -n argocd get app <name> -o
// json` bytes into the ArgoStatus proto. An empty/nil input yields a
// zero-value status (not an error) so a missing Argo app surfaces as
// "unknown" rather than failing the whole status call.
func MapArgoStatus(raw []byte) (*memqlv1.ArgoStatus, error) {
	if len(raw) == 0 {
		return &memqlv1.ArgoStatus{}, nil
	}
	var app argoAppJSON
	if err := json.Unmarshal(raw, &app); err != nil {
		return nil, fmt.Errorf("deploycontrol: parse argo app json: %w", err)
	}
	syncStatus := app.Status.Sync.Status
	return &memqlv1.ArgoStatus{
		SyncStatus:       syncStatus,
		HealthStatus:     app.Status.Health.Status,
		LastSyncRevision: app.Status.Sync.Revision,
		LastSyncAt:       app.Status.OperationState.FinishedAt,
		OutOfSync:        syncStatus != "" && syncStatus != "Synced",
	}, nil
}

// -----------------------------------------------------------------------------
// Argo Rollouts
// -----------------------------------------------------------------------------

// rolloutJSON mirrors the slice of `kubectl -n memql get rollout
// <name> -o json` we consume.
type rolloutJSON struct {
	Metadata struct {
		Name string `json:"name"`
	} `json:"metadata"`
	Spec struct {
		Strategy struct {
			BlueGreen *struct{} `json:"blueGreen"`
			Canary    *struct{} `json:"canary"`
		} `json:"strategy"`
	} `json:"spec"`
	Status struct {
		Phase            string `json:"phase"`
		CurrentStepIndex *int32 `json:"currentStepIndex"`
		BlueGreen        struct {
			ActiveSelector  string `json:"activeSelector"`
			PreviewSelector string `json:"previewSelector"`
		} `json:"blueGreen"`
		Canary struct {
			Weights struct {
				Canary struct {
					Weight int32 `json:"weight"`
				} `json:"canary"`
			} `json:"weights"`
		} `json:"canary"`
		Analysis struct {
			Status string `json:"status"`
		} `json:"analysis"`
		CurrentStepAnalysisRunStatus struct {
			Status string `json:"status"`
		} `json:"currentStepAnalysisRunStatus"`
	} `json:"status"`
}

// MapRolloutStatus maps one rollout's raw JSON into a RolloutStatus
// proto. kind is derived from spec.strategy (blueGreen vs canary).
func MapRolloutStatus(raw []byte) (*memqlv1.RolloutStatus, error) {
	if len(raw) == 0 {
		return nil, fmt.Errorf("deploycontrol: empty rollout json")
	}
	var r rolloutJSON
	if err := json.Unmarshal(raw, &r); err != nil {
		return nil, fmt.Errorf("deploycontrol: parse rollout json: %w", err)
	}

	kind := ""
	switch {
	case r.Spec.Strategy.BlueGreen != nil:
		kind = "bluegreen"
	case r.Spec.Strategy.Canary != nil:
		kind = "canary"
	}

	step := int32(-1)
	if r.Status.CurrentStepIndex != nil {
		step = *r.Status.CurrentStepIndex
	}

	// The latest analysis status lives in one of two places depending
	// on the controller version / strategy; prefer the per-step run.
	analysis := r.Status.CurrentStepAnalysisRunStatus.Status
	if analysis == "" {
		analysis = r.Status.Analysis.Status
	}

	return &memqlv1.RolloutStatus{
		Name:                 r.Metadata.Name,
		Kind:                 kind,
		Phase:                r.Status.Phase,
		ActiveColor:          r.Status.BlueGreen.ActiveSelector,
		PreviewColor:         r.Status.BlueGreen.PreviewSelector,
		CanaryWeight:         r.Status.Canary.Weights.Canary.Weight,
		CurrentStep:          step,
		LatestAnalysisResult: analysis,
	}, nil
}

// rolloutListJSON wraps a `kubectl get rollout -o json` list response.
type rolloutListJSON struct {
	Items []json.RawMessage `json:"items"`
}

// MapRolloutList maps a `kubectl -n memql get rollout -o json` LIST
// response (with a top-level items[] array) into RolloutStatus protos.
// A single-object response (no items[]) is also accepted and mapped as
// one entry.
func MapRolloutList(raw []byte) ([]*memqlv1.RolloutStatus, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var list rolloutListJSON
	if err := json.Unmarshal(raw, &list); err != nil {
		return nil, fmt.Errorf("deploycontrol: parse rollout list json: %w", err)
	}
	// Single-object fallback: no items[] -> treat the whole doc as one.
	if len(list.Items) == 0 {
		one, err := MapRolloutStatus(raw)
		if err != nil {
			return nil, err
		}
		if one.GetName() == "" {
			return nil, nil
		}
		return []*memqlv1.RolloutStatus{one}, nil
	}

	out := make([]*memqlv1.RolloutStatus, 0, len(list.Items))
	for _, item := range list.Items {
		rs, err := MapRolloutStatus(item)
		if err != nil {
			return nil, err
		}
		out = append(out, rs)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].GetName() < out[j].GetName() })
	return out, nil
}

// -----------------------------------------------------------------------------
// Deploy gate (AnalysisRun objects)
// -----------------------------------------------------------------------------

// analysisRunListJSON mirrors `kubectl -n memql get analysisrun -o
// json`. We surface the most-recent run's per-metric leg results.
type analysisRunListJSON struct {
	Items []analysisRunJSON `json:"items"`
}

type analysisRunJSON struct {
	Metadata struct {
		Name              string `json:"name"`
		CreationTimestamp string `json:"creationTimestamp"`
	} `json:"metadata"`
	Status struct {
		Phase         string `json:"phase"`
		CompletedAt   string `json:"completedAt"`
		MetricResults []struct {
			Name    string `json:"name"`
			Phase   string `json:"phase"`
			Message string `json:"message"`
		} `json:"metricResults"`
	} `json:"status"`
}

// MapGateResult maps `kubectl -n memql get analysisrun -o json` into a
// GateResult proto. It picks the most-recently-created AnalysisRun and
// maps each metricResult to a GateLeg. Overall result is "pass" when
// the run's phase is Successful, "fail" for Failed/Error/Inconclusive,
// and "unknown" otherwise (Running / no runs).
func MapGateResult(raw []byte) (*memqlv1.GateResult, error) {
	if len(raw) == 0 {
		return &memqlv1.GateResult{Result: "unknown"}, nil
	}
	var list analysisRunListJSON
	if err := json.Unmarshal(raw, &list); err != nil {
		return nil, fmt.Errorf("deploycontrol: parse analysisrun list json: %w", err)
	}
	if len(list.Items) == 0 {
		return &memqlv1.GateResult{Result: "unknown"}, nil
	}

	// Newest by creationTimestamp (RFC3339 sorts lexicographically).
	latest := list.Items[0]
	for _, item := range list.Items[1:] {
		if item.Metadata.CreationTimestamp > latest.Metadata.CreationTimestamp {
			latest = item
		}
	}

	legs := make([]*memqlv1.GateLeg, 0, len(latest.Status.MetricResults))
	for _, m := range latest.Status.MetricResults {
		legs = append(legs, &memqlv1.GateLeg{
			Name:   m.Name,
			Passed: m.Phase == "Successful",
			Detail: gateLegDetail(m.Phase, m.Message),
		})
	}

	return &memqlv1.GateResult{
		Result: gateResultFromPhase(latest.Status.Phase),
		Legs:   legs,
		RanAt:  latest.Status.CompletedAt,
	}, nil
}

func gateResultFromPhase(phase string) string {
	switch phase {
	case "Successful":
		return "pass"
	case "Failed", "Error", "Inconclusive":
		return "fail"
	default:
		return "unknown"
	}
}

func gateLegDetail(phase, message string) string {
	if message != "" {
		return message
	}
	return phase
}
