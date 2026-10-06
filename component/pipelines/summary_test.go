package pipelines

import (
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"
)

// The check run is the one surface of a pipeline a person reads on GitHub.
// These goldens ARE its copy: a change to a word shows up here, where it can
// be read in place.

const (
	goldenPRHead = "c5fa05f142af4f7c6bd9b18c24924ebed165a4ab"
	goldenMerged = "a944ae33e9bcd7b2f5d1e5d74dcd8acc6a0ae9df"
)

var failingTail = "--- FAIL: TestCartBadge (0.01s)\n" +
	"    badge_test.go:42: got 2, want 3\n" +
	"FAIL\n" +
	"FAIL\texample.test/shop/cart\t0.311s\n"

// A push run that failed in its second stage: the stage after it never ran.
func failedPushRun() RunReport {
	return RunReport{
		Pipeline: "storefront", Mode: ModeFull, Event: EventPush, SHA: goldenMerged,
		Status: "completed", Conclusion: "failure",
		Steps: []StepReport{
			{Key: "checks.build-vet", Stage: "checks", Name: "build-vet", Status: "succeeded", DurationMs: 62_000},
			{Key: "tests.go-tests#1", Stage: "tests", Name: "go-tests", Status: "succeeded", DurationMs: 251_000},
			{Key: "tests.go-tests#2", Stage: "tests", Name: "go-tests", Status: "failed", DurationMs: 245_000,
				Message: "The command exited with status 1.", LogTail: failingTail},
			{Key: "tests.os-checks", Stage: "tests", Name: "os-checks", Status: "skipped",
				Code: CodeNotAffected, Message: "No change under bucket os."},
			{Key: "deploy.verify-rollout", Stage: "deploy", Name: "verify-rollout", Status: "skipped",
				Code: CodeStageBlocked, Message: "An earlier stage failed."},
		},
	}
}

func TestCheckOutputOfAFailedRun(t *testing.T) {
	title, summary, text := CheckOutput(failedPushRun())
	wantTitle := "Failed at tests: tests.go-tests#2"
	wantSummary := "Mode full · Push to the default branch · Commit a944ae3\n" +
		"\n" +
		"| Stage | Status | Steps | Time |\n" +
		"| --- | --- | --- | --- |\n" +
		"| checks | Passed | 1 passed | 1m 02s |\n" +
		"| tests | Failed | 1 passed, 1 failed, 1 skipped | 4m 11s |\n" +
		"| deploy | Not run: an earlier stage failed | 1 not run | - |\n" +
		"\n" +
		"### Failed steps\n" +
		"\n" +
		"- `tests.go-tests#2`: The command exited with status 1.\n"
	wantText := "### tests.go-tests#2\n" +
		"\n" +
		"```text\n" +
		failingTail +
		"```\n"
	assertOutput(t, title, summary, text, wantTitle, wantSummary, wantText)
}

func TestCheckOutputOfARunningRun(t *testing.T) {
	r := RunReport{
		Pipeline: "storefront", Mode: ModeAffected, Event: EventPullRequest, SHA: goldenPRHead, PullRequest: 42,
		Status: "in_progress",
		Steps: []StepReport{
			{Key: "checks.build-vet", Stage: "checks", Status: "succeeded", DurationMs: 62_000},
			{Key: "tests.go-tests#1", Stage: "tests", Status: "running"},
			{Key: "tests.go-tests#2", Stage: "tests", Status: "succeeded", DurationMs: 120_000},
			// Skipped by the compile, before the stage began.
			{Key: "tests.os-checks", Stage: "tests", Status: "skipped", Code: CodeNotAffected, Message: "No change under bucket os."},
			{Key: "package.image", Stage: "package", Status: "pending"},
			{Key: "package.docs", Stage: "package", Status: "skipped", Code: CodeNotAffected, Message: "No change under bucket docs."},
		},
	}
	title, summary, text := CheckOutput(r)
	wantSummary := "Mode affected · Pull request #42 · Commit c5fa05f\n" +
		"\n" +
		"| Stage | Status | Steps | Time |\n" +
		"| --- | --- | --- | --- |\n" +
		"| checks | Passed | 1 passed | 1m 02s |\n" +
		"| tests | Running | 1 passed, 1 skipped, 1 running | - |\n" +
		// A compile-time skip does not make a stage that has not begun
		// read as running.
		"| package | Waiting | 1 skipped, 1 waiting | - |\n"
	assertOutput(t, title, summary, text, "Running: tests", wantSummary, "")
}

