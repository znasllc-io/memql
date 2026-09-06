// Package skills is the capability graph's runtime side: running a skill's
// script wherever the step needs it to run, and copying a script a run
// discovered back into the Library under the skill that used it.
//
// ===========================================================================
// runScript IS A COMPOSITION, NOT A SEVENTH VERB
// ===========================================================================
// The workbench and a fleet machine each speak the same six primitives --
// exec, fs_read, fs_write, fs_list, fs_stat, http_fetch -- and the reroute
// between them (integrations/agent/workbench_reroute_agent.go) is written on
// the fact that both sides carry the identical set. Adding a seventh verb
// would mean adding it on both sides AND in the cockpit, which ships from a
// different repository on its own release cadence: a capability that could
// not be used until every machine in every fleet had been upgraded.
//
// So runScript ships the script with fs_write, verifies it with fs_read and
// runs it with exec (spec section C). Nothing new crosses the wire, every
// existing gate still runs -- the environment hint, the safety classifier,
// the scope check, the exec allowlist -- and a machine that has never heard
// of this feature executes it correctly on the day it lands.
//
// ===========================================================================
// SHIPPED BY CONTENT HASH, AND VERIFICATION IS A PRECONDITION OF RUNNING
// ===========================================================================
// A skill names its script by Library artifact, never by a path on somebody's
// machine, and the bytes are addressed by their SHA-256. The far side is
// verified by reading the file back and hashing what is actually on that
// disk, rather than by asking the far side to hash it:
//
//   - a remote `sha256sum` is a binary that has to exist, has to be on PATH,
//     has to be spelled the same on macOS (`shasum -a 256`) and has to be in
//     the exec allowlist -- four ways to fail that have nothing to do with
//     whether the bytes arrived;
//   - and a hash the far side computes is a hash the far side chose. Reading
//     the bytes back and hashing them here answers the question actually
//     being asked, which is whether the file on that machine is the file in
//     the Library.
//
// The cost is that verification is bounded by the far side's fs_read cap, so
// a script larger than MaxVerifiableBytes is REFUSED rather than run
// unverified. A script we cannot verify is a script we do not run: the whole
// point of shipping by hash is that the thing that executes on somebody's
// machine is the thing the catalog holds.
package skills

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"path"
	"sort"
	"strings"
	"time"
)

// MaxVerifiableBytes is the far side's fs_read ceiling
// (integrations/workbench/dispatch.go maxFSReadBytes, and the cockpit's
// matching cap). A script at or under it can be read back whole and hashed;
// past it, verification is impossible and the call is refused.
const MaxVerifiableBytes = 1 << 20

// Where a shipped script lands. Under the working directory rather than a
// system temp dir so it is inside the workbench's own containment -- safeJoin
// refuses an absolute path, and on a fleet machine the cockpit's policy roots
// are the boundary.
const scriptDirName = ".memql-scripts"

// The refusal codes this capability owns. Every one of them means NOTHING
// RAN: each is returned before exec, which is what lets a caller retry or
// reroute on any of them without asking whether the script already had an
// effect.
const (
	// ErrSkillNotFound -- the skill row is absent, or the caller may not read
	// it. Those are one answer by design (row admission returns no rows
	// rather than an error), so the message says both.
	ErrSkillNotFound = "skill_not_found"
	// ErrNoScriptForPlatform -- the skill carries scripts, none for the
	// platform this call would run on.
	ErrNoScriptForPlatform = "no_script_for_platform"
	// ErrScriptUnreadable -- the Library file behind the script could not be
	// fetched.
	ErrScriptUnreadable = "script_unreadable"
	// ErrScriptTooLargeToVerify -- past MaxVerifiableBytes; see the package
	// comment.
	ErrScriptTooLargeToVerify = "script_too_large_to_verify"
	// ErrScriptHashMismatch -- what came back off the far side's disk is not
	// what was sent. The bytes are left in place deliberately: they are the
	// evidence.
	ErrScriptHashMismatch = "script_hash_mismatch"
	// ErrScriptShipFailed -- the fs_write did not land.
	ErrScriptShipFailed = "script_ship_failed"
	// ErrNoSurface -- neither surface was wired into this node.
	ErrNoSurface = "no_script_surface"
)

