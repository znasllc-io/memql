package vendorkeys

import (
	"context"
	"errors"
	"sort"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/memql"
)

// sweep.go -- the daily check of keys nothing has used (design record
// docs/superpowers/specs/2026-11-21-vendor-accounts-and-api-keys-design.md,
// decisions K11 and K12, owner decisions O2 and O3).
//
// # Why there is one
//
// Every call a key pays for is a check of it (observe.go). A key nobody has
// used for a day has had none, and a key can die without a call: its owner
// revokes it at the vendor, or the vendor does. A lent key that dies fails
// over to the next account without anybody noticing. So once a day the
// cluster lists the vendor's models with each key that has gone a day without
// a served call or a check, and records what the vendor said. Listing models
// sends no prompt and bills nothing (O3), which is what lets the cluster do it
// with a key that the cluster's own work may never spend (O2).
//
// # Which keys
//
// Every active key whose models the cluster lists (checkedBy "list"), with no
// served call and no check in the last day. Refused keys are included: a key
// the vendor accepts again recovers here.
//
// Two kinds are left out, each for a reason a reader could otherwise take for
// an oversight:
//
//   - A key that may not list models (K12). It is checked by the calls it pays
//     for and by nothing else; a listing would be refused for the restriction
//     and say nothing about the key.
//   - A key whose account is OUT OF CREDIT. A listing is answered whether or
//     not the account has credit, so a good answer would put the key back in
//     service on evidence that says nothing about credit. Its next real call
//     would fail, the key would fall again, and its owner would be told about
//     the same empty account once a day. The hold on such a key already runs
//     out after an hour and lets a real call try it (resolver.go).
//
// "A day" is 23 hours. The pass runs at the same minute every day, so a key
// it checked yesterday was checked a few seconds LESS than 24 hours ago, and a
// 24 hour rule would check every key every other day.
//
// # How much, and when it stops
//
// At most sweepLimit keys a run, the ones checked longest ago first, one at a
// time. The rest are counted and are first in line tomorrow. A vendor that
// answers three checks in a row with something that says nothing about the
// key (a rate limit, an outage, no answer) is not asked again in this run: its
// remaining keys wait for tomorrow, and the other vendor's keys are still
// checked. One key that cannot be opened or recorded is counted, logged
// without its value, and does not stop the run.
//
// # Who runs it
//
// The engine's own scheduled automation `vendorKeysDailyCheck`, on one bff
// replica behind a lease (dsl/router/automations.memql), and nobody else: not
// a person, a cluster owner included, and not an automation of the same name
// from a mounted bundle. A person who wants a key checked now has Check now on
// the key, which is this same check for one key they may see.
//
// Regression tests: sweep_db_test.go.

const (
	// sweepAutomation is the one automation the sweep answers to.
	sweepAutomation = "vendorKeysDailyCheck"

	// sweepIdle is how long a key must have gone without a served call and
	// without a check before the daily pass lists its models.
	sweepIdle = 23 * time.Hour

	// defaultSweepLimit is how many keys one run lists at most.
	defaultSweepLimit = 200

	// sweepGiveUpAfter is how many answers in a row that say nothing about a
	// key end a vendor's turn in one run.
	sweepGiveUpAfter = 3
)

// sweepRefusal is what anybody but the daily check is told.
const sweepRefusal = "The daily check of API keys is run by the cluster, once a day. To check one key now, open it in Settings, under API keys, and choose Check now."

// sweepCounts is what one run did.
type sweepCounts struct {
	// checked is how many keys a vendor gave a verdict on: good, refused and
	// credit together.
	checked, good, refused, credit int
	// skipped is how many keys were due and got no verdict in this run: past
	// the run's limit, at a vendor that was not answering, or not possible to
	// open or record.
	skipped int
}

// handleSweep runs the daily check. See the file comment for who may.
func (i *Integration) handleSweep(ctx context.Context, _ map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	ac, _ := auth.AccessFromContext(ctx)
	if !auth.IsEngineAutomationActor(ac, sweepAutomation) {
		return nil, refuse("%s", sweepRefusal)
	}
	counts, err := i.sweep(ctx)
	if err != nil {
		return nil, i.failed("sweep", "", err)
	}
	i.logger.Info("vendorkeys: the daily check of API keys ran",
		"checked", counts.checked, "good", counts.good, "refused", counts.refused,
		"credit", counts.credit, "skipped", counts.skipped)
	return i.node("sweep", map[string]any{
		"checked": counts.checked, "good": counts.good, "refused": counts.refused,
		"credit": counts.credit, "skipped": counts.skipped,
	})
}

