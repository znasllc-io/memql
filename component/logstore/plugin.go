package logstore

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/uptrace/bun"

	"github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/core/num"
	"github.com/znasllc-io/memql/integrations/azureblob"
)

// plugin.go -- the `logs` plug-in: the executors behind the eight builtins
// in dsl/observability/builtins.memql (integration.logs.<name>).
//
// It lives in component/, in the ROOT module, like component/packages: it is
// engine internals an operator cannot switch off -- the builtins are
// declared in the core DSL tree every binary loads, and a capability present
// in the DSL and absent from the registry is a boot-time resolution failure.
// Registered on every node type for that reason.
//
// ACCESS IS A ROLE FLOOR IN EACH HANDLER (design L3), the integrationConfigure
// precedent: a builtin's annotation set is closed and carries no
// @requiresRank, and the concept declares no row tier because no row of it
// ever passes graph admission. Reads are admin and above -- owner,
// developer, admin under the one ladder, where developer OUTRANKS admin --
// so the check is a rank comparison, never a string equality that would
// forget one rung. sweep and archiveRestore are owner only. recordClient
// admits any signed-in principal and nothing anonymous or connector.

// integrationName is the plug-in name. Spelled as a STRING LITERAL in
// RegisterPlugin below, not as this constant: the taxonomy gate
// (module_taxonomy_test.go) finds every registration by scanning source for
// the literal.
const integrationName = "logs"

// Reply concepts. They sit outside the v{major}:{domain}:{entity} grammar
// deliberately, the way integration:email:configured does: a reply is not a
// row. Only search and tail answer with real v1:observability:logLine nodes.
const (
	conceptSource      = "integration:logs:source"
	conceptStatus      = "integration:logs:status"
	conceptRecorded    = "integration:logs:recorded"
	conceptSweep       = "integration:logs:sweep"
	conceptArchive     = "integration:logs:archiveObject"
	conceptArchiveNone = "integration:logs:archiveNone"
	conceptRestore     = "integration:logs:restore"
)

// Integration binds the store to the engine.
type Integration struct {
	logger    *slog.Logger
	db        func() *bun.DB
	directDB  func() *bun.DB
	archiver  Archiver
	container string
	limiter   *ClientLimiter
	now       func() time.Time
	sink      func() *Sink
}

func init() {
	memql.RegisterPlugin("logs", func(pctx memql.PluginContext) (memql.IntegrationProvider, error) {
		return NewIntegration(pctx.Logger, pctx.BunDB, pctx.DirectBunDB), nil
	})
}

// NewIntegration wires the production integration. The archive client is
// built only when a container is configured, and its absence is a refusal
// at sweep time rather than a factory error: a cluster with no object
// storage keeps its lines, which is the design.
func NewIntegration(log *slog.Logger, db, directDB func() *bun.DB) *Integration {
	if log == nil {
		log = slog.Default()
	}
	i := &Integration{
		// The bare app logger: the sweep's one line stamps component `logs`
		// itself, so binding it here too would print the key twice.
		logger:    log,
		db:        db,
		directDB:  directDB,
		container: ArchiveContainer(),
		limiter:   NewClientLimiter(),
		now:       func() time.Time { return time.Now().UTC() },
		sink:      Current,
	}
	if i.container != "" {
		up, err := azureblob.New(context.Background())
		if err != nil {
			log.With("component", storeComponent).Warn("log store: archive container is configured but no blob client could be built; the sweep will refuse to delete",
				"container", i.container, "err", err)
		} else {
			i.archiver = up
		}
	}
	return i
}

// IntegrationName implements memql.IntegrationProvider.
func (i *Integration) IntegrationName() string { return integrationName }

