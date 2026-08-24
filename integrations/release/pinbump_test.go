package release

import (
	"os"
	"strings"
	"testing"
)

// pinbump_test.go -- the follow-on PR, and the rule that it can never fail the
// cut that opened it.

// realStackPin is the file the PR edits, read from the tree.
//
// READ RATHER THAN SYNTHESIZED, on purpose. A fabricated fixture would be a
// file shaped the way this test's author imagined stackPin.ts to be, and the
// rewrite would then be proved against that imagination. The real file carries
// a doc block, a long postmortem comment, and other mentions of the constant --
// which is exactly the environment the anchored pattern has to survive.
func realStackPin(t *testing.T) string {
	t.Helper()
	src, err := os.ReadFile("../../" + pinFile)
	if err != nil {
		t.Fatalf("could not read %s: %v.\nThis test rewrites that file's DEFAULT_STACK_TAG. "+
			"If it moved, update pinFile in pinbump.go -- the rewriter is looking in the same place.", pinFile, err)
	}
	return string(src)
}

func TestRewritePinReplacesOnlyTheDeclaration(t *testing.T) {
	src := realStackPin(t)
	if !strings.Contains(src, "DEFAULT_STACK_TAG") {
		t.Fatal("the real file does not mention DEFAULT_STACK_TAG, so this test asserts nothing")
	}
	// The real file mentions the constant several times -- in its own doc
	// block, in imageTagForVersion, and in prose. Only ONE of them is the
	// declaration, and only that one may change.
	mentionsBefore := strings.Count(src, "DEFAULT_STACK_TAG")

	updated, ok := rewritePin(src, "v9.9.9")
	if !ok {
		t.Fatal("the rewriter did not find exactly one declaration in the real file")
	}
	if !strings.Contains(updated, `export const DEFAULT_STACK_TAG = "v9.9.9";`) {
		t.Fatal("the declaration was not rewritten")
	}
	if got := strings.Count(updated, "DEFAULT_STACK_TAG"); got != mentionsBefore {
		t.Fatalf("mentions went %d -> %d; the rewrite touched something other than the declaration", mentionsBefore, got)
	}
	// And the surrounding prose is untouched. This is the property the
	// memql#4429 author asked about: their PR rewrites the comments around
	// this constant, and a rewriter anchored on prose would break on it.
	beforeLines := strings.Split(src, "\n")
	afterLines := strings.Split(updated, "\n")
	if len(beforeLines) != len(afterLines) {
		t.Fatalf("line count changed %d -> %d", len(beforeLines), len(afterLines))
	}
	changed := 0
	for idx := range beforeLines {
		if beforeLines[idx] != afterLines[idx] {
			changed++
			if !strings.Contains(afterLines[idx], "export const DEFAULT_STACK_TAG") {
				t.Errorf("a line that is not the declaration changed:\n  before: %s\n  after:  %s", beforeLines[idx], afterLines[idx])
			}
		}
	}
	if changed != 1 {
		t.Fatalf("%d lines changed, want exactly 1", changed)
	}
}

func TestRewritePinIsAnchoredAgainstCommentMentions(t *testing.T) {
	// A comment that LOOKS like the declaration must not be hit. An
	// unanchored pattern would rewrite the comment and leave the real
	// constant alone -- producing a PR that changes nothing while
	// reporting success.
	src := strings.Join([]string{
		`// Historically: export const DEFAULT_STACK_TAG = "v0.1.0";`,
		`/* export const DEFAULT_STACK_TAG = "v0.2.0"; */`,
		`export const DEFAULT_STACK_TAG = "v0.19.1";`,
	}, "\n")
	updated, ok := rewritePin(src, "v1.0.0")
	if !ok {
		t.Fatal("the anchored pattern found no declaration among the decoys")
	}
	if !strings.Contains(updated, `// Historically: export const DEFAULT_STACK_TAG = "v0.1.0";`) {
		t.Error("a line comment was rewritten")
	}
	if !strings.Contains(updated, `/* export const DEFAULT_STACK_TAG = "v0.2.0"; */`) {
		t.Error("a block comment was rewritten")
	}
	if !strings.Contains(updated, `export const DEFAULT_STACK_TAG = "v1.0.0";`) {
		t.Error("the real declaration was not rewritten")
	}
}

func TestRewritePinRefusesAnythingOtherThanExactlyOneMatch(t *testing.T) {
	// Zero means the file moved or the constant was renamed; two means
	// something is ambiguous. Guessing which occurrence was meant is worse
	// than saying so.
	if _, ok := rewritePin("export const SOMETHING_ELSE = \"v1.0.0\";", "v2.0.0"); ok {
		t.Error("a file with no declaration was rewritten anyway")
	}
	two := "export const DEFAULT_STACK_TAG = \"v1.0.0\";\nexport const DEFAULT_STACK_TAG = \"v2.0.0\";"
	if _, ok := rewritePin(two, "v3.0.0"); ok {
		t.Error("a file with two declarations was rewritten anyway")
	}
}

// pinFake builds a fakeGitHub that also serves the pin-bump endpoints.
func pinFake(t *testing.T, content string) *fakeGitHub {
	t.Helper()
	f := newFakeGitHub(t, []tagRef{{Name: "v1.0.0", Sha: "old"}}, "mainhead")
	f.pinContent = content
	return f
}

