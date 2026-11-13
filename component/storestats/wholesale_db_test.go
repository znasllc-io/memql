package storestats

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/uptrace/bun/dialect/pgdialect"

	"github.com/znasllc-io/memql/component/auth"
	langparser "github.com/znasllc-io/memql/component/language/parser"
	memqldsl "github.com/znasllc-io/memql/dsl"
)

// wholesale_db_test.go -- who is wholesale (wholesale.go): the wholesale
// pack's two reads, asked once a run, and the pass that judges a store's
// orders again when its granted set changes. Against the real engine with the
// pack linked (harness_db_test.go), its rows written through its own
// mutations by a merchant, as the pack's own tests write them.

const (
	buyerOne   = "buyer-1@example.test"
	buyerTwo   = "buyer-2@example.test"
	orderOne   = "gid://shopify/Order/2001"
	orderTwo   = "gid://shopify/Order/2002"
	orderThree = "gid://shopify/Order/2003"
)

// wholesaleMerchant owns the fixture rows written through the pack's internal
// row writer, as its admitted production handlers write them.
const wholesaleMerchant = "dbtest-storestats-merchant"

func asMerchant() context.Context {
	return auth.ContextWithInternalOrigin(auth.ContextWithUserActor(context.Background(), wholesaleMerchant))
}

// canonicalStore is the other form a wholesale row may hold a store id in.
func canonicalStore(id string) string { return "v1:shopify:store:" + id }

// wholesaleStore is runStore whose wholesale rows, under either form of its
// id, go when the test ends.
func wholesaleStore(t *testing.T) (*Integration, string) {
	t.Helper()
	i, id := runStore(t)
	forgetPackRows(t, id)
	return i, id
}

// forgetPackRows deletes the wholesale pack's rows of the store, under either
// form of its id, when the test ends.
func forgetPackRows(t *testing.T, id string) {
	t.Helper()
	_, db := statsEngine(t)
	t.Cleanup(func() {
		_, _ = db.ExecContext(context.Background(), `DELETE FROM "MemoryNodes" WHERE concept LIKE 'v1:wholesale:%' AND payload->>'storeId' = ANY(?)`,
			pgdialect.Array([]string{id, canonicalStore(id)}))
	})
}

func packExec(t *testing.T, call string) {
	t.Helper()
	eng, _ := statsEngine(t)
	if _, err := eng.Execute(asMerchant(), call); err != nil {
		t.Fatalf("%s: %v", call, err)
	}
}

// settingsFor gives the store wholesale settings, under key: the store's bare
// id or its other form.
func settingsFor(t *testing.T, id, key string) {
	t.Helper()
	packExec(t, `setWholesaleSettingsRow(settingsId: `+langparser.QuoteString("settings-"+id)+`, storeId: `+langparser.QuoteString(key)+
		`, ownerUserId: `+langparser.QuoteString("v1:identity:user:"+wholesaleMerchant)+`, applicationsOpen: true, entitlementAdapter: "customerTag")`)
}

// grant writes the store's entitlement name, under key, for email in state.
// The same name again is a new version of the same entitlement: a revocation.
func grant(t *testing.T, id, key, name, email, state string) {
	t.Helper()
	app := "app-" + name + "-" + id
	owner := langparser.QuoteString("v1:identity:user:" + wholesaleMerchant)
	when := langparser.QuoteString(time.Now().UTC().Format(time.RFC3339Nano))
	packExec(t, `createApplicationRow(applicationId: `+langparser.QuoteString(app)+`, storeId: `+langparser.QuoteString(key)+
		`, ownerUserId: `+owner+`, submittedAt: `+when+`, companyName: "Corner Bicycle Co", applicantName: "Applicant", applicantEmail: "applicant")`)
	packExec(t, `writeEntitlementRow(entitlementId: `+langparser.QuoteString("ent-"+name+"-"+id)+`, applicationId: `+langparser.QuoteString(app)+
		`, ownerUserId: `+owner+`, attemptedAt: `+when+`, storeId: `+langparser.QuoteString(key)+`, buyerEmail: `+langparser.QuoteString(email)+`, adapter: "customerTag", state: `+
		langparser.QuoteString(state)+`)`)
}

// olderOrder mirrors an order of the store by email, written an hour ago:
// behind where the walk stands after its first run, so a later run's walk
// does not fold it again and only a wholesale pass judges it again.
func olderOrder(t *testing.T, id, gid, email string) {
	t.Helper()
	mirrorOrder(t, id, gid, "email", email)
	backdate(t, orderConcept, id, gid, time.Now().Add(-time.Hour))
}

func wholesaleOfFact(t *testing.T, id, gid string) string {
	t.Helper()
	fact := orderFactsOf(t, id)[gid]
	if fact == nil {
		t.Fatalf("order %s has no fact", gid)
	}
	return cell(fact, "wholesale")
}

