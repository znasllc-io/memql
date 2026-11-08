package sitetraffic

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/core/statsperiod"
)

// The read's refusals and its payload, which need no database: the payload is
// the contract the OS parses (Task 15), and a refusal is decided before any
// table is touched.

func TestAStatisticsReadOfMoreThanTwoHundredSitesIsRefused(t *testing.T) {
	ids := make([]string, maxSitesPerRead+1)
	for i := range ids {
		ids[i] = fmt.Sprintf("site-%d", i)
	}
	_, err := NewReader(nil, nil).Statistics(context.Background(), StatisticsQuery{SiteIds: ids, Period: statsperiod.Last7Days})
	if err == nil || !strings.Contains(err.Error(), "200") {
		t.Errorf("a read of %d deployables answered %v, want a refusal naming the cap of 200 -- a short answer would read as no traffic", len(ids), err)
	}
}

func TestAStatisticsReadRefusesAPeriodItDoesNotOffer(t *testing.T) {
	for _, period := range []statsperiod.Period{"", "1y", "7D"} {
		_, err := NewReader(nil, nil).Statistics(context.Background(), StatisticsQuery{Period: period})
		if err == nil || !strings.Contains(err.Error(), "24h") || !strings.Contains(err.Error(), "12m") {
			t.Errorf("period %q answered %v, want a refusal naming the periods it offers", period, err)
		}
	}
}

func TestAStatisticsReadWithoutItsHandlesSaysWhatIsMissing(t *testing.T) {
	_, err := NewReader(nil, nil).Statistics(context.Background(), StatisticsQuery{Period: statsperiod.Last7Days})
	if err == nil || !strings.Contains(err.Error(), "database") {
		t.Errorf("no database handle answered %v, want a sentence naming it", err)
	}
}

func i64(v int64) *int64     { return &v }
func f64(v float64) *float64 { return &v }

// decode is the payload as JSON reads it back.
func decode(t *testing.T, s Statistics) map[string]any {
	t.Helper()
	raw, err := json.Marshal(s.Payload())
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return out
}

// wantKeys says every key is PRESENT, null or not: the client tells "not
// measured" from "missing" only if absence is spelled null.
func wantKeys(t *testing.T, where string, m map[string]any, keys ...string) {
	t.Helper()
	for _, k := range keys {
		if _, ok := m[k]; !ok {
			t.Errorf("%s has no %q key", where, k)
		}
	}
}

var (
	totalsKeys = []string{"visitors", "visits", "bounces", "pageViews", "requests", "errors", "notFound", "bytes",
		"botRequests", "viewsPerVisit", "bounceRate", "medianMs", "p95Ms"}
	pointKeys = []string{"at", "visitors", "visits", "bounces", "pageViews", "requests", "errors", "notFound", "p95Ms"}
)

// ABSENT IS SPELLED NULL, everywhere a figure stands: an unmeasured total, a
// bucket nothing measured, and the day counting began when it has not.
func TestThePayloadSpellsEveryAbsentFigureAsNull(t *testing.T) {
	at := time.Date(2037, 6, 9, 0, 0, 0, 0, time.UTC)
	b := statsperiod.For(statsperiod.Last7Days, time.Date(2037, 6, 15, 10, 0, 0, 0, time.UTC))
	s := Statistics{
		Period: statsperiod.Last7Days, Bucket: b.Bucket,
		Start: b.Start, End: b.End, PrevStart: b.PrevStart, PrevEnd: b.PrevEnd, HasPrevious: true,
		Totals:         Totals{Requests: i64(5), Errors: i64(0), P95Ms: f64(12.5)},
		Series:         []Point{{At: at, Requests: i64(0)}},
		PreviousSeries: []Point{{At: at.AddDate(0, 0, -7)}},
		Top:            map[string][]TopEntry{"page": {{Value: "/", Visitors: 1, Visits: 1, PageViews: 2}}},
		Sites:          []SiteTotals{{SiteId: "site-a", PageViews: i64(2)}},
	}
	p := decode(t, s)

	wantKeys(t, "the payload", p, "period", "bucket", "start", "end", "prevStart", "prevEnd", "hasPrevious",
		"measuredSince", "totals", "previous", "series", "previousSeries", "top", "sites")
	if p["start"] != "2037-06-09T00:00:00Z" || p["prevEnd"] != "2037-06-09T00:00:00Z" || p["measuredSince"] != nil {
		t.Errorf("start %v prevEnd %v measuredSince %v, want RFC3339 instants and a null measuredSince", p["start"], p["prevEnd"], p["measuredSince"])
	}
	totals, _ := p["totals"].(map[string]any)
	wantKeys(t, "totals", totals, totalsKeys...)
	if totals["visitors"] != nil || totals["requests"] != 5.0 || totals["errors"] != 0.0 || totals["p95Ms"] != 12.5 {
		t.Errorf("totals = %v, want visitors null, requests 5, errors 0 (a zero, not a null) and p95Ms 12.5", totals)
	}
	previous, _ := p["previous"].(map[string]any)
	wantKeys(t, "previous", previous, totalsKeys...)
	for _, k := range totalsKeys {
		if previous[k] != nil {
			t.Errorf("previous.%s = %v, want null: nothing was measured", k, previous[k])
		}
	}
	series, _ := p["series"].([]any)
	point, _ := series[0].(map[string]any)
	wantKeys(t, "series[0]", point, pointKeys...)
	if point["at"] != "2037-06-09T00:00:00Z" || point["requests"] != 0.0 || point["visitors"] != nil {
		t.Errorf("series[0] = %v, want at 2037-06-09T00:00:00Z, requests 0 and visitors null", point)
	}
	top, _ := p["top"].(map[string]any)
	wantKeys(t, "top", top, "page", "source", "country", "device", "browser", "os")
	for _, dim := range []string{"source", "country", "device", "browser", "os"} {
		if list, ok := top[dim].([]any); !ok || len(list) != 0 {
			t.Errorf("top.%s = %v, want an empty list rather than null", dim, top[dim])
		}
	}
	page, _ := top["page"].([]any)
	entry, _ := page[0].(map[string]any)
	wantKeys(t, "top.page[0]", entry, "value", "visitors", "visits", "pageViews")
	sites, _ := p["sites"].([]any)
	site, _ := sites[0].(map[string]any)
	wantKeys(t, "sites[0]", site, "siteId", "visitors", "visits", "pageViews")
	if site["siteId"] != "site-a" || site["visitors"] != nil || site["pageViews"] != 2.0 {
		t.Errorf("sites[0] = %v, want site-a with null visitors and 2 page views", site)
	}

	// With nothing in it at all, the lists are empty, not null.
	empty := decode(t, Statistics{Period: statsperiod.Last7Days, HasPrevious: true})
	if list, ok := empty["sites"].([]any); !ok || len(list) != 0 {
		t.Errorf("sites = %v, want []", empty["sites"])
	}
	if list, ok := empty["series"].([]any); !ok || len(list) != 0 {
		t.Errorf("series = %v, want []", empty["series"])
	}
}

