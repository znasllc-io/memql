// Package capability implements the capability-script contract for a Go
// binary.
//
// The contract (docs/internal/design/capability-script-contract.md) is a SHELL
// surface: `scripts/lib/capability.sh` provides it, and
// `scripts/lib/capability_contract_test.go` enforces it by walking
// `scripts/**/*.sh`. That walk will never see a Go binary, so
// `cmd/memql-bench` claiming to be "under the capability-script contract"
// would otherwise be a claim with nothing behind it.
//
// Two things stand behind it instead. This package emits the same envelope,
// takes the same flags and uses the same exit codes; and its test READS THE
// PRINTF FORMAT STRINGS OUT OF capability.sh AT TEST TIME and asserts the two
// agree. That is a drift gate rather than a copy: when the shell library's
// envelope changes, this goes red instead of silently diverging, which is the
// failure mode a hand-transcribed format string has.
//
// The contract, restated because this file has to implement it exactly:
//
//   - Structured params IN: `--flag=value` and bare `--flag` (which is the
//     string "true", not "1"). No positional arguments. No env tier. An
//     UNDECLARED flag is an error, not an unknown-option warning.
//   - Structured result OUT: exactly ONE JSON envelope on stdout, nothing
//     else. Every human line goes to stderr.
//   - Honest, stable exit codes: 0 ok, 1 generic, 2 bad parameter, 3 refused,
//     4 prerequisite missing, 5 operation failed.
//   - `--print-spec` emits the machine-readable descriptor and exits 0.
//   - No decisions inside: no branching on environment, version or role.
package capability

import (
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
)

// Exit codes. These are the contract's, and they are what a caller branches
// on, so they are consts rather than literals at each call site.
const (
	// ExitOK -- success.
	ExitOK = 0
	// ExitGeneric -- unexpected failure. The `set -e` abort default in shell;
	// here, a panic path or an error with no better code.
	ExitGeneric = 1
	// ExitBadParam -- missing, invalid or unknown parameter.
	ExitBadParam = 2
	// ExitRefused -- a required confirmation was not provided, or a declared
	// ceiling would be exceeded. A refusal is a RESULT, not a crash.
	ExitRefused = 3
	// ExitPrerequisite -- a required tool or resource is absent.
	ExitPrerequisite = 4
	// ExitOpFailed -- the underlying operation failed.
	ExitOpFailed = 5
)

// envelopeFormat and specFormat are the shell library's wire shapes, quoted
// here so the drift gate can compare them against `scripts/lib/capability.sh`
// AND against what this package actually emits.
//
// NOTHING IS EMITTED THROUGH THEM. They embed their values inside literal
// quotes (`"capability":"%s"`), which is the shape that requires the caller to
// hand-escape -- and hand-escaping JSON is a critical `go/unsafe-quoting`
// finding for a good reason: it is correct exactly until somebody passes a
// value carrying a quote, and the failure is a malformed envelope rather than
// an error. Bash has no alternative and so the shell library does it; Go does,
// so this package marshals structs and lets encoding/json own every escape.
//
// The formats therefore earn their place twice over: they pin what the shell
// emits, and TestTheMarshalledShapeMatchesTheFormat pins that the struct
// marshals to the same thing. A change to either side goes red.
const (
	envelopeFormat = `{"ok":%s,"capability":"%s","changed":%s,"result":%s,"error":%s}` + "\n"
	specFormat     = `{"capability":"%s","summary":"%s","params":[%s]}` + "\n"
)

// wireEnvelope is the emitted shape. Field ORDER is the declaration order
// encoding/json preserves, and it matches envelopeFormat -- a caller reading
// the line with `jq` does not care, but a human diffing two runs does.
type wireEnvelope struct {
	OK         bool            `json:"ok"`
	Capability string          `json:"capability"`
	Changed    bool            `json:"changed"`
	Result     json.RawMessage `json:"result"`
	Error      *EnvelopeError  `json:"error"`
}

// wireSpec is the --print-spec shape.
type wireSpec struct {
	Capability string      `json:"capability"`
	Summary    string      `json:"summary"`
	Params     []wireParam `json:"params"`
}

type wireParam struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Required    bool   `json:"required,omitempty"`
}

// Param is one declared flag. The declared set IS the accepted surface: a
// flag not declared here is refused with exit 2, which is what stops
// `--tier=live` silently doing nothing because somebody typed `--teir`.
type Param struct {
	Name        string
	Description string
	Required    bool
}

// Spec is a capability's descriptor: its id, what it does, and its params.
type Spec struct {
	Id      string
	Summary string
	Params  []Param
}