func TestPinBumpOpensAPullRequestWithTheRewrittenFile(t *testing.T) {
	f := pinFake(t, realStackPin(t))
	i, _ := ownerIntegration(t, f)
	cfg := settings{repo: repoRef{Owner: "acme", Name: "widget"}, token: "token"}
	v, _ := parseReleaseTag("v9.9.9")

	url, note := i.openPinBumpPR(t.Context(), cfg, v)
	if note != "" {
		t.Fatalf("the PR did not open: %s", note)
	}
	if url == "" {
		t.Fatal("no pull-request URL was returned")
	}
	if len(f.pinBranches) != 1 || !strings.Contains(f.pinBranches[0], "v9.9.9") {
		t.Fatalf("branches created = %v, want one naming the version", f.pinBranches)
	}
	if len(f.pinCommits) != 1 {
		t.Fatalf("commits = %d, want 1", len(f.pinCommits))
	}
	if !strings.Contains(f.pinCommits[0], `export const DEFAULT_STACK_TAG = "v9.9.9";`) {
		t.Fatal("the committed file does not carry the new pin")
	}
	// And nothing else in the file changed -- the commit is a one-line
	// diff, which is what makes it reviewable.
	if !strings.Contains(f.pinCommits[0], "Refs:") {
		t.Fatal("the committed file lost the rest of its content")
	}
	if !f.pinPROpened {
		t.Fatal("no pull request was opened")
	}
}

// TestPinBumpFailureNeverFailsTheCut is the rule the whole file is arranged
// around, checked at the level that matters: through Cut, not through
// openPinBumpPR.
//
// By the time this runs the Release is published and the build is running. A
// token scoped for cutting alone -- Contents: read/write, which is all a cut
// needs -- legitimately cannot open a PR (that needs Pull requests too).
// Reporting the shipped release as a failure would invite the operator to cut
// again, producing a second version of the same code.
//
// Every step of the follow-on is broken in turn, because they fail for
// different reasons in the field: `read` is a token without Contents scope,
// `branch` and `commit` are a read-only token, and `pr` is the common one --
// the exact scope a correct cutting token lacks.
func TestPinBumpFailureNeverFailsTheCut(t *testing.T) {
	for _, failAt := range []string{"read", "branch", "commit", "pr"} {
		t.Run("fails at "+failAt, func(t *testing.T) {
			f := pinFake(t, realStackPin(t))
			f.pinFailAt = failAt
			i, engine := ownerIntegration(t, f)

			out, err := i.Cut(ownerCtx(), CutRequest{Bump: "patch", BumpExtensionPin: true})
			if err != nil {
				t.Fatalf("a published cut was reported as failed because the follow-on PR could not open: %v", err)
			}
			if out.Status != "dispatched" || out.ReleaseURL == "" {
				t.Fatalf("the outcome does not describe the release that published: %+v", out)
			}
			if out.PinBumpNote == "" {
				t.Fatal("the failure was swallowed silently -- it must land as a note")
			}
			if out.PinBumpPrURL != "" {
				t.Fatalf("a URL was reported for a PR that did not open: %s", out.PinBumpPrURL)
			}
			// The note must be actionable: it says the release IS
			// published, so the operator does not cut again.
			if !strings.Contains(out.PinBumpNote, "published") {
				t.Errorf("the note does not say the release published, which is the thing that stops a second cut: %s", out.PinBumpNote)
			}
			// And it reaches the row, which is where an operator
			// finds it later.
			rows := engine.callsNamed("createReleaseCut")
			if len(rows) != 1 || !strings.Contains(rows[0], "pinBumpNote") {
				t.Fatalf("the note did not reach the row: %v", rows)
			}
		})
	}
}

func TestPinBumpSaysSoWhenThePinIsAlreadyCurrent(t *testing.T) {
	// Happens when a cut is retried after a PR already landed. Not a
	// failure, and not worth a pull request.
	f := pinFake(t, `export const DEFAULT_STACK_TAG = "v9.9.9";`)
	i, _ := ownerIntegration(t, f)
	cfg := settings{repo: repoRef{Owner: "acme", Name: "widget"}, token: "token"}
	v, _ := parseReleaseTag("v9.9.9")

	url, note := i.openPinBumpPR(t.Context(), cfg, v)
	if url != "" {
		t.Fatalf("a pull request was opened for a no-op change: %s", url)
	}
	if !strings.Contains(note, "already") {
		t.Fatalf("the note does not explain why nothing happened: %s", note)
	}
	if f.pinPROpened {
		t.Fatal("an empty pull request was opened")
	}
}

func TestPinBumpRefusesAFileItCannotAnchorIn(t *testing.T) {
	// The constant renamed or the file moved. A note saying so beats a
	// guess at which line was meant.
	f := pinFake(t, "export const SOMETHING_ELSE = \"v1.0.0\";\n")
	i, _ := ownerIntegration(t, f)
	cfg := settings{repo: repoRef{Owner: "acme", Name: "widget"}, token: "token"}
	v, _ := parseReleaseTag("v9.9.9")

	url, note := i.openPinBumpPR(t.Context(), cfg, v)
	if url != "" {
		t.Fatal("a pull request was opened against a file with no declaration")
	}
	if !strings.Contains(note, "DEFAULT_STACK_TAG") {
		t.Fatalf("the note does not name what it looked for: %s", note)
	}
	if len(f.pinCommits) != 0 {
		t.Fatal("a commit was made despite finding nothing to rewrite")
	}
}

func TestPinBumpIsNotAttemptedUnlessAsked(t *testing.T) {
	// The default is off. A cut that silently opened a PR would be a cut
	// that touched a second repository state nobody asked it to.
	f := pinFake(t, realStackPin(t))
	i, _ := ownerIntegration(t, f)
	out, err := i.Cut(ownerCtx(), CutRequest{Bump: "patch"})
	if err != nil {
		t.Fatalf("cut: %v", err)
	}
	if out.PinBumpPrURL != "" || out.PinBumpNote != "" {
		t.Fatalf("a pin bump happened without being asked for: %+v", out)
	}
	if f.pinPROpened || len(f.pinCommits) != 0 {
		t.Fatal("the follow-on ran without being asked for")
	}
}