func runOK(t *testing.T, i *Integration) {
	t.Helper()
	if _, err := run(t, i); err != nil {
		t.Fatal(err)
	}
}

// packOff turns the wholesale pack off on this node for the rest of the test,
// or until packOn.
func packOff(t *testing.T) {
	t.Helper()
	was := memqldsl.DisabledPackDomains()
	memqldsl.SetDisabledPackDomains(append(slices.Clone(was), wholesaleDomain))
	t.Cleanup(func() { memqldsl.SetDisabledPackDomains(was) })
}

func packOn() {
	memqldsl.SetDisabledPackDomains(slices.DeleteFunc(memqldsl.DisabledPackDomains(), func(d string) bool { return d == wholesaleDomain }))
}

// noBuyersRead is the integration's engine, refusing the buyers read: a run
// that makes no wholesale pass does not notice.
func noBuyersRead(i *Integration) {
	i.engine = &hookEngine{inner: i.engine, before: func(query string) error {
		if strings.Contains(query, "storeStatisticsOrderBuyers") {
			return errors.New("the buyers were read: a wholesale pass ran")
		}
		return nil
	}}
}

// A GRANTED BUYER'S ORDERS BECOME WHOLESALE WITHIN ONE RUN. Two orders,
// folded retail; the first buyer is granted; the next run judges both again:
// the granted buyer's is wholesale, the other's is not, and the pass ends.
func TestAGrantedBuyersOrdersBecomeWholesaleWithinOneRun(t *testing.T) {
	i, id := wholesaleStore(t)
	settingsFor(t, id, id)
	olderOrder(t, id, orderOne, buyerOne)
	olderOrder(t, id, orderTwo, buyerTwo)
	runOK(t, i)
	if got := wholesaleOfFact(t, id, orderOne); got != "false" {
		t.Fatalf("before any grant the first order is wholesale %s, want false", got)
	}
	grant(t, id, id, "one", buyerOne, "granted")
	runOK(t, i)
	if got := wholesaleOfFact(t, id, orderOne); got != "true" {
		t.Errorf("a granted buyer's order is wholesale %s after the run, want true", got)
	}
	if got := wholesaleOfFact(t, id, orderTwo); got != "false" {
		t.Errorf("another buyer's order is wholesale %s, want false", got)
	}
	row := storeRow(t, id)
	if row["wholesale_enabled"] != true || row["wholesale_cursor"] != "" || row["wholesale_digest"] != grantedDigest(map[string]bool{buyerOne: true}) {
		t.Errorf("after the run the store is enabled %v, its cursor %q, its digest %v; want enabled, no pass under way, and the granted set's digest",
			row["wholesale_enabled"], row["wholesale_cursor"], row["wholesale_digest"])
	}
}

// A REVOKED BUYER'S ORDERS GO BACK TO RETAIL.
func TestARevokedBuyersOrdersGoBackToRetail(t *testing.T) {
	i, id := wholesaleStore(t)
	settingsFor(t, id, id)
	grant(t, id, id, "one", buyerOne, "granted")
	olderOrder(t, id, orderOne, buyerOne)
	runOK(t, i)
	if got := wholesaleOfFact(t, id, orderOne); got != "true" {
		t.Fatalf("a granted buyer's order is wholesale %s, want true", got)
	}
	grant(t, id, id, "one", buyerOne, "revoked")
	runOK(t, i)
	if got := wholesaleOfFact(t, id, orderOne); got != "false" {
		t.Errorf("after the revocation the order is wholesale %s, want false", got)
	}
}

// A NEW ORDER BY A GRANTED BUYER IS WHOLESALE WHEN IT IS WRITTEN: the fold
// judges it with the run's state, and no pass reads it.
func TestANewOrderByAGrantedBuyerIsWholesaleWhenItIsWritten(t *testing.T) {
	i, id := wholesaleStore(t)
	settingsFor(t, id, id)
	grant(t, id, id, "one", buyerOne, "granted")
	runOK(t, i) // the store is enabled, and its pass, over no order, ends
	mirrorOrder(t, id, orderOne, "email", buyerOne)
	noBuyersRead(i)
	runOK(t, i)
	if got := wholesaleOfFact(t, id, orderOne); got != "true" {
		t.Errorf("a new order by a granted buyer is wholesale %s, want true", got)
	}
}

