package memql

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/core/airoute"
)

func (e *MemQLEngine) readEmbedderPlan(ctx context.Context) (EmbedderBinding, error) {
	result, err := e.Execute(ContextWithFreshRead(ctx), "query platform.activeEmbedderBinding()")
	if err != nil {
		return EmbedderBinding{}, err
	}
	rows := MaterializeRows(result.OutputPayload())
	if len(rows) != 1 {
		return EmbedderBinding{}, ErrNoEmbedderBound
	}
	b, err := embedderBindingFromRow(rows[0])
	if err != nil {
		return b, err
	}
	if !b.Valid() {
		return EmbedderBinding{}, ErrNoEmbedderBound
	}
	return b, nil
}

// embedderBindingFromRow decodes one embedderBindingFull row. The row's keys
// are the concept's field names, which match EmbedderBinding's fields
// case-insensitively, so a field the shape projects reaches the struct without
// a mapping to keep in step -- the indexer and the chooser included, which is
// how every replica learns whose machines memory search embeds on.
func embedderBindingFromRow(row map[string]any) (EmbedderBinding, error) {
	var b EmbedderBinding
	raw, err := json.Marshal(row)
	if err != nil {
		return EmbedderBinding{}, err
	}
	if err = json.Unmarshal(raw, &b); err != nil {
		return EmbedderBinding{}, err
	}
	return b, nil
}

func (e *MemQLEngine) ReadEmbedderBinding(ctx context.Context) (EmbedderBinding, error) {
	b, err := e.readEmbedderPlan(ctx)
	if err == nil && b.ActivatedAt == "" {
		return EmbedderBinding{}, ErrNoEmbedderBound
	}
	return b, err
}

// Vector dimensions belong to the bound model, including a fleet model whose
// runtime reports an unknown width. Explicit providers keep their own space.
func EmbeddingDimensions(ctx context.Context, provider string, declared int) int {
	if b, err := CurrentEmbedderBinding(ctx); err == nil && b.ProviderRef == provider {
		return b.Dimensions
	}
	return declared
}

func ValidateEmbeddingVector(ctx context.Context, provider string, declared int, vector []float32) error {
	expected := EmbeddingDimensions(ctx, provider, declared)
	if len(vector) == 0 || (expected > 0 && len(vector) != expected) {
		return fmt.Errorf("embedding model returned %d dimensions, expected %d", len(vector), expected)
	}
	nonzero := false
	for _, value := range vector {
		if math.IsNaN(float64(value)) || math.IsInf(float64(value), 0) {
			return fmt.Errorf("embedding model returned a non-finite vector")
		}
		nonzero = nonzero || value != 0
	}
	if !nonzero {
		return fmt.Errorf("embedding model returned a zero vector")
	}
	return nil
}

var embeddingTableName = regexp.MustCompile(`^node_vectors(?:_[0-9]+(?:_[0-9a-f]{16})?)?$`)

