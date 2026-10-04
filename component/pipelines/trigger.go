package pipelines

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// Trigger is what one signed GitHub delivery asks for (design record D4-D6).
type Trigger struct {
	Event          Event  // "" for a rerequest
	Rerequest      bool   // check_run or check_suite rerequested
	CheckRunID     int64  // check_run.id for a check_run rerequest
	Repository     string // "owner/name", lower-cased
	DefaultBranch  string
	InstallationID int64
	SHA            string // head commit; "" for a release until resolved
	BaseSHA        string // pull_request.base.sha, merge_group.base_sha, push.before
	Branch         string // head branch name (no refs/heads/)
	PullRequest    int
	Fork           bool   // the head lives in another repository (D6)
	HeadRepository string // "owner/name" of the head, for the refusal text
	Title          string // first line of the commit message, the PR title, or the release name
	ReleaseTag     string
}

// Ignored is a delivery that asks this pipeline for nothing, with why.
type Ignored struct{ Reason string }

// zeroSHA is what GitHub sends for the side of a push where the ref does not
// exist: `before` on a creation, `after` on a deletion.
const zeroSHA = "0000000000000000000000000000000000000000"

// The deliveries pipelines run on, named as X-GitHub-Event names them.
const (
	kindMergeGroup  = "merge_group"
	kindPullRequest = "pull_request"
	kindCheckRun    = "check_run"
	kindCheckSuite  = "check_suite"
	kindRelease     = "release"
	kindPush        = "push"
)

// ClassifyDelivery reads a GitHub webhook body by its SHAPE. headerEvent is
// the unsigned X-GitHub-Event; when present and it disagrees with the body,
// the delivery is ignored rather than believed.
//
// The body is what the inbound verifier checked the signature of, so the body
// decides what a delivery is (decision 4 of the pipelines-seam plan). A
// pull_request `synchronize` delivery carries a top-level before and after
// and no ref; a reader that took before/after for a push would move
// Deployables' update cue on every pull request push, which is why the push
// shape is the last one tried and needs all three keys.
//
// An Ignored delivery is never an error: it asks pipelines for nothing, and a
// delivery pipelines would not act on cannot be wrong in a way that matters
// here. An error is a delivery pipelines WOULD act on that cannot be read: no
// repository, no installation, a head that is not a SHA.
func ClassifyDelivery(headerEvent string, body []byte) (Trigger, *Ignored, error) {
	var top map[string]json.RawMessage
	if err := json.Unmarshal(body, &top); err != nil {
		return Trigger{}, nil, fmt.Errorf("a GitHub delivery is a JSON object: %w", err)
	}
	if top == nil {
		return Trigger{}, nil, errors.New("a GitHub delivery is a JSON object, and this one is null")
	}
	kind := deliveryKind(top)
	if kind == "" {
		return Trigger{}, ignore("not a delivery pipelines run on"), nil
	}
	if h := strings.ToLower(strings.TrimSpace(headerEvent)); h != "" && h != kind {
		return Trigger{}, ignore("header says %s, body is %s", h, kind), nil
	}
	var action string
	if err := decodeField(top, "action", &action); err != nil {
		return Trigger{}, nil, fmt.Errorf("github %s delivery: %w", kind, err)
	}

	var (
		t       Trigger
		ignored *Ignored
		err     error
	)
	switch kind {
	case kindPullRequest:
		t, ignored, err = classifyPullRequest(top, action)
	case kindMergeGroup:
		t, ignored, err = classifyMergeGroup(top, action)
	case kindCheckRun:
		t, ignored, err = classifyCheckRun(top, action)
	case kindCheckSuite:
		t, ignored, err = classifyCheckSuite(top, action)
	case kindRelease:
		t, ignored, err = classifyRelease(top, action)
	case kindPush:
		t, ignored, err = classifyPush(top)
	}
	if err != nil {
		return Trigger{}, nil, fmt.Errorf("github %s delivery: %w", kind, err)
	}
	if ignored != nil {
		return Trigger{}, ignored, nil
	}
	return t, nil, nil
}