// A BUYER IS MATCHED WHATEVER CASE AND SPACES THE EMAIL CARRIES, by the pass
// as by the fold.
func TestABuyerIsMatchedWhateverCaseAndSpacesTheEmailCarries(t *testing.T) {
	i, id := wholesaleStore(t)
	settingsFor(t, id, id)
	olderOrder(t, id, orderOne, " Buyer-1@Example.TEST ")
	runOK(t, i)
	grant(t, id, id, "one", buyerOne, "granted")
	runOK(t, i)
	if got := wholesaleOfFact(t, id, orderOne); got != "true" {
		t.Errorf("an order by %q is wholesale %s beside an entitlement of %s, want true", " Buyer-1@Example.TEST ", got, buyerOne)
	}
	mirrorOrder(t, id, orderTwo, "email", " BUYER-1@example.test")
	runOK(t, i)
	if got := wholesaleOfFact(t, id, orderTwo); got != "true" {
		t.Errorf("a new order by the buyer in capitals is wholesale %s, want true", got)
	}
}

// AN ORDER WITH NO EMAIL IS NEVER WHOLESALE, even beside an entitlement whose
// buyerEmail is empty; the granted buyer's order beside it is.
func TestAnOrderWithNoEmailIsNeverWholesale(t *testing.T) {
	i, id := wholesaleStore(t)
	settingsFor(t, id, id)
	grant(t, id, id, "blank", "", "granted")
	grant(t, id, id, "one", buyerOne, "granted")
	olderOrder(t, id, orderOne, buyerOne)
	olderOrder(t, id, orderTwo, "")
	olderOrder(t, id, orderThree, "   ")
	runOK(t, i)
	if got := wholesaleOfFact(t, id, orderOne); got != "true" {
		t.Errorf("the granted buyer's order is wholesale %s, want true", got)
	}
	for _, gid := range []string{orderTwo, orderThree} {
		if got := wholesaleOfFact(t, id, gid); got != "false" {
			t.Errorf("order %s, with no email, is wholesale %s, want false", gid, got)
		}
	}
}

// GRANTED BUYERS ARE FOUND UNDER EITHER FORM OF THE STORE ID (Review Focus):
// the settings under one form, an entitlement under each, both buyers
// wholesale.
func TestGrantedBuyersAreFoundUnderEitherFormOfTheStoreId(t *testing.T) {
	i, id := wholesaleStore(t)
	settingsFor(t, id, canonicalStore(id))
	grant(t, id, id, "bare", buyerOne, "granted")
	grant(t, id, canonicalStore(id), "canonical", buyerTwo, "granted")
	olderOrder(t, id, orderOne, buyerOne)
	olderOrder(t, id, orderTwo, buyerTwo)
	runOK(t, i)
	for _, gid := range []string{orderOne, orderTwo} {
		if got := wholesaleOfFact(t, id, gid); got != "true" {
			t.Errorf("order %s is wholesale %s, want true: its buyer's entitlement was not found under the form it holds", gid, got)
		}
	}
}

// A STORE WITH NO SETTINGS ROW IS NOT ENABLED, whatever it has granted; the
// store beside it with settings is.
func TestAStoreWithNoSettingsRowIsNotEnabled(t *testing.T) {
	i, set := wholesaleStore(t)
	unset := keptStore(t)
	forgetPackRows(t, unset)
	settingsFor(t, set, set)
	for _, id := range []string{set, unset} {
		grant(t, id, id, "one", buyerOne, "granted")
		olderOrder(t, id, orderOne, buyerOne)
	}
	runOK(t, i)
	if got := wholesaleOfFact(t, set, orderOne); got != "true" {
		t.Errorf("in the store with settings the granted buyer's order is wholesale %s, want true", got)
	}
	if got := wholesaleOfFact(t, unset, orderOne); got != "false" {
		t.Errorf("in the store with no settings the order is wholesale %s, want false", got)
	}
	if row := storeRow(t, unset); row["wholesale_enabled"] != false {
		t.Errorf("the store with no settings is kept enabled %v, want false", row["wholesale_enabled"])
	}
	states, err := i.wholesaleOf(walkContext(walkCtx(t)), []string{set, unset})
	if err != nil {
		t.Fatal(err)
	}
	if !states[set].Enabled || states[unset].Enabled {
		t.Errorf("wholesaleOf answers enabled %v for the store with settings and %v for the one without, want true and false",
			states[set].Enabled, states[unset].Enabled)
	}
}

