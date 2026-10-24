// Package appsession runs the engine's app sessions on this machine:
// AppSessionStart / Chunk / Control / End over the worker stream, with
// the run, open and attach kinds (memql-cockpit#347).
//
// WHY A SESSION AND NOT A ToolDispatch. A dispatch carries ONE timeout
// and returns ONE result. A headless `claude -p` can run for an hour and
// emits output the whole way, so the shape has to be a stream: start,
// chunks while it works, control from the server, one end.
//
// A SESSION IS A SEQUENCE OF TURNS (memql-cockpit#386, design D2/D7).
// The run and attach kinds no longer fork an argv and read the terminal;
// they drive the app through its OWN protocol, via
// internal/worker/harness, and a `message` control starts the next turn
// in the conversation the previous one opened. That is what makes a
// follow-up a continuation rather than a stranger, and what turns three
// guesses -- what the app said, what it spent, which session to resume --
// into three facts. The `open` kind is untouched: handing an app to a
// human is not a harness turn.
//
// Three invariants run through the file, each because getting it wrong is
// silently destructive rather than loudly broken:
//
//   - `seq` is monotonic per session and is NEVER renumbered, including
//     on a retried send. The engine drops out-of-order and duplicate
//     chunks rather than appending them, because a transcript is a record
//     and interleaving a replayed chunk corrupts it in a way no later
//     reader can detect.
//
//   - `usage.known=false` when the app reported nothing. The engine
//     records that as billing "unknown", which is the honest answer; an
//     estimate would be recorded as measured, in a ledger somebody bills
//     from.
//
//   - the MCP configuration file is deleted on EVERY exit path. It holds
//     a bearer that cannot be revoked, so deletion is the control rather
//     than the tidy-up. See mcpconfig.go.
package appsession

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"

	"github.com/znasllc-io/memql-cockpit/internal/worker/apps"
	"github.com/znasllc-io/memql-cockpit/internal/worker/harness"
)

// Session kinds, from AppSessionStart.kind.
const (
	KindRun    = "run"
	KindOpen   = "open"
	KindAttach = "attach"
)

// Chunk streams, from AppSessionChunk.stream.
//
// ALIASED from internal/worker/harness rather than re-spelled, because
// the harness sink is now where four of the five come from: two
// definitions of one word are two things a rename can pull apart in
// silence, and the symptom would be an engine mapping a chunk stream it
// has never heard of.
//
// The proto's comment names only "stdout", "stderr" and "event", and
// `text` and `tool` are new here (memql#5096 is the engine's half). They
// are safe to send ahead of it: the engine's chunk path copies
// `stream` through verbatim (component/worker/server.go
// handleAppSessionChunk), its transcript collector appends EVERY chunk
// whatever the word (component/worker/runner.go), and its live view
// renders anything that is not "event" as narration
// (integrations/agent/worker/cockpitapp.go). So a cockpit that is ahead
// of its engine loses the finer split, not the words themselves --
// which is the direction that costs a reader nothing.
const (
	StreamStdout = harness.StreamStdout
	StreamStderr = harness.StreamStderr
	StreamEvent  = harness.StreamEvent
	StreamText   = harness.StreamText
	StreamTool   = harness.StreamTool
)

// Control actions, from AppSessionControl.action.
const (
	ActionCancel          = "cancel"
	ActionRenewCredential = "renew_credential"
	// ActionMessage starts the NEXT turn of a session that is already
	// running one (design D7). The prompt it carries has a field of its
	// own, read by controlPrompt.
	ActionMessage = "message"
)

// maxQueuedFollowUps bounds the follow-ups waiting for a turn to end.
//
// The queue is fed by the network and drained by an app that takes
// minutes per turn, so an unbounded one is a hole a chatty caller could
// dig in somebody's laptop. Refusing past the bound is LOUD (a log line
// and a chunk), because a follow-up that vanished silently is
// indistinguishable to the person who typed it from one the app ignored.
const maxQueuedFollowUps = 8

// --- the memql#5096 fields, read in one place each
//
// The follow-up prompt, the response schema and the structured result
// each have ONE accessor rather than reads scattered across the call
// sites. That was the design while the fields did not exist yet -- a
// stand-in in one place is a one-line change on the day the field lands
// -- and it is why the day came and went unnoticed: the 2026-09-08 pin
// bump carried all three and none of the one-line changes was made, so
// every follow-up arrived empty, no session was asked for a schema, and
// no structured answer reached the engine (memql-cockpit#444). The
// accessors stay single for the same reason they were: a field read in
// one place is a field one test can pin.

// controlPrompt is the follow-up prompt on AppSessionControl{message}:
// its `prompt` field, and nothing else.
//
// `reason` is NOT a fallback. The proto documents it as transcript
// free-text on cancel, and the engine gives the follow-up its own field
// precisely because one field meaning two things cannot be read without
// knowing which branch wrote it. A message control with no prompt is
// refused by followUp, loudly.
func controlPrompt(c *memqlv1.AppSessionControl) string {
	return strings.TrimSpace(c.GetPrompt())
}

// startResponseSchema is the JSON Schema the engine asked this session's
// final answer to satisfy -- AppSessionStart.response_schema_json.
//
// EMPTY MEANS THE ENGINE ASKED FOR NONE, and a harness must then not
// invent one: a schema the caller did not ask for changes what the app
// says, and the answer would be structured because the COCKPIT decided it
// should be. Whether this machine's harness can honour one is the app
// descriptor's business (Register.app_descriptors): a harness that cannot
// constrain an answer reports structured_result=false, and the engine does
// not send it a schema.
func startResponseSchema(s *memqlv1.AppSessionStart) string {
	return strings.TrimSpace(s.GetResponseSchemaJson())
}