// Capability is one invocation: a spec, the parsed flags, and the accumulating
// result. It is created by Parse and finished by exactly one of OK or Fail.
type Capability struct {
	spec    Spec
	flags   map[string]string
	result  map[string]any
	changed bool
	out     io.Writer
	err     io.Writer
	emitted bool
}

// ParseError carries an exit code alongside its message, so a caller can
// return the contract's code rather than mapping error text back to one.
type ParseError struct {
	Code int
	Msg  string
}

func (e *ParseError) Error() string { return e.Msg }

// Parse reads argv against the spec. It returns a non-nil handled bool when
// a meta flag (`--print-spec`, `--help`) was consumed and written to out, in
// which case the caller exits 0 without running anything.
//
// stdin is deliberately NOT a param tier here. The shell library supports
// `--params-stdin` because a shell script has no good way to take a large
// structured argument; a Go binary does, and adding a tier nothing uses is a
// second spelling that can disagree with the first.
func Parse(spec Spec, argv []string, out, errw io.Writer) (c *Capability, handled bool, err error) {
	c = &Capability{spec: spec, flags: map[string]string{}, result: map[string]any{}, out: out, err: errw}

	declared := map[string]Param{}
	for _, p := range spec.Params {
		declared[p.Name] = p
	}

	for _, arg := range argv {
		switch {
		case arg == "--print-spec":
			c.writeSpec()
			return c, true, nil
		case arg == "--help" || arg == "-h":
			c.writeHelp()
			return c, true, nil
		case !strings.HasPrefix(arg, "--"):
			// No positional arguments, ever. A positional argument is a
			// parameter whose meaning depends on its position, which is
			// exactly what a machine caller cannot get right twice.
			return c, false, &ParseError{ExitBadParam, fmt.Sprintf("positional argument %q: this capability takes --flag=value only", arg)}
		}
		name, value, hasValue := strings.Cut(strings.TrimPrefix(arg, "--"), "=")
		if !hasValue {
			// A bare flag is the string "true", not "1". The shell library
			// settled that (memql#4629) and a Go emitter answering "1" would
			// make `cap_bool`-shaped consumers disagree across the two.
			value = "true"
		}
		if _, ok := declared[name]; !ok {
			return c, false, &ParseError{ExitBadParam, fmt.Sprintf("unknown parameter --%s (declared: %s)", name, strings.Join(declaredNames(spec), ", "))}
		}
		c.flags[name] = value
	}

	for _, p := range spec.Params {
		if p.Required && strings.TrimSpace(c.flags[p.Name]) == "" {
			return c, false, &ParseError{ExitBadParam, "missing required parameter: " + p.Name}
		}
	}
	return c, false, nil
}

func declaredNames(spec Spec) []string {
	out := make([]string, 0, len(spec.Params))
	for _, p := range spec.Params {
		out = append(out, "--"+p.Name)
	}
	sort.Strings(out)
	return out
}

// Param returns a flag's value, or def when it was not given.
func (c *Capability) Param(name, def string) string {
	if v, ok := c.flags[name]; ok && v != "" {
		return v
	}
	return def
}

// Bool returns a flag as a boolean. The truthy and falsey sets are the shell
// library's, case-insensitively, and ANY other value is an error rather than
// a silent false -- `--dry-run=ture` must not run for real.
func (c *Capability) Bool(name string, def bool) (bool, error) {
	raw, ok := c.flags[name]
	if !ok {
		return def, nil
	}
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "true", "1", "yes", "y", "on":
		return true, nil
	case "", "false", "0", "no", "n", "off":
		return false, nil
	}
	return false, &ParseError{ExitBadParam, fmt.Sprintf("--%s=%q is neither true nor false", name, raw)}
}

// Set records a result field.
func (c *Capability) Set(key string, value any) { c.result[key] = value }

// Changed marks the capability as having changed something. It is what makes
// an idempotent re-run distinguishable from a first run in the envelope.
func (c *Capability) Changed() { c.changed = true }

// Info writes a human line to STDERR. Nothing human ever goes to stdout: the
// single JSON envelope is what stdout is for, and one stray Println makes the
// whole invocation unparseable.
func (c *Capability) Info(format string, a ...any) {
	fmt.Fprintf(c.err, "INFO:  "+format+"\n", a...)
}

// Warn writes a warning line to stderr.
func (c *Capability) Warn(format string, a ...any) {
	fmt.Fprintf(c.err, "WARN:  "+format+"\n", a...)
}

// Step writes a progress line to stderr.
func (c *Capability) Step(format string, a ...any) {
	fmt.Fprintf(c.err, "==> "+format+"\n", a...)
}

