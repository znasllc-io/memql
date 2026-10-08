package workflowhost

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sort"
	"sync"

	"github.com/znasllc-io/memql/component/actions"
	"github.com/znasllc-io/memql/component/automations"
	"github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/memql/baseloader"
)

const maxSnapshotBytes = 512 << 10
const maxSnapshotConstructs = 64

// Snapshot seals the complete reachable automation/logic source, not merely
// the entry's name. It is data, not authority: callers still bind the native
// operation scope after authenticating the run. Contract versions describe
// the native operation API; the hash does not promise identical model output.
type Snapshot struct {
	Entry      string       `json:"entry"`
	Contract   string       `json:"contract"`
	Version    string       `json:"version"`
	Constructs []Definition `json:"constructs"`
	// Entries are additional native-invoked phases in the same frozen closure.
	// Each invocation still binds only that phase's native operations.
	Entries []string `json:"entries,omitempty"`
	// Phases maps native hook names to captured developer templates.
	Phases map[string]string `json:"phases,omitempty"`
}

type Definition struct {
	Kind   string `json:"kind"`
	Name   string `json:"name"`
	Source string `json:"source"`
}

// SourceLoader resolves installed source. It is injectable for bundle tools
// and tests; a wire caller must never provide it or an arbitrary source body.
type SourceLoader func(kind, name string) (string, error)

var installedSources = sync.OnceValues(func() (map[string]string, error) {
	out := map[string]string{}
	for _, raw := range baseloader.ReadAll(slog.New(slog.DiscardHandler)) {
		slices := append(memql.ExtractAutomationSlices(raw.Content), memql.ExtractFunctionSlices(raw.Content)...)
		for _, s := range slices {
			kind := ""
			switch s.Kind {
			case parser.FunctionTypeAutomation:
				kind = "automation"
			case parser.FunctionTypeLogic:
				kind = "logic"
			default:
				continue
			}
			key := kind + ":" + s.Name
			if _, exists := out[key]; exists {
				return nil, fmt.Errorf("duplicate workflow source %q", key)
			}
			out[key] = s.Source
		}
		for _, s := range memql.ExtractActionSlices(raw.Content) {
			key := "action:" + s.Name
			if _, exists := out[key]; exists {
				return nil, fmt.Errorf("duplicate workflow source %q", key)
			}
			out[key] = s.Source
		}
	}
	return out, nil
})

var ErrSourceNotFound = errors.New("workflow source is not installed")

// InstalledSource reads the installed bundle, never caller-supplied source.
func InstalledSource(kind, name string) (string, error) {
	return installedSource(kind, name)
}

func installedSource(kind, name string) (string, error) {
	all, err := installedSources()
	if err != nil {
		return "", err
	}
	source, ok := all[kind+":"+name]
	if !ok {
		return "", fmt.Errorf("%w: %s %q", ErrSourceNotFound, kind, name)
	}
	return source, nil
}

// Capture compiles and preflights installed sources before returning a durable
// snapshot. Even unreachable branches are checked before the first effect.
func Capture(entry, contract string, source SourceLoader, operations map[string]Operation) (*Snapshot, error) {
	return CaptureEntries(entry, contract, nil, source, operations)
}

