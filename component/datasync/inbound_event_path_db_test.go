package datasync

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/automations"
	"github.com/znasllc-io/memql/component/automations/steps"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/events"
	"github.com/znasllc-io/memql/component/memql"
	memqlsync "github.com/znasllc-io/memql/component/memql/sync"
)

// inbound_event_path_db_test.go -- one staged delivery through the SHIPPED
// automations, on a real engine. db-gated, like inbound_retry_db_test.go.
//
// ---- The event path feeds itself ----
//
// Every write of a row publishes graph.node.created, not only its first, and
// dispatchInboundToConnector fires on that event for a staged delivery. The
// dispatcher's own stamp of a try is such a write. So unless something stops
// it, the stamp fires the automation that made it and the connector is handed
// the same delivery again by every stamp: on 2026-11-12 one delivery its
// connector could not apply was stamped failed once a second for eighteen
// seconds, until the automation loop bound refused the chain. Run against the
// engine of that day, the first test below counts sixteen calls of the
// connector for one delivery, and the second counts sixteen for a delivery
// that was applied.
//
// Two things stop it, and each test holds both: the trigger fires for a
// `received` row only (dsl/platform/automations.memql), so a stamp starts no
// run; and the builtin leaves a worked row alone (handleDispatchInbound), so a
// run that starts anyway reaches no connector.
//
// The tests in inbound_retry_test.go hold the builtin's own answer for a worked
// row against a fake engine. What they cannot hold is the loop itself: the
// engine publishing on a write, the scheduler firing on it, and the stamp that
// fire makes publishing again. These run that whole path.

// eventPathRig is a node's event path without the mesh: a real engine, the
// shipped automations on a real scheduler, and the sync runtime over one
// connector the test scripts.
//
// TWO BUSES, SO THE TEST CAN TELL WHEN THE PATH IS QUIET. The engine publishes
// to subscribers in goroutines of their own and says nothing when they finish,
// so on one bus "no more runs are coming" could only be guessed from a pause.
// Here the engine publishes on its own bus and one subscriber hands each event
// to the scheduler's bus and waits for every automation it fires to return
// (PublishSync). A changed write is one stored version and one event, so the
// path is quiet exactly when every version of a row has had its event handed
// over and finished with (settle): nothing is then running that could write
// the row again. No sleep stands in for an outcome.
type eventPathRig struct {
	t      *testing.T
	eng    *memql.MemQLEngine
	db     *sql.DB
	i      *Integration
	c      *fakeConnector
	source string

	// settled counts, per row id, the events every automation has finished with.
	mu      sync.Mutex
	settled map[string]int
	// started counts the runs each automation started.
	started map[string]int
	// ticks is the dispatcher's clock: one second later on every reading.
	ticks atomic.Int64
	epoch time.Time
}

