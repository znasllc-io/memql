package calls

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	langparser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/secret"
	"github.com/znasllc-io/memql/core/num"
)

// store.go -- the graph seam. Every read and write is a NAMED construct rendered as
// MemQL text and handed to the engine, as integrations/siteaccess's store does.
// Nothing here reaches the database directly.
//
// # Which actor, for which rows
//
// Every v1:calls concept is @serverWritten and client-unreadable
// (component/memql site_access_guards.go), so no write lands and no read answers
// without INTERNAL ORIGIN, whatever actor is on the context. The actor then decides,
// and there are three:
//
//   - A SITE'S ROWS (a key here, the site's limits in policy.go, its sessions, rooms,
//     seats and queue entries in rooms.go, its bookings in schedule.go) are read and
//     written AS THE SITE'S OWNER,
//     with internal origin (asOwner). A create stamps ownerUserId from the actor, so a
//     row is the site's owner's at birth and never the person who asked. Borrowed
//     authority without internal origin is a CLIENT read of these concepts
//     (site_access_guards.go), which is why the stamp is here. Reconciliation
//     (reconcile.go) has no session and no site handed to it: it finds the open rooms
//     through the media servers' load, as the cluster acting, and then reads and
//     writes each room's rows as THAT ROOM'S site owner, the owner the room row names.
//   - THE CLUSTER'S ROWS (media servers) and the sealed secrets are read and written
//     AS THE CLUSTER ACTING: the system actor `system:calls`, a synthetic owner-role
//     actor, with internal origin (asCluster). Their queries require
//     actor.isClusterOwner, so a site owner's borrowed authority would read nothing.
//     A media server's LOAD is read the same way (rooms.go loadOn): the calls of every
//     deployable placed on the server share its seats, so it is counted across all of
//     them, and nothing read that way is returned. So are a site's custom domains
//     (schedule.go publicHost), which are cluster-owned rows a site's owner could not
//     read; only the live hostname a booked call's links are built on leaves.
//   - THE SITE ITSELF is read and written UNDER THE CALLER, unstamped, by the operator
//     acts, and that is what makes those two calls checks rather than lookups: a
//     person who cannot read the deployable resolves zero rows, and the switch's write
//     meets every gate a write of theirs meets (the part, the organization and the
//     row's own tier). The calls service reads it only to learn the hostname a booked
//     call's links are built on, as the site's owner (schedule.go publicHost): a
//     lookup, not a check, since whoever it answers holds a key or a link and has no
//     MemQL identity to check.
//
// # No secret in an error
//
// A statement here may carry a digest, and the engine's errors can repeat what they
// were given, so every engine error leaves through `scrubbed`. A sealed secret's
// write is quieter still: its statement carries ciphertext and the plaintext's last
// four characters, and its errors carry neither (sealSecret).

const (
	keyConcept          = "v1:calls:key"
	mediaServerConcept  = "v1:calls:mediaServer"
	globalSecretConcept = "v1:platform:globalSecret"
	siteConcept         = "v1:platform:site"
)

// systemActorName is what this package calls itself to auth.SystemActor: what a
// row's createdBy says when the cluster's own work wrote it.
const systemActorName = "calls"

// The sealed secrets, by NAME. A media server row names its credential's secret and
// never holds it (mediaServer.credentialSecret), and the names are what the
// resolver and every later reader ask for.
const (
	// SecretMediaBootstrap holds the bootstrap server's API key and secret, as a
	// MediaCredential's JSON.
	SecretMediaBootstrap = "calls.media.bootstrap"
	// SecretMediaWebhook holds the key pair the media server signs its webhooks
	// with, as a MediaCredential's JSON: the key is the token's issuer and the
	// secret its HMAC key.
	SecretMediaWebhook = "calls.media.webhook"
	// SecretKeyPepper holds 32 random bytes, hex-encoded, that every calls key is
	// digested under (calls.Digest). It is generated once, by the first boot that
	// finds none, and NEVER REGENERATED: a new pepper would orphan every key's
	// digest, so no key anybody holds would open anything again.
	SecretKeyPepper = "calls.key-pepper"
)

// Engine is the narrow engine surface this package needs.
type Engine interface {
	Execute(ctx context.Context, query string) (any, error)
}

// Store reads and writes the calls rows through the engine.
type Store struct {
	engine  Engine
	secrets func(ctx context.Context, name string) (string, error)
}

