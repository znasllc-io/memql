package harness

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"path/filepath"
	"strings"
)

// record.go is the RECORDING's wire shape: memql-cockpit#440, the cockpit
// half of epic B in the engine's
// docs/superpowers/specs/2026-09-13-app-session-recording-and-learning-program-design.md
// (decisions D2, D5, D12 and D16).
//
// ONE SHAPE, WHICHEVER APP. Every tool call an app completes leaves the
// session as one `event` chunk carrying an Action, and the session's
// first event is a Fingerprint. Both are normalised here, on the machine,
// out of each app's own protocol -- Claude Code's tool_use / tool_result
// pairs, the Codex app-server's items, the Codex mcp-server's begin / end
// events -- so the engine never parses a vendor format. A reader that has
// to know Claude Code calls it `Bash` and Codex calls it
// `commandExecution` has been handed a parser for two formats nobody
// promised to keep, which is the thing this package exists to delete.
//
// NO PROTO CHANGE. AppSessionChunk already carries `stream` and a
// monotonic `seq`, and an `event` chunk is DEFINED as a JSON body. The
// type words below are namespaced for the reason resultEventType's is:
// the same stream carries the apps' own events verbatim, and Claude
// Code's last line is literally {"type":"result"}.
//
// THE ENGINE PARSES NONE OF THIS YET. Its half (memql#5396) turns these
// events into work-spine rows; until it lands, an engine at the pin
// appends every chunk to the session's bounded transcript and maps every
// event chunk onto a live progress line, which is where these land too.
// So this file DEFINES the shape rather than transcribing one, the way the
// model labels' `params` and `quant` were defined here first, and
// TestActionWireContract pins the names so a rename is a decision.
//
// EVERY UNKNOWN STAYS UNKNOWN. A fact the app did not report is ABSENT,
// never zero: an exit code nobody reported is not 0, a call whose result
// never arrived did not succeed, and a file this machine would not read
// has no digest. The engine records an absence as an absence; a zero it
// would record as a fact.

// The two event types, and the version both shapes carry.
const (
	// ActionEventType names an event chunk carrying one Action.
	ActionEventType = "memql.app_session.action"
	// FingerprintEventType names the session's first event chunk.
	FingerprintEventType = "memql.app_session.fingerprint"
	// RecordVersion moves only for a change a reader cannot skip over. An
	// added field is not one: a reader ignores a key it does not know.
	RecordVersion = 1
)

// The action kinds, the closed vocabulary Action.Tool is drawn from.
//
// The first five are the engine's step types (its sixth, app_answer, is
// written from the End, not from here). The last two are this side's,
// and the difference between them is the difference between "known to
// touch nothing" and "not known at all":
//
//   - agent is the app's own bookkeeping: loading a deferred tool,
//     keeping its to-do list, handing a sub-task to a sub-agent whose own
//     calls are recorded separately. Nothing outside the app moved.
//   - other is a tool this package cannot classify, and its effects are
//     UNKNOWN. That is the fail-closed reading: a replay has to treat the
//     call as one it cannot reproduce, where guessing `agent` would let a
//     procedure skip a call that sent a notification.
const (
	ActionExec    = "exec"
	ActionFSRead  = "fs_read"
	ActionFSWrite = "fs_write"
	ActionFetch   = "fetch"
	ActionMCP     = "mcp"
	ActionAgent   = "agent"
	ActionOther   = "other"
)

// What an action did to the file a Content names.
const (
	ContentRead   = "read"
	ContentWrite  = "write"
	ContentDelete = "delete"
)