// Retention removes a deleted source from every historical model space. Only
// validated identifiers from PostgreSQL's table catalog enter generated SQL.
func EmbeddingVectorTables(ctx context.Context, db interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}) ([]string, error) {
	rows, err := db.QueryContext(ctx, `SELECT tablename FROM pg_tables WHERE schemaname=current_schema() AND tablename LIKE 'node_vectors%'`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var tables []string
	for rows.Next() {
		var table string
		if err := rows.Scan(&table); err != nil {
			return nil, err
		}
		if embeddingTableName.MatchString(table) {
			tables = append(tables, table)
		}
	}
	return tables, rows.Err()
}

// maxEmbeddingIndexDimensions is the widest vector memory search's index can
// hold. Every model's table carries an HNSW index on its vector column
// (createVectorTableSQL), and pgvector builds one only for a column of at most
// 2000 dimensions, so a wider model can never be bound or switched to.
const maxEmbeddingIndexDimensions = 2000

// embeddingWidthRefusal refuses a model whose vectors are wider than the index
// can hold, in words a person reads, BEFORE anything is embedded: the probe
// that would learn the width otherwise runs first, and for a vendor model the
// cluster's account pays for it (and for a confirmation answered over a switch
// that can never start). A width nothing has declared (0) is not refused here;
// the probe measures it, on the owner's machine, and EmbeddingVectorTable
// refuses it before any table exists.
func embeddingWidthRefusal(label string, dims int) error {
	if dims <= maxEmbeddingIndexDimensions {
		return nil
	}
	return fmt.Errorf("%s produces vectors %d wide, and memory search's index holds at most %d", label, dims, maxEmbeddingIndexDimensions)
}

// Model identity as well as width defines a vector space. Two 1024-wide models
// must never search one another's vectors. The identifier is derived entirely
// from a digest and a validated integer, not SQL supplied by a caller.
func EmbeddingVectorTable(provider string, dimensions int) (string, error) {
	if strings.TrimSpace(provider) == "" || dimensions < 1 || dimensions > maxEmbeddingIndexDimensions {
		return "", fmt.Errorf("embedding index requires a provider and 1–%d dimensions", maxEmbeddingIndexDimensions)
	}
	hash := sha256.Sum256([]byte(provider))
	return fmt.Sprintf("node_vectors_%d_%x", dimensions, hash[:8]), nil
}

// EnsureEmbeddingVectorTable creates a model's vector table (EmbeddingVectorTable)
// if it does not exist, and answers its name. IDEMPOTENT BY CONSTRUCTION (`IF
// NOT EXISTS` throughout, under an advisory lock on the table's name), because
// every store and every tick of the memory index job calls it.
//
// DDL AT RUNTIME IS DELIBERATE, though this repo's convention is migrations: a
// migration cannot express this table, because its model and width are not
// known until an owner chooses one, and a migration per catalog model would
// create empty tables on every cluster.
func EnsureEmbeddingVectorTable(ctx context.Context, db *sql.DB, provider string, dimensions int) (string, error) {
	table, err := EmbeddingVectorTable(provider, dimensions)
	if err != nil {
		return "", err
	}
	if db == nil {
		return "", fmt.Errorf("embedding storage is unavailable")
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, table); err != nil {
		return "", err
	}
	for _, statement := range createVectorTableSQL(dimensions) {
		statement = strings.ReplaceAll(statement, VectorTableFor(dimensions), table)
		if _, err = tx.ExecContext(ctx, statement); err != nil {
			return "", err
		}
	}
	return table, tx.Commit()
}

// Initial activation is deliberately distinct from switching an existing
// corpus. A switch rebuilds the whole index first and is the owner's
// memorySearchSwitch (memory_search_builtins.go), which the memory index job
// carries out; this builtin binds only a cluster's first model, so no partial
// space ever becomes active.
func (e *MemQLEngine) bindEmbedderBuiltin(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	ac, _ := auth.AccessFromContext(ctx)
	if ac == nil || ac.Role != auth.RoleOwner {
		return nil, fmt.Errorf("only the cluster owner can bind its embedder")
	}
	provider := strings.TrimSpace(stringArg(args, "provider"))
	if provider == "" {
		return nil, fmt.Errorf("choose an embedding model")
	}
	// The owner's own choice: they chose it, and a fleet model is indexed on
	// their machines (and the ones lent to them). A vendor model has no
	// machines, so no indexer (IndexerFor).
	choice := embedderChoice{ChosenBy: ac.UserId}
	if _, fleet := IsFleetReference(provider); fleet {
		choice.IndexedBy = ac.UserId
	}
	return e.bindEmbedder(ctx, provider, choice)
}

// errEmbedderBindingActive is bindEmbedder refusing to change a binding that is
// active. A sentinel, because memorySearchSwitch's no-binding path reads it as
// a binding that landed meanwhile rather than passing its direction on.
var errEmbedderBindingActive = errors.New("changing the active memory-search model rebuilds its whole index first; choose the model in Cluster > Settings > Memory search")

// errEmbedderPlanUnfinished is bindEmbedder refusing a first model while an
// unfinished plan of another one -- recorded, never activated -- holds the
// row: it names that model, which is the one choice that finishes it.
type errEmbedderPlanUnfinished struct{ ref string }

func (e errEmbedderPlanUnfinished) Error() string {
	model := e.ref
	if id, fleet := IsFleetReference(e.ref); fleet {
		model = id
	}
	return fmt.Sprintf("an unfinished binding of %s is in the way; choose %s to finish it", model, model)
}

// embedderChoice records, with a binding, whose machines index it and who
// chose it (design record 2026-11-23, sections 1 and 2). ChosenBy is empty for
// the automatic first choice: nobody chose it, so nobody is told they did.
type embedderChoice struct{ IndexedBy, ChosenBy string }

// bindEmbedder makes provider the cluster's embedder: it embeds one sentence to
// learn the vector width, creates the table for it, and records and activates
// the binding, under the binding's advisory lock. The FIRST binding needs no
// re-embed; changing an active one rebuilds the index first, which is a switch
// (memorySearchSwitch and the memory index job), and is refused here. A plan
// left unfinished -- recorded, never activated -- is finished by choosing its
// own model; another model is refused, naming it (errEmbedderPlanUnfinished).
func (e *MemQLEngine) bindEmbedder(ctx context.Context, provider string, choice embedderChoice) ([]memorynodes.MemoryNode, error) {
	unlock, err := e.lockAskConversationKind(ctx, "active", "embedder-binding")
	if err != nil {
		return nil, err
	}
	defer unlock()
	current, readErr := e.readEmbedderPlan(ctx)
	if readErr == nil {
		switch {
		case current.ActivatedAt != "" && current.ProviderRef != provider:
			return nil, errEmbedderBindingActive
		case current.ActivatedAt != "":
			raw, _ := json.Marshal(map[string]any{"provider": provider, "dimensions": current.Dimensions, "active": true})
			return []memorynodes.MemoryNode{{ID: "active", Payload: raw}}, nil
		case current.ProviderRef != provider:
			// An UNFINISHED plan of another model: written, never activated
			// (a replica died between the two writes). The plan write is
			// @createOnly on the model and its width -- what keeps a plan
			// from ever re-pointing a row that is active -- so another
			// model's plan cannot replace it, and only its own model can
			// finish it. The refusal NAMES that model: refused as a binding
			// that landed meanwhile, as it was, every other first model
			// answered "changed meanwhile, try again" for ever, with nothing
			// saying what would clear it.
			return nil, errEmbedderPlanUnfinished{ref: current.ProviderRef}
		}
	}
	if readErr != nil && readErr != ErrNoEmbedderBound {
		return nil, readErr
	}
	dims, err := e.probeEmbedder(ctx, provider, choice.IndexedBy)
	if err != nil {
		return nil, err
	}
	if current.Valid() && current.Dimensions != dims {
		return nil, fmt.Errorf("the pending binding has a different vector width")
	}
	if e.database() == nil {
		return nil, fmt.Errorf("embedding storage is unavailable")
	}
	if _, err = EnsureEmbeddingVectorTable(ctx, e.database().DB, provider, dims); err != nil {
		return nil, err
	}
	internal := auth.ContextWithInternalOrigin(ctx)
	for _, write := range []struct {
		name string
		args map[string]any
	}{
		{"platform.recordEmbedderBindingPlan", map[string]any{"bindingId": "active", "providerRef": provider, "dimensions": dims}},
		{"platform.activateEmbedderBinding", map[string]any{"bindingId": "active", "activatedAt": time.Now().UTC().Format(time.RFC3339Nano)}},
	} {
		// Each write is stamped after the binding's newest version, read here
		// under the binding lock (ruling R12), as every other binding write is:
		// a plan left by a replica whose clock runs ahead would otherwise stay
		// the newest version, and the binding would read as never activated.
		version, err := e.latestRowVersion(ctx, EmbedderBindingConceptID, ActiveEmbedderBindingID)
		if err != nil {
			return nil, err
		}
		write.args["versionTime"] = VersionTimeAfter(version, time.Now()).Format(time.RFC3339Nano)
		call, _ := parser.RenderCall(write.name, write.args)
		if _, err = e.Execute(internal, "mutation "+call); err != nil {
			return nil, err
		}
	}
	// Still under the binding lock taken above, which writeEmbedderChoice
	// requires.
	if err = e.writeEmbedderChoice(internal, choice); err != nil {
		return nil, err
	}
	bound, err := e.ReadEmbedderBinding(ctx)
	if err != nil {
		return nil, err
	}
	if bound.ProviderRef != provider || bound.Dimensions != dims {
		return nil, fmt.Errorf("the binding changed during activation")
	}
	raw, _ := json.Marshal(map[string]any{"provider": provider, "dimensions": dims, "active": true})
	return []memorynodes.MemoryNode{{ID: "active", Payload: raw}}, nil
}

// probeEmbedder embeds one sentence with provider and answers the vector width
// it produced, after checking the vector is usable (ValidateEmbeddingVector).
// The probe runs where the model will index, not on the caller's machines: a
// fleet model is resolved as indexer (their machines and the ones lent to
// them); a vendor model, with no indexer, on the cluster's account. bindEmbedder
// learns a first model's width this way (a first choice, the automatic choice
// and the bindEmbedder builtin), and memorySearchSwitch a new one's. So this is
// where every path refuses a model whose declared width the index cannot hold:
// after the resolution, which calls nothing, and before the one embedding,
// which a vendor bills (embeddingWidthRefusal).
func (e *MemQLEngine) probeEmbedder(ctx context.Context, provider, indexer string) (int, error) {
	model, _, err := ResolveAITyped[EmbeddingAIProvider](ContextWithEmbeddingIndexer(ctx, indexer), e, airoute.ResolveRequest{Level: airoute.LevelEmbeddings, Modality: airoute.ModalityEmbedding, ExplicitProvider: provider, UserId: indexer})
	if err != nil {
		return 0, err
	}
	if err := embeddingWidthRefusal(provider, model.Dimensions()); err != nil {
		return 0, err
	}
	vector, err := model.Embed(ctx, "MemQL embedding readiness check")
	if err != nil {
		return 0, err
	}
	if err = ValidateEmbeddingVector(ctx, provider, model.Dimensions(), vector); err != nil {
		return 0, err
	}
	return len(vector), nil
}

// autoBindEmbedder binds the cluster's FIRST embedder by itself (design record
// 2026-10-18-routes, narrowed by 2026-11-23 section 2): when none is bound and
// an ONLINE machine of an active cluster OWNER -- their own, or one lent to
// them -- advertises an embedding model, it is bound, indexed by that owner.
// There are no vectors yet, so binding plans no re-embed, and the inference
// verdict needs an embedder before it lifts the core gate.
//
// WHAT IT NEVER DOES. It never binds a vendor model: that spends the cluster's
// account on a choice nobody made, and an owner can still pick one. It never
// binds a model only a non-owner's machines offer: memory search is the
// owner's index (embedder_indexer.go) and a writer's laptop is not theirs to
// give. With no owner candidate it binds nothing and the next pass tries again.
//
// It never CHANGES a binding, and it runs under the binding's advisory lock,
// so every node may try it and exactly one binds. It also runs
// adoptEmbedderIndexer, the same pass, for a binding made before indexers.
func (e *MemQLEngine) autoBindEmbedder(ctx context.Context) {
	if e == nil || e.providers == nil {
		return
	}
	_, err := e.ReadEmbedderBinding(ctx)
	if err == nil {
		if adoptErr := e.adoptEmbedderIndexer(ctx); adoptErr != nil {
			e.safeLogger().Warn("embedder: could not name the indexer of the active binding", "error", adoptErr)
		}
		return
	}
	if err != ErrNoEmbedderBound {
		return
	}
	candidates, err := e.ownerEmbeddingCandidates(ctx)
	if err != nil {
		e.safeLogger().Warn("embedder: could not read the owners' machine models", "error", err)
		return
	}
	for _, c := range candidates {
		bctx := auth.ContextWithInternalOrigin(auth.ContextWithUserActor(ctx, c.OwnerUserId))
		if _, err := e.bindEmbedder(bctx, c.ProviderRef, embedderChoice{IndexedBy: c.OwnerUserId}); err != nil {
			e.safeLogger().Warn("embedder: could not bind the first embedding model automatically; trying the next",
				"provider", c.ProviderRef, "error", err)
			continue
		}
		e.safeLogger().Info("embedder: bound the cluster's first embedding model", "provider", c.ProviderRef, "indexedBy", c.OwnerUserId)
		return
	}
}

// embedderCandidate is one embedding model an owner's machines offer.
type embedderCandidate struct{ ProviderRef, OwnerUserId string }

// ownerEmbeddingCandidates lists the online embedding models of every active
// owner's fleet catalog -- their own machines and those lent to them, which is
// what the catalog reader returns for a person -- sorted by model id, then
// owner id, so every replica proposes the same first one.
func (e *MemQLEngine) ownerEmbeddingCandidates(ctx context.Context) ([]embedderCandidate, error) {
	owners, err := e.activeOwnerIds(ctx)
	if err != nil {
		return nil, err
	}
	var out []embedderCandidate
	for _, owner := range owners {
		models, err := e.providers.FleetCatalog(readinessEvaluateContext(ctx), owner)
		if err != nil {
			e.safeLogger().Warn("embedder: could not read an owner's machine catalog", "owner", owner, "error", err)
			continue
		}
		for _, m := range models {
			if m.Embeddings && m.Online() && m.ModelId != "" {
				out = append(out, embedderCandidate{ProviderRef: FleetReferencePrefix + m.ModelId, OwnerUserId: owner})
			}
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].ProviderRef != out[j].ProviderRef {
			return out[i].ProviderRef < out[j].ProviderRef
		}
		return out[i].OwnerUserId < out[j].OwnerUserId
	})
	return out, nil
}

