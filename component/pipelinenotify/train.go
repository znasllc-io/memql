package pipelinenotify

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// train.go -- what a release train says in Discord (design record
// docs/superpowers/specs/2026-10-27-release-trains-design.md, section 9).
//
// Discord is the owner's channel for a train: every status, and every action
// only the owner can take. The two kinds must be told apart at a glance, so
// an action-needed message opens with ACTION NEEDED and is the only kind that
// mentions anyone -- and only the one Discord user the train settings name.
// That mention is the message's `content`, because Discord never notifies a
// mention inside an embed, and allowed_mentions names that user alone, so a
// pull request title carrying "@everyone" or "<@id>" stays text.
//
// It shares the run messages' escapers, limits and wire structs (notify.go):
// one composer's rules, not a second set that drifts.

// TrainEvent is what a train announces.
type TrainEvent string

const (
	TrainDeparted         TrainEvent = "departed"
	TrainDropped          TrainEvent = "dropped"
	TrainHeld             TrainEvent = "held"
	TrainAwaitingApproval TrainEvent = "awaiting_approval"
	TrainReminder         TrainEvent = "reminder"
	TrainLanded           TrainEvent = "landed"
	TrainAdmissionHeld    TrainEvent = "admission_held"
	// TrainCancelled says a train the owner has seen will not land, so a
	// train never goes quiet after asking for anything.
	TrainCancelled TrainEvent = "cancelled"
	// TrainStalled says a train has stopped moving for a reason the owner
	// may need to fix: no run started, or a step that keeps failing.
	TrainStalled TrainEvent = "stalled"
)

// ActionNeeded reports whether the event asks something of the owner.
func (e TrainEvent) ActionNeeded() bool {
	switch e {
	case TrainHeld, TrainAwaitingApproval, TrainReminder, TrainAdmissionHeld, TrainStalled:
		return true
	}
	return false
}

// TrainItem is a pull request a message lists: on board, left off or dropped.
type TrainItem struct {
	Number             int
	Title, URL, Reason string
}

// TrainMessage is everything a train message says. Text that a person or a
// repository wrote (titles, reasons, failed step names) is escaped here.
type TrainMessage struct {
	Event                                                   TrainEvent
	Repository, Branch, Version, PullRequestURL, RunPageURL string
	OSOrigin                                                string
	OnBoard, LeftOff                                        []TrainItem
	Dropped                                                 *TrainItem
	Failed                                                  []string
	Reason, MentionUserID                                   string
	At                                                      time.Time
}

const trainUsername = "MemQL Pipelines"

// The embed colors: the run messages' red and green, blue for a departure,
// orange for anything that waits on the owner.
const (
	trainColorAction   = 15105570
	trainColorDeparted = 3447003
	trainColorDropped  = 15158332
	trainColorLanded   = 3066993
	trainColorEnded    = 9807270
)

var trainMentionRe = regexp.MustCompile(`^[0-9]{17,20}$`)