// Capabilities implements memql.IntegrationProvider.
func (i *Integration) Capabilities() []memql.IntegrationCapability {
	facets := map[string]string{
		"nodeTypes":       "[]string -- node types to include",
		"nodes":           "[]string -- node ids to include",
		"components":      "[]string -- component names, exact",
		"apps":            "[]string -- OS app ids; ORed with subjectConcepts into the scope predicate",
		"levels":          "[]string -- debug / info / warn / error",
		"subject":         "string -- only lines about this bare id",
		"subjectConcept":  "string -- only lines whose subject is of this concept",
		"subjectConcepts": "[]string -- concept ids an app owns; ORed with apps",
		"session":         "string -- only lines from this OS tab session",
		"userId":          "string -- only OS lines written by this user",
		"text":            "string -- case-insensitive substring of the message",
		"limit":           "int -- 1 to 500, default 200",
	}
	search := map[string]string{"windowStart": "datetime (required)", "windowEnd": "datetime (required)", "beforeAt": "datetime -- keyset cursor", "beforeId": "string -- keyset cursor"}
	tail := map[string]string{"afterAt": "datetime -- keyset cursor", "afterId": "string -- keyset cursor"}
	for k, v := range facets {
		search[k] = v
		tail[k] = v
	}
	return []memql.IntegrationCapability{
		{
			Name:          "search",
			Description:   "Lines inside [windowStart, windowEnd), newest first, narrowed by every facet; apps and subjectConcepts form one ORed scope predicate. Keyset-paged by beforeAt + beforeId. Admin and above.",
			Handler:       i.handleSearch,
			ArgsSchema:    search,
			PreserveOrder: true,
		},
		{
			Name:          "tail",
			Description:   "Lines newer than afterAt + afterId, oldest first; with no cursor the newest limit lines ascending. Admin and above.",
			Handler:       i.handleTail,
			ArgsSchema:    tail,
			PreserveOrder: true,
		},
		{
			Name:          "sources",
			Description:   "One row per distinct component, per (nodeType, node) and per OS app inside [windowStart, windowEnd), with counts. Admin and above.",
			Handler:       i.handleSources,
			ArgsSchema:    map[string]string{"windowStart": "datetime (required)", "windowEnd": "datetime (required)"},
			PreserveOrder: true,
		},
		{
			Name:        "status",
			Description: "What this cluster keeps: retention, the store level and rate on this node, the archive, the counters, the store's bounds and size estimate. One row. Admin and above.",
			Handler:     i.handleStatus,
			ArgsSchema:  map[string]string{},
		},
		{
			Name:        "recordClient",
			Description: "Record up to 50 lines from the MemQL OS front end under nodeType=os, stamped with the caller's user id. Any signed-in principal; refused whole past a cap, rate_limited when the (user, session) bucket is empty.",
			Handler:     i.handleRecordClient,
			ArgsSchema:  map[string]string{"session": "string (required) -- the tab session id", "lines": "[]object (required) -- { at, level, app, component, message, attributes, subject, subjectConcept }"},
		},
		{
			Name:        "sweep",
			Description: "Run the retention sweep now: archive every expired day per node type, then delete it. No archive, no delete. Owner only; the nightly automation runs it on the cron leader.",
			Handler:     i.handleSweep,
			ArgsSchema:  map[string]string{},
		},
		{
			Name:          "archiveList",
			Description:   "The archived days and node types, newest day first, with sizes when the archive reports them. Admin and above.",
			Handler:       i.handleArchiveList,
			ArgsSchema:    map[string]string{},
			PreserveOrder: true,
		},
		{
			Name:        "archiveRestore",
			Description: "Bring an archived day back into the store, idempotent on (occurredAt, id). Owner only.",
			Handler:     i.handleArchiveRestore,
			ArgsSchema:  map[string]string{"day": "string (required) -- YYYY-MM-DD", "nodeType": "string -- one node type, or blank for every node type archived that day"},
		},
	}
}

// ---------------------------------------------------------------------------
// Role floors
// ---------------------------------------------------------------------------