// WITH THE PACK OFF NO PACK QUERY IS ASKED AND THE FLAGS ARE LEFT: the store
// is kept not enabled, its digest and its cursor stay, and an order judged
// wholesale stays wholesale.
func TestWithThePackOffNoPackQueryIsAskedAndTheFlagsAreLeft(t *testing.T) {
	i, id := wholesaleStore(t)
	settingsFor(t, id, id)
	grant(t, id, id, "one", buyerOne, "granted")
	olderOrder(t, id, orderOne, buyerOne)
	runOK(t, i)
	if got := wholesaleOfFact(t, id, orderOne); got != "true" {
		t.Fatalf("with the pack on the granted buyer's order is wholesale %s, want true", got)
	}
	cursor := time.Now().Add(-30 * time.Minute).UTC().Format(time.RFC3339Nano)
	exec(t, `UPDATE store_sales_store SET wholesale_cursor = ? WHERE store_id = ?`, cursor, id)
	digest := storeRow(t, id)["wholesale_digest"]

	packOff(t)
	engine := &contextEngine{inner: i.engine}
	i.engine = engine
	runOK(t, i)
	for _, c := range engine.calls {
		if strings.Contains(c.query, "wholesaleStatistics") {
			t.Errorf("with the pack off the run asked %s", c.query)
		}
	}
	if engine.asked() == 0 {
		t.Fatal("the run asked nothing, so nothing above measures what it asked")
	}
	row := storeRow(t, id)
	if row["wholesale_enabled"] != false || row["wholesale_digest"] != digest || row["wholesale_cursor"] != cursor {
		t.Errorf("with the pack off the store is enabled %v, digest %v, cursor %v; want not enabled, and %v and %v left",
			row["wholesale_enabled"], row["wholesale_digest"], row["wholesale_cursor"], digest, cursor)
	}
	if got := wholesaleOfFact(t, id, orderOne); got != "true" {
		t.Errorf("with the pack off the order's flag moved to %s; want it left", got)
	}
}

// A STORE ENABLED AGAIN JUDGES THE ORDERS WRITTEN MEANWHILE. The store was
// enabled with this granted set; with the pack off a granted buyer's order is
// folded retail; with the pack on again, the same set, the pass judges it.
func TestAStoreEnabledAgainJudgesTheOrdersWrittenMeanwhile(t *testing.T) {
	i, id := wholesaleStore(t)
	_, db := statsEngine(t)
	settingsFor(t, id, id)
	grant(t, id, id, "one", buyerOne, "granted")
	ensureRow(t, db, id)
	digest := grantedDigest(map[string]bool{buyerOne: true})
	exec(t, `UPDATE store_sales_store SET wholesale_enabled = true, wholesale_digest = ? WHERE store_id = ?`, digest, id)

	packOff(t)
	olderOrder(t, id, orderOne, buyerOne)
	runOK(t, i)
	if got := wholesaleOfFact(t, id, orderOne); got != "false" {
		t.Fatalf("with the pack off the order was folded wholesale %s, want false", got)
	}
	if row := storeRow(t, id); row["wholesale_enabled"] != false || row["wholesale_digest"] != digest {
		t.Fatalf("with the pack off the store is enabled %v with digest %v; want not enabled, its digest kept", row["wholesale_enabled"], row["wholesale_digest"])
	}

	packOn()
	runOK(t, i)
	if got := wholesaleOfFact(t, id, orderOne); got != "true" {
		t.Errorf("enabled again with the same granted set, the order written meanwhile is wholesale %s, want true", got)
	}
}

// AN ORDER WHOSE EMAIL WAS REMOVED GOES BACK TO RETAIL: its row rewritten with
// another email, the fold judges the new version, with no pass.
func TestAnOrderWhoseEmailWasRemovedGoesBackToRetail(t *testing.T) {
	i, id := wholesaleStore(t)
	settingsFor(t, id, id)
	grant(t, id, id, "one", buyerOne, "granted")
	olderOrder(t, id, orderOne, buyerOne)
	runOK(t, i)
	if got := wholesaleOfFact(t, id, orderOne); got != "true" {
		t.Fatalf("the granted buyer's order is wholesale %s, want true", got)
	}
	mirrorOrder(t, id, orderOne, "email", "someone-else@example.test")
	noBuyersRead(i)
	runOK(t, i)
	if got := wholesaleOfFact(t, id, orderOne); got != "false" {
		t.Errorf("the order rewritten with another email is wholesale %s, want false", got)
	}
}

// passStore is a store of the test's own with its row, its floor at floor,
// and its generation.
func passStore(t *testing.T, floor time.Time) (*Integration, string, int64) {
	t.Helper()
	i, id := wholesaleStore(t)
	_, db := statsEngine(t)
	ensureRow(t, db, id)
	floorAt(t, floor)
	generation, err := generationOf(context.Background(), db, id)
	if err != nil {
		t.Fatal(err)
	}
	return i, id, generation
}

func enabledWith(emails ...string) wholesaleState {
	granted := map[string]bool{}
	for _, e := range emails {
		granted[foldEmail(e)] = true
	}
	return wholesaleState{Enabled: true, Granted: granted, Digest: grantedDigest(granted)}
}

func buyersReads(e *pagingEngine) []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	var out []string
	for _, r := range e.reads {
		if strings.Contains(r.query, "storeStatisticsOrderBuyers") {
			out = append(out, r.query)
		}
	}
	return out
}

func sinceOf(t *testing.T, query string) time.Time {
	t.Helper()
	m := sinceInRead.FindStringSubmatch(query)
	if m == nil {
		t.Fatalf("no since in %s", query)
	}
	at, err := time.Parse(time.RFC3339Nano, m[1])
	if err != nil {
		t.Fatal(err)
	}
	return at
}