// NewStore wraps an engine and the engine's system secret resolver
// (PluginContext.ResolveSystemSecret).
func NewStore(engine Engine, resolveSystemSecret func(ctx context.Context, name string) (string, error)) *Store {
	return &Store{engine: engine, secrets: resolveSystemSecret}
}

// asCluster is the context the cluster's own reads and writes run under: the
// system actor, with internal origin, as integrations/siteaccess's asCluster is.
//
// THE CALLER'S FORWARDED AUTHORITY STAYS BEHIND: under a test persona it is an
// assertion the system actor cannot honour, and the engine would refuse the call as an
// actor who differs from its session. The persona is asked here instead, because
// nothing after this will ask it.
func asCluster(ctx context.Context) (context.Context, error) {
	if err := auth.ValidateRoleTestContext(ctx); err != nil {
		return nil, fmt.Errorf("calls: the test persona behind this call: %w", err)
	}
	blank := auth.ContextWithForwardedAuthority(ctx, auth.ForwardedAuthority{})
	return auth.ContextWithInternalOrigin(auth.ContextWithSystemActor(blank, systemActorName)), nil
}

// asOwner is the context a site's rows are read and written under: the site's owner,
// with internal origin. A BLANK OWNER ANSWERS FALSE, because auth.ContextWithUserActor
// leaves a context unchanged for one, and the row would belong to whoever happened to
// be calling.
func asOwner(ctx context.Context, owner string) (context.Context, bool) {
	owner = memql.BareShortId(strings.TrimSpace(owner))
	if owner == "" {
		return ctx, false
	}
	return auth.ContextWithInternalOrigin(auth.ContextWithUserActor(ctx, owner)), true
}

// errNoOwner is a site row write with no owner to write as.
var errNoOwner = errors.New("calls: a deployable's calls rows belong to its owner, and this deployable has none")

// ---------------------------------------------------------------------------
// keys
// ---------------------------------------------------------------------------

// Key is one v1:calls:key as the calls integration needs it. Its ids are BARE. The
// digest is not here: it is filtered on and never read back.
type Key struct {
	ID, SiteID, OwnerUserID, AccountID, Label, Kind, Status string
	Version                                                 time.Time
}

// newKey is what a mint writes. The digest is all that is left of the key by the time
// it gets here: no shape of this call can store the key itself.
type newKey struct {
	ID, SiteID, Digest, Label, Kind string
}

// createKey writes a minted key as the site's owner, at an id nobody holds yet.
func (s *Store) createKey(ctx context.Context, owner string, k newKey, versionTime time.Time) error {
	owned, ok := asOwner(ctx, owner)
	if !ok {
		return errNoOwner
	}
	args := map[string]any{
		"keyId":       k.ID,
		"siteId":      k.SiteID,
		"digest":      k.Digest,
		"kind":        k.Kind,
		"versionTime": versionStamp(versionTime),
	}
	// Absent rather than empty: a key nobody named has no name.
	if k.Label != "" {
		args["label"] = k.Label
	}
	call, err := langparser.RenderCall("createCallsKey", args)
	return s.write(witnessedAbsent(owned, keyConcept, k.ID), "the key", call, err)
}

// KeyByDigest finds a key by the digest of what its holder presented, on one site,
// as the site's owner. A miss is (nil, nil), and that covers a key of another site.
// A frozen or banned key still answers: which of those means what is the caller's.
func (s *Store) KeyByDigest(ctx context.Context, owner, siteID, digest string) (*Key, error) {
	owned, ok := asOwner(ctx, owner)
	if !ok {
		return nil, errNoOwner
	}
	call, err := langparser.RenderCall("callsKeyByDigest", map[string]any{"siteId": memql.BareShortId(siteID), "digest": digest})
	rows, err := s.read(memql.ContextWithRootsOnlyRead(owned), "a key by its digest", call, err)
	if err != nil || len(rows) == 0 {
		return nil, err
	}
	r := rows[0]
	return &Key{
		ID:          memql.BareShortId(rowString(r, "id")),
		SiteID:      memql.BareShortId(rowString(r, "siteId")),
		OwnerUserID: memql.BareShortId(rowString(r, "ownerUserId")),
		AccountID:   memql.BareShortId(rowString(r, "accountId")),
		Label:       rowString(r, "label"),
		Kind:        rowString(r, "kind"),
		Status:      rowString(r, "status"),
		Version:     rowTime(r, "createdAt"),
	}, nil
}

