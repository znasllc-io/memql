package pipelines

import (
	"fmt"
	"strings"
	"unicode/utf8"
)

// checkOutputLimit is GitHub's limit on each of a check run's output fields.
const checkOutputLimit = 65535

// StepReport is one step as the check run reports it.
type StepReport struct {
	Key, Stage, Name string
	Status           string // succeeded|failed|cancelled|skipped|refused|pending|running
	DurationMs       int64
	Code, Message    string
	LogTail          string
}

// RunReport is a run as the check run reports it.
type RunReport struct {
	Pipeline    string
	Mode        Mode
	Event       Event
	SHA         string
	PullRequest int
	Status      string // queued|in_progress|completed
	Conclusion  string // success|failure|cancelled|refused, when completed
	Refusal     *Refusal
	Steps       []StepReport
}

// CheckOutput renders the check run's title, summary and text (D4: the
// stage table as summary). Plain Markdown, no emojis; text carries the
// failed steps' last lines; each is cut to GitHub's 65535-byte field limit.
//
// The check run is the one surface of a pipeline a person reads on GitHub,
// so the copy follows the OS interface language: a state that is not settled
// never reads as a result (a stage still running says Running, not Passed,
// and the column is Status rather than Result for that reason); a refusal
// says what is wrong, where, and what to do; a time nobody measured is "-",
// never a guess. Text a manifest or a runner supplied is quoted literally:
// it cannot open a link, an image, an HTML tag or a table cell. The title is
// plain text, which is how GitHub shows it.
func CheckOutput(r RunReport) (title, summary, text string) {
	stages := reportStages(r)
	title = cutBytes(checkTitle(r, stages), checkOutputLimit)
	summary = cutBytes(checkSummary(r, stages), checkOutputLimit)
	text = cutBytes(checkText(r), checkOutputLimit)
	return title, summary, text
}

// refusalTitles is what a refused run's title says, by code. The scope, when
// there is one, follows in parentheses.
var refusalTitles = map[string]string{
	CodeNotDeclared:       "no pipeline block in memql-package.yaml",
	CodeStageInvalid:      "invalid stage",
	CodeStepInvalid:       "invalid step",
	CodeSelectInvalid:     "invalid select block",
	CodeSelectMissing:     "package selection not declared",
	CodeEventUnknown:      "unknown event",
	CodeBucketUnknown:     "unknown bucket",
	CodeServiceUnknown:    "unknown service",
	CodeNeedUnknown:       "unknown need",
	CodeSecretInvalid:     "invalid secret name",
	CodeSecretNotAllowed:  "secret not allowed",
	CodeFleetNotConsented: "fleet compute not allowed",
	CodeForkRefused:       "pull request from a fork",
	CodeDisconnected:      "pipeline disconnected",
}

// manifestRemedy is the remedy for every refusal a manifest edit fixes.
const manifestRemedy = "Correct the pipeline block in `memql-package.yaml` and push again."

// refusalRemedies is what a person does about a refused run, by code. A
// refusal that says what is wrong and not what to do is half a sentence.
var refusalRemedies = map[string]string{
	CodeNotDeclared:       "Add a pipeline block to `memql-package.yaml`, or disconnect the pipeline in MemQL OS.",
	CodeStageInvalid:      manifestRemedy,
	CodeStepInvalid:       manifestRemedy,
	CodeSelectInvalid:     manifestRemedy,
	CodeSelectMissing:     manifestRemedy,
	CodeEventUnknown:      manifestRemedy,
	CodeBucketUnknown:     manifestRemedy,
	CodeServiceUnknown:    manifestRemedy,
	CodeNeedUnknown:       manifestRemedy,
	CodeSecretInvalid:     manifestRemedy,
	CodeSecretNotAllowed:  "Allow the secret on the pipeline in MemQL OS, or remove it from the step.",
	CodeFleetNotConsented: "Allow fleet compute for the pipeline in MemQL OS, or remove the step's needs.",
	CodeForkRefused:       "Checks run only on branches in this repository. Push the branch here to run them.",
	CodeDisconnected:      "Reconnect the pipeline in MemQL OS to run checks again.",
}

