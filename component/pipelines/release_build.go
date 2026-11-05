package pipelines

import (
	"errors"
	"fmt"
	"strings"
)

// ReleaseBuildRequest names a selected source. Empty ExpectedSHA and binding
// fields are allowed only for the first read, which then chooses the commit
// (ReleaseSourceWalk); effects require the full binding.
// CredentialID is a nonsecret source identity, never a bearer or token.
type ReleaseBuildRequest struct {
	PipelineID, SourceID, CredentialID, Repository, Branch, ExpectedSHA, Tag string
	InstallationID, RepositoryID                                             int64
}

// ReleaseBuildSource is fresh provider evidence, not authority supplied by a
// caller. ProofSHA identifies where checks actually ran; it may be the exact
// tree-equivalent head of the PR which produced SHA, never an arbitrary commit.
type ReleaseBuildSource struct {
	OwnerUserID, PipelineID, SourceID, CredentialID, Repository, Branch string
	HeadSHA, SHA, TreeSHA, ProofSHA                                     string
	InstallationID, RepositoryID                                        int64
	ProofPullRequest                                                    int
	TagExists                                                           bool
	TagSHA                                                              string
	Checks                                                              []ReleaseBuildCheck
	ExistingRun                                                         *ReleaseBuildRun
	// Walk is how the first read chose SHA when the request named no commit:
	// the head it started from and every newer commit it passed over. It is
	// nil when the head itself was chosen and for a request that named its
	// commit, so a source chosen without passing anything over encodes exactly
	// as it did before the field existed. Saved releases keep this type inside
	// the selection their digest covers, and an older engine decodes it
	// strictly (TestAReleaseSourceWithoutAWalkEncodesAsBefore).
	Walk *ReleaseSourceWalk `json:"Walk,omitempty"`
}

// MaxReleaseSourceLookBack bounds any recorded walk, whatever look back the
// engine that recorded it used.
const MaxReleaseSourceLookBack = 100

// Why a walk passed over a commit. The same words as githubapp's, which
// classifies; they are stored in saved releases and read by MemQL OS.
const (
	ReleaseSourcePending   = "pending"
	ReleaseSourceFailed    = "failed"
	ReleaseSourceCancelled = "cancelled"
	ReleaseSourceMissing   = "missing"
)

// ReleaseSourceWalk records how release preparation chose a commit when it was
// asked for none: it looked back along the default branch's first parents from
// HeadSHA, at most LookBack commits, and passed over Newer, newest first, to
// reach the newest commit whose required checks passed (design record
// docs/superpowers/specs/2026-11-05-owner-pushes-main-design.md, D6).
type ReleaseSourceWalk struct {
	HeadSHA  string
	LookBack int
	Newer    []ReleaseSourceSkip
}

// ReleaseSourceSkip is one commit a walk passed over: why (State), the
// required check that decided (Check) and its conclusion, when it had one.
type ReleaseSourceSkip struct {
	SHA, State, Check, Conclusion string
}

// Validate checks a walk against selected, the commit it chose, or "" for a
// walk that chose nothing. A walk is recorded only when it passed something
// over, so Newer is never empty and starts at the head, and nothing it passed
// over is the commit it chose. LookBack counts every commit examined, the
// chosen one included, so a walk that chose one passed over fewer.
func (w ReleaseSourceWalk) Validate(selected string) error {
	examined := len(w.Newer)
	if selected != "" {
		examined++
	}
	if selected != "" && !exactCommit(selected) || !exactCommit(w.HeadSHA) || w.HeadSHA == selected || w.LookBack < 1 || w.LookBack > MaxReleaseSourceLookBack || len(w.Newer) == 0 || examined > w.LookBack || w.Newer[0].SHA != w.HeadSHA {
		return errors.New("release source walk does not describe how its commit was chosen")
	}
	seen := map[string]bool{}
	for _, skip := range w.Newer {
		if !exactCommit(skip.SHA) || skip.SHA == selected || seen[skip.SHA] || skip.Check == "" || len(skip.Check) > 255 || len(skip.Conclusion) > 64 {
			return errors.New("release source walk names an invalid commit or check")
		}
		seen[skip.SHA] = true
		switch skip.State {
		case ReleaseSourcePending, ReleaseSourceMissing:
			if skip.Conclusion != "" {
				return errors.New("release source walk gives a conclusion to a check that has none")
			}
		case ReleaseSourceCancelled:
			if skip.Conclusion != "cancelled" {
				return errors.New("release source walk records a cancellation without its conclusion")
			}
		case ReleaseSourceFailed:
			if skip.Conclusion == "success" || skip.Conclusion == "cancelled" {
				return errors.New("release source walk records a failure that did not fail")
			}
		default:
			return fmt.Errorf("release source walk records an unknown state %q", skip.State)
		}
	}
	return nil
}