// requireAdmin admits admin and above under the one ladder: a RANK
// comparison, so developer (300) clears an admin (200) floor without this
// file listing rungs. Anonymous and connector actors rank below every rung
// and are refused by the same comparison; both are also refused by name so
// the reason is legible.
func requireAdmin(ctx context.Context) error {
	ac, ok := auth.AccessFromContext(ctx)
	if !ok || ac == nil {
		return fmt.Errorf("logs: no authenticated caller")
	}
	if ac.IsAnonymousActor() || ac.IsConnector() {
		return fmt.Errorf("logs: an anonymous or connector actor may not read the log store")
	}
	floor := auth.RoleRank(auth.RoleAdmin)
	if floor <= 0 {
		// The floor did not resolve. Refuse rather than admit everyone: a
		// `>=` against 0 is a gate every rank clears.
		return fmt.Errorf("logs: the admin rank floor did not resolve; refusing")
	}
	if auth.RoleRank(ac.Role) >= floor {
		return nil
	}
	return fmt.Errorf("logs: role %q may not read the log store (admin, developer or owner required)", string(ac.Role))
}

// requireOwner admits the cluster owner and nobody else -- the same escape
// the composite row tier uses, and the maintenance principal the nightly
// automation runs under (component/auth/maintenance_actor.go).
func requireOwner(ctx context.Context) error {
	ac, ok := auth.AccessFromContext(ctx)
	if !ok || ac == nil {
		return fmt.Errorf("logs: no authenticated caller")
	}
	if ac.IsClusterOwner() {
		return nil
	}
	return fmt.Errorf("logs: role %q may not run this; it is reserved to a cluster owner", string(ac.Role))
}

// requirePrincipal admits any signed-in person -- an actor with a user id
// that is neither anonymous nor a connector.
func requirePrincipal(ctx context.Context) (*auth.AccessContext, error) {
	ac, ok := auth.AccessFromContext(ctx)
	if !ok || ac == nil {
		return nil, fmt.Errorf("logs: no authenticated caller; lines are recorded only for a signed-in person")
	}
	if ac.IsAnonymousActor() || ac.IsConnector() || strings.TrimSpace(ac.UserId) == "" {
		return nil, fmt.Errorf("logs: an anonymous or connector actor may not record lines")
	}
	return ac, nil
}

// ---------------------------------------------------------------------------
// Handlers
// ---------------------------------------------------------------------------

func (i *Integration) handleSearch(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	if err := requireAdmin(ctx); err != nil {
		return nil, err
	}
	q, err := queryFromArgs(args)
	if err != nil {
		return nil, err
	}
	start, err := requireTime(args, "windowStart")
	if err != nil {
		return nil, err
	}
	end, err := requireTime(args, "windowEnd")
	if err != nil {
		return nil, err
	}
	if !end.After(start) {
		return nil, fmt.Errorf("logs: windowEnd must be after windowStart")
	}
	q.WindowStart, q.WindowEnd = start, end
	if at, ok, err := argTime(args, "beforeAt"); err != nil {
		return nil, err
	} else if ok {
		q.BeforeAt = at
		q.BeforeId = argString(args, "beforeId")
	}
	rows, err := Search(ctx, i.database(), q)
	if err != nil {
		return nil, err
	}
	return rowNodes(rows)
}

func (i *Integration) handleTail(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	if err := requireAdmin(ctx); err != nil {
		return nil, err
	}
	q, err := queryFromArgs(args)
	if err != nil {
		return nil, err
	}
	if at, ok, err := argTime(args, "afterAt"); err != nil {
		return nil, err
	} else if ok {
		q.AfterAt = at
		q.AfterId = argString(args, "afterId")
	}
	rows, err := Tail(ctx, i.database(), q)
	if err != nil {
		return nil, err
	}
	return rowNodes(rows)
}