// Sender is the worker's side of the stream, as this package needs it.
type Sender interface {
	SendAppSessionChunk(sessionID, stream string, data []byte, seq uint64) error
	SendAppSessionEnd(end *memqlv1.AppSessionEnd) error
}

// Options configures a Manager.
type Options struct {
	Logger *slog.Logger
	// StateDir is the worker's state directory. The MCP write ledger
	// lives under it, which is what lets a restart sweep what a crash
	// left behind.
	StateDir string
	// ClusterURL is the worker's cluster_url; the Library origin is
	// derived from it.
	ClusterURL string
	// LibraryBase overrides that derivation. Tests set it; nothing else
	// should need to.
	LibraryBase string
	// HTTPClient is the client the Library calls use.
	HTTPClient *http.Client
	// Allowed reports whether policy.yaml allows an app for THIS
	// manager's cluster (apps.homes.<home>.allow) -- consent is per
	// cluster, and each home runs its own Manager. Nil means nothing is
	// allowed, which is the default-deny posture the rest of the worker
	// has.
	Allowed func(appID string) bool
	// Levels returns the machine owner's policy.yaml apps.levels entries
	// for one app, and the levels those entries refuse, each with the
	// sentence saying why (tools.Policy.AppLevels). The session lays the
	// entries over the harness's built-in table.
	//
	// Nil means no entries, and every app runs the built-in table
	// (harness.BuiltinLevels) -- which is also what an absent block
	// means. Unlike Allowed, silence here grants nothing: it decides how
	// an allowed app runs, not whether it may.
	Levels func(appID string) (harness.Table, map[string]string)
	// CheckWorkspace vetoes a workspace path. The delegation policy
	// picks the workspace root, but the cockpit still gets to refuse a
	// path outside its own -- the engine is naming a directory on
	// somebody else's machine. A directory this machine chose itself
	// (workspace.go) passes through it too.
	CheckWorkspace func(path string) error
	// Home is the cluster home this manager serves (the fleet's home id).
	// A workspace this machine chooses is filed under it, so two clusters
	// enrolled on one machine never share a directory. Empty files it
	// under "default", the id a home with no cluster URL gets.
	Home string
	// WorkspaceRoot is policy.yaml's fs.workspace_root, already expanded,
	// or "" when it names none (tools.Policy.WorkspaceRoot). A function so
	// a SIGHUP reaches the next session. When it names a root, a workspace
	// this machine chooses goes under it.
	WorkspaceRoot func() string
	// ScratchRoot is where a workspace this machine chooses goes when
	// fs.workspace_root names none. Empty is the platform's data directory
	// (scratchRootFor); tests set it.
	ScratchRoot string
	// WorkerPaths are this worker's own files and directories: its
	// configuration (the worker tokens), policy.yaml (apps.allow, the app
	// consent gate), its state. DenyPaths is policy.yaml's fs.deny, already
	// expanded (tools.Policy.DenyPaths); a function so a SIGHUP reaches the
	// next session.
	//
	// A session may not run in, above or below any of them, and the app is
	// told to neither read nor write one (protected.go).
	WorkerPaths []string
	DenyPaths   func() []string
	// OpenCommand is how the open kind puts a terminal in front of a
	// person. Nil is the platform's (platformOpenCommand); tests set it, so
	// a test does not open a window on the machine running it.
	OpenCommand func(script string) (argv []string, note string, err error)
	// Detector resolves WHICH HARNESS drives an app on this machine.
	//
	// It is a field rather than a package call because the answer is a
	// property of the installed binary: two Codexes answer to the id
	// `codex`, and only a probe of the one on this machine says which.
	// Nil builds a detector here, so the worker's wiring keeps working
	// unchanged; passing the SAME detector the app inventory reports
	// with is better still, because then the harness word a session
	// drives and the harness word the registration advertised come from
	// one cache and cannot be a probe apart.
	Detector *apps.Detector
	// ToolVersions reports the developer tools the session fingerprint
	// lists. Nil probes this machine (fingerprint.go); tests set it, so a
	// session test does not fork every compiler on the machine running it.
	ToolVersions func(ctx context.Context) []harness.ToolVersion
}

// Manager owns every live session on this machine.
type Manager struct {
	opts   Options
	logger *slog.Logger
	// tools is the fingerprint's toolchain probe, shared by every session
	// so its cache is too.
	tools *toolchain

	mu       sync.Mutex
	sessions map[string]*session
}

// NewManager builds a Manager and sweeps whatever a previous process
// left behind.
func NewManager(opts Options) *Manager {
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}
	if opts.HTTPClient == nil {
		opts.HTTPClient = &http.Client{Timeout: libraryTimeout}
	}
	if opts.Detector == nil {
		opts.Detector = &apps.Detector{}
	}
	m := &Manager{opts: opts, logger: logger, sessions: map[string]*session{}, tools: newToolchain()}
	if swept := Sweep(opts.StateDir); swept > 0 {
		// Worth a line at boot: it means a previous process died with a
		// live session, and a bearer sat on disk until now.
		logger.Warn("swept MCP configuration files left by a previous cockpit process",
			"files", swept)
	}
	return m
}