// deliveryKind names a body by the keys it carries, or "" for one pipelines
// do not run on.
func deliveryKind(top map[string]json.RawMessage) string {
	switch {
	case present(top, "merge_group"):
		return kindMergeGroup
	case present(top, "pull_request"):
		// A review, a review comment and a review thread carry the pull
		// request they are about at the top level too. They are not its
		// events, whatever their action.
		if present(top, "review") || present(top, "comment") || present(top, "thread") {
			return ""
		}
		return kindPullRequest
	case present(top, "check_run"):
		return kindCheckRun
	case present(top, "check_suite"):
		return kindCheckSuite
	case present(top, "release"):
		return kindRelease
	case present(top, "ref") && present(top, "before") && present(top, "after"):
		return kindPush
	}
	return ""
}

// The parts of a delivery ClassifyDelivery reads. Pointers, so an absent
// field and an empty one stay apart where it matters (a fork's null head
// repository).
type (
	ghRepository struct {
		FullName      *string `json:"full_name"`
		DefaultBranch *string `json:"default_branch"`
	}
	ghInstallation struct {
		ID *int64 `json:"id"`
	}
	ghCommit struct {
		Message *string `json:"message"`
	}
	ghBranchRef struct {
		Ref  *string       `json:"ref"`
		SHA  *string       `json:"sha"`
		Repo *ghRepository `json:"repo"`
	}
	ghPullRequest struct {
		Number *int         `json:"number"`
		Title  *string      `json:"title"`
		Head   *ghBranchRef `json:"head"`
		Base   *ghBranchRef `json:"base"`
	}
	ghMergeGroup struct {
		HeadSHA    *string   `json:"head_sha"`
		HeadRef    *string   `json:"head_ref"`
		BaseSHA    *string   `json:"base_sha"`
		HeadCommit *ghCommit `json:"head_commit"`
	}
	ghCheckRun struct {
		ID      *int64  `json:"id"`
		HeadSHA *string `json:"head_sha"`
	}
	ghCheckSuite struct {
		HeadSHA *string `json:"head_sha"`
	}
	ghRelease struct {
		TagName *string `json:"tag_name"`
		Name    *string `json:"name"`
	}
)

func classifyPullRequest(top map[string]json.RawMessage, action string) (Trigger, *Ignored, error) {
	switch action {
	case "opened", "synchronize", "reopened":
	default:
		return Trigger{}, ignore("pull request %s", describeAction(action)), nil
	}
	var pr ghPullRequest
	if err := decodeField(top, "pull_request", &pr); err != nil {
		return Trigger{}, nil, err
	}
	t, err := envelope(top)
	if err != nil {
		return Trigger{}, nil, err
	}
	head, base := pr.Head, pr.Base
	if head == nil {
		head = &ghBranchRef{}
	}
	if base == nil {
		base = &ghBranchRef{}
	}
	t.Event = EventPullRequest
	if t.SHA, err = requireSHA("pull_request.head.sha", head.SHA); err != nil {
		return Trigger{}, nil, err
	}
	if t.BaseSHA, err = optionalSHA("pull_request.base.sha", base.SHA); err != nil {
		return Trigger{}, nil, err
	}
	t.Branch = deref(head.Ref)
	var number int
	if err := decodeField(top, "number", &number); err != nil {
		return Trigger{}, nil, err
	}
	if number == 0 && pr.Number != nil {
		number = *pr.Number
	}
	t.PullRequest = number
	// D6: a head in another repository is a stranger's code. A head whose
	// repository GitHub reports as null (the fork was deleted) is one nobody
	// can vouch for either.
	if head.Repo != nil {
		t.HeadRepository = strings.TrimSpace(deref(head.Repo.FullName))
	}
	t.Fork = t.HeadRepository == "" || !strings.EqualFold(t.HeadRepository, t.Repository)
	t.Title = firstLine(deref(pr.Title))
	return t, nil, nil
}

func classifyMergeGroup(top map[string]json.RawMessage, action string) (Trigger, *Ignored, error) {
	if action != "checks_requested" {
		return Trigger{}, ignore("merge group %s", describeAction(action)), nil
	}
	var mg ghMergeGroup
	if err := decodeField(top, "merge_group", &mg); err != nil {
		return Trigger{}, nil, err
	}
	t, err := envelope(top)
	if err != nil {
		return Trigger{}, nil, err
	}
	t.Event = EventMergeGroup
	if t.SHA, err = requireSHA("merge_group.head_sha", mg.HeadSHA); err != nil {
		return Trigger{}, nil, err
	}
	if t.BaseSHA, err = optionalSHA("merge_group.base_sha", mg.BaseSHA); err != nil {
		return Trigger{}, nil, err
	}
	t.Branch = strings.TrimPrefix(deref(mg.HeadRef), "refs/heads/")
	if mg.HeadCommit != nil {
		t.Title = firstLine(deref(mg.HeadCommit.Message))
	}
	return t, nil, nil
}