func (i *Integration) handleSources(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	if err := requireAdmin(ctx); err != nil {
		return nil, err
	}
	start, err := requireTime(args, "windowStart")
	if err != nil {
		return nil, err
	}
	end, err := requireTime(args, "windowEnd")
	if err != nil {
		return nil, err
	}
	sources, err := Sources(ctx, i.database(), start, end)
	if err != nil {
		return nil, err
	}
	out := make([]memorynodes.MemoryNode, 0, len(sources))
	for _, s := range sources {
		node, err := replyNode(conceptSource, s.Kind+":"+s.NodeType+":"+s.Value, s, i.now())
		if err != nil {
			return nil, err
		}
		out = append(out, node)
	}
	return out, nil
}

// StatusReport is what logsStatus answers: the cluster's configuration, this
// node's store, and the table's bounds.
type StatusReport struct {
	RetentionDays     int    `json:"retentionDays"`
	Level             string `json:"level"`
	MaxLinesPerSecond int    `json:"maxLinesPerSecond"`
	ArchiveConfigured bool   `json:"archiveConfigured"`
	ArchiveContainer  string `json:"archiveContainer,omitempty"`
	ArchiveClient     bool   `json:"archiveClient"`
	NodeType          string `json:"nodeType"`
	Node              string `json:"node"`
	// StoreActive is false on a node with no sink: no database, or the store
	// is off here. Counters are then absent rather than zero.
	StoreActive bool        `json:"storeActive"`
	Counters    *Stats      `json:"counters,omitempty"`
	Table       TableStatus `json:"table"`
	TableError  string      `json:"tableError,omitempty"`
}

func (i *Integration) handleStatus(ctx context.Context, _ map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	if err := requireAdmin(ctx); err != nil {
		return nil, err
	}
	rep := StatusReport{
		RetentionDays:     RetentionDays(),
		Level:             LevelName(),
		MaxLinesPerSecond: MaxLinesPerSecond(),
		ArchiveConfigured: i.container != "",
		ArchiveContainer:  i.container,
		ArchiveClient:     i.archiver != nil,
		NodeType:          resolveNodeType(),
		Node:              resolveNodeId(),
	}
	if s := i.sink(); s != nil {
		st := s.Stats()
		rep.StoreActive = true
		rep.Counters = &st
		rep.NodeType, rep.Node = s.NodeType(), s.Node()
		rep.MaxLinesPerSecond = s.MaxLinesPerSecond()
	}
	if table, err := Status(ctx, i.database()); err != nil {
		rep.TableError = err.Error()
	} else {
		rep.Table = table
	}
	node, err := replyNode(conceptStatus, "logsStatus", rep, i.now())
	if err != nil {
		return nil, err
	}
	return []memorynodes.MemoryNode{node}, nil
}

func (i *Integration) handleRecordClient(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	ac, err := requirePrincipal(ctx)
	if err != nil {
		return nil, err
	}
	session := strings.TrimSpace(argString(args, "session"))
	if err := ValidateSession(session); err != nil {
		return nil, fmt.Errorf("logs: %w", err)
	}
	var raw []any
	switch v := args["lines"].(type) {
	case nil:
	case []any:
		raw = v
	case []map[string]any:
		raw = make([]any, 0, len(v))
		for _, m := range v {
			raw = append(raw, m)
		}
	default:
		return nil, fmt.Errorf("logs: lines is not a list of objects")
	}
	now := i.now()
	lines, err := ParseClientLines(raw, now)
	if err != nil {
		return nil, fmt.Errorf("logs: %w", err)
	}
	if !i.limiter.Allow(ac.UserId, session, len(lines), now) {
		// A refusal REPLY, not an error: the shell drops and counts rather
		// than retrying into the same empty bucket.
		return i.recorded(ClientReply{Dropped: len(lines), Reason: ReasonRateLimited})
	}
	s := i.sink()
	if s == nil {
		return nil, ErrNoStore
	}
	res := s.WriteLines(lines, Stamp{NodeType: NodeTypeOS, Node: "", Session: session, UserId: ac.UserId})
	reply := ClientReply{Accepted: res.Accepted, Dropped: res.Dropped}
	if res.Dropped > 0 {
		reply.Reason = "store_backpressure"
	}
	return i.recorded(reply)
}