// Start opens a session. It returns immediately; the session runs on its
// own goroutine and reports itself through sender.
func (m *Manager) Start(ctx context.Context, sender Sender, start *memqlv1.AppSessionStart) {
	if start == nil || sender == nil {
		return
	}
	id := start.GetSessionId()
	if strings.TrimSpace(id) == "" {
		m.logger.Warn("app session start with no session id; ignoring")
		return
	}

	runCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	s := &session{
		id:      id,
		start:   start,
		sender:  sender,
		manager: m,
		logger:  m.logger.With("session_id", id, "app", start.GetApp(), "kind", start.GetKind()),
		cancel:  cancel,
		// From the first moment, so a session refused before it reaches
		// the app still redacts the End it sends and the log line that
		// says why.
		redact: newRedactor(start.GetCredential()),
		// Closed until a turn loop opens it. A session that is not
		// driving turns -- the open kind, or one that failed before it
		// started -- must REFUSE a follow-up rather than swallow it into
		// a queue nothing will ever drain.
		turnsClosed: true,
	}

	m.mu.Lock()
	if _, exists := m.sessions[id]; exists {
		m.mu.Unlock()
		cancel()
		// A duplicate start is the server retrying an envelope it thinks
		// was lost. Running it twice would give one session id two
		// processes and two transcripts.
		m.logger.Warn("duplicate app session start ignored")
		return
	}
	m.sessions[id] = s
	m.mu.Unlock()

	go s.run(runCtx)
}

// Control applies a server-side steer to a live session.
func (m *Manager) Control(ctl *memqlv1.AppSessionControl) {
	if ctl == nil {
		return
	}
	m.mu.Lock()
	s := m.sessions[ctl.GetSessionId()]
	m.mu.Unlock()
	if s == nil {
		// A control for a session that already ended is normal: the end
		// and the cancel crossed on the wire.
		return
	}
	switch ctl.GetAction() {
	case ActionCancel:
		s.logger.Info("app session cancelled by the server", "reason", ctl.GetReason())
		s.cancelReason(ctl.GetReason())
	case ActionRenewCredential:
		s.renew(ctl.GetCredential())
	case ActionMessage:
		s.followUp(controlPrompt(ctl))
	default:
		s.logger.Warn("unknown app session control action", "action", ctl.GetAction())
	}
}

// StopAll cancels every live session.
//
// Called when the stream dies and when the cockpit shuts down. A session
// whose stream is gone has nowhere to send chunks and nobody waiting for
// its end, and leaving the process running would be an agent working on
// somebody's machine with nothing watching it -- the same thing cancel
// exists to prevent.
func (m *Manager) StopAll(reason string) {
	m.mu.Lock()
	live := make([]*session, 0, len(m.sessions))
	for _, s := range m.sessions {
		live = append(live, s)
	}
	m.mu.Unlock()
	for _, s := range live {
		s.cancelReason(reason)
	}
}

// Live reports how many sessions are running.
func (m *Manager) Live() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.sessions)
}

// toolVersions is the fingerprint's toolchain: Options.ToolVersions when
// a caller supplied one, this machine's probe otherwise.
func (m *Manager) toolVersions(ctx context.Context) []harness.ToolVersion {
	if m.opts.ToolVersions != nil {
		return m.opts.ToolVersions(ctx)
	}
	return m.tools.versions(ctx)
}

func (m *Manager) forget(id string) {
	m.mu.Lock()
	delete(m.sessions, id)
	m.mu.Unlock()
}

// session is one live app session.
type session struct {
	id      string
	start   *memqlv1.AppSessionStart
	sender  Sender
	manager *Manager
	logger  *slog.Logger
	cancel  context.CancelFunc

	// sendMu is held from the moment a chunk is numbered until it is
	// sent, so chunks reach the stream in the order of their seq. The
	// engine drops a chunk that arrives behind a higher one, and since
	// the recording is sent from the harness's goroutines as well as the
	// narration's, numbering under one lock and sending after it lost
	// whichever lower chunk came second.
	sendMu sync.Mutex
	seqMu  sync.Mutex
	seq    uint64

	// streamed is how many transcript bytes have been SENT, against
	// limits.max_transcript_bytes.
	streamed int64
	capped   bool

	redact *redactor

	mu            sync.Mutex
	mcp           *mcpConfig
	library       *Library
	child         *child
	cancelReason_ string
	// workspace is the directory the session runs in, once resolved --
	// the start's, or one this machine chose (workspace.go).
	// ownsWorkspace is true only for a directory made for this session
	// alone, which is removed when it ends.
	workspace     string
	ownsWorkspace bool
	// deny is what the app may neither read nor write (protected.go),
	// read once with the workspace check so the two agree.
	deny []string
	// policy decides which files the recording reads back (record.go);
	// pulled are the Library inputs as they landed, for the fingerprint.
	policy *contentPolicy
	pulled []pulledInput

	transcript *os.File
	// before is the workspace as it stood when the run started, so the
	// push at the end carries what the run PRODUCED.
	before map[string]fileStamp
	// Files explicitly written by recorded app tools survive the distinction
	// between a new checkout and work product, including after a commit.
	written map[string]bool

	// turnMu guards the follow-up queue and the latch that closes it.
	turnMu sync.Mutex
	// pending are follow-up prompts waiting for the turn in flight.
	pending []string
	// turnsClosed latches when no further turn will be taken, so a
	// follow-up that arrives a moment too late is REFUSED with a reason
	// rather than queued into a loop that has already exited.
	turnsClosed bool

	// usage is what the app reported about itself, if anything.
	usageMu sync.Mutex
	usage   *memqlv1.AppSessionUsage
	// usageGaps counts turns that reported no spend at all. One is
	// enough to make the session's total unknown -- see reportedUsage.
	usageGaps int
	appRef    string
	// result is the LAST turn's structured answer, when it produced one.
	result []byte
	// servedModel and servedEffort are what the app REPORTED serving the
	// last turn that reported a model, kept as a pair -- see recordTurn.
	servedModel  string
	servedEffort string
}

