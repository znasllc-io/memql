package pipelinehop

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/pgdialect"
	"github.com/uptrace/bun/driver/pgdriver"
	"google.golang.org/protobuf/types/known/structpb"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/automations"
	"github.com/znasllc-io/memql/component/automations/steps"
	"github.com/znasllc-io/memql/component/database/dbtest"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/events"
	"github.com/znasllc-io/memql/component/identity/githubconnect"
	"github.com/znasllc-io/memql/component/inbound"
	langparser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/node"
	"github.com/znasllc-io/memql/component/pipelinerun"
	"github.com/znasllc-io/memql/component/pipelines"
	"github.com/znasllc-io/memql/component/workjournal"
)

// harness_test.go -- two nodes over one Postgres, joined the way the mesh
// joins them.
//
// Node A plays the bff: the inbound receiver stages a GitHub delivery there,
// and the shipped trigger automation runs there. Its pipelines plug-in opens
// runs and does not drive them. Node B plays the agent: its plug-in is a
// DRIVER, subscribed to the run's events on its own bus, exactly as
// app/integrations_pipelines_agent.go subscribes it. The two share only what
// two replicas share: the database, the advisory lock on it, GitHub, and the
// events the routing rules let across.

const (
	// The two replicas' MEMQL_NODE_IDs, and their node types: the link
	// delivers a targeted rule only to a node of its type.
	nodeA, typeA = "node-a", "bff"
	nodeB, typeB = "node-b", "agent"

	// osOrigin is MemQL OS's origin on this cluster: a check run's details
	// link is a run page under it.
	osOrigin = "https://os.pipelinehop.test"

	// triggerAutomation is the shipped automation a staged delivery fires.
	triggerAutomation = "triggerPipelinesOnGitHubDelivery"

	// hookID is the X-GitHub-Hook-ID every delivery carries.
	hookID = "5491"

	// concludeWithin bounds the wait for a run's conclusion: a drive is a
	// few gated round trips and three steps, seconds at most; a minute is
	// a hang.
	concludeWithin = 60 * time.Second
)

// The topics the hop is about.
var (
	runCreated     = events.BuildTopicWithConcept(events.TopicGraphNodeCreated, pipelinerun.RunConcept)
	runUpdated     = events.BuildTopicWithConcept(events.TopicGraphNodeUpdated, pipelinerun.RunConcept)
	inboundConcept = "v1:platform:inboundRequest"
	stagedCreated  = events.BuildTopicWithConcept(events.TopicGraphNodeCreated, inboundConcept)
)

// githubSource is the inbound source a GitHub App's webhook arrives on: the
// last segment of githubconnect.WebhookPath, the way app/ derives it.
var githubSource = strings.TrimPrefix(githubconnect.WebhookPath, inbound.RoutePrefix)

// cluster is the two replicas, and everything the test reads them through.
type cluster struct {
	run    string // scopes every row and name this run of the test writes
	secret string // the webhook's HMAC secret

	direct *sql.DB // the gate's pool, and the test's own row counts
	gate   pipelinerun.Gate

	github *fakeGitHub
	runner *fakeExecutor
	a, b   *replica
	link   *meshLink

	receiver *inbound.Handler
	trigger  *automations.Automation
	fires    *automations.Executor

	staged *recorder // A's created events for staged deliveries
	heardB *recorder // every graph event B's bus carried

	logs *lockedBuffer
	seq  int
}

// replica is one node.
type replica struct {
	id, typ string
	eng     *memql.MemQLEngine
	bus     *events.Bus
	integ   *pipelinerun.Integration
	store   pipelinerun.Store
}

