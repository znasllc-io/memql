package shopify

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strings"
	stdsync "sync"
	"time"

	"github.com/znasllc-io/memql/component/memql"
	memqlsync "github.com/znasllc-io/memql/component/memql/sync"
)

// Store is one configured Shopify store, as the connector needs it.
//
// The three credentials are held as REFERENCES on the row and resolved on
// demand, never stored here. A token that lives in a struct for the life of a
// process is a token that appears in a heap dump, a panic trace and a
// debugger; one resolved per call is only where the call is.
type Store struct {
	ID                 string
	Domain             string
	Name               string
	AppClientID        string
	AdminTokenRef      string
	StorefrontTokenRef string
	WebhookSecretRef   string
	APIVersion         string
	ScopesGranted      []string
	ProtectedDataLevel string
	Plan               string
	Status             string
	Health             map[string]any
	OwnerUserID        string
	RedactedAt         string
}

// Store lifecycle statuses, mirroring the concept's enum.
const (
	StatusConfigured  = "configured"
	StatusBackfilling = "backfilling"
	StatusLive        = "live"
	StatusPaused      = "paused"
	StatusError       = "error"
)

// Protected customer data approval levels.
const (
	ProtectedNone   = "none"
	ProtectedLevel1 = "level1"
	ProtectedLevel2 = "level2"
)

// Ingests reports whether deliveries should be applied for this store. A
// paused store still STAGES: the receiver keeps recording what arrived, so a
// pause loses telemetry rather than events, and resuming does not need a
// backfill.
func (s Store) Ingests() bool {
	return s.Status == StatusConfigured || s.Status == StatusBackfilling || s.Status == StatusLive
}

// ProtectedLevelAtLeast reports whether the store's approval reaches a level.
// The ShopifyQL pass-through needs Level 2, and refusing on this side gives a
// reason a Shopify 403 does not.
func (s Store) ProtectedLevelAtLeast(level string) bool {
	rank := map[string]int{ProtectedNone: 0, ProtectedLevel1: 1, ProtectedLevel2: 2}
	return rank[s.ProtectedDataLevel] >= rank[level]
}

// StoreRegistry reads store rows and resolves their credentials.
//
// It caches the ROWS and never the secrets. The rows change when an operator
// edits them, which is rare and tolerates a short staleness; a cached secret
// would outlive a rotation, and the failure would be a store that keeps
// verifying with a key its owner has already revoked.
type StoreRegistry struct {
	engine  memql.IntegrationEngineAccess
	secrets func(ctx context.Context, name string) (string, error)
	ttl     time.Duration
	now     func() time.Time

	mu       stdsync.RWMutex
	cached   map[string]Store
	cachedAt time.Time
}

// NewStoreRegistry builds a registry. `secrets` resolves a globalSecret by
// name -- PluginContext.ResolveSystemSecret in production.
func NewStoreRegistry(engine memql.IntegrationEngineAccess, secrets func(context.Context, string) (string, error)) *StoreRegistry {
	return &StoreRegistry{
		engine:  engine,
		secrets: secrets,
		ttl:     30 * time.Second,
		now:     time.Now,
	}
}

// Stores lists every configured store, refreshing the cache when it is stale.
func (r *StoreRegistry) Stores(ctx context.Context) ([]Store, error) {
	if r == nil || r.engine == nil {
		return nil, nil
	}
	r.mu.RLock()
	fresh := r.cached != nil && r.now().Sub(r.cachedAt) < r.ttl
	if fresh {
		out := sortedStores(r.cached)
		r.mu.RUnlock()
		return out, nil
	}
	r.mu.RUnlock()
	return r.refresh(ctx)
}

// Invalidate drops the cache. Called after a write that changes a store row,
// so an operator's edit takes effect on the next call rather than after the
// TTL.
func (r *StoreRegistry) Invalidate() {
	if r == nil {
		return
	}
	r.mu.Lock()
	r.cached = nil
	r.mu.Unlock()
}

func (r *StoreRegistry) refresh(ctx context.Context) ([]Store, error) {
	res, err := r.engine.Execute(connectorContext(ctx), "stores()")
	if err != nil {
		return nil, fmt.Errorf("shopify: list stores: %w", err)
	}
	next := map[string]Store{}
	for _, row := range memql.MaterializeRows(res) {
		s, ok := storeFromRow(row)
		if !ok {
			continue
		}
		next[s.ID] = s
	}
	r.mu.Lock()
	r.cached = next
	r.cachedAt = r.now()
	r.mu.Unlock()
	return sortedStores(next), nil
}