// OK emits the success envelope and returns the exit code. Calling it twice,
// or after Fail, is a programming error: the contract is EXACTLY ONE envelope,
// and a second one makes the first unparseable to a caller reading one line.
func (c *Capability) OK() int {
	if c.emitted {
		panic("proving/capability: a second envelope was emitted; the contract is exactly one")
	}
	c.emitted = true
	c.emit(wireEnvelope{OK: true, Capability: c.spec.Id, Changed: c.changed, Result: c.resultJSON()})
	return ExitOK
}

// Fail emits the failure envelope and returns the exit code. A code outside
// 1..255 is clamped to 1, matching the shell library, because an exit code of
// 0 on a failure envelope is the one inconsistency a caller cannot recover
// from.
func (c *Capability) Fail(code int, format string, a ...any) int {
	if code < 1 || code > 255 {
		code = ExitGeneric
	}
	msg := fmt.Sprintf(format, a...)
	if c.emitted {
		// Do not emit a second envelope, but do say what happened -- on
		// stderr, where it cannot corrupt the first.
		fmt.Fprintf(c.err, "ERROR: %s (an envelope was already emitted)\n", msg)
		return code
	}
	c.emitted = true
	c.emit(wireEnvelope{
		OK: false, Capability: c.spec.Id, Changed: c.changed, Result: c.resultJSON(),
		Error: &EnvelopeError{Code: code, Message: msg},
	})
	return code
}

// emit writes one envelope, on one line. The single Marshal is the whole
// escaping story: a capability id, a message or a result value carrying a
// quote, a tab or a NUL comes out as valid JSON without this package knowing
// the escaping rules.
func (c *Capability) emit(e wireEnvelope) {
	b, err := json.Marshal(e)
	if err != nil {
		// Unreachable: every field is a marshallable type and Result is
		// already-valid JSON. A caller parsing one line still needs one line,
		// so say so in the envelope's own shape rather than writing nothing.
		fmt.Fprintf(c.out, "{\"ok\":false,\"capability\":\"\",\"changed\":false,\"result\":{},\"error\":{\"code\":1,\"message\":\"envelope encoding failed\"}}\n")
		fmt.Fprintf(c.err, "ERROR: encoding the envelope: %v\n", err)
		return
	}
	fmt.Fprintf(c.out, "%s\n", b)
}

// resultJSON marshals the accumulated result. A result that will not marshal
// is reported IN the envelope rather than replacing it, because a caller
// parsing one line needs one line.
func (c *Capability) resultJSON() json.RawMessage {
	b, err := json.Marshal(c.result)
	if err != nil {
		fallback, _ := json.Marshal(map[string]string{"resultEncodingError": err.Error()})
		return fallback
	}
	return b
}

func (c *Capability) writeSpec() {
	out := wireSpec{Capability: c.spec.Id, Summary: c.spec.Summary, Params: []wireParam{}}
	for _, p := range c.spec.Params {
		out.Params = append(out.Params, wireParam{Name: p.Name, Description: p.Description, Required: p.Required})
	}
	b, err := json.Marshal(out)
	if err != nil {
		fmt.Fprintf(c.err, "ERROR: encoding the spec: %v\n", err)
		return
	}
	fmt.Fprintf(c.out, "%s\n", b)
	c.emitted = true
}

func (c *Capability) writeHelp() {
	fmt.Fprintf(c.out, "Capability: %s\n", c.spec.Id)
	fmt.Fprintf(c.out, "  %s\n\n", c.spec.Summary)
	fmt.Fprintf(c.out, "Parameters:\n")
	for _, p := range c.spec.Params {
		req := ""
		if p.Required {
			req = " (required)"
		}
		fmt.Fprintf(c.out, "  --%s=<value>%s\n      %s\n", p.Name, req, p.Description)
	}
	fmt.Fprintf(c.out, "\n  --print-spec   machine-readable descriptor\n  --help         this text\n")
	c.emitted = true
}

// Envelope is the parsed shape, for a caller (or a test) that reads one back.
type Envelope struct {
	OK         bool           `json:"ok"`
	Capability string         `json:"capability"`
	Changed    bool           `json:"changed"`
	Result     map[string]any `json:"result"`
	Error      *EnvelopeError `json:"error"`
}

// EnvelopeError is the failure block.
type EnvelopeError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// ParseEnvelope reads one envelope back. It exists so the CI job and the tests
// read the wire shape through the same code the emitter is checked against,
// rather than each writing their own struct.
func ParseEnvelope(line []byte) (Envelope, error) {
	var e Envelope
	if err := json.Unmarshal(line, &e); err != nil {
		return Envelope{}, fmt.Errorf("proving/capability: %w", err)
	}
	return e, nil
}
