package pipelines

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func walkCommit(c byte) string { return strings.Repeat(string(c), 40) }

func recordedWalk() ReleaseSourceWalk {
	return ReleaseSourceWalk{HeadSHA: walkCommit('a'), LookBack: 20, Newer: []ReleaseSourceSkip{
		{SHA: walkCommit('a'), State: ReleaseSourcePending, Check: "MemQL / memql / push"},
		{SHA: walkCommit('b'), State: ReleaseSourceFailed, Check: "MemQL / memql / push", Conclusion: "failure"},
		{SHA: walkCommit('c'), State: ReleaseSourceCancelled, Check: "MemQL / memql / push", Conclusion: "cancelled"},
		{SHA: walkCommit('d'), State: ReleaseSourceMissing, Check: "MemQL / memql / push"},
	}}
}

// A source with no walk -- every release saved before the walk existed, and
// every release of a head that passed -- must encode exactly as it did before
// the field existed: its selection digest and the strict decoders of older
// engines both read these bytes.
func TestAReleaseSourceWithoutAWalkEncodesAsBefore(t *testing.T) {
	source := ReleaseBuildSource{OwnerUserID: "owner", SHA: walkCommit('e'), Checks: []ReleaseBuildCheck{{Context: "ci", AppID: 1}}}
	encoded, err := json.Marshal(source)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "Walk") {
		t.Fatalf("a source without a walk names one: %s", encoded)
	}
	walk := recordedWalk()
	source.Walk = &walk
	if encoded, _ = json.Marshal(source); !strings.Contains(string(encoded), `"Walk":{"HeadSHA":"`) {
		t.Fatalf("a walk is not recorded under its Go name: %s", encoded)
	}
}

func TestAReleaseSourceWalkIsCheckedAgainstWhatItChose(t *testing.T) {
	selected := walkCommit('e')
	if err := recordedWalk().Validate(selected); err != nil {
		t.Fatalf("a recorded walk refused: %v", err)
	}
	for _, tc := range []struct {
		name     string
		selected string
		alter    func(*ReleaseSourceWalk)
	}{
		{"selected among the skipped", walkCommit('c'), nil},
		{"selected is the head", walkCommit('a'), nil},
		{"selected is not a commit", "main", nil},
		{"head is not a commit", selected, func(w *ReleaseSourceWalk) { w.HeadSHA = "HEAD" }},
		{"walk does not start at the head", selected, func(w *ReleaseSourceWalk) { w.Newer = w.Newer[1:] }},
		{"nothing skipped", selected, func(w *ReleaseSourceWalk) { w.Newer = nil }},
		{"longer than its look back", selected, func(w *ReleaseSourceWalk) { w.LookBack = 3 }},
		{"chose a commit past its look back", selected, func(w *ReleaseSourceWalk) { w.LookBack = 4 }},
		{"no look back", selected, func(w *ReleaseSourceWalk) { w.LookBack = 0 }},
		{"look back beyond any walk", selected, func(w *ReleaseSourceWalk) { w.LookBack = MaxReleaseSourceLookBack + 1 }},
		{"repeated commit", selected, func(w *ReleaseSourceWalk) { w.Newer[2].SHA = walkCommit('b') }},
		{"upper-case commit", selected, func(w *ReleaseSourceWalk) { w.Newer[1].SHA = strings.Repeat("B", 40) }},
		{"unknown state", selected, func(w *ReleaseSourceWalk) { w.Newer[1].State = "green" }},
		{"passed state", selected, func(w *ReleaseSourceWalk) { w.Newer[1].State = "success" }},
		{"no check named", selected, func(w *ReleaseSourceWalk) { w.Newer[3].Check = "" }},
		{"conclusion on a running check", selected, func(w *ReleaseSourceWalk) { w.Newer[0].Conclusion = "failure" }},
		{"failed with success", selected, func(w *ReleaseSourceWalk) { w.Newer[1].Conclusion = "success" }},
		{"cancelled with another conclusion", selected, func(w *ReleaseSourceWalk) { w.Newer[2].Conclusion = "failure" }},
		{"oversized check", selected, func(w *ReleaseSourceWalk) { w.Newer[0].Check = strings.Repeat("x", 300) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			walk := recordedWalk()
			if tc.alter != nil {
				tc.alter(&walk)
			}
			if err := walk.Validate(tc.selected); err == nil {
				t.Fatalf("an invalid walk accepted: %+v", walk)
			}
		})
	}
	// A walk that chose nothing names nothing it chose, and may have examined
	// its whole look back.
	empty := recordedWalk()
	empty.LookBack = len(empty.Newer)
	if err := empty.Validate(""); err != nil {
		t.Fatalf("an empty-handed walk refused: %v", err)
	}
}

func TestAnUntestedSourceSaysWhatTheWalkFound(t *testing.T) {
	walk := recordedWalk()
	walk.Newer = append(walk.Newer, ReleaseSourceSkip{SHA: walkCommit('f'), State: ReleaseSourceMissing, Check: "MemQL / memql / push"},
		ReleaseSourceSkip{SHA: walkCommit('1'), State: ReleaseSourcePending, Check: "MemQL / memql / push"})
	if got := walk.Summary(); got != "2 running, 1 failed, 1 superseded, 2 untested" {
		t.Fatalf("summary %q", got)
	}
	var err error = &ReleaseSourceUntestedError{Branch: "main", Walk: walk}
	var untested *ReleaseSourceUntestedError
	if !errors.As(err, &untested) || untested.Walk.HeadSHA != walk.HeadSHA || !strings.Contains(err.Error(), "6 commits on main") || !strings.Contains(err.Error(), walk.Summary()) {
		t.Fatalf("untested error: %v", err)
	}
	one := ReleaseSourceWalk{HeadSHA: walkCommit('a'), LookBack: 20, Newer: walk.Newer[:1]}
	if got := one.Summary(); got != "1 running" {
		t.Fatalf("summary %q", got)
	}
}