func newEventPathRig(t *testing.T, applyErr error) *eventPathRig {
	t.Helper()
	eng, db := healthEngine(t)
	if eng == nil {
		return nil
	}
	quiet := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
	r := &eventPathRig{
		t: t, eng: eng, db: db,
		source:  fmt.Sprintf("dbtest-eventpath-%d", time.Now().UnixNano()),
		settled: map[string]int{}, started: map[string]int{},
		epoch: time.Now().UTC().Truncate(time.Second),
	}
	r.cleanup()

	// The sync runtime, over a connector that claims this run's source and
	// answers every Apply with applyErr.
	r.c = &fakeConnector{
		name:     r.source,
		domains:  []memqlsync.DomainSpec{{Concept: testMirrorConcept, Direction: memqlsync.DirectionInbound}},
		applyErr: applyErr,
	}
	r.i = NewIntegration(engineSeam{eng}, quiet)
	r.i.dispatcher.lookup = func(_ context.Context, name string) (memqlsync.Connector, bool) { return r.c, name == r.source }
	// EVERY READING OF THE CLOCK IS A SECOND LATER, so no two stamps of a row
	// share a processedAt. A stamp that repeats the last one's second is an
	// unchanged write and publishes nothing, which is what ended most of these
	// chains after two runs and hid the loop; a real dispatch takes longer than
	// a second whenever its connector does.
	r.i.dispatcher.now = func() time.Time { return r.epoch.Add(time.Duration(r.ticks.Add(1)) * time.Second) }
	if err := eng.RegisterIntegration(r.i); err != nil {
		t.Fatalf("register the sync runtime: %v", err)
	}

	engineBus := events.NewBus(events.WithLogger(quiet))
	schedulerBus := events.NewBus(events.WithLogger(quiet))
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
	eng.SetLogicRunner(automations.NewLogicRunner(eng, registry, quiet))
	scheduler, err := automations.NewScheduler(automations.SchedulerOptions{
		Logger:       quiet,
		Loader:       automations.NewLoader(automations.LoaderOptions{Logger: quiet, Registry: memorynodes.DefaultRegistry(), Functions: eng.Functions()}),
		Engine:       eng,
		EventBus:     schedulerBus,
		StepRegistry: registry,
		// No scheduled pass runs here: the retry pass is the one other thing
		// that dispatches a failed delivery, and these tests are about what the
		// event path does on its own.
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
	if scheduler.LookupAutomation("dispatchInboundToConnector") == nil {
		t.Fatal("dispatchInboundToConnector is not among the automations the scheduler loaded; this rig tests nothing without it")
	}
	return r
}

// cleanup removes what a run leaves: its staged deliveries, and the journal of
// every automation run they fired (a failed run is journaled).
func (r *eventPathRig) cleanup() {
	r.t.Cleanup(func() {
		ctx := context.Background()
		like := "%" + r.source + "%"
		_, _ = r.db.ExecContext(ctx, `DELETE FROM "MemoryNodes" WHERE concept LIKE 'v1:work:%' AND payload->>'runId' IN
			(SELECT split_part(id, ':', 4) FROM "MemoryNodes" WHERE concept = 'v1:work:run' AND payload::text LIKE $1)`, like)
		_, _ = r.db.ExecContext(ctx, `DELETE FROM "MemoryNodes" WHERE concept LIKE 'v1:work:%' AND payload::text LIKE $1`, like)
		_, _ = r.db.ExecContext(ctx, `DELETE FROM "MemoryNodes" WHERE concept = 'v1:platform:inboundRequest' AND payload->>'source' = $1`, r.source)
	})
}

// stage writes one delivery as the receiver stages it and returns its row id.
func (r *eventPathRig) stage(name string) string {
	r.t.Helper()
	id := r.source + "-" + name
	if _, err := r.i.store.exec(OperatorContext(context.Background()), call("mutation", "stageInboundRequest",
		arg{"requestId", id}, arg{"source", r.source}, arg{"medium", "webhook"},
		arg{"body", `{"topic":"orders/updated"}`},
		arg{"headersJson", `{"x-test-topic":"orders/updated"}`},
		arg{"signatureVerified", true}, arg{"verifiedBy", "connector"},
		arg{"receivedAt", r.epoch.Format(time.RFC3339)})); err != nil {
		r.t.Fatalf("staging %s: %v", name, err)
	}
	return inboundRequestConcept + ":" + id
}

// versions is how many versions of a row the database holds.
func (r *eventPathRig) versions(rowID string) int {
	r.t.Helper()
	var n int
	if err := r.db.QueryRowContext(context.Background(), `SELECT count(*) FROM "MemoryNodes" WHERE id = $1`, rowID).Scan(&n); err != nil {
		r.t.Fatalf("counting the versions of %s: %v", rowID, err)
	}
	return n
}

// settle waits until the event path has nothing left to do for a row -- every
// stored version's event handed to the automations and finished with -- and
// returns how many versions the row then has. See the rig's comment for why
// that is the whole of "quiet".
func (r *eventPathRig) settle(rowID string) int {
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

// field reads one payload field of a row's newest version.
func (r *eventPathRig) field(rowID, name string) string {
	r.t.Helper()
	var v sql.NullString
	if err := r.db.QueryRowContext(context.Background(),
		`SELECT payload->>$2 FROM "MemoryNodes" WHERE id = $1 ORDER BY "createdAt" DESC LIMIT 1`, rowID, name).Scan(&v); err != nil {
		r.t.Fatalf("reading %s of %s: %v", name, rowID, err)
	}
	return v.String
}

func (r *eventPathRig) applyCalls() int {
	r.c.mu.Lock()
	defer r.c.mu.Unlock()
	return r.c.applyCalls
}

func (r *eventPathRig) runsStarted(automation string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.started[automation]
}

// A DELIVERY ITS CONNECTOR CANNOT APPLY IS HANDED TO THE CONNECTOR ONCE. The
// stamp that records the failure publishes the row again and fires the
// automation again; that fire must not reach the connector. The next try is
// the retry pass's, a minute later.
func TestAFailedDeliveryIsHandedToItsConnectorOnceByTheEventPath(t *testing.T) {
	r := newEventPathRig(t, errors.New("delivery carried no object id"))
	if r == nil {
		return
	}
	row := r.stage("fails")
	stored := r.settle(row)

	if got := r.applyCalls(); got != 1 {
		t.Fatalf("the connector was handed one failed delivery %d times by the event path, want 1: "+
			"every stamp of the failure fired the automation that dispatched it again (%d versions of the row, %d runs of dispatchInboundToConnector)",
			got, stored, r.runsStarted("dispatchInboundToConnector"))
	}
	if status := r.field(row, "status"); status != "failed" {
		t.Fatalf("the delivery ended %q, want failed", status)
	}
	if stored != 2 {
		t.Fatalf("the row has %d versions, want 2: the staged delivery and the one stamp of its failed try", stored)
	}
	r.firedOnceEach(stored)
}

// firedOnceEach holds the trigger's half: the automations on a staged delivery
// each ran once for a row that now has `stored` versions, so no version but
// the staged one started a run. Without the trigger's filter the builtin's own
// guard still keeps the connector to one call, and this is what tells the two
// apart.
func (r *eventPathRig) firedOnceEach(stored int) {
	r.t.Helper()
	for _, automation := range []string{"dispatchInboundToConnector", "notePackageUpstreamFromWebhook", "ingestCampaignFeedback"} {
		if got := r.runsStarted(automation); got != 1 {
			r.t.Errorf("%s ran %d times for one delivery of %d versions, want 1: it fires for the staged row and not for the write that stamps it",
				automation, got, stored)
		}
	}
}

// A DELIVERY THAT WAS APPLIED IS HANDED TO THE CONNECTOR ONCE TOO. The stamp of
// a try that worked is a write like any other.
func TestAProcessedDeliveryIsHandedToItsConnectorOnceByTheEventPath(t *testing.T) {
	r := newEventPathRig(t, nil)
	if r == nil {
		return
	}
	row := r.stage("works")
	stored := r.settle(row)

	if got := r.applyCalls(); got != 1 {
		t.Fatalf("the connector was handed one delivery %d times by the event path, want 1 (%d versions of the row, %d runs of dispatchInboundToConnector)",
			got, stored, r.runsStarted("dispatchInboundToConnector"))
	}
	if status := r.field(row, "status"); status != "processed" {
		t.Fatalf("the delivery ended %q, want processed", status)
	}
	if stored != 2 {
		t.Fatalf("the row has %d versions, want 2: the staged delivery and the one stamp of the try that worked", stored)
	}
	r.firedOnceEach(stored)
}
