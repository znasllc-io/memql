package campaigns

import (
	"context"
	"sort"
	"strings"
	"sync"
	"time"
)

// reputation.go -- the per-domain counters (memql#3462).
//
// # Why this half is worth having on its own
//
// The issue is a request for a warming ramp and a finding that one could not
// honestly be built: a ramp advances when the previous step produced good
// signals, and this deployment collected none. No delivery-vs-bounce ratio
// over time, no complaint rate, and above all no per-domain breakdown --
// which is the one that matters, because mailbox providers judge a sender
// independently. "Our bounce rate is 1%" is not a number any of them acts
// on. "Our complaint rate at gmail.com is 0.4% this week" is.
//
// So this ships first and stands alone. It answers a question an operator
// asks long before they want anything automated, and it is the input without
// which the ramp in warmup.go would be a hardcoded table pretending to be a
// control loop.
//
// # Accumulated in memory, written absolute, one row per replica
//
// The send path increments a map, not a row. Flushing writes the running
// total to a row keyed by (identity, domain, day, THIS node) -- absolute
// values, never increments, so there is no read-modify-write anywhere and no
// interleaving for two replicas to lose counts to. A reader sums across
// nodeIds, which it was going to do across domains and days anyway.
//
// The one thing that costs: a replica restarting mid-day would write a total
// that started again from zero and clobber its own earlier row. So the first
// flush after boot SEEDS from what this node already wrote today. That is
// one read per process, and without it a rolling deploy would quietly reset
// the evidence the ramp is holding on.
//
// # What `accepted` means, precisely
//
// Messages the TRANSPORT accepted. Not deliveries: Graph answers 202 and the
// bounce arrives hours later through the feedback path. Delivery is inferred
// as accepted minus bounces, and there is no `delivered` counter because
// nothing on this side could honestly produce one.

// reputationKey is one counter bucket.
type reputationKey struct {
	identity string
	domain   string
	day      string // YYYY-MM-DD, UTC
}

type reputationCounts struct {
	accepted   int
	hardBounce int
	softBounce int
	complaint  int
}

// reputationCollector accumulates counters and flushes them.
type reputationCollector struct {
	mu       sync.Mutex
	counts   map[reputationKey]*reputationCounts
	seeded   map[reputationKey]bool
	identity string
	nodeID   string
}

func newReputationCollector(identity, nodeID string) *reputationCollector {
	if strings.TrimSpace(identity) == "" {
		identity = "default"
	}
	if strings.TrimSpace(nodeID) == "" {
		// A deployment that does not set MEMQL_NODE_ID is a single-process
		// one, and one bucket per (identity, domain, day) is exactly right
		// there. Naming it rather than leaving the field empty keeps the
		// derived row id well-formed.
		nodeID = "single"
	}
	return &reputationCollector{
		counts:   map[reputationKey]*reputationCounts{},
		seeded:   map[reputationKey]bool{},
		identity: identity,
		nodeID:   nodeID,
	}
}

// observe adds one event for an address's domain. Called on the send path
// (accepted) and from the feedback path (the three verdicts).
func (c *reputationCollector) observe(now time.Time, addr, field string) {
	if c == nil {
		return
	}
	domain := EmailDomain(addr)
	if domain == "" {
		return
	}
	key := reputationKey{identity: c.identity, domain: domain, day: now.UTC().Format(time.DateOnly)}

	c.mu.Lock()
	defer c.mu.Unlock()
	bucket, ok := c.counts[key]
	if !ok {
		bucket = &reputationCounts{}
		c.counts[key] = bucket
	}
	switch field {
	case "accepted":
		bucket.accepted++
	case "hard_bounce":
		bucket.hardBounce++
	case "soft_bounce":
		bucket.softBounce++
	case "complaint":
		bucket.complaint++
	}
}

// pending returns the buckets with something to write, deterministically
// ordered so a log or a test sees a reproducible sequence.
func (c *reputationCollector) pending() []reputationKey {
	c.mu.Lock()
	defer c.mu.Unlock()
	keys := make([]reputationKey, 0, len(c.counts))
	for k := range c.counts {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].day != keys[j].day {
			return keys[i].day < keys[j].day
		}
		return keys[i].domain < keys[j].domain
	})
	return keys
}