// Why a Content carries no bytes: a closed set, because the engine
// records it as the file's omission and a replay reads each one
// differently. over_ceiling, over_budget and digest_only still carry the
// digest, so the file can be compared; too_large carries only the size;
// the rest could not be read at all, or -- contains_credential -- were
// read and must not leave.
const (
	// OmittedOverCeiling: larger than one file may be inline.
	OmittedOverCeiling = "over_ceiling"
	// OmittedOverBudget: the action had no room left for these bytes --
	// earlier files in it spent its inline budget, or its own arguments
	// left it too large to carry them.
	OmittedOverBudget = "over_budget"
	// OmittedTooLarge: too large to hash; the size is still reported.
	OmittedTooLarge = "too_large"
	// OmittedOutsideWorkspace: the path leaves the session's workspace.
	OmittedOutsideWorkspace = "outside_workspace"
	// OmittedScaffolding: the session's own files -- the MCP
	// configuration with the per-run bearer in it, the transcript.
	OmittedScaffolding = "session_scaffolding"
	// OmittedNotFound: nothing is at the path.
	OmittedNotFound = "not_found"
	// OmittedNotRegular: a directory, a device, a pipe or a socket.
	OmittedNotRegular = "not_regular"
	// OmittedUnreadable: it could not be opened or read.
	OmittedUnreadable = "unreadable"
	// OmittedDigestOnly: the recording carries this file's digest and, by
	// rule, not its bytes -- a read whose bytes the app did not itself
	// report whole (`head -1 .env` read one line of a file of secrets), or
	// a file inside a dependency or version-control directory.
	OmittedDigestOnly = "digest_only"
	// OmittedCredential: the file holds this session's own bearer.
	OmittedCredential = "contains_credential"
)

// Why an Action carries no arguments.
const (
	// ArgsTooLarge: the arguments would have made the action too large
	// to send; their digest stands in for them.
	ArgsTooLarge = "too_large"
)

// How inline bytes are carried.
const (
	EncodingUTF8   = "utf8"
	EncodingBase64 = "base64"
)

// DigestPrefix names the hash every digest in the recording is.
const DigestPrefix = "sha256:"

// Digest is the recording's digest of b.
func Digest(b []byte) string {
	sum := sha256.Sum256(b)
	return DigestPrefix + hex.EncodeToString(sum[:])
}

// ArgsDigest is the digest an Action carries for arguments too large to
// send: over their canonical JSON, the encoding resultDigest uses for a
// value, so arguments that differ only in spacing or key order are one
// set of arguments. Arguments that are not JSON at all are digested as
// their bytes.
func ArgsDigest(args json.RawMessage) string {
	if v, ok := decodeValue(args); ok {
		if canon, err := canonicalJSON(v); err == nil {
			return Digest(canon)
		}
	}
	return Digest(args)
}