func (i *Integration) recorded(reply ClientReply) ([]memorynodes.MemoryNode, error) {
	node, err := replyNode(conceptRecorded, "logsRecorded", reply, i.now())
	if err != nil {
		return nil, err
	}
	return []memorynodes.MemoryNode{node}, nil
}

func (i *Integration) handleSweep(ctx context.Context, _ map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	if err := requireOwner(ctx); err != nil {
		return nil, err
	}
	rep, err := i.sweeper().Run(ctx)
	if err != nil {
		return nil, err
	}
	node, err := replyNode(conceptSweep, "logsSweep", rep, i.now())
	if err != nil {
		return nil, err
	}
	return []memorynodes.MemoryNode{node}, nil
}

func (i *Integration) handleArchiveList(ctx context.Context, _ map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	if err := requireAdmin(ctx); err != nil {
		return nil, err
	}
	if i.archiver == nil || i.container == "" {
		reason := "no archive container is configured (" + EnvArchiveContainer + " / " + envBlobContainer + ")"
		if i.container != "" {
			reason = "an archive container is configured (" + i.container + ") but this node could build no blob client"
		}
		node, err := replyNode(conceptArchiveNone, "logsArchiveNone", map[string]any{"configured": i.container != "", "container": i.container, "reason": reason}, i.now())
		if err != nil {
			return nil, err
		}
		return []memorynodes.MemoryNode{node}, nil
	}
	objects, err := i.sweeper().ListArchive(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]memorynodes.MemoryNode, 0, len(objects))
	for _, o := range objects {
		node, err := replyNode(conceptArchive, o.Object, o, i.now())
		if err != nil {
			return nil, err
		}
		out = append(out, node)
	}
	return out, nil
}

func (i *Integration) handleArchiveRestore(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	if err := requireOwner(ctx); err != nil {
		return nil, err
	}
	day := strings.TrimSpace(argString(args, "day"))
	if day == "" {
		return nil, fmt.Errorf("logs: day is required (YYYY-MM-DD)")
	}
	rep, err := i.sweeper().Restore(ctx, day, strings.TrimSpace(argString(args, "nodeType")))
	if err != nil {
		return nil, err
	}
	node, err := replyNode(conceptRestore, "logsRestore:"+rep.Day, rep, i.now())
	if err != nil {
		return nil, err
	}
	return []memorynodes.MemoryNode{node}, nil
}

// ---------------------------------------------------------------------------
// Wiring
// ---------------------------------------------------------------------------

func (i *Integration) database() *bun.DB {
	if i.db == nil {
		return nil
	}
	return i.db()
}

func (i *Integration) sweeper() *Sweeper {
	var lock *bun.DB
	if i.directDB != nil {
		lock = i.directDB()
	}
	return &Sweeper{
		DB:        i.database(),
		LockDB:    lock,
		Archive:   i.archiver,
		Container: i.container,
		Logger:    i.logger,
		Now:       i.now,
	}
}

// rowNodes returns rows as v1:observability:logLine nodes: the id is the
// row's, createdAt is occurredAt, and the payload is the concept's fields.
func rowNodes(rows []Row) ([]memorynodes.MemoryNode, error) {
	out := make([]memorynodes.MemoryNode, 0, len(rows))
	for idx := range rows {
		payload, err := json.Marshal(&rows[idx])
		if err != nil {
			return nil, fmt.Errorf("logs: marshal row %s: %w", rows[idx].ID, err)
		}
		out = append(out, memorynodes.MemoryNode{
			ID:        rows[idx].ID,
			Concept:   ConceptID,
			Type:      memorynodes.NodeTypeObject,
			CreatedAt: rows[idx].OccurredAt,
			Payload:   payload,
		})
	}
	return out, nil
}