// A PASS THAT STOPPED AT ITS BUDGET CARRIES ON FROM ITS CURSOR. 600 orders,
// three pages, a budget of one page: the first two passes stop with the
// cursor set, each next one reads on from it, and the third ends the pass.
func TestAPassThatStoppedAtItsBudgetCarriesOnFromItsCursor(t *testing.T) {
	i, id, generation := passStore(t, time.Now().Add(-time.Hour))
	for n := range 600 {
		mirror(t, orderConcept, id, fmt.Sprintf("gid://shopify/Order/%d", 5000+n), map[string]any{"email": buyerOne})
	}
	engine := &pagingEngine{inner: i.engine}
	i.engine = engine
	st := enabledWith(buyerOne)
	var cursors []string
	for pass := 1; pass <= 3; pass++ {
		if pass > 1 {
			engine.mu.Lock()
			engine.reads = nil
			engine.mu.Unlock()
		}
		if err := wholesalePass(walkCtx(t), i, id, generation, st, time.Now, 1); err != nil {
			t.Fatalf("pass %d: %v", pass, err)
		}
		if pass > 1 {
			reads := buyersReads(engine)
			if len(reads) == 0 {
				t.Fatalf("pass %d read no buyers", pass)
			}
			kept, err := time.Parse(time.RFC3339Nano, cursors[len(cursors)-1])
			if err != nil {
				t.Fatalf("the cursor %q is not an instant: %v", cursors[len(cursors)-1], err)
			}
			if since := sinceOf(t, reads[0]); !since.Equal(kept.Add(-watermarkOverlap)) {
				t.Errorf("pass %d read from %v, want the cursor %v less the overlap", pass, since, kept)
			}
		}
		cursors = append(cursors, fmt.Sprint(storeRow(t, id)["wholesale_cursor"]))
	}
	if cursors[0] == "" || cursors[1] == "" || cursors[2] != "" {
		t.Errorf("the cursor after each pass is %q; want set, set, and empty", cursors)
	}
}

// A PASS KEEPS THE WINDOW WIDTH IT LEARNED: a page that outlasts a read
// halves wholesale_window_seconds, and the next pass reads at that width.
func TestAPassKeepsTheWindowWidthItLearned(t *testing.T) {
	floor := time.Now().Add(-10 * 24 * time.Hour).UTC()
	i, id, generation := passStore(t, floor)
	_, db := statsEngine(t)
	timeout := driverStatementTimeout(t, db)
	failed := false
	i.engine = &hookEngine{inner: i.engine, before: func(query string) error {
		if strings.Contains(query, "storeStatisticsOrderBuyers") && !failed {
			failed = true
			return timeout
		}
		return nil
	}}
	st := enabledWith(buyerOne)
	if err := wholesalePass(walkCtx(t), i, id, generation, st, time.Now, maxPagesPerRun); err != nil {
		t.Fatal(err)
	}
	row := storeRow(t, id)
	if fmt.Sprint(row["wholesale_window_seconds"]) != "43200" || row["wholesale_cursor"] != floor.Format(time.RFC3339Nano) {
		t.Fatalf("after a page outlasted its read the pass keeps width %v and cursor %v; want 43200 and the floor %s",
			row["wholesale_window_seconds"], row["wholesale_cursor"], floor.Format(time.RFC3339Nano))
	}
	engine := &pagingEngine{inner: i.engine}
	i.engine = engine
	if err := wholesalePass(walkCtx(t), i, id, generation, st, time.Now, maxPagesPerRun); err != nil {
		t.Fatal(err)
	}
	reads := buyersReads(engine)
	if len(reads) == 0 {
		t.Fatal("the next pass read no buyers")
	}
	until := quoteInstant(floor.Add(12 * time.Hour))
	if !strings.HasPrefix(reads[0], "asOf(") || !strings.Contains(reads[0], until) {
		t.Errorf("the next pass first read %s; want a past window as of the floor plus twelve hours, %s", reads[0], until)
	}
}

