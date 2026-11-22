package vendorkeys

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	langparser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/worker"
)

// store.go -- the graph seam for a person's API keys (design record
// docs/superpowers/specs/2026-11-21-vendor-accounts-and-api-keys-design.md,
// decisions K1, K2 and K16, mesh rule M1). Every read and write is a NAMED
// construct rendered as MemQL text and handed to the engine, the shape
// integrations/siteaccess uses. Nothing here reaches the database.
//
// # Two rows, two writers
//
// v1:router:vendorKey holds what a key's owner decides; v1:router:vendorKeyState
// holds what the engine observed. Create, UpdateSettings, SetSharing and Remove
// write the first. observe.go writes the second, and nothing writes both, so a
// stamp the engine makes from whichever replica served a call can never put
// back a share or a removal its owner had just decided.
//
// # Who each call runs as
//
// EVERY READ RUNS AS THE CLUSTER ACTING: a synthetic actor with internal origin.
// Both concepts are unreadable by any client whatever the row tier says
// (clientUnreadableConcepts, component/memql), and the readers that matter are
// not the row's owner: the resolver finding a key somebody LENT to the person
// a call is for, the request unsealing that key, and the daily check, which
// has no person. So no read here authorizes anything. Who may see or change a
// key is decided by the callers of this file: the resolver for a call
// (resolver.go), the builtins for a person (integration.go).
//
// CREATE AND SET SHARING RUN AS THE KEY'S OWNER, with internal origin, because
// each stamps a fact from the actor: whose key it is, and who lent it. The
// other writes run as the cluster acting.
//
// # Every read is fresh
//
// Whether a key may pay is decided per call from the rows (M1, S9): a removal,
// a pause, a change of lending or a refusal written through another replica
// has to count on this one's next call. Each query is @cache(0), the concepts
// are off the result cache's default path, and every read here also carries
// ContextWithFreshRead, so no layer is trusted to have heard.
//
// # The sealed value
//
// The ciphertext is read by ONE function (sealed), through the one query that
// projects it, and held in a type that prints as nothing. Key carries
// everything else. No error leaving this file repeats a statement's text,
// because a create's statement contains the ciphertext.

const (
	keyConcept   = "v1:router:vendorKey"
	stateConcept = "v1:router:vendorKeyState"

	// systemActorName is what this package calls itself to auth.SystemActor.
	// It is what a row's createdBy says when the cluster wrote the version, so
	// an engine stamp is never mistaken for something a person did.
	systemActorName = "vendor-keys"

	statusActive  = "active"
	statusRemoved = "removed"

	// CheckedByList and CheckedByUse are how a key is checked (K12).
	CheckedByList = "list"
	CheckedByUse  = "use"

	// The three states the engine records about a key.
	StateGood    = "good"
	StateRefused = "refused"
	StateCredit  = "credit"
)

// Engine is the narrow engine surface this package needs.
type Engine interface {
	Execute(ctx context.Context, query string) (any, error)
}

// sealedValue holds a key's ciphertext, or for one request its plain value.
// It prints as nothing: a log line, an error or a %+v of a struct that holds
// one cannot carry it. reveal is the one deliberate read.
type sealedValue string

const sealedPlaceholder = "[sealed]"

func (sealedValue) String() string             { return sealedPlaceholder }
func (sealedValue) GoString() string           { return sealedPlaceholder }
func (sealedValue) Format(f fmt.State, _ rune) { _, _ = f.Write([]byte(sealedPlaceholder)) }
func (sealedValue) LogValue() slog.Value       { return slog.StringValue(sealedPlaceholder) }
func (sealedValue) MarshalText() ([]byte, error) {
	return []byte(sealedPlaceholder), nil
}
func (v sealedValue) reveal() string { return string(v) }

// Key is one v1:router:vendorKey without its sealed value. Its ids are BARE.
type Key struct {
	Id, OwnerUserId, Vendor, Label, Fingerprint, CheckedBy, Status string
	IsDefault, AutoUse                                             bool
	AddedAt, RemovedAt                                             time.Time
	RemovedBy                                                      string
	// Sharing is who the key is lent to. Its mode is owner or people: a key
	// cannot be lent to everyone, and a stored `cluster` reads as owner.
	Sharing worker.Sharing
}

// Active reports whether the key may still pay.
func (k Key) Active() bool { return k.Status == statusActive }

