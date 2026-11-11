package shopify

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode"

	memqlsync "github.com/znasllc-io/memql/component/memql/sync"
	"github.com/znasllc-io/memql/integrations/shopify/generated"
)

// verdict.go -- one word for each store, and why.
//
// A store read "Connected" as soon as its tokens existed, whatever the mirror
// was doing. The verdict answers the question a person managing a storefront
// asks instead: is this store being recorded, and if not, what fixes it. It is
// computed here, in one place, from what was read about the store -- its row,
// its sync state, its given-up deliveries -- and the screens draw it without
// a copy of the rules.
//
// computeVerdict is PURE: it reads nothing and writes nothing. What it is
// given is gathered by Connector.storeVerdict (capabilities.go), and the
// watch that records it and mails the owner is a later pass of its own.

// The five states, exactly as the screens and the watch compare them.
const (
	VerdictRecording      = "recording"
	VerdictStarting       = "starting"
	VerdictPaused         = "paused"
	VerdictNeedsAttention = "needs attention"
	VerdictNotConnected   = "not connected"
)

// The reasons a store needs attention, in the order they are listed.
const (
	ReasonDataNotDeclared     = "data_not_declared"
	ReasonApprovalOutdated    = "approval_outdated"
	ReasonGrantLost           = "grant_lost"
	ReasonEventsNotRegistered = "events_not_registered"
	ReasonCheckFailing        = "check_failing"
	ReasonChangesNotRecorded  = "changes_not_recorded"
)

// The one act of a reason. A reason with no act (check_failing) names when
// the next sweep runs instead.
const (
	ActApprove   = "approve"
	ActReconnect = "reconnect"
	ActRegister  = "register"
	ActRetry     = "retry"
	ActDeclare   = "declare"
)

// protectedDataURL is Shopify's own page on declaring protected customer
// data: the OS has no helper for linking into MemQL's docs, and the steps in
// shopify-connector.md start from this page.
const protectedDataURL = "https://shopify.dev/docs/apps/launch/protected-customer-data"

// subscriptionsUnreadable is the failure a subscription pass records when it
// could not read the store's subscriptions at all, so nothing else it did is
// known. The connect's pass and the daily pass both record it.
const subscriptionsUnreadable = "subscriptions: the store's webhook subscriptions could not be read"

// VerdictReason is one reason a store needs attention: a code, one sentence
// for a person, and at most one act.
type VerdictReason struct {
	Code, Domain, Sentence, Detail string
	Count                          int
	Act, ActLabel, ActURL          string // Act: "", "approve", "reconnect", "register", "retry", "declare"
}

// RecordedKind is one line of "What is recorded": when the last change of the
// kind arrived and when it was last checked.
type RecordedKind struct{ Kind, LastChangeAt, LastCheckedAt string }

// Verdict is one store's state, with what holds it there.
type Verdict struct {
	StoreID, Domain, State string
	Since                  time.Time
	ResumedAt              time.Time
	ConnectedAt            time.Time
	Reasons                []VerdictReason
	Recorded               []RecordedKind
	Outside, Withheld      []string
	OrdersFrom             string
}

// verdictInput is everything computeVerdict reads, gathered beforehand.
type verdictInput struct {
	Store     Store
	States    []map[string]any         // this store's sync-state rows
	Intervals map[string]time.Duration // concept id to its sweep interval
	GivenUp   []map[string]any         // this store's given-up deliveries
	Denied    map[string][]string      // fieldsDenied
	Previous  map[string]any           // health.verdict as last recorded
	// ConnectedAt is when the store was connected, or zero when the row
	// does not say. One of the three times a late check is measured from.
	ConnectedAt time.Time
	Now         time.Time
}

// recordedKinds are the kinds "What is recorded" lists, each with the domain
// it reads its two times from. Returns are read from orders and listed only
// while the grant holds read_returns.
var recordedKinds = []struct{ kind, concept, needs string }{
	{"catalog", "product", ""},
	{"orders", "order", ""},
	{"customers", "customer", ""},
	{"discounts", "discountNode", ""},
	{"returns", "order", "read_returns"},
}

