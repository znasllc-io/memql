package models

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
)

// probeTimeout bounds one runtime probe. Discovery runs on the Register
// path and again whenever the worker re-checks its inventory; a runtime
// that accepts a connection and then says nothing must not hold the
// worker's registration behind it.
const probeTimeout = 5 * time.Second

// DeclaredRuntime is an OpenAI-compatible endpoint the machine's
// policy.yaml declares (spec D1: LM Studio, vLLM, llamafile, and Ollama's
// own /v1 surface all reach the cockpit this way).
type DeclaredRuntime struct {
	// Transcription chooses the actual local server protocol: chat (default),
	// openai (/audio/transcriptions), or whisper-cpp (/inference).
	Transcription string `yaml:"transcription"`
	// Voices maps user-facing male/female preferences to this runtime's voice IDs.
	Voices    map[string]string `yaml:"voices"`
	Name      string            `yaml:"name"`
	BaseURL   string            `yaml:"base_url"`
	APIKeyEnv string            `yaml:"api_key_env"`
	Models    []DeclaredModel   `yaml:"models"`
}

// DeclaredModel is one model on a declared runtime, with the attributes
// the operator states.
//
// THEY ARE STATED RATHER THAN PROBED, and that is not laziness. An
// OpenAI-compatible `/v1/models` returns ids and nothing else -- no
// context length, no capability flag, no size. An attribute the machine
// cannot observe has to be declared or left absent; inferring one from a
// model name would be a guess that fails at parse time, three layers from
// here.
//
// UNDECLARED IS ABSENT, for every field. Silence is not a zero this side
// makes up: an unstated size sorts last in the engine's ranking (D5),
// where a params of 0 would be this machine claiming a size it never
// read, and an unstated tools keeps the model out of tool turns (D11)
// rather than into one it cannot serve. The single exception is
// max_concurrent, which probeDeclared floors at 1 -- absent means
// UNLIMITED to the engine, so it is the one attribute silence must not
// reach it as.
//
// params, quant and tools are spelled here as the LABEL spells them,
// unlike context_window and structured_output: an operator comparing
// `quant=Q4_K_M` on the Fleet page against this file should read one word
// in both places, whereas `ctx` and `structured` are abbreviations nobody
// would guess from a policy.yaml.
type DeclaredModel struct {
	ID               string `yaml:"id"`
	ContextWindow    int    `yaml:"context_window"`
	StructuredOutput bool   `yaml:"structured_output"`
	Embeddings       bool   `yaml:"embeddings"`
	MaxConcurrent    int    `yaml:"max_concurrent"`
	// Params is the parameter COUNT, written out (7620000000), not the
	// human string a model card prints. It is what the engine ranks on.
	Params       int64 `yaml:"params"`
	ActiveParams int64 `yaml:"activeparams"`
	// Quant is the quantization level, an operator-facing string that
	// gates nothing and breaks the label if it carries a ',' or an '='
	// -- quantSafe drops such a level rather than emitting it.
	Quant string `yaml:"quant"`
	// Tools says this runtime can carry a tool-calling turn for this
	// model. Separate from StructuredOutput because on a declared runtime
	// they are two independent claims; a runtime that serves a schema and
	// refuses tool calls is an ordinary thing to own.
	Tools bool `yaml:"tools"`

	// The four MODALITIES (engine memql#5137, record D4).
	//
	// THREE OF THE FOUR HAVE NO PROBE AT ALL, and a declaration is the
	// only way they can ever be true. Ollama reports a `vision`
	// capability and reports nothing about audio in, audio out or image
	// generation -- so an operator running a Kokoro speech runtime or a
	// transcription endpoint states it here, and a machine that states
	// nothing offers nothing. That is the fail-closed direction, and
	// the alternative was never "probe harder": it was guessing from a
	// model id, which advertises a modality on the strength of a name
	// and fails on somebody else's prompt.
	//
	// Spelled as the LABEL spells them, like params/quant/tools and
	// unlike context_window: an operator comparing `audioout=1` on the
	// Fleet page against this file reads one word in both places.
	Vision   bool `yaml:"vision"`
	AudioIn  bool `yaml:"audio_in"`
	AudioOut bool `yaml:"audio_out"`
	ImageGen bool `yaml:"image_gen"`
}

