package repositoryauthority

import (
	"context"
	"errors"
	"regexp"
	"strings"

	"github.com/znasllc-io/memql/component/automations/workflowhost"
)

var commit = regexp.MustCompile(`^[a-f0-9]{40}$`)
var sha256hex = regexp.MustCompile(`^[a-f0-9]{64}$`)

// ExecutionRecipe is a native dispatch gate, not a public capability. The
// caller's persisted Work context must already name this exact owner/goal/run.
// It refuses changed or revoked delegation before any recipe step can run and
// returns only the captured closure, never the currently installed substitute.
func (i *Integration) ExecutionRecipe(ctx context.Context, id, template string) (*workflowhost.Snapshot, error) {
	actor, err := person(ctx, "", "")
	if err != nil {
		return nil, err
	}
	r, err := i.get(ctx, id, actor)
	if err != nil {
		return nil, err
	}
	if template != r.Route.Template {
		return nil, errors.New("repository execution names a different approved recipe")
	}
	if _, err := i.AuthorizeEffect(ctx, id, r.Request.Operation, r.Request.Paths); err != nil {
		return nil, err
	}
	snapshot, err := workflowhost.SnapshotFromMap(r.Route.Recipe)
	if err != nil {
		return nil, err
	}
	if snapshot.Contract != recipeContract || snapshot.Entry != template {
		return nil, errors.New("repository execution has no matching captured recipe")
	}
	return snapshot, nil
}

func validateProposal(p Proposal) ([]string, error) {
	if !identifier.MatchString(p.RequestID) || !commit.MatchString(p.ExpectedHead) || p.BaseBranch == "" || len(p.BaseBranch) > 200 || strings.ContainsAny(p.BaseBranch, "\x00\n\r ~^:?*[\\") || strings.Contains(p.BaseBranch, "..") || strings.HasPrefix(p.BaseBranch, "-") || strings.HasSuffix(p.BaseBranch, "/") || len(p.Title) == 0 || len(p.Title) > 200 || len(p.Body) > 16000 || len(p.Files) == 0 || len(p.Files) > 256 {
		return nil, errors.New("proposal requires an exact base commit, branch, title and bounded replacements")
	}
	paths := make([]string, 0, len(p.Files))
	bytes := 0
	for _, f := range p.Files {
		bytes += len(f.Content)
		if bytes > 4<<20 || (!sha256hex.MatchString(f.BeforeSHA256) && f.BeforeSHA256 != "") || strings.HasSuffix(f.Path, "/") {
			return nil, errors.New("proposal replacement exceeds its content or before-hash contract")
		}
		paths = append(paths, f.Path)
	}
	if err := validatePaths(paths, 256); err != nil {
		return nil, err
	}
	return paths, nil
}

// Propose admits exactly one immutable proposal in the bound execution. A
// interrupted effect is reconciled only under the same immutable provider intent.
func (i *Integration) Propose(ctx context.Context, p Proposal) (ProposalReceipt, error) {
	paths, err := validateProposal(p)
	if err != nil {
		return ProposalReceipt{}, err
	}
	grant, err := i.AuthorizeEffect(ctx, p.RequestID, OperationPropose, paths)
	if err != nil {
		return ProposalReceipt{}, err
	}
	d := i.deps()
	if d.Propose == nil {
		return ProposalReceipt{}, errors.New("bounded repository proposal adapter is unavailable")
	}
	fingerprint := digest(p)

	r, err := i.update(ctx, p.RequestID, func(r *record) error {
		// A review resumes the Work goal before any status poll need occur.
		// Reconcile its fresh decision under this existing row lock; the
		// read-only effect guard must never take another lock on this row.
		observed, err := i.observe(ctx, *r)
		if err != nil {
			return err
		}
		r.State, r.Reason = observed.State, observed.Reason
		if r.State != "approved" || !i.now().Before(r.ExpiresAt) {
			return errors.New("delegation changed before proposal intent")
		}
		if r.ProposalHash != "" {
			if r.ProposalHash != fingerprint {
				return errors.New("request already captured a different proposal")
			}
			return nil
		}
		r.ProposalHash, r.ProposalState = fingerprint, "prepared"
		r.ProposalCreatedAt = i.now()
		r.Proposal = &p
		return nil
	})
	if err != nil {
		return ProposalReceipt{}, err
	}
	if r.ProposalReceipt != nil {
		return *r.ProposalReceipt, nil
	}
	grant.EffectID, grant.EffectCreatedAt = r.ProposalHash, r.ProposalCreatedAt
	var result ProposalReceipt
	// Serialize replicas and revocation across this bounded provider effect. The
	// adapter reconciles this exact hash/timestamp/branch after a lost response;
	// it may never substitute a new head or replay a different proposal.
	_, err = i.update(ctx, p.RequestID, func(current *record) error {
		if current.ProposalHash != fingerprint || current.State != "approved" || !i.now().Before(current.ExpiresAt) {
			return errors.New("proposal authority changed")
		}
		if current.ProposalReceipt != nil {
			result = *current.ProposalReceipt
			return nil
		}
		var e error
		result, e = d.Propose(ctx, grant, p)
		if e != nil {
			return e
		}
		if result.State != "complete" || result.Branch == "" || !commit.MatchString(result.Commit) || result.PullRequestURL == "" {
			return errors.New("proposal adapter returned no complete immutable receipt")
		}
		current.ProposalState = "complete"
		current.ProposalReceipt = &result
		return nil
	})
	if err != nil {
		return ProposalReceipt{State: "prepared"}, err
	}
	return result, nil
}

func (i *Integration) ReadContext(ctx context.Context, id string) (RepositorySnapshot, error) {
	actor, err := person(ctx, "", "")
	if err != nil {
		return RepositorySnapshot{}, err
	}
	r, err := i.get(ctx, id, actor)
	if err != nil {
		return RepositorySnapshot{}, err
	}
	grant, err := i.AuthorizeEffect(ctx, id, r.Request.Operation, r.Request.Paths)
	if err != nil {
		return RepositorySnapshot{}, err
	}
	d := i.deps()
	if d.Read == nil {
		return RepositorySnapshot{}, errors.New("bounded repository read adapter is unavailable")
	}
	result, err := d.Read(ctx, grant, r.Request.Paths)
	if err != nil {
		return RepositorySnapshot{}, err
	}
	proposal := Proposal{RequestID: id, ExpectedHead: result.ExpectedHead, BaseBranch: result.BaseBranch, Title: "source snapshot", Files: result.Files}
	paths, err := validateProposal(proposal)
	if err != nil || !containsPaths(r.Request.Paths, paths) {
		return RepositorySnapshot{}, errors.New("repository snapshot exceeds the approved scope")
	}
	return result, nil
}