// State is one v1:router:vendorKeyState: what the engine observed about a key.
type State struct {
	KeyId, Vendor, State, Detail                                     string
	EvidenceAt, HeldUntil, CheckedAt, GoodAt, LastUsedAt, NotifiedAt time.Time
	// NoticeTriedAt is when the cluster last started telling the owner that
	// the key stopped working (notice.go).
	NoticeTriedAt time.Time
}

// NewKey is a key about to be stored, already checked and already sealed.
type NewKey struct {
	Id, Vendor, Label, Fingerprint, CheckedBy string
	IsDefault                                 bool
	Sealed                                    sealedValue
	// CheckedAt is when the check that let the key in started: the time of
	// the evidence its state is born with. Zero is the store's clock.
	CheckedAt time.Time
}

// Settings are the owner's settings on a key. A nil field is left as it was.
type Settings struct {
	Label              *string
	IsDefault, AutoUse *bool
}

// Store reads and writes API keys through the engine.
type Store struct {
	engine Engine
	// gate returns the pool write gates run on. Nil, or a nil pool, means this
	// node cannot coordinate a state write, and it writes none.
	gate   func() *sql.DB
	logger *slog.Logger
	now    func() time.Time
	// notify tells a key's owner when it stops working (notice.go). Installed
	// by app/, which is where a mail sender can be reached.
	notify notifierHolder
}

// NewStore wraps an engine. gate is PluginContext.GateDB.
func NewStore(engine Engine, gate func() *sql.DB, logger *slog.Logger) *Store {
	if logger == nil {
		logger = slog.Default()
	}
	return &Store{engine: engine, gate: gate, logger: logger, now: func() time.Time { return time.Now().UTC() }}
}

// asCluster is the context every read and the cluster's own writes run under:
// the cluster acting, with internal origin, reading the database and not a
// cache. The caller's forwarded authority stays behind, for
// integrations/siteaccess's reason: under a test persona it is an assertion a
// service actor cannot honour, and the engine would refuse the call.
func asCluster(ctx context.Context) context.Context {
	blank := auth.ContextWithForwardedAuthority(ctx, auth.ForwardedAuthority{})
	cluster := auth.ContextWithInternalOrigin(auth.ContextWithSystemActor(blank, systemActorName))
	return memql.ContextWithFreshRead(memql.ContextWithRootsOnlyRead(cluster))
}

// asOwner is the context the two writes that stamp the actor run under.
//
// A BLANK OWNER ANSWERS FALSE, because auth.ContextWithUserActor leaves a
// context unchanged for one: the row would belong to whoever the inbound
// caller happened to be.
func asOwner(ctx context.Context, owner string) (context.Context, bool) {
	owner = memql.BareShortId(owner)
	if owner == "" || auth.NamesNoPerson(owner) {
		return ctx, false
	}
	blank := auth.ContextWithForwardedAuthority(ctx, auth.ForwardedAuthority{})
	return auth.ContextWithInternalOrigin(auth.ContextWithUserActor(blank, owner)), true
}

// Key reads one key by id, removed ones included. A miss is (Key{}, false, nil).
func (s *Store) Key(ctx context.Context, keyId string) (Key, bool, error) {
	keyId = memql.BareShortId(keyId)
	if keyId == "" {
		return Key{}, false, nil
	}
	call, err := langparser.RenderCall("vendorKeyById", map[string]any{"keyId": keyId})
	rows, err := s.read(ctx, "the API key", call, err)
	if err != nil || len(rows) == 0 {
		return Key{}, false, err
	}
	return keyFromRow(rows[0]), true, nil
}

// ActiveKeysAtVendor reads every active key at one vendor, whoever owns it, in
// the order they were added (ties by id, so every replica agrees).
func (s *Store) ActiveKeysAtVendor(ctx context.Context, vendor string) ([]Key, error) {
	call, err := langparser.RenderCall("vendorKeysActiveAtVendor", map[string]any{"vendor": vendor})
	rows, err := s.readAll(ctx, "the active API keys at "+vendor, call, err)
	if err != nil {
		return nil, err
	}
	return keysFromRows(rows), nil
}