// run drives the whole session and is the only place End is sent.
func (s *session) run(ctx context.Context) {
	defer s.manager.forget(s.id)
	defer func() {
		if rec := recover(); rec != nil {
			// A panic here must still delete the MCP config and still
			// close the session, or the caller waits forever on a run
			// that is already gone.
			s.logger.Error("app session panicked", "panic", rec)
			s.teardown()
			s.keepWorkspace("the session panicked before its outputs were pushed")
			s.sendEnd(-1, fmt.Sprintf("cockpit panic during session: %v", rec), nil)
		}
	}()

	code, err := s.execute(ctx)
	s.teardown()

	artifacts, pushErr := s.pushOutputs(ctx)
	if err == nil && pushErr != nil {
		err = pushErr
	} else if pushErr != nil {
		err = fmt.Errorf("%w; additionally: %v", err, pushErr)
	}
	// After the push, which reads the directory, and before the End, so
	// nobody told the session is over can find it still on disk -- unless
	// the push failed, when the directory holds the only copy of what the
	// app made.
	if pushErr != nil {
		s.keepWorkspace("its outputs could not all be pushed to the Library")
	} else {
		s.releaseWorkspace()
	}

	message := ""
	if err != nil {
		message = err.Error()
	}
	s.sendEnd(code, message, artifacts)
}

// execute resolves the session and runs it, returning the app's real exit
// code. A code of -1 means no process ran.
func (s *session) execute(ctx context.Context) (int, error) {
	spec, err := s.resolveApp(ctx)
	if err != nil {
		return -1, err
	}
	// The level is settled here, before anything is written or fetched: a
	// session this machine will not run at its level costs the refusal and
	// nothing else -- no bearer on disk, no inputs pulled, no transcript
	// pushed. The open kind hands the app to a PERSON, who picks their own
	// model, so it reads no level at all.
	var plan levelPlan
	if s.start.GetKind() != KindOpen {
		if plan, err = s.resolveLevel(spec); err != nil {
			return -1, err
		}
	}
	workspace, err := s.resolveWorkspace()
	if err != nil {
		return -1, err
	}

	var tools []string
	if !s.start.GetInferenceOnly() {
		tools, err = preflightMCP(ctx, s.manager.opts.HTTPClient, s.start.GetMcpEndpoint(), s.start.GetCredential())
	}
	if err != nil {
		if cancelled := s.contextFailure(ctx); cancelled != nil {
			return -1, cancelled
		}
		return -1, err
	}
	// The MCP configuration first: an app that starts without it reaches
	// nothing over MCP and reports that as "MemQL's tools are broken".
	resumeRef := ""
	if s.start.GetKind() == KindAttach {
		resumeRef = s.start.GetAppSessionRef()
	}
	mcp, err := writeMCPConfig(spec.ID, workspace, s.start.GetMcpEndpoint(),
		s.start.GetCredential(), s.id, s.manager.opts.StateDir, sessionMCPSettings{ResumeRef: resumeRef, Tools: tools, DenyPaths: s.denyPaths(), InferenceOnly: s.start.GetInferenceOnly()})
	if err != nil {
		return -1, err
	}
	config, backup := mcp.paths()
	s.mu.Lock()
	s.mcp = mcp
	// What the recording may read back from this workspace: never the
	// session's own scaffolding, which from here on holds the bearer.
	s.policy = newContentPolicy(workspace, s.redact, filepath.Join(workspace, sessionScaffoldDir), config, backup)
	s.mu.Unlock()

	base := s.manager.opts.LibraryBase
	if strings.TrimSpace(base) == "" {
		base, err = LibraryBaseURL(s.manager.opts.ClusterURL)
		if err != nil {
			return -1, err
		}
	}
	library := NewLibrary(base, s.start.GetCredential(), s.manager.opts.HTTPClient)
	s.mu.Lock()
	s.library = library
	s.mu.Unlock()

	// Inputs land BEFORE the app starts. An agent that begins work and
	// finds its inputs half-arrived produces confidently wrong output
	// rather than an error.
	if err := s.pullInputs(ctx, workspace); err != nil {
		return -1, err
	}

	if err := s.openTranscript(workspace); err != nil {
		return -1, err
	}

	// The wall-clock ceiling. 0 means none.
	if max := s.start.GetLimits().GetMaxDurationSeconds(); max > 0 {
		var stop context.CancelFunc
		ctx, stop = context.WithTimeout(ctx, time.Duration(max)*time.Second)
		defer stop()
	}

	// Snapshot before the run so the push at the end carries what the
	// run PRODUCED, not everything already in the directory.
	before := snapshotWorkspace(workspace)
	s.before = before

	// THE FINGERPRINT IS THE SESSION'S FIRST EVENT (fingerprint.go): the
	// world as the app is about to find it -- inputs landed, scaffolding
	// written and left out -- sent before any kind starts anything.
	s.sendFingerprint(ctx, spec, workspace)

	switch s.start.GetKind() {
	case KindOpen:
		return s.runOpen(ctx, spec, workspace)
	case KindAttach:
		ref := strings.TrimSpace(s.start.GetAppSessionRef())
		if ref == "" {
			return -1, errors.New("app session: kind=attach with no app_session_ref names no run to resume")
		}
		if strings.TrimSpace(s.start.GetPrompt()) == "" {
			// Attaching is now RESUMING AND SPEAKING, because that is
			// the only thing either protocol offers: neither Claude
			// Code's headless mode nor Codex's app-server has a "watch
			// the run somebody else started" primitive. A turn with
			// nothing to say would spend the owner's subscription to
			// ask the app nothing, so it is refused by name instead.
			return -1, fmt.Errorf("app session: kind=attach resumes %q and sends it a turn, "+
				"so it needs a prompt; there is no way to stream a run already in flight",
				s.start.GetAppSessionRef())
		}
		return s.runTurns(ctx, spec, workspace, ref, plan)
	case KindRun, "":
		return s.runTurns(ctx, spec, workspace, "", plan)
	default:
		return -1, fmt.Errorf("app session: unknown kind %q", s.start.GetKind())
	}
}