// ByID resolves one store.
func (r *StoreRegistry) ByID(ctx context.Context, id string) (Store, bool) {
	stores, err := r.Stores(ctx)
	if err != nil {
		return Store{}, false
	}
	for _, s := range stores {
		if s.ID == id {
			return s, true
		}
	}
	return Store{}, false
}

// ByDomain resolves a store from a delivery's X-Shopify-Shop-Domain header.
func (r *StoreRegistry) ByDomain(ctx context.Context, domain string) (Store, bool) {
	domain = strings.ToLower(strings.TrimSpace(domain))
	if domain == "" {
		return Store{}, false
	}
	stores, err := r.Stores(ctx)
	if err != nil {
		return Store{}, false
	}
	for _, s := range stores {
		if strings.ToLower(s.Domain) == domain {
			return s, true
		}
	}
	return Store{}, false
}

// Secret resolves one of a store's credential references.
//
// A blank reference is NOT an error here: a store may legitimately have no
// Storefront token yet. An unresolvable one IS, and the error names the
// reference rather than the value.
func (r *StoreRegistry) Secret(ctx context.Context, ref string) (string, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return "", nil
	}
	if r == nil || r.secrets == nil {
		return "", fmt.Errorf("shopify: no secret resolver configured, so %q cannot be read", ref)
	}
	v, err := r.secrets(ctx, ref)
	if err != nil {
		return "", fmt.Errorf("shopify: resolve secret %q: %w", ref, err)
	}
	return v, nil
}

// AdminToken resolves the Admin API access token for a store.
func (r *StoreRegistry) AdminToken(ctx context.Context, s Store) (string, error) {
	tok, err := r.Secret(ctx, s.AdminTokenRef)
	if err != nil {
		return "", err
	}
	if tok == "" {
		return "", fmt.Errorf("shopify: store %q has no Admin token (adminTokenRef=%q)", s.ID, s.AdminTokenRef)
	}
	return tok, nil
}

// InboundSourceFor builds the receiver policy for one store.
//
// The scheme is Shopify's, spelled in the receiver's ENCODING vocabulary
// rather than as a vendor: base64 HMAC-SHA256 over the raw body, in
// X-Shopify-Hmac-Sha256, deduped on X-Shopify-Webhook-Id. There is no vendor
// table in component/inbound and this does not add one.
func (r *StoreRegistry) InboundSourceFor(ctx context.Context, s Store) (memqlsync.InboundSource, bool) {
	secret, err := r.Secret(ctx, s.WebhookSecretRef)
	if err != nil || secret == "" {
		return memqlsync.InboundSource{
			Name:      memqlsync.SourceName(ConnectorName, s.ID),
			SecretRef: s.WebhookSecretRef,
		}, true
	}
	return memqlsync.InboundSource{
		Name:            memqlsync.SourceName(ConnectorName, s.ID),
		Scheme:          "hmac-sha256-base64",
		SignatureHeader: HeaderHMAC,
		DedupeHeader:    HeaderWebhookID,
		Secret:          secret,
		SecretRef:       s.WebhookSecretRef,
	}, true
}

// Shopify's delivery headers. Named here rather than inline because three
// separate paths read them -- the receiver policy, the apply path and the
// compliance route -- and a typo in one is a silent mismatch in that one.
const (
	HeaderHMAC        = "X-Shopify-Hmac-Sha256"
	HeaderWebhookID   = "X-Shopify-Webhook-Id"
	HeaderEventID     = "X-Shopify-Event-Id"
	HeaderTopic       = "X-Shopify-Topic"
	HeaderShopDomain  = "X-Shopify-Shop-Domain"
	HeaderAPIVersion  = "X-Shopify-Api-Version"
	HeaderTriggeredAt = "X-Shopify-Triggered-At"
)