// ActiveKeysOfOwner reads one person's active keys, at both vendors, in the
// order they were added.
func (s *Store) ActiveKeysOfOwner(ctx context.Context, owner string) ([]Key, error) {
	owner = memql.BareShortId(owner)
	if owner == "" {
		return nil, nil
	}
	call, err := langparser.RenderCall("vendorKeysForOwner", map[string]any{"ownerUserId": owner})
	rows, err := s.read(ctx, "a person's API keys", call, err)
	if err != nil {
		return nil, err
	}
	return keysFromRows(rows), nil
}

// States reads what the engine observed about several keys, by bare key id. A
// key with no state row is absent from the map.
func (s *Store) States(ctx context.Context, keyIds []string) (map[string]State, error) {
	if len(keyIds) == 0 {
		return map[string]State{}, nil
	}
	out := make(map[string]State, len(keyIds))
	// In lots, so one statement never names more ids than a few hundred.
	for start := 0; start < len(keyIds); start += statesPerRead {
		lot := keyIds[start:min(start+statesPerRead, len(keyIds))]
		call, err := langparser.RenderCall("vendorKeyStatesForKeys", map[string]any{"keyIds": lot})
		rows, err := s.readAll(ctx, "the API keys' states", call, err)
		if err != nil {
			return nil, err
		}
		for _, row := range rows {
			state := stateFromRow(row)
			out[state.KeyId] = state
		}
	}
	return out, nil
}

// state reads one key's state row.
func (s *Store) state(ctx context.Context, keyId string) (State, bool, error) {
	call, err := langparser.RenderCall("vendorKeyStateById", map[string]any{"keyId": keyId})
	rows, err := s.read(ctx, "the API key's state", call, err)
	if err != nil || len(rows) == 0 {
		return State{}, false, err
	}
	return stateFromRow(rows[0]), true, nil
}

// sealed reads one key's ciphertext, for the one request it pays for. It is
// the only reader of the query that projects it.
func (s *Store) sealed(ctx context.Context, keyId string) (value sealedValue, vendor, status string, found bool, err error) {
	keyId = memql.BareShortId(keyId)
	if keyId == "" {
		return "", "", "", false, nil
	}
	call, err := langparser.RenderCall("vendorKeySealedById", map[string]any{"keyId": keyId})
	rows, err := s.read(ctx, "the API key", call, err)
	if err != nil || len(rows) == 0 {
		return "", "", "", false, err
	}
	row := rows[0]
	return sealedValue(rowString(row, "encryptedValue")), rowString(row, "vendor"), rowString(row, "status"), true, nil
}

// UserName is a person's display name, for a card, or "" when they are not an
// active user or could not be read. A name is decoration: nothing decides by it.
func (s *Store) UserName(ctx context.Context, userId string) string {
	userId = memql.BareShortId(userId)
	if userId == "" || auth.NamesNoPerson(userId) {
		return ""
	}
	call, err := langparser.RenderCall("userByIdSystem", map[string]any{"userId": userId})
	rows, err := s.read(ctx, "a key owner's name", call, err)
	if err != nil || len(rows) == 0 {
		return ""
	}
	return rowString(rows[0], "displayName")
}

// UserIsActive reports whether a person is an active user of this cluster. A
// key stops serving when its owner stops being one (K16).
func (s *Store) UserIsActive(ctx context.Context, userId string) (bool, error) {
	userId = memql.BareShortId(userId)
	if userId == "" || auth.NamesNoPerson(userId) {
		return false, nil
	}
	call, err := langparser.RenderCall("userByIdSystem", map[string]any{"userId": userId})
	rows, err := s.read(ctx, "the key's owner", call, err)
	if err != nil {
		return false, err
	}
	return len(rows) > 0, nil
}

// owner reads a person's name and address, as the cluster acting, for the one
// notice a key's owner is sent (notice.go). active is false for somebody who
// is no longer an active user of this cluster.
func (s *Store) owner(ctx context.Context, userId string) (name, email string, active bool, err error) {
	userId = memql.BareShortId(userId)
	if userId == "" || auth.NamesNoPerson(userId) {
		return "", "", false, nil
	}
	call, err := langparser.RenderCall("userByIdSystem", map[string]any{"userId": userId})
	rows, err := s.read(ctx, "a key owner's address", call, err)
	if err != nil || len(rows) == 0 {
		return "", "", false, err
	}
	return rowString(rows[0], "displayName"), rowString(rows[0], "primaryEmail"), true, nil
}