// ---------------------------------------------------------------------------
// media servers
// ---------------------------------------------------------------------------

// MediaServer is one v1:calls:mediaServer. CredentialSecret is the NAME of the sealed
// secret holding its API credential, never the credential.
type MediaServer struct {
	ID, Provider, APIURL, PublicURL, Region, State, CredentialSecret string
	Seats                                                            int
	Version                                                          time.Time
}

// sameTarget reports whether two rows describe the same server the same way, which is
// what the bootstrap compares the environment against. State and region are not the
// environment's: an operator's drain is kept whatever the environment says.
func (m MediaServer) sameTarget(o MediaServer) bool {
	return m.Provider == o.Provider && m.APIURL == o.APIURL && m.PublicURL == o.PublicURL &&
		m.Seats == o.Seats && m.CredentialSecret == o.CredentialSecret
}

// MediaServerByID reads one media server as the cluster acting. A miss is (nil, nil).
func (s *Store) MediaServerByID(ctx context.Context, id string) (*MediaServer, error) {
	call, err := langparser.RenderCall("callsMediaServerById", map[string]any{"id": memql.BareShortId(id)})
	rows, err := s.readAsCluster(ctx, "the media server", call, err)
	if err != nil || len(rows) == 0 {
		return nil, err
	}
	server := mediaServerFromRow(rows[0])
	return &server, nil
}

// ActiveMediaServers is every media server rooms may be placed on, as the cluster
// acting.
func (s *Store) ActiveMediaServers(ctx context.Context) ([]MediaServer, error) {
	call, err := langparser.RenderCall("callsActiveMediaServers", nil)
	rows, err := s.readAsCluster(ctx, "the active media servers", call, err)
	if err != nil {
		return nil, err
	}
	out := make([]MediaServer, 0, len(rows))
	for _, r := range rows {
		out = append(out, mediaServerFromRow(r))
	}
	return out, nil
}

// MediaServersNotRemoved is every media server that may still carry calls -- active or
// draining -- as the cluster acting: what reconciliation walks (reconcile.go).
func (s *Store) MediaServersNotRemoved(ctx context.Context) ([]MediaServer, error) {
	call, err := langparser.RenderCall("callsMediaServersNotRemoved", nil)
	rows, err := s.readAsCluster(ctx, "the media servers not removed", call, err)
	if err != nil {
		return nil, err
	}
	out := make([]MediaServer, 0, len(rows))
	for _, r := range rows {
		out = append(out, mediaServerFromRow(r))
	}
	return out, nil
}

func mediaServerFromRow(r map[string]any) MediaServer {
	state := rowString(r, "state")
	if state == "" {
		// The concept's own rule: absent reads as active.
		state = "active"
	}
	return MediaServer{
		ID:               memql.BareShortId(rowString(r, "id")),
		Provider:         rowString(r, "provider"),
		APIURL:           rowString(r, "apiUrl"),
		PublicURL:        rowString(r, "publicUrl"),
		Region:           rowString(r, "region"),
		State:            state,
		CredentialSecret: rowString(r, "credentialSecret"),
		Seats:            rowInt(r, "seats"),
		Version:          rowTime(r, "createdAt"),
	}
}

// createMediaServer writes a new media server as the cluster acting, at an id nobody
// holds yet: a row that appeared since the caller read refuses the write
// (memql.ErrRowVersionChanged) rather than landing on it as a second version.
func (s *Store) createMediaServer(ctx context.Context, m MediaServer, versionTime time.Time) error {
	cluster, err := asCluster(ctx)
	if err != nil {
		return err
	}
	args := map[string]any{
		"mediaServerId":    m.ID,
		"provider":         m.Provider,
		"apiUrl":           m.APIURL,
		"publicUrl":        m.PublicURL,
		"seats":            m.Seats,
		"credentialSecret": m.CredentialSecret,
		"versionTime":      versionStamp(versionTime),
	}
	if m.Region != "" {
		args["region"] = m.Region
	}
	call, err := langparser.RenderCall("createCallsMediaServer", args)
	return s.write(witnessedAbsent(cluster, mediaServerConcept, m.ID), "the media server", call, err)
}

