package pipelinesteps

import (
	"strings"
	"testing"

	pl "github.com/znasllc-io/memql/component/pipelines"
)

func TestRunnerKeepsCommandFailureWhenItsPackageWasNotProduced(t *testing.T) {
	h := newRunnerHarness(t)
	run := rtRun()
	run.Artifacts = []string{"dist"}
	rtArtifactStream(t, h, extractTestTgz(t, nil))
	h.c.script(testJobName, rtFinishingScript(testJobName, 7,
		captureKubeLine(rtAt(1100), "test assertion failed before packaging")))

	res := h.run(t, run)
	if res.Status != pl.OutcomeFailed || res.ExitCode != 7 || res.Failure != nil {
		t.Fatalf("missing output displaced the command failure: %+v", res)
	}
	if res.LogFileID == "" || len(res.ArtifactFileIDs) != 0 {
		t.Fatalf("failed command lost its log or gained a package: %+v", res)
	}
	found := false
	for _, note := range res.Notes {
		found = found || note.Code == pl.CodeArtifactMissing && strings.Contains(note.Message, "required artifacts were not captured")
	}
	if !found {
		t.Fatalf("secondary artifact failure was lost: %+v", res.Notes)
	}
	h.leftNoArchive(t)
}