func TestCheckOutputOfAPassedRun(t *testing.T) {
	r := RunReport{
		Pipeline: "storefront", Mode: ModeFull, Event: EventMergeGroup, SHA: goldenMerged,
		Status: "completed", Conclusion: "success",
		Steps: []StepReport{
			{Key: "checks.build-vet", Stage: "checks", Status: "succeeded", DurationMs: 62_000},
			{Key: "tests.go-tests#1", Stage: "tests", Status: "succeeded", DurationMs: 251_000},
			{Key: "tests.go-tests#2", Stage: "tests", Status: "succeeded", DurationMs: 245_000},
			{Key: "tests.os-checks", Stage: "tests", Status: "succeeded", DurationMs: 180_000},
			{Key: "release.notes", Stage: "release", Status: "succeeded", DurationMs: 45_000},
			{Key: "docs.publish", Stage: "docs", Status: "skipped", Code: CodePassedEarlier,
				Message: "Passed in attempt 1."},
		},
	}
	title, summary, text := CheckOutput(r)
	wantSummary := "Mode full · Merge queue · Commit a944ae3\n" +
		"\n" +
		"| Stage | Status | Steps | Time |\n" +
		"| --- | --- | --- | --- |\n" +
		"| checks | Passed | 1 passed | 1m 02s |\n" +
		"| tests | Passed | 3 passed | 4m 11s |\n" +
		"| release | Passed | 1 passed | 45s |\n" +
		"| docs | Skipped: Passed in attempt 1. | 1 skipped | - |\n"
	// Three stages ran, in 1m 02s + 4m 11s + 45s; a stage skipped whole is in
	// the table and not in the count.
	assertOutput(t, title, summary, text, "Passed: 3 stages in 5m 58s", wantSummary, "")
}

// D6 and decision 15: a fork's run is refused with a failure conclusion, and
// the check run says what to do instead.
func TestCheckOutputOfARefusedFork(t *testing.T) {
	r := RunReport{
		Pipeline: "storefront", Mode: ModeAffected, Event: EventPullRequest, PullRequest: 43,
		SHA: "466b2bf6caf80eca5c375e0ea27c0687ad2b74d1", Status: "completed", Conclusion: "refused",
		Refusal: &Refusal{Code: CodeForkRefused,
			Detail: "The head of pull request #43 is in stranger/Storefront, not in this repository."},
	}
	title, summary, text := CheckOutput(r)
	wantSummary := "Mode affected · Pull request #43 · Commit 466b2bf\n" +
		"\n" +
		"The head of pull request #43 is in stranger/Storefront, not in this repository (`pipeline_fork_refused`).\n" +
		"\n" +
		"Checks run only on branches in this repository. Push the branch here to run them.\n"
	assertOutput(t, title, summary, text, "Refused: pull request from a fork", wantSummary, "")
}

// D9: a manifest that cannot compile is a failed run carrying a typed
// refusal. It renders the refusal, its scope and its remedy, and no table.
func TestCheckOutputOfAManifestThatCannotCompile(t *testing.T) {
	r := RunReport{
		Pipeline: "storefront", Mode: ModeAffected, Event: EventPullRequest, PullRequest: 42, SHA: goldenPRHead,
		Status: "completed", Conclusion: "failure",
		Refusal: &Refusal{Code: CodeNeedUnknown, Scope: "tests/os-checks",
			Detail: `needs "network" is not one of display, docker, gpu.`},
	}
	title, summary, text := CheckOutput(r)
	wantSummary := "Mode affected · Pull request #42 · Commit c5fa05f\n" +
		"\n" +
		"needs \"network\" is not one of display, docker, gpu (`pipeline_need_unknown` in `tests/os-checks`).\n" +
		"\n" +
		"Correct the pipeline block in `memql-package.yaml` and push again.\n"
	assertOutput(t, title, summary, text, "Refused: unknown need (tests/os-checks)", wantSummary, "")
}

