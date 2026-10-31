// Package gamespack is the games pack: memory and skill learning for a game
// runtime that runs OUTSIDE the cluster (design record
// docs/superpowers/specs/2026-10-18-games-pack-design.md).
//
// A runtime plays in real time -- reflexes every tick, a decision every few
// seconds -- and the engine is what it remembers with and what it learns
// with. The runtime executes every action itself. This package stores,
// ranks, generalizes and governs; it never runs a game action.
//
// It is a PACK in the default build, like packs/reviewspack: linked into
// every binary by packs/anchor with no build tag, declared DISABLED, and
// governed per instance by v1:platform:packState.
package gamespack

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"io/fs"
	"log/slog"
	"time"

	"github.com/znasllc-io/memql/component/memql"
	memqldsl "github.com/znasllc-io/memql/dsl"
)

// Domain is the DSL namespace this pack owns: concepts are v1:games:*.
const Domain = "games"

// The pack's concept ids, and the user concept ownerUserId points at.
const (
	conceptUser       = "v1:identity:user"
	conceptPlayer     = "v1:games:player"
	conceptEpisode    = "v1:games:episode"
	conceptPreference = "v1:games:preference"
	conceptPromise    = "v1:games:promise"
	conceptPlace      = "v1:games:place"
	conceptSkill      = "v1:games:skill"
	conceptPlanTrace  = "v1:games:planTrace"
)

// ContractVersion is the Plugin SDK contract this pack was built against.
const ContractVersion = memql.PluginContractVersion

// integrationName is the <name> in every @executor("integration.<name>.*").
const integrationName = "games"

// DefaultEnabled is what this pack ships as when no v1:platform:packState
// row governs it. FALSE: the pack stores memories about the people who play,
// and holding that kind of data is a decision an owner makes, not one an
// upgrade makes for them.
const DefaultEnabled = false

//go:embed all:dsl
var packFS embed.FS

// Tree returns the pack's embedded .memql subtree.
func Tree() fs.FS {
	sub, err := fs.Sub(packFS, "dsl")
	if err != nil {
		panic("gamespack: embedded dsl tree missing: " + err.Error())
	}
	return sub
}

// Provider is the pack's IntegrationProvider.
//
// Every dependency is a seam so the decisions are testable over values and
// the engine wiring over a real database: store reads and writes rows through
// the engine under the caller's actor, space embeds and stores vectors, db is
// the raw handle the hand-written statements need (recall, the near-match
// lookup, the pending scan, and rowHolderSQL, who holds a row) plus forget's
// deletes, and gates is the write-gate pool a trace id, learning and every ladder
// move are serialized on.
type Provider struct {
	store      rowStore
	space      embeddingSpace
	db         func() *sql.DB
	gates      func() *sql.DB
	invalidate func(string)
	log        *slog.Logger
	now        func() time.Time

	// embedTimeout bounds how long a recall waits for the vector of its query.
	// Zero means recallEmbedTimeout; a test shortens it.
	embedTimeout time.Duration

	// afterForgetStep is a test seam. A forget calls it after each of its statements,
	// inside its transaction, with the name of the statement, and an error it returns
	// ends the forget there. A test makes the forget fail after each statement in turn,
	// to show that the ones before it are undone. Production leaves it nil.
	afterForgetStep func(step string) error

	// vectorTables is a test seam: the names of the vector tables a forget deletes
	// from. Nil means the catalogue's (memql.EmbeddingVectorTables). A test hands the
	// forget a name the catalogue never would, to show that it fails, and deletes
	// nothing, instead of leaving that table's vectors behind.
	vectorTables func(ctx context.Context, tx *sql.Tx) ([]string, error)

	// forgetGateWait is a test seam: how long a forget waits, in all, for the write gates of
	// the skills it deletes (fenceSkills). Zero is the gate's own wait, six seconds; a test
	// that holds a skill's gate and wants the refusal shortens it.
	forgetGateWait time.Duration

	// lock and holderOf are test seams for the two things recording a trace needs
	// from a database: the cross-replica write gate (gate.go) and the owner of a row
	// whoever that is (rowHolder). Nil means the real ones. A unit test over a fake
	// store, which has no gate pool and no database, hands them stand-ins that also
	// record the order things happen in: the gate, then the look, then the write.
	lock     func(ctx context.Context, key string) (release func(), err error)
	holderOf func(ctx context.Context, concept, id string) (owner string, found bool, err error)
}

// NewProvider builds the pack's IntegrationProvider from the PluginContext.
//
// It REFUSES a context with no GateDB (the contract on PluginContext.GateDB):
// learning and every ladder move take a write gate on that pool and on nothing
// else, and a pack built without it would record traces and answer lookups
// until its first learning decision, and only then say that shared write
// coordination is unavailable.
func NewProvider(pctx memql.PluginContext) (memql.IntegrationProvider, error) {
	if pctx.GateDB == nil {
		return nil, fmt.Errorf("games pack: no GateDB in PluginContext: its learning and ladder moves take write gates on the gate pool (database.Database.GateDB; a test or a tool opens one with database.NewGatePool)")
	}
	log := pctx.Logger
	if log == nil {
		log = slog.Default()
	}
	store := engineStore{engine: pctx.Engine}
	db := func() *sql.DB {
		if pctx.BunDB == nil {
			return nil
		}
		b := pctx.BunDB()
		if b == nil {
			return nil
		}
		return b.DB
	}
	return &Provider{
		store:      store,
		space:      clusterSpace{resolve: pctx.ResolveEmbeddingProvider, rows: store, db: db, log: log},
		db:         db,
		gates:      pctx.GateDB,
		invalidate: pctx.InvalidateConcept,
		log:        log,
		now:        time.Now,
	}, nil
}

// IntegrationName implements memql.IntegrationProvider.
func (p *Provider) IntegrationName() string { return integrationName }

// Register wires the pack into the engine registries. packs/anchor calls it
// once per process, before the first concept load.
//
//   - RegisterPackDefault(domain, false): the pack ships disabled; an owner
//     enables it in Cluster > Modules and every node picks it up at its next
//     restart.
//   - registerRoutingRules: the live graph's forward rules for this pack's
//     concepts, without which a row written through one replica never
//     reaches a subscriber on another (routing.go).
//   - BindPluginToPack + RegisterPluginForContract: the Go half, skipped by
//     app's materializePlugins while the pack is disabled.
//
// It does NOT call RegisterStorefrontPack: the games pack has no shopper
// surface, and only an owner may switch it on.
func Register(domain string) {
	memqldsl.RegisterTree(domain, Tree())
	memqldsl.RegisterPackDefault(domain, DefaultEnabled)
	registerRoutingRules()
	memql.BindPluginToPack(domain, domain)
	memql.RegisterPluginForContract(domain, ContractVersion, NewProvider)
}

// clock returns the provider's notion of now, in UTC.
func (p *Provider) clock() time.Time {
	if p.now == nil {
		return time.Now().UTC()
	}
	return p.now().UTC()
}

// logger never returns nil.
func (p *Provider) logger() *slog.Logger {
	if p.log == nil {
		return slog.Default()
	}
	return p.log
}

// ctxDone reports a cancelled caller, so a loop over rows stops early.
func ctxDone(ctx context.Context) bool { return ctx.Err() != nil }
