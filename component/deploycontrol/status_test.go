package deploycontrol

import "testing"

func TestMapArgoStatusSynced(t *testing.T) {
	raw := `{
      "status": {
        "sync": {"status": "Synced", "revision": "abc123def"},
        "health": {"status": "Healthy"},
        "operationState": {"phase": "Succeeded", "finishedAt": "2026-06-02T01:02:03Z"}
      }
    }`
	got, err := MapArgoStatus([]byte(raw))
	if err != nil {
		t.Fatalf("MapArgoStatus: %v", err)
	}
	if got.GetSyncStatus() != "Synced" {
		t.Errorf("SyncStatus = %q", got.GetSyncStatus())
	}
	if got.GetHealthStatus() != "Healthy" {
		t.Errorf("HealthStatus = %q", got.GetHealthStatus())
	}
	if got.GetLastSyncRevision() != "abc123def" {
		t.Errorf("LastSyncRevision = %q", got.GetLastSyncRevision())
	}
	if got.GetLastSyncAt() != "2026-06-02T01:02:03Z" {
		t.Errorf("LastSyncAt = %q", got.GetLastSyncAt())
	}
	if got.GetOutOfSync() {
		t.Error("OutOfSync should be false when Synced")
	}
}

func TestMapArgoStatusOutOfSyncDegraded(t *testing.T) {
	raw := `{
      "status": {
        "sync": {"status": "OutOfSync", "revision": "deadbeef"},
        "health": {"status": "Degraded"},
        "operationState": {"phase": "Error", "finishedAt": "2026-06-02T02:00:00Z"}
      }
    }`
	got, err := MapArgoStatus([]byte(raw))
	if err != nil {
		t.Fatalf("MapArgoStatus: %v", err)
	}
	if got.GetSyncStatus() != "OutOfSync" {
		t.Errorf("SyncStatus = %q", got.GetSyncStatus())
	}
	if got.GetHealthStatus() != "Degraded" {
		t.Errorf("HealthStatus = %q", got.GetHealthStatus())
	}
	if !got.GetOutOfSync() {
		t.Error("OutOfSync should be true when OutOfSync")
	}
}

func TestMapArgoStatusEmpty(t *testing.T) {
	got, err := MapArgoStatus(nil)
	if err != nil {
		t.Fatalf("MapArgoStatus(nil): %v", err)
	}
	if got.GetOutOfSync() {
		t.Error("empty argo status should not be OutOfSync")
	}
}

func TestMapRolloutStatusBlueGreen(t *testing.T) {
	raw := `{
      "metadata": {"name": "bff"},
      "spec": {"strategy": {"blueGreen": {}}},
      "status": {
        "phase": "Paused",
        "currentStepIndex": 1,
        "blueGreen": {"activeSelector": "abc123", "previewSelector": "def456"},
        "currentStepAnalysisRunStatus": {"status": "Running"}
      }
    }`
	got, err := MapRolloutStatus([]byte(raw))
	if err != nil {
		t.Fatalf("MapRolloutStatus: %v", err)
	}
	if got.GetName() != "bff" {
		t.Errorf("Name = %q", got.GetName())
	}
	if got.GetKind() != "bluegreen" {
		t.Errorf("Kind = %q, want bluegreen", got.GetKind())
	}
	if got.GetPhase() != "Paused" {
		t.Errorf("Phase = %q", got.GetPhase())
	}
	if got.GetActiveColor() != "abc123" || got.GetPreviewColor() != "def456" {
		t.Errorf("colors active=%q preview=%q", got.GetActiveColor(), got.GetPreviewColor())
	}
	if got.GetCurrentStep() != 1 {
		t.Errorf("CurrentStep = %d", got.GetCurrentStep())
	}
	if got.GetLatestAnalysisResult() != "Running" {
		t.Errorf("LatestAnalysisResult = %q", got.GetLatestAnalysisResult())
	}
}

func TestMapRolloutStatusCanary(t *testing.T) {
	raw := `{
      "metadata": {"name": "cognition"},
      "spec": {"strategy": {"canary": {}}},
      "status": {
        "phase": "Progressing",
        "currentStepIndex": 2,
        "canary": {"weights": {"canary": {"weight": 50}}},
        "analysis": {"status": "Successful"}
      }
    }`
	got, err := MapRolloutStatus([]byte(raw))
	if err != nil {
		t.Fatalf("MapRolloutStatus: %v", err)
	}
	if got.GetKind() != "canary" {
		t.Errorf("Kind = %q, want canary", got.GetKind())
	}
	if got.GetCanaryWeight() != 50 {
		t.Errorf("CanaryWeight = %d, want 50", got.GetCanaryWeight())
	}
	if got.GetLatestAnalysisResult() != "Successful" {
		t.Errorf("LatestAnalysisResult = %q", got.GetLatestAnalysisResult())
	}
}