// The stage table's Status values that are not built from a reason.
const (
	stagePassed    = "Passed"
	stageFailed    = "Failed"
	stageCancelled = "Cancelled"
	stageRunning   = "Running"
	stageWaiting   = "Waiting"
	stageNotRun    = "Not run"
	stageBlocked   = "Not run: an earlier stage failed"
	stageSkipped   = "Skipped"
)

// stageRow is one stage of a report, settled into a table row.
type stageRow struct {
	name   string
	steps  []StepReport
	tally  stepTally
	status string
	// ms is the stage's time: its longest step, since a stage's steps run at
	// once. 0 when the stage has not finished or a step that ran reported no
	// time.
	ms int64
}

type stepTally struct {
	passed, failed, refused, cancelled, skipped, blocked, running, waiting int
}

// reportStages groups a report's steps by stage, in the order the steps come.
func reportStages(r RunReport) []stageRow {
	var rows []stageRow
	at := map[string]int{}
	for _, s := range r.Steps {
		name := stageOf(s)
		i, ok := at[name]
		if !ok {
			i = len(rows)
			at[name] = i
			rows = append(rows, stageRow{name: name})
		}
		rows[i].steps = append(rows[i].steps, s)
	}
	completed := r.Status != "queued" && r.Status != "in_progress"
	for i := range rows {
		rows[i].settle(completed)
	}
	return rows
}

// stageOf is a step's stage: its Stage, or the part of its key before the
// first dot. A key is StepKey(stage, step), and the name grammar keeps a dot
// out of a stage name.
func stageOf(s StepReport) string {
	if s.Stage != "" {
		return s.Stage
	}
	stage, _, _ := strings.Cut(s.Key, ".")
	return stage
}

func (row *stageRow) settle(runCompleted bool) {
	t := &row.tally
	var reasons []string
	for _, s := range row.steps {
		switch s.Status {
		case "succeeded":
			t.passed++
		case "failed":
			t.failed++
		case "refused":
			t.refused++
		case "cancelled":
			t.cancelled++
		case "running":
			t.running++
		case "skipped":
			if s.Code == CodeStageBlocked {
				t.blocked++
				continue
			}
			t.skipped++
			if reason := strings.TrimSpace(s.Message); reason != "" && !containsString(reasons, reason) {
				reasons = append(reasons, reason)
			}
		default: // pending, or a status this package does not know
			t.waiting++
		}
	}
	// A stage is running while any step is, or once some step has finished
	// and others still wait. A step the compile skipped is settled before
	// the stage begins, so it does not make a waiting stage read as started.
	started := t.passed + t.failed + t.cancelled
	switch {
	case t.running > 0 || (t.waiting > 0 && started > 0):
		row.status = stageRunning
	case t.failed+t.refused > 0:
		row.status = stageFailed
	case t.cancelled > 0:
		row.status = stageCancelled
	case t.waiting > 0 && runCompleted:
		row.status = stageNotRun
	case t.waiting > 0:
		row.status = stageWaiting
	case t.blocked > 0 && t.passed == 0:
		row.status = stageBlocked
	case t.passed == 0:
		row.status = stageSkipped
		if len(reasons) > 0 {
			row.status += ": " + strings.Join(reasons, " ")
		}
	default:
		row.status = stagePassed
	}
	switch row.status {
	case stagePassed, stageFailed, stageCancelled:
		row.ms = stageTime(row.steps)
	}
}

// stageTime is the longest time among the steps that ran, or 0 when one of
// them reported none: a partial maximum would be a smaller number than the
// stage took.
func stageTime(steps []StepReport) int64 {
	var longest int64
	for _, s := range steps {
		switch s.Status {
		case "succeeded", "failed", "cancelled":
			if s.DurationMs <= 0 {
				return 0
			}
			longest = max(longest, s.DurationMs)
		}
	}
	return longest
}

// words is the Steps cell: how many steps ended each way, in a fixed order.
func (t stepTally) words() string {
	var parts []string
	for _, c := range []struct {
		n    int
		word string
	}{
		{t.passed, "passed"}, {t.failed, "failed"}, {t.refused, "refused"}, {t.cancelled, "cancelled"},
		{t.skipped, "skipped"}, {t.blocked, "not run"}, {t.running, "running"}, {t.waiting, "waiting"},
	} {
		if c.n > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", c.n, c.word))
		}
	}
	if len(parts) == 0 {
		return "-"
	}
	return strings.Join(parts, ", ")
}

