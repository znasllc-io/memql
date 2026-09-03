package logger

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"
)

// store.go -- the seam between a log line and the store that keeps it
// (epic memql#4893, design record docs/superpowers/specs/2026-09-03-logs-design.md
// section B).
//
// This package is the `core` module: stdlib only, and it cannot import
// component/*. So the store itself -- the batching sink over the log_line
// hypertable -- lives in component/logstore, and what lives HERE is the
// half a handler chain needs: a Line, a Sink to hand it to, a process-global
// registration, and the slog.Handler that turns a record into a Line. The
// handler chain logger.New builds is
//
//	redactingHandler -> fanout(JSONHandler, storeHandler)
//
// with the store handler INSIDE the redactor, so every attribute the store
// keeps is exactly what the console would have printed -- a `token` attribute
// arrives at the sink as "<redacted>", never as the token.
//
// Before boot registers a sink, lines go to a bounded ring of preBootRingSize
// so the lines a node writes while it is still wiring its database are kept;
// SetSink drains that ring, in order, into the sink it installs. A binary that
// never registers a sink keeps the ring's newest lines and drops the overflow,
// and nothing else about it changes.

// Line is one log line as the store keeps it. Attributes carries every
// attribute the record had other than the ones lifted into named fields, keys
// dotted for groups (`request.method`), values already rendered to JSON-safe
// Go values (string, int64, uint64, float64, bool, or nil).
type Line struct {
	At             time.Time
	Level          slog.Level
	Component      string
	App            string
	Message        string
	Subject        string
	SubjectConcept string
	Attributes     map[string]any
}

// Sink receives lines from every store handler in the process. Write MUST
// NOT block: it runs on the caller's goroutine inside a log call, and a
// sink that waits on a database turns logging into a head-of-line latency
// hazard. component/logstore.Sink is a non-blocking channel send.
type Sink interface {
	Write(Line)
}

// preBootRingSize bounds the lines kept before a sink is registered. Oldest
// dropped when full: the ring exists to keep the boot lines, and a node that
// never registers a sink should not grow without bound.
const preBootRingSize = 2048

// storeRecursionPrefix names the component whose lines are NEVER forwarded to
// the sink. The store's own failures -- an insert that errored, a database
// that went away -- are logged under it and reach the console only, which is
// where a broken store can still be read. Forwarding them would make a
// failing insert produce a line that produces an insert that fails.
const storeRecursionPrefix = "logs.store"

// storeLevelEnv is the store's own floor, independent of the console level.
// debug / info / warn / error, or off to disable the store on this node.
// Default info. Read ONCE per process, the way MEMQL_OBSERVE_LEVEL is.
const storeLevelEnv = "MEMQL_LOGS_LEVEL"

var (
	sinkMu   sync.Mutex
	sink     Sink
	ring     []Line
	ringHead int // index of the oldest line when the ring is full
	ringFull bool
)

// SetSink installs the process-wide sink and drains the pre-boot ring into
// it, oldest first. Passing nil clears the sink; lines written afterwards go
// back to the ring.
//
// The drain happens outside the lock so a sink's Write never runs under it,
// and the ordering still holds: a line arriving during the drain observes the
// sink already set and writes directly, which is after every ring line
// because the ring was taken under the same lock the sink was set under.
func SetSink(s Sink) {
	sinkMu.Lock()
	sink = s
	var drained []Line
	if s != nil {
		drained = takeRingLocked()
	}
	sinkMu.Unlock()
	for _, l := range drained {
		s.Write(l)
	}
}

// CurrentSink returns the installed sink, or nil before boot registers one.
func CurrentSink() Sink {
	sinkMu.Lock()
	defer sinkMu.Unlock()
	return sink
}

