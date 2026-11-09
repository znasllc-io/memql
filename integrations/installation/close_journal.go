package installation

import (
	"context"
	"database/sql"
	"errors"
	"slices"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/znasllc-io/memql/integrations/argocd"
)

// maxCloseReasonRunes matches the installation_revision_close CHECK, which
// counts characters the same way.
const maxCloseReasonRunes = 500

// validCloseReason bounds the operator's own words. They are shown back to
// operators beside the closed attempt, so a reason is one printable line.
func validCloseReason(reason string) error {
	if !utf8.ValidString(reason) || reason != strings.TrimSpace(reason) || reason == "" || utf8.RuneCountInString(reason) > maxCloseReasonRunes {
		return errors.New("closing an installation needs a reason of at most 500 characters")
	}
	for _, r := range reason {
		if !unicode.IsPrint(r) {
			return errors.New("an installation close reason must be one line of printable text")
		}
	}
	return nil
}

// startedIntent is one controller intent this attempt durably started, with
// the member it belongs to ("" for a single-Application plan).
type startedIntent struct {
	application string
	digest      string
	intent      argocd.Intent
}

// startedIntentList names, once each and in member order, every controller
// intent this attempt may have written: the forward intent once started (in a
// group, each started member), and for a recorded restoration each started
// member's planned reversal and any reconciled or replanned replacement of it.
// An unstarted member or reversal was never written, because every Apply
// commits its durable start first, under the same effect lock a close holds.
func (r revisionRecord) startedIntentList() ([]startedIntent, error) {
	out := []startedIntent{}
	seen := map[string]bool{}
	add := func(application string, intent argocd.Intent) error {
		digest, err := intent.Digest()
		if err != nil {
			return err
		}
		if !seen[digest] {
			seen[digest] = true
			out = append(out, startedIntent{application: application, digest: digest, intent: intent})
		}
		return nil
	}
	started := r.State == "applying" || r.State == "reverting" || r.State == "complete" || r.State == "rolled_back" || r.State == "closed"
	for _, app := range r.Plan.applicationMembers() {
		forward := started
		if r.Plan.FormatVersion == 2 {
			forward = r.Applications[app.Key].Started
		}
		if forward {
			if err := add(app.Key, app.Intent); err != nil {
				return nil, err
			}
		}
		reverse, ok := r.Rollback.application(app.Key)
		if !ok || !reverse.Started {
			continue
		}
		planned := r.Rollback.Plan.Intent
		if r.Rollback.Plan.FormatVersion == 2 {
			for _, member := range r.Rollback.Plan.Applications {
				if member.Key == app.Key {
					planned = member.Intent
				}
			}
		}
		if err := add(app.Key, planned); err != nil {
			return nil, err
		}
		if saved := r.Rollback.Applications[app.Key].ReconciledIntent; saved != nil {
			if err := add(app.Key, *saved); err != nil {
				return nil, err
			}
		}
		if err := add(app.Key, reverse.Intent); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// startedIntents indexes startedIntentList by intent digest. A plan whose
// stored intents no longer validate has none.
func (r revisionRecord) startedIntents() map[string]startedIntent {
	list, err := r.startedIntentList()
	out := map[string]startedIntent{}
	if err != nil {
		return out
	}
	for _, item := range list {
		out[item.digest] = item
	}
	return out
}

// dispatchedIntents is startedIntentList's intents, the set a close examines.
func (r revisionRecord) dispatchedIntents() ([]argocd.Intent, error) {
	list, err := r.startedIntentList()
	if err != nil {
		return nil, err
	}
	out := make([]argocd.Intent, 0, len(list))
	for _, item := range list {
		out = append(out, item.intent)
	}
	return out, nil
}

func intentDigests(intents []argocd.Intent) ([]string, error) {
	out := make([]string, 0, len(intents))
	for _, intent := range intents {
		digest, err := intent.Digest()
		if err != nil {
			return nil, err
		}
		out = append(out, digest)
	}
	sort.Strings(out)
	return out, nil
}

// close ends a started attempt that can no longer reach a completion receipt,
// typically because its saved protected-resource evidence cannot be reproduced
// (Preparer.CloseSelected). It is the original operator's statement, not
// evidence: no receipt is written, Restore previous release stays refused
// afterwards (rollbackEligibility admits only a complete attempt), and the
// attempt keeps every observation and recorded reversal it had. It releases the
// installation head and the group's Application claims, so a new preparation
// can begin.
//
// Only applying is closed here. A prepared plan is cancelled instead. A
// restoration of a completed update (reverting) is not closed into this state:
// its forward completion was proven, so a restoration that wrote no Argo intent
// returns to that completed attempt instead (withdrawRestoration, reached from
// CloseSelected), and one that wrote anything cannot be closed at all.
//
// The caller holds the plan's effect lock and has found every intent in settled
// with no controller operation of its own still waiting or running. The lock
// excludes every Apply of the plan on every host while that classification and
// this transaction run, and the set is compared again here under the head lock,
// so an intent that started since cannot be closed unexamined.
func (j *revisionJournal) close(ctx context.Context, installation, key, reason string, settled []string) (revisionRecord, error) {
	if err := validCloseReason(reason); err != nil {
		return revisionRecord{}, err
	}
	return j.withRecord(ctx, installation, key, func(tx *sql.Tx, r *revisionRecord, actor string) error {
		if actor != r.Plan.RequestedBy {
			return errors.New("only the operator who started this installation may close it")
		}
		if r.State == "closed" {
			return nil // A lost reply recovers the stored close; its reason stands.
		}
		if r.State != "applying" {
			return errors.New("only an installation that is still being applied can be closed")
		}
		intents, err := r.dispatchedIntents()
		if err != nil {
			return err
		}
		digests, err := intentDigests(intents)
		if err != nil {
			return err
		}
		if !slices.Equal(digests, settled) {
			return errChanged
		}
		if _, err := tx.ExecContext(ctx, `UPDATE installation_revision_attempts SET state='closed',closed_at=clock_timestamp(),closed_by=$2,close_reason=$3,updated_at=clock_timestamp() WHERE plan_id=$1`, key, actor, reason); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE installation_revision_heads SET active_plan_id=NULL WHERE installation_id=$1 AND active_plan_id=$2 AND slot_epoch=$3`, installation, key, r.SlotEpoch); err != nil {
			return err
		}
		if r.Plan.Preparation != nil {
			return releaseApplicationClaims(ctx, tx, installation, r.Plan.Preparation.ID)
		}
		return nil
	})
}