// Create stores a new key and starts the engine's record of it, as the owner.
//
// TWO WRITES, AND THE KEY FIRST. A key with no state row is a key nothing was
// observed about, which reads as usable and is corrected by its first call or
// check; a state row with no key would be a record of nothing. So a failure
// between the two leaves the harmless half.
func (s *Store) Create(ctx context.Context, owner string, key NewKey) error {
	owned, ok := asOwner(ctx, owner)
	if !ok {
		return errors.New("vendorkeys: an API key belongs to the person adding it, and nobody is")
	}
	id := memql.BareShortId(key.Id)
	if id == "" || key.Sealed.reveal() == "" {
		return errors.New("vendorkeys: an API key needs an id and a sealed value")
	}
	added := s.now().UTC()
	checked := key.CheckedAt.UTC()
	if key.CheckedAt.IsZero() {
		checked = added
	}
	at := checked.Format(time.RFC3339Nano)
	call, err := langparser.RenderCall("createVendorKey", map[string]any{
		"keyId": id, "vendor": key.Vendor, "label": key.Label,
		"encryptedValue": key.Sealed.reveal(), "fingerprint": key.Fingerprint,
		"isDefault": key.IsDefault, "checkedBy": key.CheckedBy, "addedAt": added.Format(time.RFC3339Nano),
	})
	if err := s.write(owned, "the API key", call, err); err != nil {
		return err
	}
	// A key whose models were listed is born GOOD, as of that check. A key
	// that may not list models (K12) is born UNCHECKED: the vendor said it is
	// a key and nothing more, and its first real call is its check.
	args := map[string]any{"keyId": id, "vendor": key.Vendor, "evidenceAt": at}
	if key.CheckedBy == CheckedByUse {
		args["state"] = StateUnchecked
	} else {
		args["state"], args["goodAt"], args["checkedAt"] = StateGood, at, at
	}
	call, err = langparser.RenderCall("createVendorKeyState", args)
	return s.write(owned, "the API key's state", call, err)
}

// UpdateSettings writes the owner's settings on a key. The caller has checked
// that it is the key's owner asking.
func (s *Store) UpdateSettings(ctx context.Context, keyId string, change Settings) error {
	args := map[string]any{"keyId": memql.BareShortId(keyId)}
	if change.Label != nil {
		args["label"] = *change.Label
	}
	if change.IsDefault != nil {
		args["isDefault"] = *change.IsDefault
	}
	if change.AutoUse != nil {
		args["autoUse"] = *change.AutoUse
	}
	if len(args) == 1 {
		return nil
	}
	call, err := langparser.RenderCall("updateVendorKey", args)
	return s.write(asCluster(ctx), "the API key's settings", call, err)
}

// SetSharing writes who a key is lent to, as its owner, so the row says who
// lent it. A cluster-wide share cannot be written: it is stored as owner.
func (s *Store) SetSharing(ctx context.Context, owner, keyId string, sharing worker.Sharing) error {
	owned, ok := asOwner(ctx, owner)
	if !ok {
		return errors.New("vendorkeys: only a key's owner lends it, and nobody is acting")
	}
	sharing = keySharing(sharing)
	args := map[string]any{
		"keyId": memql.BareShortId(keyId), "mode": sharing.Mode, "paused": sharing.Paused,
		"userIds": []string{}, "groupIds": []string{}, "pausedUserIds": []string{}, "pausedGroupIds": []string{},
	}
	if sharing.Mode == worker.SharingModePeople {
		args["userIds"] = nonNil(sharing.UserIds)
		args["groupIds"] = nonNil(sharing.GroupIds)
		args["pausedUserIds"] = nonNil(sharing.PausedUserIds)
		args["pausedGroupIds"] = nonNil(sharing.PausedGroupIds)
	}
	call, err := langparser.RenderCall("setVendorKeySharing", args)
	return s.write(owned, "who the API key is lent to", call, err)
}

// Remove ends a key: one-way, and the row stays (K16). removedBy is the
// authenticated person who asked, the owner or a cluster owner.
func (s *Store) Remove(ctx context.Context, keyId, removedBy string) error {
	call, err := langparser.RenderCall("removeVendorKey", map[string]any{
		"keyId": memql.BareShortId(keyId), "removedBy": memql.BareShortId(removedBy),
	})
	return s.write(asCluster(ctx), "the API key's removal", call, err)
}