func checkTitle(r RunReport, stages []stageRow) string {
	if r.Refusal != nil {
		if why := refusalTitle(r.Refusal); why != "" {
			return "Refused: " + why
		}
		return "Refused"
	}
	switch r.Status {
	case "queued":
		return "Queued"
	case "in_progress":
		if len(stages) == 0 {
			return "Preparing the run"
		}
		for _, want := range []string{stageRunning, stageWaiting} {
			for _, row := range stages {
				if row.status == want {
					return "Running: " + oneLine(row.name, 80)
				}
			}
		}
		return "Running"
	}
	switch conclusionOf(r) {
	case "cancelled":
		return "Cancelled"
	case "refused":
		return "Refused"
	case "failure":
		return failedTitle(r.Steps)
	}
	return passedTitle(stages)
}

// conclusionOf is the run's conclusion, or the one its steps imply when the
// report carries none.
func conclusionOf(r RunReport) string {
	if r.Conclusion != "" {
		return r.Conclusion
	}
	cancelled := false
	for _, s := range r.Steps {
		switch s.Status {
		case "failed", "refused":
			return "failure"
		case "cancelled":
			cancelled = true
		}
	}
	if cancelled {
		return "cancelled"
	}
	return "success"
}

func refusalTitle(ref *Refusal) string {
	why := refusalTitles[ref.Code]
	if why == "" {
		why = oneLine(firstLine(ref.Detail), 120)
	}
	if why == "" {
		why = ref.Code
	}
	if why != "" && ref.Scope != "" && ref.Code != CodeForkRefused {
		why += " (" + oneLine(ref.Scope, 80) + ")"
	}
	return why
}

func failedTitle(steps []StepReport) string {
	failed := failedSteps(steps)
	if len(failed) == 0 {
		return "Failed"
	}
	first := failed[0]
	title := "Failed at " + oneLine(stageOf(first), 80) + ": " + oneLine(first.Key, 120)
	if more := len(failed) - 1; more > 0 {
		title += fmt.Sprintf(" and %d more", more)
	}
	return title
}

// passedTitle counts the stages that ran and adds their times. A stage the
// plan skipped whole is in the table, not in the count, and a time nobody
// measured leaves the total out rather than understated.
func passedTitle(stages []stageRow) string {
	ran, known := 0, true
	var total int64
	for _, row := range stages {
		if row.status != stagePassed {
			continue
		}
		ran++
		if row.ms <= 0 {
			known = false
		}
		total += row.ms
	}
	if ran == 0 {
		return "Passed: nothing to run"
	}
	title := "Passed: " + count(ran, "stage", "stages")
	if known {
		title += " in " + formatDuration(total)
	}
	return title
}

func checkSummary(r RunReport, stages []stageRow) string {
	var b strings.Builder
	if meta := metaLine(r); meta != "" {
		b.WriteString(meta + "\n\n")
	}
	switch {
	case r.Refusal != nil:
		writeRefusal(&b, r.Refusal)
	case len(stages) == 0 && r.Status == "queued":
		b.WriteString("Waiting for the cluster to start this run.\n")
	case len(stages) == 0 && r.Status == "in_progress":
		b.WriteString("Reading `memql-package.yaml` and choosing the steps to run.\n")
	case len(stages) == 0:
		b.WriteString("No step ran.\n")
	default:
		b.WriteString("| Stage | Status | Steps | Time |\n| --- | --- | --- | --- |\n")
		for _, row := range stages {
			fmt.Fprintf(&b, "| %s | %s | %s | %s |\n",
				inlineText(row.name), inlineText(row.status), row.tally.words(), formatDuration(row.ms))
		}
		writeFailedSteps(&b, r.Steps)
	}
	return b.String()
}

// metaLine says which run this is: its mode, what opened it, its commit.
func metaLine(r RunReport) string {
	var parts []string
	if r.Mode != "" {
		parts = append(parts, "Mode "+inlineText(string(r.Mode)))
	}
	switch r.Event {
	case EventPullRequest:
		if r.PullRequest > 0 {
			parts = append(parts, fmt.Sprintf("Pull request #%d", r.PullRequest))
		} else {
			parts = append(parts, "Pull request")
		}
	case EventMergeGroup:
		parts = append(parts, "Merge queue")
	case EventPush:
		parts = append(parts, "Push to the default branch")
	case EventRelease:
		parts = append(parts, "Release")
	}
	if sha := strings.TrimSpace(r.SHA); sha != "" {
		if len(sha) > 7 {
			sha = sha[:7]
		}
		parts = append(parts, "Commit "+inlineText(sha))
	}
	return strings.Join(parts, " · ")
}