// SurfaceName is where a step ran.
const (
	SurfaceWorkbench = "workbench"
	SurfaceMachine   = "machine"
)

// Script is one entry of a skill's `scripts[]`.
type Script struct {
	// Platform is linux, darwin, windows or any.
	Platform string `json:"platform"`
	// ArtifactID is the Library artifact holding the bytes. A skill never
	// names a path on a machine -- that is the whole point of capture.
	ArtifactID string `json:"artifactId"`
	// Entry is the command line, with `{script}` standing for the shipped
	// file's path on the far side. A script that does not name `{script}`
	// gets it appended, because "run this script" is the only thing the
	// entry can mean.
	Entry string `json:"entry"`
	// ArgsSchema documents the arguments; it is not validated here.
	ArgsSchema map[string]any `json:"argsSchema,omitempty"`
}

const platformAny = "any"

// PickScript chooses the entry for a platform. Exact match wins over `any`,
// and among equals the first in declared order wins -- the skill's author
// ordered them and this does not second-guess it.
//
// An EMPTY platform matches only `any`. That is deliberate: "we do not know
// what we are running on" must not select the linux script by accident.
func PickScript(scripts []Script, platform string) (Script, bool) {
	platform = strings.ToLower(strings.TrimSpace(platform))
	var fallback Script
	haveFallback := false
	for _, s := range scripts {
		p := strings.ToLower(strings.TrimSpace(s.Platform))
		if p == platform && platform != "" {
			return s, true
		}
		if p == platformAny && !haveFallback {
			fallback, haveFallback = s, true
		}
	}
	return fallback, haveFallback
}

// CallResult is one primitive's answer, normalized across the two surfaces.
type CallResult struct {
	OK        bool
	ErrorCode string
	ErrorMsg  string
	Payload   map[string]any
}

// Surface is one place a script can run. Both implementations adapt a
// dispatcher that already exists, so nothing here knows about workspaces,
// routing policies or gRPC.
type Surface interface {
	// Name is SurfaceWorkbench or SurfaceMachine.
	Name() string
	// Platform is the GOOS the surface runs, or "" when it is not known
	// before the call. PickScript treats "" as "only an `any` script".
	Platform(ctx context.Context, req Request) string
	// Call runs one primitive.
	Call(ctx context.Context, req Request, action string, args map[string]any) (CallResult, error)
}

// Request is what the caller asked for.
type Request struct {
	SkillID string
	// ScriptArtifactID pins one of the skill's scripts. Empty means "pick by
	// platform", which is the ordinary case.
	ScriptArtifactID string
	// Args are appended to the entry command, already quoted by the caller's
	// own rules. They are NOT interpolated into the script.
	Args []string
	// PlanID scopes the workbench workspace and is the fleet's per-task
	// approval key. Both dispatchers refuse without it.
	PlanID  string
	AgentID string
	OwnerID string
	// StepID, when this call is a step of a run, is where the receipt is
	// stamped. Empty for an ad-hoc call.
	StepID string
	RunID  string
	// Environment is the {os, needs[]} hint, passed through to the surface
	// unchanged so the workbench's own mismatch rules apply.
	Environment map[string]any
	// RequireLabels forces the fleet. An empty map is not a requirement.
	RequireLabels map[string]string
	TimeoutSec    int
}

