package memql

// THE SELF ACCOUNT CANNOT BE ARCHIVED (memql#4837).
//
// `v1:accounts:account:self` is the owner's own company -- the row the
// Accounts app's first-run card configures, the row every "who is this work
// for" answer is measured against, and the one account in the cluster that
// names the deployment itself rather than a client of it.
//
// ArchivePanel rendered for it like any other row, so the owner could file
// away their own company. There is no unarchive mutation and no delete in this
// model (archive is the only exit), so that was a one-click, one-way trip.
//
// REFUSED IN THE ENGINE, NOT HIDDEN IN THE UI, because archiveClientAccount
// takes the id as a CALLER ARGUMENT. Hiding the control stops the button and
// nothing else: the mutation is generated into both SDKs and reachable by any
// caller who can name the id, which is a literal constant anybody can read in
// this file. The panel stops rendering too -- an action you cannot take should
// not be offered -- but that is the courtesy, and this is the rule.
//
// It guards the STATUS FLIP rather than the mutation name. archiveClientAccount
// is the only writer today; keying on the name would leave a second one (a
// bulk tidy-up, a lifecycle automation) to rediscover this in production, and
// the invariant being protected is "this row is never archived", not "this one
// mutation does not archive it".

import (
	"context"
	"fmt"
	"strings"
)

// conceptAccountsAccount + selfAccountId name the singleton this guards.
// Written out rather than derived: the id is a literal in the DSL seed too,
// and a constant a reader can grep for beats a construction they have to
// reassemble.
const (
	conceptAccountsAccount = "v1:accounts:account"
	selfAccountId          = "v1:accounts:account:self"
)

// validateSelfAccountNotArchived refuses a write that would leave the self
// account archived.
//
// It reads the MERGED payload, which is what makes it a status check rather
// than a mutation check: whatever combination of delta and stored row produced
// this value, the question is the same one.
//
// NO ESCAPE, DELIBERATELY -- not internal origin, not the cluster owner. Every
// other guard in this package exempts one or both, because they protect a rule
// somebody with enough authority is entitled to break. This one does not: the
// row is a singleton with no second owner and no way back, so "an operator may
// do it on purpose" and "an operator did it by accident" are the same call, and
// the accident is unrecoverable. An operator who genuinely wants the row gone
// is describing a cluster with no company, which is not a state this model has.
func (e *MemQLEngine) validateSelfAccountNotArchived(ctx context.Context, id string, payload map[string]any) error {
	_ = ctx
	if payload == nil || strings.TrimSpace(id) != selfAccountId {
		return nil
	}
	if strings.TrimSpace(stringFromAny(payload["status"])) != "archived" {
		return nil
	}
	return fmt.Errorf(
		"v1:accounts:account: %s is this cluster's own company and cannot be archived (memql#4837). "+
			"Archive is the only exit in this model -- there is no unarchive and no delete -- so "+
			"archiving the self account would leave the deployment with no company and no way back. "+
			"Every other field stays editable, including name and domain",
		selfAccountId)
}