// Action is one tool call an app completed, normalised.
//
// Every pointer is a fact that can be ABSENT, and absent means the app
// did not say -- never zero.
type Action struct {
	// Type is ActionEventType and V is RecordVersion.
	Type string `json:"type"`
	V    int    `json:"v"`
	// Seq numbers the session's actions densely from 1, in the order they
	// completed, across every turn. It is NOT the chunk seq: it counts
	// actions only, so a reader holding 1, 2 and 4 knows exactly one call
	// is missing, where a gap in the chunk seq cannot say whether the lost
	// chunk was a call or a line of prose. The fingerprint is 0.
	Seq uint64 `json:"seq"`
	// Turn is the session's turn the call completed in, from 1. A
	// follow-up is a new turn, so this is what ties a call to the prompt
	// that asked for it.
	Turn int `json:"turn"`
	// ID is the app's OWN id for the call -- Claude Code's tool_use id,
	// Codex's item or call id -- and the only join back to the app's own
	// transcript.
	ID string `json:"id"`
	// ParentID is the call this one ran inside, when the app said so
	// (Claude Code's parent_tool_use_id, on a sub-agent's calls).
	ParentID string `json:"parentId,omitempty"`
	// Tool is the kind, from the closed set above; AppTool is the app's
	// own name for the tool, kept as provenance and never branched on.
	Tool    string `json:"tool"`
	AppTool string `json:"appTool"`
	// Args is the call's arguments WHOLE, as the app expressed them:
	// Claude Code's tool input, or the Codex item's own fields for a call
	// the app-server reports as an item rather than as arguments. Whole,
	// except in the one case that cannot travel at all: arguments that
	// would make the action larger than the stream carries (a Codex patch
	// deleting a large file carries the whole file) are replaced by null,
	// with ArgsDigest over them and ArgsOmitted saying why -- as are
	// Command, URL and Query, which are read out of them.
	Args        json.RawMessage `json:"args"`
	ArgsDigest  string          `json:"argsDigest,omitempty"`
	ArgsOmitted string          `json:"argsOmitted,omitempty"`
	// Cwd is the directory the call ran in, as far as the app said. Codex
	// names it on every command. Claude Code names it once, on init, and
	// its Bash tool keeps a `cd` across calls without reporting one -- so
	// a command that moved carries the move in its own text.
	Cwd string `json:"cwd"`
	// Command is the command line an exec ran, as one string.
	Command string `json:"command,omitempty"`
	// MCP is the server and tool an mcp call reached.
	MCP *MCPTarget `json:"mcp,omitempty"`
	// URL and Query are what a fetch reached for.
	URL   string `json:"url,omitempty"`
	Query string `json:"query,omitempty"`
	// ExitCode is the status the app REPORTED for an exec; absent when it
	// reported none.
	ExitCode *int `json:"exitCode,omitempty"`
	// IsError is the app's own verdict on the call. Absent when nobody
	// knows: an Incomplete call, an end event this build could not read,
	// or a call the app reports no verdict on at all (a Codex web search
	// or image view carries no status).
	IsError *bool `json:"isError,omitempty"`
	// ResultType is the inferred JSON type of the result -- object,
	// array, string, number, boolean or null. Text that parses as JSON is
	// typed by what it parses as, so a command that printed an object
	// returned an object; any other text is a string.
	ResultType string `json:"resultType,omitempty"`
	// ResultDigest is over the result: its exact text when it is text, its
	// canonical JSON otherwise. (ResultType, ResultDigest) together are
	// the result's identity.
	ResultDigest string `json:"resultDigest,omitempty"`
	// Contents are the files the call read or wrote, one entry each.
	Contents []Content `json:"contents,omitempty"`
	// Incomplete marks a call the app STARTED that the session never saw
	// finish -- the process died, the turn was cancelled, or its result
	// was too large to read. Recorded rather than dropped, so a gap reads
	// as a gap.
	Incomplete bool `json:"incomplete,omitempty"`
}

// MCPTarget is where an MCP call went.
type MCPTarget struct {
	Server string `json:"server"`
	Tool   string `json:"tool"`
}

// Content is one file an action touched.
//
// A harness fills Op and Path and nothing else. The SESSION reads the
// file, because what this machine lets leave it is the machine's policy
// rather than a protocol's (appsession/record.go).
type Content struct {
	Op   string `json:"op"`
	Path string `json:"path"`
	// Digest is over the file's bytes as they stood when the call
	// completed -- the whole file, not the window the app looked at.
	Digest string `json:"digest,omitempty"`
	// Bytes is the file's size.
	Bytes *int64 `json:"bytes,omitempty"`
	// Encoding and Data carry the bytes inline when they fit: the text
	// itself for utf8, base64 otherwise. A present Data may be empty -- an
	// empty file is still a file.
	Encoding string  `json:"encoding,omitempty"`
	Data     *string `json:"data,omitempty"`
	// Omitted says why Data is absent, from the closed set above.
	Omitted string `json:"omitted,omitempty"`

	// Seen is what the app ITSELF reported of this file's bytes, when it
	// reported them -- Claude Code's Read record, the output of a Codex
	// command whose own parse says it read this one file. It never goes on
	// the wire. A read's bytes travel inline only when the file still holds
	// exactly these: then they left the machine already, in the app's own
	// report, and the recording has widened nothing.
	Seen []byte `json:"-"`
}

// contentFor names one file for the session to read, made absolute
// against cwd when the app named it relatively.
func contentFor(op, path, cwd string) []Content {
	path = strings.TrimSpace(path)
	if path == "" {
		return nil
	}
	if !filepath.IsAbs(path) && cwd != "" {
		path = filepath.Join(cwd, path)
	}
	return []Content{{Op: op, Path: filepath.Clean(path)}}
}