// Request is what discovery is asked to consider.
type Request struct {
	// Allow is policy.yaml models.allow. Empty offers nothing.
	Allow []string
	// Runtimes are the declared OpenAI-compatible endpoints.
	Runtimes []DeclaredRuntime
	// Parallel is policy.yaml models.parallel, validated. Zero is "not said".
	Parallel int
	// Resident is policy.yaml models.resident, validated. Empty changes
	// nothing about how any model is called.
	Resident []Resident
}

// Discoverer probes this machine. Every external fact is a seam, so the
// whole package is exercisable without Ollama, without a GPU and without
// a network -- which matters because CI has none of the three, and a
// discovery that can only be driven by hand is one nobody drives.
type Discoverer struct {
	// HTTPClient defaults to a client with probeTimeout.
	HTTPClient *http.Client
	// Getenv defaults to os.Getenv.
	Getenv func(string) string
	// OllamaBaseURL overrides where Ollama is looked for. Empty resolves
	// OLLAMA_HOST, then the documented default.
	OllamaBaseURL string
	// FloorFn defaults to Floor.
	FloorFn func() FloorVerdict
	// ManagedParallel reports the OLLAMA_NUM_PARALLEL of a runtime this
	// cockpit manages -- the unit it wrote, read where it lives -- or zero
	// when there is none or it says nothing. A seam rather than a file read
	// here, because this package knows nothing about systemd and the
	// package that wrote the unit is the one that can read it back. Nil
	// means there is no such runtime (Docker, a Mac, one started by hand).
	ManagedParallel func() int
	// ParallelEnv answers "what OLLAMA_NUM_PARALLEL does the WORKER have", for a
	// process that is not the worker. It returns the variable's text and the
	// name of the place it was read, as the tail of a sentence ("the worker's
	// environment file (~/.memql/worker.env)").
	//
	// Nil means this process IS the worker, and Getenv is its environment. A
	// command that runs in the owner's shell is not: the worker takes its
	// environment from its unit, so the shell's value is the shell's, and
	// reading it names a ceiling the worker does not advertise. When set, this
	// replaces Getenv for the ceiling and for nothing else (OLLAMA_HOST is
	// still Getenv's).
	ParallelEnv func() (value, from string)
}

// DefaultOllamaBaseURL is where Ollama listens unless told otherwise.
const DefaultOllamaBaseURL = "http://127.0.0.1:11434"

func (d *Discoverer) getenv(k string) string {
	if d != nil && d.Getenv != nil {
		return d.Getenv(k)
	}
	return os.Getenv(k)
}

func (d *Discoverer) client() *http.Client {
	if d != nil && d.HTTPClient != nil {
		return d.HTTPClient
	}
	return &http.Client{Timeout: probeTimeout}
}

func (d *Discoverer) floor() FloorVerdict {
	if d != nil && d.FloorFn != nil {
		return d.FloorFn()
	}
	return Floor()
}

// Discover returns what this machine offers.
//
// Below the hardware floor it probes NOTHING and returns the verdict
// alone. That is the cheap direction as well as the correct one: this
// runs on every registration, and a machine that can never serve a model
// has no reason to reach for a runtime socket to prove it.
func (d *Discoverer) Discover(ctx context.Context, req Request) Inventory {
	floor := d.floor()
	if !floor.Met {
		return Inventory{Floor: floor}
	}
	return d.Probe(ctx, req)
}

