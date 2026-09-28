package release

import (
	"strings"
	"testing"
)

// versionfile_test.go -- a cut with a stale VERSION is refused (memql#5714).
//
// Every refusal here is followed by the three things a refused cut must not
// have done -- a tag, a Release, a row -- because a refusal that fired AFTER
// the tag would be the half-done state wearing a different code.

// assertNothingCreated is the shared tail of every refusal test in this file.
func assertNothingCreated(t *testing.T, f *fakeGitHub, engine *recordingEngine) {
	t.Helper()
	if len(f.createdRefs) != 0 || len(f.createdReleases) != 0 {
		t.Fatalf("a refused cut created tags %v and releases %v", f.createdRefs, f.createdReleases)
	}
	if len(engine.calls) != 0 {
		t.Fatalf("a refused cut wrote to the graph: %v", engine.calls)
	}
}

// TestCutRefusesAStaleVersionFile is the lag itself. VERSION still names the
// PREVIOUS release -- the state it was left in between v0.22.10 and v0.22.12 --
// and the cut computes the next one. Tagging anyway would publish a release
// whose tree says it is a different release.
func TestCutRefusesAStaleVersionFile(t *testing.T) {
	f := newFakeGitHub(t, []tagRef{{Name: "v0.17.1", Sha: "old"}}, "headsha1234567").withVersionFile("0.17.1\n")
	i, engine := ownerIntegration(t, f)

	_, err := i.Cut(ownerCtx(), CutRequest{Bump: "patch"})
	if got := RefusalCode(err); got != CodeVersionFileStale {
		t.Fatalf("refusal = %q, want %q (error: %v)", got, CodeVersionFileStale, err)
	}
	// Both values, so the operator knows what the file says AND what to set
	// it to without reading anything else.
	for _, want := range []string{`"0.17.1"`, "v0.17.2", "0.17.2", "headsha"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not name %s: %v", want, err)
		}
	}
	// VERSION names the release already cut, so the prepare step was skipped
	// and no bump can reach 0.17.1 again. Advice to "choose the bump that
	// reaches it" would send the operator after a release that exists.
	if !strings.Contains(err.Error(), "has not landed") {
		t.Errorf("the refusal does not say the prepare pull request is missing: %v", err)
	}
	if strings.Contains(err.Error(), "bump reaches") {
		t.Errorf("the refusal offers a bump that reaches a release already cut: %v", err)
	}
	assertNothingCreated(t, f, engine)
}

// TestStaleVersionRefusalNamesTheRemedyThatFits drives each cause the refusal
// distinguishes. The newest tag is v1.0.0 and the operator asked for a patch, so
// the cut computes v1.0.1; what VERSION says instead decides what to do.
func TestStaleVersionRefusalNamesTheRemedyThatFits(t *testing.T) {
	for _, tc := range []struct {
		name    string
		content string
		want    []string
		notWant []string
	}{
		{"the prepare step skipped: VERSION is the last tag", "1.0.0\n",
			[]string{"has not landed", "setting VERSION to 1.0.1"}, []string{"bump reaches"}},
		{"older than the last tag", "0.15.0\n",
			[]string{"has not landed", "v1.0.0"}, []string{"bump reaches"}},
		{"prepared for a minor, cut as a patch", "1.1.0\n",
			[]string{"the minor bump reaches", "cut with bump minor"}, []string{"has not landed"}},
		{"prepared for a major, cut as a patch", "2.0.0\n",
			[]string{"the major bump reaches", "cut with bump major"}, []string{"has not landed"}},
		{"newer, but no single bump reaches it", "1.5.0\n",
			[]string{"setting VERSION to 1.0.1"}, []string{"bump reaches", "has not landed"}},
		{"a leading v", "v1.0.1\n",
			[]string{"unprefixed", "setting VERSION to 1.0.1"}, []string{"bump reaches"}},
		{"not a version", "next\n",
			[]string{"setting VERSION to 1.0.1"}, []string{"bump reaches", "has not landed"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeGitHub(t, []tagRef{{Name: "v1.0.0", Sha: "old"}}, "head").withVersionFile(tc.content)
			i, engine := ownerIntegration(t, f)
			_, err := i.Cut(ownerCtx(), CutRequest{Bump: "patch"})
			if got := RefusalCode(err); got != CodeVersionFileStale {
				t.Fatalf("refusal = %q, want %q (error: %v)", got, CodeVersionFileStale, err)
			}
			for _, w := range tc.want {
				if !strings.Contains(err.Error(), w) {
					t.Errorf("the refusal does not say %q: %v", w, err)
				}
			}
			for _, nw := range tc.notWant {
				if strings.Contains(err.Error(), nw) {
					t.Errorf("the refusal says %q, which does not fit this cause: %v", nw, err)
				}
			}
			assertNothingCreated(t, f, engine)
		})
	}
}