func sortedStores(in map[string]Store) []Store {
	out := make([]Store, 0, len(in))
	for _, s := range in {
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func storeFromRow(p map[string]any) (Store, bool) {
	if p == nil {
		return Store{}, false
	}
	s := Store{
		ID:                 shortID(mapString(p, "id")),
		Domain:             mapString(p, "domain"),
		Name:               mapString(p, "name"),
		AppClientID:        mapString(p, "appClientId"),
		AdminTokenRef:      mapString(p, "adminTokenRef"),
		StorefrontTokenRef: mapString(p, "storefrontTokenRef"),
		WebhookSecretRef:   mapString(p, "webhookSecretRef"),
		APIVersion:         mapString(p, "apiVersion"),
		ProtectedDataLevel: mapString(p, "protectedDataLevel"),
		Plan:               mapString(p, "plan"),
		Status:             mapString(p, "status"),
		OwnerUserID:        mapString(p, "ownerUserId"),
		RedactedAt:         mapString(p, "redactedAt"),
		ScopesGranted:      mapStringSlice(p, "scopesGranted"),
	}
	if h, ok := rowValue(p, "health").(map[string]any); ok {
		s.Health = h
	}
	if s.Domain == "" {
		return Store{}, false
	}
	if s.ProtectedDataLevel == "" {
		s.ProtectedDataLevel = ProtectedNone
	}
	return s, true
}

// shortID strips the canonical `{concept}:{shortId}` prefix. Store ids are
// composed into an inbound source segment and into every mirror row id, and
// both want the bare form -- the canonical id carries colons, which a URL
// path segment and an env-var suffix cannot.
func shortID(id string) string {
	if i := strings.LastIndex(id, ":"); i >= 0 {
		return id[i+1:]
	}
	return id
}

// mapString reads a string field off a materialised row.
//
// It probes the BARE key and then a nested `payload` map, because the two
// row shapes the engine returns differ and a reader that knew only one would
// be silently empty against the other. A shape-projected query answers with
// flat rows; a query with no shape clause falls back to the raw graph bundle,
// whose rows are node ENVELOPES with the concept's fields under `payload`.
// The generated mirror reads carry no shape (the default projection is the
// whole concept), so both shapes reach this function in practice --
// component/memql/seed_materializer.go's rowStringField makes the same probe
// for the same reason.
func mapString(m map[string]any, key string) string {
	if v, ok := rowValue(m, key).(string); ok {
		return strings.TrimSpace(v)
	}
	return ""
}

// rowValue is the shared bare-then-payload probe.
func rowValue(m map[string]any, key string) any {
	if m == nil {
		return nil
	}
	if v, ok := m[key]; ok && v != nil {
		return v
	}
	if payload, ok := m["payload"].(map[string]any); ok {
		return payload[key]
	}
	return nil
}

func mapStringSlice(m map[string]any, key string) []string {
	raw, ok := rowValue(m, key).([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(raw))
	for _, v := range raw {
		if s, ok := v.(string); ok && s != "" {
			out = append(out, s)
		}
	}
	return out
}

// jsonString renders a value as a MemQL string literal. Used everywhere the
// connector builds a call: json.Marshal is what guarantees the literal is
// valid whatever the origin returned, including a name with a quote in it.
func jsonString(s string) string {
	b, err := json.Marshal(s)
	if err != nil {
		return `""`
	}
	return string(b)
}

// mapInt reads an integer field off a materialised row, through the same
// bare-then-payload probe mapString makes.
//
// The narrowing is SATURATING rather than bare (CodeQL
// go/incorrect-integer-conversion). Both wider cases are reachable with a
// value int cannot hold: JSON decodes every number to float64, and a mirrored
// Shopify field is whatever that vendor's API said. A bare conversion wraps
// on a 32-bit build and is implementation-defined for an out-of-range float,
// so an inventory count could arrive negative with nothing anywhere to show
// it had been mangled. Clamping keeps the sign and the ordering, which is all
// a count or a limit is read for.
func mapInt(m map[string]any, key string) int {
	switch v := rowValue(m, key).(type) {
	case int:
		return v
	case int64:
		return clampInt64ToInt(v)
	case float64:
		return clampFloat64ToInt(v)
	}
	return 0
}

// clampInt64ToInt narrows an int64 to int without wrapping. Exact on a
// 64-bit build, where int is already 64 bits; saturating on a 32-bit one.
func clampInt64ToInt(v int64) int {
	if v > math.MaxInt {
		return math.MaxInt
	}
	if v < math.MinInt {
		return math.MinInt
	}
	return int(v)
}

// clampFloat64ToInt narrows a float64 to int without wrapping. NaN has no
// ordering and so no clamp: it becomes the same zero an absent field does.
func clampFloat64ToInt(v float64) int {
	switch {
	case math.IsNaN(v):
		return 0
	case v >= math.MaxInt:
		return math.MaxInt
	case v <= math.MinInt:
		return math.MinInt
	}
	return int(v)
}
