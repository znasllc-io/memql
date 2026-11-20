package calls

import (
	"context"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	langparser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/memql"

	callrules "github.com/znasllc-io/memql/component/calls"
)

// policy.go -- a site's calls limits (v1:calls:policy): the field table, the conversion
// between the stored seconds and the rules module's durations, the read and the write.
//
// # Per site, and the site owner's row
//
// A policy is a SITE'S. It was keyed by a package deployable's name, which is unique
// only together with its package, so one package's limits reached every other
// package's deployable of that name, across organizations. It is now one row per site,
// at the site's own id (policyIDFor), owned by the site's owner and read and written as
// them with internal origin (policyContext, over asOwner), as a key is. The operator who set a limit is
// not its owner: the row stays the site's whoever changes it.
//
// # The bounds are the cluster's
//
// Every value callsPolicySet takes is held to policyFields' bounds before it is
// written. They keep a value meaningful and they are the MOST any site may claim: a
// deployable's operator chooses inside them, never past them, so no site can give
// itself a room the size of the cluster or a pass that outlives MintPass's ceiling.

const policyConcept = "v1:calls:policy"

// policyField is one calls.Policy value as v1:calls:policy stores it. The table is the
// ONE place a stored number becomes a Policy field and back: counts as they are, and
// durations in WHOLE SECONDS (the concept's unit), converted here and nowhere else.
type policyField struct {
	name     string // the v1:calls:policy field, and the callsPolicySet argument
	seconds  bool   // stored in seconds; a time.Duration in calls.Policy
	min, max int    // the cluster's bounds, in the stored unit (see the file note)
	get      func(callrules.Policy) int
	set      func(*callrules.Policy, int)
}

// policyFields are the cluster's bounds (see the file note). A room admits at least one
// person (the rules refuse a room of agents); a pass lives long enough to connect with
// and no longer than MintPass mints; a waiter polls at least once a minute.
var policyFields = []policyField{
	{"usagePreview", false, 0, 1, func(p callrules.Policy) int {
		if p.UsagePreview {
			return 1
		}
		return 0
	}, func(p *callrules.Policy, v int) { p.UsagePreview = v == 1 }},
	{"simulatedFunding", false, 0, 1, func(p callrules.Policy) int {
		if p.SimulatedFunding {
			return 1
		}
		return 0
	}, func(p *callrules.Policy, v int) { p.SimulatedFunding = v == 1 }},
	{"requirePasskeys", false, 0, 1, func(p callrules.Policy) int {
		if p.RequirePasskeys {
			return 1
		}
		return 0
	}, func(p *callrules.Policy, v int) { p.RequirePasskeys = v == 1 }},
	{"maxPeople", false, 1, 1000,
		func(p callrules.Policy) int { return p.MaxPeople }, func(p *callrules.Policy, v int) { p.MaxPeople = v }},
	{"maxAgents", false, 0, 100,
		func(p callrules.Policy) int { return p.MaxAgents }, func(p *callrules.Policy, v int) { p.MaxAgents = v }},
	{"roomJoinsPerMinute", false, 1, 10000,
		func(p callrules.Policy) int { return p.RoomJoinsPerMinute }, func(p *callrules.Policy, v int) { p.RoomJoinsPerMinute = v }},
	{"maxUpcomingScheduled", false, 0, 1000,
		func(p callrules.Policy) int { return p.MaxUpcomingScheduled }, func(p *callrules.Policy, v int) { p.MaxUpcomingScheduled = v }},
	{"maxScheduledMinutes", false, 1, 1440,
		func(p callrules.Policy) int { return p.MaxScheduledMinutes }, func(p *callrules.Policy, v int) { p.MaxScheduledMinutes = v }},
	{"hostLeftGrace", true, 0, 86400,
		func(p callrules.Policy) int { return seconds(p.HostLeftGrace) }, func(p *callrules.Policy, v int) { p.HostLeftGrace = duration(v) }},
	{"passTtl", true, 10, int(MaxPassTTL / time.Second),
		func(p callrules.Policy) int { return seconds(p.PassTTL) }, func(p *callrules.Policy, v int) { p.PassTTL = duration(v) }},
	{"scheduleHorizon", true, 0, 366 * 86400,
		func(p callrules.Policy) int { return seconds(p.ScheduleHorizon) }, func(p *callrules.Policy, v int) { p.ScheduleHorizon = duration(v) }},
	{"queueEntryTtl", true, 10, 86400,
		func(p callrules.Policy) int { return seconds(p.QueueEntryTTL) }, func(p *callrules.Policy, v int) { p.QueueEntryTTL = duration(v) }},
	{"queuePoll", true, 1, 60,
		func(p callrules.Policy) int { return seconds(p.QueuePoll) }, func(p *callrules.Policy, v int) { p.QueuePoll = duration(v) }},
}