// newCluster boots the two replicas over the database at dsn, or skips when
// none is reachable there (MEMQL_REQUIRE_DB=1 makes that a failure).
func newCluster(t *testing.T, dsn string) *cluster {
	t.Helper()
	direct := sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(dsn)))
	t.Cleanup(func() { _ = direct.Close() })
	ping, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := direct.PingContext(ping); err != nil {
		dbtest.Unreachable(t, "the pipelines hop across two engines (memql#5491)", dsn, err)
		return nil
	}
	if _, err := memql.LoadUnifiedConcepts(nil); err != nil {
		t.Fatalf("LoadUnifiedConcepts: %v", err)
	}

	c := &cluster{
		run:    strconv.FormatInt(time.Now().UnixNano(), 36),
		direct: direct,
		github: newFakeGitHub(),
		runner: &fakeExecutor{},
		staged: &recorder{},
		heardB: &recorder{},
		logs:   &lockedBuffer{},
	}
	c.secret = "hop-webhook-" + c.run
	// ONE gate for both replicas: the production one, a Postgres advisory
	// lock held on a session of the direct pool. Two replicas are
	// serialized by the database, not by anything this process shares.
	c.gate = func(ctx context.Context, key string, fn func(context.Context) error) error {
		return githubconnect.WithGate(ctx, direct, key, fn)
	}
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("the replicas' log, last lines:\n%s", c.logs.tail(120))
		}
		// Every row this run wrote names its run id somewhere.
		_, _ = direct.Exec(`DELETE FROM "MemoryNodes" WHERE id LIKE $1 OR payload::text LIKE $1`, "%"+c.run+"%")
	})

	// The runner, process-wide as the substrate registers it. Only B
	// drives, so only B ever calls it.
	previous := pipelines.RegisterExecutor(c.runner)
	t.Cleanup(func() { pipelines.RegisterExecutor(previous) })

	c.a = c.boot(t, nodeA, typeA, dsn)
	c.b = c.boot(t, nodeB, typeB, dsn)

	// B is the agent: a driver, subscribed to the run's events on its own
	// bus -- created AND updated, as app/integrations_pipelines_agent.go
	// subscribes it.
	c.b.integ.EnableDriver()
	// ...without the poll's recovery hook. Recovery claims EVERY unfinished
	// run in the database that nobody is driving, and in CI this package
	// shares that database with component/pipelinerun, whose own tests
	// strand a run on purpose to watch another replica take it over.
	// Recovery is that package's to prove; here the only way B may learn of
	// a run is its event.
	c.b.integ.Configure(func(d *pipelinerun.Deps) { d.Recover = nil })
	for _, topic := range []string{runCreated, runUpdated} {
		c.b.bus.Subscribe(topic, c.b.integ.HandleRunEvent, events.WithSubscriberName("pipelines:run-driver"))
	}
	c.b.bus.Subscribe("graph.node.#", c.heardB.record, events.WithSubscriberName("test:heard-on-b"))
	c.a.bus.Subscribe(stagedCreated, c.staged.record, events.WithSubscriberName("test:staged-on-a"))

	c.link = newMeshLink(t, c.a, c.b)

	// A is the bff: the inbound receiver, with GitHub's webhook policy for
	// a GitHub App the cluster registered (app/transport_inbound.go's
	// githubWebhookPolicy), and the shipped trigger automation.
	c.receiver = inbound.NewHandler(inbound.Config{Enabled: true, MaxBodyBytes: 256 << 10}, engineAdapter{c.a.eng}, c.logger("inbound"))
	c.receiver.SetRegisteredSource(func(_ context.Context, name string) (inbound.SourceConfig, bool) {
		if name != githubSource {
			return inbound.SourceConfig{}, false
		}
		return inbound.SourceConfig{
			Secret:          c.secret,
			Scheme:          inbound.SchemeHMACSHA256Hex,
			SignatureHeader: "X-Hub-Signature-256",
			SignaturePrefix: "sha256=",
			DedupeHeader:    "X-GitHub-Delivery",
			ForwardHeaders:  []string{"X-GitHub-Event", "X-GitHub-Delivery", "X-GitHub-Hook-ID"},
		}, true
	})
	auto, err := automations.NewLoader(automations.LoaderOptions{Logger: c.logger("automations")}).LoadByName(triggerAutomation)
	if err != nil {
		t.Fatalf("load %s: %v", triggerAutomation, err)
	}
	if auto.Trigger == nil || auto.Trigger.FilterLambda == nil {
		if err := automations.PrepareExpressions(auto); err != nil {
			t.Fatalf("prepare %s: %v", triggerAutomation, err)
		}
	}
	if auto.Trigger == nil || auto.Trigger.FilterLambda == nil {
		t.Fatalf("%s carries no @filter: every status write and every redelivery of a staged row would fire it", triggerAutomation)
	}
	if !events.Match(auto.Trigger.Event, stagedCreated) {
		t.Fatalf("%s triggers on %q, which a staged delivery (%s) does not publish", triggerAutomation, auto.Trigger.Event, stagedCreated)
	}
	c.trigger = auto
	c.fires = automations.NewExecutor(automations.ExecutorOptions{Engine: c.a.eng, Logger: c.logger("automations"), StepRegistry: steps.NewRegistry()})
	t.Cleanup(c.fires.Close)
	return c
}

