package repositoryauthority

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/znasllc-io/memql/component/identity/githubapp"
)

// MergeGroupReviewIntent contains only native observations and durable review
// receipts. It is not accepted by a public operation or a model-generated tool.
type MergeGroupReviewIntent struct {
	Source                   Source
	Evidence                 githubapp.MergeGroupEvidence
	Members                  []MergeGroupReviewMember
	ExternalID, ReviewDigest string
}
type MergeGroupReviewMember struct {
	RequestID, ProposalHash, ReviewDigest, ExternalID string
	CheckRunID, AppID                                 int64
}
type MergeGroupReviewReceipt struct {
	State, CheckName, HeadSHA, ExternalID, ReviewDigest string
	CheckRunID, AppID                                   int64
}
type mergeGroupReviewRecord struct {
	Intent  MergeGroupReviewIntent
	Receipt *MergeGroupReviewReceipt
}

// ReportMergeGroup is a native adapter seam, not a registered public builtin.
// The claimed pipeline host supplies the source, provider-observed membership
// and fresh lease fence. Every member must independently retain the maintainer
// delegation which produced its exact reviewed bytes. Pipeline ownership never
// substitutes for that delegation.
func (i *Integration) ReportMergeGroup(ctx context.Context, source Source, evidence githubapp.MergeGroupEvidence, fence func(context.Context) error) (MergeGroupReviewReceipt, error) {
	if fence == nil || i.deps().ReportMergeGroup == nil {
		return MergeGroupReviewReceipt{}, errors.New("merge-group review reporting is unavailable")
	}
	if err := fence(ctx); err != nil {
		return MergeGroupReviewReceipt{}, err
	}
	intent, err := i.mergeGroupIntent(ctx, source, evidence)
	if err != nil {
		return MergeGroupReviewReceipt{}, err
	}
	anchor := intent.Members[0].RequestID
	key := intent.ExternalID
	_, err = i.update(ctx, anchor, func(r *record) error {
		if r.MergeGroupReports == nil {
			r.MergeGroupReports = map[string]mergeGroupReviewRecord{}
		}
		if old, ok := r.MergeGroupReports[key]; ok {
			if digest(old.Intent) != digest(intent) {
				return errors.New("merge-group review intent changed")
			}
			return nil
		}
		if len(r.MergeGroupReports) >= 128 {
			return errors.New("this reviewed proposal reached its bounded merge-group report limit")
		}
		r.MergeGroupReports[key] = mergeGroupReviewRecord{Intent: intent}
		return nil
	})
	if err != nil {
		return MergeGroupReviewReceipt{}, err
	}
	authorize := func(c context.Context) error {
		if e := fence(c); e != nil {
			return e
		}
		current, e := i.mergeGroupIntent(c, source, evidence)
		if e != nil {
			return e
		}
		if digest(current) != digest(intent) {
			return errors.New("merge-group review authority changed")
		}
		return fence(c)
	}
	var out MergeGroupReviewReceipt
	_, err = i.update(ctx, anchor, func(r *record) error {
		saved, ok := r.MergeGroupReports[key]
		if !ok || digest(saved.Intent) != digest(intent) {
			return errors.New("merge-group review intent is not durable")
		}
		if e := authorize(ctx); e != nil {
			return e
		}
		if saved.Receipt != nil {
			if !matchingMergeGroupReview(intent, *saved.Receipt) {
				return errors.New("merge-group review receipt changed")
			}
			// A cached receipt is identity, not proof the provider still reports
			// this check. Reconcile it through the read-before-write adapter.
		}
		receipt, e := i.deps().ReportMergeGroup(ctx, intent, authorize)
		if e != nil {
			return e
		}
		if !matchingMergeGroupReview(intent, receipt) {
			return errors.New("merge-group check did not confirm exact app-owned review evidence")
		}
		if e := authorize(ctx); e != nil {
			return e
		}
		if saved.Receipt != nil && *saved.Receipt != receipt {
			return errors.New("recorded merge-group check identity changed")
		}
		out = receipt
		saved.Receipt = &receipt
		r.MergeGroupReports[key] = saved
		return nil
	})
	return out, err
}

