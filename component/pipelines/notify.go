package pipelines

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// notify.go -- the message a notify stage sends (epic memql#5480, design
// record D16): what a pipeline's run looks like on a channel, composed as a
// function over values so it can be read in a test with nothing running.
//
// It replaces the Python observer's Discord message, field for field -- the
// version, the MemQL OS link, and the run page where the observer promised
// "Coming soon in MemQL OS." -- and adds the one thing the observer could not
// say: what failed.
//
// The text it carries is a repository's or a person's, and it lands somewhere
// that interprets it: Discord renders Markdown and mentions, and a mail client
// reads a Subject header. So the composers neutralize what they are given, and
// hold the result to the limits Discord answers with a refusal.

// NotifyOutcome is what a notification announces.
type NotifyOutcome string

const (
	NotifyPassed NotifyOutcome = "passed"
	NotifyFailed NotifyOutcome = "failed"
	// NotifyRecovered is a pass after the previous run of this pipeline and
	// event did not pass: the news a channel is for.
	NotifyRecovered NotifyOutcome = "recovered"
)

// Valid reports whether o is an outcome a notification announces: passed,
// failed or recovered. Both composers refuse any other, and a caller that would
// rather know first can ask.
func (o NotifyOutcome) Valid() bool {
	_, ok := outcomeColors[o]
	return ok
}

// Link is a labelled URL in a notification: one a manifest's notify stage
// declares (Docs), or one of the run's Library files.
type Link struct {
	Label string `json:"label" yaml:"label"`
	URL   string `json:"url" yaml:"url"`
}

// Notification is everything a message says. Every string is already masked by
// the caller (MaskSecrets), so a composer renders what it is given.
//
// Every text in it that something outside this package wrote is collapsed to
// one line and escaped for the medium: Pipeline, Title, Branch, Version, the
// failed step's name, code and message, and the labels of Links and Artifacts.
// That includes FailedStep and FailedCode. Nothing here makes a code one from
// the catalogue -- the driver takes it from the executor's answer and only
// masks it, which is not vouching for it -- or a step's name one the manifest's
// grammar allows, so neither is a place a link may be written. The cost is that
// a Discord body spells pipeline_step_timeout as pipeline\_step\_timeout,
// which Discord draws as the code it is.
type Notification struct {
	Pipeline    string // the pipeline's name: "memql"
	Event       Event
	Version     string // MEMQL_VERSION: the tag for a release, the SHA otherwise
	SHA         string
	Branch      string
	PullRequest int
	Title       string // the commit message's first line or the pull request's title
	Outcome     NotifyOutcome
	Stages      int   // stages before the notify stage that ran
	DurationMs  int64 // the run so far
	// The first failed step, for NotifyFailed: its name as "stage/step" (a
	// refusal's scope) or "stage.step" (a step's key), its code and its message.
	FailedStep, FailedCode, FailedMessage string
	OSOrigin                              string // "https://os.<domain>", "" when unknown
	RunPageURL                            string // "" when unknown
	Links                                 []Link // the stage's own links (Docs)
	Artifacts                             []Link // the run's Library files
	At                                    time.Time
}

// Discord's limits on one embed, as its API documents them. A message over any
// of them is refused with a 400, and no retry cures it: the same body would be
// refused again. They are counted in runes, the unit Discord documents.
const (
	discordTitleMax       = 256
	discordDescriptionMax = 4096
	discordFieldNameMax   = 256
	discordFieldValueMax  = 1024
	discordFieldsMax      = 25
	discordEmbedMax       = 6000
)

