package campaigns

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/pgdialect"
	"github.com/uptrace/bun/driver/pgdriver"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/automations"
	"github.com/znasllc-io/memql/component/automations/steps"
	"github.com/znasllc-io/memql/component/database/dbtest"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/events"
	langparser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/memql"
)

// feedback_event_path_db_test.go -- one staged bounce report through the
// SHIPPED automation, on a real engine. db-gated.
//
// ---- The event path feeds itself ----
//
// Every write of a row publishes graph.node.created, not only its first, and
// ingestCampaignFeedback fires on that event for a staged delivery. The
// ingester's own stamp of the row it just worked is such a write. So unless
// something stops it, the stamp fires the automation that made it and the same
// report is ingested again: counted into the sending ramp's reputation window
// again, and given another consent event. The tests in feedback_parse_test.go
// hold one call of the handler against a fake engine; they cannot hold the
// loop, which needs the engine publishing on a write and the scheduler firing
// on it. This runs that whole path.

// feedbackPathRig is a node's event path without the mesh: a real engine, the
// shipped automations on a real scheduler, and the campaigns worker with one
// source configured as a feedback feed.
//
// TWO BUSES, SO THE TEST CAN TELL WHEN THE PATH IS QUIET. The engine publishes
// to subscribers in goroutines of their own and says nothing when they finish,
// so on one bus "no more runs are coming" could only be guessed from a pause.
// Here the engine publishes on its own bus and one subscriber hands each event
// to the scheduler's bus and waits for every automation it fires to return
// (PublishSync). A changed write is one stored version and one event, so the
// path is quiet exactly when every version of a row has had its event handed
// over and finished with (settle).
type feedbackPathRig struct {
	t      *testing.T
	db     *bun.DB
	w      *Worker
	source string
	// stage writes one delivery as the receiver stages it and returns its row id.
	stage func(name, body string) string

	mu      sync.Mutex
	settled map[string]int
	started map[string]int
}

// secondApartEngine holds every reading of a staged delivery after the first
// until the wall clock's second has changed since the reading before it.
//
// The ingester stamps a row with the time it finished, to the second, and a
// stamp that repeats the last one's second is an unchanged write that
// publishes nothing: that is what ends most of these chains after two passes
// and hides the loop. A pass that takes a second -- a slow provider, a node
// mid-roll -- does not have that luck. The first reading is never held, so a
// path that works a delivery once does not wait at all.
type secondApartEngine struct {
	deliveryDBEngine
	mu   sync.Mutex
	last time.Time
}

func (e *secondApartEngine) Execute(ctx context.Context, q string) (any, error) {
	if strings.HasPrefix(q, "query inboundRequestById") {
		e.mu.Lock()
		if !e.last.IsZero() {
			for !time.Now().Truncate(time.Second).After(e.last) {
				time.Sleep(20 * time.Millisecond)
			}
		}
		e.last = time.Now().Truncate(time.Second)
		e.mu.Unlock()
	}
	return e.deliveryDBEngine.Execute(ctx, q)
}