// Receipt is what the call produces: enough to say what ran, where, on which
// bytes, and what it did. It is the value stamped onto the step's binding.
type Receipt struct {
	SkillID     string `json:"skillId"`
	ArtifactID  string `json:"artifactId"`
	ContentHash string `json:"contentHash"`
	// Verified is true only when the far side's own bytes hashed to
	// ContentHash. There is no third state: a call that could not verify
	// never reached exec.
	Verified   bool   `json:"verified"`
	Surface    string `json:"surface"`
	Platform   string `json:"platform,omitempty"`
	Path       string `json:"path"`
	Command    string `json:"command"`
	ExitCode   int    `json:"exitCode"`
	Signal     string `json:"signal,omitempty"`
	Stdout     string `json:"stdout,omitempty"`
	Stderr     string `json:"stderr,omitempty"`
	DurationMs int64  `json:"durationMs"`
	// Shipped is false when the far side already held the identical bytes,
	// which is what content addressing buys: the second run of a script does
	// not re-send it.
	Shipped bool `json:"shipped"`
}

// SkillScripts is the narrow read runScript needs off a skill row. The
// resolver is injected so this package needs neither the engine nor the DSL.
type SkillScripts struct {
	SkillID string
	Slug    string
	Scripts []Script
	Active  bool
}

// SkillResolver reads a skill's scripts under the caller's own actor. An
// absent skill and an unreadable one are one answer -- row admission returns
// no rows rather than an error -- so the resolver returns (zero, false) for
// both and the refusal says so.
type SkillResolver interface {
	SkillScripts(ctx context.Context, skillID string) (SkillScripts, bool, error)
}

// ScriptBytes is the Library read: an artifact's content and its hash.
type ScriptBytes struct {
	Data   []byte
	Sha256 string
	Name   string
}

// ArtifactReader fetches a Library artifact's bytes. The hash is recomputed
// here rather than trusted off the row: the row's sha256 is a dedup hint that
// is legitimately ABSENT for a chunked upload the analysis pass has not
// reached yet, and "the field was empty" must not become "the script is
// whatever arrived".
type ArtifactReader interface {
	ReadArtifact(ctx context.Context, artifactID string) (ScriptBytes, error)
}

// Runner is the composition.
type Runner struct {
	skills    SkillResolver
	artifacts ArtifactReader
	workbench Surface
	fleet     Surface
	now       func() time.Time

	// The capture half, wired separately by WithLibrary. runScript works
	// without them; Capture refuses by name without them.
	artifactWriter ArtifactWriter
	skillWriter    SkillWriter
}

// NewRunner builds one. Either surface may be nil -- a node that hosts
// neither refuses with ErrNoSurface rather than pretending.
func NewRunner(skills SkillResolver, artifacts ArtifactReader, workbench, fleet Surface) *Runner {
	return &Runner{skills: skills, artifacts: artifacts, workbench: workbench, fleet: fleet, now: time.Now}
}

// Refusal is a named failure that ran nothing.
type Refusal struct {
	Code    string
	Message string
}

func (r Refusal) Error() string { return r.Code + ": " + r.Message }

func refuse(code, format string, a ...any) Refusal {
	return Refusal{Code: code, Message: fmt.Sprintf(format, a...)}
}

// chooseSurface decides where the script runs.
//
// The workbench is the default and the fleet is the exception, which is the
// same preference the whole platform states (`cognitionReply.tmpl`, the
// workbench knowledge domain): the sandboxed workspace touches nothing of the
// person's, so it is what a step should reach for unless the work genuinely
// cannot happen there.
//
// Exactly two things move a call to the fleet, and both are the CALLER
// saying so rather than this code inferring it:
//
//   - a label requirement, which only a machine can satisfy;
//   - an environment need the workbench does not provide. The workbench's own
//     evaluator is the authority on that, so the hint is passed through
//     unchanged and its `environment_mismatch` is what reroutes -- this
//     function only reads the SHAPE of the hint to avoid a round trip it
//     knows will be refused.
func (r *Runner) chooseSurface(req Request) (Surface, error) {
	wantsMachine := len(req.RequireLabels) > 0 || needsBeyondWorkbench(req.Environment)
	if wantsMachine {
		if r.fleet != nil {
			return r.fleet, nil
		}
		// No fleet on this node. Falling back to the workbench would run the
		// step in the one place the caller just said it cannot run.
		return nil, refuse(ErrNoSurface, "this step requires a machine and no fleet surface is wired on this node")
	}
	if r.workbench != nil {
		return r.workbench, nil
	}
	if r.fleet != nil {
		return r.fleet, nil
	}
	return nil, refuse(ErrNoSurface, "no workbench and no fleet surface is wired on this node")
}

