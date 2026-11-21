package calls

import (
	"context"
	"crypto/rand"
	"database/sql"
	"fmt"
	"io"
	"log/slog"
	"os"
	"time"

	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/secret"
)

// plugin.go -- self-registration, and the one piece of boot work.
//
// REGISTERED ON EVERY NODE TYPE, for integrations/siteaccess's reason exactly: the six
// builtins are declared in dsl/calls/builtins.memql, which EVERY binary loads, and a
// capability present in the DSL and absent from the registry is a boot-time resolution
// failure. Gating the registration by build tag would also make whether an operator can
// mint a key depend on which replica their stream landed on. The reconciliation pass is
// placed on bff replicas by its automation's trigger (node="bff"), and a media server's
// webhook is applied on the bff that received and staged it, which a delivery never
// leaves (events.go); neither is placed by this registration.
//
// It is an INTEGRATION in the module taxonomy (module_taxonomy_test.go): it talks to a
// media server's control API, somebody else's system, through LiveKit's client.
//
// # Boot
//
// A partial or unusable media configuration REFUSES BOOT here, in the factory, and so
// does one whose credentials cannot be sealed (MEMQL_MASTER_KEY missing or malformed):
// either way the node would run with a calls capability that fails at its first use.
// No configuration is not a refusal: it is a cluster without calls, and the operator
// builtins still register and refuse with a sentence when asked; a reconciliation pass
// there finds no media server and does nothing.
//
// The bootstrap itself does not run in the factory. A factory runs before the engine
// has started, and the bootstrap reads and writes rows through it, so it waits for the
// engine to say it is ready and then runs in the background, retried a few times
// (bootstrapAtBoot). Its failure is logged, not fatal: the node still serves everything
// else, and the next boot with the same configuration finishes the work.

// pluginName is the integration's name, and so the middle of every executor the DSL
// names: integration.calls.<capability>. The registration below spells it as a
// literal, which module_taxonomy_test.go requires so the kind is decided at review.
const pluginName = "calls"

func init() {
	memql.RegisterPlugin("calls", func(pctx memql.PluginContext) (memql.IntegrationProvider, error) {
		// The bootstrap and every policy change take write gates on the gate pool
		// (memql#6022) and on nothing else, so the plug-in refuses first.
		if pctx.GateDB == nil {
			return nil, fmt.Errorf("calls plug-in: no GateDB in plugin context: the media bootstrap and the calls limits take write gates on the gate pool (database.Database.GateDB; a test or a tool opens one with database.NewGatePool)")
		}
		if pctx.ResolveSystemSecret == nil {
			return nil, fmt.Errorf("calls plug-in: no ResolveSystemSecret in plugin context: the key pepper and the media credentials are sealed global secrets")
		}
		cfg, present, err := MediaConfigFromEnv(os.Getenv)
		if err != nil {
			return nil, err
		}
		integration := New(Deps{
			Engine:              engineAdapter{pctx.Engine},
			GateDB:              pctx.GateDB,
			ResolveSystemSecret: pctx.ResolveSystemSecret,
			Logger:              pctx.Logger,
		})
		if !present {
			return integration, nil
		}
		if _, _, err := secret.Encrypt("calls: can this node seal its media credentials?"); err != nil {
			return nil, fmt.Errorf("calls plug-in: the media server's credentials are sealed under %s, and this node cannot seal: %w", secret.EnvMasterKey, err)
		}
		go integration.bootstrapAtBoot(engineReady(pctx.Engine), cfg)
		return integration, nil
	})
}

// engineAdapter narrows IntegrationEngineAccess to the one method this package uses.
// Narrow deliberately: nothing here should reach the AI provider registry or the tool
// surface.
type engineAdapter struct{ engine memql.IntegrationEngineAccess }

func (a engineAdapter) Execute(ctx context.Context, query string) (any, error) {
	return a.engine.Execute(ctx, query)
}

// engineReady is the engine's own readiness signal, which it closes once it has
// started and can take writes; the seed materializer waits on the same channel. An
// engine that offers none (a test's) is ready now.
func engineReady(engine memql.IntegrationEngineAccess) <-chan struct{} {
	if ready, ok := engine.(interface{ Ready() <-chan struct{} }); ok {
		return ready.Ready()
	}
	now := make(chan struct{})
	close(now)
	return now
}

