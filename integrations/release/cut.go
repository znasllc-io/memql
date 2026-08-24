package release

import (
	"context"
	"errors"
	"strings"

	"github.com/znasllc-io/memql/component/auth"
)

// cut.go -- the release-cut handler, in the order the steps must happen.
//
// ===========================================================================
// THE OWNER WALL IS FIRST, AND IT IS THE LOAD-BEARING ONE
// ===========================================================================
// A builtin is not covered by the per-row-authz classification buckets: those
// walk queries and mutations, and a builtin is neither. So the DSL half of the
// double wall -- `requiresOwner` on the releaseCuts query -- gates the HISTORY
// READ and by construction cannot gate this. The Go check below is what stands
// between any authenticated caller and a published release of the product.
//
// It runs BEFORE the configuration is resolved, which is not merely tidy. A
// non-owner who reached the config first would learn, from the refusal they
// got back, whether this cluster has a release credential seeded -- and the
// difference between release_repo_unconfigured and credential_unavailable
// tells them how far along the setup is. Refusing first means a non-owner
// learns exactly one thing: no.
//
// THE PREDICATE IS AccessContext.IsClusterOwner, i.e. Role == owner. Chosen so
// the two walls agree by construction: `requiresOwner` is `role == "owner"`
// over the same actor envelope, and a Go wall that checked something subtly
// different (IsPrivilegedUser, say, which admits admin) would make the read
// and the write disagree about who may do this.
//
// FAIL CLOSED ON A MISSING ACTOR. No AccessContext means the middleware never
// resolved one, and the honest reading of that is "unauthenticated", not
// "trusted internal call". There is no legitimate caller of this builtin
// without one.
//
// ===========================================================================
// THE ORDER OF OPERATIONS, AND WHY IT IS THIS ORDER
// ===========================================================================
//  1. owner wall            -- above.
//  2. resolve config        -- repo, then credential; setup states, not faults.
//  3. read tags + head sha  -- one walk, so the two answers cannot disagree.
//  4. refuse if head is already released.
//  5. compute the next version.
//  6. dry run stops here    -- with the plan and nothing created.
//  7. create the tag ref    -- ATOMIC, and therefore the concurrency gate.
//  8. publish the Release   -- the step that fires the cascade.
//  9. optional pin-bump PR  -- degrades to a note, never fails the cut.
// 10. write the row + audit -- bookkeeping; a failure here is logged, not
//     propagated, because the release has already shipped.
//
// Seven and eight are the only irreversible steps, and they are adjacent and
// last-but-three on purpose: everything that can refuse has refused by then.

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
	Bump             string
	Notes            string
	BumpExtensionPin bool
	DryRun           bool
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

// Cut performs a release cut.
func (i *Integration) Cut(ctx context.Context, req CutRequest) (Outcome, error) {
	actor, err := requireOwner(ctx)
	if err != nil {
		return Outcome{}, err
	}

	// Validate the bump before anything touches the network. The DSL enum
	// refuses a bad value first; this is the direct-Go-caller path and the
	// place a typo stops rather than becoming a request.
	bump := strings.TrimSpace(req.Bump)
	if bump != "major" && bump != "minor" && bump != "patch" {
		return Outcome{}, refuse(CodeInvalidBump, "bump must be major, minor or patch, not %q", req.Bump)
	}

	cfg, err := i.resolver.loadSettings(ctx)
	if err != nil {
		return Outcome{}, err
	}

	tags, err := i.github.ListTagRefs(ctx, cfg.token, cfg.repo)
	if err != nil {
		return Outcome{}, err
	}
	headSha, err := i.github.MainHeadSha(ctx, cfg.token, cfg.repo)
	if err != nil {
		return Outcome{}, err
	}

	if existing := tagsAtSha(tags, headSha); len(existing) > 0 {
		return Outcome{}, refuse(CodeAlreadyReleasedAtHead,
			"main's head (%s) already carries the release tag %s. Cutting again would publish a second version of identical code; land a change first, or move the existing tag by hand if that is really what you want.",
			shortSha(headSha), strings.Join(existing, ", "))
	}

	previous, ok := newestRelease(tagNames(tags))
	if !ok {
		return Outcome{}, refuse(CodeNoReleaseTags,
			"%s has no vX.Y.Z tag, so there is no previous version to bump. The FIRST release of a repository is a version somebody chooses; create that tag and Release by hand, and this button takes over from there.",
			cfg.repo)
	}
	next, err := previous.bump(bump)
	if err != nil {
		return Outcome{}, err
	}

	out := Outcome{
		Version:     next.tag(),
		BareVersion: next.bare(),
		Tag:         next.tag(),
		Bump:        bump,
		BaseSha:     headSha,
		PreviousTag: previous.tag(),
		Repository:  cfg.repo.String(),
	}

	if req.DryRun {
		// Nothing created, nothing written, nothing audited. The whole
		// value of this path is that it exercises the credential, the
		// repository name and the arithmetic against the real API
		// without producing a release -- which is what makes it the
		// runbook's first step after seeding a token.
		out.DryRun = true
		out.Status = "dry_run"
		return out, nil
	}

	if err := i.github.CreateTagRef(ctx, cfg.token, cfg.repo, next.tag(), headSha); err != nil {
		return Outcome{}, err
	}

	release, err := i.github.CreateRelease(ctx, cfg.token, cfg.repo, next.tag(), req.Notes)
	if err != nil {
		// THE HALF-DONE STATE. The tag exists and no Release does, so
		// the cascade never fired. Recorded as a row before returning,
		// because the alternative -- a bare error -- leaves a tag on
		// the repository that nothing in this cluster knows about, and
		// the next cut of the same bump then fails with ref_exists
		// naming a tag whose origin nobody can explain.
		rec := Record{
			Version: next.tag(), Bump: bump, BaseSha: headSha,
			RequestedBy: actor.UserId, RequestedByEmail: actor.PrimaryEmail,
			Status: "tag_created_release_failed", TagName: next.tag(),
			Error: describeRefusal(err),
		}
		i.recordAndAudit(ctx, rec, string(actor.Role))
		return Outcome{}, refuseHalfDone(next.tag(),
			"the tag %s was created and the GitHub Release was not, so no images will be built. Publish a Release for that tag by hand to start the build, or delete the tag to undo the cut. The underlying failure was: %s",
			next.tag(), describeRefusal(err))
	}

	out.ReleaseURL = release.HTMLURL
	out.Status = "dispatched"

	if req.BumpExtensionPin {
		// A follow-on, and explicitly not part of the cut's success.
		// The release is published by the time this runs; a token
		// scoped for cutting alone legitimately cannot open a PR, and
		// failing here would report a shipped release as a failure.
		prURL, note := i.openPinBumpPR(ctx, cfg, next)
		out.PinBumpPrURL, out.PinBumpNote = prURL, note
	}

	rec := Record{
		Version: next.tag(), Bump: bump, BaseSha: headSha,
		RequestedBy: actor.UserId, RequestedByEmail: actor.PrimaryEmail,
		Status: "dispatched", TagName: next.tag(), ReleaseURL: release.HTMLURL,
		PinBumpPrURL: out.PinBumpPrURL, PinBumpNote: out.PinBumpNote,
	}
	i.recordAndAudit(ctx, rec, string(actor.Role))
	return out, nil
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