func TestMapRolloutStatusNoCurrentStep(t *testing.T) {
	raw := `{"metadata": {"name": "x"}, "spec": {"strategy": {"canary": {}}}, "status": {"phase": "Healthy"}}`
	got, err := MapRolloutStatus([]byte(raw))
	if err != nil {
		t.Fatalf("MapRolloutStatus: %v", err)
	}
	if got.GetCurrentStep() != -1 {
		t.Errorf("CurrentStep = %d, want -1 when absent", got.GetCurrentStep())
	}
}

func TestMapRolloutList(t *testing.T) {
	raw := `{
      "items": [
        {"metadata": {"name": "cognition"}, "spec": {"strategy": {"canary": {}}}, "status": {"phase": "Healthy"}},
        {"metadata": {"name": "bff"}, "spec": {"strategy": {"blueGreen": {}}}, "status": {"phase": "Paused"}}
      ]
    }`
	got, err := MapRolloutList([]byte(raw))
	if err != nil {
		t.Fatalf("MapRolloutList: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d rollouts, want 2", len(got))
	}
	// Sorted by name: bff before cognition.
	if got[0].GetName() != "bff" || got[1].GetName() != "cognition" {
		t.Errorf("order = %q, %q; want bff, cognition", got[0].GetName(), got[1].GetName())
	}
}

func TestMapRolloutListSingleObject(t *testing.T) {
	raw := `{"metadata": {"name": "bff"}, "spec": {"strategy": {"blueGreen": {}}}, "status": {"phase": "Healthy"}}`
	got, err := MapRolloutList([]byte(raw))
	if err != nil {
		t.Fatalf("MapRolloutList: %v", err)
	}
	if len(got) != 1 || got[0].GetName() != "bff" {
		t.Fatalf("single-object fallback: got %+v", got)
	}
}

func TestMapGateResultPass(t *testing.T) {
	raw := `{
      "items": [
        {
          "metadata": {"name": "deploy-gate-1", "creationTimestamp": "2026-06-02T00:00:00Z"},
          "status": {
            "phase": "Successful",
            "completedAt": "2026-06-02T00:05:00Z",
            "metricResults": [
              {"name": "readyz-schema", "phase": "Successful", "message": "schema ok"},
              {"name": "authenticated-query", "phase": "Successful", "message": "query ok"}
            ]
          }
        }
      ]
    }`
	got, err := MapGateResult([]byte(raw))
	if err != nil {
		t.Fatalf("MapGateResult: %v", err)
	}
	if got.GetResult() != "pass" {
		t.Errorf("Result = %q, want pass", got.GetResult())
	}
	if len(got.GetLegs()) != 2 {
		t.Fatalf("got %d legs, want 2", len(got.GetLegs()))
	}
	if !got.GetLegs()[0].GetPassed() {
		t.Error("leg 0 should be passed")
	}
	if got.GetRanAt() != "2026-06-02T00:05:00Z" {
		t.Errorf("RanAt = %q", got.GetRanAt())
	}
}

func TestMapGateResultFailedLegPicksLatest(t *testing.T) {
	raw := `{
      "items": [
        {
          "metadata": {"name": "deploy-gate-old", "creationTimestamp": "2026-06-01T00:00:00Z"},
          "status": {"phase": "Successful", "completedAt": "2026-06-01T00:05:00Z",
            "metricResults": [{"name": "readyz-schema", "phase": "Successful"}]}
        },
        {
          "metadata": {"name": "deploy-gate-new", "creationTimestamp": "2026-06-02T00:00:00Z"},
          "status": {
            "phase": "Failed",
            "completedAt": "2026-06-02T00:05:00Z",
            "metricResults": [
              {"name": "readyz-schema", "phase": "Successful", "message": "schema ok"},
              {"name": "authenticated-query", "phase": "Failed", "message": "Unauthenticated"}
            ]
          }
        }
      ]
    }`
	got, err := MapGateResult([]byte(raw))
	if err != nil {
		t.Fatalf("MapGateResult: %v", err)
	}
	if got.GetResult() != "fail" {
		t.Errorf("Result = %q, want fail (latest run failed)", got.GetResult())
	}
	if len(got.GetLegs()) != 2 {
		t.Fatalf("got %d legs, want 2", len(got.GetLegs()))
	}
	var queryLeg *struct {
		passed bool
		detail string
	}
	for _, leg := range got.GetLegs() {
		if leg.GetName() == "authenticated-query" {
			queryLeg = &struct {
				passed bool
				detail string
			}{leg.GetPassed(), leg.GetDetail()}
		}
	}
	if queryLeg == nil {
		t.Fatal("missing authenticated-query leg")
	}
	if queryLeg.passed {
		t.Error("authenticated-query leg should not be passed")
	}
	if queryLeg.detail != "Unauthenticated" {
		t.Errorf("authenticated-query detail = %q", queryLeg.detail)
	}
}

func TestMapGateResultNoRuns(t *testing.T) {
	got, err := MapGateResult([]byte(`{"items": []}`))
	if err != nil {
		t.Fatalf("MapGateResult: %v", err)
	}
	if got.GetResult() != "unknown" {
		t.Errorf("Result = %q, want unknown", got.GetResult())
	}
}