// needsBeyondWorkbench reads the hint's `needs` for anything the sandbox
// structurally does not have. It is deliberately a SHAPE check and not a
// policy: the workbench's evaluator remains the authority, and an
// unrecognised need is left for it to refuse by name
// (`invalid_environment_hint`) rather than being read here as "send it to a
// machine". A typo must never route somebody's script onto their laptop.
func needsBeyondWorkbench(env map[string]any) bool {
	raw, ok := env["needs"]
	if !ok {
		return false
	}
	list, ok := raw.([]any)
	if !ok {
		return false
	}
	for _, item := range list {
		switch strings.TrimSpace(strings.ToLower(fmt.Sprint(item))) {
		case "display", "gpu", "macos_tooling", "user_files":
			return true
		}
	}
	return false
}

// Run ships, verifies and executes.
func (r *Runner) Run(ctx context.Context, req Request) (Receipt, error) {
	if strings.TrimSpace(req.SkillID) == "" {
		return Receipt{}, refuse(ErrSkillNotFound, "skillId is required")
	}
	surface, err := r.chooseSurface(req)
	if err != nil {
		return Receipt{}, err
	}

	skill, found, err := r.skills.SkillScripts(ctx, req.SkillID)
	if err != nil {
		return Receipt{}, refuse(ErrSkillNotFound, "reading skill %s: %v", req.SkillID, err)
	}
	if !found {
		return Receipt{}, refuse(ErrSkillNotFound, "skill %s is not readable here -- it does not exist, or it is not yours", req.SkillID)
	}

	platform := surface.Platform(ctx, req)
	script, ok := selectScript(skill.Scripts, req.ScriptArtifactID, platform)
	if !ok {
		return Receipt{}, refuse(ErrNoScriptForPlatform,
			"skill %s carries no script for %s", describeSkill(skill), describePlatform(platform))
	}

	bytesIn, err := r.artifacts.ReadArtifact(ctx, script.ArtifactID)
	if err != nil {
		return Receipt{}, refuse(ErrScriptUnreadable, "reading script artifact %s: %v", script.ArtifactID, err)
	}
	hash := sha256Hex(bytesIn.Data)
	if len(bytesIn.Data) > MaxVerifiableBytes {
		return Receipt{}, refuse(ErrScriptTooLargeToVerify,
			"script %s is %d bytes and the far side can only read %d back, so it cannot be verified",
			script.ArtifactID, len(bytesIn.Data), MaxVerifiableBytes)
	}

	remotePath := path.Join(scriptDirName, hash[:16]+scriptExt(bytesIn.Name, script.Entry))
	receipt := Receipt{
		SkillID:     skill.SkillID,
		ArtifactID:  script.ArtifactID,
		ContentHash: hash,
		Surface:     surface.Name(),
		Platform:    platform,
		Path:        remotePath,
	}

	// SHIP ONLY IF IT IS NOT ALREADY THERE. Content addressing is what makes
	// this safe: the path IS the hash, so a file already at that path with
	// the right bytes is by construction the right file, and the check costs
	// one fs_read instead of re-sending the script on every step.
	existing, present, err := r.readRemote(ctx, surface, req, remotePath)
	if err != nil {
		return receipt, err
	}
	if present && sha256Hex(existing) == hash {
		receipt.Verified = true
	} else {
		if err := r.ship(ctx, surface, req, remotePath, bytesIn.Data); err != nil {
			return receipt, err
		}
		receipt.Shipped = true
		back, gotBack, err := r.readRemote(ctx, surface, req, remotePath)
		if err != nil {
			return receipt, err
		}
		if !gotBack {
			return receipt, refuse(ErrScriptHashMismatch, "the script is not on %s after it was written", surface.Name())
		}
		if got := sha256Hex(back); got != hash {
			return receipt, refuse(ErrScriptHashMismatch,
				"the script on %s hashes to %s and the Library holds %s -- nothing was run", surface.Name(), got, hash)
		}
		receipt.Verified = true
	}

	command := buildCommand(script.Entry, remotePath, req.Args)
	receipt.Command = command

	started := r.now()
	execArgs := map[string]any{"cmd": command}
	if req.TimeoutSec > 0 {
		execArgs["timeoutSec"] = req.TimeoutSec
	}
	res, err := surface.Call(ctx, req, "exec", execArgs)
	receipt.DurationMs = r.now().Sub(started).Milliseconds()
	if err != nil {
		return receipt, err
	}
	if !res.OK {
		// A refusal from the surface -- a scope denial, a kill switch, an
		// environment mismatch -- is returned VERBATIM. This layer knows the
		// script is verified and shipped; it does not know better than the
		// dispatcher why the dispatcher said no, and translating the code
		// here is what would make a reroutable refusal unrecognisable to the
		// thing that reroutes.
		return receipt, Refusal{Code: res.ErrorCode, Message: res.ErrorMsg}
	}
	receipt.ExitCode = intFrom(res.Payload["exitCode"])
	receipt.Signal = stringFrom(res.Payload["signal"])
	receipt.Stdout = stringFrom(res.Payload["stdout"])
	receipt.Stderr = stringFrom(res.Payload["stderr"])
	if d := intFrom(res.Payload["durationMs"]); d > 0 {
		receipt.DurationMs = int64(d)
	}
	return receipt, nil
}