// computeVerdict decides one store's state, first match: disconnected, then
// paused, then any reason, then a backfill not yet done or a domain never
// swept, else recording. A paused or disconnected store carries no reasons:
// nothing it would say is something to act on while it is not recorded.
func computeVerdict(in verdictInput) Verdict {
	store := in.Store
	rows := rowsByConcept(in.States)
	v := Verdict{
		StoreID:     store.ID,
		Domain:      store.Domain,
		OrdersFrom:  healthString(store.Health, "ordersFrom"),
		ConnectedAt: in.ConnectedAt,
		Recorded:    recordedOf(store, rows),
		Outside:     outsideOf(store, rows),
		Withheld:    withheldOf(in.Denied),
	}
	switch {
	case store.UninstalledAt != "" || store.RedactedAt != "":
		v.State = VerdictNotConnected
	case store.Status == StatusPaused:
		v.State = VerdictPaused
	}
	v.ResumedAt = resumedAtOf(in.Previous, v.State == "", in.Now)
	if v.State == "" {
		v.Reasons = reasonsOf(in, rows, lateFrom(in.ConnectedAt, v.ResumedAt))
		switch {
		case len(v.Reasons) > 0:
			v.State = VerdictNeedsAttention
		case starting(store, rows):
			v.State = VerdictStarting
		default:
			v.State = VerdictRecording
		}
	}
	v.Since = sinceOf(in.Previous, v.State, in.Now)
	return v
}

// askedDomains is what the verdict asks about: the domains the sweep
// considers on its own, inside the store's connection. A child read with its
// parent, and a domain the connection never asks for, are never asked.
func askedDomains(store Store) []string {
	return sweptDomains(func(spec *generated.TypeSpec) bool { return domainInConnection(store, spec) })
}

func reasonsOf(in verdictInput, rows map[string]map[string]any, from time.Time) []VerdictReason {
	store := in.Store
	asked := askedDomains(store)
	var out []VerdictReason

	// A row refused as not requested -- a scope the connection never asks
	// for, a feature the plan does not have -- is outside the connection and
	// never a reason, whatever else holds below.
	inside := asked[:0:0]
	for _, concept := range asked {
		if !isNotRequested(mapString(rows[concept], "lastError")) {
			inside = append(inside, concept)
		}
	}

	var undeclared []string
	for _, concept := range inside {
		lastError := mapString(rows[concept], "lastError")
		if isNotGranted(lastError) && protectedDataRefusal(lastError) {
			undeclared = append(undeclared, plainDomain(concept))
		}
	}
	if len(undeclared) > 0 {
		out = append(out, VerdictReason{
			Code:     ReasonDataNotDeclared,
			Domain:   joinWords(undeclared),
			Sentence: "Shopify refuses " + joinWords(undeclared) + " for this app, because the app has not declared the customer data it uses.",
			Act:      ActDeclare, ActLabel: "Declare the customer data", ActURL: protectedDataURL,
		})
	}

	if missing := scopesNotApproved(store); len(missing) > 0 {
		out = append(out, VerdictReason{
			Code:     ReasonApprovalOutdated,
			Sentence: "This store approved an older list of permissions.",
			Detail:   strings.Join(missing, ", "),
			Act:      ActApprove, ActLabel: "Open MemQL OS from the store's Shopify admin and approve",
			ActURL: "https://" + store.Domain + "/admin/apps",
		})
	}

	for _, concept := range inside {
		if lastError := mapString(rows[concept], "lastError"); grantLost(lastError) {
			out = append(out, VerdictReason{
				Code: ReasonGrantLost, Sentence: "MemQL can no longer reach this store.", Detail: lastError,
				Act: ActReconnect, ActLabel: "Reconnect in Settings",
			})
			break
		}
	}

	if detail, ok := eventsNotRegistered(store); ok {
		out = append(out, VerdictReason{
			Code: ReasonEventsNotRegistered, Sentence: "Shopify is not sending every change.", Detail: detail,
			Act: ActRegister, ActLabel: "Register again",
		})
	}

	for _, concept := range inside {
		if r, ok := checkFailing(concept, rows[concept], intervalOf(in.Intervals, concept), from, in.Now); ok {
			out = append(out, r)
		}
	}

	if n := changesNotRecorded(in.GivenUp, inside, rows); n > 0 {
		sentence := fmt.Sprintf("%d changes could not be recorded.", n)
		if n == 1 {
			sentence = "1 change could not be recorded."
		}
		out = append(out, VerdictReason{
			Code: ReasonChangesNotRecorded, Sentence: sentence, Count: n,
			Act: ActRetry, ActLabel: "Try again",
		})
	}
	return out
}

// grantLost reports whether a domain's error says the store's grant no longer
// works: the offline grant could not be renewed, or Shopify answered 401.
func grantLost(lastError string) bool {
	return strings.Contains(lastError, "reconnect this store") ||
		strings.Contains(lastError, "could not be renewed") ||
		strings.Contains(lastError, "HTTP 401")
}