// adoptEmbedderIndexer names an indexer for an active `fleet:` binding that has
// none -- production's binding, made when a model resolved against whoever
// asked. TRIGGER: boot readiness (autoBindEmbedder). EVIDENCE: the first active
// owner, in id order, whose catalog OFFERS the model, online or not: an
// offline machine is still the one that holds the model, and waiting for it to
// wake would leave search resolving as each caller meanwhile. If no owner
// offers it nothing is written and the binding keeps resolving as the caller
// until one does. ChosenBy stays empty: adoption is not a choice, and nobody is
// shown as having made one. It writes only when IndexedBy is still empty after
// a fresh read under the binding lock, so a concurrent switch or choice on
// another replica is never overwritten (memql#5431).
func (e *MemQLEngine) adoptEmbedderIndexer(ctx context.Context) error {
	b, err := e.ReadEmbedderBinding(ctx)
	if err != nil {
		return nil
	}
	modelId, fleet := IsFleetReference(b.ProviderRef)
	if !fleet || strings.TrimSpace(b.IndexedBy) != "" {
		return nil
	}
	owners, err := e.activeOwnerIds(ctx)
	if err != nil {
		return err
	}
	adopted := ""
	for _, owner := range owners {
		models, cErr := e.providers.FleetCatalog(readinessEvaluateContext(ctx), owner)
		if cErr != nil {
			continue
		}
		for _, m := range models {
			if m.ModelId == modelId {
				adopted = owner
				break
			}
		}
		if adopted != "" {
			break
		}
	}
	if adopted == "" {
		e.safeLogger().Info("embedder: no owner's machines offer the bound memory-search model yet; it keeps resolving as the caller", "provider", b.ProviderRef)
		return nil
	}
	unlock, err := e.lockAskConversationKind(ctx, "active", "embedder-binding")
	if err != nil {
		return err
	}
	defer unlock()
	fresh, err := e.readEmbedderPlan(ctx)
	if err != nil || fresh.ProviderRef != b.ProviderRef || strings.TrimSpace(fresh.IndexedBy) != "" {
		return nil
	}
	if err := e.writeEmbedderChoice(auth.ContextWithInternalOrigin(ctx), embedderChoice{IndexedBy: adopted}); err != nil {
		return err
	}
	e.safeLogger().Info("embedder: named the indexer of the active memory-search model", "provider", b.ProviderRef, "indexedBy", adopted)
	return nil
}