// writeRefusal renders a refused run: the refusal's own sentence with its
// code and scope, then the remedy. No table: no step ran.
func writeRefusal(b *strings.Builder, ref *Refusal) {
	where := ""
	if ref.Code != "" {
		where = codeSpan(ref.Code)
		if ref.Scope != "" {
			where += " in " + codeSpan(ref.Scope)
		}
	} else if ref.Scope != "" {
		where = "in " + codeSpan(ref.Scope)
	}
	detail := inlineText(ref.Detail)
	if detail == "" {
		detail = "The run was refused."
	}
	b.WriteString(withAside(detail, where) + "\n")
	if remedy := refusalRemedies[ref.Code]; remedy != "" {
		b.WriteString("\n" + remedy + "\n")
	}
}

// writeFailedSteps lists every failed step with its message and code, as
// many as fit, then says how many more there are.
func writeFailedSteps(b *strings.Builder, steps []StepReport) {
	failed := failedSteps(steps)
	if len(failed) == 0 {
		return
	}
	b.WriteString("\n### Failed steps\n\n")
	const reserve = 120 // room for the closing count
	for i, s := range failed {
		line := "- " + codeSpan(s.Key)
		if s.Status == "refused" {
			line += " (refused)"
		}
		message := clipRunes(inlineText(s.Message), 400)
		code := ""
		if s.Code != "" {
			code = codeSpan(s.Code)
		}
		switch {
		case message != "":
			line += ": " + withAside(message, code)
		case code != "":
			line += " (" + code + ")"
		}
		line += "\n"
		if b.Len()+len(line) > checkOutputLimit-reserve {
			more := len(failed) - i
			fmt.Fprintf(b, "\n%d more failed %s listed on the run page.\n", more, plural(more, "step is", "steps are"))
			return
		}
		b.WriteString(line)
	}
}

// checkText is the last lines of each failed step's output, as many whole as
// fit, then the last lines of the next one, then how many more there are.
func checkText(r RunReport) string {
	var withOutput []StepReport
	for _, s := range failedSteps(r.Steps) {
		if strings.TrimSpace(s.LogTail) != "" {
			withOutput = append(withOutput, s)
		}
	}
	var b strings.Builder
	const reserve = 160 // room for the closing count
	for i, s := range withOutput {
		tail := strings.ToValidUTF8(s.LogTail, "\uFFFD")
		if !strings.HasSuffix(tail, "\n") {
			tail += "\n"
		}
		sep := ""
		if b.Len() > 0 {
			sep = "\n"
		}
		room := checkOutputLimit - reserve - b.Len() - len(sep)
		if section := outputSection(s.Key, tail, false); len(section) <= room {
			b.WriteString(sep + section)
			continue
		}
		// It does not fit whole: its last lines, the end of a failing log
		// being where the failure is, when enough fit to be worth reading.
		more := len(withOutput) - i
		if fitted, ok := fitOutput(s.Key, tail, room); ok {
			b.WriteString(sep + fitted)
			more--
		}
		if more > 0 {
			fmt.Fprintf(&b, "\n%d more failed %s their output on the run page.\n", more, plural(more, "step has", "steps have"))
		}
		break
	}
	return b.String()
}

// outputSection is one step's output under its key, in a code block whose
// fence is longer than any run of backticks inside it, so the output cannot
// close the block.
func outputSection(key, tail string, cut bool) string {
	fence := strings.Repeat("`", max(3, longestRun(tail, '`')+1))
	var b strings.Builder
	b.WriteString("### " + inlineText(key) + "\n\n")
	if cut {
		b.WriteString("Showing the last lines only; the full output is on the run page.\n\n")
	}
	b.WriteString(fence + "text\n" + tail + fence + "\n")
	return b.String()
}

