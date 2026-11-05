package release

import (
	"fmt"
	"strings"
	"testing"

	pl "github.com/znasllc-io/memql/component/pipelines"
)

func skippedWalk() *pl.ReleaseSourceWalk {
	return &pl.ReleaseSourceWalk{HeadSHA: strings.Repeat("e", 40), LookBack: 20, Newer: []pl.ReleaseSourceSkip{
		{SHA: strings.Repeat("e", 40), State: pl.ReleaseSourcePending, Check: "MemQL / memql / push"},
		{SHA: strings.Repeat("f", 40), State: pl.ReleaseSourceMissing, Check: "MemQL / memql / push"},
	}}
}

// The refusal says how much the walk looked at and what it found, and its code
// survives the wrapping it picks up on its way to a client.
func TestAnUntestedSourceIsRefusedWithWhatTheWalkFound(t *testing.T) {
	walk := *skippedWalk()
	walk.Newer = append(walk.Newer, pl.ReleaseSourceSkip{SHA: strings.Repeat("1", 40), State: pl.ReleaseSourceFailed, Check: "MemQL / memql / push", Conclusion: "failure"})
	refusal := untestedSource(&pl.ReleaseSourceUntestedError{Branch: "main", Walk: walk})
	wrapped := fmt.Errorf("integration.release.prepareBuild: %w", refusal)
	if RefusalCode(wrapped) != CodeReleaseSourceUntested || CodeReleaseSourceUntested != "release_source_untested" {
		t.Fatalf("code %q", RefusalCode(wrapped))
	}
	if want := "None of the newest 3 commits on main has passed its required checks: 1 running, 1 failed, 1 untested."; !strings.HasPrefix(refusal.Message, want) {
		t.Fatalf("message %q, want it to open with %q", refusal.Message, want)
	}
	one := untestedSource(&pl.ReleaseSourceUntestedError{Branch: "trunk", Walk: pl.ReleaseSourceWalk{HeadSHA: walk.HeadSHA, LookBack: 20, Newer: walk.Newer[:1]}})
	if !strings.HasPrefix(one.Message, "The newest commit on trunk has not passed its required checks (1 running).") {
		t.Fatalf("message %q", one.Message)
	}
}

// A saved selection holds the walk that chose it to the commit it released.
func TestASelectionHoldsItsWalkToTheCommitItChose(t *testing.T) {
	selection := func(walk *pl.ReleaseSourceWalk) releaseBuildSelection {
		s := releaseBuildSelection{Request: pl.ReleaseBuildRequest{ExpectedSHA: strings.Repeat("b", 40)}}
		s.Source.Walk = walk
		return s
	}
	if err := selectionWalk(selection(nil)); err != nil {
		t.Fatalf("a selection without a walk refused: %v", err)
	}
	if err := selectionWalk(selection(skippedWalk())); err != nil {
		t.Fatalf("a recorded walk refused: %v", err)
	}
	for name, alter := range map[string]func(*pl.ReleaseSourceWalk){
		"released commit passed over": func(w *pl.ReleaseSourceWalk) { w.Newer[1].SHA = strings.Repeat("b", 40) },
		"unknown state":               func(w *pl.ReleaseSourceWalk) { w.Newer[1].State = "success" },
		"longer than its look back":   func(w *pl.ReleaseSourceWalk) { w.LookBack = 1 },
		"not starting at the head":    func(w *pl.ReleaseSourceWalk) { w.HeadSHA = strings.Repeat("f", 40) },
		"not a commit":                func(w *pl.ReleaseSourceWalk) { w.Newer[1].SHA = "HEAD" },
	} {
		t.Run(name, func(t *testing.T) {
			walk := skippedWalk()
			alter(walk)
			if err := selectionWalk(selection(walk)); err == nil {
				t.Fatal("an invalid walk accepted")
			}
		})
	}
}