func (r *Runner) ship(ctx context.Context, surface Surface, req Request, remotePath string, data []byte) error {
	res, err := surface.Call(ctx, req, "fs_write", map[string]any{
		"path":    remotePath,
		"content": string(data),
		// Executable by its owner. The entry runs it through an interpreter
		// in the ordinary case, so this is belt and braces rather than the
		// mechanism.
		"mode": "0700",
	})
	if err != nil {
		return err
	}
	if !res.OK {
		return Refusal{Code: firstNonEmpty(res.ErrorCode, ErrScriptShipFailed), Message: res.ErrorMsg}
	}
	return nil
}

// readRemote reads the shipped file back. A read that fails because the file
// is not there is (nil, false, nil) -- an absence, not an error -- because
// the first ship of a script is the ordinary case and must not look like a
// failure.
func (r *Runner) readRemote(ctx context.Context, surface Surface, req Request, remotePath string) ([]byte, bool, error) {
	res, err := surface.Call(ctx, req, "fs_read", map[string]any{
		"path":     remotePath,
		"maxBytes": MaxVerifiableBytes,
	})
	if err != nil {
		return nil, false, err
	}
	if !res.OK {
		return nil, false, nil
	}
	// TRUNCATED IS NOT READ. A file the far side clipped hashes to something
	// else, and reporting that as a mismatch would send somebody looking for
	// a corruption that never happened.
	if boolFrom(res.Payload["truncated"]) {
		return nil, false, refuse(ErrScriptTooLargeToVerify,
			"the far side truncated the script at %d bytes, so it cannot be verified", MaxVerifiableBytes)
	}
	return []byte(stringFrom(res.Payload["content"])), true, nil
}