// boot is one replica: its own engine over its own pool, its own bus, and
// the pipelines plug-in built by its own factory and wired as app/'s
// wirePipelines wires it on every node.
func (c *cluster) boot(t *testing.T, id, typ, dsn string) *replica {
	t.Helper()
	raw := sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(dsn)))
	t.Cleanup(func() { _ = raw.Close() })
	db := bun.NewDB(raw, pgdialect.New())
	eng, err := memql.New(db)
	if err != nil {
		t.Fatalf("%s: memql.New: %v", id, err)
	}
	eng.Logger = slog.New(slog.NewTextHandler(c.logs, &slog.HandlerOptions{Level: slog.LevelError})).With("node", id)
	if err := eng.Init(memorynodes.DefaultRegistry()); err != nil {
		t.Fatalf("%s: engine Init: %v", id, err)
	}
	bus := events.NewBus(events.WithLogger(slog.New(slog.NewTextHandler(io.Discard, nil))))
	t.Cleanup(bus.Close)
	eng.SetEventBus(bus)
	// The result cache's evictor, as the engine's own start runs it: a write
	// on the other replica evicts here when its cache.invalidate event
	// crosses the link.
	life, stop := context.WithCancel(context.Background())
	t.Cleanup(stop)
	eng.StartCacheInvalidationSubscriber(life)

	logger := c.logger("pipelines").With("node", id)
	for _, p := range memql.RegisteredPlugins() {
		if p.Name != pipelinerun.IntegrationName {
			continue
		}
		if err := p.ValidateContract(); err != nil {
			t.Fatalf("%s: %v", id, err)
		}
		prov, err := p.Factory(memql.PluginContext{
			Logger:              logger,
			Engine:              eng,
			BunDB:               func() *bun.DB { return db },
			DirectBunDB:         func() *bun.DB { return db },
			ResolveSystemSecret: eng.ResolveSystemSecret,
		})
		if err != nil || prov == nil {
			t.Fatalf("%s: the %s plug-in's factory: %v", id, p.Name, err)
		}
		if err := eng.RegisterIntegration(prov); err != nil {
			t.Fatalf("%s: register the %s plug-in: %v", id, p.Name, err)
		}
	}
	integ, ok := eng.IntegrationByName(pipelinerun.IntegrationName).(*pipelinerun.Integration)
	if !ok {
		t.Fatalf("%s: the %q plug-in did not materialize (is component/pipelinerun linked into this binary?)", id, pipelinerun.IntegrationName)
	}
	integ.Configure(func(d *pipelinerun.Deps) {
		d.GitHub = c.github.as(id)
		d.Gate = c.gate
		d.NodeID = id
		d.OSOrigin = func() string { return osOrigin }
		d.Journal = workjournal.New(workjournal.ExecutorFunc(func(ctx context.Context, q string) (any, error) {
			return eng.Execute(ctx, q)
		}), logger, id)
	})
	return &replica{id: id, typ: typ, eng: eng, bus: bus, integ: integ, store: pipelinerun.NewDSLStore(eng)}
}

func (c *cluster) logger(component string) *slog.Logger {
	return slog.New(slog.NewTextHandler(c.logs, &slog.HandlerOptions{Level: slog.LevelInfo})).With("component", component)
}

// ---------------------------------------------------------------------------
// A source and its pipeline
// ---------------------------------------------------------------------------

// source is one repository with its pipeline connected: a package owned by a
// person, a GitHub repository the fake serves, and the pipeline row.
type source struct {
	label        string
	name         string // owner/name, lower-cased
	owner        string
	installation int64
	base         string // the default branch's head when it was connected
	pipeline     pipelinerun.Pipeline
}