// CaptureEntries also freezes phases invoked by native callbacks, whose
// dependency edges cannot be found by walking static automation calls.
func CaptureEntries(entry, contract string, entries []string, source SourceLoader, operations map[string]Operation) (*Snapshot, error) {
	if entry == "" || contract == "" {
		return nil, fmt.Errorf("workflow snapshot requires an entry and contract")
	}
	if source == nil {
		source = installedSource
	}
	s := &Snapshot{Entry: entry, Contract: contract, Entries: slices.Clone(entries)}
	sort.Strings(s.Entries)
	s.Entries = slices.Compact(s.Entries)
	seen := map[string]bool{}
	bytes := 0
	load := func(kind, name string) (string, error) {
		body, err := source(kind, name)
		if err != nil {
			return "", err
		}
		key := kind + ":" + name
		if !seen[key] {
			bytes += len(body)
			if len(s.Constructs) >= maxSnapshotConstructs || bytes > maxSnapshotBytes {
				return "", fmt.Errorf("workflow snapshot exceeds its size limit")
			}
			seen[key] = true
			s.Constructs = append(s.Constructs, Definition{Kind: kind, Name: name, Source: body})
		}
		return body, nil
	}
	opts := sourceOptions(load, operations)
	h := &host{opts: opts, definitions: map[string]*automations.Automation{}, logics: map[string]*preparedLogic{}}
	if err := h.prepare(entry, map[string]bool{}); err != nil {
		return nil, err
	}
	for _, name := range s.Entries {
		if err := h.prepare(name, map[string]bool{}); err != nil {
			return nil, err
		}
	}
	sort.Slice(s.Constructs, func(i, j int) bool {
		a, b := s.Constructs[i], s.Constructs[j]
		return a.Kind+":"+a.Name < b.Kind+":"+b.Name
	})
	s.Version = s.digest()
	return s, nil
}

// CapturePhases captures named hook overrides alongside any configuration entry.
// The mapping is fingerprinted with the source; it grants no operations.
func CapturePhases(entry, contract string, phases map[string]string, entries []string, source SourceLoader, operations map[string]Operation) (*Snapshot, error) {
	entries = slices.Clone(entries)
	for _, name := range phases {
		entries = append(entries, name)
	}
	s, err := CaptureEntries(entry, contract, entries, source, operations)
	if err != nil {
		return nil, err
	}
	s.Phases = make(map[string]string, len(phases))
	for phase, name := range phases {
		if phase == "" || name == "" {
			return nil, fmt.Errorf("empty workflow phase")
		}
		s.Phases[phase] = name
	}
	s.Version = s.digest()
	return s, nil
}

// PhaseEntry resolves a native hook through its immutable source configuration.
func (s *Snapshot) PhaseEntry(phase string) string {
	if name := s.Phases[phase]; name != "" {
		return name
	}
	return phase
}

func sourceOptions(source SourceLoader, operations map[string]Operation) Options {
	logger := slog.New(slog.DiscardHandler)
	return Options{Logger: logger, Operations: operations,
		LoadAction: func(ref string) (*actions.Action, error) { return sourceAction(source, ref) },
		Load: func(name string) (*automations.Automation, error) {
			body, err := source("automation", name)
			if err != nil {
				return nil, err
			}
			if slices := memql.ExtractAutomationSlices(body); len(slices) != 1 || slices[0].Name != name {
				return nil, fmt.Errorf("invalid snapshot automation %q", name)
			}
			return automations.NewLoader(automations.LoaderOptions{Logger: logger}).CompileSource(body, "snapshot:"+name)
		},
		LoadLogic: func(name string) (*memql.Function, error) {
			body, err := source("logic", name)
			if err != nil {
				return nil, err
			}
			if slices := memql.ExtractFunctionSlices(body); len(slices) != 1 || slices[0].Kind != parser.FunctionTypeLogic || slices[0].Name != name {
				return nil, fmt.Errorf("invalid snapshot logic %q", name)
			}
			return memql.BuildFunctionConstruct(body, name, "snapshot:"+name, nil)
		},
	}
}

func (s *Snapshot) digest() string {
	copy := *s
	copy.Version = ""
	b, _ := json.Marshal(copy)
	h := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(h[:])
}

// CheckArgs validates an invocation against the captured entry, before an
// admitting host opens durable work. Children bind their own arguments at run time.
func (s *Snapshot) CheckArgs(args map[string]any) error {
	if s == nil || s.Version != s.digest() {
		return fmt.Errorf("invalid workflow snapshot")
	}
	for _, d := range s.Constructs {
		if d.Kind == "automation" && d.Name == s.Entry {
			opts := sourceOptions(func(string, string) (string, error) { return d.Source, nil }, nil)
			a, err := opts.Load(s.Entry)
			if err != nil {
				return err
			}
			return automations.CheckArgs(a, args)
		}
	}
	return fmt.Errorf("workflow snapshot is missing entry %q", s.Entry)
}