// Probe runs discovery WITHOUT the floor gate, reporting the verdict
// alongside what it found.
//
// `memql worker models` uses this: an operator below the floor still
// wants to see that they have Ollama with three models pulled, because
// "you have models but this machine cannot serve them" and "you have no
// models" send them to entirely different places. Inventory.Advertised
// still returns nothing, so nothing here can leak into a registration.
func (d *Discoverer) Probe(ctx context.Context, req Request) Inventory {
	inv := Inventory{Floor: d.floor()}

	allow := make(map[string]bool, len(req.Allow))
	for _, a := range req.Allow {
		if id := strings.TrimSpace(a); id != "" {
			allow[id] = true
		}
	}

	found, note := d.probeOllama(ctx, req)
	if note != "" {
		inv.ProbeNotes = append(inv.ProbeNotes, note)
	}
	inv.Models = append(inv.Models, found...)
	if len(found) > 0 {
		// The version is read only when the runtime ALREADY ANSWERED
		// with models, so this adds no round trip to a machine that has
		// no Ollama -- the case the probe is fastest on today, and the
		// one that must not become slower.
		inv.RuntimeVersions = setRuntimeVersion(inv.RuntimeVersions, KindOllama, d.ollamaVersion(ctx))
	}

	for _, rt := range req.Runtimes {
		got, n := d.probeDeclared(ctx, rt)
		if n != "" {
			inv.ProbeNotes = append(inv.ProbeNotes, n)
		}
		inv.Models = append(inv.Models, got...)
	}

	// The allow verdict is applied last and to everything, so a model
	// present on two runtimes gets the same answer on both.
	for i := range inv.Models {
		inv.Models[i].Allowed = allow[inv.Models[i].ID]
	}

	// Sorted by id, then runtime, so the same machine produces the same
	// labels every time. An unstable order would rewrite the registration
	// row on every reconnect for no actual change.
	sort.Slice(inv.Models, func(i, j int) bool {
		if inv.Models[i].ID != inv.Models[j].ID {
			return inv.Models[i].ID < inv.Models[j].ID
		}
		return inv.Models[i].Runtime < inv.Models[j].Runtime
	})
	inv.Models, inv.ProbeNotes = resolveDuplicates(inv.Models, inv.ProbeNotes)
	inv.ProbeNotes = append(inv.ProbeNotes, residentNotes(inv.Models, req.Resident)...)
	return inv
}

// setRuntimeVersion records a version, dropping an empty one rather
// than storing a blank entry. The map's presence is not the
// advertisement -- the LABEL is -- so an entry with no version says
// nothing that its absence does not.
func setRuntimeVersion(m map[string]string, kind, version string) map[string]string {
	if strings.TrimSpace(version) == "" {
		return m
	}
	if m == nil {
		m = map[string]string{}
	}
	m[kind] = strings.TrimSpace(version)
	return m
}

// ollamaVersion asks /api/version. A runtime that does not answer it
// keeps an empty version, which is "present, version unknown" -- the
// label is still written, because the runtime is still there.
func (d *Discoverer) ollamaVersion(ctx context.Context) string {
	var body struct {
		Version string `json:"version"`
	}
	if err := d.getJSON(ctx, d.ollamaBaseURL()+"/api/version", "", &body); err != nil {
		return ""
	}
	return strings.TrimSpace(body.Version)
}

// resolveDuplicates keeps ONE entry per model id.
//
// A model id is a label KEY -- `model:<id>` -- so two runtimes offering
// the same id cannot both be advertised: the second silently overwrites
// the first's attributes, and Find would then hand a call to whichever
// entry sorted first. The machine would be telling the cluster one
// context window and serving from a runtime with another, and nothing
// anywhere would report a conflict.
//
// A DECLARED runtime wins over the native Ollama probe. The operator went
// out of their way to write that entry down, and it is the documented way
// to correct what the probe inferred -- declaring a model against Ollama's
// own /v1 surface with structured_output: true is how you overrule the
// `tools` heuristic. If the auto-discovered entry shadowed it, that escape
// hatch would quietly not work.
func resolveDuplicates(in []Info, notes []string) ([]Info, []string) {
	out := make([]Info, 0, len(in))
	for i := 0; i < len(in); {
		j := i
		for j < len(in) && in[j].ID == in[i].ID {
			j++
		}
		group := in[i:j]
		i = j
		if len(group) == 1 {
			out = append(out, group[0])
			continue
		}
		pick := 0
		for k, m := range group {
			if m.Kind == KindOpenAICompatible {
				pick = k
				break
			}
		}
		out = append(out, group[pick])
		for k, m := range group {
			if k == pick {
				continue
			}
			notes = append(notes, fmt.Sprintf(
				"model %s is offered by both %s and %s; serving it from %s, because a model id can be advertised once",
				m.ID, group[pick].Runtime, m.Runtime, group[pick].Runtime))
		}
	}
	return out, notes
}

// ResolvedOllamaBaseURL is where this machine's Ollama is, resolved the
// way the discoverer itself resolves it. Named for the resolution rather
// than the field beside it because the OllamaBaseURL FIELD is only the
// override -- usually empty -- and a caller reading that and finding ""
// would conclude the machine has no Ollama.
//
// It is exported for ONE caller and for one reason: internal/worker/
// inference pulls a model over Ollama's HTTP API, and a pull is the thing
// a machine does when it has NO models yet -- which is exactly the state
// in which nothing else can tell it where Ollama is. Info.BaseURL carries
// the answer only for a model that was found, so a machine with
// OLLAMA_HOST set to a non-default port and an empty library would have
// had its very first pull go to 127.0.0.1:11434 and fail with a
// connection refused that named no cause.
//
// A second resolver in that package was the alternative and is the thing
// to avoid: two readings of OLLAMA_HOST drift, and the failure is a pull
// that lands somewhere the discoverer will never look, so the model
// arrives and is never advertised.
func (d *Discoverer) ResolvedOllamaBaseURL() string { return d.ollamaBaseURL() }