func classifyCheckRun(top map[string]json.RawMessage, action string) (Trigger, *Ignored, error) {
	if action != "rerequested" {
		return Trigger{}, ignore("check run %s", describeAction(action)), nil
	}
	var cr ghCheckRun
	if err := decodeField(top, "check_run", &cr); err != nil {
		return Trigger{}, nil, err
	}
	t, err := envelope(top)
	if err != nil {
		return Trigger{}, nil, err
	}
	if cr.ID == nil || *cr.ID <= 0 {
		return Trigger{}, nil, errors.New("check_run.id is missing")
	}
	t.Rerequest, t.CheckRunID = true, *cr.ID
	if t.SHA, err = requireSHA("check_run.head_sha", cr.HeadSHA); err != nil {
		return Trigger{}, nil, err
	}
	return t, nil, nil
}

func classifyCheckSuite(top map[string]json.RawMessage, action string) (Trigger, *Ignored, error) {
	// `requested` arrives on every push once the app holds checks: write; it
	// is GitHub announcing a suite, not a person asking for a run.
	if action != "rerequested" {
		return Trigger{}, ignore("check suite %s", describeAction(action)), nil
	}
	var cs ghCheckSuite
	if err := decodeField(top, "check_suite", &cs); err != nil {
		return Trigger{}, nil, err
	}
	t, err := envelope(top)
	if err != nil {
		return Trigger{}, nil, err
	}
	t.Rerequest = true
	if t.SHA, err = requireSHA("check_suite.head_sha", cs.HeadSHA); err != nil {
		return Trigger{}, nil, err
	}
	return t, nil, nil
}

func classifyRelease(top map[string]json.RawMessage, action string) (Trigger, *Ignored, error) {
	if action != "published" {
		return Trigger{}, ignore("release %s", describeAction(action)), nil
	}
	var rel ghRelease
	if err := decodeField(top, "release", &rel); err != nil {
		return Trigger{}, nil, err
	}
	t, err := envelope(top)
	if err != nil {
		return Trigger{}, nil, err
	}
	tag := strings.TrimSpace(deref(rel.TagName))
	if tag == "" {
		return Trigger{}, nil, errors.New("release.tag_name is missing")
	}
	t.Event, t.ReleaseTag = EventRelease, tag
	t.Title = firstLine(deref(rel.Name))
	if t.Title == "" {
		t.Title = tag
	}
	// The SHA stays empty: a release names a tag, and the trigger resolves
	// the tag's commit through the API rather than trusting a branch name.
	return t, nil, nil
}

func classifyPush(top map[string]json.RawMessage) (Trigger, *Ignored, error) {
	var ref, before, after string
	for key, dst := range map[string]*string{"ref": &ref, "before": &before, "after": &after} {
		if err := decodeField(top, key, dst); err != nil {
			return Trigger{}, nil, err
		}
	}
	branch, isBranch := strings.CutPrefix(ref, "refs/heads/")
	tag, isTag := strings.CutPrefix(ref, "refs/tags/")
	switch {
	case after == zeroSHA && isBranch:
		return Trigger{}, ignore("branch %s deleted", branch), nil
	case after == zeroSHA && isTag:
		return Trigger{}, ignore("tag %s deleted", tag), nil
	case after == zeroSHA:
		return Trigger{}, ignore("ref %s deleted", ref), nil
	case isTag:
		return Trigger{}, ignore("tag %s pushed", tag), nil
	case ref == "":
		return Trigger{}, ignore("push with no ref"), nil
	case !isBranch:
		return Trigger{}, ignore("push to %s, not a branch", ref), nil
	}
	t, err := envelope(top)
	if err != nil {
		return Trigger{}, nil, err
	}
	if t.DefaultBranch == "" {
		return Trigger{}, nil, errors.New("repository.default_branch is missing, so a push cannot be told to be the default branch's")
	}
	if branch != t.DefaultBranch {
		return Trigger{}, ignore("push to %s, not the default branch %s", branch, t.DefaultBranch), nil
	}
	t.Event, t.Branch = EventPush, branch
	if t.SHA, err = requireSHA("after", &after); err != nil {
		return Trigger{}, nil, err
	}
	if before != zeroSHA {
		if t.BaseSHA, err = optionalSHA("before", &before); err != nil {
			return Trigger{}, nil, err
		}
	}
	var head ghCommit
	if err := decodeField(top, "head_commit", &head); err != nil {
		return Trigger{}, nil, err
	}
	t.Title = firstLine(deref(head.Message))
	return t, nil, nil
}