// writeEmbedderChoice records the indexer and/or chooser on the active binding
// through setEmbedderBindingChoice, which read-merges the row and so REQUIRES
// the caller to hold the binding lock (lockAskConversationKind "active",
// "embedder-binding") around its own read and this write: a flip on another
// replica between the two would otherwise be silently reverted (memql#5431).
// Only non-empty fields are sent, so an empty chooser never clears one.
//
// The new version is stamped after the binding's newest one (ruling R12,
// memql#5431's third part), read here under the caller's lock: written at this
// replica's clock alone, a choice made on a replica whose clock lags the last
// binding writer's would be an OLDER version, and every reader would go on
// seeing the binding without it.
func (e *MemQLEngine) writeEmbedderChoice(ctx context.Context, choice embedderChoice) error {
	args := map[string]any{"bindingId": "active"}
	if v := strings.TrimSpace(choice.IndexedBy); v != "" {
		args["indexedBy"] = v
	}
	if v := strings.TrimSpace(choice.ChosenBy); v != "" {
		args["chosenBy"] = v
	}
	if len(args) == 1 {
		return nil
	}
	version, err := e.latestRowVersion(ctx, EmbedderBindingConceptID, ActiveEmbedderBindingID)
	if err != nil {
		return err
	}
	args["versionTime"] = VersionTimeAfter(version, time.Now()).Format(time.RFC3339Nano)
	call, err := parser.RenderCall("platform.setEmbedderBindingChoice", args)
	if err != nil {
		return err
	}
	_, err = e.Execute(auth.ContextWithInternalOrigin(ctx), "mutation "+call)
	return err
}

