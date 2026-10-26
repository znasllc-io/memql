package pipelinerun

import (
	"testing"

	pl "github.com/znasllc-io/memql/component/pipelines"
)

func TestImageVerificationJournalPreservesNonPublicationDecision(t *testing.T) {
	for _, only := range []bool{false, true} {
		driver := runDriver{tracks: []*stepTrack{{step: pl.Step{Key: "tests.image", Stage: "tests", Name: "image", Kind: pl.StepCommand, ImageBuild: &pl.ImageBuild{VerificationOnly: only}}, definition: "work-definition-v2:fixture"}}}
		declarations := driver.decls()
		flag, present := declarations[0].Call["imageVerificationOnly"]
		if present != only || (only && flag != true) {
			t.Fatalf("nonpublication evidence lost: %+v", declarations)
		}
	}
}