// writeState writes fields of a key's state row, as the cluster acting. The
// caller holds the key's state gate (observe.go).
func (s *Store) writeState(ctx context.Context, keyId string, fields map[string]any) error {
	args := map[string]any{"keyId": keyId}
	for k, v := range fields {
		args[k] = v
	}
	call, err := langparser.RenderCall("updateVendorKeyState", args)
	return s.write(asCluster(ctx), "the API key's state", call, err)
}

func (s *Store) read(ctx context.Context, what, call string, rendered error) ([]map[string]any, error) {
	if rendered != nil {
		return nil, fmt.Errorf("vendorkeys: render the read of %s: %w", what, scrubbed(rendered))
	}
	if s == nil || s.engine == nil {
		return nil, errNoEngine
	}
	res, err := s.engine.Execute(asCluster(ctx), "query "+call)
	if err != nil {
		return nil, fmt.Errorf("vendorkeys: read %s: %w", what, scrubbed(err))
	}
	return memql.MaterializeRows(res), nil
}

// statesPerRead is how many keys' states one statement asks for.
const statesPerRead = 200

// maxListPages bounds one walk of a list read. At the engine's default page
// of 500 rows it is 100,000 rows, far past anything a cluster holds; a walk
// that would go further fails loudly instead of running on.
const maxListPages = 200