// Deps is what the integration is built from. Random and Now are the randomness and
// the clock, for a test to pin; nil means crypto/rand and time.Now. NewMediaBackend
// builds one media server's control API from its row and its credential, so a test
// can stand a fake in for every server; nil means LiveKit's (liveKitFor).
type Deps struct {
	Engine              Engine
	GateDB              func() *sql.DB
	ResolveSystemSecret func(ctx context.Context, name string) (string, error)
	Logger              *slog.Logger
	Random              io.Reader
	Now                 func() time.Time
	NewMediaBackend     func(server MediaServer, credential MediaCredential) (MediaBackend, error)
}

// Integration is the calls capability's Go half.
type Integration struct {
	store   *Store
	gateDB  func() *sql.DB
	logger  *slog.Logger
	random  io.Reader
	clock   func() time.Time
	service *Service
}

// New builds the integration, and the calls service over the same store, gate pool,
// pepper, clock and randomness (service.go).
func New(d Deps) *Integration {
	i := &Integration{
		store:  NewStore(d.Engine, d.ResolveSystemSecret),
		gateDB: d.GateDB,
		logger: d.Logger,
		random: d.Random,
		clock:  d.Now,
	}
	if i.logger == nil {
		i.logger = slog.Default()
	}
	if i.random == nil {
		i.random = rand.Reader
	}
	if i.clock == nil {
		i.clock = time.Now
	}
	media := d.NewMediaBackend
	if media == nil {
		media = liveKitFor
	}
	i.service = &Service{
		store: i.store, gateDB: i.gateDB, pepper: i.pepper, random: i.random,
		now: i.now, media: media, logger: i.logger, guestRetirement: &guestRetirementScan{},
	}
	return i
}

// liveKitFor is a media server's control API as LiveKit's room service, at the server's
// control address and signed with its credential.
func liveKitFor(server MediaServer, credential MediaCredential) (MediaBackend, error) {
	return NewLiveKitBackend(server.APIURL, credential.Key, credential.Secret)
}

func (i *Integration) now() time.Time { return i.clock().UTC() }

// Store is the integration's graph seam, for the later halves of the capability.
func (i *Integration) Store() *Store { return i.store }

// Service is the calls service: unlocking a key, starting, joining, queueing and
// leaving a call, booking, moving and cancelling one for later, a host's controls, the
// media server's webhooks, and the reconciliation pass. The calls route family serves it.
func (i *Integration) Service() *Service { return i.service }

// IntegrationName implements memql.IntegrationProvider.
func (i *Integration) IntegrationName() string { return pluginName }

