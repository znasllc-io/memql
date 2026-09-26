package proving

// procedure_world.go -- the proving world as a learned procedure's steps and
// the fixture app both reach it (epic memql#5408).
//
// ONE WORLD, TWO CALLERS. The fixture app performs a goal's actions here, and
// a canary or trusted replay dispatches the same actions here; the fake
// machine counts both, and a delivery one of them repeats is a duplicate
// whoever made it. That is the whole of the durability claim this epic adds:
// when a replay stops part-way and the app takes over, the world must see
// every side effect exactly once -- and the world is not the thing that
// prevents a second one. It RECORDS a duplicate (package world's rule); the
// platform's guidance to the app is what must keep it from happening.
//
// A SHADOW replay never reaches this world. It runs beside the app in a
// sandbox, and a sandboxed command is answered by a scratch copy of the same
// machine, so its answer is comparable with the app's and its delivery lands
// nowhere -- exactly as a shadow replay's distinct workspace means in
// production.

import (
	"fmt"
	"path"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/znasllc-io/memql/component/proving/scenario"
	"github.com/znasllc-io/memql/component/proving/world"
)

// The fake machine's environment, as both the fixture app's fingerprint and a
// replay's precondition probe read it. Fixed values rather than this host's,
// so a recording made on a developer's laptop and a replay in CI describe the
// same machine -- which the proving world is.
const (
	provingMachineOS   = "linux"
	provingMachineArch = "amd64"
	// provingToolVersion is every fake script's version. A procedure learns
	// the version of each tool its commands invoke (D16), so the value is
	// checked on every replay; its being one number for all of them is a
	// fact about the fake, not a shortcut in the check.
	provingToolVersion = "1.0.0"
)

// commandNotFound is the exit code a shell gives a command it cannot find, and
// what the fake machine answers for a script it does not declare.
const commandNotFound = 127

// ProcedureWorld wraps the scenario's world with the failures the CURRENT goal
// injects.
type ProcedureWorld struct {
	world   *world.World
	scripts map[string]string
	steps   []scenario.Step

	mu    sync.Mutex
	armed map[string]scenario.Injection // script -> the current goal's injection
	fired []string
}

// execAnswer is what the fake machine answered one command with, in the terms
// an app and a replay both report: the exit code, the output, and whether a
// delivery reached the world.
type execAnswer struct {
	Stdout   string
	ExitCode int
	IsError  bool
	Error    string
	// Delivered reports that the command reached the world and was counted --
	// false for an injected failure, a sandboxed command and an unknown
	// script alike.
	Delivered bool
}

func newProcedureWorld(s scenario.Scenario, w *world.World) *ProcedureWorld {
	scripts := map[string]string{}
	if s.World.Machine != nil {
		for k, v := range s.World.Machine.Scripts {
			scripts[k] = v
		}
	}
	return &ProcedureWorld{world: w, scripts: scripts, steps: s.Steps, armed: map[string]scenario.Injection{}}
}

// armGoal makes one goal's injections live, replacing whatever an earlier goal
// left unfired: an injection belongs to the goal that declares it, and one
// carried into the next goal would fail an action nobody aimed it at.
func (p *ProcedureWorld) armGoal(injections []scenario.Injection) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.armed = map[string]scenario.Injection{}
	for _, in := range injections {
		for _, st := range p.steps {
			if st.Key == in.At {
				p.armed[st.Script()] = in
			}
		}
	}
}

// firedInjections lists what fired, in order, for a failure message.
func (p *ProcedureWorld) firedInjections() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.fired...)
}