// connect creates a person's source and connects its pipeline through the
// plug-in's own Connect, on the bff, as the person: the real path, which
// proves the grant reaches the repository and reads the manifest before it
// writes the row.
func (c *cluster) connect(t *testing.T, label, delivery string) *source {
	t.Helper()
	c.seq++
	scope := c.run + "-" + label
	s := &source{
		label:        label,
		name:         "hop-" + scope + "/shop",
		owner:        "hop-owner-" + scope,
		installation: int64(54910000 + c.seq),
		base:         commitFor(scope, "base"),
	}
	account, pkg, credential := "hop-account-"+scope, "hop-pkg-"+scope, "hop-cred-"+scope
	c.github.add(s.name, &fakeRepo{
		owner: s.owner, credential: credential, installation: s.installation, defaultBranch: "main",
		heads:   map[string]string{"main": s.base},
		tags:    map[string]string{},
		commits: map[string]bool{s.base: true},
		changed: []string{"main.go"},
		files:   tinyRepository(),
	})
	if _, err := c.a.eng.Execute(seedCtx(), fmt.Sprintf(`mutation createClientAccount(accountId: %s, name: %s)`,
		langparser.QuoteString(account), langparser.QuoteString("Pipelines hop "+label))); err != nil {
		t.Fatalf("createClientAccount: %v", err)
	}
	if _, err := c.a.eng.Execute(signedIn(s.owner), fmt.Sprintf(
		`mutation createPackage(accountId: %s, packageId: %s, name: "shop", sourceKind: "repo", repoUrl: %s, credentialId: %s)`,
		langparser.QuoteString(account), langparser.QuoteString(pkg),
		langparser.QuoteString("https://github.com/"+s.name), langparser.QuoteString(credential))); err != nil {
		t.Fatalf("createPackage: %v", err)
	}
	res, err := c.a.integ.Connect(signedIn(s.owner), pipelinerun.ConnectRequest{PackageID: pkg, Delivery: delivery})
	if err != nil {
		t.Fatalf("connect %s: %v", s.name, err)
	}
	if res.Repository != s.name || !slices.Equal(res.Stages, testStages) {
		t.Fatalf("connect answered %+v; want %s with stages %v", res, s.name, testStages)
	}
	p, err := c.b.store.PipelineByID(fresh(), res.PipelineID)
	if err != nil || p == nil {
		t.Fatalf("the connected pipeline %s, read on the agent: %+v %v", res.PipelineID, p, err)
	}
	if p.InstallationID != s.installation || p.Delivery != delivery || !p.Active() {
		t.Fatalf("the pipeline row = %+v; want installation %d, delivery %s, active", p, s.installation, delivery)
	}
	s.pipeline = *p
	return s
}

// commit is a new commit of the source the fake serves a tarball for.
func (c *cluster) commit(s *source, name string) string {
	sha := commitFor(c.run, s.label, name)
	c.github.update(s.name, func(r *fakeRepo) { r.commits[sha] = true })
	return sha
}

// ---------------------------------------------------------------------------
// GitHub deliveries, through A's real receiver
// ---------------------------------------------------------------------------