// Fingerprint is what the world looked like when the session started
// (D16): the session's FIRST event, seq 0.
//
// Every part of it is a fact a later replay compares before it acts,
// which is why anything that could identify the person is a digest
// rather than a value -- a variable's value can carry a home directory or
// a token, and "the same as last time" needs only the hash.
//
// The files the session READ are not here, because at the start it has
// read none: each read is on its own action, as a Content with op read,
// digested at the moment it happened -- the only moment that says what
// the app saw.
type Fingerprint struct {
	Type    string `json:"type"`
	V       int    `json:"v"`
	Seq     uint64 `json:"seq"`
	TakenAt string `json:"takenAt"`
	// App is what drove the session. Harness is empty for an `open`
	// session, which a person drives rather than a protocol.
	App      FingerprintApp `json:"app"`
	Platform Platform       `json:"platform"`
	// Tools are the developer tools on this machine's PATH that recorded
	// commands are most likely to run, each as it reported its own
	// version. A tool that is not installed is not in the list.
	Tools []ToolVersion `json:"tools"`
	// Cwd is the workspace. CwdDigest is over its LISTING -- names and
	// kinds, never contents, never the session's own scaffolding -- and
	// CwdEntries counts the entries the digest covers. Both are absent
	// when the workspace could not be listed.
	Cwd          string `json:"cwd"`
	CwdDigest    string `json:"cwdDigest,omitempty"`
	CwdEntries   *int   `json:"cwdEntries,omitempty"`
	CwdTruncated bool   `json:"cwdTruncated,omitempty"`
	// Variables are the environment variables the harness names as the
	// ones that change what a command does, each as set-or-not plus a
	// digest of its value.
	Variables []Variable `json:"variables"`
	// Inputs are the Library artifacts the session was handed, as they
	// landed in the workspace before the app started.
	Inputs []Input `json:"inputs"`
}

// FingerprintApp names the app, its version as it reports itself, and
// the harness that drove it.
type FingerprintApp struct {
	ID      string `json:"id"`
	Version string `json:"version,omitempty"`
	Harness string `json:"harness,omitempty"`
}

// Platform is the operating system and architecture, as Go names them.
type Platform struct {
	OS   string `json:"os"`
	Arch string `json:"arch"`
}

// ToolVersion is one tool's own report of its version.
type ToolVersion struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

// Variable is one environment variable: set or not, and a digest of its
// value when set. Unset and set-to-empty are different facts, so Set is
// carried even when false.
type Variable struct {
	Name   string `json:"name"`
	Set    bool   `json:"set"`
	Digest string `json:"digest,omitempty"`
}

// Input is one Library artifact as it landed in the workspace.
type Input struct {
	Artifact string `json:"artifact"`
	Path     string `json:"path"`
	Digest   string `json:"digest,omitempty"`
	Bytes    *int64 `json:"bytes,omitempty"`
	Omitted  string `json:"omitted,omitempty"`
}

// fingerprintCommonVariables change what ANY command does: which binary
// a name resolves to, which shell reads it, how its output is sorted and
// worded, and what time it thinks it is.
var fingerprintCommonVariables = []string{"PATH", "SHELL", "LANG", "LC_ALL", "TZ"}

// FingerprintVariables returns the environment variables the fingerprint
// records for a session driven through harness word.
//
// The harness names them because only the harness knows its app. Claude
// Code reads its settings -- permissions, allowed tools, its own MCP
// servers -- from CLAUDE_CONFIG_DIR, and ANTHROPIC_MODEL changes which
// model answers. CODEX_HOME is deliberately NOT named: the cockpit points
// it at a fresh directory for every session, so its digest would differ
// on every start by construction and match nothing.
func FingerprintVariables(word string) []string {
	out := append([]string(nil), fingerprintCommonVariables...)
	if word == HarnessClaudeHeadless {
		out = append(out, "CLAUDE_CONFIG_DIR", "ANTHROPIC_MODEL")
	}
	return out
}