// Twelve months carry no previous period, and the payload says so with nulls
// rather than with a comparison against nothing.
func TestTwelveMonthsCarriesNoPreviousInThePayload(t *testing.T) {
	b := statsperiod.For(statsperiod.Last12Months, time.Date(2038, 3, 10, 9, 0, 0, 0, time.UTC))
	p := decode(t, Statistics{
		Period: statsperiod.Last12Months, Bucket: b.Bucket, Start: b.Start, End: b.End,
		MeasuredSince: func() *time.Time { d := time.Date(2037, 5, 20, 0, 0, 0, 0, time.UTC); return &d }(),
		Previous:      Totals{Visitors: i64(9)}, // whatever a caller left here, it is not a comparison
	})
	for _, k := range []string{"previous", "previousSeries", "prevStart", "prevEnd"} {
		if v, ok := p[k]; !ok || v != nil {
			t.Errorf("%s = %v (present %v), want present and null", k, v, ok)
		}
	}
	if p["hasPrevious"] != false || p["measuredSince"] != "2037-05-20" || p["end"] != "2038-04-01T00:00:00Z" {
		t.Errorf("hasPrevious %v measuredSince %v end %v, want false, 2037-05-20 and 2038-04-01T00:00:00Z", p["hasPrevious"], p["measuredSince"], p["end"])
	}
}

// pagedSitesEngine answers sitesAll pages with no rows, recording the cursor
// each call carried; the walk's continuation is what it tests.
type pagedSitesEngine struct {
	pages   []pagedSitesAnswer
	cursors []string
	queries []string
}

type pagedSitesAnswer struct {
	more   bool
	cursor string
}

func (e *pagedSitesEngine) Execute(ctx context.Context, query string) (*memql.ExecuteResult, error) {
	e.cursors = append(e.cursors, memql.CursorFromContext(ctx))
	e.queries = append(e.queries, query)
	i := len(e.cursors) - 1
	if i >= len(e.pages) {
		return &memql.ExecuteResult{Meta: &memql.ResultMeta{}}, nil
	}
	return &memql.ExecuteResult{Meta: &memql.ResultMeta{HasMore: e.pages[i].more, Cursor: e.pages[i].cursor}}, nil
}

// The walk follows the engine's cursor to the end of the list.
func TestTheEveryDeployableWalkFollowsTheCursorToTheEnd(t *testing.T) {
	e := &pagedSitesEngine{pages: []pagedSitesAnswer{{true, "c1"}, {true, "c2"}, {false, ""}}}
	if _, err := NewReader(nil, e).readableSitesForAccounts(context.Background(), nil); err != nil {
		t.Fatalf("walk: %v", err)
	}
	if got := strings.Join(e.cursors, ","); got != ",c1,c2" {
		t.Errorf("cursors %q, want the first page and then each cursor in turn", got)
	}
	for _, q := range e.queries {
		if q != fmt.Sprintf("paginate(query sitesAll(), %d)", sitesPageSize) {
			t.Errorf("read %q, want sitesAll in pages of %d", q, sitesPageSize)
		}
	}
}

// A list the walk cannot finish is REFUSED, never read short: a short list
// would answer "these have no traffic" for the deployables it left out.
func TestTheEveryDeployableWalkRefusesAListItCannotFinish(t *testing.T) {
	endless := make([]pagedSitesAnswer, maxSitePages+5)
	for i := range endless {
		endless[i] = pagedSitesAnswer{true, fmt.Sprintf("c%d", i+1)}
	}
	for name, pages := range map[string][]pagedSitesAnswer{
		"more rows and no cursor":    {{true, ""}},
		"a cursor that stands still": {{true, "c1"}, {true, "c1"}},
		"more pages than the bound":  endless,
	} {
		e := &pagedSitesEngine{pages: pages}
		ids, err := NewReader(nil, e).readableSitesForAccounts(context.Background(), nil)
		if err == nil {
			t.Errorf("%s: answered %v, want a refusal", name, ids)
		}
		if len(e.cursors) > maxSitePages {
			t.Errorf("%s: %d reads, want at most %d", name, len(e.cursors), maxSitePages)
		}
	}
}