// resolveApp checks the app is one this machine will run, and settles
// WHICH HARNESS drives it here.
//
// The harness comes from the Detector's ResolveSpec rather than from the
// static apps.SpecFor, and that is the whole point: SpecFor carries the
// FLOOR, the protocol that works on every machine with the binary at
// all, so trusting it would drive every Codex in the fleet through
// `codex mcp-server` -- losing usage numbers and structured answers on
// every machine whose Codex has the app-server. ResolveSpec is also the
// same answer the registration advertised, so the engine cannot route a
// protocol here that the session then declines to speak.
//
// The order of the three refusals is deliberate: the closed set first
// (it names the app), then policy (a refusal the operator owns, and no
// reason to fork a probe for an app this machine will not run), then
// PATH.
func (s *session) resolveApp(ctx context.Context) (apps.Spec, error) {
	id := strings.TrimSpace(s.start.GetApp())
	if !apps.IsKnownID(id) {
		return apps.Spec{}, fmt.Errorf("app session: this cockpit has no runner for app %q", id)
	}
	allowed := s.manager.opts.Allowed
	if allowed == nil || !allowed(id) {
		// The engine should never route here -- it derives the routing
		// label from the allowed flag this machine reported. Enforcing
		// it again is the point: policy.yaml is the machine owner's
		// word, and it is checked where it is enforced rather than
		// trusted from a round trip.
		return apps.Spec{}, fmt.Errorf("app session: %q is not allowed for this cluster by this machine's policy.yaml (apps.homes)", id)
	}
	spec, ok := s.manager.opts.Detector.ResolveSpec(ctx, id)
	if !ok {
		// ResolveSpec answers false for "outside the closed set" and for
		// "not on PATH" alike; the first is already excluded above, so
		// this is the second. Name it, because a LaunchAgent's PATH is
		// not the operator's shell PATH and that is the usual cause.
		return apps.Spec{}, fmt.Errorf("app session: %q is allowed here but is not on this worker's PATH", id)
	}
	return spec, nil
}

// levelPlan is what a session's LEVEL became on this machine
// (memql-cockpit#437): the table the harness resolves it through, the
// knobs that produced, and whose entry they were.
type levelPlan struct {
	level string
	table harness.Table
	knobs harness.Knobs
	// owner is true when the level's entry came from the machine owner's
	// policy.yaml rather than the built-in table -- the first thing a
	// person reading the transcript needs when the model is not the one
	// they expected.
	owner    bool
	explicit bool
}

// resolveLevel settles the knobs this session runs at.
//
// The table is the built-in one for the harness THIS machine drives the
// app through (so both Codex harnesses share Codex's), with the owner's
// apps.levels entries laid over it level by level. The harness resolves the
// level through the same table again in its own Start, with the same
// function, so the two cannot disagree; resolving it here as well is what
// lets the refusal come before the session has written or fetched anything.
//
// An owner's entry the app would misread REFUSES its level, in the
// policy's own sentence naming the line to fix, rather than falling back to
// the built-in entry it was written to replace (tools.Policy.AppLevels says
// why). The refusal names the app and the level either way, because the
// person reading it is the one deciding whether to fix a policy file or a
// call site.
func (s *session) resolveLevel(spec apps.Spec) (levelPlan, error) {
	level := s.start.GetLevel()
	var override harness.Table
	var refused map[string]string
	if f := s.manager.opts.Levels; f != nil {
		override, refused = f(spec.ID)
	}
	if reason, ok := refused[level]; ok && level != "" {
		return levelPlan{}, fmt.Errorf("app session: %s", reason)
	}
	table := harness.MergeLevels(harness.BuiltinLevels(spec.Harness), override)
	// A refused level leaves the table too, not only this session: the
	// harness resolves the level again through this table, and a built-in
	// row left standing under a refused entry is the default the owner's
	// entry was written to replace -- one skipped check away from running.
	for level := range refused {
		delete(table, level)
	}
	requested := harness.Knobs{Model: s.start.GetModel(), Effort: s.start.GetEffort()}
	knobs, err := harness.ResolveSessionKnobs(level, table, requested)
	if err == nil {
		err = harness.CheckKnobs(spec.Harness, knobs)
	}
	// An attach resumes a session rather than starting one, and the Codex
	// MCP fallback cannot configure a session it resumes. The harness
	// refuses that too, in Start; asking here is what makes the refusal
	// come before the bearer and the inputs.
	if err == nil && s.start.GetKind() == KindAttach {
		err = harness.CheckResume(spec.Harness, knobs)
	}
	if err != nil {
		return levelPlan{}, fmt.Errorf("app session: %s cannot run at level %q: %w", spec.ID, level, err)
	}
	_, owner := override[level]
	return levelPlan{level: level, table: table, knobs: knobs, owner: owner,
		explicit: strings.TrimSpace(requested.Model) != "" || strings.TrimSpace(requested.Effort) != ""}, nil
}