func isNotGranted(lastError string) bool {
	return strings.HasPrefix(lastError, memqlsync.ErrNotGranted.Error())
}

func isNotRequested(lastError string) bool {
	return strings.HasPrefix(lastError, memqlsync.ErrNotRequested.Error())
}

// eventsNotRegistered reads the last subscription pass off the store's
// health: a failure, or fewer subscriptions left than it wanted.
//
// LIKE WITH LIKE. `desired` counts the generated topics the store's grant
// holds (EnsureSubscriptionsForStore leaves out the ones it cannot, as
// notGranted), and a grant can hold a scope its connection does not ask for.
// A failure of a topic whose domain is outside the connection is therefore
// not counted, and neither is the subscription it left out of `desired`. A
// failure that names no generated topic -- the list could not be read -- is
// always counted. No pass recorded yet is not a reason: nothing is known.
func eventsNotRegistered(store Store) (string, bool) {
	sub, ok := store.Health["subscriptions"].(map[string]any)
	if !ok {
		return "", false
	}
	var failed []string
	outside := 0
	for _, entry := range mapStringSlice(sub, "failed") {
		topic, _, _ := strings.Cut(entry, ":")
		if route, known := generated.Topics[strings.TrimSpace(topic)]; known {
			if spec := generated.Types[route.Concept]; spec != nil && !domainInConnection(store, spec) {
				outside++
				continue
			}
		}
		failed = append(failed, entry)
	}
	left := mapInt(sub, "existing") + len(mapStringSlice(sub, "created")) - len(mapStringSlice(sub, "removed"))
	short := left < mapInt(sub, "desired")-outside
	if len(failed) == 0 && !short {
		return "", false
	}
	if len(failed) > 0 {
		return strings.Join(failed, "; "), true
	}
	return fmt.Sprintf("The last registration left %d of the %d subscriptions it wanted.", left, mapInt(sub, "desired")-outside), true
}

// checkFailing is one domain's failing check: a backfill that failed, at
// once; or, on a row that was attempted, twice the domain's interval passed
// since its baseline with no clean sweep after it. That stands for "failed
// its last two sweeps" without a counter of its own.
//
// THE BASELINE is the latest of the domain's last clean sweep, the store's
// connection (from) and its last return from paused or not connected
// (from). Measured from the clean sweep alone, a store resumed after a long
// pause read late until its first sweep, and a domain never yet swept clean
// was late at its first error. With no baseline at all -- no clean sweep, no
// connection time, never resumed -- an attempted domain with no clean sweep
// is late, as nothing says how long it has been waiting.
//
// An error about the grant is its own reason and never this one, and a domain
// an operator paused is not failing.
func checkFailing(concept string, row map[string]any, interval time.Duration, from, now time.Time) (VerdictReason, bool) {
	if row == nil {
		return VerdictReason{}, false
	}
	lastError := mapString(row, "lastError")
	if grantLost(lastError) || isNotGranted(lastError) || isNotRequested(lastError) {
		return VerdictReason{}, false
	}
	word := plainDomain(concept)
	r := VerdictReason{Code: ReasonCheckFailing, Domain: word, Sentence: "The last check of " + word + " failed"}
	if mapString(row, "backfillStatus") == "failed" {
		// Without this, a store whose past could not be brought in would
		// read recording while its sweeps are clean.
		r.Detail = lastError
		if r.Detail == "" {
			r.Detail = "The backfill did not finish."
		}
		return r, true
	}
	if paused, _ := rowValue(row, "paused").(bool); paused {
		return VerdictReason{}, false
	}
	clean := rowTime(row, "lastReconcileAt")
	checked := rowTime(row, "lastAttemptAt")
	if checked.IsZero() {
		checked = clean
	}
	if checked.IsZero() && lastError == "" {
		// Never attempted: the store is starting, not failing.
		return VerdictReason{}, false
	}
	baseline := clean
	if from.After(baseline) {
		baseline = from
	}
	if !baseline.IsZero() && now.Sub(baseline) <= 2*interval {
		return VerdictReason{}, false
	}
	if lastError != "" {
		r.Detail = strings.TrimRight(lastError, ".") + "."
	} else {
		r.Detail = "No check has run since " + checked.UTC().Format(time.RFC3339) + "."
	}
	if next := checked.Add(interval); !checked.IsZero() && next.After(now) {
		r.Detail += " The next check is at " + next.UTC().Format(time.RFC3339) + "."
	} else {
		r.Detail += " The next check is due now."
	}
	return r, true
}

