package release

import (
	"context"
	"errors"
	"slices"
	"strings"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/automations/workflowhost"
)

// Publication is composed by releaseCutWorkflow in dsl/cluster. The adapter
// retains the owner gate, exact-candidate validation, atomic tag creation and
// honest recording of a tag whose Release failed to publish.

// Outcome is what a cut returns to the DSL caller.
type Outcome struct {
	Version      string `json:"version"`
	BareVersion  string `json:"bareVersion"`
	Tag          string `json:"tag"`
	Bump         string `json:"bump"`
	BaseSha      string `json:"baseSha"`
	PreviousTag  string `json:"previousTag"`
	ReleaseURL   string `json:"releaseUrl"`
	Status       string `json:"status"`
	Repository   string `json:"repository"`
	DryRun       bool   `json:"dryRun"`
	PinBumpPrURL string `json:"pinBumpPrUrl,omitempty"`
	PinBumpNote  string `json:"pinBumpNote,omitempty"`
}

// CutRequest is one call's arguments.
type CutRequest struct {
	Bump               string
	Notes              string
	BumpExtensionPin   bool
	DryRun             bool
	ExpectedRepository string
	ExpectedSha        string
	ExpectedVersion    string
}

// requireOwner is THE GATE. Exported-shaped as a method so the test can drive
// it through the same path a call takes rather than around it.
func requireOwner(ctx context.Context) (*auth.AccessContext, error) {
	ac, ok := auth.AccessFromContext(ctx)
	if !ok || ac == nil {
		return nil, refuse(CodeNotOwner,
			"cutting a release needs the owner role, and this call carries no resolved identity.")
	}
	if !ac.IsClusterOwner() {
		// The role IS named, deliberately. It tells an owner who is
		// signed in as somebody else what happened, and it tells a
		// non-owner nothing they did not already know about themselves.
		return nil, refuse(CodeNotOwner,
			"cutting a release needs the owner role; this caller holds %q.", string(ac.Role))
	}
	return ac, nil
}

// Cut preserves the public entry point while executing the installed DSL
// workflow. The scope contains credentials and irreversible-effect state; none
// of it can be supplied by DSL arguments or carried in an untrusted row.
func (i *Integration) Cut(ctx context.Context, req CutRequest) (Outcome, error) {
	actor, err := requireOwner(ctx)
	if err != nil {
		return Outcome{}, err
	}
	req.Bump = strings.TrimSpace(req.Bump)
	if req.Bump != "major" && req.Bump != "minor" && req.Bump != "patch" {
		return Outcome{}, refuse(CodeInvalidBump, "bump must be major, minor or patch, not %q", req.Bump)
	}
	scope := &cutScope{i: i, actor: actor, req: req}
	_, err = workflowhost.Run(ctx, "releaseCutWorkflow", map[string]any{
		"dryRun": req.DryRun, "bumpExtensionPin": req.BumpExtensionPin,
	}, workflowhost.Options{Logger: i.logger, Operations: scope.operations()})
	if err != nil {
		return Outcome{}, err
	}
	return scope.out, nil
}

type cutScope struct {
	i                                 *Integration
	actor                             *auth.AccessContext
	req                               CutRequest
	cfg                               settings
	tags                              []tagRef
	head                              string
	previous, next                    version
	out                               Outcome
	versionChecked, tagged, published bool
}

func (s *cutScope) operations() map[string]workflowhost.Operation {
	return map[string]workflowhost.Operation{
		"releaseReadSnapshot":    s.readSnapshot,
		"releaseSelectCandidate": s.selectCandidate,
		"releaseRejectCandidate": s.rejectCandidate,
		"releaseCheckVersion":    s.checkVersion,
		"releaseMarkDryRun": func(context.Context, map[string]any) (any, error) {
			s.out.DryRun, s.out.Status = true, "dry_run"
			return nil, nil
		},
		"releaseCreateTag":        s.createTag,
		"releasePublishTag":       s.publishTag,
		"releaseOpenExtensionPin": s.openExtensionPin,
		"releaseRecordCut":        s.recordCut,
	}
}