func TestCheckOutputBeforeAnyStepExists(t *testing.T) {
	queued := RunReport{Mode: ModeAffected, Event: EventPullRequest, PullRequest: 42, SHA: goldenPRHead, Status: "queued"}
	title, summary, text := CheckOutput(queued)
	assertOutput(t, title, summary, text, "Queued",
		"Mode affected · Pull request #42 · Commit c5fa05f\n\nWaiting for the cluster to start this run.\n", "")

	preparing := queued
	preparing.Status = "in_progress"
	title, summary, text = CheckOutput(preparing)
	assertOutput(t, title, summary, text, "Preparing the run",
		"Mode affected · Pull request #42 · Commit c5fa05f\n\nReading `memql-package.yaml` and choosing the steps to run.\n", "")
}

// Every title shape, each from the smallest report that produces it.
func TestCheckOutputTitles(t *testing.T) {
	step := func(key, status string, ms int64) StepReport {
		stage, _, _ := strings.Cut(key, ".")
		return StepReport{Key: key, Stage: stage, Status: status, DurationMs: ms}
	}
	cases := []struct {
		name string
		r    RunReport
		want string
	}{
		{"one stage", RunReport{Status: "completed", Conclusion: "success",
			Steps: []StepReport{step("checks.vet", "succeeded", 45_000)}}, "Passed: 1 stage in 45s"},
		{"nothing to run", RunReport{Status: "completed", Conclusion: "success",
			Steps: []StepReport{{Key: "tests.go", Stage: "tests", Status: "skipped", Code: CodeNotAffected}}},
			"Passed: nothing to run"},
		// A runner that reported no time: the title does not invent one.
		{"no time", RunReport{Status: "completed", Conclusion: "success",
			Steps: []StepReport{step("checks.vet", "succeeded", 0)}}, "Passed: 1 stage"},
		{"more failed", RunReport{Status: "completed", Conclusion: "failure",
			Steps: []StepReport{
				step("tests.go#1", "failed", 1_000), step("tests.go#2", "refused", 0), step("tests.go#3", "failed", 2_000),
			}}, "Failed at tests: tests.go#1 and 2 more"},
		{"failed with no step to name", RunReport{Status: "completed", Conclusion: "failure"}, "Failed"},
		{"cancelled", RunReport{Status: "completed", Conclusion: "cancelled",
			Steps: []StepReport{step("tests.go", "cancelled", 3_000)}}, "Cancelled"},
		// No conclusion yet but every step done: the steps decide.
		{"no conclusion", RunReport{Status: "completed",
			Steps: []StepReport{step("checks.vet", "failed", 1_000)}}, "Failed at checks: checks.vet"},
		{"running, next stage waiting", RunReport{Status: "in_progress",
			Steps: []StepReport{step("checks.vet", "succeeded", 1_000), step("tests.go", "pending", 0)}},
			"Running: tests"},
		{"refused with no refusal", RunReport{Status: "completed", Conclusion: "refused"}, "Refused"},
		// A code this package does not own reads as its own detail.
		{"a foreign refusal", RunReport{Status: "completed", Conclusion: "failure",
			Refusal: &Refusal{Code: "package_credential_revoked", Detail: "The source's credential was revoked.\nReconnect it."}},
			"Refused: The source's credential was revoked."},
	}
	for _, c := range cases {
		if got, _, _ := CheckOutput(c.r); got != c.want {
			t.Errorf("%s: title %q, want %q", c.name, got, c.want)
		}
	}
}