// A GRANTED SET THAT CHANGES MID-PASS STARTS THE PASS AGAIN: from the floor,
// under the new digest, and not from the cursor it had reached.
func TestAGrantedSetThatChangesMidPassStartsThePassAgain(t *testing.T) {
	floor := time.Now().Add(-2 * time.Hour).UTC()
	i, id, generation := passStore(t, floor)
	cursor := time.Now().Add(-time.Hour).UTC()
	exec(t, `UPDATE store_sales_store SET wholesale_enabled = true, wholesale_digest = ?, wholesale_cursor = ? WHERE store_id = ?`,
		grantedDigest(map[string]bool{buyerOne: true}), cursor.Format(time.RFC3339Nano), id)
	engine := &pagingEngine{inner: i.engine}
	i.engine = engine
	st := enabledWith(buyerOne, buyerTwo)
	if err := wholesalePass(walkCtx(t), i, id, generation, st, time.Now, maxPagesPerRun); err != nil {
		t.Fatal(err)
	}
	reads := buyersReads(engine)
	if len(reads) == 0 {
		t.Fatal("the pass read no buyers")
	}
	if since := sinceOf(t, reads[0]); !since.Equal(floor.Add(-watermarkOverlap)) {
		t.Errorf("the pass under a new granted set read from %v, want the floor %v less the overlap: it went on from its cursor", since, floor)
	}
	row := storeRow(t, id)
	if row["wholesale_digest"] != st.Digest || row["wholesale_cursor"] != "" {
		t.Errorf("after the pass the digest is %v and the cursor %q; want the new set's digest and the pass ended", row["wholesale_digest"], row["wholesale_cursor"])
	}
}

// A PASS PAGE READ BEFORE A REBUILD WRITES NOTHING: the store's generation is
// raised between the page's read and its write, and no flag moves.
func TestAPassPageReadBeforeARebuildWritesNothing(t *testing.T) {
	i, id := wholesaleStore(t)
	_, db := statsEngine(t)
	settingsFor(t, id, id)
	olderOrder(t, id, orderOne, buyerOne)
	runOK(t, i)
	if got := wholesaleOfFact(t, id, orderOne); got != "false" {
		t.Fatalf("before any grant the order is wholesale %s, want false", got)
	}
	generation, err := generationOf(context.Background(), db, id)
	if err != nil {
		t.Fatal(err)
	}
	i.engine = &hookEngine{inner: i.engine, after: func(query string) {
		if strings.Contains(query, "storeStatisticsOrderBuyers") {
			exec(t, `UPDATE store_sales_store SET generation = nextval('store_sales_generation') WHERE store_id = ?`, id)
		}
	}}
	err = wholesalePass(walkCtx(t), i, id, generation, enabledWith(buyerOne), time.Now, maxPagesPerRun)
	if !errors.Is(err, errRebuilt) {
		t.Fatalf("a pass whose page was read before a rebuild answered %v, want errRebuilt", err)
	}
	if got := wholesaleOfFact(t, id, orderOne); got != "false" {
		t.Errorf("the page read before the rebuild moved the order's flag to %s", got)
	}
	if c := storeRow(t, id)["wholesale_cursor"]; c == "" {
		t.Errorf("the pass's cursor is %q: the page read before the rebuild moved it, or the pass never started", c)
	}
}

// NO EMAIL REACHES A TABLE OR A LOG. After a run with a granted buyer and an
// unreadable row whose email is an address too, no text or JSON column of any
// store_sales_ table, each cast to text, holds an "@", and no line the walk's
// logger wrote does.
func TestNoEmailReachesATableOrALog(t *testing.T) {
	i, id := wholesaleStore(t)
	logs := captureLogs(i)
	_, db := statsEngine(t)
	settingsFor(t, id, id)
	grant(t, id, id, "one", buyerOne, "granted")
	olderOrder(t, id, orderOne, buyerOne)
	mirrorOrder(t, id, orderTwo, "email", buyerTwo, "currencyCode", nil)
	rep, err := run(t, i)
	if err != nil {
		t.Fatal(err)
	}
	if got := wholesaleOfFact(t, id, orderOne); got != "true" || rep.Unreadable == 0 {
		t.Fatalf("the granted buyer's order is wholesale %s and the run met %d unreadable rows; want true and one, or nothing below measures a leak",
			got, rep.Unreadable)
	}
	rows, err := db.QueryContext(context.Background(), `SELECT table_name, column_name FROM information_schema.columns
		WHERE table_schema = current_schema() AND table_name LIKE 'store\_sales\_%' AND data_type IN ('text', 'character varying', 'character', 'json', 'jsonb', 'ARRAY')
		ORDER BY table_name, column_name`)
	if err != nil {
		t.Fatal(err)
	}
	type column struct{ table, name string }
	var columns []column
	for rows.Next() {
		var c column
		if err := rows.Scan(&c.table, &c.name); err != nil {
			t.Fatal(err)
		}
		columns = append(columns, c)
	}
	_ = rows.Close()
	// The report as the run's node carries it: counts and ids only.
	raw, err := json.Marshal(rep)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "@") {
		t.Errorf("the run's report holds an address: %s", raw)
	}
	tables := map[string]bool{}
	for _, c := range columns {
		tables[c.table] = true
		var n int
		if err := db.QueryRowContext(context.Background(),
			fmt.Sprintf(`SELECT count(*) FROM %s WHERE %s::text LIKE '%%@%%'`, c.table, c.name)).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n > 0 {
			t.Errorf("%s.%s holds an address in %d rows", c.table, c.name, n)
		}
	}
	for _, table := range []string{"store_sales_order_fact", "store_sales_store", "store_sales_unreadable"} {
		if !tables[table] {
			t.Errorf("no text column of %s was looked at", table)
		}
	}
	lines := logs()
	if len(lines) == 0 {
		t.Fatal("the run wrote no log line, so nothing above measures one")
	}
	for _, line := range lines {
		if strings.Contains(line, "@") {
			t.Errorf("a log line holds an address: %s", line)
		}
	}
}