// dueKey is a key the pass will list, with when it was last checked.
type dueKey struct {
	key       Key
	checkedAt time.Time
}

// sweep lists the models of every key that is due and records the answers.
// An error is a failure to READ which keys are due: nothing was checked.
func (i *Integration) sweep(ctx context.Context) (sweepCounts, error) {
	var counts sweepCounts
	due, err := i.dueForCheck(ctx, i.store.now().UTC())
	if err != nil {
		return counts, err
	}
	limit := i.sweepLimit
	if limit <= 0 {
		limit = defaultSweepLimit
	}
	if len(due) > limit {
		counts.skipped += len(due) - limit
		due = due[:limit]
	}

	silent := map[string]int{}
	for n, d := range due {
		if ctx.Err() != nil {
			// The run was cancelled: what is left waits for the next one.
			counts.skipped += len(due) - n
			break
		}
		if silent[d.key.Vendor] >= sweepGiveUpAfter {
			counts.skipped++
			continue
		}
		result, err := i.check(ctx, d.key)
		if err != nil {
			// Named by id, never by value. A worded refusal is this package's
			// own sentence; anything else is reduced to a reason a log can
			// keep, because an engine error can repeat what it was given.
			reason := failureReason(err)
			var worded *refusal
			if errors.As(err, &worded) {
				reason = worded.sentence
			}
			i.logger.Warn("vendorkeys: the daily check could not check an API key", keySubject(d.key.Id), "reason", reason)
			counts.skipped++
			continue
		}
		switch result.verdict {
		case memql.VendorKeyProbeGood:
			counts.checked, counts.good = counts.checked+1, counts.good+1
			silent[d.key.Vendor] = 0
		case memql.VendorKeyProbeRefused:
			counts.checked, counts.refused = counts.checked+1, counts.refused+1
			silent[d.key.Vendor] = 0
		case memql.VendorKeyProbeCredit:
			counts.checked, counts.credit = counts.checked+1, counts.credit+1
			silent[d.key.Vendor] = 0
		case memql.VendorKeyProbeUseOnly:
			// The key was restricted since it was added. The vendor answered,
			// and said nothing about whether the key works.
			counts.skipped++
			silent[d.key.Vendor] = 0
		default:
			counts.skipped++
			silent[d.key.Vendor]++
			if silent[d.key.Vendor] == sweepGiveUpAfter {
				i.logger.Warn("vendorkeys: a vendor is not answering key checks; its remaining keys wait for the next daily check",
					"vendor", d.key.Vendor)
			}
		}
	}
	return counts, nil
}

// dueForCheck reads which keys the pass lists now, the ones checked longest
// ago first (ties by id, so two replicas would agree).
func (i *Integration) dueForCheck(ctx context.Context, now time.Time) ([]dueKey, error) {
	var due []dueKey
	for _, vendor := range []string{memql.VendorAnthropic, memql.VendorOpenAI} {
		keys, err := i.store.ActiveKeysAtVendor(ctx, vendor)
		if err != nil {
			return nil, err
		}
		listed := make([]Key, 0, len(keys))
		ids := make([]string, 0, len(keys))
		for _, key := range keys {
			if key.CheckedBy == CheckedByUse {
				continue
			}
			listed = append(listed, key)
			ids = append(ids, key.Id)
		}
		states, err := i.store.States(ctx, ids)
		if err != nil {
			return nil, err
		}
		for _, key := range listed {
			state, found := states[key.Id]
			if !found {
				// A key with no record (Store.Add says how). Nothing has ever
				// been learned about it, so it is due, and first: its check is
				// what starts its record (observe.go).
				due = append(due, dueKey{key: key})
				continue
			}
			if state.State == StateCredit {
				// An account out of credit, which a listing cannot judge.
				continue
			}
			if now.Sub(state.GoodAt) < sweepIdle || now.Sub(state.CheckedAt) < sweepIdle {
				continue
			}
			due = append(due, dueKey{key: key, checkedAt: state.CheckedAt})
		}
	}
	sort.SliceStable(due, func(a, b int) bool {
		if !due[a].checkedAt.Equal(due[b].checkedAt) {
			return due[a].checkedAt.Before(due[b].checkedAt)
		}
		return due[a].key.Id < due[b].key.Id
	})
	return due, nil
}