// exec runs one command on the fake machine.
//
// The world keys a delivery on the command's ARGUMENT VECTOR, not its text: a
// replay materializes a command from its template and may quote it
// differently from the app, and a duplicate the world missed because two
// spellings of one command differed would be a zero measured by accident.
func (p *ProcedureWorld) exec(command, idempotencyKey string, sandbox bool) execAnswer {
	argv := splitCommand(command)
	if len(argv) == 0 {
		return execAnswer{ExitCode: commandNotFound, IsError: true, Error: "empty command"}
	}
	script := path.Base(argv[0])

	if !sandbox {
		// An injected failure fires on the world's REAL answer only. A
		// shadow's sandbox is a copy of the machine, and a failure aimed at
		// this goal's delivery is not a fact about the copy.
		p.mu.Lock()
		in, armed := p.armed[script]
		if armed {
			delete(p.armed, script)
			p.fired = append(p.fired, fmt.Sprintf("%s (%s) under %s", in.At, in.Kind, idempotencyKey))
		}
		p.mu.Unlock()
		if armed {
			// NOTHING IS DELIVERED. The injection is the machine refusing the
			// command, so a later successful run of it is its first delivery,
			// not its second -- which is what lets the durability figure count
			// only what a divergence genuinely repeats.
			return execAnswer{ExitCode: 1, IsError: true, Error: in.Message}
		}
	}

	target := p.world
	if sandbox {
		target = world.New(world.Config{Scripts: p.scripts})
	}
	out, err := target.RunScript(script, strings.Join(argv, "\x1f"), idempotencyKey)
	if err != nil {
		return execAnswer{ExitCode: commandNotFound, IsError: true, Error: err.Error()}
	}
	return execAnswer{Stdout: out, ExitCode: 0, Delivered: !sandbox}
}

// fingerprint is the environment a session starts in, in the cockpit's shape
// (memql-cockpit internal/worker/harness/record.go, Fingerprint) -- the shape
// component/procedure.LearnPreconditions reads. Every script the scenario's
// actions run is a tool at provingToolVersion, and a session starts in an
// empty workspace.
func (p *ProcedureWorld) fingerprint(workspace string, takenAt time.Time) map[string]any {
	tools := make([]any, 0, len(p.steps))
	seen := map[string]bool{}
	for _, st := range p.steps {
		if name := st.Script(); name != "" && !seen[name] {
			seen[name] = true
			tools = append(tools, map[string]any{"name": name, "version": provingToolVersion})
		}
	}
	return map[string]any{
		"type":    "fingerprint",
		"v":       1,
		"seq":     0,
		"takenAt": takenAt.UTC().Format(time.RFC3339),
		"app": map[string]any{
			"id": fixtureAppId, "version": fixtureAppVersion, "harness": "proving",
		},
		"platform":   map[string]any{"os": provingMachineOS, "arch": provingMachineArch},
		"tools":      tools,
		"cwd":        workspace,
		"cwdEntries": 0,
		"variables":  []any{},
		"inputs":     []any{},
	}
}

// splitCommand splits a command line into its argument vector the way a POSIX
// shell does for words: single quotes literal, double quotes with backslash
// escapes, a backslash escaping the next rune, whitespace separating. It is
// the inverse of component/procedure.Materialize's argv quoting, which
// single-quotes an element that needs it and writes a quote inside one by
// closing the quotes, escaping it with a backslash, and reopening them.
func splitCommand(s string) []string {
	var (
		out     []string
		cur     strings.Builder
		inWord  bool
		quote   rune
		escaped bool
	)
	for _, r := range s {
		switch {
		case escaped:
			cur.WriteRune(r)
			escaped = false
			inWord = true
		case quote == '\'':
			if r == '\'' {
				quote = 0
			} else {
				cur.WriteRune(r)
			}
		case quote == '"':
			switch r {
			case '"':
				quote = 0
			case '\\':
				escaped = true
			default:
				cur.WriteRune(r)
			}
		case r == '\\':
			escaped = true
			inWord = true
		case r == '\'' || r == '"':
			quote = r
			inWord = true
		case unicode.IsSpace(r):
			if inWord {
				out = append(out, cur.String())
				cur.Reset()
				inWord = false
			}
		default:
			cur.WriteRune(r)
			inWord = true
		}
	}
	if inWord {
		out = append(out, cur.String())
	}
	return out
}