// updateMediaServer writes what the environment says about a server it already
// recorded, against the version it was decided from. The state is not written: a
// drain is the operator's, and the read-merge keeps it.
func (s *Store) updateMediaServer(ctx context.Context, m MediaServer, priorVersion, versionTime time.Time) error {
	cluster, err := asCluster(ctx)
	if err != nil {
		return err
	}
	fenced, err := witnessed(cluster, mediaServerConcept, m.ID, priorVersion)
	if err != nil {
		return err
	}
	call, err := langparser.RenderCall("updateCallsMediaServer", map[string]any{
		"mediaServerId":    m.ID,
		"provider":         m.Provider,
		"apiUrl":           m.APIURL,
		"publicUrl":        m.PublicURL,
		"seats":            m.Seats,
		"credentialSecret": m.CredentialSecret,
		"versionTime":      versionStamp(versionTime),
	})
	return s.write(fenced, "the media server", call, err)
}

// ---------------------------------------------------------------------------
// sealed secrets
// ---------------------------------------------------------------------------

// MediaCredential is a key pair a media server signs with: the API key (the token's
// issuer, not secret) and the secret it signs with. It is sealed as JSON in one
// global secret, so a reader gets both halves from one lookup and they cannot drift.
type MediaCredential struct {
	Key    string `json:"key"`
	Secret string `json:"secret"`
}

// String prints the key and never the secret; GoString and LogValue say the same, so
// %#v and slog cannot print it either.
func (c MediaCredential) String() string       { return "media credential " + c.Key }
func (c MediaCredential) GoString() string     { return c.String() }
func (c MediaCredential) LogValue() slog.Value { return slog.StringValue(c.String()) }

func (c MediaCredential) seal() (string, error) {
	raw, err := json.Marshal(c)
	if err != nil {
		return "", fmt.Errorf("calls: encode the media credential: %w", err)
	}
	return string(raw), nil
}

// ParseMediaCredential reads what a media credential secret holds. Both halves are
// required: half a credential signs nothing anybody can verify. The error never
// repeats the value.
func ParseMediaCredential(plaintext string) (MediaCredential, error) {
	var c MediaCredential
	if err := json.Unmarshal([]byte(plaintext), &c); err != nil {
		return MediaCredential{}, errors.New("calls: a media credential secret does not hold a key and a secret")
	}
	if strings.TrimSpace(c.Key) == "" || strings.TrimSpace(c.Secret) == "" {
		return MediaCredential{}, errors.New("calls: a media credential secret is missing its key or its secret")
	}
	return c, nil
}

// resolveSecret reads a sealed secret through the engine's resolver, answered by the
// database: the bootstrap compares what is stored with what it would write, and a
// cached miss would have it write a second pepper.
func (s *Store) resolveSecret(ctx context.Context, name string) (string, error) {
	if s == nil || s.secrets == nil {
		return "", errors.New("calls: no secret resolver")
	}
	return s.secrets(memql.ContextWithFreshRead(ctx), name)
}

// secretRowID is the id to write a sealed secret at: the one row already carrying the
// name, or the seeder's derivation (secret-global-<name>) when none does. Two rows
// carrying one name refuse: the resolver reads the first row by name, so a write beside
// another would change nothing anybody reads, or change it only sometimes.
func (s *Store) secretRowID(ctx context.Context, name string) (string, error) {
	query := fmt.Sprintf(`concept==%s&&payload.name==%s`, globalSecretConcept, langparser.QuoteString(name))
	res, err := s.engine.Execute(ctx, query)
	if err != nil {
		return "", quietly("calls: look up the sealed secret "+name, err)
	}
	ids := map[string]bool{}
	for _, row := range memql.MaterializeRows(res) {
		if id := memql.BareShortId(rowString(row, "id")); id != "" {
			ids[id] = true
		}
	}
	switch len(ids) {
	case 0:
		return "secret-global-" + strings.ToLower(strings.ReplaceAll(name, "_", "-")), nil
	case 1:
		for id := range ids {
			return id, nil
		}
	}
	return "", fmt.Errorf("calls: more than one row carries the sealed secret %s, so no write could say which one it changed", name)
}