// RunSnapshot never consults the installed registry. Missing children, changed
// source, incompatible contracts and invalid operations fail before any effect.
func RunSnapshot(ctx context.Context, s *Snapshot, contract string, args map[string]any, opts Options) (any, error) {
	if s == nil {
		return nil, fmt.Errorf("missing workflow snapshot")
	}
	return RunSnapshotEntry(ctx, s, contract, s.Entry, args, opts)
}

// RunSnapshotEntry invokes one admitted phase without substituting installed source.
func RunSnapshotEntry(ctx context.Context, s *Snapshot, contract, entry string, args map[string]any, opts Options) (any, error) {
	if s == nil || s.Contract != contract || s.Entry == "" || s.Version != s.digest() {
		return nil, fmt.Errorf("invalid or incompatible workflow snapshot")
	}
	if entry != s.Entry && !slices.Contains(s.Entries, entry) {
		return nil, fmt.Errorf("workflow entry %q was not captured", entry)
	}
	if len(s.Constructs) == 0 || len(s.Constructs) > maxSnapshotConstructs {
		return nil, fmt.Errorf("invalid workflow snapshot size")
	}
	all := map[string]string{}
	bytes := 0
	for _, d := range s.Constructs {
		key := d.Kind + ":" + d.Name
		bytes += len(d.Source)
		if bytes > maxSnapshotBytes || (d.Kind != "automation" && d.Kind != "logic" && d.Kind != "action") || d.Name == "" || all[key] != "" {
			return nil, fmt.Errorf("invalid workflow snapshot definition %q", key)
		}
		all[key] = d.Source
	}
	frozen := sourceOptions(func(kind, name string) (string, error) {
		body, ok := all[kind+":"+name]
		if !ok {
			return "", fmt.Errorf("workflow snapshot is missing %s %q", kind, name)
		}
		return body, nil
	}, opts.Operations)
	frozen.AmbientEngine, frozen.Logger = opts.AmbientEngine, opts.Logger
	return Run(ctx, entry, args, frozen)
}

// Map is a detached JSON object suitable for an immutable, server-written row.
func (s *Snapshot) Map() map[string]any {
	b, _ := json.Marshal(s)
	var out map[string]any
	_ = json.Unmarshal(b, &out)
	return out
}

func SnapshotFromMap(value map[string]any) (*Snapshot, error) {
	b, err := json.Marshal(value)
	if err != nil || len(b) > 2*maxSnapshotBytes {
		return nil, fmt.Errorf("invalid workflow snapshot encoding")
	}
	var s Snapshot
	if err := json.Unmarshal(b, &s); err != nil {
		return nil, err
	}
	if s.Entry == "" || s.Version != s.digest() {
		return nil, fmt.Errorf("invalid workflow snapshot fingerprint")
	}
	return &s, nil
}

// RunPhase uses the stored multi-phase closure, or installed definitions for
// ordinary calls and v1 snapshots written before phase capture existed.
func RunPhase(ctx context.Context, value map[string]any, contract, entry string, args map[string]any, opts Options) (any, error) {
	if value != nil {
		snapshot, err := SnapshotFromMap(value)
		if err != nil {
			return nil, err
		}
		if snapshot.Contract != contract {
			return nil, fmt.Errorf("incompatible workflow contract")
		}
		if len(snapshot.Entries) > 0 {
			return RunSnapshotEntry(ctx, snapshot, contract, snapshot.PhaseEntry(entry), args, opts)
		}
	}
	// Capture even legacy calls before execution so actions and children cannot
	// change mid-invocation. This does not promise replay across separate calls.
	snapshot, err := Capture(entry, contract, nil, opts.Operations)
	if err != nil {
		return nil, err
	}
	return RunSnapshot(ctx, snapshot, contract, args, opts)
}