// selectScript resolves which entry to run. A pinned artifact id wins and is
// NOT filtered by platform: the caller named one, and silently running a
// different script than the one asked for is worse than failing.
func selectScript(scripts []Script, pinnedArtifactID, platform string) (Script, bool) {
	if pinnedArtifactID != "" {
		for _, s := range scripts {
			if s.ArtifactID == pinnedArtifactID {
				return s, true
			}
		}
		return Script{}, false
	}
	return PickScript(scripts, platform)
}

// buildCommand puts the shipped path into the entry. `{script}` is the
// placeholder; an entry without it gets the path appended, because an entry
// that never names the script can only mean "run it".
func buildCommand(entry, remotePath string, args []string) string {
	entry = strings.TrimSpace(entry)
	quotedPath := shellQuote(remotePath)
	var head string
	switch {
	case entry == "":
		head = quotedPath
	case strings.Contains(entry, "{script}"):
		head = strings.ReplaceAll(entry, "{script}", quotedPath)
	default:
		head = entry + " " + quotedPath
	}
	parts := make([]string, 0, len(args)+1)
	parts = append(parts, head)
	for _, a := range args {
		parts = append(parts, shellQuote(a))
	}
	return strings.Join(parts, " ")
}

// shellQuote wraps a value in single quotes, which is the one quoting form
// with no escapes inside it. An embedded single quote is closed, escaped and
// reopened -- the standard POSIX idiom, and the only case that needs care.
//
// This is not defence against a hostile argument: the exec allowlist and the
// safety classifier are. It is what stops a path with a space in it from
// becoming two arguments.
func shellQuote(v string) string {
	return "'" + strings.ReplaceAll(v, "'", `'\''`) + "'"
}

// scriptExt keeps the artifact's extension so the interpreter named in the
// entry sees the suffix it expects. It falls back to the entry's own hint,
// then to nothing -- a suffix is a courtesy, never a requirement.
func scriptExt(name, entry string) string {
	if ext := path.Ext(strings.TrimSpace(name)); ext != "" && len(ext) <= 8 {
		return ext
	}
	switch {
	case strings.Contains(entry, "python"):
		return ".py"
	case strings.Contains(entry, "node"):
		return ".js"
	case strings.Contains(entry, "bash"), strings.Contains(entry, "sh "):
		return ".sh"
	}
	return ""
}

func describeSkill(s SkillScripts) string {
	if s.Slug != "" {
		return s.Slug
	}
	return s.SkillID
}

func describePlatform(p string) string {
	if strings.TrimSpace(p) == "" {
		return "a platform it could not determine"
	}
	return p
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

func stringFrom(v any) string {
	s, _ := v.(string)
	return s
}

func boolFrom(v any) bool {
	b, _ := v.(bool)
	return b
}

// intFrom narrows a decoded payload number. The three arms are what
// core/num's saturate answer does for the values a dispatch payload can
// actually carry (an exit code, a duration), and anything else answers zero
// -- "not reported" rather than a guess.
func intFrom(v any) int {
	switch n := v.(type) {
	case int:
		return n
	case int64:
		if n > int64(^uint(0)>>1) {
			return int(^uint(0) >> 1)
		}
		if n < -int64(^uint(0)>>1)-1 {
			return -int(^uint(0)>>1) - 1
		}
		return int(n)
	case float64:
		if n > float64(int64(^uint(0)>>1)) {
			return int(^uint(0) >> 1)
		}
		if n < float64(-int64(^uint(0)>>1)-1) {
			return -int(^uint(0)>>1) - 1
		}
		return int(n)
	default:
		return 0
	}
}

// SortScripts orders a skill's scripts for display: exact platforms
// alphabetically, `any` last. The catalog view and the skill row both read
// it, so it lives here rather than in either.
func SortScripts(in []Script) []Script {
	out := append([]Script(nil), in...)
	sort.SliceStable(out, func(i, j int) bool {
		pi, pj := strings.ToLower(out[i].Platform), strings.ToLower(out[j].Platform)
		if (pi == platformAny) != (pj == platformAny) {
			return pj == platformAny
		}
		return pi < pj
	})
	return out
}