// TestDryRunRefusesAStaleVersionFile holds the rule cut.go states: everything
// that can refuse has refused before the plan is returned. The card shows a
// dry run's plan before the operator confirms, so a plan the real cut would
// refuse is a plan that lies.
func TestDryRunRefusesAStaleVersionFile(t *testing.T) {
	f := newFakeGitHub(t, []tagRef{{Name: "v3.1.4", Sha: "old"}}, "deadbeefcafe").withVersionFile("3.1.4\n")
	i, engine := ownerIntegration(t, f)

	out, err := i.Cut(ownerCtx(), CutRequest{Bump: "minor", DryRun: true})
	if got := RefusalCode(err); got != CodeVersionFileStale {
		t.Fatalf("a dry run with a stale VERSION returned %+v, %v; want the %q refusal", out, err, CodeVersionFileStale)
	}
	assertNothingCreated(t, f, engine)
}

// TestCutRefusesAMissingVersionFile pins the 404 mapping. GetFile reads a 404
// as release_repo_unconfigured, which here would send the operator to fix a
// repository setting the same call already proved correct by listing its tags.
func TestCutRefusesAMissingVersionFile(t *testing.T) {
	f := newFakeGitHub(t, []tagRef{{Name: "v1.0.0", Sha: "old"}}, "head")
	i, engine := ownerIntegration(t, f)

	_, err := i.Cut(ownerCtx(), CutRequest{Bump: "patch"})
	if got := RefusalCode(err); got != CodeVersionFileStale {
		t.Fatalf("refusal = %q, want %q -- a missing file is not a missing repository (error: %v)",
			got, CodeVersionFileStale, err)
	}
	if !strings.Contains(err.Error(), "no VERSION file") {
		t.Errorf("the refusal does not say the file is absent: %v", err)
	}
	assertNothingCreated(t, f, engine)
}

// TestCutReadsVersionAtTheShaItTags proves the file is read at the commit the
// tag points at, not at `main`. A merge landing between the head read and the
// VERSION read would otherwise let one commit's VERSION vouch for another.
func TestCutReadsVersionAtTheShaItTags(t *testing.T) {
	f := newFakeGitHub(t, []tagRef{{Name: "v1.0.0", Sha: "old"}}, "abcdef1234567890").withVersionFile("1.0.1\n")
	i, _ := ownerIntegration(t, f)

	if _, err := i.Cut(ownerCtx(), CutRequest{Bump: "patch"}); err != nil {
		t.Fatalf("cut: %v", err)
	}
	if len(f.fileReads) != 1 || f.fileReads[0] != "VERSION@abcdef1234567890" {
		t.Fatalf("contents reads = %v, want exactly [VERSION@abcdef1234567890]", f.fileReads)
	}
	var tagged string
	for _, tag := range f.tags {
		if tag.Name == "v1.0.1" {
			tagged = tag.Sha
		}
	}
	if tagged != "abcdef1234567890" {
		t.Fatalf("the tag points at %q, not at the sha VERSION was read at", tagged)
	}
}

// TestVersionFileComparison is the comparison, case by case. The accepted
// cases are the spellings a correct file takes on disk; every refused one is
// a way a near miss could otherwise pass.
func TestVersionFileComparison(t *testing.T) {
	for _, tc := range []struct {
		name    string
		content string
		ok      bool
	}{
		{"exact", "1.0.1", true},
		{"trailing newline", "1.0.1\n", true},
		{"CRLF", "1.0.1\r\n", true},
		{"surrounding spaces", "  1.0.1  \n", true},
		{"first line decides", "1.0.1\nnotes nobody reads\n", true},
		{"the previous release", "1.0.0\n", false},
		{"the incident: a long-stale VERSION", "0.15.0\n", false},
		{"a v prefix belongs on the tag only", "v1.0.1\n", false},
		{"a pre-release suffix", "1.0.1-rc1\n", false},
		{"a prefix of a longer version", "1.0.10\n", false},
		{"empty", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeGitHub(t, []tagRef{{Name: "v1.0.0", Sha: "old"}}, "head").withVersionFile(tc.content)
			i, engine := ownerIntegration(t, f)
			_, err := i.Cut(ownerCtx(), CutRequest{Bump: "patch"})
			if tc.ok {
				if err != nil {
					t.Fatalf("VERSION %q should cut v1.0.1: %v", tc.content, err)
				}
				return
			}
			if got := RefusalCode(err); got != CodeVersionFileStale {
				t.Fatalf("VERSION %q: refusal = %q, want %q (error: %v)", tc.content, got, CodeVersionFileStale, err)
			}
			assertNothingCreated(t, f, engine)
		})
	}
}