// activeOwnerIds lists every ACTIVE user whose stored role is `owner`, sorted.
// It reads through activeUsers (@serverOnly, internal origin), the read the
// identity admin and the seed sweep use; a role that only ALIASES owner is not
// an owner here, because the design names the stored role.
func (e *MemQLEngine) activeOwnerIds(ctx context.Context) ([]string, error) {
	ictx := auth.ContextWithInternalOrigin(systemActorContext(ctx))
	var ids []string
	seen := map[string]bool{}
	err := WalkQueryPages(ictx, e.Execute, `query activeUsers(role: "owner")`, 100, func(result *ExecuteResult) error {
		for _, id := range extractRowIds(result) {
			if !seen[id] {
				seen[id] = true
				ids = append(ids, id)
			}
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("activeUsers(owner): %w", err)
	}
	sort.Strings(ids)
	return ids, nil
}

// indexerOwnerTTL bounds how stale the resolver's owner re-check may be while
// the database answers; indexerOwnerRetry is how soon a FAILED read is retried.
const (
	indexerOwnerTTL   = 60 * time.Second
	indexerOwnerRetry = 5 * time.Second
)

type ownerSetCache struct {
	mu      sync.Mutex // guards the fields only; never held across a read
	at      time.Time  // when owners was read
	retryAt time.Time  // no read before this, after a failed one
	owners  map[string]bool
	failed  error // the last read's failure; nil once a read succeeds
	reading bool
	read    func(context.Context) ([]string, error) // nil: activeOwnerIds; set by tests
}

// IndexerIsActiveOwner reports whether userId is an active user with the
// stored role `owner`, from a per-replica set.
//
// FRESHNESS. While the database answers, the set is at most indexerOwnerTTL
// old. The refresh runs OUTSIDE the lock and one caller at a time; others keep
// the current set meanwhile, so no embedding waits behind a read. A FAILED
// read keeps the last good set and is retried after indexerOwnerRetry, not on
// every call. DURING AN OUTAGE THE OLD SET IS SERVED WITHOUT AN AGE BOUND: the
// 60 s bound is for a reachable database, and the same outage would stop the
// embedding's own reads. A replica that never read a set refuses
// (false): an indexer that cannot be confirmed is not trusted, and the
// writer's "until backfilled" path catches the item up later.
func (e *MemQLEngine) IndexerIsActiveOwner(ctx context.Context, userId string) bool {
	owner, _ := e.indexerOwnerAnswer(ctx, userId)
	return owner
}

// errIndexerOwnersUnread is an owner check on a replica whose first read of the
// owner set has not finished.
var errIndexerOwnersUnread = errors.New("the cluster's owners have not been read yet; try again in a moment")

// indexerOwnerAnswer is IndexerIsActiveOwner's answer and, when the replica has
// none to give, why. It answers from the set it has, stale during an outage as
// documented above; only while NO set has been read -- every read so far
// failed, or the first is still running on another call -- does it answer false
// with an error. IndexerIsActiveOwner reads that as false (an indexer that
// cannot be confirmed is not trusted); an owner's act on Memory search reports
// it, because "you are not an active cluster owner" would be a guess.
func (e *MemQLEngine) indexerOwnerAnswer(ctx context.Context, userId string) (bool, error) {
	userId = strings.TrimPrefix(strings.TrimSpace(userId), conceptIdentityUser+":")
	if userId == "" {
		return false, nil
	}
	c := &e.indexerOwners
	now := time.Now()
	c.mu.Lock()
	due := (c.owners == nil || now.Sub(c.at) > indexerOwnerTTL) && !now.Before(c.retryAt) && !c.reading
	if due {
		c.reading = true
	}
	read := c.read
	c.mu.Unlock()
	if due {
		if read == nil {
			read = e.activeOwnerIds
		}
		ids, err := c.readOnce(ctx, read)
		c.mu.Lock()
		c.reading = false
		if err != nil {
			c.retryAt = time.Now().Add(indexerOwnerRetry)
			c.failed = err
			c.mu.Unlock()
			lg := e.safeLogger()
			if lg == nil {
				lg = slog.Default()
			}
			lg.Warn("memory search: could not confirm who the owners are; keeping the last answer", "error", err)
			c.mu.Lock()
		} else {
			c.owners = make(map[string]bool, len(ids))
			for _, id := range ids {
				c.owners[strings.TrimPrefix(id, conceptIdentityUser+":")] = true
			}
			c.at, c.failed = time.Now(), nil
		}
		defer c.mu.Unlock()
		return c.answer(userId)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.answer(userId)
}

// readOnce runs one read of the owner set for a caller that marked the cache
// as reading. A read that PANICS clears the mark before the panic goes on to
// whoever recovers it: left set, it would keep every later call on this
// replica from reading again, so the replica would answer from the set it had
// -- or refuse every indexer, with none -- for good. A read that returns
// leaves the mark to the caller, which clears it with what it stores, under
// one lock.
func (c *ownerSetCache) readOnce(ctx context.Context, read func(context.Context) ([]string, error)) ([]string, error) {
	returned := false
	defer func() {
		if !returned {
			c.mu.Lock()
			c.reading = false
			c.mu.Unlock()
		}
	}()
	ids, err := read(ctx)
	returned = true
	return ids, err
}

// answer is the set's word on userId, or, with no set read yet, why not. The
// caller holds mu.
func (c *ownerSetCache) answer(userId string) (bool, error) {
	switch {
	case c.owners != nil:
		return c.owners[userId], nil
	case c.failed != nil:
		return false, fmt.Errorf("the cluster's owners could not be read: %w", c.failed)
	}
	return false, errIndexerOwnersUnread
}