func TestFormatDuration(t *testing.T) {
	for ms, want := range map[int64]string{
		0:          "-",
		-5:         "-",
		400:        "<1s",
		1_499:      "1s",
		45_000:     "45s",
		59_500:     "1m 00s",
		62_000:     "1m 02s",
		3_725_000:  "1h 02m 05s",
		36_000_000: "10h 00m 00s",
	} {
		if got := formatDuration(ms); got != want {
			t.Errorf("formatDuration(%d) = %q, want %q", ms, got, want)
		}
	}
}

// Text that came from a manifest or a runner is text: it cannot open a link,
// an image, an HTML tag or a table cell, and it cannot close the code block
// it is quoted in.
func TestCheckOutputKeepsReportedTextLiteral(t *testing.T) {
	r := failedPushRun()
	r.Steps[2].Message = `exit 1 <img src="https://tracker.example.test/p.gif"> [click](https://x.example.test) | *bold*`
	r.Steps[2].LogTail = "before\n```\nafter a fence of three\n"
	r.Steps = append(r.Steps, StepReport{Key: "ui.os-checks", Stage: "ui", Status: "skipped",
		Code: CodeNotAffected, Message: "No change under a|b."})
	_, summary, text := CheckOutput(r)
	for _, want := range []string{
		// An image, a tag, a link and emphasis, each escaped where it starts.
		"- `tests.go-tests#2`: exit 1 \\<img src=\"https://tracker.example.test/p.gif\"\\> \\[click\\](https://x.example.test) \\| \\*bold\\*\n",
		// A reason inside a table cell cannot open a cell of its own.
		"| ui | Skipped: No change under a\\|b. | 1 skipped | - |\n",
	} {
		if !strings.Contains(summary, want) {
			t.Errorf("the summary does not carry\n%s\nin\n%s", want, summary)
		}
	}
	if !strings.Contains(text, "````text\nbefore\n```\nafter a fence of three\n````\n") {
		t.Errorf("a tail holding a fence is not quoted in a longer one:\n%s", text)
	}

	// A refusal's sentence begins a paragraph, where a leading "#", "-", ">"
	// or "1." would make it a heading, a list or a quote.
	for detail, want := range map[string]string{
		"# not a heading\n- nor a list": "\\# not a heading - nor a list",
		"- not a list":                  "\\- not a list",
		"> not a quote":                 "\\> not a quote",
		"1. not a numbered list":        "1\\. not a numbered list",
	} {
		_, summary, _ := CheckOutput(RunReport{Status: "completed", Conclusion: "failure",
			Refusal: &Refusal{Code: CodeStepInvalid, Detail: detail}})
		if !strings.HasPrefix(summary, want) {
			t.Errorf("refusal %q renders as\n%s\nwant it to begin %q", detail, summary, want)
		}
	}
}