// Capabilities implements memql.IntegrationProvider: the four operator acts
// (operator.go), the reconciliation pass the reconcileCalls automation runs
// (reconcile.go), and the media server's webhooks the applyMediaEvent automation applies
// (events.go), declared in dsl/calls/builtins.memql.
func (i *Integration) Capabilities() []memql.IntegrationCapability {
	return []memql.IntegrationCapability{
		{Name: "reportList", Description: "Private bounded report metadata inventory.", Handler: i.handleReportList, ArgsSchema: map[string]string{"siteId": "string required", "cursor": "string optional"}},
		{Name: "reportRead", Description: "Audited private report read.", Handler: i.handleReportRead, ArgsSchema: map[string]string{"siteId": "string required", "caseId": "string required"}},
		{Name: "reportReview", Description: "Revision-bound private report review, with no sanction side effect.", Handler: i.handleReportReview, ArgsSchema: map[string]string{"siteId": "string required", "caseId": "string required", "revision": "integer required", "outcome": "string required", "note": "string required", "idempotencyKey": "string required"}},
		{Name: "processingRooms", Description: "Bounded trusted worker room discovery.", Handler: i.handleProcessingRooms, ArgsSchema: map[string]string{"siteId": "string required"}},
		{Name: "processingLease", Description: "Trusted processing lease; operator only, outside runs.", Handler: i.handleProcessingLease, ArgsSchema: map[string]string{"siteId": "string required", "roomId": "string required", "leaseId": "string optional", "reset": "boolean optional", "maxTracks": "integer required"}},
		{Name: "processingPublish", Description: "Ephemeral consent-bound processing result; operator only, outside runs.", Handler: i.handleProcessingPublish, ArgsSchema: map[string]string{"siteId": "string required", "roomId": "string required", "leaseId": "string required", "revision": "integer required", "event": "object required"}},
		{
			Name:        "testKeyCreate",
			Description: "Mint a test calls key for a deployable. Returns {keyId, key}: the key once, here and nowhere else; only its digest is stored.",
			Handler:     i.handleTestKeyCreate,
			ArgsSchema: map[string]string{
				"siteId": "string (required) -- the deployable the key is for.",
				"label":  "string (optional) -- the operator's name for the key, at most 80 characters.",
			},
		},
		{
			Name:        "siteEnable",
			Description: "Turn the calls capability on or off for a deployable, as the person asking. Returns {siteId, calls}.",
			Handler:     i.handleSiteEnable,
			ArgsSchema: map[string]string{
				"siteId": "string (required) -- the deployable.",
				"on":     "boolean (required) -- true to turn calls on, false to turn them off.",
			},
		},
		{
			Name:        "policySet",
			Description: "Set any of a deployable's calls limits; a limit not named keeps its value. Durations are whole seconds. Returns the effective limits.",
			Handler:     i.handlePolicySet,
			ArgsSchema:  policySetArgsSchema(),
		},
		{
			Name:        "mediaServers",
			Description: "The active media servers rooms are placed on, one row each, never a credential or the name of the secret holding one.",
			Handler:     i.handleMediaServers,
		},
		{
			Name:        "reconcile",
			Description: "One reconciliation pass over every open call: seats repaired to what the media server shows, the host-left countdown applied, expired queue entries marked. Only the engine's own reconcileCalls automation runs it.",
			Handler:     i.handleReconcile,
		},
		{
			Name:        "applyMediaEvent",
			Description: "Apply one of the media server's webhooks, staged as an inbound request, to the call it names. Only the engine's own applyMediaEvent automation runs it, and only on a delivery the calls media credential verified.",
			Handler:     i.handleApplyMediaEvent,
			ArgsSchema: map[string]string{
				"requestId": "string (required) -- the staged v1:platform:inboundRequest row's id.",
			},
		},
	}
}

// The bootstrap's retry: attempts, the first wait between them (doubling), and each
// attempt's own bound. Six attempts over about half a minute outlast a sibling
// replica holding the gate and a database that is still coming up, and stop well
// short of retrying for ever against a configuration that will not work.
const (
	bootstrapAttempts     = 6
	bootstrapFirstBackoff = time.Second
	bootstrapAttemptBound = 30 * time.Second
)

// bootstrapAtBoot runs the media bootstrap once the engine is ready, retrying a failed
// attempt with a doubling wait. Success is logged with the server it recorded and what
// it wrote, never a value; a final failure is logged as an error.
func (i *Integration) bootstrapAtBoot(ready <-chan struct{}, cfg MediaConfig) {
	<-ready
	wait := bootstrapFirstBackoff
	for attempt := 1; ; attempt++ {
		ctx, cancel := context.WithTimeout(context.Background(), bootstrapAttemptBound)
		result, err := i.Bootstrap(ctx, cfg)
		cancel()
		if err == nil {
			i.logger.Info("calls: media server ready", "component", pluginName,
				"mediaServer", result.ServerID, "server", cfg, "state", result.State,
				"created", result.ServerCreated, "updated", result.ServerUpdated, "secretsWritten", result.SecretsWritten)
			return
		}
		if attempt == bootstrapAttempts {
			i.logger.Error("calls: the media bootstrap did not complete, so no room can be placed until a node boots with this configuration again",
				"component", pluginName, "attempts", attempt, "error", err.Error())
			return
		}
		i.logger.Warn("calls: the media bootstrap failed; trying again", "component", pluginName,
			"attempt", attempt, "retryIn", wait.String(), "error", err.Error())
		time.Sleep(wait)
		wait *= 2
	}
}