// fitOutput is the section for the last lines of tail that fit in room, and
// false when fewer than minTail bytes would.
func fitOutput(key, tail string, room int) (string, bool) {
	const minTail = 512
	overhead := len(outputSection(key, "", true))
	// The fence is sized by the whole tail, which can only be longer than any
	// run in the part kept.
	overhead += 2 * (max(3, longestRun(tail, '`')+1) - 3)
	keep := room - overhead
	if keep < minTail {
		return "", false
	}
	return outputSection(key, lastLines(tail, keep), true), true
}

// lastLines is the end of s in at most n bytes, starting on a line boundary
// when one falls inside the window, and on a rune boundary always.
func lastLines(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := s[len(s)-n:]
	if i := strings.IndexByte(cut, '\n'); i >= 0 && i+1 < len(cut) {
		return cut[i+1:]
	}
	for len(cut) > 0 && !utf8.RuneStart(cut[0]) {
		cut = cut[1:]
	}
	return cut
}

func failedSteps(steps []StepReport) []StepReport {
	var out []StepReport
	for _, s := range steps {
		if s.Status == "failed" || s.Status == "refused" {
			out = append(out, s)
		}
	}
	return out
}

// formatDuration renders a time as a person reads one: 45s, 1m 02s,
// 1h 02m 05s. "-" is a time nobody measured.
func formatDuration(ms int64) string {
	switch {
	case ms <= 0:
		return "-"
	case ms < 500:
		return "<1s"
	}
	secs := (ms + 500) / 1000
	h, m, s := secs/3600, secs%3600/60, secs%60
	switch {
	case h > 0:
		return fmt.Sprintf("%dh %02dm %02ds", h, m, s)
	case m > 0:
		return fmt.Sprintf("%dm %02ds", m, s)
	}
	return fmt.Sprintf("%ds", s)
}

// withAside puts a parenthetical before a sentence's closing period, so the
// sentence still ends where it did.
func withAside(sentence, aside string) string {
	switch {
	case aside == "":
		return sentence
	case strings.HasSuffix(sentence, "."):
		return strings.TrimSuffix(sentence, ".") + " (" + aside + ")."
	}
	return sentence + " (" + aside + ")"
}

// markdownInline are the characters that give inline Markdown or HTML its
// meaning: escapes, code spans, emphasis, strikethrough, links and images,
// tags and autolinks, table cells, entities.
const markdownInline = "\\`*_~[]<>|&"

// inlineText makes reported text literal on one line of GitHub Markdown:
// whitespace collapses to single spaces, and every character that could
// start inline Markdown or HTML is escaped, as is one that would make the
// line a heading, a list or a quote.
func inlineText(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	var b strings.Builder
	b.Grow(len(s))
	for i, r := range s {
		if strings.ContainsRune(markdownInline, r) || (i == 0 && strings.ContainsRune("#+-=", r)) {
			b.WriteByte('\\')
		}
		b.WriteRune(r)
	}
	out := b.String()
	// "1. text" and "1) text" open an ordered list.
	if digits := len(out) - len(strings.TrimLeft(out, "0123456789")); digits > 0 && digits < len(out) &&
		(out[digits] == '.' || out[digits] == ')') {
		out = out[:digits] + "\\" + out[digits:]
	}
	return out
}

// codeSpan quotes s as inline code: a delimiter one backtick longer than any
// run inside it, padded when s starts or ends with a backtick.
func codeSpan(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	fence := strings.Repeat("`", longestRun(s, '`')+1)
	if strings.HasPrefix(s, "`") || strings.HasSuffix(s, "`") {
		s = " " + s + " "
	}
	return fence + s + fence
}

// oneLine is s on one line, at most n runes, for the plain-text title.
func oneLine(s string, n int) string {
	return clipRunes(strings.Join(strings.Fields(s), " "), n)
}

// clipRunes keeps at most n runes of s, marking a cut with "...".
func clipRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	runes := []rune(s)
	return string(runes[:n]) + "..."
}

// cutBytes keeps at most n bytes of s, never splitting a rune.
func cutBytes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

func longestRun(s string, c byte) int {
	longest, run := 0, 0
	for i := 0; i < len(s); i++ {
		if s[i] == c {
			run++
			longest = max(longest, run)
		} else {
			run = 0
		}
	}
	return longest
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

func containsString(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}