func newFeedbackPathRig(t *testing.T) *feedbackPathRig {
	t.Helper()
	db := bun.NewDB(sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(dbtest.DSN()))), pgdialect.New())
	t.Cleanup(func() { _ = db.Close() })
	ping, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := db.PingContext(ping); err != nil {
		dbtest.Unreachable(t, "campaign feedback event path", dbtest.DSN(), err)
		return nil
	}
	if _, err := memql.LoadUnifiedConcepts(nil); err != nil {
		t.Fatalf("LoadUnifiedConcepts: %v", err)
	}
	eng, err := memql.New(db)
	if err != nil {
		t.Fatalf("memql.New: %v", err)
	}
	eng.Logger = quietLogger()
	if err := eng.Init(memorynodes.DefaultRegistry()); err != nil {
		t.Fatalf("engine Init: %v", err)
	}

	r := &feedbackPathRig{
		t: t, db: db,
		source:  fmt.Sprintf("dbtest-feedbackpath-%d", time.Now().UnixNano()),
		settled: map[string]int{}, started: map[string]int{},
	}
	r.w = newTestWorker(t, &secondApartEngine{deliveryDBEngine: deliveryDBEngine{eng}}, &recordingSender{})
	r.w.cfg.FeedbackSources = map[string]string{r.source: FormatRFC3464}
	if err := eng.RegisterIntegration(r.w); err != nil {
		t.Fatalf("register the campaigns worker: %v", err)
	}

	engineBus := events.NewBus(events.WithLogger(quietLogger()))
	schedulerBus := events.NewBus(events.WithLogger(quietLogger()))
	t.Cleanup(engineBus.Close)
	t.Cleanup(schedulerBus.Close)
	eng.SetEventBus(engineBus)
	schedulerBus.Subscribe(events.TopicAutomationStarted, func(ev events.Event) {
		name, _ := ev.Payload["automationName"].(string)
		r.mu.Lock()
		r.started[name]++
		r.mu.Unlock()
	})
	engineBus.Subscribe("#", func(ev events.Event) {
		schedulerBus.PublishSync(ev)
		if id, _ := ev.Payload["id"].(string); strings.HasPrefix(ev.Topic, events.TopicGraphNodeCreated) && id != "" {
			r.mu.Lock()
			r.settled[id]++
			r.mu.Unlock()
		}
	})

	registry := steps.NewRegistry()
	eng.SetLogicRunner(automations.NewLogicRunner(eng, registry, quietLogger()))
	scheduler, err := automations.NewScheduler(automations.SchedulerOptions{
		Logger:       quietLogger(),
		Loader:       automations.NewLoader(automations.LoaderOptions{Logger: quietLogger(), Registry: memorynodes.DefaultRegistry(), Functions: eng.Functions()}),
		Engine:       eng,
		EventBus:     schedulerBus,
		StepRegistry: registry,
		// No scheduled pass runs here: this is about what the event path does
		// on its own.
		LeaderGate: func() bool { return false },
	})
	if err != nil {
		t.Fatalf("NewScheduler: %v", err)
	}
	ctx, stop := context.WithCancel(context.Background())
	t.Cleanup(stop)
	go scheduler.Start(ctx)
	select {
	case <-scheduler.Ready():
	case <-time.After(30 * time.Second):
		t.Fatal("the scheduler did not become ready within 30s")
	}
	if scheduler.LookupAutomation("ingestCampaignFeedback") == nil {
		t.Fatal("ingestCampaignFeedback is not among the automations the scheduler loaded; this rig tests nothing without it")
	}

	t.Cleanup(func() {
		c := context.Background()
		like := "%" + r.source + "%"
		_, _ = db.ExecContext(c, `DELETE FROM "MemoryNodes" WHERE concept LIKE 'v1:work:%' AND payload->>'runId' IN
			(SELECT split_part(id, ':', 4) FROM "MemoryNodes" WHERE concept = 'v1:work:run' AND payload::text LIKE ?)`, like)
		_, _ = db.ExecContext(c, `DELETE FROM "MemoryNodes" WHERE concept LIKE 'v1:work:%' AND payload::text LIKE ?`, like)
		_, _ = db.ExecContext(c, `DELETE FROM "MemoryNodes" WHERE concept = 'v1:platform:inboundRequest' AND payload->>'source' = ?`, r.source)
	})

	stage := func(name, body string) string {
		t.Helper()
		id := r.source + "-" + name
		operator := auth.ContextWithInternalOrigin(auth.ContextWithSystemActor(context.Background(), "inbound"))
		if _, err := eng.Execute(operator, fmt.Sprintf(
			`mutation stageInboundRequest(requestId: %s, source: %s, medium: "webhook", body: %s, signatureVerified: true, receivedAt: "2026-10-05T12:00:00Z")`,
			langparser.QuoteString(id), langparser.QuoteString(r.source), langparser.QuoteString(body))); err != nil {
			t.Fatalf("staging %s: %v", name, err)
		}
		return "v1:platform:inboundRequest:" + id
	}
	r.stage = stage
	return r
}

// versions is how many versions of a row the database holds.
func (r *feedbackPathRig) versions(rowID string) int {
	r.t.Helper()
	var n int
	if err := r.db.QueryRowContext(context.Background(), `SELECT count(*) FROM "MemoryNodes" WHERE id = ?`, rowID).Scan(&n); err != nil {
		r.t.Fatalf("counting the versions of %s: %v", rowID, err)
	}
	return n
}

