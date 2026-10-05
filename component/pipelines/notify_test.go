package pipelines

import (
	"bytes"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// notify_test.go -- the message a notify stage sends (epic memql#5480, design
// record D16). The goldens ARE the copy: a change to a word shows up here,
// where it can be read in place.
//
// The message is read where a raw commit message cannot be trusted: Discord
// renders Markdown and mentions, and a mail client reads a Subject header. So
// beside the goldens sit the cases a hostile commit title, branch or failure
// message would aim at, and the limits Discord answers with a refusal.

// wireMessage mirrors the payload as Discord reads it. It is decoded with
// DisallowUnknownFields, so a key this file does not name fails the test: the
// shape is pinned from the reader's side, not the writer's.
type wireMessage struct {
	Username        string `json:"username"`
	AllowedMentions struct {
		Parse []string `json:"parse"`
	} `json:"allowed_mentions"`
	Embeds []wireEmbed `json:"embeds"`
}

type wireEmbed struct {
	Title       string      `json:"title"`
	URL         string      `json:"url"`
	Description string      `json:"description"`
	Color       int         `json:"color"`
	Fields      []wireField `json:"fields"`
	Timestamp   string      `json:"timestamp"`
}

type wireField struct {
	Name   string `json:"name"`
	Value  string `json:"value"`
	Inline bool   `json:"inline"`
}

// composeDiscord composes n and decodes what a webhook would receive. Every
// message any test composes is held to the two rules that never vary: it is
// the payload Discord reads, and it pings nobody.
func composeDiscord(t *testing.T, n Notification) (wireEmbed, string) {
	t.Helper()
	raw, err := DiscordMessage(n)
	if err != nil {
		t.Fatalf("DiscordMessage: %v", err)
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var msg wireMessage
	if err := dec.Decode(&msg); err != nil {
		t.Fatalf("the message is not the payload Discord reads: %v\n%s", err, raw)
	}
	if msg.Username != "MemQL Pipelines" {
		t.Errorf("username = %q, want MemQL Pipelines", msg.Username)
	}
	// An empty array, not null and not absent: either of those leaves
	// Discord's default mention parsing in force.
	if msg.AllowedMentions.Parse == nil || len(msg.AllowedMentions.Parse) != 0 {
		t.Errorf("allowed_mentions.parse = %#v, want an empty array", msg.AllowedMentions.Parse)
	}
	if !strings.Contains(string(raw), `"allowed_mentions":{"parse":[]}`) {
		t.Errorf("the payload does not say allowed_mentions.parse is []:\n%s", raw)
	}
	if len(msg.Embeds) != 1 {
		t.Fatalf("the message carries %d embeds, want 1", len(msg.Embeds))
	}
	return msg.Embeds[0], string(raw)
}

// composeEmail composes n as an email and fails the test when it is refused.
func composeEmail(t *testing.T, n Notification) (subject, body string) {
	t.Helper()
	subject, body, err := EmailMessage(n)
	if err != nil {
		t.Fatalf("EmailMessage: %v", err)
	}
	return subject, body
}

func fieldNames(e wireEmbed) []string {
	var out []string
	for _, f := range e.Fields {
		out = append(out, f.Name)
	}
	return out
}

func fieldNamed(t *testing.T, e wireEmbed, name string) wireField {
	t.Helper()
	for _, f := range e.Fields {
		if f.Name == name {
			return f
		}
	}
	t.Fatalf("the embed has no field %q; it has %q", name, fieldNames(e))
	return wireField{}
}

// embedSize is Discord's measure of an embed: the characters of its title,
// its description and every field name and value, summed. Runes are what
// Discord documents, and a rune is what this counts.
func embedSize(e wireEmbed) int {
	size := utf8.RuneCountInString(e.Title) + utf8.RuneCountInString(e.Description)
	for _, f := range e.Fields {
		size += utf8.RuneCountInString(f.Name) + utf8.RuneCountInString(f.Value)
	}
	return size
}

// endsInsideAnEscape reports whether s ends, before the "..." that marks a cut,
// in an odd run of backslashes: the last one escapes whatever the reader
// appends, so a cut that left it behind would swallow the first dot of its own
// mark. The mark is trimmed first. A cut string always ends in it, so a check
// of the last characters could never see the defect it exists to look for.
func endsInsideAnEscape(s string) bool {
	s = strings.TrimSuffix(s, "...")
	return (len(s)-len(strings.TrimRight(s, `\`)))%2 == 1
}

// assertWithinDiscordLimits holds an embed to every limit Discord documents
// and answers with a 400: the cap on each part, 25 fields, the 6000
// characters of the whole, and no empty field name or value.
func assertWithinDiscordLimits(t *testing.T, e wireEmbed) {
	t.Helper()
	check := func(what, s string, limit int) {
		t.Helper()
		if got := utf8.RuneCountInString(s); got > limit {
			t.Errorf("%s is %d runes, over Discord's %d", what, got, limit)
		}
		if endsInsideAnEscape(s) {
			t.Errorf("%s ends in a lone backslash, which would escape whatever follows: %q", what, s[max(0, len(s)-12):])
		}
	}
	check("the title", e.Title, 256)
	check("the description", e.Description, 4096)
	if len(e.Fields) > 25 {
		t.Errorf("the embed has %d fields, over Discord's 25", len(e.Fields))
	}
	for i, f := range e.Fields {
		check(fmt.Sprintf("the name of field %d", i), f.Name, 256)
		check(fmt.Sprintf("the value of field %d (%.20q)", i, f.Name), f.Value, 1024)
		if f.Name == "" || f.Value == "" {
			t.Errorf("field %d (%q: %q) has an empty name or value, which Discord refuses", i, f.Name, f.Value)
		}
	}
	if got := embedSize(e); got > 6000 {
		t.Errorf("the embed is %d characters, over Discord's 6000", got)
	}
}

// releasePassed is the message the Python observer sent, as this package
// sends it: the version, the OS link, and the run page where the observer
// promised "Coming soon in MemQL OS.".
func releasePassed() Notification {
	return Notification{
		Pipeline: "memql", Event: EventRelease, Version: "v0.21.7", SHA: goldenMerged, Branch: "main",
		Title: "Add the notify stage", Outcome: NotifyPassed, Stages: 4, DurationMs: 358_000,
		OSOrigin: "https://os.example.test", RunPageURL: "https://os.example.test/?pipelineRun=run1",
		At: time.Date(2026, time.September, 9, 16, 13, 12, 0, time.UTC),
	}
}

func numberedArtifacts(count, nameRunes int) []Link {
	var out []Link
	for i := 1; i <= count; i++ {
		out = append(out, Link{
			Label: fmt.Sprintf("%0*d", nameRunes, i) + ".tgz",
			URL:   fmt.Sprintf("https://os.example.test/?libraryFile=f%d", i),
		})
	}
	return out
}

// The whole payload, byte for byte: the keys, their order, the empty
// allowed_mentions array, the color, the inline flag on Version alone and the
// timestamp. Everything below can pass while this one catches a reordering.
func TestDiscordMessageOfAPassedRelease(t *testing.T) {
	raw, err := DiscordMessage(releasePassed())
	if err != nil {
		t.Fatal(err)
	}
	want := `{"username":"MemQL Pipelines","allowed_mentions":{"parse":[]},"embeds":[{"title":"memql · Release v0.21.7 passed",` +
		`"url":"https://os.example.test/?pipelineRun=run1",` +
		`"description":"Add the notify stage (a944ae3)\nAll 4 stages passed in 5m 58s.","color":3066993,` +
		`"fields":[{"name":"Version","value":"v0.21.7","inline":true},` +
		`{"name":"MemQL OS","value":"[Open](https://os.example.test/)"},` +
		`{"name":"Deployment details","value":"[Open the run](https://os.example.test/?pipelineRun=run1)"}],` +
		`"timestamp":"2026-09-09T16:13:12Z"}]}`
	if string(raw) != want {
		t.Errorf("payload:\n got %s\nwant %s", raw, want)
	}
}

// A push has no tag: MEMQL_VERSION is the commit, shown as its first seven
// characters, and a failure names the step, its code and the message. The code
// is escaped like every other text the message carries: its underscores become
// \_, which Discord draws as the underscores they are.
func TestDiscordMessageOfAFailedPush(t *testing.T) {
	n := releasePassed()
	n.Event, n.Outcome, n.Title = EventPush, NotifyFailed, "Fix the cart badge"
	n.Version = goldenMerged
	n.FailedStep, n.FailedCode, n.FailedMessage = "tests/go-tests#2", "pipeline_step_timeout", "The step ran past its 20m timeout."

	e, _ := composeDiscord(t, n)
	want := wireEmbed{
		Title: "memql · Push to main failed at tests",
		URL:   "https://os.example.test/?pipelineRun=run1",
		Description: "Fix the cart badge (a944ae3)\n" +
			"tests/go-tests#2 failed: pipeline\\_step\\_timeout. The step ran past its 20m timeout.",
		Color: 15158332,
		Fields: []wireField{
			{"Version", "a944ae3", true},
			{"MemQL OS", "[Open](https://os.example.test/)", false},
			{"Deployment details", "[Open the run](https://os.example.test/?pipelineRun=run1)", false},
		},
		Timestamp: "2026-09-09T16:13:12Z",
	}
	if !reflect.DeepEqual(e, want) {
		t.Errorf("embed:\n got %+v\nwant %+v", e, want)
	}
}

// Every outcome of every event: the headline's subject by event, its verdict
// by outcome, the description's second line, and the color. The email's
// subject is the Discord title, so the same table holds both.
func TestDiscordMessageForEveryOutcomeOfEveryEvent(t *testing.T) {
	events := []struct {
		name          string
		set           func(n *Notification)
		subject       string // what the title calls the run
		step, stage   string // the first failed step, and the stage it is in
		code, message string
		codeShown     string // the code as the Discord body spells it: underscores escaped
	}{
		// A scope is "stage/step" and a step's key is "stage.step" (StepKey): the
		// stage is the part before either, so the table has both.
		{"release", func(n *Notification) {}, "Release v0.21.7",
			"deploy/verify-rollout", "deploy", "rollout_version_mismatch", "bff-2 reports v0.21.6, not v0.21.7.",
			`rollout\_version\_mismatch`},
		{"push", func(n *Notification) { n.Event = EventPush }, "Push to main",
			"tests.go-tests#2", "tests", "pipeline_step_timeout", "The step ran past its 20m timeout.",
			`pipeline\_step\_timeout`},
		{"pull request", func(n *Notification) { n.Event, n.PullRequest = EventPullRequest, 42 }, "Pull request #42",
			"checks/build-vet", "checks", "pipeline_step_failed", "The command exited with status 1.",
			`pipeline\_step\_failed`},
		{"merge queue", func(n *Notification) { n.Event = EventMergeGroup }, "Merge queue a944ae3",
			"tests.db-tests#1", "tests", "pipeline_service_failed", "The postgres service did not become ready.",
			`pipeline\_service\_failed`},
	}
	const commit = "Add the notify stage (a944ae3)"
	const passedLine = "All 4 stages passed in 5m 58s."

	for _, ev := range events {
		for _, outcome := range []NotifyOutcome{NotifyPassed, NotifyFailed, NotifyRecovered} {
			t.Run(ev.name+" "+string(outcome), func(t *testing.T) {
				n := releasePassed()
				ev.set(&n)
				n.Outcome = outcome

				var wantTitle, wantDescription string
				wantColor := 3066993
				switch outcome {
				case NotifyPassed:
					wantTitle = "memql · " + ev.subject + " passed"
					wantDescription = commit + "\n" + passedLine
				case NotifyRecovered:
					wantTitle = "memql · " + ev.subject + " recovered"
					wantDescription = commit + "\nPassed after the previous run failed. " + passedLine
				case NotifyFailed:
					n.FailedStep, n.FailedCode, n.FailedMessage = ev.step, ev.code, ev.message
					wantTitle = "memql · " + ev.subject + " failed at " + ev.stage
					wantDescription = commit + "\n" + ev.step + " failed: " + ev.codeShown + ". " + ev.message
					wantColor = 15158332
				}

				e, _ := composeDiscord(t, n)
				if e.Title != wantTitle {
					t.Errorf("title = %q, want %q", e.Title, wantTitle)
				}
				if e.Description != wantDescription {
					t.Errorf("description:\n got %q\nwant %q", e.Description, wantDescription)
				}
				if e.Color != wantColor {
					t.Errorf("color = %d, want %d", e.Color, wantColor)
				}
				// A failed run's report link exists the moment the run row does,
				// so it carries the link too.
				if e.URL != n.RunPageURL {
					t.Errorf("url = %q, want the run page %q", e.URL, n.RunPageURL)
				}
				subject, _ := composeEmail(t, n)
				if subject != e.Title {
					t.Errorf("email subject = %q, want the Discord title %q", subject, e.Title)
				}
				if want := "memql · " + Headline(n); e.Title != want {
					t.Errorf("title = %q, want the pipeline and Headline(): %q", e.Title, want)
				}
			})
		}
	}
}

func TestHeadlineNamesTheRunByItsEventAndTheOutcomeByItsWords(t *testing.T) {
	cases := []struct {
		name string
		n    Notification
		want string
	}{
		{"a release", Notification{Event: EventRelease, Version: "v0.21.7", Outcome: NotifyPassed}, "Release v0.21.7 passed"},
		{"a release that is only a commit", Notification{Event: EventRelease, Version: goldenMerged, Outcome: NotifyPassed}, "Release a944ae3 passed"},
		{"a release with no version", Notification{Event: EventRelease, SHA: goldenMerged, Outcome: NotifyPassed}, "Release a944ae3 passed"},
		{"a push", Notification{Event: EventPush, Branch: "main", SHA: goldenMerged, Outcome: NotifyPassed}, "Push to main passed"},
		{"a push with no branch", Notification{Event: EventPush, SHA: goldenMerged, Outcome: NotifyPassed}, "Push a944ae3 passed"},
		{"a push with neither", Notification{Event: EventPush, Outcome: NotifyPassed}, "Push passed"},
		{"a pull request", Notification{Event: EventPullRequest, PullRequest: 42, Outcome: NotifyPassed}, "Pull request #42 passed"},
		{"a pull request with no number", Notification{Event: EventPullRequest, Outcome: NotifyPassed}, "Pull request passed"},
		{"the merge queue", Notification{Event: EventMergeGroup, SHA: goldenMerged, Outcome: NotifyPassed}, "Merge queue a944ae3 passed"},
		{"an event nobody named", Notification{Event: "deployment", SHA: goldenMerged, Outcome: NotifyPassed}, "Run a944ae3 passed"},

		{"failed at the stage of the step", Notification{Event: EventPush, Branch: "main", Outcome: NotifyFailed, FailedStep: "deploy/verify-rollout"},
			"Push to main failed at deploy"},
		{"failed at a stage named alone", Notification{Event: EventPush, Branch: "main", Outcome: NotifyFailed, FailedStep: "deploy"},
			"Push to main failed at deploy"},
		// A step's key is "stage.step" (StepKey), where a scope is "stage/step":
		// neither separator is in a stage's name, so the first of them ends it.
		{"failed at the stage of a step key", Notification{Event: EventPush, Branch: "main", Outcome: NotifyFailed, FailedStep: "tests.go-tests#2"},
			"Push to main failed at tests"},
		{"failed at the stage of another step key", Notification{Event: EventPush, Branch: "main", Outcome: NotifyFailed, FailedStep: "deploy.verify-rollout"},
			"Push to main failed at deploy"},
		{"a slash before a dot", Notification{Event: EventPush, Branch: "main", Outcome: NotifyFailed, FailedStep: "tests/go.tests"},
			"Push to main failed at tests"},
		{"a dot before a slash", Notification{Event: EventPush, Branch: "main", Outcome: NotifyFailed, FailedStep: "tests.go/tests"},
			"Push to main failed at tests"},
		{"a stage with a trailing separator", Notification{Event: EventPush, Branch: "main", Outcome: NotifyFailed, FailedStep: "deploy."},
			"Push to main failed at deploy"},
		{"a step with no stage in front of it", Notification{Event: EventPush, Branch: "main", Outcome: NotifyFailed, FailedStep: ".verify-rollout"},
			"Push to main failed"},
		{"a step key broken across lines", Notification{Event: EventPush, Branch: "main", Outcome: NotifyFailed, FailedStep: "te\nsts.go-tests"},
			"Push to main failed at te sts"},
		{"failed with no step to name", Notification{Event: EventPush, Branch: "main", Outcome: NotifyFailed}, "Push to main failed"},
		{"recovered", Notification{Event: EventPush, Branch: "main", Outcome: NotifyRecovered}, "Push to main recovered"},
		// An outcome this package does not know is never a pass: it says
		// nothing, and both composers refuse it (below).
		{"an outcome nobody named", Notification{Event: EventPush, Branch: "main", Outcome: "unknown"}, "Push to main"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Headline(tc.n); got != tc.want {
				t.Errorf("Headline = %q, want %q", got, tc.want)
			}
		})
	}
}

// A message that cannot say how the run ended is refused rather than sent as
// a pass: a notification that reads green for a run nobody judged is the
// false green this whole epic exists to count.
func TestDiscordMessageRefusesAnOutcomeItDoesNotKnow(t *testing.T) {
	for _, outcome := range []NotifyOutcome{"", "unknown", "PASSED"} {
		n := releasePassed()
		n.Outcome = outcome
		raw, err := DiscordMessage(n)
		if err == nil {
			t.Errorf("outcome %q: DiscordMessage composed %s, want an error", outcome, raw)
			continue
		}
		if !strings.Contains(err.Error(), fmt.Sprintf("%q", string(outcome))) {
			t.Errorf("outcome %q: the error %q does not name it", outcome, err)
		}
	}
}

// The two composers refuse the same outcomes, with the same error. An email
// that said nothing of how the run ended would be read as good news by anyone
// who saw only that it arrived.
func TestEmailMessageRefusesAnOutcomeItDoesNotKnow(t *testing.T) {
	for _, outcome := range []NotifyOutcome{"", "unknown", "PASSED", "passed "} {
		n := releasePassed()
		n.Outcome = outcome
		subject, body, err := EmailMessage(n)
		if err == nil {
			t.Errorf("outcome %q: EmailMessage composed %q, want an error", outcome, subject)
			continue
		}
		if subject != "" || body != "" {
			t.Errorf("outcome %q: a refused email carries the subject %q and the body %q; nothing may be sent", outcome, subject, body)
		}
		if !strings.Contains(err.Error(), fmt.Sprintf("%q", string(outcome))) {
			t.Errorf("outcome %q: the error %q does not name it", outcome, err)
		}
		if _, discordErr := DiscordMessage(n); discordErr == nil || discordErr.Error() != err.Error() {
			t.Errorf("outcome %q: EmailMessage refuses with %q and DiscordMessage with %v; the two must say the same", outcome, err, discordErr)
		}
	}
}

// NotifyOutcome.Valid is the question both composers ask, so a caller may ask
// it first, and the three agree about every outcome.
func TestDiscordMessageAndEmailMessageAgreeWithValid(t *testing.T) {
	for outcome, valid := range map[NotifyOutcome]bool{
		NotifyPassed: true, NotifyFailed: true, NotifyRecovered: true,
		"": false, "unknown": false, "PASSED": false, "passed ": false,
	} {
		n := releasePassed()
		n.Outcome = outcome
		if got := outcome.Valid(); got != valid {
			t.Errorf("%q.Valid() = %v, want %v", outcome, got, valid)
		}
		_, discordErr := DiscordMessage(n)
		_, _, emailErr := EmailMessage(n)
		if (discordErr == nil) != valid || (emailErr == nil) != valid {
			t.Errorf("outcome %q (valid = %v): DiscordMessage error %v, EmailMessage error %v", outcome, valid, discordErr, emailErr)
		}
	}
}

// Every outcome Valid admits is one the composers have words for: a verdict in
// the headline and a line of its own under the title. An outcome added to the
// one table and not to those would be sent as a message that says nothing of
// the run -- the default of a switch deciding what an unknown value means.
func TestDiscordEveryValidOutcomeIsSpokenFor(t *testing.T) {
	if len(outcomeColors) != 3 {
		t.Fatalf("outcomeColors has %d outcomes; a new one needs words in headline and outcomeLine, and a case here", len(outcomeColors))
	}
	for outcome := range outcomeColors {
		n := releasePassed()
		n.Outcome = outcome
		n.FailedStep, n.FailedCode = "tests/go-tests", "pipeline_step_failed"
		if head := Headline(n); head == runSubject(n, asWritten) {
			t.Errorf("outcome %q has no verdict in its headline: %q", outcome, head)
		}
		if lines := descriptionLines(n, asWritten); len(lines) != 2 {
			t.Errorf("outcome %q has no line of its own under the title: %q", outcome, lines)
		}
	}
}

// The injection the review focus names: a commit title is attacker-chosen
// text, and it must read as text. The brief's own example, and what is not
// escaped on purpose.
func TestDiscordMessageEscapesWhatARepositoryWrote(t *testing.T) {
	n := releasePassed()
	n.Title = "[click](https://evil.test) @everyone **x**"
	e, _ := composeDiscord(t, n)
	want := `\[click\]\(https://evil.test\) @everyone \*\*x\*\* (a944ae3)` + "\nAll 4 stages passed in 5m 58s."
	if e.Description != want {
		t.Errorf("description:\n got %q\nwant %q", e.Description, want)
	}
}

func TestDiscordEscapeCoversExactlyTheMarkdownCharacters(t *testing.T) {
	const all = "a\\b*c_d~e`f>g|h[i]j(k)l"
	const want = "a\\\\b\\*c\\_d\\~e\\`f\\>g\\|h\\[i\\]j\\(k\\)l"
	if got := escapeDiscord(all); got != want {
		t.Errorf("escapeDiscord(%q) = %q, want %q", all, got, want)
	}
	// @ stays as written: allowed_mentions.parse is what stops a ping, and
	// rewriting the text would misquote the commit. Nothing else is touched.
	const plain = "@everyone @here fix #12 v1.2 -- 50% done +1 =2 :smile: {x} $y ^z, see https://example.test/a?b=c&d=e"
	if got := escapeDiscord(plain); got != plain {
		t.Errorf("escapeDiscord changed text with no Markdown in it: %q", got)
	}
}

// Every place a repository's, a runner's or a person's words are rendered, in
// one hostile string: each must arrive escaped and on one line, so that nothing
// in it can open a link, a mention, a heading or a quote, or start a line of
// its own.
//
// That includes the failed step's name and its code. Nothing at this seam makes
// a code one from the catalogue -- the driver takes it from the executor's
// answer and only masks it -- and a step's name is whatever the caller passes,
// so neither is a place a link may be written.
func TestDiscordMessageEscapesEveryTextAPersonSupplied(t *testing.T) {
	const hostile = "[x](https://evil.test)"
	const escaped = `\[x\]\(https://evil.test\)`
	const hostileCode = "[Sign in again](https://evil.test/phish) **urgent** @everyone"
	const escapedCode = `\[Sign in again\]\(https://evil.test/phish\) \*\*urgent\*\* @everyone`
	const hostileStep = "**tests**/[go-tests](https://evil.test)"
	const escapedStep = `\*\*tests\*\*/\[go-tests\]\(https://evil.test\)`

	n := releasePassed()
	n.Pipeline = "my_repo"
	n.Event, n.Branch = EventPush, hostile
	n.Version = "v1_beta*"
	n.Outcome = NotifyFailed
	n.FailedStep, n.FailedCode, n.FailedMessage = hostileStep, hostileCode, hostile+"\n# heading\n> quote"
	n.Links = []Link{{Label: "*" + hostile + "*", URL: "https://memql.io/docs/"}}
	n.Artifacts = []Link{{Label: hostile, URL: "https://os.example.test/?libraryFile=f1"}}

	e, _ := composeDiscord(t, n)
	// The stage the title says the run failed at is the step's name up to its
	// first "/" or ".", and it is escaped like the rest of the title.
	if want := `my\_repo · Push to ` + escaped + ` failed at \*\*tests\*\*`; e.Title != want {
		t.Errorf("title = %q, want %q", e.Title, want)
	}
	// One line, with the heading and the quote the message tried to start
	// folded into it and the quote's marker escaped.
	wantLine := escapedStep + ` failed: ` + escapedCode + `. ` + escaped + ` # heading \> quote`
	if lines := strings.Split(e.Description, "\n"); len(lines) != 2 || lines[1] != wantLine {
		t.Errorf("description lines = %q, want 2 lines ending in %q", lines, wantLine)
	}
	if got := fieldNamed(t, e, "Version").Value; got != `v1\_beta\*` {
		t.Errorf("Version = %q, want %q", got, `v1\_beta\*`)
	}
	if got := fieldNamed(t, e, `\*`+escaped+`\*`).Value; got != "[memql.io/docs/](https://memql.io/docs/)" {
		t.Errorf("a link whose label is hostile = %q", got)
	}
	if got := fieldNamed(t, e, "Artifacts").Value; got != "["+escaped+"](https://os.example.test/?libraryFile=f1)" {
		t.Errorf("an artifact whose name is hostile = %q", got)
	}
}

// Mentions are neutralized by allowed_mentions (composeDiscord checks it on
// every message), not by rewriting the text: @everyone stays as the commit
// wrote it. A mention's own syntax cannot close, because its `>` is escaped.
func TestDiscordMessageLeavesMentionsAsWritten(t *testing.T) {
	n := releasePassed()
	n.Title = "@everyone @here <@123456789> <@&987654321> deploy now"
	e, _ := composeDiscord(t, n)
	want := `@everyone @here <@123456789\> <@&987654321\> deploy now (a944ae3)`
	if lines := strings.Split(e.Description, "\n"); lines[0] != want {
		t.Errorf("first line = %q, want %q", lines[0], want)
	}
}

// The stored body is what an operator reads when a delivery is in question, so
// "&" and ">" stay as written rather than becoming \u0026 and \u003e; the
// reader is Discord's JSON decoder, which reads either the same. And the title
// links to the run page exactly as it was given -- it is a JSON property, not
// Markdown -- while the Markdown link in Deployment details encodes a
// parenthesis, which would otherwise end its destination early.
func TestDiscordMessageKeepsTheStoredBodyReadable(t *testing.T) {
	n := releasePassed()
	n.RunPageURL = "https://os.example.test/?pipelineRun=run1&note=(a)"
	n.Title = "<@123456789> a > b"
	e, raw := composeDiscord(t, n)
	if !strings.Contains(raw, `"url":"https://os.example.test/?pipelineRun=run1&note=(a)"`) {
		t.Errorf("the run page's & was escaped, or its parentheses were encoded, in the title's link:\n%s", raw)
	}
	if !strings.Contains(raw, `<@123456789\\> a \\> b (a944ae3)`) {
		t.Errorf("the title's < and > were escaped in the payload:\n%s", raw)
	}
	if e.URL != n.RunPageURL {
		t.Errorf("url = %q, want the run page exactly as given %q", e.URL, n.RunPageURL)
	}
	if got, want := fieldNamed(t, e, "Deployment details").Value, "[Open the run](https://os.example.test/?pipelineRun=run1&note=%28a%29)"; got != want {
		t.Errorf("Deployment details = %q, want %q", got, want)
	}
}

// A title is the first thing on the description's first line, where Markdown
// reads a heading, a list or a subtext from its first characters.
func TestDiscordMessageNeutralizesATitleThatStartsALine(t *testing.T) {
	for title, want := range map[string]string{
		"# Release notes":      `\# Release notes (a944ae3)`,
		"### Third level":      `\### Third level (a944ae3)`,
		"-# small print":       `\-# small print (a944ae3)`,
		"- first item":         `\- first item (a944ae3)`,
		"1. first":             `1\. first (a944ae3)`,
		"12. twelfth":          `12\. twelfth (a944ae3)`,
		"> a quote":            `\> a quote (a944ae3)`,
		"* a bullet":           `\* a bullet (a944ae3)`,
		"1.5 release":          `1\.5 release (a944ae3)`,
		"Release 1. Later":     `Release 1. Later (a944ae3)`,
		"Fix #12 and -# more":  `Fix #12 and -# more (a944ae3)`,
		"line one\n# line two": `line one # line two (a944ae3)`,
		"\n\n  # after blanks": `\# after blanks (a944ae3)`,
	} {
		n := releasePassed()
		n.Title = title
		e, _ := composeDiscord(t, n)
		lines := strings.Split(e.Description, "\n")
		if len(lines) != 2 || lines[0] != want {
			t.Errorf("title %q: description lines = %q, want first line %q and 2 lines", title, lines, want)
		}
	}
}

// An origin is "https://os.<domain>", and a person may well write it with the
// slash a URL ends in: the link is one slash either way, never two.
func TestDiscordMessageLinksTheOSOriginWithOneSlash(t *testing.T) {
	for _, origin := range []string{"https://os.example.test", "https://os.example.test/", " https://os.example.test// "} {
		n := releasePassed()
		n.OSOrigin = origin
		e, _ := composeDiscord(t, n)
		if got := fieldNamed(t, e, "MemQL OS").Value; got != "[Open](https://os.example.test/)" {
			t.Errorf("origin %q: MemQL OS = %q", origin, got)
		}
		if _, body := composeEmail(t, n); !strings.Contains(body, "\nMemQL OS: https://os.example.test/\n") {
			t.Errorf("origin %q: the email's OS link is not one slash:\n%s", origin, body)
		}
	}
}

// The failure line opens with the failed step's name, which is as much text
// someone chose as the commit's title above it: a step called "# URGENT" must
// not draw as a heading, nor "- item" as a bullet.
func TestDiscordMessageNeutralizesAStepNameThatStartsALine(t *testing.T) {
	for step, want := range map[string]string{
		"# URGENT":   `\# URGENT failed: c.`,
		"-# small":   `\-# small failed: c.`,
		"- first":    `\- first failed: c.`,
		"1. first":   `1\. first failed: c.`,
		"> a quote":  `\> a quote failed: c.`,
		"* a bullet": `\* a bullet failed: c.`,
		"tests/a":    `tests/a failed: c.`,
		"tests.a-1":  `tests.a-1 failed: c.`,
	} {
		n := releasePassed()
		n.Outcome = NotifyFailed
		n.FailedStep, n.FailedCode = step, "c"
		e, _ := composeDiscord(t, n)
		lines := strings.Split(e.Description, "\n")
		if len(lines) != 2 || lines[1] != want {
			t.Errorf("step %q: description lines = %q, want a second line %q", step, lines, want)
		}
	}
}

func TestDiscordMessageOmitsWhatItDoesNotKnow(t *testing.T) {
	t.Run("no OS origin means no MemQL OS field, never an invented one", func(t *testing.T) {
		n := releasePassed()
		n.OSOrigin = ""
		e, _ := composeDiscord(t, n)
		if got, want := fieldNames(e), []string{"Version", "Deployment details"}; !reflect.DeepEqual(got, want) {
			t.Errorf("fields = %q, want %q", got, want)
		}
	})
	t.Run("no run page says why, in plain text", func(t *testing.T) {
		n := releasePassed()
		n.OSOrigin, n.RunPageURL = "", ""
		e, raw := composeDiscord(t, n)
		const want = "Not available: this cluster has no MemQL OS domain configured."
		if got := fieldNamed(t, e, "Deployment details").Value; got != want {
			t.Errorf("Deployment details = %q, want %q", got, want)
		}
		if e.URL != "" || strings.Contains(raw, `"url"`) {
			t.Errorf("a message with no run page links its title to %q", e.URL)
		}
	})
	t.Run("no time means no timestamp", func(t *testing.T) {
		n := releasePassed()
		n.At = time.Time{}
		e, raw := composeDiscord(t, n)
		if e.Timestamp != "" || strings.Contains(raw, `"timestamp"`) {
			t.Errorf("a message with no time carries the timestamp %q", e.Timestamp)
		}
	})
	t.Run("no commit and no title leave the outcome alone", func(t *testing.T) {
		n := releasePassed()
		n.Title, n.SHA = "", ""
		e, _ := composeDiscord(t, n)
		if want := "All 4 stages passed in 5m 58s."; e.Description != want {
			t.Errorf("description = %q, want %q", e.Description, want)
		}
		n.SHA = goldenMerged
		e, _ = composeDiscord(t, n)
		if want := "(a944ae3)\nAll 4 stages passed in 5m 58s."; e.Description != want {
			t.Errorf("a commit with no title: description = %q, want %q", e.Description, want)
		}
	})
}

func TestDiscordMessageSaysTimeAndCountsPlainly(t *testing.T) {
	for _, tc := range []struct {
		stages int
		ms     int64
		want   string
	}{
		{4, 358_000, "All 4 stages passed in 5m 58s."},
		{1, 45_000, "All 1 stage passed in 45s."},
		{12, 3_725_000, "All 12 stages passed in 1h 02m 05s."},
		// A time nobody measured is left out, not guessed.
		{4, 0, "All 4 stages passed."},
		{0, 358_000, "All stages passed in 5m 58s."},
	} {
		n := releasePassed()
		n.Stages, n.DurationMs = tc.stages, tc.ms
		e, _ := composeDiscord(t, n)
		if got := strings.Split(e.Description, "\n")[1]; got != tc.want {
			t.Errorf("%d stages in %dms: %q, want %q", tc.stages, tc.ms, got, tc.want)
		}
	}
}

func TestDiscordMessageStampsTheTimeInUTC(t *testing.T) {
	n := releasePassed()
	n.At = time.Date(2026, time.September, 9, 18, 13, 12, 0, time.FixedZone("CEST", 2*60*60))
	e, _ := composeDiscord(t, n)
	if want := "2026-09-09T16:13:12Z"; e.Timestamp != want {
		t.Errorf("timestamp = %q, want %q", e.Timestamp, want)
	}
}

func TestDiscordMessageShowsTheVersionTheWayAPersonReadsIt(t *testing.T) {
	for _, tc := range []struct {
		name         string
		version, sha string
		want         string
		wantNone     bool
	}{
		{name: "a tag is kept whole", version: "v0.21.7", sha: goldenMerged, want: "v0.21.7"},
		{name: "a commit is its first seven", version: goldenMerged, sha: goldenMerged, want: "a944ae3"},
		{name: "a commit with no sha beside it is still one", version: goldenMerged, want: "a944ae3"},
		{name: "a sha-256 commit too", version: strings.Repeat("ab", 32), want: "abababa"},
		{name: "a short version equal to the sha", version: "abc1234def", sha: "abc1234def", want: "abc1234"},
		{name: "no version falls back to the commit", sha: goldenMerged, want: "a944ae3"},
		{name: "nothing to show", wantNone: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			n := releasePassed()
			n.Version, n.SHA = tc.version, tc.sha
			e, _ := composeDiscord(t, n)
			if tc.wantNone {
				for _, f := range e.Fields {
					if f.Name == "Version" {
						t.Errorf("a message with no version carries %+v", f)
					}
				}
				return
			}
			got := fieldNamed(t, e, "Version")
			if got.Value != tc.want || !got.Inline {
				t.Errorf("Version = %+v, want %q, inline", got, tc.want)
			}
		})
	}
}

func TestDiscordMessageCarriesTheStagesLinksInOrder(t *testing.T) {
	n := releasePassed()
	n.Links = []Link{
		{Label: "Docs", URL: "https://memql.io/docs/"},
		{Label: "Changelog", URL: "https://memql.io/changelog"},
		{Label: "Home", URL: "https://memql.io"},
	}
	n.Artifacts = []Link{{Label: "memql-linux-amd64.tar.gz", URL: "https://os.example.test/?libraryFile=f1"}}

	e, _ := composeDiscord(t, n)
	want := []wireField{
		{"Version", "v0.21.7", true},
		{"MemQL OS", "[Open](https://os.example.test/)", false},
		{"Deployment details", "[Open the run](https://os.example.test/?pipelineRun=run1)", false},
		{"Docs", "[memql.io/docs/](https://memql.io/docs/)", false},
		{"Changelog", "[memql.io/changelog](https://memql.io/changelog)", false},
		{"Home", "[memql.io](https://memql.io)", false},
		{"Artifacts", "[memql-linux-amd64.tar.gz](https://os.example.test/?libraryFile=f1)", false},
	}
	if !reflect.DeepEqual(e.Fields, want) {
		t.Errorf("fields:\n got %+v\nwant %+v", e.Fields, want)
	}
}

// A link is the manifest author's, and a URL can end the Markdown around it:
// a parenthesis closes the destination early. And a URL that carries a
// credential, or is not a web address, is not published at all.
func TestDiscordMessageRendersOnlyLinksItCanMakeSafe(t *testing.T) {
	n := releasePassed()
	n.Links = []Link{
		{Label: "Go", URL: "https://en.wikipedia.org/wiki/Go_(programming_language)"},
		{Label: "Secret", URL: "https://user:hunter2@memql.io/docs/"},
		{Label: "Script", URL: "javascript:alert(1)"},
		{Label: "Relative", URL: "/docs"},
		{Label: "Spaced", URL: "https://memql.io/a b"},
		{Label: "", URL: "https://memql.io/unlabelled"},
		{Label: "Plain http", URL: "http://memql.io/plain"},
	}
	e, raw := composeDiscord(t, n)
	want := []wireField{
		{`Go`, `[en.wikipedia.org/wiki/Go\_\(programming\_language\)](https://en.wikipedia.org/wiki/Go_%28programming_language%29)`, false},
		{"Link", "[memql.io/unlabelled](https://memql.io/unlabelled)", false},
		{"Plain http", "[memql.io/plain](http://memql.io/plain)", false},
	}
	if got := e.Fields[3:]; !reflect.DeepEqual(got, want) {
		t.Errorf("link fields:\n got %+v\nwant %+v", got, want)
	}
	if strings.Contains(raw, "hunter2") {
		t.Errorf("a credential in a link's URL reached the message:\n%s", raw)
	}
}

func TestDiscordMessageListsFiveArtifactsAndCountsTheRest(t *testing.T) {
	for _, tc := range []struct {
		count     int
		wantLines int
		wantMore  string
	}{
		{1, 1, ""},
		{5, 5, ""},
		{6, 5, "and 1 more on the run page"},
		{9, 5, "and 4 more on the run page"},
	} {
		n := releasePassed()
		n.Artifacts = numberedArtifacts(tc.count, 1)
		e, _ := composeDiscord(t, n)
		lines := strings.Split(fieldNamed(t, e, "Artifacts").Value, "\n")

		wantLen := tc.wantLines
		if tc.wantMore != "" {
			wantLen++
		}
		if len(lines) != wantLen {
			t.Errorf("%d artifacts: %d lines %q, want %d", tc.count, len(lines), lines, wantLen)
			continue
		}
		for i := 0; i < tc.wantLines; i++ {
			want := fmt.Sprintf("[%d.tgz](https://os.example.test/?libraryFile=f%d)", i+1, i+1)
			if lines[i] != want {
				t.Errorf("%d artifacts: line %d = %q, want %q", tc.count, i, lines[i], want)
			}
		}
		if tc.wantMore != "" && lines[tc.wantLines] != tc.wantMore {
			t.Errorf("%d artifacts: last line = %q, want %q", tc.count, lines[tc.wantLines], tc.wantMore)
		}
	}

	n := releasePassed()
	if e, _ := composeDiscord(t, n); strings.Contains(strings.Join(fieldNames(e), ","), "Artifacts") {
		t.Error("a run with no artifacts has an Artifacts field")
	}
}

func TestDiscordMessageNamesAnArtifactThatHasNoNameOrNoLink(t *testing.T) {
	n := releasePassed()
	n.Artifacts = []Link{
		{Label: "", URL: "https://os.example.test/?libraryFile=f1"},
		{Label: "report.html", URL: ""},
		{Label: "bad.html", URL: "javascript:alert(1)"},
		{Label: "", URL: ""},
	}
	e, _ := composeDiscord(t, n)
	want := "[os.example.test/](https://os.example.test/?libraryFile=f1)\nreport.html\nbad.html"
	if got := fieldNamed(t, e, "Artifacts").Value; got != want {
		t.Errorf("Artifacts = %q, want %q", got, want)
	}
}

// The limits Discord enforces with a 400, which would fail the delivery
// permanently: every part of the message is text a repository chose, so every
// part is held to its cap, on a rune boundary and with a mark that says it
// was cut.
func hostileNotification() Notification {
	huge := strings.Repeat("[é_]", 5_000) // 20 000 runes, 3 in 4 of them Markdown, all of them multi-byte or special
	n := releasePassed()
	n.Outcome = NotifyFailed
	n.Pipeline, n.Title, n.Branch, n.Version, n.FailedMessage = huge, huge, huge, huge, huge
	n.FailedStep = strings.Repeat("stage", 2_000) + "/step"
	n.FailedCode = strings.Repeat("code_", 2_000)
	n.Event = EventPush
	for range 30 {
		n.Links = append(n.Links, Link{Label: huge, URL: "https://memql.io/" + strings.Repeat("a_(b)", 400)})
		n.Artifacts = append(n.Artifacts, Link{Label: huge, URL: "https://os.example.test/?libraryFile=" + strings.Repeat("f", 600)})
	}
	return n
}

func TestDiscordMessageHoldsEveryLimitUnderHostileInput(t *testing.T) {
	e, raw := composeDiscord(t, hostileNotification())
	assertWithinDiscordLimits(t, e)
	if !utf8.ValidString(raw) {
		t.Error("the payload is not valid UTF-8")
	}
	// What the reader needs most survives the cuts: the run page.
	if got := fieldNamed(t, e, "Deployment details").Value; got != "[Open the run](https://os.example.test/?pipelineRun=run1)" {
		t.Errorf("Deployment details = %q", got)
	}
}

// Each part is cut at exactly its own cap, with the mark counted toward it:
// 256 and 4096 and 256 and 1024, no more and no fewer, because a part cut
// short of its cap gives up text for nothing and one over it is refused.
func TestDiscordMessageCutsEachPartAtItsLimit(t *testing.T) {
	long := strings.Repeat("é", 10_000)
	for _, tc := range []struct {
		name  string
		set   func(n *Notification)
		part  func(e wireEmbed) string
		limit int
	}{
		{"the title", func(n *Notification) { n.Pipeline = long },
			func(e wireEmbed) string { return e.Title }, 256},
		{"the description", func(n *Notification) { n.Outcome, n.FailedStep = NotifyFailed, strings.Repeat("s", 10_000)+"/step" },
			func(e wireEmbed) string { return e.Description }, 4096},
		{"a field name", func(n *Notification) { n.Links = []Link{{Label: long, URL: "https://memql.io/docs/"}} },
			func(e wireEmbed) string { return e.Fields[3].Name }, 256},
		{"a field value", func(n *Notification) { n.Version = long },
			func(e wireEmbed) string { return fieldNamedValue(e, "Version") }, 1024},
		{"a link too long to carry its own address", func(n *Notification) {
			n.Links = []Link{{Label: "Docs", URL: "https://memql.io/" + strings.Repeat("a", 2_000)}}
		}, func(e wireEmbed) string { return e.Fields[3].Value }, 1024},
	} {
		t.Run(tc.name, func(t *testing.T) {
			n := releasePassed()
			tc.set(&n)
			e, _ := composeDiscord(t, n)
			assertWithinDiscordLimits(t, e)
			part := tc.part(e)
			if got := utf8.RuneCountInString(part); got != tc.limit || !strings.HasSuffix(part, "...") {
				t.Errorf("the part is %d runes ending %q, want %d ending in ...", got, part[max(0, len(part)-6):], tc.limit)
			}
		})
	}
}

func fieldNamedValue(e wireEmbed, name string) string {
	for _, f := range e.Fields {
		if f.Name == name {
			return f.Value
		}
	}
	return ""
}

// The brief's case: a 10 000-rune failure message. The description carries 300
// of its runes, the mark that says so counted, and the embed stays whole.
func TestDiscordMessageCutsAFailureMessageAt300Runes(t *testing.T) {
	n := releasePassed()
	n.Outcome = NotifyFailed
	n.FailedStep, n.FailedCode = "deploy/verify-rollout", "rollout_failed"
	n.FailedMessage = strings.Repeat("x", 10_000)
	e, _ := composeDiscord(t, n)
	assertWithinDiscordLimits(t, e)

	const prefix = `deploy/verify-rollout failed: rollout\_failed. `
	line := strings.Split(e.Description, "\n")[1]
	message := strings.TrimPrefix(line, prefix)
	if message == line {
		t.Fatalf("the failure line = %q, want it to start %q", line, prefix)
	}
	if got := utf8.RuneCountInString(message); got != 300 || !strings.HasSuffix(message, "...") {
		t.Errorf("the message is %d runes ending %q, want 300 ending in ...", got, message[len(message)-5:])
	}

	// A message of exactly 300 is not cut.
	n.FailedMessage = strings.Repeat("x", 300)
	e, _ = composeDiscord(t, n)
	if line := strings.Split(e.Description, "\n")[1]; line != prefix+strings.Repeat("x", 300) {
		t.Errorf("a 300-rune message was cut: %q", line)
	}
}

// A cut never leaves half an escape. Escaping doubles a Markdown character
// into a pair, so a cut that lands between the pair's two characters would
// leave a backslash to escape the first dot of the mark. Sweeping the length
// of the prefix before the text moves the cut across both parities.
func TestDiscordMessageNeverCutsInsideAnEscape(t *testing.T) {
	for prefix := 1; prefix <= 4; prefix++ {
		for length := 125; length <= 140; length++ {
			n := releasePassed()
			n.Event = EventPush
			n.Pipeline = strings.Repeat("p", prefix)
			n.Branch = strings.Repeat("[", length)
			e, _ := composeDiscord(t, n)
			assertWithinDiscordLimits(t, e)
			if !strings.HasSuffix(e.Title, "...") {
				t.Fatalf("prefix %d, length %d: the title was not cut: %q", prefix, length, e.Title)
			}
		}
	}
}

func TestDiscordFitRunes(t *testing.T) {
	for _, tc := range []struct {
		name string
		s    string
		n    int
		want string
	}{
		{"exactly the limit", "hello", 5, "hello"},
		{"one over", "hello!", 5, "he..."},
		{"counts the mark toward the limit", "héllo wörld", 8, "héllo..."},
		{"cuts on a rune boundary", "日本語のテキスト", 6, "日本語..."},
		{"a limit the mark fills", "hello world", 3, "..."},
		{"a limit under the mark", "hello world", 2, ".."},
		{"nothing fits", "hello world", 0, ""},
		{"a negative limit", "hello world", -4, ""},
		{"empty", "", 5, ""},
		{"drops a lone backslash the cut left", `abc\*defg`, 7, "abc..."},
		{"keeps a pair the cut left whole", `ab\\cdefg`, 7, `ab\\...`},
		{"drops one of an odd run", `\\\\\\\\\\`, 6, `\\...`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := fitRunes(tc.s, tc.n); got != tc.want {
				t.Errorf("fitRunes(%q, %d) = %q, want %q", tc.s, tc.n, got, tc.want)
			}
		})
	}
}

func TestDiscordOneLineCollapsesWhatCouldStartALine(t *testing.T) {
	for in, want := range map[string]string{
		"  a \t b\r\n\r\nc  ":        "a b c",
		"ctrl\x00chars\x07here\x7f.": "ctrl chars here .",
		"nbsp\u00a0and\u2028sep":     "nbsp and sep",
		"already one line":           "already one line",
		"":                           "",
		"\n\n":                       "",
	} {
		if got := oneLineText(in); got != want {
			t.Errorf("oneLineText(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestDiscordWebURLAcceptsOnlyAddressesItCanPublish(t *testing.T) {
	for raw, want := range map[string]bool{
		"https://memql.io/docs/":           true,
		"  https://memql.io/docs/  ":       true,
		"http://localhost:8080/":           true,
		"https://os.example.test/?a=1&b=2": true,
		"https://user:pw@memql.io/":        false,
		"https://@memql.io/":               false,
		"ftp://memql.io/":                  false,
		"javascript:alert(1)":              false,
		"//memql.io/docs":                  false,
		"/docs":                            false,
		"memql.io/docs":                    false,
		"https://":                         false,
		"https:memql.io":                   false,
		"https://memql.io/a b":             false,
		"https://memql.io/a\nb":            false,
		"https://memql.io/\x00":            false,
		"https://memql.io/\n":              true, // surrounding white space is trimmed, as a YAML scalar leaves it
		"":                                 false,
		"https://memql.io:notaport/":       false,
	} {
		if _, ok := webURL(raw); ok != want {
			t.Errorf("webURL(%q) accepted = %v, want %v", raw, ok, want)
		}
	}
	if got := markdownDestination("https://x.test/a(b)c<d>e\\f"); got != "https://x.test/a%28b%29c%3Cd%3Ee%5Cf" {
		t.Errorf("markdownDestination = %q", got)
	}
}

// Past the cap on the whole embed, the message sheds what the reader needs
// least first: the Artifacts lines, then the description's first line (the
// commit's own words, kept as long as there is room), and the outcome line
// last of all.
func TestDiscordMessageShedsArtifactsBeforeItShortensTheDescription(t *testing.T) {
	base := releasePassed()
	base.Title = strings.Repeat("t", 3_900)
	base.Links = []Link{{Label: "Docs", URL: "https://memql.io/" + strings.Repeat("d", 480)}}

	without, _ := composeDiscord(t, base)
	if size := embedSize(without); size > 6000 || size < 4900 {
		t.Fatalf("the fixture without artifacts is %d characters; it must fit, and leave less than an Artifacts field's room", size)
	}

	with := base
	with.Artifacts = numberedArtifacts(9, 140)
	var unshed []string
	for _, a := range with.Artifacts[:5] {
		unshed = append(unshed, "["+a.Label+"]("+a.URL+")")
	}
	unshedSize := utf8.RuneCountInString(strings.Join(unshed, "\n") + "\nand 4 more on the run page")
	if embedSize(without)+len("Artifacts")+unshedSize <= 6000 {
		t.Fatalf("the fixture's artifacts fit whole (%d + %d), so nothing would be shed", embedSize(without), unshedSize)
	}

	e, _ := composeDiscord(t, with)
	assertWithinDiscordLimits(t, e)
	if e.Description != without.Description {
		t.Errorf("the description was shortened while Artifacts lines could still go:\n got %.60q...\nwant %.60q...", e.Description, without.Description)
	}
	if !reflect.DeepEqual(e.Fields[:len(without.Fields)], without.Fields) {
		t.Errorf("a field other than Artifacts changed:\n got %+v\nwant %+v", e.Fields[:len(without.Fields)], without.Fields)
	}

	lines := strings.Split(fieldNamed(t, e, "Artifacts").Value, "\n")
	shown := 0
	var more int
	for _, line := range lines {
		if _, err := fmt.Sscanf(line, "and %d more on the run page", &more); err == nil {
			continue
		}
		shown++
	}
	if shown >= 5 || shown < 1 {
		t.Errorf("Artifacts shows %d lines, want fewer than 5 and at least 1: %q", shown, lines)
	}
	if shown+more != 9 {
		t.Errorf("Artifacts shows %d and says %d more, but the run has 9", shown, more)
	}
}

func TestDiscordMessageShortensTheDescriptionWhenTheArtifactsAreNotEnough(t *testing.T) {
	n := releasePassed()
	n.Title = strings.Repeat("t", 3_900)
	for i := range 5 {
		n.Links = append(n.Links, Link{Label: fmt.Sprintf("Link %d", i+1), URL: "https://memql.io/" + strings.Repeat("d", 480)})
	}
	n.Artifacts = numberedArtifacts(9, 40)

	e, _ := composeDiscord(t, n)
	assertWithinDiscordLimits(t, e)
	for _, f := range e.Fields {
		if f.Name == "Artifacts" {
			t.Errorf("Artifacts survived while the description was cut: %+v", f)
		}
	}
	for i := 1; i <= 5; i++ {
		got := fieldNamed(t, e, fmt.Sprintf("Link %d", i)).Value
		if want := "[memql.io/" + strings.Repeat("d", 480) + "](https://memql.io/" + strings.Repeat("d", 480) + ")"; got != want {
			t.Errorf("link %d was altered while the description was cut", i)
		}
	}
	// The commit's words give way; what happened to the run does not.
	lines := strings.Split(e.Description, "\n")
	if len(lines) != 2 || lines[1] != "All 4 stages passed in 5m 58s." {
		t.Fatalf("description lines = %.80q, want 2 lines ending in the outcome", lines)
	}
	if !strings.HasPrefix(lines[0], "ttt") || !strings.HasSuffix(lines[0], "...") || utf8.RuneCountInString(lines[0]) >= 3_900 {
		t.Errorf("the first line = %.20q...%q, want the title cut short and marked", lines[0], lines[0][max(0, len(lines[0])-10):])
	}
	// And it is cut to the room that is left, not further: nothing is wasted.
	if got := embedSize(e); got != 6000 {
		t.Errorf("the embed is %d characters; the description should take every one of the 6000 the fields leave it", got)
	}
}

// When the fields alone are over the cap, the LAST fields go, one at a time,
// and the three that say what ran and where to read about it do not. What
// room the last field's going leaves is the description's.
func TestDiscordMessageDropsTrailingFieldsOnlyAsALastResort(t *testing.T) {
	n := releasePassed()
	n.Title = strings.Repeat("t", 3_900)
	for i := range 25 {
		n.Links = append(n.Links, Link{Label: fmt.Sprintf("Link %d", i+1), URL: "https://memql.io/" + strings.Repeat("d", 480)})
	}
	n.Artifacts = numberedArtifacts(3, 1)

	e, _ := composeDiscord(t, n)
	assertWithinDiscordLimits(t, e)
	want := []string{"Version", "MemQL OS", "Deployment details", "Link 1", "Link 2", "Link 3", "Link 4", "Link 5"}
	if got := fieldNames(e); !reflect.DeepEqual(got, want) {
		t.Errorf("fields = %q, want %q", got, want)
	}
	if got := embedSize(e); got != 6000 {
		t.Errorf("the embed is %d characters; the room the dropped fields left should have gone to the description", got)
	}
	if lines := strings.Split(e.Description, "\n"); len(lines) != 2 || lines[1] != "All 4 stages passed in 5m 58s." {
		t.Errorf("description lines = %.80q, want 2 lines ending in the outcome", lines)
	}
}

func TestDiscordMessageHoldsTheFieldCountWithoutLosingTheArtifacts(t *testing.T) {
	n := releasePassed()
	for i := range 40 {
		n.Links = append(n.Links, Link{Label: fmt.Sprintf("L%d", i+1), URL: fmt.Sprintf("https://memql.io/%d", i+1)})
	}
	n.Artifacts = numberedArtifacts(2, 1)
	e, _ := composeDiscord(t, n)
	assertWithinDiscordLimits(t, e)
	if len(e.Fields) != 25 {
		t.Errorf("%d fields, want exactly Discord's 25", len(e.Fields))
	}
	if last := e.Fields[len(e.Fields)-1].Name; last != "Artifacts" {
		t.Errorf("the last field is %q, want Artifacts: the links give way, not the run's files", last)
	}
	if got := e.Fields[3].Name; got != "L1" {
		t.Errorf("the first link is %q, want L1: the links keep their order", got)
	}
}

// The email is the same message for a reader who sees plain text: the same
// words, no Markdown, so nothing is escaped, and a subject is one line.
func TestEmailMessageOfAPassedRelease(t *testing.T) {
	n := releasePassed()
	n.Links = []Link{{Label: "Docs", URL: "https://memql.io/docs/"}}
	n.Artifacts = []Link{
		{Label: "memql-linux-amd64.tar.gz", URL: "https://os.example.test/?libraryFile=f1"},
		{Label: "SHA256SUMS", URL: "https://os.example.test/?libraryFile=f2"},
	}
	subject, body := composeEmail(t, n)
	if want := "memql · Release v0.21.7 passed"; subject != want {
		t.Errorf("subject = %q, want %q", subject, want)
	}
	want := "Add the notify stage (a944ae3)\n" +
		"All 4 stages passed in 5m 58s.\n" +
		"\n" +
		"Version: v0.21.7\n" +
		"MemQL OS: https://os.example.test/\n" +
		"Deployment details: https://os.example.test/?pipelineRun=run1\n" +
		"Docs: https://memql.io/docs/\n" +
		"Artifacts:\n" +
		"- memql-linux-amd64.tar.gz: https://os.example.test/?libraryFile=f1\n" +
		"- SHA256SUMS: https://os.example.test/?libraryFile=f2\n"
	if body != want {
		t.Errorf("body:\n--- got ---\n%s\n--- want ---\n%s", body, want)
	}
}

func TestEmailMessageOfAFailedRunOnAClusterWithNoOSDomain(t *testing.T) {
	n := releasePassed()
	n.Event, n.Outcome, n.Title = EventPush, NotifyFailed, "Fix the cart badge"
	n.Version = goldenMerged
	n.OSOrigin, n.RunPageURL = "", ""
	n.FailedStep, n.FailedCode, n.FailedMessage = "tests/go-tests#2", "pipeline_step_timeout", "The step ran past its 20m timeout."
	subject, body := composeEmail(t, n)
	if want := "memql · Push to main failed at tests"; subject != want {
		t.Errorf("subject = %q, want %q", subject, want)
	}
	want := "Fix the cart badge (a944ae3)\n" +
		"tests/go-tests#2 failed: pipeline_step_timeout. The step ran past its 20m timeout.\n" +
		"\n" +
		"Version: a944ae3\n" +
		"Deployment details: Not available: this cluster has no MemQL OS domain configured.\n"
	if body != want {
		t.Errorf("body:\n--- got ---\n%s\n--- want ---\n%s", body, want)
	}
}

// The same five-and-the-rest rule: a run with hundreds of files is announced
// with the run page, not with a list nobody scrolls.
func TestEmailMessageListsFiveArtifactsAndCountsTheRest(t *testing.T) {
	n := releasePassed()
	n.Artifacts = numberedArtifacts(9, 1)
	_, body := composeEmail(t, n)
	want := "Artifacts:\n" +
		"- 1.tgz: https://os.example.test/?libraryFile=f1\n" +
		"- 2.tgz: https://os.example.test/?libraryFile=f2\n" +
		"- 3.tgz: https://os.example.test/?libraryFile=f3\n" +
		"- 4.tgz: https://os.example.test/?libraryFile=f4\n" +
		"- 5.tgz: https://os.example.test/?libraryFile=f5\n" +
		"and 4 more on the run page\n"
	if !strings.HasSuffix(body, want) {
		t.Errorf("body does not end with the artifacts block:\n%s", body)
	}
}

// A mail client shows plain text: a backslash a Markdown escape added would be
// read, so the email carries the words as written, and a subject cannot be
// made into two header lines.
func TestEmailMessageIsPlainTextOnOneSubjectLine(t *testing.T) {
	n := releasePassed()
	n.Event, n.Branch = EventPush, "feature_x\r\nBcc: someone@example.test"
	n.Title = "[click](https://evil.test) @everyone **x**"
	n.Outcome = NotifyFailed
	// The step and its code are text a manifest and a runner wrote: they may
	// carry a line break and Markdown, and neither may start a line of the
	// email or a header of its own.
	n.FailedStep = "tests/go-tests\r\nX-Injected: step"
	n.FailedCode = "pipeline_step_failed\r\nX-Injected: *code*"
	n.FailedMessage = "exit status 1\n> quoted"
	n.Links = []Link{{Label: "Release *notes*", URL: "https://memql.io/docs/a_b"}}

	subject, body := composeEmail(t, n)
	if want := "memql · Push to feature_x Bcc: someone@example.test failed at tests"; subject != want {
		t.Errorf("subject = %q, want %q", subject, want)
	}
	if strings.ContainsAny(subject, "\r\n") {
		t.Errorf("the subject spans lines: %q", subject)
	}
	if strings.Contains(body, `\`) {
		t.Errorf("the plain-text body carries a Markdown escape:\n%s", body)
	}
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(line, "X-Injected") || strings.HasPrefix(line, "Bcc") {
			t.Errorf("a line break in the step or its code started a line of its own: %q", line)
		}
	}
	for _, want := range []string{
		"[click](https://evil.test) @everyone **x** (a944ae3)\n",
		"tests/go-tests X-Injected: step failed: pipeline_step_failed X-Injected: *code*. exit status 1 > quoted\n",
		"Release *notes*: https://memql.io/docs/a_b\n",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body lacks %q:\n%s", want, body)
		}
	}
}

func TestEmailMessageHoldsItsSubjectToOneLineOfSaneLength(t *testing.T) {
	subject, _ := composeEmail(t, hostileNotification())
	if got := utf8.RuneCountInString(subject); got > 256 {
		t.Errorf("the subject is %d runes", got)
	}
	if strings.ContainsAny(subject, "\r\n") {
		t.Errorf("the subject spans lines: %q", subject[:40])
	}
}

// A subject is plain text, so where the Discord title escapes a branch's
// underscores the email's does not: the two are equal when nothing needed
// escaping, which is every branch a person reads.
func TestEmailSubjectEqualsTheDiscordTitleExceptWhereMarkdownNeededEscaping(t *testing.T) {
	n := releasePassed()
	n.Event, n.Branch = EventPush, "release_candidate"
	e, _ := composeDiscord(t, n)
	subject, _ := composeEmail(t, n)
	if e.Title != `memql · Push to release\_candidate passed` || subject != "memql · Push to release_candidate passed" {
		t.Errorf("title = %q, subject = %q", e.Title, subject)
	}
}

// A sentence the driver writes beside a message spells a time the way the
// message and the check run do.
func TestFormatDurationIsTheChecksOwnFormat(t *testing.T) {
	for d, want := range map[time.Duration]string{
		time.Second:      "1s",
		45 * time.Second: "45s",
		62 * time.Second: "1m 02s",
		20 * time.Minute: "20m 00s",
		time.Hour + 2*time.Minute + 5*time.Second: "1h 02m 05s",
	} {
		if got := FormatDuration(d); got != want {
			t.Errorf("FormatDuration(%v) = %q, want %q", d, got, want)
		}
	}
}