// THE DIGEST OF NO BUYERS IS NOT THE DIGEST NEVER COMPUTED: a store's stored
// digest starts empty, and a store enabled with nobody granted has a digest
// of its own, so the pass that judges its orders retail still runs. The
// digest is of the folded set, in no order. Not a database test.
func TestTheDigestOfNoBuyersIsNotTheDigestNeverComputed(t *testing.T) {
	none := grantedDigest(nil)
	if none == "" || none != grantedDigest(map[string]bool{}) {
		t.Errorf("the digest of no buyers is %q, and of an empty set %q; want one digest, not empty", none, grantedDigest(map[string]bool{}))
	}
	one := grantedDigest(map[string]bool{buyerOne: true})
	if one == none || one != grantedDigest(map[string]bool{" Buyer-1@Example.TEST ": true}) {
		t.Errorf("the digest of one buyer is %q: it must differ from no buyers' and not depend on case or spaces", one)
	}
	if grantedDigest(map[string]bool{buyerOne: true, buyerTwo: true}) != grantedDigest(map[string]bool{buyerTwo: true, buyerOne: true}) {
		t.Error("the digest of two buyers depends on their order")
	}
	if len(none) != 64 {
		t.Errorf("the digest %q is not a hex SHA-256", none)
	}
}

// THE WHOLESALE READS ENDING INSIDE THEIR BUDGET ANSWER EVERY STORE: pages of
// one row, the budgets' clock moving half a second each reading, and every
// store answered -- one with settings and buyers under either form, one with
// settings under the other form and nobody granted, and one with neither.
func TestTheWholesaleReadsEndingInsideTheirBudgetAnswerEveryStore(t *testing.T) {
	i, a := wholesaleStore(t)
	b, c := keptStore(t), keptStore(t)
	forgetPackRows(t, b)
	forgetPackRows(t, c)
	settingsFor(t, a, a)
	settingsFor(t, b, canonicalStore(b))
	grant(t, a, a, "one", buyerOne, "granted")
	grant(t, a, canonicalStore(a), "two", " Buyer-2@Example.TEST", "granted")
	grant(t, c, c, "one", buyerOne, "granted")
	lower(t, &wholesalePageSize, 1)
	useClock(t, &budgetClock{at: time.Now(), step: 500 * time.Millisecond})
	engine := &pagingEngine{inner: i.engine}
	i.engine = engine
	states, err := i.wholesaleOf(walkContext(walkCtx(t)), []string{a, b, c})
	if err != nil {
		t.Fatal(err)
	}
	if engine.asked() < 4 {
		t.Fatalf("the reads took %d pages; with pages of one row they take several, or nothing here measures the budget asked between them", engine.asked())
	}
	for id, want := range map[string]wholesaleState{a: enabledWith(buyerOne, buyerTwo), b: enabledWith(), c: {}} {
		got := states[id]
		if got.Enabled != want.Enabled || got.Digest != want.Digest || len(got.Granted) != len(want.Granted) {
			t.Errorf("store %s: enabled %v with %d granted and digest %q; want %v with %d and %q",
				id, got.Enabled, len(got.Granted), got.Digest, want.Enabled, len(want.Granted), want.Digest)
		}
		for e := range want.Granted {
			if !got.Granted[e] {
				t.Errorf("store %s: a granted buyer is not in its set", id)
			}
		}
	}
}

// A WHOLESALE READ PAST ITS BUDGET SKIPS THE WALK AND KEEPS THE HOUSEKEEPING:
// the first read moves the budgets' clock past the reads' budget, the run
// answers an error that says so with no value of a row, no store is walked,
// and the store still has its row.
func TestAWholesaleReadPastItsBudgetSkipsTheWalkAndKeepsTheHousekeeping(t *testing.T) {
	i, id := wholesaleStore(t)
	settingsFor(t, id, id)
	grant(t, id, id, "one", buyerOne, "granted")
	clock := &budgetClock{at: time.Now()}
	useClock(t, clock)
	i.engine = &hookEngine{inner: i.engine, after: func(query string) {
		if strings.Contains(query, "wholesaleStatistics") {
			clock.advance(wholesaleReadBudget)
		}
	}}
	rec := &walkRecorder{}
	i.walkStore = rec.walk
	_, err := run(t, i)
	if err == nil || !strings.Contains(err.Error(), "budget") {
		t.Fatalf("a run whose wholesale reads outlasted their budget answered %v, want an error that says so", err)
	}
	if strings.Contains(err.Error(), "@") {
		t.Errorf("the error holds an address: %v", err)
	}
	if rec.count(id) != 0 {
		t.Errorf("the store was walked %d times with no wholesale state, want none", rec.count(id))
	}
	if storeRow(t, id) == nil {
		t.Error("the store has no row: its housekeeping did not run")
	}
}