// takeRingLocked returns the ring's lines in write order and empties it.
// Caller holds sinkMu.
func takeRingLocked() []Line {
	if len(ring) == 0 {
		return nil
	}
	var out []Line
	if ringFull {
		out = make([]Line, 0, len(ring))
		out = append(out, ring[ringHead:]...)
		out = append(out, ring[:ringHead]...)
	} else {
		out = append([]Line(nil), ring...)
	}
	ring = nil
	ringHead = 0
	ringFull = false
	return out
}

// deliver hands a line to the sink, or to the ring when none is installed.
func deliver(l Line) {
	sinkMu.Lock()
	if s := sink; s != nil {
		sinkMu.Unlock()
		s.Write(l)
		return
	}
	if ringFull {
		ring[ringHead] = l
		ringHead = (ringHead + 1) % preBootRingSize
	} else {
		ring = append(ring, l)
		if len(ring) == preBootRingSize {
			ringFull = true
			ringHead = 0
		}
	}
	sinkMu.Unlock()
}

// pendingRingLen reports how many lines the pre-boot ring holds. For tests.
func pendingRingLen() int {
	sinkMu.Lock()
	defer sinkMu.Unlock()
	return len(ring)
}

// resetStoreForTest clears the sink and the ring. Tests only.
func resetStoreForTest() {
	sinkMu.Lock()
	sink = nil
	ring = nil
	ringHead = 0
	ringFull = false
	sinkMu.Unlock()
}

// Subject names the thing a line is about: the concept id and the bare id of
// the row -- a deployment, a site, a plan.
//
// THIS IS THE SEAM between a line and the thing it is about. The store keeps
// the pair in two columns (subject, subjectConcept), a subject in a row of the
// Logs app narrows on click, and an app's per-app Logs section selects lines
// whose subjectConcept is one the app owns. It is an slog.Group so a caller
// writes it beside its other attributes:
//
//	logger.Info("packages: deploy started",
//	    "component", "packages.pipeline",
//	    logger.Subject("v1:platform:packageDeployment", deploymentId))
//
// The id is BARE, never canonical (docs/public/concepts/identifiers.md): the
// client holds concept ids as generated constants and compares bare ids, and
// nothing on either side composes or parses a `{concept}:{shortId}` string.
func Subject(concept, id string) slog.Attr {
	return slog.Group("subject", slog.String("id", id), slog.String("concept", concept))
}

// ---------------------------------------------------------------------------
// The store's floor
// ---------------------------------------------------------------------------

var (
	storeFloorOnce  sync.Once
	storeFloorLevel slog.Level
	storeFloorOff   bool
	storeFloorName  string
)

// parseStoreLevel maps a MEMQL_LOGS_LEVEL value to (level, off, name).
// Unknown or blank values are the default, info -- never a silent "store
// everything" and never a silent "store nothing".
func parseStoreLevel(raw string) (slog.Level, bool, string) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "debug":
		return slog.LevelDebug, false, "debug"
	case "warn", "warning":
		return slog.LevelWarn, false, "warn"
	case "error":
		return slog.LevelError, false, "error"
	case "off", "none", "false", "0":
		return slog.LevelInfo, true, "off"
	default:
		return slog.LevelInfo, false, "info"
	}
}

func storeFloor() (slog.Level, bool) {
	storeFloorOnce.Do(func() {
		storeFloorLevel, storeFloorOff, storeFloorName = parseStoreLevel(os.Getenv(storeLevelEnv))
	})
	return storeFloorLevel, storeFloorOff
}

// StoreLevelName returns the store floor this process runs with, as the word
// MEMQL_LOGS_LEVEL was read as: debug / info / warn / error / off. The one
// parse of that variable; component/logstore reports it through logsStatus
// rather than reading the variable a second time.
func StoreLevelName() string {
	storeFloor()
	return storeFloorName
}

// StoreFloor returns the store floor as a level plus whether the store is off
// on this node. The sink applies it a second time to lines that did not come
// through a handler -- the OS write path hands it lines carrying their own
// levels -- so a debug line from a browser is held to the same floor an
// engine debug line is.
func StoreFloor() (slog.Level, bool) {
	return storeFloor()
}

