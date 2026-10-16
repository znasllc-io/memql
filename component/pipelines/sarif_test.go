package pipelines

import (
	"strings"
	"testing"
)

func reportSpec() *Spec {
	return &Spec{Image: "toolchain", Caches: []string{"go"}, Stages: []StageSpec{
		{Name: "analysis", Steps: []StepSpec{{Name: "go", Run: "scan", Artifacts: []string{".reports"}}}},
		{Name: "reports", RunAfterFailure: true, Steps: []StepSpec{{Name: "go", SARIF: &SARIFReport{Step: "analysis.go", Path: ".reports/go.sarif"}}}},
	}}
}

func TestSARIFCompilationBindsProducerWithoutWorkerAuthority(t *testing.T) {
	spec := reportSpec()
	plan, refusal := Compile(spec, CompileInput{Mode: ModeFull, Event: EventPush, Compute: ComputeCluster})
	if refusal != nil {
		t.Fatal(refusal)
	}
	r := plan.Steps()[1]
	if r.Kind != StepSARIF || r.Image != "" || r.Run != "" || len(r.Caches) != 0 || !r.RunAfterFailure || r.RequiresFleet() {
		t.Fatalf("report acquired a worker runtime: %+v", r)
	}
	spec.Stages[1].Steps[0].SARIF.Path = "different.sarif"
	if r.SARIF.Path != ".reports/go.sarif" {
		t.Fatal("compiled publication aliases editable manifest")
	}
}

func TestSARIFCompilationRefusesAmbiguousOrExcludedSources(t *testing.T) {
	for name, change := range map[string]func(*Spec){
		"outside":          func(s *Spec) { s.Stages[1].Steps[0].SARIF.Path = "../outside.sarif" },
		"glob":             func(s *Spec) { s.Stages[1].Steps[0].SARIF.Path = "*.sarif" },
		"not sarif":        func(s *Spec) { s.Stages[1].Steps[0].SARIF.Path = "report.json" },
		"missing producer": func(s *Spec) { s.Stages[1].Steps[0].SARIF.Step = "other.scan" },
		"self":             func(s *Spec) { s.Stages[1].Steps[0].SARIF.Step = "reports.go" },
		"sharded":          func(s *Spec) { s.Stages[0].Steps[0].Shards = 2 },
		"no artifacts":     func(s *Spec) { s.Stages[0].Steps[0].Artifacts = nil },
		"shell":            func(s *Spec) { s.Stages[1].Steps[0].Run = "publish" },
		"credentials":      func(s *Spec) { s.Stages[1].Steps[0].Secrets = []string{"TOKEN"} },
		"fleet":            func(s *Spec) { s.Stages[1].Steps[0].Placement = PlacementFleet },
		"unbounded wait":   func(s *Spec) { s.Stages[1].Steps[0].Timeout = "1h" },
	} {
		t.Run(name, func(t *testing.T) {
			s := reportSpec()
			change(s)
			if Validate(s) == nil {
				t.Fatal("unsafe report admitted")
			}
		})
	}
	s := reportSpec()
	if _, refusal := Compile(s, CompileInput{StageSelection: &StageSelection{Included: []string{"reports"}}}); refusal == nil {
		t.Fatal("report compiled without its producer")
	}
}

func TestScannerReceiptIdentitySurvivesEnclosingRunCompletion(t *testing.T) {
	r, s := releaseWorkFixture()
	var digest string
	for _, status := range []string{"running", "failed", "succeeded"} {
		r["status"] = status
		got, err := InspectScannerReceipt("owner", "run", "test.full", r, s)
		if err != nil {
			t.Fatal(err)
		}
		if digest != "" && got.ReceiptDigest != digest {
			t.Fatal("run conclusion changed publication identity")
		}
		digest = got.ReceiptDigest
		if status != "succeeded" {
			if _, err := InspectReleaseWorkReceipt("owner", "run", "test.full", r, s); err == nil {
				t.Fatal("scanner admission weakened release admission")
			}
		}
	}
	for _, status := range []string{"queued", "cancelled", "unknown"} {
		r["status"] = status
		if _, err := InspectScannerReceipt("owner", "run", "test.full", r, s); err == nil {
			t.Fatal("unsafe scanner run admitted", status)
		}
	}
}

func TestReleaseReportReceiptRequiresConfirmedIngestion(t *testing.T) {
	r, s := releaseWorkFixture()
	s["call"].(map[string]any)["pipelineKind"] = "sarif"
	result := map[string]any{"status": "succeeded", "publicationId": strings.Repeat("c", 64), "sha256": strings.Repeat("d", 64), "uploadId": "upload-42", "ingestionStatus": "complete"}
	s["result"] = result
	if _, err := InspectReleaseStepReceipt("owner", "run", "test.full", r, s); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"publicationId", "sha256", "uploadId", "ingestionStatus"} {
		before := result[key]
		result[key] = ""
		if _, err := InspectReleaseStepReceipt("owner", "run", "test.full", r, s); err == nil {
			t.Fatal("missing ingestion evidence admitted", key)
		}
		result[key] = before
	}
	if _, err := InspectScannerReceipt("owner", "run", "test.full", r, s); err == nil {
		t.Fatal("report delivery became scanner execution")
	}
}
