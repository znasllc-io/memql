package release

import (
	"errors"
	"fmt"
)

// refusals.go -- the typed vocabulary a cut refuses in.
//
// EVERY REFUSAL IS A CODE, and the code is what the portal branches on. A cut
// fails for reasons that call for completely different operator responses --
// seed a variable, mint a token, wait for a colleague, go delete a tag by hand
// -- and prose alone cannot be branched on, so a card built over string
// matching drifts the first time a message is reworded.
//
// TWO OF THESE ARE SETUP STATES RATHER THAN FAILURES. release_repo_unconfigured
// and credential_unavailable mean the instance has not been told what to cut or
// with what; the card renders the setup step naming the variable to seed
// INSTEAD of the button, because offering a button whose only outcome is this
// refusal is the thing instanceActions' doctrine refuses to do.
//
// AND ONE IS NEITHER. tag_created_release_failed is the HALF-DONE state: a tag
// exists on the repository and no Release does, so the CI cascade never fired
// and a human has to either publish the Release or delete the tag. It is stated
// rather than hidden, it is written to the row, and releaseCutStatus keeps
// saying it until somebody resolves it. A cut that reported plain failure here
// would leave a tag nobody knew about, which is the one outcome worse than
// either honest answer.

// Refusal is a typed refusal carrying a stable code, a message safe to show an
// operator, and the tag name when a tag was created before the failure.
type Refusal struct {
	// Code is the stable identifier. Never localized, never reworded --
	// the portal branches on it.
	Code string
	// Message says what happened, in words an operator can act on. It
	// NEVER carries a credential value: a refusal about a token names the
	// variable to seed, which is the actionable half, and printing the
	// value would put a secret in a row, a log line and a browser.
	Message string
	// TagName is set only when a tag was created and the operation then
	// failed -- the one fact needed to finish or undo a half-done cut.
	TagName string
}

func (r *Refusal) Error() string {
	if r.TagName != "" {
		return fmt.Sprintf("%s: %s (tag %s exists)", r.Code, r.Message, r.TagName)
	}
	return fmt.Sprintf("%s: %s", r.Code, r.Message)
}

// The refusal codes. Kept as constants because they cross three boundaries --
// Go, the row's `error` field, and the portal card -- and a literal repeated at
// each is a rename waiting to go half-applied.
const (
	// CodeReleaseRepoUnconfigured -- MEMQL_RELEASE_REPO names no repository.
	// The engine carries NO compiled-in default on purpose: it is
	// product-agnostic and must not ship an organization's name as a
	// literal, so an instance that wants this button seeds the variable.
	CodeReleaseRepoUnconfigured = "release_repo_unconfigured"

	// CodeCredentialUnavailable -- no MEMQL_GITHUB_RELEASE_TOKEN resolved,
	// or GitHub rejected the one that did (401/403). One code for both,
	// because from the operator's chair the action is the same: mint a
	// fine-grained token and seed it.
	CodeCredentialUnavailable = "credential_unavailable"

	// CodeGithubUnreachable -- a transport failure or a 5xx. NOTHING was
	// created; that is the guarantee this code carries, and it is why the
	// half-done case has a code of its own rather than folding in here.
	CodeGithubUnreachable = "github_unreachable"

	// CodeRefExists -- the tag for the computed next version already
	// exists. This IS the concurrency gate: GitHub's ref-create is atomic,
	// so two owners racing means the second one gets this and no advisory
	// lock is needed anywhere.
	CodeRefExists = "ref_exists"

	// CodeAlreadyReleasedAtHead -- main's head sha already carries a
	// release tag. Cutting again would publish a second version of
	// identical code, which reads as two releases and is one.
	CodeAlreadyReleasedAtHead = "already_released_at_head"

	// CodeTagCreatedReleaseFailed -- the half-done state. See the file
	// comment: the tag exists, the Release does not, and no images will be
	// built until a human resolves it.
	CodeTagCreatedReleaseFailed = "tag_created_release_failed"

	// CodeNoReleaseTags -- the repository has no vX.Y.Z tag at all, so
	// there is no previous version to bump. Deliberately NOT defaulted to
	// v0.0.0-and-bump: inventing a starting point for somebody's release
	// history is a decision the engine should not make silently.
	CodeNoReleaseTags = "no_release_tags"

	// CodeInvalidBump -- the bump argument was not major/minor/patch. The
	// DSL enum refuses this first; the code exists so a direct Go caller
	// gets the same typed answer rather than a bare error.
	CodeInvalidBump = "invalid_bump"

	// CodeNotOwner -- the actor is not a cluster owner. THE GO WALL. It is
	// checked before anything else in the handler, so a non-owner never
	// causes a network call, never learns whether the credential is
	// seeded, and never appears in the release history.
	CodeNotOwner = "not_owner"

	// CodeVersionNotCut -- releaseCutStatus was asked about a version this
	// cluster has no row for. Distinct from "the images are missing":
	// a version cut by hand has no row here and never will, and reporting
	// that as absent images would be a claim about a registry nobody asked.
	CodeVersionNotCut = "version_not_cut"

	// CodeRegistryCheckFailed -- the GHCR manifest check itself errored.
	// The status is NOT guessed from it (D5): the row stays where it was
	// and the error is shown. A check that cannot reach the registry knows
	// nothing about whether the images exist.
	CodeRegistryCheckFailed = "registry_check_failed"
)

// refuse builds a Refusal.
func refuse(code, format string, args ...any) *Refusal {
	return &Refusal{Code: code, Message: fmt.Sprintf(format, args...)}
}

// refuseHalfDone builds the one refusal that carries a tag name.
func refuseHalfDone(tag, format string, args ...any) *Refusal {
	return &Refusal{Code: CodeTagCreatedReleaseFailed, Message: fmt.Sprintf(format, args...), TagName: tag}
}

// RefusalCode reads the code off any error in a chain, or "" when the error is
// not a Refusal. Callers that need to branch use this rather than string
// matching on Error().
func RefusalCode(err error) string {
	var r *Refusal
	if errors.As(err, &r) {
		return r.Code
	}
	return ""
}

// asRefusalPtr is errors.As with the target spelled out, so a test can read the
// TagName off a half-done refusal without repeating the type assertion.
func asRefusalPtr(err error, target **Refusal) bool { return errors.As(err, target) }