func (c *reputationCollector) snapshot(key reputationKey) (reputationCounts, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	bucket, ok := c.counts[key]
	if !ok {
		return reputationCounts{}, false
	}
	return *bucket, true
}

// seed folds a previously-written total for this node back into memory, once
// per bucket per process. See the file doc: without it a restart mid-day
// clobbers this node's earlier row with a total that began again at zero.
func (c *reputationCollector) seed(key reputationKey, prior reputationCounts) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.seeded[key] {
		return
	}
	c.seeded[key] = true
	bucket, ok := c.counts[key]
	if !ok {
		bucket = &reputationCounts{}
		c.counts[key] = bucket
	}
	bucket.accepted += prior.accepted
	bucket.hardBounce += prior.hardBounce
	bucket.softBounce += prior.softBounce
	bucket.complaint += prior.complaint
}

func (c *reputationCollector) needsSeed(key reputationKey) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return !c.seeded[key]
}

// flushReputation writes every dirty bucket. Called once per drain pass;
// best-effort, because a lost counter delays a ramp decision and nothing
// worse.
func (w *Worker) flushReputation(systemCtx context.Context) {
	if w.reputation == nil || w.store == nil {
		return
	}
	for _, key := range w.reputation.pending() {
		if w.reputation.needsSeed(key) {
			prior, err := w.store.ReputationForBucket(systemCtx, key, w.reputation.nodeID)
			if err != nil {
				// Not seeded, so not written: writing an unseeded total would
				// clobber this node's earlier row for the day. Try again next
				// pass.
				w.logger.Debug("campaigns: could not read this node's existing reputation counters", "error", err)
				continue
			}
			w.reputation.seed(key, prior)
		}
		counts, ok := w.reputation.snapshot(key)
		if !ok {
			continue
		}
		if err := w.store.RecordReputationWindow(systemCtx, key, w.reputation.nodeID, counts); err != nil {
			w.logger.Debug("campaigns: could not write reputation counters", "domain", key.domain, "error", err)
		}
	}
}

// DomainReputation is the aggregate an operator or the ramp reads: one
// domain's counters over a rolling window, summed across replicas and days.
type DomainReputation struct {
	Domain     string
	Accepted   int
	HardBounce int
	SoftBounce int
	Complaint  int
}

// HardBounceRate and ComplaintRate are the two the ramp thresholds on.
//
// The denominator is `accepted`, not accepted-minus-bounces, because that is
// the ratio a mailbox provider computes about a sender: of everything you
// tried to send us, how much came back. A zero denominator returns 0 rather
// than an error -- a step that has sent nothing has no rate, and the ramp's
// volume condition is what stops it reading that as a clean bill of health.
func (d DomainReputation) HardBounceRate() float64 { return ratio(d.HardBounce, d.Accepted) }

// ComplaintRate is the same ratio for spam complaints, which providers weigh
// an order of magnitude more heavily.
func (d DomainReputation) ComplaintRate() float64 { return ratio(d.Complaint, d.Accepted) }

func ratio(n, d int) float64 {
	if d <= 0 {
		return 0
	}
	return float64(n) / float64(d)
}

// AggregateReputation sums the window rows into per-domain totals plus one
// "" entry carrying the deployment-wide total.
//
// The deployment-wide row is included and is deliberately NOT what the ramp
// thresholds on. It is the number an operator asks for first and the number
// that hides the problem: one badly-performing domain inside a large healthy
// aggregate is exactly the shape a per-domain breakdown exists to reveal.
func AggregateReputation(rows []ReputationWindow) map[string]DomainReputation {
	out := map[string]DomainReputation{}
	add := func(domain string, r ReputationWindow) {
		agg := out[domain]
		agg.Domain = domain
		agg.Accepted += r.Accepted
		agg.HardBounce += r.HardBounce
		agg.SoftBounce += r.SoftBounce
		agg.Complaint += r.Complaint
		out[domain] = agg
	}
	for _, r := range rows {
		if strings.TrimSpace(r.Domain) == "" {
			continue
		}
		add(r.Domain, r)
		add("", r)
	}
	return out
}