// GitHub refuses an output field over 65535 bytes. A run with more failures
// than fit keeps every field under the limit, as valid text, ending in a
// count of what was left out rather than a silent cut.
func TestCheckOutputFitsGitHubsLimit(t *testing.T) {
	r := RunReport{Mode: ModeFull, Event: EventPush, SHA: goldenMerged, Status: "completed", Conclusion: "failure"}
	tail := strings.Repeat("panic: an error that repeats é\n", 200) // about 6 KB, with a multi-byte rune
	for i := 1; i <= 400; i++ {
		r.Steps = append(r.Steps, StepReport{
			Key: fmt.Sprintf("tests.go-tests#%d", i), Stage: "tests", Status: "failed", DurationMs: 1_000,
			Message: strings.Repeat("The command exited with status 1. ", 20), LogTail: tail,
		})
	}
	title, summary, text := CheckOutput(r)
	for name, field := range map[string]string{"title": title, "summary": summary, "text": text} {
		if len(field) > checkOutputLimit {
			t.Errorf("%s is %d bytes, over GitHub's %d", name, len(field), checkOutputLimit)
		}
		if !utf8.ValidString(field) {
			t.Errorf("%s was cut inside a rune", name)
		}
	}
	if !strings.Contains(summary, "more failed steps are listed on the run page") {
		t.Errorf("the summary does not say what it left out:\n...%s", summary[len(summary)-300:])
	}
	if !strings.Contains(text, "more failed steps have their output on the run page") {
		t.Errorf("the text does not say what it left out:\n...%s", text[len(text)-300:])
	}
	if strings.Count(text, "```")%2 != 0 {
		t.Error("the text leaves a code block open")
	}

	// One tail longer than the limit keeps its LAST lines: the end of a
	// failing log is where the failure is.
	one := failedPushRun()
	one.Steps[2].LogTail = strings.Repeat("noise line\n", 10_000) + "the line that explains the failure\n"
	_, _, text = CheckOutput(one)
	if len(text) > checkOutputLimit || !strings.Contains(text, "the line that explains the failure") {
		t.Errorf("a long tail lost its end (len %d)", len(text))
	}
	if !strings.Contains(text, "Showing the last lines only") {
		t.Error("a cut tail does not say it was cut")
	}
}

// Plain text indicators only: no emoji, no pictographic arrows or checks.
func TestCheckOutputUsesNoEmoji(t *testing.T) {
	reports := []RunReport{
		failedPushRun(),
		{Status: "queued"},
		{Status: "in_progress"},
		{Status: "completed", Conclusion: "refused", Refusal: &Refusal{Code: CodeForkRefused, Detail: "A fork."}},
		{Status: "completed", Conclusion: "success", Steps: []StepReport{{Key: "a.b", Stage: "a", Status: "succeeded", DurationMs: 1}}},
		{Status: "completed", Conclusion: "cancelled", Steps: []StepReport{{Key: "a.b", Stage: "a", Status: "cancelled"}}},
	}
	for code := range codeClasses {
		reports = append(reports, RunReport{Status: "completed", Conclusion: "failure", Refusal: &Refusal{Code: code, Detail: "d"}})
	}
	for _, r := range reports {
		title, summary, text := CheckOutput(r)
		for _, field := range []string{title, summary, text} {
			for _, ru := range field {
				if isPictographic(ru) {
					t.Errorf("output for %+v carries %U (%q)", r, ru, ru)
				}
			}
		}
	}
}

// Every refusal a run can conclude with names its remedy: a refusal that
// tells a person what is wrong and not what to do is half a sentence.
func TestEveryRefusalCodeHasATitleAndARemedy(t *testing.T) {
	for code, class := range codeClasses {
		if class != ClassRefusal {
			continue
		}
		if refusalTitles[code] == "" {
			t.Errorf("%s has no title phrase", code)
		}
		if refusalRemedies[code] == "" {
			t.Errorf("%s has no remedy", code)
		}
	}
}

func isPictographic(r rune) bool {
	switch {
	case r >= 0x1F000 && r <= 0x1FAFF, // emoji, pictographs, symbols
		r >= 0x2190 && r <= 0x21FF, // arrows
		r >= 0x2300 && r <= 0x23FF, // technical: hourglasses, watches
		r >= 0x2600 && r <= 0x27BF, // symbols and dingbats: check marks, crosses, stars
		r >= 0x2B00 && r <= 0x2BFF, // arrows and stars
		r == 0xFE0F, r == 0x200D:   // emoji presentation, joiner
		return true
	}
	return false
}

func assertOutput(t *testing.T, title, summary, text, wantTitle, wantSummary, wantText string) {
	t.Helper()
	if title != wantTitle {
		t.Errorf("title:\n got %q\nwant %q", title, wantTitle)
	}
	if summary != wantSummary {
		t.Errorf("summary:\n--- got ---\n%s\n--- want ---\n%s", summary, wantSummary)
	}
	if text != wantText {
		t.Errorf("text:\n--- got ---\n%s\n--- want ---\n%s", text, wantText)
	}
}