// envelope reads what every delivery pipelines act on carries: the repository
// and the installation it was delivered to.
func envelope(top map[string]json.RawMessage) (Trigger, error) {
	if !present(top, "repository") {
		return Trigger{}, errors.New("the delivery names no repository")
	}
	var repo ghRepository
	if err := decodeField(top, "repository", &repo); err != nil {
		return Trigger{}, err
	}
	full := strings.TrimSpace(deref(repo.FullName))
	owner, name, ok := strings.Cut(full, "/")
	if !ok || owner == "" || name == "" || strings.Contains(name, "/") {
		return Trigger{}, fmt.Errorf("repository.full_name %q is not owner/name", clip(full))
	}
	// An installation is what an App delivery carries and a repository
	// webhook does not: pipelines run on the App's deliveries only.
	if !present(top, "installation") {
		return Trigger{}, errors.New("the delivery names no installation; pipelines run on deliveries to the GitHub App")
	}
	var inst ghInstallation
	if err := decodeField(top, "installation", &inst); err != nil {
		return Trigger{}, err
	}
	if inst.ID == nil || *inst.ID <= 0 {
		return Trigger{}, errors.New("installation.id is missing")
	}
	return Trigger{
		Repository:     strings.ToLower(full),
		DefaultBranch:  strings.TrimSpace(deref(repo.DefaultBranch)),
		InstallationID: *inst.ID,
	}, nil
}

// present reports whether the body carries key with a value other than null.
func present(top map[string]json.RawMessage, key string) bool {
	raw, ok := top[key]
	return ok && !bytes.Equal(bytes.TrimSpace(raw), []byte("null"))
}

// decodeField decodes top[key] into v, leaving v untouched when key is absent
// or null.
func decodeField(top map[string]json.RawMessage, key string, v any) error {
	raw, ok := top[key]
	if !ok {
		return nil
	}
	if err := json.Unmarshal(raw, v); err != nil {
		return fmt.Errorf("reading %s: %w", key, err)
	}
	return nil
}

// requireSHA is a head commit: present, and a SHA.
func requireSHA(field string, v *string) (string, error) {
	if v == nil || *v == "" {
		return "", fmt.Errorf("%s is missing", field)
	}
	return checkSHA(field, *v)
}

// optionalSHA is a base commit: absent is fine, malformed is not.
func optionalSHA(field string, v *string) (string, error) {
	if v == nil || *v == "" {
		return "", nil
	}
	return checkSHA(field, *v)
}

// checkSHA admits 40 hex characters and returns them lower-cased: one commit
// is one string, whoever spelled it.
func checkSHA(field, v string) (string, error) {
	if len(v) != 40 {
		return "", fmt.Errorf("%s %q is not a 40-character SHA", field, clip(v))
	}
	for i := 0; i < len(v); i++ {
		c := v[i]
		if !('0' <= c && c <= '9' || 'a' <= c && c <= 'f' || 'A' <= c && c <= 'F') {
			return "", fmt.Errorf("%s %q is not a hex SHA", field, clip(v))
		}
	}
	return strings.ToLower(v), nil
}

func ignore(format string, args ...any) *Ignored {
	return &Ignored{Reason: fmt.Sprintf(format, args...)}
}

func describeAction(action string) string {
	if action == "" {
		return "with no action"
	}
	return action
}

// firstLine is a message's subject line.
func firstLine(s string) string {
	line, _, _ := strings.Cut(s, "\n")
	return strings.TrimSpace(line)
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// clip keeps an echoed value short: an error message quotes what it refused,
// not however much of it a body carried.
func clip(s string) string {
	const max = 64
	if len(s) <= max {
		return s
	}
	return s[:max] + "..."
}