// Protocol reads return immutable facts; DSL selects the version from this snapshot.
func (s *cutScope) readSnapshot(ctx context.Context, _ map[string]any) (any, error) {
	cfg, err := s.i.resolver.loadSettings(ctx)
	if err != nil {
		return nil, err
	}
	tags, err := s.i.github.ListTagRefs(ctx, cfg.token, cfg.repo)
	if err != nil {
		return nil, err
	}
	head, err := s.i.github.MainHeadSha(ctx, cfg.token, cfg.repo)
	if err != nil {
		return nil, err
	}
	versions := []version{}
	for _, t := range tags {
		if v, ok := parseReleaseTag(t.Name); ok {
			versions = append(versions, v)
		}
	}
	slices.SortFunc(versions, func(a, b version) int {
		if a.newer(b) {
			return -1
		}
		if b.newer(a) {
			return 1
		}
		return 0
	})
	names := []string{}
	for _, v := range versions {
		names = append(names, v.tag())
	}
	s.cfg, s.tags, s.head = cfg, tags, head
	return map[string]any{"versions": names, "tagsAtHead": tagsAtSha(tags, head)}, nil
}

func (s *cutScope) selectCandidate(_ context.Context, a map[string]any) (any, error) {
	tag, _ := a["previousTag"].(string)
	previous, ok := parseReleaseTag(tag)
	if !ok || s.head == "" || !slices.Contains(tagNames(s.tags), tag) {
		return nil, refuse(CodeCandidateRequired, "selected version is outside the repository snapshot")
	}
	next, err := previous.bump(s.req.Bump)
	if err != nil {
		return nil, err
	}
	s.previous, s.next = previous, next
	s.out = Outcome{Version: next.tag(), BareVersion: next.bare(), Tag: next.tag(), Bump: s.req.Bump, BaseSha: s.head, PreviousTag: previous.tag(), Repository: s.cfg.repo.String()}
	return nil, nil
}

func (s *cutScope) rejectCandidate(_ context.Context, a map[string]any) (any, error) {
	code, _ := a["code"].(string)
	switch code {
	case CodeAlreadyReleasedAtHead:
		return nil, refuse(code, "main's head (%s) already carries the release tag %s. Cutting again would publish a second version of identical code; land a change first, or move the existing tag by hand if that is really what you want.", shortSha(s.head), strings.Join(tagsAtSha(s.tags, s.head), ", "))
	case CodeNoReleaseTags:
		return nil, refuse(code, "%s has no vX.Y.Z tag, so there is no previous version to bump. The FIRST release of a repository is a version somebody chooses; create that tag and Release by hand, and this button takes over from there.", s.cfg.repo)
	default:
		return nil, refuse(CodeCandidateRequired, "release workflow refused its candidate")
	}
}

func (s *cutScope) checkVersion(ctx context.Context, _ map[string]any) (any, error) {
	if s.out.BaseSha == "" {
		return nil, refuse(CodeCandidateRequired, "read a release candidate before checking its version")
	}
	err := s.i.checkVersionFile(ctx, s.cfg, s.out.BaseSha, s.previous, s.next, s.req.Bump)
	s.versionChecked = err == nil
	return nil, err
}

func (s *cutScope) createTag(ctx context.Context, _ map[string]any) (any, error) {
	if _, err := requireOwner(ctx); err != nil {
		return nil, err
	}
	if !s.versionChecked || s.req.DryRun {
		return nil, refuse(CodeCandidateRequired, "a checked publication candidate is required")
	}
	if s.req.ExpectedRepository == "" || s.req.ExpectedSha == "" || s.req.ExpectedVersion == "" {
		return nil, refuse(CodeCandidateRequired, "publishing requires expectedRepository, expectedSha and expectedVersion from a reviewed dry run; no tag or Release was created.")
	}
	if s.req.ExpectedRepository != s.out.Repository || s.req.ExpectedSha != s.out.BaseSha || s.req.ExpectedVersion != s.out.Version {
		return nil, refuse(CodeCandidateChanged, "the release candidate changed: the current plan is %s %s at %s. Review a new dry run before publishing; no tag or Release was created.", s.out.Repository, s.out.Version, s.out.BaseSha)
	}
	if s.tagged {
		return nil, nil
	}
	err := s.i.github.CreateTagRef(ctx, s.cfg.token, s.cfg.repo, s.next.tag(), s.out.BaseSha)
	s.tagged = err == nil
	return nil, err
}