func (s *source) envelope() map[string]any {
	return map[string]any{
		"repository":   map[string]any{"full_name": s.name, "default_branch": "main"},
		"installation": map[string]any{"id": s.installation},
		"sender":       map[string]any{"login": "octocat"},
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// pullRequest is a pull_request delivery whose head lives in headRepository.
func (s *source) pullRequest(t *testing.T, action string, number int, head, headRepository string) []byte {
	body := s.envelope()
	body["action"] = action
	body["number"] = number
	body["pull_request"] = map[string]any{
		"number": number,
		"title":  fmt.Sprintf("Change %d", number),
		"head":   map[string]any{"ref": fmt.Sprintf("change-%d", number), "sha": head, "repo": map[string]any{"full_name": headRepository}},
		"base":   map[string]any{"ref": "main", "sha": s.base, "repo": map[string]any{"full_name": s.name}},
	}
	return mustJSON(t, body)
}

func (s *source) mergeGroup(t *testing.T, head string) []byte {
	body := s.envelope()
	body["action"] = "checks_requested"
	body["merge_group"] = map[string]any{
		"head_sha": head, "head_ref": "refs/heads/gh-readonly-queue/main/pr-31", "base_sha": s.base,
		"head_commit": map[string]any{"message": "Merge pull request #31"},
	}
	return mustJSON(t, body)
}

func (s *source) push(t *testing.T, before, after string) []byte {
	body := s.envelope()
	body["ref"] = "refs/heads/main"
	body["before"] = before
	body["after"] = after
	body["head_commit"] = map[string]any{"message": "Land the change\n\nLonger body."}
	return mustJSON(t, body)
}

func (s *source) release(t *testing.T, tag string) []byte {
	body := s.envelope()
	body["action"] = "published"
	body["release"] = map[string]any{"tag_name": tag, "name": "Shop " + tag}
	return mustJSON(t, body)
}

func (s *source) checkRunRerequested(t *testing.T, checkRunID int64, head string) []byte {
	body := s.envelope()
	body["action"] = "rerequested"
	body["check_run"] = map[string]any{"id": checkRunID, "head_sha": head}
	return mustJSON(t, body)
}

// deliver posts one GitHub delivery to A's inbound receiver, signed the way
// GitHub signs it, and answers the staged row's id.
func (c *cluster) deliver(t *testing.T, event, deliveryID string, body []byte) string {
	t.Helper()
	mac := hmac.New(sha256.New, []byte(c.secret))
	_, _ = mac.Write(body)
	req := httptest.NewRequest(http.MethodPost, githubconnect.WebhookPath, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Hub-Signature-256", "sha256="+hex.EncodeToString(mac.Sum(nil)))
	req.Header.Set("X-GitHub-Event", event)
	req.Header.Set("X-GitHub-Delivery", deliveryID)
	req.Header.Set("X-GitHub-Hook-ID", hookID)
	rec := httptest.NewRecorder()
	c.receiver.ServeHTTP(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("the receiver answered %s delivery %s with %d %s; want 202", event, deliveryID, rec.Code, rec.Body)
	}
	var receipt struct{ ID, Status string }
	if err := json.Unmarshal(rec.Body.Bytes(), &receipt); err != nil || receipt.ID == "" || receipt.Status != "received" {
		t.Fatalf("the receiver's receipt %q: %v", rec.Body, err)
	}
	return receipt.ID
}

// stagedEvents waits for n graph.node.created events for the staged row on
// A's bus -- the events a production scheduler fires the trigger from.
func (c *cluster) stagedEvents(t *testing.T, stagedID string, n int) []events.Event {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for {
		got := c.staged.matching(func(ev events.Event) bool { return bareOf(ev.Payload["id"]) == stagedID })
		if len(got) >= n {
			return got
		}
		if time.Now().After(deadline) {
			t.Fatalf("A's bus carried %d created event(s) for staged delivery %s within 15s; want %d", len(got), stagedID, n)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// admits is the shipped automation's @filter, evaluated over the event the
// way the scheduler evaluates it: the lambda's parameter is the triggering
// row (automations.TriggerRow). ExecuteWithEvent does not evaluate it, so
// fire asks it first.
func (c *cluster) admits(ev events.Event) (bool, error) {
	lam := c.trigger.Trigger.FilterLambda
	return memql.EvalCondition(context.Background(), lam.Body,
		memql.MapScope{lam.Params[0]: automations.TriggerRow(&ev), "args": ev.Payload}, memql.EvalOptions{})
}

// fire runs the shipped trigger automation on A for one staged event, as
// A's scheduler would: the filter first, then the automation through the
// real executor -- a tree-loaded automation, so its builtin step runs with
// the internal origin it has in production. It answers what the trigger
// builtin answered.
func (c *cluster) fire(t *testing.T, ev events.Event) map[string]any {
	t.Helper()
	ok, err := c.admits(ev)
	if err != nil || !ok {
		t.Fatalf("%s's @filter refuses the row the receiver staged (%v): %v", triggerAutomation, err, ev.Payload["payload"])
	}
	exec, err := c.fires.ExecuteWithEvent(context.Background(), c.trigger, "event:"+ev.Topic, &ev)
	if err != nil || exec == nil || exec.Status != "completed" {
		var detail any
		if exec != nil {
			detail = exec.Error
			for name, step := range exec.Steps {
				detail = fmt.Sprintf("%v; step %s: %s %s %v", detail, name, step.Status, step.Error, step.Result)
			}
		}
		t.Fatalf("%s did not complete on A: %v (%v)", triggerAutomation, err, detail)
	}
	// The automation is one statement: the trigger builtin.
	if len(exec.Steps) != 1 {
		t.Fatalf("%s ran %d steps; the trigger is its one statement", triggerAutomation, len(exec.Steps))
	}
	for _, step := range exec.Steps {
		if rows := memql.MaterializeRows(step.Result); len(rows) == 1 {
			return rows[0]
		}
		t.Fatalf("the trigger builtin answered %T %+v; want one answer", step.Result, step.Result)
	}
	return nil
}

// answered is the run ids a trigger answer lists under key ("opened" or
// "existing").
func answered(t *testing.T, answer map[string]any, key string) []string {
	t.Helper()
	raw, ok := answer[key]
	if !ok {
		t.Fatalf("the trigger's answer %v carries no %q", answer, key)
	}
	var out []string
	switch ids := raw.(type) {
	case []string:
		out = append(out, ids...)
	case []any:
		for _, id := range ids {
			s, _ := id.(string)
			out = append(out, memql.BareShortId(s))
		}
	default:
		t.Fatalf("the trigger's answer %q is %T", key, raw)
	}
	return out
}

// deliverAndFire is one delivery end to end on the bff: received, staged,
// and the trigger fired from the staged row's created event.
func (c *cluster) deliverAndFire(t *testing.T, event, deliveryID string, body []byte) map[string]any {
	t.Helper()
	staged := c.deliver(t, event, deliveryID, body)
	return c.fire(t, c.stagedEvents(t, staged, 1)[0])
}

// poll runs the every-minute poll on B, the agent its cron lease places it
// on, under the context the scheduled automation's builtin step carries: a
// system actor named for the automation, with internal origin.
func (c *cluster) poll(t *testing.T) pipelinerun.PollResult {
	t.Helper()
	ctx := auth.ContextWithInternalOrigin(auth.ContextWithSystemActor(context.Background(), "pollPipelines"))
	res, err := c.b.integ.Poll(ctx)
	if err != nil {
		t.Fatalf("poll: %v", err)
	}
	return res
}

// ---------------------------------------------------------------------------
// Reading the rows
// ---------------------------------------------------------------------------

func fresh() context.Context { return memql.ContextWithFreshRead(context.Background()) }

// onlyRun is the one run of key: one row id in the table -- counted in SQL,
// so no query shape or page can hide a second -- and that row as the
// server-only read answers it.
func (c *cluster) onlyRun(t *testing.T, key string) pipelinerun.Run {
	t.Helper()
	if n := c.runRows(t, key); n != 1 {
		t.Fatalf("run key %s has %d run rows; want exactly one", key, n)
	}
	runs, err := c.b.store.RunsForKey(fresh(), key)
	if err != nil || len(runs) != 1 {
		t.Fatalf("pipelineRunsForKey(%s) = %d runs, %v; want one", key, len(runs), err)
	}
	return runs[0]
}

// runRows counts the distinct run rows of a run key.
func (c *cluster) runRows(t *testing.T, key string) int {
	t.Helper()
	var n int
	if err := c.direct.QueryRow(`SELECT count(DISTINCT id) FROM "MemoryNodes" WHERE concept = $1 AND payload->>'runKey' = $2`,
		pipelinerun.RunConcept, key).Scan(&n); err != nil {
		t.Fatalf("counting the runs of %s: %v", key, err)
	}
	return n
}

// awaitConcluded waits, with a fresh read on the agent, for a run to
// complete, and answers it.
func (c *cluster) awaitConcluded(t *testing.T, s *source, runID string) pipelinerun.Run {
	t.Helper()
	deadline := time.Now().Add(concludeWithin)
	for {
		r, err := c.b.store.RunByID(fresh(), runID)
		if err == nil && r != nil && r.Finished() {
			return *r
		}
		if time.Now().After(deadline) {
			state := "unreadable"
			if r != nil {
				state = fmt.Sprintf("%s/%s, driver %q, check run %d %s", r.Status, r.Conclusion, r.DriverNodeID, r.CheckRunID, r.CheckRunState)
			}
			t.Fatalf("run %s did not conclude within %s: %s (read error %v)\n%s", runID, concludeWithin, state, err, c.diagnose(s, runID))
		}
		time.Sleep(25 * time.Millisecond)
	}
}

// diagnose is what a hung hop looks like from outside: did the run's event
// cross, did B hear it, what did GitHub and the runner see.
func (c *cluster) diagnose(s *source, runID string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "  forwarded A->B: %s=%d %s=%d\n", runCreated, c.link.forwardedFrom(nodeA, runCreated),
		runUpdated, c.link.forwardedFrom(nodeA, runUpdated))
	heard := c.heardB.matching(func(ev events.Event) bool { return bareOf(ev.Payload["id"]) == memql.BareShortId(runID) })
	fmt.Fprintf(&b, "  B heard %d event(s) for the run:", len(heard))
	for _, ev := range heard {
		fmt.Fprintf(&b, " [%s from %q]", ev.Topic, ev.OriginNodeId)
	}
	fmt.Fprintf(&b, "\n  GitHub check-run writes for %s:", s.name)
	for _, w := range c.github.writesFor(s.name) {
		fmt.Fprintf(&b, " [%s create=%v %d %s/%s]", w.Node, w.Create, w.ID, w.Run.Status, w.Run.Conclusion)
	}
	fmt.Fprintf(&b, "\n  runner requests for the run: %d\n", len(c.runner.forRun(runID)))
	return b.String()
}

// runVersion is one stored version of a run row.
type runVersion struct {
	Status, Conclusion, Driver string
}

// versions is every stored version of a run row, oldest first: the table is
// append-only, so the row's whole history -- who opened it, who claimed it,
// whether anyone else ever held it -- is in it.
func (c *cluster) versions(t *testing.T, runID string) []runVersion {
	t.Helper()
	rows, err := c.direct.Query(`SELECT COALESCE(payload->>'status', ''), COALESCE(payload->>'conclusion', ''), COALESCE(payload->>'driverNodeId', '')
		FROM "MemoryNodes" WHERE concept = $1 AND id = $2 ORDER BY "createdAt"`,
		pipelinerun.RunConcept, pipelinerun.RunConcept+":"+memql.BareShortId(runID))
	if err != nil {
		t.Fatalf("reading the versions of run %s: %v", runID, err)
	}
	defer rows.Close()
	var out []runVersion
	for rows.Next() {
		var v runVersion
		if err := rows.Scan(&v.Status, &v.Conclusion, &v.Driver); err != nil {
			t.Fatal(err)
		}
		out = append(out, v)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if len(out) == 0 {
		t.Fatalf("run %s has no stored version", runID)
	}
	return out
}

// workRunStatus is the work run's status, through the work spine's own
// server-only read.
func (c *cluster) workRunStatus(t *testing.T, workRunID string) map[string]any {
	t.Helper()
	ctx := auth.ContextWithInternalOrigin(auth.ContextWithSystemActor(fresh(), "pipelinehop"))
	res, err := c.b.eng.Execute(ctx, fmt.Sprintf(`query workRunById(runId: %s)`, langparser.QuoteString(memql.BareShortId(workRunID))))
	if err != nil {
		t.Fatalf("workRunById %s: %v", workRunID, err)
	}
	rows := memql.MaterializeRows(res)
	if len(rows) != 1 {
		t.Fatalf("workRunById %s answered %d rows", workRunID, len(rows))
	}
	return rows[0]
}

// ---------------------------------------------------------------------------
// The mesh
// ---------------------------------------------------------------------------

// meshLink joins the two buses the way component/node's EventBridge joins two
// nodes: every LOCAL event is offered to the routing rules
// (node.ForwardDecisionFor, the decision EventBridge.onLocalEvent makes over
// the same default rules), and only one they forward crosses -- to a peer of
// the rule's target type, or to every peer for a broadcast -- carrying its
// origin node id, so it is never forwarded back, and its payload
// round-tripped through structpb, as the wire carries it. Like
// EventBridge.ReceiveForward, the arriving copy carries its cause and no
// metadata.
type meshLink struct {
	t      *testing.T
	mu     sync.Mutex
	closed bool
	unsubs []func()
	// forwarded and refused count, per "<from>|<topic>", the events that
	// crossed and the ones the rules kept local: "the rule kept it" and
	// "nobody published it" are different answers.
	forwarded map[string]int
	refused   map[string]int
}

func newMeshLink(t *testing.T, a, b *replica) *meshLink {
	l := &meshLink{t: t, forwarded: map[string]int{}, refused: map[string]int{}}
	l.bridge(a, b)
	l.bridge(b, a)
	t.Cleanup(l.close)
	return l
}

func (l *meshLink) bridge(from, to *replica) {
	unsub := from.bus.Subscribe("#", func(evt events.Event) {
		if evt.IsRemote() {
			return // a peer's event is never forwarded again as this node's own
		}
		forward, broadcast, target := node.ForwardDecisionFor(evt.Topic)
		crosses := forward && (broadcast || string(target) == to.typ)
		l.mu.Lock()
		defer l.mu.Unlock()
		if l.closed {
			return
		}
		key := from.id + "|" + evt.Topic
		if !crosses {
			l.refused[key]++
			return
		}
		// The mesh encodes a payload with structpb.NewStruct, which refuses
		// a Go value it does not know: a payload that cannot cross would
		// fail here instead of silently in production.
		encoded, err := structpb.NewStruct(evt.Payload)
		if err != nil {
			l.t.Errorf("%s's payload is not structpb-encodable, so it could never cross the mesh: %v", evt.Topic, err)
			return
		}
		l.forwarded[key]++
		to.bus.Publish(events.Event{
			Topic:        evt.Topic,
			Kind:         evt.Kind,
			Timestamp:    evt.Timestamp,
			Payload:      encoded.AsMap(),
			Metadata:     map[string]string{},
			OriginNodeId: from.id,
			Cause:        evt.Cause,
		})
	}, events.WithSubscriberName("test:meshLink:"+from.id))
	l.mu.Lock()
	l.unsubs = append(l.unsubs, unsub)
	l.mu.Unlock()
}

func (l *meshLink) close() {
	l.mu.Lock()
	l.closed = true
	unsubs := l.unsubs
	l.unsubs = nil
	l.mu.Unlock()
	for _, u := range unsubs {
		u()
	}
}

func (l *meshLink) forwardedFrom(from, topic string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.forwarded[from+"|"+topic]
}

func (l *meshLink) refusedFrom(from, topic string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.refused[from+"|"+topic]
}

// recorder keeps the events one subscription saw, in arrival order.
type recorder struct {
	mu     sync.Mutex
	events []events.Event
}

func (r *recorder) record(ev events.Event) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, ev)
}

func (r *recorder) matching(keep func(events.Event) bool) []events.Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []events.Event
	for _, ev := range r.events {
		if keep(ev) {
			out = append(out, ev)
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// Small things
// ---------------------------------------------------------------------------

type engineAdapter struct{ *memql.MemQLEngine }

func (e engineAdapter) Execute(ctx context.Context, q string) (any, error) {
	return e.MemQLEngine.Execute(ctx, q)
}

// seedCtx writes an organization the way a boot seed does: a synthetic owner
// with internal origin.
func seedCtx() context.Context {
	ctx := auth.ContextWithInternalOrigin(auth.ContextWithAccess(context.Background(), &auth.AccessContext{
		UserId: "pipelinehop-seeder", Role: auth.RoleOwner, Synthetic: true, Unranked: true,
	}))
	return auth.ContextWithToken(ctx, &auth.TokenInfo{Subject: "pipelinehop-seeder"})
}

// signedIn is a person as the stream interceptor presents one. A developer,
// because registering a source is Deployables' `sources` part.
func signedIn(userID string) context.Context {
	ctx := auth.ContextWithAccess(context.Background(), &auth.AccessContext{UserId: userID, Role: auth.RoleDeveloper})
	return auth.ContextWithToken(ctx, &auth.TokenInfo{Subject: userID})
}

// bareOf is an event's id field as a bare short id.
func bareOf(v any) string {
	s, _ := v.(string)
	return memql.BareShortId(s)
}

// lockedBuffer is a log sink many goroutines write to at once.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

// tail is the last n lines written.
func (b *lockedBuffer) tail(n int) string {
	b.mu.Lock()
	defer b.mu.Unlock()
	lines := strings.Split(strings.TrimRight(b.buf.String(), "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}