// levelNote is the line a session writes into its transcript saying what
// its level became here and where that came from, e.g.
//
//	[memql] level reasoning runs claude-code with --model opus --effort xhigh (the cockpit's built-in table)
//
// It is the one place a person reading a session can see that "reasoning"
// meant Opus on this machine -- the End reports what the app SAID it ran,
// and when the two differ, both halves are what explains it.
func levelNote(spec apps.Spec, p levelPlan) string {
	source := "the cockpit's built-in table"
	if p.owner {
		source = "this machine's policy.yaml apps.levels"
	}
	if p.explicit {
		source = "the session's explicit override over its level"
	}
	if p.level == "" {
		return fmt.Sprintf("[memql] session runs %s with %s (the session's explicit override)\n",
			spec.ID, harness.DescribeKnobs(spec.Harness, p.knobs))
	}
	return fmt.Sprintf("[memql] level %s runs %s with %s (%s)\n",
		p.level, spec.ID, harness.DescribeKnobs(spec.Harness, p.knobs), source)
}

// resolveWorkspace settles the directory the session runs in: the one the
// engine named, or -- when it named none -- one this machine chooses
// (workspace.go). Either way it passes this machine's own check before
// anything is written into it.
func (s *session) resolveWorkspace() (string, error) {
	workspace := strings.TrimSpace(s.start.GetWorkspace())
	chosen, bySession := workspace == "", false
	if chosen {
		var err error
		if workspace, bySession, err = s.chooseWorkspace(); err != nil {
			return "", err
		}
	}
	if !filepath.IsAbs(workspace) {
		return "", fmt.Errorf("app session: workspace %q is not absolute", workspace)
	}
	if check := s.manager.opts.CheckWorkspace; check != nil {
		if err := check(workspace); err != nil {
			return "", fmt.Errorf("app session: workspace refused by this machine's policy: %w", err)
		}
	}
	// The workspace is the app's write grant, so it may not reach what the
	// session must never touch (protected.go) -- a check CheckWorkspace does
	// not make, for the engine's path or this machine's own choice.
	protected := s.protectedPaths()
	if err := checkWorkspaceOverlap(workspace, protected); err != nil {
		return "", err
	}
	if err := os.MkdirAll(workspace, configDirMode); err != nil {
		return "", fmt.Errorf("app session: workspace: %w", err)
	}
	// An open session's directory is never this machine's to remove: the
	// person it was handed to can still be working in it after the session
	// ends (a cancel stops the launcher, not their terminal).
	owns := chosen && bySession && s.start.GetKind() != KindOpen
	s.mu.Lock()
	s.workspace = workspace
	s.ownsWorkspace = owns
	s.deny = appDenyPaths(protected)
	s.mu.Unlock()
	if chosen {
		keyedBy := "run"
		if bySession {
			keyedBy = "session"
		}
		s.logger.Info("app session named no workspace; running in one this machine chose",
			"workspace", workspace, "keyed_by", keyedBy, "removed_at_end", owns)
	}
	return workspace, nil
}

// pullInputs fetches every named artifact before the run starts.
func (s *session) pullInputs(ctx context.Context, workspace string) error {
	inputs := s.start.GetInputs()
	if len(inputs) == 0 {
		return nil
	}
	s.mu.Lock()
	library := s.library
	s.mu.Unlock()

	for _, id := range inputs {
		path, err := library.Pull(ctx, id, workspace)
		if err != nil {
			// Name the id that failed. "an input could not be fetched"
			// sends whoever reads this to check all of them.
			return err
		}
		// Where it landed, for the fingerprint's digest of what the app
		// was handed.
		s.mu.Lock()
		s.pulled = append(s.pulled, pulledInput{artifact: id, path: path})
		s.mu.Unlock()
	}
	s.logger.Info("app session inputs landed", "count", len(inputs))
	return nil
}