func (s *cutScope) publishTag(ctx context.Context, _ map[string]any) (any, error) {
	if _, err := requireOwner(ctx); err != nil {
		return nil, err
	}
	if !s.tagged {
		return nil, refuse(CodeCandidateRequired, "publication requires the tag created for this checked candidate")
	}
	if s.published {
		return nil, nil
	}
	release, err := s.i.github.CreateRelease(ctx, s.cfg.token, s.cfg.repo, s.next.tag(), s.req.Notes)
	if err != nil {
		rec := Record{Version: s.next.tag(), Bump: s.req.Bump, BaseSha: s.out.BaseSha, RequestedBy: s.actor.UserId, RequestedByEmail: s.actor.PrimaryEmail, Status: "tag_created_release_failed", TagName: s.next.tag(), Error: describeRefusal(err)}
		s.i.recordAndAudit(ctx, rec, string(s.actor.Role))
		return nil, refuseHalfDone(s.next.tag(), "the tag %s was created and the GitHub Release was not, so no images will be built. Publish a Release for that tag by hand to start the build, or delete the tag to undo the cut. The underlying failure was: %s", s.next.tag(), describeRefusal(err))
	}
	s.published = true
	s.out.ReleaseURL, s.out.Status = release.HTMLURL, "dispatched"
	return nil, nil
}

func (s *cutScope) openExtensionPin(ctx context.Context, _ map[string]any) (any, error) {
	if !s.published {
		return nil, refuse(CodeCandidateRequired, "the release must be published before opening its extension pin")
	}
	s.out.PinBumpPrURL, s.out.PinBumpNote = s.i.openPinBumpPR(ctx, s.cfg, s.next)
	return nil, nil
}

func (s *cutScope) recordCut(ctx context.Context, _ map[string]any) (any, error) {
	if !s.published {
		return nil, refuse(CodeCandidateRequired, "no release was published in this invocation")
	}
	s.i.recordAndAudit(ctx, Record{Version: s.next.tag(), Bump: s.req.Bump, BaseSha: s.out.BaseSha, RequestedBy: s.actor.UserId, RequestedByEmail: s.actor.PrimaryEmail, Status: "dispatched", TagName: s.next.tag(), ReleaseURL: s.out.ReleaseURL, PinBumpPrURL: s.out.PinBumpPrURL, PinBumpNote: s.out.PinBumpNote}, string(s.actor.Role))
	return nil, nil
}

// recordAndAudit writes the row and the audit event, logging rather than
// returning either failure.
//
// See Store.WriteCut's note: at every call site of this function the release
// has already happened, so a bookkeeping failure must not be reported as a
// failed cut. It IS logged at Error, because a cut missing from the history is
// a real defect -- just not one whose correct response is "cut again".
func (i *Integration) recordAndAudit(ctx context.Context, rec Record, actorRole string) {
	if err := i.store.WriteCut(ctx, rec); err != nil {
		i.logger.Error("release: the cut happened and its row did not land",
			"component", "integrations.release", "version", rec.Version, "error", err)
	}
	if err := i.store.WriteAudit(ctx, rec, actorRole); err != nil {
		i.logger.Error("release: the cut happened and its audit event did not land",
			"component", "integrations.release", "version", rec.Version, "error", err)
	}
}

// describeRefusal renders an error for the row's `error` field.
//
// Prefixed with the CODE when there is one, so the stored string stays
// machine-readable enough for a reader to tell a credential problem from a
// half-done tag without parsing prose.
func describeRefusal(err error) string {
	if err == nil {
		return ""
	}
	var r *Refusal
	if errors.As(err, &r) {
		return r.Code + ": " + r.Message
	}
	return err.Error()
}

// shortSha renders the first seven characters, which is what a human compares.
func shortSha(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}
