package harness

import "sync"

// recording.go pairs each call with its result and numbers what comes
// out, for one session.
//
// PER SESSION, NOT PER TURN. The action seq runs across every turn, which
// is what lets a reader see a gap anywhere in a session; and an id seen
// in one turn is remembered into the next, so a result replayed on resume
// cannot record one call twice.

// recording is one session's bookkeeping. Every method is a no-op on a
// nil receiver, so a harness that was never started records nothing
// rather than panicking.
type recording struct {
	mu   sync.Mutex
	seq  uint64
	turn int
	// pending are calls begun and not yet finished, by the app's id, and
	// order is the order they began in -- the order a flush reports them.
	pending map[string]*Action
	order   []string
	// completed are ids already emitted, so a repeat is dropped.
	completed map[string]bool
}

func newRecording() *recording {
	return &recording{pending: map[string]*Action{}, completed: map[string]bool{}}
}

// startTurn opens the session's next turn.
func (r *recording) startTurn() {
	if r == nil {
		return
	}
	r.mu.Lock()
	r.turn++
	r.mu.Unlock()
}

// begin records a call the app has started.
//
// A call is not an action until it completes, so nothing is emitted here.
// A begin for an id the session already holds or already emitted is
// dropped: an app that replays a started item on reattach must not open a
// second record of one call. A call with no id has nothing to pair its
// result with; its completion stands alone.
func (r *recording) begin(a Action) {
	if r == nil || a.ID == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.completed[a.ID] {
		return
	}
	if _, held := r.pending[a.ID]; held {
		return
	}
	call := a
	r.pending[a.ID] = &call
	r.order = append(r.order, a.ID)
}

// complete finishes the call with id and returns it numbered, or false
// when that call was already emitted.
//
// finish writes what the completion reported onto the call as it began.
// When the begin never arrived -- the line carrying it was too large to
// read -- finish writes onto a bare call instead, because a result the
// session can see is still a completed call. The bare call is `other`
// with no arguments, which is exactly what is known about it.
func (r *recording) complete(id string, finish func(*Action)) (Action, bool) {
	var out Action
	var ok bool
	r.completeAndEmit(id, finish, func(a Action) { out, ok = a, true })
	return out, ok
}

// flush closes every call still open as Incomplete, in the order they
// began.
//
// It runs at the end of every turn. A call begun in a turn that has ended
// will never finish -- Claude Code's next turn is a new process, and a
// Codex turn that ended took its in-flight items with it -- so holding it
// open would lose it silently, which is the one thing a recording must
// not do. What it would have reported is unknown, so nothing about a
// result survives into the record, and no file is read for it.
func (r *recording) flush() []Action {
	var out []Action
	r.flushAndEmit(func(a Action) { out = append(out, a) })
	return out
}

// completeAndEmit is complete with the numbered action handed to emit
// while the recording's lock is still held, and flushAndEmit is flush the
// same way.
//
// A HARNESS WITH TWO GOROUTINES NEEDS THESE. The Codex clients complete
// calls on the connection's reader and flush them on the turn's own
// goroutine when the turn ends; numbering under the lock and sending after
// it let a flushed seq 2 leave before a completed seq 1 -- and the engine
// drops an action that arrives behind a higher one, so the lower call
// would be lost. Holding the lock through the send makes the order on the
// wire the order of the seq. It also means a flush waits for a completion
// that is mid-send, so nothing is still being sent once a turn returns --
// and "send" includes the session reading the call's files, so a
// cancelled turn returns only once those reads are done. That wait is
// bounded by the files, never by the app: the app is killed on cancel
// either way. A workspace whose filesystem hangs hangs the session with
// or without it, since the transcript is written there on every chunk.
// (Claude Code's client completes and flushes on one goroutine and keeps
// the plain methods.)
func (r *recording) completeAndEmit(id string, finish func(*Action), emit func(Action)) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if id != "" && r.completed[id] {
		return
	}
	call, held := r.pending[id]
	if held {
		delete(r.pending, id)
		r.order = removeID(r.order, id)
	} else {
		call = &Action{ID: id, Tool: ActionOther}
	}
	if finish != nil {
		finish(call)
	}
	if id != "" {
		r.completed[id] = true
	}
	emit(r.stamp(*call))
}

func (r *recording) flushAndEmit(emit func(Action)) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, id := range r.order {
		call := r.pending[id]
		if call == nil {
			continue
		}
		call.Incomplete = true
		call.IsError, call.ExitCode = nil, nil
		call.ResultType, call.ResultDigest = "", ""
		call.Contents = nil
		r.completed[id] = true
		emit(r.stamp(*call))
	}
	r.pending = map[string]*Action{}
	r.order = nil
}

// stamp numbers an action for the wire. Callers hold r.mu.
func (r *recording) stamp(a Action) Action {
	r.seq++
	a.Type = ActionEventType
	a.V = RecordVersion
	a.Seq = r.seq
	a.Turn = r.turn
	a.Args = rawOrNull(a.Args)
	return a
}

func removeID(ids []string, id string) []string {
	for i, v := range ids {
		if v == id {
			return append(ids[:i], ids[i+1:]...)
		}
	}
	return ids
}

// mergeCall lays the ended report of a call over its begun one: every
// field the end carries wins, and a field it left empty keeps what the
// begin said. An older Codex's patch_apply_end carries no changes, and
// taking it alone would record a write to nowhere.
func mergeCall(begun, ended Action) Action {
	out := ended
	if out.ID == "" {
		out.ID = begun.ID
	}
	if out.ParentID == "" {
		out.ParentID = begun.ParentID
	}
	if out.AppTool == "" {
		out.AppTool = begun.AppTool
	}
	if isNull(out.Args) {
		out.Args = begun.Args
	}
	if out.Cwd == "" {
		out.Cwd = begun.Cwd
	}
	if out.Command == "" {
		out.Command = begun.Command
	}
	if out.MCP == nil {
		out.MCP = begun.MCP
	}
	if out.URL == "" {
		out.URL = begun.URL
	}
	if out.Query == "" {
		out.Query = begun.Query
	}
	if len(out.Contents) == 0 {
		out.Contents = begun.Contents
	}
	return out
}