// AN UNREADABLE CURSOR RESTARTS THE PASS FROM THE FLOOR. Only this package
// writes wholesale_cursor, but one that is not an instant must not stop the
// store's pass until its set changes: the pass starts again from the floor
// (a judgement is the same however often it is made), says so once at Warn
// naming the store and nothing of the cursor, and ends.
func TestAnUnreadableCursorRestartsThePassFromTheFloor(t *testing.T) {
	floor := time.Now().Add(-time.Hour).UTC()
	i, id, generation := passStore(t, floor)
	logs := captureLogs(i)
	st := enabledWith(buyerOne)
	exec(t, `UPDATE store_sales_store SET wholesale_enabled = true, wholesale_digest = ?, wholesale_cursor = 'not-an-instant' WHERE store_id = ?`, st.Digest, id)
	engine := &pagingEngine{inner: i.engine}
	i.engine = engine
	if err := wholesalePass(walkCtx(t), i, id, generation, st, time.Now, maxPagesPerRun); err != nil {
		t.Fatalf("a pass with an unreadable cursor answered %v, want it to heal", err)
	}
	reads := buyersReads(engine)
	if len(reads) == 0 {
		t.Fatal("the pass read no buyers")
	}
	if since := sinceOf(t, reads[0]); !since.Equal(floor.Add(-watermarkOverlap)) {
		t.Errorf("the pass read from %v, want the floor %v less the overlap", since, floor)
	}
	if c := storeRow(t, id)["wholesale_cursor"]; c != "" {
		t.Errorf("after the pass the cursor is %q, want the pass ended", c)
	}
	warned := 0
	for _, line := range logs() {
		if strings.Contains(line, "not-an-instant") {
			t.Errorf("a log line holds the cursor's text: %s", line)
		}
		if strings.Contains(line, `"level":"WARN"`) && strings.Contains(line, id) {
			warned++
		}
	}
	if warned != 1 {
		t.Errorf("the pass wrote %d Warn lines naming the store, want one", warned)
	}
}

// failingRead is the integration's engine, refusing every read whose
// statement names read, counting what it asked.
func failingRead(i *Integration, read string) (asked func(name string) int) {
	var calls []string
	i.engine = &hookEngine{inner: i.engine, before: func(query string) error {
		calls = append(calls, query)
		if strings.Contains(query, read) {
			return errors.New("the read " + read + " failed")
		}
		return nil
	}}
	return func(name string) int {
		n := 0
		for _, q := range calls {
			if strings.Contains(q, name) {
				n++
			}
		}
		return n
	}
}

// A WALK THAT FAILED ON THE FLOOR MAKES NO PASS: the pass would ask the
// floor's probes again and report the same failure twice.
func TestAWalkThatFailedOnTheFloorMakesNoPass(t *testing.T) {
	i, id := wholesaleStore(t)
	settingsFor(t, id, id)
	asked := failingRead(i, "storeStatisticsStoreAt")
	_, err := run(t, i)
	if err == nil {
		t.Fatal("a run whose floor could not be found answered no error")
	}
	if n := strings.Count(err.Error(), "the read storeStatisticsStoreAt failed"); n != 1 {
		t.Errorf("the floor's failure is reported %d times, want once: %v", n, err)
	}
	if n := asked("storeStatisticsStoreAt"); n != 1 {
		t.Errorf("the floor was probed %d times in the turn, want once", n)
	}
	if n := asked("storeStatisticsOrderBuyers"); n != 0 {
		t.Errorf("the pass read the buyers %d times after the walk failed on the floor, want none", n)
	}
}

// A WALK THAT FAILED OTHERWISE STILL MAKES ITS PASS: a source that fails does
// not hold back the wholesale split.
func TestAWalkThatFailedOtherwiseStillMakesItsPass(t *testing.T) {
	i, id := wholesaleStore(t)
	settingsFor(t, id, id)
	asked := failingRead(i, "storeStatisticsOrdersChanged")
	if _, err := run(t, i); err == nil {
		t.Fatal("a run whose order read failed answered no error")
	}
	if asked("storeStatisticsOrderBuyers") == 0 {
		t.Error("the pass did not run after the walk's order source failed")
	}
	if row := storeRow(t, id); row["wholesale_enabled"] != true || row["wholesale_cursor"] != "" {
		t.Errorf("after the run the store is enabled %v with cursor %q; want enabled and the pass ended", row["wholesale_enabled"], row["wholesale_cursor"])
	}
}