// Summary counts what the walk passed over, in the words a person reads:
// "2 running, 1 failed, 17 untested".
func (w ReleaseSourceWalk) Summary() string {
	counts := map[string]int{}
	for _, skip := range w.Newer {
		counts[skip.State]++
	}
	var parts []string
	for _, state := range []struct{ state, word string }{{ReleaseSourcePending, "running"}, {ReleaseSourceFailed, "failed"}, {ReleaseSourceCancelled, "superseded"}, {ReleaseSourceMissing, "untested"}} {
		if n := counts[state.state]; n > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", n, state.word))
		}
	}
	return strings.Join(parts, ", ")
}

// ReleaseSourceUntestedError is a walk that found nothing to release: no commit
// within its look back passed its required checks. Nothing was saved. The
// release integration answers it as the refusal release_source_untested.
type ReleaseSourceUntestedError struct {
	Branch string
	Walk   ReleaseSourceWalk
}

func (e *ReleaseSourceUntestedError) Error() string {
	return fmt.Sprintf("no commit among the newest %d commits on %s passed its required checks (%s)", len(e.Walk.Newer), e.Branch, e.Walk.Summary())
}

// exactCommit is a full lower-case commit id, the one spelling a saved
// selection compares.
func exactCommit(sha string) bool {
	if len(sha) != 40 {
		return false
	}
	for i := 0; i < len(sha); i++ {
		if c := sha[i]; !('0' <= c && c <= '9' || 'a' <= c && c <= 'f') {
			return false
		}
	}
	return true
}

type ReleaseBuildCheck struct {
	Context                 string
	AppID                   int64
	SHA, Status, Conclusion string
}

// ReleaseBuildRun is the native run's identity and lifecycle only. A projection
// never carries a worker credential, raw policy snapshot or command payload.
type ReleaseBuildRun struct {
	ID, OwnerUserID, PipelineID, Repository, SHA, Tag string
	Mode, Event, Status, Conclusion, WorkRunID        string
	Attempt                                           int
	CancelRequested                                   bool
	// RerunOf is the attempt this one re-runs; empty for attempt 1.
	//
	// Both re-run fields are omitted when empty. Saved-release journals store
	// this type inside the selection their digest covers, and an older engine
	// decodes it strictly, so a run without re-run facts must encode exactly
	// as it did before these fields existed. The JSON keys stay the Go names:
	// the journal's SQL fence reads run.Chain (integrations/release/build_ledger.go).
	RerunOf string `json:"RerunOf,omitempty"`
	// Chain lists the earlier attempts of the same release run key, newest
	// first, read through rerunOf. It is nil for attempt 1 and for a chain
	// that cannot be followed back to attempt 1 inside its run key, so a saved
	// release adopts a re-run only through an unbroken chain
	// (integrations/release/build_adopt.go).
	Chain []ReleaseBuildAttempt `json:"Chain,omitempty"`
}

// ReleaseBuildAttempt is one earlier attempt's lifecycle: enough to decide
// whether a later attempt of the same release may replace it.
type ReleaseBuildAttempt struct {
	ID                 string
	Attempt            int
	Status, Conclusion string
	CancelRequested    bool
}