// sealSecret seals a value under MEMQL_MASTER_KEY and writes it as the named global
// secret, as the cluster acting. The plaintext goes no further than secret.Encrypt:
// not into a log, not into an error, and not into the statement, which carries the
// ciphertext and the four-character fingerprint every sealed secret carries.
func (s *Store) sealSecret(ctx context.Context, name, plaintext, description string) error {
	cluster, err := asCluster(ctx)
	if err != nil {
		return err
	}
	rowID, err := s.secretRowID(memql.ContextWithFreshRead(cluster), name)
	if err != nil {
		return err
	}
	ciphertext, fingerprint, err := secret.Encrypt(plaintext)
	if err != nil {
		// secret.Encrypt's errors are about the master key, never the value.
		return fmt.Errorf("calls: seal %s: %w", name, err)
	}
	call, err := langparser.RenderCall("setGlobalSecret", map[string]any{
		"id":             rowID,
		"name":           name,
		"encryptedValue": ciphertext,
		"fingerprint":    fingerprint,
		"kind":           "integration",
		"description":    description,
		"addedBy":        "system:" + systemActorName,
		"active":         true,
	})
	if err != nil {
		return quietly("calls: render the write of the sealed secret "+name, err)
	}
	if _, err := s.engine.Execute(cluster, "mutation "+call); err != nil {
		return quietly("calls: write the sealed secret "+name, err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// the deployable
// ---------------------------------------------------------------------------

// siteView is the projection of v1:platform:site an operator's act is judged by. Its
// ids are BARE.
type siteView struct {
	ID, OwnerUserID, AccountID, DeployableName string
	SystemOwned                                bool
}

// siteForCaller resolves a deployable UNDER THE CALLER, unstamped: the read that IS the
// check. A miss is (nil, nil), and a site this caller cannot read and one that does not
// exist are the same answer, because telling them apart would tell a caller which ids
// exist. Fresh, because a move of the site's organization or owner counts from the
// next request.
func (s *Store) siteForCaller(ctx context.Context, siteID string) (*siteView, error) {
	call, err := langparser.RenderCall("siteById", map[string]any{"siteId": siteID})
	rows, err := s.read(memql.ContextWithFreshRead(ctx), "the deployable", call, err)
	if err != nil || len(rows) == 0 {
		return nil, err
	}
	r := rows[0]
	return &siteView{
		ID:             memql.BareShortId(rowString(r, "id")),
		OwnerUserID:    memql.BareShortId(rowString(r, "ownerUserId")),
		AccountID:      memql.BareShortId(rowString(r, "accountId")),
		DeployableName: strings.TrimSpace(rowString(r, "packageDeployableName")),
		SystemOwned:    rowBool(r, "systemOwned"),
	}, nil
}

// setSiteCalls writes the deployable's calls switch UNDER THE CALLER, unstamped, so the
// engine judges it as any write of theirs: updateSiteCalls' own capability gate and
// the organization gate behind it, the site's row tier, and the organization's calls
// part for the change (validateOrganizationSensitiveChanges). Nothing here widens it.
func (s *Store) setSiteCalls(ctx context.Context, siteID string, on bool) error {
	call, err := langparser.RenderCall("updateSiteCalls", map[string]any{"siteId": siteID, "calls": on})
	return s.write(ctx, "the deployable's calls switch", call, err)
}

// ---------------------------------------------------------------------------
// reading and writing
// ---------------------------------------------------------------------------

// readAsCluster is one of the cluster's own reads: the rows and nothing else (every
// shape these name projects the row's own fields), answered by the database, because a
// bootstrap and a policy write each decide from what they read.
func (s *Store) readAsCluster(ctx context.Context, what, call string, rendered error) ([]map[string]any, error) {
	cluster, err := asCluster(ctx)
	if err != nil {
		return nil, err
	}
	return s.read(memql.ContextWithRootsOnlyRead(memql.ContextWithFreshRead(cluster)), what, call, rendered)
}

// read runs one rendered query. Every read and write here leaves through this or
// write, which is what makes "no digest in an error" a property of the file.
func (s *Store) read(ctx context.Context, what, call string, rendered error) ([]map[string]any, error) {
	if rendered != nil {
		return nil, fmt.Errorf("calls: render the read of %s: %w", what, scrubbed(rendered))
	}
	if s == nil || s.engine == nil {
		return nil, errNoEngine
	}
	res, err := s.engine.Execute(ctx, "query "+call)
	if err != nil {
		return nil, fmt.Errorf("calls: read %s: %w", what, scrubbed(err))
	}
	return memql.MaterializeRows(res), nil
}

// write runs one rendered mutation, for read's reason.
func (s *Store) write(ctx context.Context, what, call string, rendered error) error {
	if rendered != nil {
		return fmt.Errorf("calls: render the write of %s: %w", what, scrubbed(rendered))
	}
	if s == nil || s.engine == nil {
		return errNoEngine
	}
	if _, err := s.engine.Execute(ctx, "mutation "+call); err != nil {
		return fmt.Errorf("calls: write %s: %w", what, scrubbed(err))
	}
	return nil
}

var errNoEngine = errors.New("calls: no engine")

// witnessed adds to a write's context the version of the row it was decided from. A
// missing version fails here rather than becoming a zero witness, which means "the row
// must be absent" and would refuse every write as a conflict.
func witnessed(ctx context.Context, concept, rowID string, version time.Time) (context.Context, error) {
	if version.IsZero() {
		return nil, fmt.Errorf("calls: the read of %s carried no version to write against", concept)
	}
	return memql.ContextWithRowVersionFence(ctx, concept, rowID, version), nil
}

// witnessedAbsent adds to a create's context the requirement that its row not exist
// yet: the engine looks again inside the write's transaction, so a row that appeared
// since the read refuses the create rather than receiving it as a second version.
func witnessedAbsent(ctx context.Context, concept, rowID string) context.Context {
	return memql.ContextWithRowVersionFence(ctx, concept, rowID, time.Time{})
}

// versionStamp is how a write's version is passed: the row's createdAt is
// `args.versionTime ?? now` on every calls mutation, and PostgreSQL keeps microseconds.
func versionStamp(at time.Time) string {
	return at.UTC().Truncate(time.Microsecond).Format(time.RFC3339Nano)
}

// ---------------------------------------------------------------------------
// keeping secrets out of errors
// ---------------------------------------------------------------------------

// digestRun is what a stored digest, or the front of one, looks like in text: a long
// run of lowercase hex. An id never matches: a short id is a UUID, whose longest run
// is 12.
var digestRun = regexp.MustCompile(`[0-9a-f]{16,}`)

// scrubbedError is an engine error with any digest taken out of its words. It keeps
// the error's identity, so errors.Is still finds a lost fence behind it.
type scrubbedError struct {
	text  string
	cause error
}

func (e *scrubbedError) Error() string { return e.text }
func (e *scrubbedError) Unwrap() error { return e.cause }

func scrubbed(err error) error {
	if err == nil {
		return nil
	}
	text := err.Error()
	clean := digestRun.ReplaceAllString(text, "[digest]")
	if clean == text {
		return err
	}
	return &scrubbedError{text: clean, cause: err}
}

// quietly is an error that keeps its cause for errors.Is and says nothing of it: the
// cause may repeat a statement carrying a sealed secret's ciphertext and fingerprint,
// and the words are what reach a log.
func quietly(what string, cause error) error {
	return &scrubbedError{text: what + " failed", cause: cause}
}

// ---------------------------------------------------------------------------
// reading rows
// ---------------------------------------------------------------------------

// rowTime reads a stamp, answering the zero time for anything else. A shaped
// `row.createdAt` arrives as a time.Time and a stored datetime as text.
func rowTime(m map[string]any, key string) time.Time {
	switch v := m[key].(type) {
	case time.Time:
		return v.UTC()
	case string:
		at, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(v))
		if err != nil {
			return time.Time{}
		}
		return at.UTC()
	default:
		return time.Time{}
	}
}

func rowString(m map[string]any, key string) string {
	if v, ok := m[key].(string); ok {
		return v
	}
	return ""
}

func rowBool(m map[string]any, key string) bool {
	switch v := m[key].(type) {
	case bool:
		return v
	case string:
		return v == "true"
	default:
		return false
	}
}

// rowInt reads a stored integer, whichever way the read carried it, zero for an absent
// or unreadable one.
func rowInt(m map[string]any, key string) int {
	v, _ := rowIntPresent(m, key)
	return v
}

// rowIntPresent reads a stored integer and says whether one was there: an absent
// policy value means the shipped default, which a zero would not. A value that does
// not fit, or is not a whole number, is treated as absent rather than saturated.
func rowIntPresent(m map[string]any, key string) (int, bool) {
	switch v := m[key].(type) {
	case int:
		return v, true
	case int32:
		return int(v), true
	case int64:
		n := num.Int64OrZero(v)
		return n, n != 0 || v == 0
	case float64:
		whole, ok := num.WholeInt64(v)
		if !ok {
			return 0, false
		}
		n := num.Int64OrZero(whole)
		return n, n != 0 || whole == 0
	case json.Number:
		n, err := strconv.Atoi(v.String())
		return n, err == nil
	case string:
		n, err := strconv.Atoi(strings.TrimSpace(v))
		return n, err == nil
	default:
		return 0, false
	}
}