// runTurns drives the session as a sequence of turns through the app's
// own harness -- the run and attach kinds.
//
// TURNS ARE STRICTLY SEQUENTIAL, and this loop is where that is decided.
// The harness clients refuse a concurrent Turn rather than queueing one
// (Claude Code's is the clearest case: two processes resuming a single
// session id both append to the one transcript on disk and neither sees
// the other's turn, so the conversation silently forgets half of
// itself), and a refusal reaching the ENGINE would be the wrong answer
// to the wrong question -- the engine has no way to know a turn is in
// flight, because chunks are asynchronous and there is no reply to a
// control. So a follow-up QUEUES here and the queue is drained one turn
// at a time. Dropping it instead would lose a prompt a person typed,
// with nothing anywhere saying it had been lost.
//
// The session ends when a turn finishes and the queue is empty, which is
// the behaviour a session had before follow-ups existed: the engine
// waits on an End, and a cockpit that held every session open until it
// was cancelled would park every run for its whole wall-clock ceiling.
func (s *session) runTurns(ctx context.Context, spec apps.Spec, workspace, resumeRef string, plan levelPlan) (int, error) {
	h, err := harness.New(spec.Harness)
	if err != nil {
		// The set of harness words is closed; this is a descriptor and a
		// runner that disagree, which is worth naming loudly because the
		// engine has already committed a turn to this machine.
		return -1, fmt.Errorf("app session: %s: %w", spec.ID, err)
	}

	// Codex's MCP configuration travels as CODEX_HOME in the
	// ENVIRONMENT; Claude Code's travels as the --mcp-config PATH. Both
	// come from the one mcpConfig this session wrote, and both are set
	// here for whichever harness is driving: getting this wrong is
	// silent, because an app with no MemQL server configured runs
	// perfectly well and simply cannot reach a single tool.
	hspec := harness.Spec{
		InferenceOnly:  s.start.GetInferenceOnly(),
		Binary:         spec.Binary,
		Workspace:      workspace,
		Env:            s.mcpEnv(),
		MCPConfigPath:  s.mcpConfigPath(),
		ResponseSchema: startResponseSchema(s.start),
		ResumeRef:      resumeRef,
		Level:          plan.level,
		Levels:         plan.table,
		Model:          s.start.GetModel(),
		Effort:         s.start.GetEffort(),
		DenyPaths:      s.denyPaths(),
		Launch:         s.launcher(),
	}
	if err := h.Start(ctx, hspec); err != nil {
		// Close on the FAILED start too: the app-server harnesses fork
		// in Start, so a handshake that failed can still have left a
		// process holding the machine.
		_ = h.Close()
		return -1, s.turnFailure(ctx, err)
	}
	defer func() { _ = h.Close() }()

	// Said once the harness has taken the level, so the line never
	// describes a session that did not start at it.
	if plan.level != "" || plan.explicit {
		s.logger.Info("app session running at its level",
			"level", plan.level, "model", plan.knobs.Model, "effort", plan.knobs.Effort, "owner_entry", plan.owner)
		_ = s.emitChunk(StreamStderr, []byte(levelNote(spec, plan)))
	}

	// Every chunk the app produces arrives here already classified by
	// the harness, which reads the app's own protocol -- this replaces the
	// "does this line parse as JSON" test that used to stand in for it --
	// and every call the app completes arrives as an Action for the
	// recording (record.go).
	sink := sessionSink{s: s}

	s.openFollowUps()
	prompt := s.start.GetPrompt()
	for {
		res, err := h.Turn(ctx, prompt, sink)
		s.recordTurn(res)
		if refErr := s.mcp.rememberProviderRef(res.AppSessionRef); refErr != nil {
			return res.ExitCode, errors.Join(err, refErr)
		}
		// The app's REAL status, unnormalised. A harness whose process
		// is still alive reports 0 for a protocol-level failure, and
		// that is not a lie the engine acts on: it files a session with
		// a non-empty `error` as failed whatever the code
		// (component/worker/runner.go), so the honest code plus the
		// honest error is a complete report and an invented one would
		// not be.
		code := res.ExitCode
		if err != nil {
			if !errors.Is(err, harness.ErrNoStructuredResult) {
				return code, s.turnFailure(ctx, err)
			}
			// The PROCESS succeeded and the SCHEMA did not. Those bill
			// and retry differently, so this is not a failed run -- but
			// it is also not silence, or whoever asked for a structured
			// answer is left wondering where it went.
			s.logger.Info("the app answered but not against the requested schema")
			_ = s.emitChunk(StreamStderr,
				[]byte("[memql] the app answered, but not against the schema this session asked for; "+
					"no structured result is reported for this turn\n"))
		}
		next, ok := s.nextFollowUp()
		if !ok {
			return code, s.contextFailure(ctx)
		}
		prompt = next
	}
}

// turnFailure reports what a turn's failure should end the session with.
//
// A cancel and the wall-clock ceiling name THEMSELVES, because what the
// harness saw is a process that died mid-sentence and its message would
// send an operator hunting for an app problem that does not exist. Only
// when the context is fine is the app's own account the real one.
func (s *session) turnFailure(ctx context.Context, err error) error {
	if cerr := s.contextFailure(ctx); cerr != nil {
		return cerr
	}
	return err
}

// followUp queues a `message` control's prompt as the next turn.
func (s *session) followUp(prompt string) {
	if prompt == "" {
		// Into the transcript as well as the log, for the reason a refused
		// queue says so there: an empty follow-up is exactly what every
		// follow-up looked like while this runner read the wrong field, and
		// a person reading the session is the one who would have to notice.
		s.logger.Warn("app session message control carried no prompt; ignoring")
		_ = s.emitChunk(StreamStderr, []byte("[memql] a follow-up arrived with no prompt, so no turn was started\n"))
		return
	}
	if err := s.queueFollowUp(prompt); err != nil {
		s.logger.Warn("app session follow-up refused", "error", err)
		// Into the transcript as well as the log: the log is on a laptop
		// nobody is reading, and the person who sent the follow-up is
		// looking at the transcript.
		_ = s.emitChunk(StreamStderr, []byte("[memql] "+err.Error()+"\n"))
		return
	}
	s.logger.Info("app session follow-up queued as the next turn")
}

// queueFollowUp accepts a follow-up, or says why it cannot.
func (s *session) queueFollowUp(prompt string) error {
	s.turnMu.Lock()
	defer s.turnMu.Unlock()
	if s.turnsClosed {
		return errors.New("this session is no longer taking turns, so the follow-up was not delivered")
	}
	if len(s.pending) >= maxQueuedFollowUps {
		return fmt.Errorf("this session already has %d follow-ups waiting, which is the limit, "+
			"so this one was not delivered", maxQueuedFollowUps)
	}
	s.pending = append(s.pending, prompt)
	return nil
}

// openFollowUps lets the queue accept work. Only a running turn loop
// calls it, because only a running turn loop will ever drain it.
func (s *session) openFollowUps() {
	s.turnMu.Lock()
	s.turnsClosed = false
	s.turnMu.Unlock()
}