// ollamaBaseURL resolves where to look for Ollama.
//
// OLLAMA_HOST is what Ollama's own client reads, and it is documented in
// three shapes -- "host:port", "http://host:port" and a bare host -- so
// all three are accepted here rather than the one this code would prefer.
func (d *Discoverer) ollamaBaseURL() string {
	if d != nil && d.OllamaBaseURL != "" {
		return strings.TrimRight(d.OllamaBaseURL, "/")
	}
	host := strings.TrimSpace(d.getenv("OLLAMA_HOST"))
	if host == "" {
		return DefaultOllamaBaseURL
	}
	if !strings.Contains(host, "://") {
		if !strings.Contains(host, ":") {
			host += ":11434"
		}
		host = "http://" + host
	}
	return strings.TrimRight(host, "/")
}

// envParallelism is OLLAMA_NUM_PARALLEL as the worker's own environment
// states it, or zero when it is unset or not a positive number, and the name of
// the place it was read.
func (d *Discoverer) envParallelism() (int, string) {
	value, from := d.getenv("OLLAMA_NUM_PARALLEL"), "the worker's environment"
	if d != nil && d.ParallelEnv != nil {
		value, from = d.ParallelEnv()
	}
	return ParseOllamaCount(value), from
}

// ParseOllamaCount reads a count out of the text of an environment variable
// the way Ollama's own envconfig reads OLLAMA_NUM_PARALLEL: spaces are
// trimmed, then any quotes around what is left, then strconv.ParseUint in
// base ten. So "+2", "2.0", "0x2" and "1_0" are not counts -- Ollama logs them
// as invalid and runs its default of one slot, and a ceiling read more
// generously here would advertise slots that do not exist. Spaces INSIDE the
// quotes are part of the text, as they are to Ollama.
//
// It returns zero for text that says no usable count: empty, invalid, zero
// (Ollama's "decide for me"), or past what an int holds on every platform,
// which would wrap when the machine ceiling is summed -- and a ceiling of
// zero is the engine's "unlimited". A caller treats zero as "this source says
// nothing" and moves on to the next.
func ParseOllamaCount(s string) int {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0
	}
	n, err := strconv.ParseUint(strings.Trim(s, "\"'"), 10, 31)
	if err != nil {
		return 0
	}
	return int(n)
}

// ParallelSource is the per-model concurrency this machine will claim and
// the sentence-ready name of where it came from. The sources are read most
// deliberate first: the owner's models.parallel, the worker's
// OLLAMA_NUM_PARALLEL (the only knob there was before the policy key, so it
// keeps its place ahead of the unit; a command that is not the worker reads it
// through ParallelEnv), the unit this cockpit wrote for its own Ollama, and
// one.
//
// ZERO IS NOT AN OPTION HERE, and this is the one place in the package
// where absent is the PERMISSIVE direction rather than the safe one: the
// engine reads a missing `max=` as unlimited. Left unset, a laptop with a
// single 8B model would be handed as many concurrent generations as the
// router had work for. So a ceiling is always declared, and a source that
// holds a nonsense value is skipped rather than believed.
//
// The sources can disagree with the Ollama that is actually running --
// the unit is edited and Ollama has not restarted yet -- and that is the
// benign direction either way: a call admitted past the runtime's slots
// queues inside Ollama, and a ceiling below them wastes a slot until the
// next refresh.
func (d *Discoverer) ParallelSource(configured int) (int, string) {
	if configured > 0 {
		return configured, "models.parallel in policy.yaml"
	}
	if n, from := d.envParallelism(); n > 0 {
		return n, "OLLAMA_NUM_PARALLEL in " + from
	}
	if d != nil && d.ManagedParallel != nil {
		if n := d.ManagedParallel(); n > 0 {
			return n, "the memql-ollama.service unit"
		}
	}
	return 1, "the default"
}
