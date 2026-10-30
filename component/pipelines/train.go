package pipelines

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// train.go -- the decisions a release train makes before it touches GitHub
// (design record docs/superpowers/specs/2026-10-27-release-trains-design.md,
// sections 4.1 to 4.5): which repositories run trains and when they leave,
// which pull requests board, what the train's branch is called and what
// version it carries. Pure functions over values, so the coordinator in
// component/pipelinerun is the only half that reads a row or calls GitHub.
//
// WHY A TRAIN AT ALL. Every pull request here is the owner's own, so GitHub
// never lets one be approved and each merges through the admin bypass, which
// skips the merge queue: every merge leaves the others behind main to run the
// whole pipeline again. A train tests the ready pull requests once, together,
// and lands them through one approval the owner CAN give, on a pull request
// the MemQL app authors.

// TrainSettingsVariable is the global variable that turns trains on, read the
// way MEMQL_PIPELINES_REPOSITORY_CHANGE_RULES is. It is named here so a
// refusal can name it.
const TrainSettingsVariable = "MEMQL_PIPELINES_TRAINS"

// The labels a train reads, and the prefix that marks its own branches.
//
// train:hold is the owner's or a session's word: "not on a train yet".
// train:dropped is a train's: this pull request broke the last one; its
// session removes the label once a fix is pushed. A train/ head is a train's
// own pull request and is never a candidate for another train.
const (
	TrainHoldLabel    = "train:hold"
	TrainDroppedLabel = "train:dropped"
	TrainBranchPrefix = "train/"
)

// IsTrainEvent classifies the trusted run identity used by both run admission
// and cloud allocation. A repository's command/environment cannot choose it.
// A writer may name a train/ branch; that repository write access is already
// trusted by the release-train admission contract.
func IsTrainEvent(event Event, headBranch string) bool {
	return event == EventPullRequest && strings.HasPrefix(headBranch, TrainBranchPrefix)
}

// defaultTrainRebuilds is the cap on rebuilds in one departure when the
// settings name none: two, the owner's figure (spec section 6, step 6).
const defaultTrainRebuilds = 2

// TrainSettings is one repository's train configuration.
type TrainSettings struct {
	Repository         string // "owner/name", lower-cased
	Enabled            bool
	Departures         []string // "HH:MM" in TimeZone, at least one
	TimeZone           string   // IANA name
	Channel            string   // the notification channel messages go to
	OwnerDiscordUserID string   // the one Discord user an action-needed message mentions; may be empty
	MaxRebuilds        int      // 1..5
}

var (
	trainDepartureRe  = regexp.MustCompile(`^([01][0-9]|2[0-3]):[0-5][0-9]$`)
	trainDiscordIDRe  = regexp.MustCompile(`^[0-9]{17,20}$`)
	trainRepositoryRe = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)
	trainVersionRe    = regexp.MustCompile(`^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$`)
)