// ---------------------------------------------------------------------------
// storeHandler
// ---------------------------------------------------------------------------

// boundAttr is a pre-bound attribute, already flattened under the groups that
// were open when WithAttrs ran.
type boundAttr struct {
	key string
	val any
}

// storeHandler turns each record into a Line and delivers it. Enabled is the
// store's own floor, independent of the console's, so a node whose
// MEMQL_LOGS_LEVEL says debug keeps debug lines the console never prints.
type storeHandler struct {
	floor  slog.Level
	off    bool
	bound  []boundAttr // from WithAttrs, flattened
	groups []string    // open groups for subsequent attrs
}

// newStoreHandler builds the handler at the process floor.
func newStoreHandler() *storeHandler {
	level, off := storeFloor()
	return newStoreHandlerAt(level, off)
}

// newStoreHandlerAt builds a handler at an explicit floor. Tests use it so
// they do not depend on the once-read environment.
func newStoreHandlerAt(floor slog.Level, off bool) *storeHandler {
	return &storeHandler{floor: floor, off: off}
}

func (h *storeHandler) Enabled(_ context.Context, level slog.Level) bool {
	return !h.off && level >= h.floor
}

func (h *storeHandler) Handle(_ context.Context, rec slog.Record) error {
	attrs := make(map[string]any, len(h.bound)+rec.NumAttrs())
	for _, b := range h.bound {
		attrs[b.key] = b.val
	}
	prefix := joinGroups(h.groups)
	rec.Attrs(func(a slog.Attr) bool {
		flattenAttr(attrs, prefix, a)
		return true
	})

	line := Line{
		At:         rec.Time,
		Level:      rec.Level,
		Message:    rec.Message,
		Attributes: attrs,
	}
	if line.At.IsZero() {
		line.At = time.Now()
	}
	line.Component = takeString(attrs, "component")
	if strings.HasPrefix(line.Component, storeRecursionPrefix) {
		// The recursion guard: the store's own lines go to the console only.
		return nil
	}
	line.App = takeString(attrs, "app")
	// logger.Subject lands as the `subject` group; a bare `subject` /
	// `subjectConcept` string pair written by hand lands in the same columns,
	// so a caller that spelled it out gets the same seam.
	line.Subject = takeString(attrs, "subject.id")
	line.SubjectConcept = takeString(attrs, "subject.concept")
	if line.Subject == "" {
		line.Subject = takeString(attrs, "subject")
	}
	if line.SubjectConcept == "" {
		line.SubjectConcept = takeString(attrs, "subjectConcept")
	}
	if len(attrs) == 0 {
		line.Attributes = nil
	}
	deliver(line)
	return nil
}

func (h *storeHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	if len(attrs) == 0 {
		return h
	}
	flat := make(map[string]any, len(attrs))
	prefix := joinGroups(h.groups)
	for _, a := range attrs {
		flattenAttr(flat, prefix, a)
	}
	next := &storeHandler{floor: h.floor, off: h.off, groups: h.groups}
	next.bound = make([]boundAttr, 0, len(h.bound)+len(flat))
	next.bound = append(next.bound, h.bound...)
	for k, v := range flat {
		next.bound = append(next.bound, boundAttr{key: k, val: v})
	}
	return next
}

func (h *storeHandler) WithGroup(name string) slog.Handler {
	if name == "" {
		return h
	}
	groups := make([]string, 0, len(h.groups)+1)
	groups = append(groups, h.groups...)
	groups = append(groups, name)
	return &storeHandler{floor: h.floor, off: h.off, bound: h.bound, groups: groups}
}

func joinGroups(groups []string) string {
	if len(groups) == 0 {
		return ""
	}
	return strings.Join(groups, ".") + "."
}