func matchingMergeGroupReview(in MergeGroupReviewIntent, out MergeGroupReviewReceipt) bool {
	return len(in.Members) > 0 && out.State == "complete" && out.CheckName == ReviewCheckName && out.CheckRunID > 0 && out.AppID == in.Members[0].AppID && out.HeadSHA == in.Evidence.HeadSHA && out.ExternalID == in.ExternalID && out.ReviewDigest == in.ReviewDigest
}

func (i *Integration) mergeGroupIntent(ctx context.Context, source Source, evidence githubapp.MergeGroupEvidence) (MergeGroupReviewIntent, error) {
	out := MergeGroupReviewIntent{Source: source, Evidence: evidence}
	d := i.deps()
	if d.Source == nil || d.DeliveryContext == nil || len(evidence.Members) == 0 || len(evidence.Members) > 100 || evidence.RepositoryID <= 0 || !commit.MatchString(evidence.HeadSHA) || !commit.MatchString(evidence.BaseSHA) || evidence.HeadRef == "" || evidence.BaseBranch == "" || !strings.EqualFold(source.Repository, evidence.Repository) {
		return out, errors.New("merge-group review requires complete provider evidence")
	}
	fresh, err := d.Source(ctx, source.ID, true)
	if err != nil {
		return out, err
	}
	if fresh != source {
		return out, errors.New("pipeline source authority changed")
	}
	access, err := i.access(ctx, fresh)
	if err != nil {
		return out, err
	}
	if !access.CanWrite || access.RepositoryID != evidence.RepositoryID {
		return out, errors.New("pipeline source lacks current authority for this repository")
	}
	seen := map[string]bool{}
	for _, member := range evidence.Members {
		if member.Number <= 0 || !commit.MatchString(member.HeadSHA) || member.BaseBranch != evidence.BaseBranch || member.QueueEntryID == "" || seen[member.HeadSHA] {
			return out, errors.New("merge-group membership is incomplete or duplicated")
		}
		seen[member.HeadSHA] = true
		proof, present, e := i.InspectNativeReviewMember(ctx, source, evidence.RepositoryID, member)
		if e != nil {
			return out, e
		}
		if !present {
			return out, errNativeReviewAbsent
		}
		if len(out.Members) > 0 && proof.Review.AppID != out.Members[0].AppID {
			return out, errors.New("merge-group members were reviewed by different GitHub Apps")
		}
		out.Members = append(out.Members, proof.Review)
	}
	out.ReviewDigest = digest(struct {
		Domain   string
		Source   Source
		Evidence githubapp.MergeGroupEvidence
		Members  []MergeGroupReviewMember
	}{"merge-group-independent-review.v1", out.Source, out.Evidence, out.Members})
	out.ExternalID = "memql-review:" + out.ReviewDigest
	return out, nil
}

func (i *Integration) reviewedGroupMember(ctx context.Context, repositoryID int64, head, pullRequestURL string) (record, error) {
	var out record
	db, err := i.database()
	if err != nil {
		return out, err
	}
	// This native-only lookup is keyed by provider identity and immutable commit,
	// never by a requester-selected maintainer. Ambiguity refuses rather than
	// selecting whichever grant happens to be easiest to exercise.
	rows, err := db.QueryContext(ctx, `SELECT body FROM repository_change_requests WHERE body->>'RepositoryID'=$1 AND (body->'ProposalReceipt'->>'commit'=$2 OR lower(body->'ProposalReceipt'->>'pullRequestUrl')=lower($3) OR lower(body->'ReviewReportIntent'->'Proposal'->>'pullRequestUrl')=lower($3)) LIMIT 2`, fmt.Sprint(repositoryID), head, pullRequestURL)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	count := 0
	for rows.Next() {
		var raw []byte
		if err = rows.Scan(&raw); err != nil {
			return out, err
		}
		if err = json.Unmarshal(raw, &out); err != nil {
			return out, err
		}
		count++
	}
	if err = rows.Err(); err != nil {
		return out, err
	}
	if count == 0 {
		return out, errNativeReviewAbsent
	}
	if count != 1 {
		return out, errors.New("merge-group member has missing or ambiguous native review evidence")
	}
	return out, nil
}