// changesNotRecorded counts the given-up deliveries received after the last
// clean sweep of the domain their topic routes to. One older than that sweep
// is not counted: the sweep read what it named. One whose topic routes to no
// domain asked about is not counted either.
func changesNotRecorded(givenUp []map[string]any, inside []string, rows map[string]map[string]any) int {
	asked := map[string]bool{}
	for _, concept := range inside {
		asked[concept] = true
	}
	n := 0
	for _, delivery := range givenUp {
		route, ok := generated.Topics[generated.TopicHeaderToEnum(topicHeaderOf(delivery))]
		if !ok {
			continue
		}
		concept := generated.ConceptID(route.Concept)
		if !asked[concept] {
			continue
		}
		received := rowTime(delivery, "receivedAt")
		clean := rowTime(rows[concept], "lastReconcileAt")
		if clean.IsZero() || received.IsZero() || received.After(clean) {
			n++
		}
	}
	return n
}

// topicHeaderOf is a staged delivery's X-Shopify-Topic header. The receiver
// stages header names lowercased; the match is case-blind anyway.
func topicHeaderOf(delivery map[string]any) string {
	var headers map[string]any
	if err := json.Unmarshal([]byte(mapString(delivery, "headersJson")), &headers); err != nil {
		return ""
	}
	for k, v := range headers {
		if strings.EqualFold(k, HeaderTopic) {
			if s, ok := v.(string); ok {
				return s
			}
		}
	}
	return ""
}

// starting is a domain of the connection whose backfill is queued or
// running, or a swept domain of it never attempted.
func starting(store Store, rows map[string]map[string]any) bool {
	for concept, row := range rows {
		if spec := generated.Types[generated.ConceptFromID(concept)]; spec == nil || !domainInConnection(store, spec) {
			continue
		}
		if s := mapString(row, "backfillStatus"); s == "queued" || s == "running" {
			return true
		}
	}
	for _, concept := range askedDomains(store) {
		row := rows[concept]
		if row == nil || (mapString(row, "lastAttemptAt") == "" && mapString(row, "lastReconcileAt") == "") {
			return true
		}
	}
	return false
}

// lateFrom is the later of the store's connection and its last resume: the
// part of a late check's baseline that is the store's rather than a domain's.
func lateFrom(connectedAt, resumedAt time.Time) time.Time {
	if resumedAt.After(connectedAt) {
		return resumedAt
	}
	return connectedAt
}

// resumedAtOf is when the store last left paused or not connected: now, when
// the last recorded verdict held one of the two and the store is active
// again; otherwise what that verdict carried, unchanged, or zero when it
// carried none.
func resumedAtOf(previous map[string]any, active bool, now time.Time) time.Time {
	if previous == nil {
		return time.Time{}
	}
	if was := mapString(previous, "state"); active && (was == VerdictPaused || was == VerdictNotConnected) {
		return now.UTC()
	}
	return rowTime(previous, "resumedAt")
}

// sinceOf is when the state began: kept from the last recorded verdict while
// the state is the same, else now.
func sinceOf(previous map[string]any, state string, now time.Time) time.Time {
	if previous != nil && mapString(previous, "state") == state {
		if at, err := time.Parse(time.RFC3339Nano, mapString(previous, "since")); err == nil {
			return at.UTC()
		}
	}
	return now.UTC()
}

// recordedOf is one line for each kind in the store's connection.
func recordedOf(store Store, rows map[string]map[string]any) []RecordedKind {
	granted := grantIncludes(store.ScopesGranted)
	out := []RecordedKind{}
	for _, k := range recordedKinds {
		spec := generated.Types[k.concept]
		if spec == nil || !domainInConnection(store, spec) || (k.needs != "" && !granted[k.needs]) {
			continue
		}
		row := rows[generated.ConceptID(k.concept)]
		checked := mapString(row, "lastAttemptAt")
		if checked == "" {
			checked = mapString(row, "lastReconcileAt")
		}
		out = append(out, RecordedKind{Kind: k.kind, LastChangeAt: mapString(row, "lastInboundAt"), LastCheckedAt: checked})
	}
	return out
}