// nextFollowUp pops the next turn's prompt, or latches the session shut.
//
// The pop and the latch are ONE critical section on purpose. Anything
// less leaves a window in which a follow-up is accepted by a loop that
// has already decided to exit -- and a queued prompt nothing will ever
// drain is exactly the silent loss the refusal exists to prevent.
func (s *session) nextFollowUp() (string, bool) {
	s.turnMu.Lock()
	defer s.turnMu.Unlock()
	if len(s.pending) == 0 {
		s.turnsClosed = true
		return "", false
	}
	next := s.pending[0]
	s.pending = s.pending[1:]
	return next, true
}

// denyPaths is what the app may neither read nor write, as resolveWorkspace
// settled it.
func (s *session) denyPaths() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.deny...)
}

// mcpEnv is the environment the app must run with -- CODEX_HOME for
// Codex, nothing for Claude Code.
func (s *session) mcpEnv() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.mcp.Env()
}

// mcpConfigPath is the file writeMCPConfig laid the bearer down in.
//
// It takes the config's OWN lock rather than reading the field directly.
// Today configPath is written once, before the config is published to
// the session, so a bare read would be safe -- and would stop being safe
// the day a layout writes it twice, in a way no test would show.
func (s *session) mcpConfigPath() string {
	s.mu.Lock()
	mcp := s.mcp
	s.mu.Unlock()
	if mcp == nil {
		return ""
	}
	mcp.mu.Lock()
	defer mcp.mu.Unlock()
	return mcp.configPath
}

// pump starts the stdout and stderr readers and returns a channel closed
// when both have drained to EOF.
func (s *session) pump(c *child, onStdout, onStderr func(string, []byte) error) <-chan struct{} {
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		_ = (&streamReader{name: StreamStdout, src: c.stdout, emit: onStdout, holdBack: s.redact.holdBack()}).run()
	}()
	go func() {
		defer wg.Done()
		_ = (&streamReader{name: StreamStderr, src: c.stderr, emit: onStderr, holdBack: s.redact.holdBack()}).run()
	}()
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	return done
}

// runOpen hands the app to the human -- the open kind.
func (s *session) runOpen(ctx context.Context, spec apps.Spec, workspace string) (int, error) {
	root := filepath.Join(workspace, sessionScaffoldDir)
	if err := os.MkdirAll(root, configDirMode); err != nil {
		return -1, err
	}
	// Open sessions also overlap in a run workspace. Own their launcher and
	// exit receipt together; cleanup must never consume another window's result.
	dir, err := os.MkdirTemp(root, "open-")
	if err != nil {
		return -1, err
	}
	defer os.RemoveAll(dir)
	exitFile := filepath.Join(dir, "exit")
	openCommand := s.manager.opts.OpenCommand
	if openCommand == nil {
		openCommand = platformOpenCommand
	}
	c, note, err := launchOpen(ctx, spec, workspace, s.start.GetPrompt(), s.mcpEnv(), s.mcpConfigPath(), exitFile, openCommand)
	if err != nil {
		// Immediately, with a reason, and with no fallback to headless:
		// the user asked to drive it themselves.
		return -1, err
	}
	s.mu.Lock()
	s.child = c
	s.mu.Unlock()

	_ = s.emitChunk(StreamStderr, []byte(note+"\n"))

	readersDone := s.pump(c, s.emitChunk, s.emitChunk)
	code := waitForOpen(ctx, c, exitFile, readersDone)
	// Usage on this path is normally unknown, and that is correct: a
	// human-driven session does not report tokens back to the cockpit.
	// The engine records it as billing "unknown" rather than as free.
	return code, s.contextFailure(ctx)
}

// contextFailure turns a cancelled or expired context into the named
// error the End should carry, or nil when the run simply finished.
func (s *session) contextFailure(ctx context.Context) error {
	if ctx.Err() == nil {
		return nil
	}
	s.mu.Lock()
	reason := s.cancelReason_
	s.mu.Unlock()
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return fmt.Errorf("app session: exceeded limits.max_duration_seconds (%d)",
			s.start.GetLimits().GetMaxDurationSeconds())
	}
	if reason != "" {
		return fmt.Errorf("app session cancelled: %s", reason)
	}
	return errors.New("app session cancelled")
}

// cancelReason cancels the session, recording why for the End.
func (s *session) cancelReason(reason string) {
	s.mu.Lock()
	if s.cancelReason_ == "" {
		s.cancelReason_ = reason
	}
	c := s.child
	s.mu.Unlock()
	s.cancel()
	if c != nil {
		c.terminate()
	}
}

// renew swaps the bearer in the MCP configuration file and in the
// Library client.
func (s *session) renew(credential string) {
	if strings.TrimSpace(credential) == "" {
		s.logger.Warn("renew_credential carried no credential; ignoring")
		return
	}
	s.mu.Lock()
	mcp, library := s.mcp, s.library
	s.mu.Unlock()

	// Redact the NEW one too, and keep redacting the old: a chunk
	// already buffered may still carry it.
	s.redact.add(credential)
	if library != nil {
		library.SetCredential(credential)
	}
	if mcp != nil {
		if err := mcp.Renew(credential); err != nil {
			// Never log the credential, only the failure.
			s.logger.Error("app session credential renewal failed", "error", err)
			return
		}
	}
	s.logger.Info("app session credential renewed in place")
}

// teardown deletes the MCP configuration and stops the process. Every
// exit path passes through here, including the panic recovery.
func (s *session) teardown() {
	s.mu.Lock()
	mcp, c, transcript := s.mcp, s.child, s.transcript
	s.mcp = nil
	s.mu.Unlock()

	if c != nil {
		c.terminate()
	}
	if mcp != nil {
		mcp.Remove()
	}
	if transcript != nil {
		_ = transcript.Sync()
	}
}