// NativeReviewMember is an exact current journal proof, never an ordinary PR
// verdict. Present=true with an error forbids falling back to another reviewer.
type NativeReviewMember struct {
	Review  MergeGroupReviewMember
	BaseSHA string
}

var errNativeReviewAbsent = errors.New("native repository review evidence is absent")

func (i *Integration) InspectNativeReviewMember(ctx context.Context, source Source, repositoryID int64, member githubapp.MergeGroupMember) (NativeReviewMember, bool, error) {
	d := i.deps()
	if d.Source == nil || d.DeliveryContext == nil || repositoryID <= 0 || member.Number <= 0 || !commit.MatchString(member.HeadSHA) || member.BaseBranch == "" {
		return NativeReviewMember{}, true, errors.New("native member review identity is incomplete")
	}
	fresh, err := d.Source(ctx, source.ID, true)
	if err != nil {
		return NativeReviewMember{}, true, err
	}
	if fresh != source {
		return NativeReviewMember{}, true, errors.New("native member source changed")
	}
	access, err := i.access(ctx, fresh)
	if err != nil {
		return NativeReviewMember{}, true, err
	}
	if !access.CanWrite || access.RepositoryID != repositoryID {
		return NativeReviewMember{}, true, errors.New("native member repository authority changed")
	}
	r, e := i.reviewedGroupMember(ctx, repositoryID, member.HeadSHA, fmt.Sprintf("https://github.com/%s/pull/%d", source.Repository, member.Number))
	if errors.Is(e, errNativeReviewAbsent) {
		return NativeReviewMember{}, false, nil
	}
	if e != nil {
		return NativeReviewMember{}, true, e
	}
	c, e := d.DeliveryContext(ctx, r.Maintainer.OwnerUserID, r.GoalID, r.RunID)
	if e != nil {
		return NativeReviewMember{}, true, e
	}
	observed, e := i.observe(c, r)
	if e != nil {
		return NativeReviewMember{}, true, e
	}
	if observed.State != "approved" {
		return NativeReviewMember{}, true, fmt.Errorf("merge-group member approval is %s", observed.State)
	}
	if _, e = i.currentAuthority(c, r); e != nil {
		return NativeReviewMember{}, true, e
	}
	if r.Request.Operation != OperationDeliver || r.Proposal == nil || r.ProposalReceipt == nil || r.ReviewReceipt == nil || r.ReviewInput == nil || len(r.Route.ReviewPrompt) == 0 || !r.ReviewReceipt.Approved || r.ProposalHash != digest(*r.Proposal) || r.ReviewHash != r.ProposalHash || r.ReviewReceipt.ProposalHash != r.ProposalHash || digest(r.ReviewInput.Proposal) != r.ProposalHash || r.ProposalReceipt.State != "complete" || r.ProposalReceipt.Commit != member.HeadSHA || r.Proposal.BaseBranch != member.BaseBranch || !strings.EqualFold(r.Source.Repository, source.Repository) {
		return NativeReviewMember{}, true, errors.New("merge-group member has no exact accepted independent review")
	}
	expectedURL := fmt.Sprintf("https://github.com/%s/pull/%d", source.Repository, member.Number)
	if !strings.EqualFold(r.ProposalReceipt.PullRequestURL, expectedURL) {
		return NativeReviewMember{}, true, errors.New("merge-group member names a different pull request")
	}
	report := reviewReportIntent(r)
	if r.ReviewReportIntent == nil || digest(*r.ReviewReportIntent) != digest(report) || !matchingReviewReport(report, r.ReviewReportReceipt) {
		return NativeReviewMember{}, true, errors.New("merge-group member has no exact app-owned review report")
	}
	rr := r.ReviewReportReceipt
	return NativeReviewMember{BaseSHA: r.Proposal.ExpectedHead, Review: MergeGroupReviewMember{RequestID: r.ID, ProposalHash: r.ProposalHash, ReviewDigest: rr.ReviewDigest, ExternalID: rr.ExternalID, CheckRunID: rr.CheckRunID, AppID: rr.AppID}}, true, nil
}