// readAll reads EVERY row a list read matches, a page at a time, as the
// cluster acting.
//
// One read of the engine returns a bounded page (its MaxResults, 500 by
// default) and says nothing when there were more rows than that. For the two
// reads that span owners that silence would be a wrong answer: a key past the
// first page would never be offered to the call it should pay for, and the
// daily check would never reach it. So those reads walk the pages
// (memql.WalkQueryPages), which either reaches the end or returns an error,
// and never a short list presented as the whole one.
//
// The walk takes no snapshot time. A snapshot would be this replica's clock,
// and a key removed a second ago on a replica whose clock runs ahead would
// still be read as active here. Without one, a row rewritten between two pages
// can be missed by that one walk, which for a key means it is not offered for
// one call: the narrowing direction. Below one page, which is every cluster
// today, neither can happen.
func (s *Store) readAll(ctx context.Context, what, call string, rendered error) ([]map[string]any, error) {
	if rendered != nil {
		return nil, fmt.Errorf("vendorkeys: render the read of %s: %w", what, scrubbed(rendered))
	}
	if s == nil || s.engine == nil {
		return nil, errNoEngine
	}
	execute := func(ctx context.Context, query string) (*memql.ExecuteResult, error) {
		res, err := s.engine.Execute(ctx, query)
		if err != nil {
			return nil, err
		}
		page, ok := res.(*memql.ExecuteResult)
		if !ok {
			return nil, fmt.Errorf("the engine answered a read with %T", res)
		}
		return page, nil
	}
	var rows []map[string]any
	seen := map[string]bool{}
	err := memql.WalkQueryPages(asCluster(ctx), execute, "query "+call, maxListPages, func(page *memql.ExecuteResult) error {
		for _, row := range memql.MaterializeRows(page) {
			// A row rewritten between two pages can appear on both.
			if id := rowString(row, "id"); id != "" {
				if seen[id] {
					continue
				}
				seen[id] = true
			}
			rows = append(rows, row)
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("vendorkeys: read %s: %w", what, scrubbed(err))
	}
	return rows, nil
}

// write runs one rendered mutation under the context its caller chose.
func (s *Store) write(ctx context.Context, what, call string, rendered error) error {
	if rendered != nil {
		return fmt.Errorf("vendorkeys: render the write of %s: %w", what, scrubbed(rendered))
	}
	if s == nil || s.engine == nil {
		return errNoEngine
	}
	if _, err := s.engine.Execute(ctx, "mutation "+call); err != nil {
		return fmt.Errorf("vendorkeys: write %s: %w", what, scrubbed(err))
	}
	return nil
}

var errNoEngine = errors.New("vendorkeys: no engine")

// ---------------------------------------------------------------------------
// keeping the sealed value out of errors
// ---------------------------------------------------------------------------

// sealedRun is what a sealed value looks like in text: a long run of base64.
// A create's statement carries one, and an engine error can repeat what it was
// given. An id is never caught: a short id is a UUID, whose longest run is 12.
var sealedRun = regexp.MustCompile(`[A-Za-z0-9+/=_-]{40,}`)

type scrubbedError struct {
	text  string
	cause error
}

func (e *scrubbedError) Error() string { return e.text }

// Is and As still see the cause, so a caller can tell a busy gate or a
// cancelled context from anything else without the cause's words escaping.
func (e *scrubbedError) Is(target error) bool { return errors.Is(e.cause, target) }

func scrubbed(err error) error {
	if err == nil {
		return nil
	}
	return &scrubbedError{text: sealedRun.ReplaceAllString(err.Error(), "[removed]"), cause: err}
}

// ---------------------------------------------------------------------------
// rows
// ---------------------------------------------------------------------------

func keysFromRows(rows []map[string]any) []Key {
	keys := make([]Key, 0, len(rows))
	for _, row := range rows {
		keys = append(keys, keyFromRow(row))
	}
	sort.SliceStable(keys, func(i, j int) bool {
		if !keys[i].AddedAt.Equal(keys[j].AddedAt) {
			return keys[i].AddedAt.Before(keys[j].AddedAt)
		}
		return keys[i].Id < keys[j].Id
	})
	return keys
}

func keyFromRow(row map[string]any) Key {
	return Key{
		Id:          memql.BareShortId(rowString(row, "id")),
		OwnerUserId: memql.BareShortId(rowString(row, "ownerUserId")),
		Vendor:      rowString(row, "vendor"),
		Label:       rowString(row, "label"),
		Fingerprint: rowString(row, "fingerprint"),
		CheckedBy:   rowString(row, "checkedBy"),
		Status:      rowString(row, "status"),
		IsDefault:   rowBool(row, "isDefault"),
		AutoUse:     rowBool(row, "autoUse"),
		AddedAt:     rowTime(row, "addedAt"),
		RemovedAt:   rowTime(row, "removedAt"),
		RemovedBy:   memql.BareShortId(rowString(row, "removedBy")),
		Sharing:     keySharing(worker.SharingFromRow(row["sharing"])),
	}
}

// keySharing narrows a machine's sharing to a key's: owner or people. A
// cluster-wide share does not exist for a key (a key is one person's money),
// so a row that says so anyway is read as lent to nobody.
func keySharing(s worker.Sharing) worker.Sharing {
	if s.Mode != worker.SharingModePeople {
		return worker.Sharing{Mode: worker.SharingModeOwner, SharedAt: s.SharedAt, SharedBy: s.SharedBy}
	}
	return s
}

func stateFromRow(row map[string]any) State {
	keyId := memql.BareShortId(rowString(row, "keyId"))
	if keyId == "" {
		keyId = memql.BareShortId(rowString(row, "id"))
	}
	return State{
		KeyId:      keyId,
		Vendor:     rowString(row, "vendor"),
		State:      rowString(row, "state"),
		Detail:     rowString(row, "detail"),
		EvidenceAt: rowTime(row, "evidenceAt"),
		HeldUntil:  rowTime(row, "heldUntil"),
		CheckedAt:  rowTime(row, "checkedAt"),
		GoodAt:     rowTime(row, "goodAt"),
		LastUsedAt: rowTime(row, "lastUsedAt"),
		NotifiedAt: rowTime(row, "notifiedAt"),

		NoticeTriedAt: rowTime(row, "noticeTriedAt"),
	}
}

func rowString(m map[string]any, key string) string {
	if v, ok := m[key].(string); ok {
		return strings.TrimSpace(v)
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

func nonNil(v []string) []string {
	if v == nil {
		return []string{}
	}
	return v
}

// callsPaidBy reads EVERY call the router recorded as paid by one key since a
// moment, as the cluster acting. It lives here, beside asCluster, because the
// read is server-only and the file that renders a server-only call is the file
// that stamps it (test/dslconformance's stamping gate reads one file at a time).
//
// Every page, because the activity is a sum: a busy key pays for more calls in
// a month than one read returns, and the first page added up would be an
// undercount shown as the total. Past maxListPages (100,000 calls at the
// default page) the read fails instead, and the activity says it did not
// complete: a wrong total is worse than none.
func (s *Store) callsPaidBy(ctx context.Context, keyId string, since time.Time) ([]map[string]any, error) {
	call, err := langparser.RenderCall("routerCallsPaidByKey", map[string]any{
		"keyId": keyId, "since": since.UTC().Format(time.RFC3339),
	})
	return s.readAll(ctx, "the calls an API key paid for", call, err)
}