const (
	notifyUsername = "MemQL Pipelines"

	// Green for a run that passed, which is the green the observer used, and
	// red for one that did not.
	colorPassed = 3066993  // 0x2ECC71
	colorFailed = 15158332 // 0xE74C3C

	// maxFailedMessageRunes is how much of a failure's own words the message
	// carries: enough to say what happened. When the message is a stretch of
	// output rather than a sentence, the run page holds all of it.
	maxFailedMessageRunes = 300

	// maxListedArtifacts is how many of the run's files a message names. A run
	// can produce hundreds, and the run page lists every one.
	maxListedArtifacts = 5

	ellipsis = "..."

	runPageUnavailable = "Not available: this cluster has no MemQL OS domain configured."

	// discordMarkdown are the characters Discord gives a meaning to in the
	// text a message carries: the escape itself, emphasis, strikethrough, code,
	// a quote or the close of a mention, a spoiler, and a link's brackets and
	// parentheses. "@" is not one of them on purpose: allowed_mentions.parse is
	// what stops a ping, and rewriting the text would misquote the commit.
	discordMarkdown = "\\*_~`>|[]()"
)

// DiscordMessage composes the webhook payload for n: one embed whose title is
// the run's headline, linked to the run page, with the version, the OS link and
// the run page as fields.
//
// An outcome that is not passed, failed or recovered is an error rather than a
// pass (see NotifyOutcome.Valid): a message that reads green for a run nobody
// judged is the false green pipelines exist to count.
//
// Nothing in the message may notify anyone, whatever a commit title says.
// allowed_mentions.parse is therefore always the empty array: present, and an
// array, because null or absent leaves Discord's defaults in force. A mention
// in the text stays text, written as the commit wrote it.
func DiscordMessage(n Notification) ([]byte, error) {
	if err := checkOutcome(n.Outcome); err != nil {
		return nil, err
	}
	payload := discordPayload{
		Username:        notifyUsername,
		AllowedMentions: discordMentions{Parse: []string{}},
		Embeds:          []discordEmbed{newEmbedPlan(n, outcomeColors[n.Outcome]).embed()},
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	// The reader is Discord's JSON decoder, which reads "&" and ">" as they
	// stand; leaving them readable keeps a stored body legible.
	enc.SetEscapeHTML(false)
	if err := enc.Encode(payload); err != nil {
		return nil, fmt.Errorf("pipelines: encode the Discord message: %w", err)
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n")), nil
}

// EmailMessage composes the same message for a mail client: the same lines in
// plain text. Nothing is escaped, because nothing is rendered; the subject is
// one line, since a line break in it would start a header.
//
// An outcome that is not passed, failed or recovered is refused exactly as
// DiscordMessage refuses it -- the same error, and no subject and no body -- so
// the two composers agree about what may be sent: an email that says nothing of
// how the run ended is no announcement, and is never a pass.
func EmailMessage(n Notification) (subject, body string, err error) {
	if err = checkOutcome(n.Outcome); err != nil {
		return "", "", err
	}
	subject = fitRunes(notifyTitle(n, asWritten), discordTitleMax)

	var b strings.Builder
	for _, line := range descriptionLines(n, asWritten) {
		b.WriteString(line + "\n")
	}
	if b.Len() > 0 {
		b.WriteString("\n")
	}
	for _, fact := range plainFacts(n) {
		b.WriteString(fact + "\n")
	}
	return subject, b.String(), nil
}

// Headline is the notification's one line, in plain text: what ran and how it
// ended, as "Release v0.21.7 passed" or "Push to main failed at tests". The
// Discord title and the email subject are the pipeline's name before it.
//
// It has no error to return, so an outcome it does not know leaves the verdict
// off rather than refusing: it says nothing of the run, and never that it
// passed.
func Headline(n Notification) string { return headline(n, asWritten) }

type discordPayload struct {
	Username        string          `json:"username"`
	AllowedMentions discordMentions `json:"allowed_mentions"`
	Embeds          []discordEmbed  `json:"embeds"`
}

type discordMentions struct {
	Parse []string `json:"parse"`
}

type discordEmbed struct {
	Title       string         `json:"title"`
	URL         string         `json:"url,omitempty"`
	Description string         `json:"description"`
	Color       int            `json:"color"`
	Fields      []discordField `json:"fields,omitempty"`
	Timestamp   string         `json:"timestamp,omitempty"`
}

type discordField struct {
	Name   string `json:"name"`
	Value  string `json:"value"`
	Inline bool   `json:"inline,omitempty"`
}

// size is Discord's measure of an embed against its 6000: the characters of
// the title, the description and every field's name and value.
func (e discordEmbed) size() int {
	size := utf8.RuneCountInString(e.Title) + utf8.RuneCountInString(e.Description)
	for _, f := range e.Fields {
		size += utf8.RuneCountInString(f.Name) + utf8.RuneCountInString(f.Value)
	}
	return size
}

// outcomeColors is every outcome a notification announces, with the color of
// the embed that announces it. It is the one place that says which there are:
// Valid asks it, and so does the Discord color, so the two cannot disagree.
var outcomeColors = map[NotifyOutcome]int{
	NotifyPassed:    colorPassed,
	NotifyFailed:    colorFailed,
	NotifyRecovered: colorPassed,
}

// checkOutcome is how both composers refuse an outcome they do not know, in
// the same words.
func checkOutcome(o NotifyOutcome) error {
	if o.Valid() {
		return nil
	}
	return fmt.Errorf("pipelines: a notification's outcome is %q; it must be %s, %s or %s",
		o, NotifyPassed, NotifyFailed, NotifyRecovered)
}

// embedPlan is an embed before the whole is held to Discord's limits: every
// part already within its own, and the pieces the whole's limit takes from
// kept apart so it can take them in order.
type embedPlan struct {
	title, url, timestamp string
	color                 int
	lines                 []string       // the description's lines
	fixed                 []discordField // Version, MemQL OS and Deployment details
	links                 []discordField // the stage's own links, within the field count
	artifacts             []string       // one rendered line per file the message can name
	maxShown              int            // how many artifact lines the field can hold: -1 for no field
}

func newEmbedPlan(n Notification, color int) embedPlan {
	p := embedPlan{
		title:     fitRunes(notifyTitle(n, escapeDiscord), discordTitleMax),
		color:     color,
		lines:     descriptionLines(n, escapeDiscord),
		artifacts: discordArtifactLines(n.Artifacts),
	}
	// Every line begins with text a person or a runner wrote: the first with the
	// commit's title, the failure's with the failed step's name.
	for i := range p.lines {
		p.lines[i] = atLineStart(p.lines[i])
	}
	// The title's link is a JSON property, not Markdown: the run page as it
	// was given, with nothing percent-encoded that the caller did not.
	if raw, ok := webURL(n.RunPageURL); ok {
		p.url = raw
	}
	if !n.At.IsZero() {
		p.timestamp = n.At.UTC().Format(time.RFC3339)
	}

	if version := versionText(n); version != "" {
		p.fixed = append(p.fixed, discordField{Name: "Version", Value: fitRunes(escapeDiscord(version), discordFieldValueMax), Inline: true})
	}
	if raw, ok := osAddress(n.OSOrigin); ok {
		p.fixed = append(p.fixed, discordField{Name: "MemQL OS", Value: markdownLink("Open", markdownDestination(raw), discordFieldValueMax)})
	}
	p.fixed = append(p.fixed, discordField{Name: "Deployment details", Value: discordRunPage(n.RunPageURL)})

	// One slot stays for Artifacts: the links give way to the run's files when
	// there are too many fields, not the other way round.
	room := discordFieldsMax - len(p.fixed)
	p.maxShown = -1
	if len(p.artifacts) > 0 {
		room--
		p.maxShown = min(len(p.artifacts), maxListedArtifacts)
		for p.maxShown > 0 && utf8.RuneCountInString(p.artifactsValue(p.maxShown)) > discordFieldValueMax {
			p.maxShown--
		}
	}
	p.links = discordLinkFields(n.Links, room)
	return p
}

// embed holds the whole to Discord's 6000 characters by taking from what the
// reader needs least, in this order: the Artifacts lines, one at a time and
// then the field; the description, down to the room the fields leave it; and
// last, only when the fields alone are over the limit, the fields at the end,
// one at a time -- never the first three, which say what ran and where to read
// about it. Whatever room a step leaves over goes to the description.
func (p embedPlan) embed() discordEmbed {
	for shown := p.maxShown; shown >= -1; shown-- {
		if e := p.assemble(shown, 0, discordDescriptionMax); e.size() <= discordEmbedMax {
			return e
		}
	}
	for dropped := 0; ; dropped++ {
		room := discordEmbedMax - p.assemble(-1, dropped, 0).size()
		if room >= 0 || dropped >= len(p.fixed)+len(p.links) {
			return p.assemble(-1, dropped, min(max(room, 0), discordDescriptionMax))
		}
	}
}

// assemble is the embed with the first `shown` Artifacts lines (a negative
// number leaves the field out), without the last `dropped` of the other
// fields, and with a description of at most descriptionLimit runes.
func (p embedPlan) assemble(shown, dropped, descriptionLimit int) discordEmbed {
	others := slices.Concat(p.fixed, p.links)
	others = others[:len(others)-min(dropped, len(others))]
	if shown >= 0 {
		others = append(others, discordField{Name: "Artifacts", Value: p.artifactsValue(shown)})
	}
	return discordEmbed{
		Title:       p.title,
		URL:         p.url,
		Description: fitLines(p.lines, descriptionLimit),
		Color:       p.color,
		Fields:      others,
		Timestamp:   p.timestamp,
	}
}

// artifactsValue is the Artifacts field with its first `shown` lines, and a
// last line that counts the rest.
func (p embedPlan) artifactsValue(shown int) string {
	lines := slices.Clone(p.artifacts[:shown])
	if hidden := len(p.artifacts) - shown; hidden > 0 {
		lines = append(lines, moreArtifacts(hidden))
	}
	return strings.Join(lines, "\n")
}

func moreArtifacts(hidden int) string {
	return "and " + strconv.Itoa(hidden) + " more on the run page"
}

// escaper makes the text a repository or a person wrote safe for the medium
// that renders it.
type escaper func(string) string

// asWritten is the escaper for a medium that renders nothing.
func asWritten(s string) string { return s }

// line is s on one line, then escaped: a line break in a title would start a
// line of Markdown the message did not mean to open, or a header in an email.
func (e escaper) line(s string) string { return e(oneLineText(s)) }

// escapeDiscord backslash-escapes every character Discord reads as Markdown,
// so that what a commit says renders as the text it is: `[click](https://...)`
// is not a link, `**x**` is not bold, and a mention's closing `>` is not one.
func escapeDiscord(s string) string {
	if !strings.ContainsAny(s, discordMarkdown) {
		return s
	}
	var b strings.Builder
	b.Grow(len(s) + 8)
	for _, r := range s {
		if strings.ContainsRune(discordMarkdown, r) {
			b.WriteByte('\\')
		}
		b.WriteRune(r)
	}
	return b.String()
}

// atLineStart makes escaped text safe to open a line. Markdown reads a
// heading, a subtext or a numbered list from a line's first characters, and a
// description line opens with text a person or a runner picked -- a commit's
// title that could be "# Release notes", a step's name that could be "- item".
// A backslash before the character that would make it one is dropped by the
// reader.
func atLineStart(s string) string {
	if s == "" {
		return s
	}
	if s[0] == '#' || s[0] == '-' {
		return `\` + s
	}
	if digits := len(s) - len(strings.TrimLeft(s, "0123456789")); digits > 0 && digits < len(s) && s[digits] == '.' {
		return s[:digits] + `\` + s[digits:]
	}
	return s
}

// oneLineText is s on a single line: every run of white space or control
// characters (a line break, a tab, a terminal escape from a log) becomes one
// space, and the ends are trimmed. It is how a title, a branch or a failure
// message is kept from starting a line or a header of its own.
func oneLineText(s string) string {
	return strings.Join(strings.FieldsFunc(s, isSpaceOrControl), " ")
}

func isSpaceOrControl(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }

// fitRunes keeps s within n runes. A cut ends in "...", and the mark counts
// toward n, so the result is never longer than the limit that asked for it.
//
// It cuts on a rune boundary, and never between the two characters of an
// escape: a backslash left at the end would escape the first dot of the mark.
// A run of backslashes at the cut has an odd length exactly when the last one
// is an escape's first half, so that one is dropped.
func fitRunes(s string, n int) string {
	if n <= 0 {
		return ""
	}
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	if n <= len(ellipsis) {
		return ellipsis[:n]
	}
	kept := string([]rune(s)[:n-len(ellipsis)])
	if (len(kept)-len(strings.TrimRight(kept, `\`)))%2 == 1 {
		kept = kept[:len(kept)-1]
	}
	return kept + ellipsis
}

// fitLines joins a description's lines within limit runes. What goes is the
// first line's tail, which is the commit's own words, because the lines after
// it say what happened to the run; the whole is cut only when even that is not
// enough.
func fitLines(lines []string, limit int) string {
	if limit <= 0 {
		return ""
	}
	text := strings.Join(lines, "\n")
	if utf8.RuneCountInString(text) <= limit {
		return text
	}
	if len(lines) > 1 {
		rest := strings.Join(lines[1:], "\n")
		if room := limit - utf8.RuneCountInString(rest) - 1; room > len(ellipsis) {
			return fitRunes(lines[0], room) + "\n" + rest
		}
	}
	return fitRunes(text, limit)
}

// notifyTitle is "<pipeline> · <headline>": the Discord title and the email
// subject. The dot is U+00B7, as in the observer's.
func notifyTitle(n Notification, esc escaper) string {
	head := headline(n, esc)
	if name := esc.line(n.Pipeline); name != "" {
		return name + " · " + head
	}
	return head
}

func headline(n Notification, esc escaper) string {
	subject := runSubject(n, esc)
	switch n.Outcome {
	case NotifyPassed:
		return subject + " passed"
	case NotifyRecovered:
		return subject + " recovered"
	case NotifyFailed:
		if stage := stageOfStep(oneLineText(n.FailedStep)); stage != "" {
			return subject + " failed at " + esc(stage)
		}
		return subject + " failed"
	}
	// An outcome this package does not know says nothing about the run: never
	// "passed" by default.
	return subject
}

// stageOfStep is the stage a failed step is in. A step is named "stage/step"
// where a refusal's scope is, and "stage.step" where a step's key is (StepKey):
// the stage is what comes before the first "/" or ".", neither of which a
// stage's name holds.
func stageOfStep(step string) string {
	if i := strings.IndexAny(step, "/."); i >= 0 {
		return step[:i]
	}
	return step
}

// runSubject names the run by what opened it.
func runSubject(n Notification, esc escaper) string {
	sha := esc.line(shortSHA(n.SHA))
	switch n.Event {
	case EventRelease:
		return words("Release", esc(versionText(n)))
	case EventPush:
		if branch := esc.line(n.Branch); branch != "" {
			return "Push to " + branch
		}
		return words("Push", sha)
	case EventPullRequest:
		if n.PullRequest > 0 {
			return "Pull request #" + strconv.Itoa(n.PullRequest)
		}
		return "Pull request"
	case EventMergeGroup:
		return words("Merge queue", sha)
	}
	return words("Run", sha)
}

// words joins the parts that are not empty with a space.
func words(parts ...string) string {
	kept := make([]string, 0, len(parts))
	for _, part := range parts {
		if part != "" {
			kept = append(kept, part)
		}
	}
	return strings.Join(kept, " ")
}

// shortSHA is a commit as a person reads one: its first seven characters.
func shortSHA(sha string) string { return cutBytes(oneLineText(sha), 7) }

// versionText is the version as a person reads it. A release's is its tag and
// is kept whole; every other run's MEMQL_VERSION is its commit, shown as the
// first seven characters, as is a run with no version at all.
func versionText(n Notification) string {
	version := oneLineText(n.Version)
	switch {
	case version == "":
		return shortSHA(n.SHA)
	case version == oneLineText(n.SHA), isFullHash(version):
		return shortSHA(version)
	}
	return version
}

// isFullHash reports whether s is a whole commit id: 40 hexadecimal
// characters, or 64 in a repository that hashes with SHA-256.
func isFullHash(s string) bool {
	if len(s) != 40 && len(s) != 64 {
		return false
	}
	for i := 0; i < len(s); i++ {
		if c := s[i]; !('0' <= c && c <= '9' || 'a' <= c && c <= 'f' || 'A' <= c && c <= 'F') {
			return false
		}
	}
	return true
}

// descriptionLines are the two lines under the title: the commit, then what
// happened to the run. The email carries the same two.
func descriptionLines(n Notification, esc escaper) []string {
	var lines []string
	if commit := commitLine(n, esc); commit != "" {
		lines = append(lines, commit)
	}
	if outcome := outcomeLine(n, esc); outcome != "" {
		lines = append(lines, outcome)
	}
	return lines
}

// commitLine is the commit's own title and its first seven characters.
func commitLine(n Notification, esc escaper) string {
	title, sha := esc.line(n.Title), esc.line(shortSHA(n.SHA))
	switch {
	case title != "" && sha != "":
		return title + " (" + sha + ")"
	case title != "":
		return title
	case sha != "":
		return "(" + sha + ")"
	}
	return ""
}

func outcomeLine(n Notification, esc escaper) string {
	switch n.Outcome {
	case NotifyPassed:
		return passedLine(n)
	case NotifyRecovered:
		return "Passed after the previous run failed. " + passedLine(n)
	case NotifyFailed:
		return failedLine(n, esc)
	}
	return ""
}

// passedLine counts the stages that ran and says how long they took. A count
// or a time nobody has is left out, not guessed.
func passedLine(n Notification) string {
	line := "All stages passed"
	if n.Stages > 0 {
		line = "All " + count(n.Stages, "stage", "stages") + " passed"
	}
	if n.DurationMs > 0 {
		line += " in " + formatDuration(n.DurationMs)
	}
	return line + "."
}

// failedLine names the step that failed, its code and what the runner said.
// All three are text something outside this package wrote, so all three are
// escaped. The message is cut to maxFailedMessageRunes before it is, so the cut
// is of the words as written and an escape is never split.
func failedLine(n Notification, esc escaper) string {
	who := "The run"
	if step := esc.line(n.FailedStep); step != "" {
		who = step
	}
	line := who + " failed"
	if code := esc.line(n.FailedCode); code != "" {
		line += ": " + code
	}
	line += "."
	if message := fitRunes(oneLineText(n.FailedMessage), maxFailedMessageRunes); message != "" {
		line += " " + esc(message)
	}
	return line
}

// osAddress is the cluster's MemQL OS as a link target, and false when the
// cluster has no OS domain: the address is derived from the domain by the
// caller and never composed from anything else here.
func osAddress(origin string) (string, bool) {
	origin = strings.TrimSpace(origin)
	if origin == "" {
		return "", false
	}
	return webURL(strings.TrimRight(origin, "/") + "/")
}

func discordRunPage(runPageURL string) string {
	if raw, ok := webURL(runPageURL); ok {
		return markdownLink("Open the run", markdownDestination(raw), discordFieldValueMax)
	}
	return runPageUnavailable
}

// discordLinkFields are the stage's links as fields, at most limit of them: the
// label names the field and the address it leads to is the text, so a reader
// sees where a link goes before they follow it. A link whose address cannot be
// made safe is left out.
func discordLinkFields(links []Link, limit int) []discordField {
	var fields []discordField
	for _, link := range links {
		if len(fields) >= limit {
			break
		}
		raw, ok := webURL(link.URL)
		if !ok {
			continue
		}
		name := escapeDiscord(oneLineText(link.Label))
		if name == "" {
			name = "Link"
		}
		fields = append(fields, discordField{
			Name:  fitRunes(name, discordFieldNameMax),
			Value: markdownLink(escapeDiscord(linkDisplay(raw)), markdownDestination(raw), discordFieldValueMax),
		})
	}
	return fields
}

// discordArtifactLines are the run's files as lines of the Artifacts field: a
// link named by the file, or by where it is when it has no name; the name alone
// when it has no address; and nothing for a file that has neither.
func discordArtifactLines(artifacts []Link) []string {
	var lines []string
	for _, artifact := range artifacts {
		name := oneLineText(artifact.Label)
		raw, ok := webURL(artifact.URL)
		switch {
		case ok && name == "":
			name = linkDisplay(raw)
			fallthrough
		case ok:
			lines = append(lines, markdownLink(escapeDiscord(name), markdownDestination(raw), discordFieldValueMax))
		case name != "":
			lines = append(lines, fitRunes(escapeDiscord(name), discordFieldValueMax))
		}
	}
	return lines
}

// linkDisplay is what a link reads as: its host and path, on one line.
func linkDisplay(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return oneLineText(raw)
	}
	return oneLineText(u.Host + u.Path)
}

// webURL reports whether raw is an address a message may publish -- http or
// https, with a host, no user name or password and no white space -- and
// returns it trimmed. A credential in a URL would be published to a channel
// whose members the pipeline's owner never chose, and white space or a control
// character would end the Markdown or the line around it.
func webURL(raw string) (string, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" || strings.ContainsFunc(raw, isSpaceOrControl) {
		return "", false
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Hostname() == "" || u.User != nil {
		return "", false
	}
	return raw, true
}

// destinationEscaper percent-encodes what would end a Markdown link's
// destination early (a parenthesis), or turn it into an autolink or an escape
// (an angle bracket, a backslash). The address it leads to is the same one.
var destinationEscaper = strings.NewReplacer("(", "%28", ")", "%29", "<", "%3C", ">", "%3E", `\`, "%5C")

func markdownDestination(raw string) string { return destinationEscaper.Replace(raw) }

// markdownLink is [label](destination) within limit runes. The label gives way
// and the address never does, because half an address is a link to somewhere
// else; a destination too long to leave room for any label is left out, and
// the label stands alone. label is already escaped.
func markdownLink(label, destination string, limit int) string {
	room := limit - utf8.RuneCountInString(destination) - len("[]()")
	if room < 1 {
		return fitRunes(label, limit)
	}
	return "[" + fitRunes(label, room) + "](" + destination + ")"
}

// plainFacts are the lines after the description in an email: the same fields
// the embed has, in the same order, as "name: value".
func plainFacts(n Notification) []string {
	var facts []string
	if version := versionText(n); version != "" {
		facts = append(facts, "Version: "+version)
	}
	if raw, ok := osAddress(n.OSOrigin); ok {
		facts = append(facts, "MemQL OS: "+raw)
	}
	if raw, ok := webURL(n.RunPageURL); ok {
		facts = append(facts, "Deployment details: "+raw)
	} else {
		facts = append(facts, "Deployment details: "+runPageUnavailable)
	}
	for _, link := range n.Links {
		raw, ok := webURL(link.URL)
		if !ok {
			continue
		}
		label := oneLineText(link.Label)
		if label == "" {
			label = "Link"
		}
		facts = append(facts, label+": "+raw)
	}

	var files []string
	for _, artifact := range n.Artifacts {
		name := oneLineText(artifact.Label)
		raw, ok := webURL(artifact.URL)
		switch {
		case ok && name == "":
			files = append(files, "- "+linkDisplay(raw)+": "+raw)
		case ok:
			files = append(files, "- "+name+": "+raw)
		case name != "":
			files = append(files, "- "+name)
		}
	}
	if len(files) > 0 {
		shown := min(len(files), maxListedArtifacts)
		facts = append(facts, "Artifacts:")
		facts = append(facts, files[:shown]...)
		if hidden := len(files) - shown; hidden > 0 {
			facts = append(facts, moreArtifacts(hidden))
		}
	}
	return facts
}