// TrainDiscordMessage composes the webhook payload for a train event.
func TrainDiscordMessage(m TrainMessage) ([]byte, error) {
	if m.MentionUserID != "" && !trainMentionRe.MatchString(m.MentionUserID) {
		return nil, fmt.Errorf("pipelines: %q is not a Discord user id to mention", m.MentionUserID)
	}
	branch := escapeDiscord(oneLineText(m.Branch))
	c, err := trainCopy(m, branch)
	if err != nil {
		return nil, err
	}
	embed := discordEmbed{
		Title:       fitRunes(escapeDiscord(oneLineText(m.Repository))+" · "+c.headline, discordTitleMax),
		Description: fitRunes(c.description, discordDescriptionMax),
		Color:       c.color,
	}
	prURL, hasPR := webURL(m.PullRequestURL)
	if hasPR {
		embed.URL = prURL
	}
	if !m.At.IsZero() {
		embed.Timestamp = m.At.UTC().Format(time.RFC3339)
	}

	train := branch
	if hasPR {
		train = markdownLink(branch, markdownDestination(prURL), discordFieldValueMax)
	}
	embed.Fields = append(embed.Fields, discordField{Name: "Train", Value: train})
	if v := escapeDiscord(oneLineText(m.Version)); v != "" {
		embed.Fields = append(embed.Fields, discordField{Name: "Version", Value: fitRunes(v, discordFieldValueMax)})
	}
	if strings.TrimSpace(m.RunPageURL) != "" {
		embed.Fields = append(embed.Fields, discordField{Name: "Run", Value: discordRunPage(m.RunPageURL)})
	}
	if c.todo != "" {
		embed.Fields = append(embed.Fields, discordField{Name: "What you need to do", Value: c.todo})
	}
	if len(m.OnBoard) > 0 {
		embed.Fields = append(embed.Fields, discordField{Name: "On board", Value: trainItemLines(m.OnBoard, false)})
	}
	if len(m.LeftOff) > 0 {
		embed.Fields = append(embed.Fields, discordField{Name: "Left off", Value: trainItemLines(m.LeftOff, true)})
	}
	if m.Dropped != nil {
		embed.Fields = append(embed.Fields, discordField{Name: "Dropped", Value: trainItemLines([]TrainItem{*m.Dropped}, true)})
	}
	if len(m.Failed) > 0 {
		var failed []string
		for _, f := range m.Failed {
			failed = append(failed, escapeDiscord(oneLineText(f)))
		}
		embed.Fields = append(embed.Fields, discordField{Name: "Failed", Value: fitRunes(strings.Join(failed, ", "), discordFieldValueMax)})
	}
	if raw, ok := osAddress(m.OSOrigin); ok {
		embed.Fields = append(embed.Fields, discordField{Name: "MemQL OS", Value: markdownLink("Open", markdownDestination(raw), discordFieldValueMax)})
	}
	fitTrainEmbed(&embed)

	payload := discordPayload{
		Username:        trainUsername,
		AllowedMentions: discordMentions{Parse: []string{}},
		Embeds:          []discordEmbed{embed},
	}
	if m.Event.ActionNeeded() && m.MentionUserID != "" {
		payload.Content = "<@" + m.MentionUserID + "> " + c.headline
		payload.AllowedMentions.Users = []string{m.MentionUserID}
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(payload); err != nil {
		return nil, fmt.Errorf("pipelines: encode the train message: %w", err)
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n")), nil
}

type trainWords struct {
	headline, description, todo string
	color                       int
}

const (
	trainTodoApprove = "Review the train pull request on GitHub and approve it. It lands within a minute of your approval."
	trainTodoHeld    = "Close the train pull request to cancel this train, or add train:hold to a pull request on board to rebuild without it. If main itself fails, land its fix on an express train first: this train's pull requests leave on a new train as soon as it lands."
	trainTodoAdmit   = "Open an admission window for the build pool: the Azure cost guard is holding new runs."
	trainTodoStalled = "Fix what the reason names; the train goes on by itself once it clears. Close the train pull request to cancel it instead."
)

func trainCopy(m TrainMessage, branch string) (trainWords, error) {
	reason := escapeDiscord(oneLineText(m.Reason))
	or := func(s, fallback string) string {
		if s != "" {
			return s
		}
		return fallback
	}
	switch m.Event {
	case TrainDeparted:
		return trainWords{
			headline:    "Train " + branch + " left with " + plural(len(m.OnBoard), "pull request"),
			description: fmt.Sprintf("%s on board, %d left off. The full run starts now.", plural(len(m.OnBoard), "pull request"), len(m.LeftOff)),
			color:       trainColorDeparted,
		}, nil
	case TrainDropped:
		if m.Dropped == nil {
			return trainWords{}, fmt.Errorf("pipelines: a dropped message must name the pull request it dropped")
		}
		return trainWords{
			headline:    "Dropped #" + strconv.Itoa(m.Dropped.Number) + " from " + branch,
			description: or(reason, "A pull request on this train broke its run, so the train goes on without it."),
			color:       trainColorDropped,
		}, nil
	case TrainHeld:
		return trainWords{headline: "ACTION NEEDED: " + branch + " is held", description: or(reason, "This train needs you before it can go on."), todo: trainTodoHeld, color: trainColorAction}, nil
	case TrainAwaitingApproval:
		return trainWords{headline: "ACTION NEEDED: approve " + branch, description: "The full run passed. Approve the train pull request on GitHub to land it.", todo: trainTodoApprove, color: trainColorAction}, nil
	case TrainReminder:
		return trainWords{headline: "ACTION NEEDED: " + branch + " is still waiting for your approval", description: or(reason, "This train passed and is still waiting for your approval."), todo: trainTodoApprove, color: trainColorAction}, nil
	case TrainLanded:
		return trainWords{headline: "PASSED: " + branch + " landed on main", description: or(reason, "The train landed on main."), color: trainColorLanded}, nil
	case TrainAdmissionHeld:
		return trainWords{headline: "ACTION NEEDED: the cost guard is holding " + branch + "'s run", description: or(reason, "The build pool has not admitted this train's run."), todo: trainTodoAdmit, color: trainColorAction}, nil
	case TrainCancelled:
		return trainWords{headline: "Train " + branch + " was cancelled", description: or(reason, "This train will not land; its pull requests board the next one."), color: trainColorEnded}, nil
	case TrainStalled:
		return trainWords{headline: "ACTION NEEDED: " + branch + " is stuck", description: or(reason, "This train has stopped moving."), todo: trainTodoStalled, color: trainColorAction}, nil
	}
	return trainWords{}, fmt.Errorf("pipelines: %q is not a train event", m.Event)
}

// trainItemLines lists pull requests one per line: "#N" (linked when the
// item has a page) and its title, or its reason. "#N" is this package's own
// text, so it opens the line as it is; everything after it is escaped.
func trainItemLines(items []TrainItem, withReason bool) string {
	var lines []string
	for _, it := range items {
		label := "#" + strconv.Itoa(it.Number)
		if raw, ok := webURL(it.URL); ok {
			label = "[" + label + "](" + markdownDestination(raw) + ")"
		}
		text := escapeDiscord(oneLineText(it.Title))
		if withReason {
			reason := escapeDiscord(oneLineText(it.Reason))
			switch {
			case text != "" && reason != "":
				text += ": " + reason
			case reason != "":
				text = reason
			}
		}
		lines = append(lines, strings.TrimSpace(label+" "+text))
	}
	return fitLines(lines, discordFieldValueMax)
}

// fitTrainEmbed holds the whole embed to Discord's 6000: the longest field is
// halved until it fits. Lists are what grow, and a shortened list still says
// what it is; the train pull request holds the whole.
func fitTrainEmbed(e *discordEmbed) {
	for e.size() > discordEmbedMax {
		longest := -1
		for i, f := range e.Fields {
			if longest < 0 || utf8.RuneCountInString(f.Value) > utf8.RuneCountInString(e.Fields[longest].Value) {
				longest = i
			}
		}
		if longest < 0 {
			return
		}
		n := utf8.RuneCountInString(e.Fields[longest].Value)
		if n <= 16 {
			return
		}
		e.Fields[longest].Value = fitRunes(e.Fields[longest].Value, n/2)
	}
}

func plural(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return strconv.Itoa(n) + " " + noun + "s"
}