func seconds(d time.Duration) int  { return int(d / time.Second) }
func duration(s int) time.Duration { return time.Duration(s) * time.Second }

// policyRow is one stored v1:calls:policy: the values somebody set, by field name in
// the stored unit, and nothing for a value nobody set.
type policyRow struct {
	ID, SiteID string
	Values     map[string]int
	Version    time.Time
}

// policyFromValues is the effective policy: the shipped defaults with every stored
// value laid over them.
func policyFromValues(values map[string]int) callrules.Policy {
	p := callrules.DefaultPolicy()
	for _, f := range policyFields {
		if v, set := values[f.name]; set {
			f.set(&p, v)
		}
	}
	return p
}

// policyIDFor is the id of a site's policy row: the site's own bare id, the same for
// every writer, so one site has one row and two first writes meet at it (the gate they
// run under lets one of them create it).
func policyIDFor(siteID string) string { return memql.BareShortId(siteID) }

// policyContext is the context a site's policy row is read and written under: the
// site's owner, with internal origin. asOwner already stamps the origin; it is stamped
// again here because callsPolicyForSite, createCallsPolicy and updateCallsPolicy are
// @serverOnly, and the server-only caller gate
// (test/dslconformance/server_only_callers_stamp_test.go) asks the file that renders a
// @serverOnly call to stamp the origin itself, so the stamp stays visible beside the
// calls it admits. A blank owner answers false with the context unchanged, as asOwner's
// does.
func policyContext(ctx context.Context, owner string) (context.Context, bool) {
	owned, ok := asOwner(ctx, owner)
	if !ok {
		return ctx, false
	}
	return auth.ContextWithInternalOrigin(owned), true
}

// policyRowFor reads a site's stored limits as the site's owner, within that site. None
// is (nil, nil).
func (s *Store) policyRowFor(ctx context.Context, owner, siteID string) (*policyRow, error) {
	owned, ok := policyContext(ctx, owner)
	if !ok {
		return nil, errNoOwner
	}
	call, err := langparser.RenderCall("callsPolicyForSite", map[string]any{"siteId": memql.BareShortId(siteID)})
	rows, err := s.read(memql.ContextWithRootsOnlyRead(memql.ContextWithFreshRead(owned)), "the deployable's calls limits", call, err)
	if err != nil || len(rows) == 0 {
		return nil, err
	}
	r := rows[0]
	row := &policyRow{
		ID:      memql.BareShortId(rowString(r, "id")),
		SiteID:  memql.BareShortId(rowString(r, "siteId")),
		Values:  map[string]int{},
		Version: rowTime(r, "createdAt"),
	}
	for _, f := range policyFields {
		if v, set := rowIntPresent(r, f.name); set {
			row.Values[f.name] = v
		}
	}
	return row, nil
}

// PolicyFor is the effective policy of one site: the shipped defaults
// (calls.DefaultPolicy) with every value an operator set laid over them, durations
// converted from the stored seconds, read as the site's owner. A site nobody set limits
// for gets the defaults.
func (s *Store) PolicyFor(ctx context.Context, owner, siteID string) (callrules.Policy, error) {
	row, err := s.policyRowFor(ctx, owner, siteID)
	if err != nil {
		return callrules.Policy{}, err
	}
	if row == nil {
		return callrules.DefaultPolicy(), nil
	}
	return policyFromValues(row.Values), nil
}

// savePolicy writes the named values of p, in the stored unit, as the site's owner:
// onto the site's row when it has one (prior, fenced on the version read), or a new row
// at policyIDFor otherwise (fenced on its absence). Values p carries for fields not
// named are not written, so they keep what they had.
func (s *Store) savePolicy(ctx context.Context, owner string, prior *policyRow, siteID string, p callrules.Policy, named []string, versionTime time.Time) (string, error) {
	owned, ok := policyContext(ctx, owner)
	if !ok {
		return "", errNoOwner
	}
	args := map[string]any{"versionTime": versionStamp(versionTime)}
	for _, f := range policyFields {
		for _, name := range named {
			if name == f.name {
				args[f.name] = f.get(p)
			}
		}
	}
	if prior == nil {
		id := policyIDFor(siteID)
		args["policyId"], args["siteId"] = id, memql.BareShortId(siteID)
		call, err := langparser.RenderCall("createCallsPolicy", args)
		return id, s.write(witnessedAbsent(owned, policyConcept, id), "the calls limits", call, err)
	}
	fenced, err := witnessed(owned, policyConcept, prior.ID, prior.Version)
	if err != nil {
		return "", err
	}
	args["policyId"] = prior.ID
	call, err := langparser.RenderCall("updateCallsPolicy", args)
	return prior.ID, s.write(fenced, "the calls limits", call, err)
}