// flattenAttr writes attr into out under prefix, recursing into groups with a
// dotted key. An attr with an empty key and a group value is inlined, as slog
// specifies; an empty-keyed non-group attr is dropped, as slog's own handlers
// do.
func flattenAttr(out map[string]any, prefix string, a slog.Attr) {
	a.Value = a.Value.Resolve()
	if a.Value.Kind() == slog.KindGroup {
		members := a.Value.Group()
		if len(members) == 0 {
			return
		}
		next := prefix
		if a.Key != "" {
			next = prefix + a.Key + "."
		}
		for _, m := range members {
			flattenAttr(out, next, m)
		}
		return
	}
	if a.Key == "" {
		return
	}
	out[prefix+a.Key] = renderValue(a.Value)
}

// renderValue turns an slog value into a JSON-safe Go value: strings, numbers
// and booleans as themselves, times as RFC 3339, errors as their text, and
// everything else through fmt -- a stored attribute is read by a person in a
// list, and a struct's %v is that reading.
func renderValue(v slog.Value) any {
	switch v.Kind() {
	case slog.KindString:
		return v.String()
	case slog.KindInt64:
		return v.Int64()
	case slog.KindUint64:
		return v.Uint64()
	case slog.KindFloat64:
		return v.Float64()
	case slog.KindBool:
		return v.Bool()
	case slog.KindDuration:
		return v.Duration().String()
	case slog.KindTime:
		return v.Time().UTC().Format(time.RFC3339Nano)
	case slog.KindAny:
		return renderAny(v.Any())
	default:
		return fmt.Sprint(v)
	}
}

func renderAny(a any) any {
	switch x := a.(type) {
	case nil:
		return nil
	case error:
		return x.Error()
	case time.Time:
		return x.UTC().Format(time.RFC3339Nano)
	case string:
		return x
	case bool:
		return x
	case int:
		return int64(x)
	case int8:
		return int64(x)
	case int16:
		return int64(x)
	case int32:
		return int64(x)
	case int64:
		return x
	case uint:
		return uint64(x)
	case uint8:
		return uint64(x)
	case uint16:
		return uint64(x)
	case uint32:
		return uint64(x)
	case uint64:
		return x
	case float32:
		return float64(x)
	case float64:
		return x
	case fmt.Stringer:
		return x.String()
	default:
		return fmt.Sprint(x)
	}
}

// takeString removes key from attrs and returns its string form, or "" when
// absent or not a string.
func takeString(attrs map[string]any, key string) string {
	v, ok := attrs[key]
	if !ok {
		return ""
	}
	delete(attrs, key)
	s, _ := v.(string)
	return s
}

// ---------------------------------------------------------------------------
// fanoutHandler
// ---------------------------------------------------------------------------

// fanoutHandler delivers one record to every handler whose Enabled says yes.
// Enabled is the OR: a debug line the console will not print still reaches a
// store whose floor is debug. An error from the store handler is swallowed --
// storing is best-effort and must never fail the console line beside it; the
// console handler's error is returned as it always was.
type fanoutHandler struct {
	console slog.Handler
	store   slog.Handler
}

func newFanoutHandler(console, store slog.Handler) slog.Handler {
	if store == nil {
		return console
	}
	return &fanoutHandler{console: console, store: store}
}

func (f *fanoutHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return f.console.Enabled(ctx, level) || f.store.Enabled(ctx, level)
}

func (f *fanoutHandler) Handle(ctx context.Context, rec slog.Record) error {
	var err error
	if f.console.Enabled(ctx, rec.Level) {
		err = f.console.Handle(ctx, rec)
	}
	if f.store.Enabled(ctx, rec.Level) {
		_ = f.store.Handle(ctx, rec.Clone())
	}
	return err
}

func (f *fanoutHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &fanoutHandler{console: f.console.WithAttrs(attrs), store: f.store.WithAttrs(attrs)}
}

func (f *fanoutHandler) WithGroup(name string) slog.Handler {
	return &fanoutHandler{console: f.console.WithGroup(name), store: f.store.WithGroup(name)}
}