// ParseTrainSettings reads TrainSettingsVariable. An empty value is no trains
// anywhere, not an error: the variable is absent on every installation that
// has not opted in.
//
// Every refusal names the field or repository it is about. A malformed value
// sends no train anywhere, so the person reading the log must be able to fix
// it from the message alone.
func ParseTrainSettings(raw string) (map[string]TrainSettings, error) {
	out := map[string]TrainSettings{}
	if strings.TrimSpace(raw) == "" {
		return out, nil
	}
	var doc struct {
		Repositories map[string]struct {
			Enabled            bool     `json:"enabled"`
			Departures         []string `json:"departures"`
			TimeZone           string   `json:"timeZone"`
			Channel            string   `json:"channel"`
			OwnerDiscordUserID string   `json:"ownerDiscordUserId"`
			MaxRebuilds        int      `json:"maxRebuilds"`
		} `json:"repositories"`
	}
	dec := json.NewDecoder(bytes.NewReader([]byte(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&doc); err != nil {
		return nil, fmt.Errorf("%s is not a train settings object: %w", TrainSettingsVariable, err)
	}
	for key, e := range doc.Repositories {
		repo := strings.ToLower(strings.TrimSpace(key))
		if !trainRepositoryRe.MatchString(repo) {
			return nil, fmt.Errorf("%s: repository %q must be owner/name", TrainSettingsVariable, key)
		}
		if _, dup := out[repo]; dup {
			return nil, fmt.Errorf("%s: repository %q is configured twice", TrainSettingsVariable, key)
		}
		if len(e.Departures) == 0 {
			return nil, fmt.Errorf("%s: %s: departures must name at least one HH:MM time", TrainSettingsVariable, repo)
		}
		for _, d := range e.Departures {
			if !trainDepartureRe.MatchString(d) {
				return nil, fmt.Errorf("%s: %s: departures entry %q is not HH:MM (00:00 to 23:59)", TrainSettingsVariable, repo, d)
			}
		}
		if strings.TrimSpace(e.TimeZone) == "" {
			return nil, fmt.Errorf("%s: %s: timeZone is required (an IANA name such as America/Chicago)", TrainSettingsVariable, repo)
		}
		if _, err := time.LoadLocation(e.TimeZone); err != nil {
			return nil, fmt.Errorf("%s: %s: timeZone %q is not a known IANA zone", TrainSettingsVariable, repo, e.TimeZone)
		}
		if strings.TrimSpace(e.Channel) == "" {
			return nil, fmt.Errorf("%s: %s: channel must name a notification channel", TrainSettingsVariable, repo)
		}
		if e.OwnerDiscordUserID != "" && !trainDiscordIDRe.MatchString(e.OwnerDiscordUserID) {
			return nil, fmt.Errorf("%s: %s: ownerDiscordUserId %q is not a Discord user id (17 to 20 digits)", TrainSettingsVariable, repo, e.OwnerDiscordUserID)
		}
		rebuilds := e.MaxRebuilds
		if rebuilds == 0 {
			rebuilds = defaultTrainRebuilds
		}
		if rebuilds < 1 || rebuilds > 5 {
			return nil, fmt.Errorf("%s: %s: maxRebuilds %d is outside 1 to 5", TrainSettingsVariable, repo, e.MaxRebuilds)
		}
		out[repo] = TrainSettings{
			Repository:         repo,
			Enabled:            e.Enabled,
			Departures:         append([]string(nil), e.Departures...),
			TimeZone:           e.TimeZone,
			Channel:            strings.TrimSpace(e.Channel),
			OwnerDiscordUserID: e.OwnerDiscordUserID,
			MaxRebuilds:        rebuilds,
		}
	}
	return out, nil
}

// DepartureDue answers whether a scheduled departure fell in (last, now], and
// which: the LATEST one. A coordinator that was down across two departures
// runs one train, not two back to back -- the second would only carry what
// the first would have.
//
// Only today and yesterday (in the train's zone) are searched: every
// repository departs at least once a day, so the latest departure at or
// before now is never older than that.
func DepartureDue(s TrainSettings, last, now time.Time) (time.Time, bool) {
	loc, err := time.LoadLocation(s.TimeZone)
	if err != nil {
		return time.Time{}, false
	}
	local := now.In(loc)
	var best time.Time
	for back := 1; back >= 0; back-- {
		day := local.AddDate(0, 0, -back)
		for _, d := range s.Departures {
			hh, err1 := strconv.Atoi(d[:2])
			mm, err2 := strconv.Atoi(d[3:])
			if err1 != nil || err2 != nil || len(d) != 5 {
				continue
			}
			at := time.Date(day.Year(), day.Month(), day.Day(), hh, mm, 0, 0, loc)
			if at.After(last) && !at.After(now) && at.After(best) {
				best = at
			}
		}
	}
	if best.IsZero() {
		return time.Time{}, false
	}
	return best.UTC(), true
}

// TrainCandidate is an open pull request as boarding sees it. CheckStatus and
// CheckConclusion are the newest pull request check of the train's pipeline
// (CheckRunName) on HeadSHA.
type TrainCandidate struct {
	Number                                   int
	Title, HeadSHA, HeadRef, BaseRef, Author string
	URL                                      string
	Draft                                    bool
	// Fork marks a head in another repository, or in none (a deleted fork).
	// A fork never boards: its pull request is refused a run of its own
	// (trigger.go, D6), and a train would run its code here with the
	// pipeline's secrets on the strength of a check anything can write.
	Fork                         bool
	Labels                       []string
	CheckStatus, CheckConclusion string
}

// TrainSkip is a pull request that did not board, and why, in a sentence the
// owner reads in Discord and the pull request's session reads in a comment.
type TrainSkip struct {
	Number int
	Reason string
}

// BoardingOrder picks the pull requests a train carries, in pull request
// number order (spec 4.1 and 4.3). checkName is the pipeline's pull request
// check, CheckRunName of its name, which a refusal names. With express
// non-empty only the named pull requests are considered: an express train
// carries exactly what was asked for, and a named one that cannot board is
// reported, not silently dropped.
//
// A train's own pull request (a train/ head) is ignored without a reason: it
// is not a candidate, and listing it as "skipped" would read as a fault.
func BoardingOrder(defaultBranch, checkName string, cands []TrainCandidate, express []int) (board []TrainCandidate, skipped []TrainSkip) {
	named := map[int]bool{}
	for _, n := range express {
		named[n] = true
	}
	for _, c := range cands {
		if len(named) > 0 && !named[c.Number] {
			continue
		}
		if strings.HasPrefix(c.HeadRef, TrainBranchPrefix) {
			continue
		}
		if reason := boardingRefusal(defaultBranch, checkName, c); reason != "" {
			skipped = append(skipped, TrainSkip{Number: c.Number, Reason: reason})
			continue
		}
		board = append(board, c)
	}
	sort.Slice(board, func(i, j int) bool { return board[i].Number < board[j].Number })
	sort.Slice(skipped, func(i, j int) bool { return skipped[i].Number < skipped[j].Number })
	return board, skipped
}

func boardingRefusal(defaultBranch, checkName string, c TrainCandidate) string {
	switch {
	case c.Fork:
		return "comes from a fork; a train carries only this repository's branches"
	case c.BaseRef != defaultBranch:
		return fmt.Sprintf("targets %s, not %s", c.BaseRef, defaultBranch)
	case c.Draft:
		return "is a draft"
	case hasLabel(c.Labels, TrainHoldLabel):
		return "is held with " + TrainHoldLabel
	case hasLabel(c.Labels, TrainDroppedLabel):
		return "was dropped from an earlier train (" + TrainDroppedLabel + "); remove the label once a fix is pushed"
	case c.CheckStatus != "completed" || c.CheckConclusion != "success":
		return checkName + " has not passed on " + shortSHA(c.HeadSHA)
	}
	return ""
}

func hasLabel(labels []string, want string) bool {
	for _, l := range labels {
		if strings.EqualFold(strings.TrimSpace(l), want) {
			return true
		}
	}
	return false
}

func shortSHA(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}

// NextPatchVersion is the version a train carries when no pull request on
// board changed the version file: the patch number plus one. Only a plain
// MAJOR.MINOR.PATCH is accepted; anything else is a version a person chose,
// and a train never guesses over one.
func NextPatchVersion(current string) (string, error) {
	v := strings.TrimSpace(current)
	m := trainVersionRe.FindStringSubmatch(v)
	if m == nil {
		return "", fmt.Errorf("version %q is not MAJOR.MINOR.PATCH", v)
	}
	patch, err := strconv.Atoi(m[3])
	if err != nil {
		return "", fmt.Errorf("version %q: %w", v, err)
	}
	return m[1] + "." + m[2] + "." + strconv.Itoa(patch+1), nil
}

// TrainBranch names the n-th train of a day: train/2026-10-28-2.
func TrainBranch(day time.Time, n int) string {
	return fmt.Sprintf("%s%s-%d", TrainBranchPrefix, day.Format("2006-01-02"), n)
}

// TrainScratchPrefix marks the ref a train is built on before its branch
// moves. It is not under train/: a scratch ref has no pull request, and is
// never a train's run.
const TrainScratchPrefix = "train-build/"

// TrainScratchBranch is the scratch ref a train branch is built on:
// train-build/2026-10-28-2 for train/2026-10-28-2. The merges and the
// version bump happen there, and the train branch then moves once, to the
// finished head: it never points at the default branch's head, where GitHub
// could take its open pull request for merged.
func TrainScratchBranch(branch string) string {
	return TrainScratchPrefix + strings.TrimPrefix(branch, TrainBranchPrefix)
}