// outsideOf names, in plain words, the swept domains the store's connection
// does not ask for, and the ones refused as not requested although it does
// (a feature the store's plan does not have). The row's own sentence is never
// repeated: it says what the sweep found, not what a person should read.
func outsideOf(store Store, rows map[string]map[string]any) []string {
	seen := map[string]bool{}
	out := []string{}
	add := func(concept string) {
		if !seen[concept] {
			seen[concept] = true
			out = append(out, plainDomain(concept))
		}
	}
	outside := map[string]bool{}
	for _, concept := range domainsOutside(store) {
		outside[concept] = true
	}
	for _, concept := range sweptDomains(func(*generated.TypeSpec) bool { return true }) {
		if outside[concept] || isNotRequested(mapString(rows[concept], "lastError")) {
			add(concept)
		}
	}
	return out
}

// withheldOf is the fields Shopify withholds, in plain words: "product
// variants: delivery profile". A withheld field is never a reason.
func withheldOf(denied map[string][]string) []string {
	out := []string{}
	for concept, fields := range denied {
		for _, field := range fields {
			parts := strings.Split(field, ".")
			for i, p := range parts {
				parts[i] = camelWords(p)
			}
			out = append(out, plainDomain(concept)+": "+strings.Join(parts, " "))
		}
	}
	sort.Strings(out)
	return out
}

// plainNames is how a person reads a domain. A concept not listed is its
// camel-case name split into lower-case words.
var plainNames = map[string]string{
	"order":             "orders",
	"customer":          "customers",
	"product":           "products",
	"productVariant":    "product variants",
	"collection":        "collections",
	"discountNode":      "discounts",
	"inventoryItem":     "inventory",
	"location":          "locations",
	"abandonedCheckout": "abandoned checkouts",
	"tenderTransaction": "payments",
	"segment":           "customer segments",
	"company":           "companies",
	"companyLocation":   "company locations",
	"priceList":         "price lists",
	"sellingPlanGroup":  "selling plans",
	"inventoryShipment": "inventory shipments",
	"inventoryTransfer": "inventory transfers",
}

// plainDomain names a domain, given by concept id or by its bare name, in
// plain words.
func plainDomain(concept string) string {
	name := generated.ConceptFromID(strings.TrimSpace(concept))
	if word, ok := plainNames[name]; ok {
		return word
	}
	return camelWords(name)
}

// camelWords splits a camel-case name into lower-case words: "urlRedirect"
// is "url redirect", "shopifyQLTable" is "shopify ql table".
func camelWords(name string) string {
	runes := []rune(name)
	var b strings.Builder
	for i, r := range runes {
		if i > 0 && unicode.IsUpper(r) {
			prev := runes[i-1]
			nextLower := i+1 < len(runes) && unicode.IsLower(runes[i+1])
			if unicode.IsLower(prev) || unicode.IsDigit(prev) || (unicode.IsUpper(prev) && nextLower) {
				b.WriteByte(' ')
			}
		}
		b.WriteRune(unicode.ToLower(r))
	}
	return b.String()
}

// joinWords joins names as a sentence does: "orders", "orders and
// customers", "orders, customers and products".
func joinWords(words []string) string {
	switch len(words) {
	case 0:
		return ""
	case 1:
		return words[0]
	}
	return strings.Join(words[:len(words)-1], ", ") + " and " + words[len(words)-1]
}

// intervalOf is a domain's sweep interval, from the connector's own domain
// list, or the allowlist's cadence when the list does not name it.
func intervalOf(intervals map[string]time.Duration, concept string) time.Duration {
	if d, ok := intervals[concept]; ok && d > 0 {
		return d
	}
	if spec := generated.Types[generated.ConceptFromID(concept)]; spec != nil {
		return parseCadence(spec.Cadence, spec.Reconcile)
	}
	return time.Hour
}

// sweepIntervals is each inbound domain's sweep interval, by concept id.
func sweepIntervals(domains []memqlsync.DomainSpec) map[string]time.Duration {
	out := map[string]time.Duration{}
	for _, d := range domains {
		if d.Direction == memqlsync.DirectionInbound {
			out[d.Concept] = d.ReconcileInterval
		}
	}
	return out
}

func rowsByConcept(states []map[string]any) map[string]map[string]any {
	out := map[string]map[string]any{}
	for _, row := range states {
		if concept := mapString(row, "conceptId"); concept != "" {
			out[concept] = row
		}
	}
	return out
}

// rowTime reads a datetime field, or the zero time when it is absent or
// unreadable.
func rowTime(row map[string]any, key string) time.Time {
	at, err := time.Parse(time.RFC3339Nano, mapString(row, key))
	if err != nil {
		return time.Time{}
	}
	return at.UTC()
}

func healthString(health map[string]any, key string) string {
	s, _ := health[key].(string)
	return strings.TrimSpace(s)
}