func replyNode(concept, id string, payload any, at time.Time) (memorynodes.MemoryNode, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return memorynodes.MemoryNode{}, fmt.Errorf("logs: marshal reply: %w", err)
	}
	return memorynodes.MemoryNode{
		ID:        id,
		Concept:   concept,
		Type:      memorynodes.NodeTypeObject,
		CreatedAt: at,
		Payload:   body,
	}, nil
}

// ---------------------------------------------------------------------------
// Argument decoding
// ---------------------------------------------------------------------------

// queryFromArgs reads the facet set shared by search and tail.
func queryFromArgs(args map[string]any) (Query, error) {
	q := Query{
		NodeTypes:       argStrings(args, "nodeTypes"),
		Nodes:           argStrings(args, "nodes"),
		Components:      argStrings(args, "components"),
		Apps:            argStrings(args, "apps"),
		Levels:          argStrings(args, "levels"),
		SubjectConcepts: argStrings(args, "subjectConcepts"),
		Subject:         argString(args, "subject"),
		SubjectConcept:  argString(args, "subjectConcept"),
		Session:         argString(args, "session"),
		UserId:          argString(args, "userId"),
		Text:            argString(args, "text"),
		Limit:           argInt(args, "limit", DefaultLimit),
	}
	for _, l := range q.Levels {
		if _, ok := clientLevels[strings.ToLower(l)]; !ok {
			return q, fmt.Errorf("logs: level %q is not one of debug, info, warn, error", l)
		}
	}
	return q, nil
}

func argString(args map[string]any, key string) string {
	if args == nil {
		return ""
	}
	v, ok := args[key]
	if !ok || v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	return fmt.Sprint(v)
}

// argStrings reads a []string field, which arrives as []any from a JSON
// caller, []string from Go, or a single string.
func argStrings(args map[string]any, key string) []string {
	if args == nil {
		return nil
	}
	switch v := args[key].(type) {
	case []string:
		return cleanList(v)
	case []any:
		out := make([]string, 0, len(v))
		for _, item := range v {
			if s, ok := item.(string); ok {
				out = append(out, s)
			}
		}
		return cleanList(out)
	case string:
		return cleanList([]string{v})
	}
	return nil
}

// argTime reads a datetime field: an RFC 3339 string (the wire form) or a
// time.Time. Absent is (zero, false, nil); present but unreadable is an
// error rather than a silent open bound.
func argTime(args map[string]any, key string) (time.Time, bool, error) {
	if args == nil {
		return time.Time{}, false, nil
	}
	v, ok := args[key]
	if !ok || v == nil {
		return time.Time{}, false, nil
	}
	switch x := v.(type) {
	case time.Time:
		return x.UTC(), true, nil
	case string:
		s := strings.TrimSpace(x)
		if s == "" {
			return time.Time{}, false, nil
		}
		t, err := time.Parse(time.RFC3339Nano, s)
		if err != nil {
			return time.Time{}, false, fmt.Errorf("logs: %s %q is not an RFC 3339 datetime", key, x)
		}
		return t.UTC(), true, nil
	}
	return time.Time{}, false, fmt.Errorf("logs: %s is not a datetime", key)
}

func requireTime(args map[string]any, key string) (time.Time, error) {
	t, ok, err := argTime(args, key)
	if err != nil {
		return time.Time{}, err
	}
	if !ok {
		return time.Time{}, fmt.Errorf("logs: %s is required", key)
	}
	return t, nil
}

// argInt reads an int field. A JSON caller sends float64, a Go caller int or
// int64, a rendered call a string; every narrowing goes through core/num
// with the caller's default as the answer for a value that does not fit.
func argInt(args map[string]any, key string, def int) int {
	if args == nil {
		return def
	}
	switch v := args[key].(type) {
	case int:
		return v
	case int64:
		return num.Int64Or(v, def)
	case float64:
		return num.Float64Or(v, def)
	case string:
		if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil {
			return n
		}
	}
	return def
}