// settle waits until the event path has nothing left to do for a row -- every
// stored version's event handed to the automations and finished with -- and
// returns how many versions the row then has.
func (r *feedbackPathRig) settle(rowID string) int {
	r.t.Helper()
	deadline := time.Now().Add(2 * time.Minute)
	for {
		stored := r.versions(rowID)
		r.mu.Lock()
		done := r.settled[rowID]
		r.mu.Unlock()
		if stored > 0 && done == stored && r.versions(rowID) == stored {
			return stored
		}
		if time.Now().After(deadline) {
			r.t.Fatalf("the event path did not go quiet for %s: %d versions stored, the events of %d finished with", rowID, stored, done)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func (r *feedbackPathRig) field(rowID, name string) string {
	r.t.Helper()
	var v sql.NullString
	if err := r.db.QueryRowContext(context.Background(),
		`SELECT payload->>? FROM "MemoryNodes" WHERE id = ? ORDER BY "createdAt" DESC LIMIT 1`, name, rowID).Scan(&v); err != nil {
		r.t.Fatalf("reading %s of %s: %v", name, rowID, err)
	}
	return v.String
}

func (r *feedbackPathRig) runsStarted(automation string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.started[automation]
}

// A BOUNCE REPORT IS INGESTED ONCE. The stamp that records it as processed
// publishes the row again and fires the automation again; that fire must not
// ingest the report a second time.
func TestABounceReportIsIngestedOnceByTheEventPath(t *testing.T) {
	r := newFeedbackPathRig(t)
	if r == nil {
		return
	}
	address := fmt.Sprintf("dead-%d@example.test", time.Now().UnixNano())
	t.Cleanup(func() {
		_, _ = r.db.ExecContext(context.Background(),
			`DELETE FROM "MemoryNodes" WHERE concept LIKE 'v1:campaigns:%' AND payload->>'emailDigest' = ?`, EmailDigest(address))
	})
	row := r.stage("bounce", strings.ReplaceAll(hardBounceDSN, "dead@example.test", address))
	stored := r.settle(row)

	if got := countedHardBounces(r.w); got != 1 {
		t.Fatalf("one bounce report was counted into the reputation window %d times, want 1: "+
			"the stamp of the processed report fired the automation that ingested it again (%d versions of the row, %d runs of ingestCampaignFeedback)",
			got, stored, r.runsStarted("ingestCampaignFeedback"))
	}
	if status := r.field(row, "status"); status != "processed" {
		t.Fatalf("the report ended %q, want processed", status)
	}
	if stored != 2 {
		t.Fatalf("the row has %d versions, want 2: the staged report and the one stamp of its ingestion", stored)
	}
	// The trigger's half. Without its filter the handler's own guard still
	// keeps the report to one ingestion, and this is what tells the two apart.
	if got := r.runsStarted("ingestCampaignFeedback"); got != 1 {
		t.Fatalf("ingestCampaignFeedback ran %d times for one report, want 1: it fires for the staged row and not for the write that stamps it", got)
	}
}

// A REPORT THAT CANNOT BE READ IS FAILED ONCE. The stamp that records the
// failure is a write like any other.
func TestAnUnreadableReportIsFailedOnceByTheEventPath(t *testing.T) {
	r := newFeedbackPathRig(t)
	if r == nil {
		return
	}
	row := r.stage("unreadable", "this is not a delivery status notification")
	stored := r.settle(row)

	if status := r.field(row, "status"); status != "failed" {
		t.Fatalf("the report ended %q, want failed", status)
	}
	if stored != 2 {
		t.Fatalf("the row has %d versions, want 2: the staged report and the one stamp of its failure "+
			"(%d runs of ingestCampaignFeedback): every stamp of the failure fired the automation that failed it again",
			stored, r.runsStarted("ingestCampaignFeedback"))
	}
	if got := r.runsStarted("ingestCampaignFeedback"); got != 1 {
		t.Fatalf("ingestCampaignFeedback ran %d times for one report, want 1: it fires for the staged row and not for the write that stamps it", got)
	}
}
