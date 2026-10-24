package harness

import (
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"

	"github.com/znasllc-io/memql/core/airoute"
)

// levels.go is where a LEVEL -- the engine's word for how much intelligence
// a step needs -- becomes the knobs one app understands (design D8 of the
// engine's docs/superpowers/specs/2026-09-13-app-session-recording-and-
// learning-program-design.md; memql-cockpit#437).
//
// THE TRANSLATION LIVES ON THE MACHINE because both halves of it are facts
// about the binary installed here. The knob NAMES are the app's own --
// Claude Code's --model and --effort, Codex's model and
// model_reasoning_effort -- and so are the VALUES: an alias Claude Code
// resolves to its newest model, an effort word one release accepts and the
// next ignores. The engine names a level on AppSessionStart; the machine,
// which can see the app, decides what that level means to it, and the
// machine's owner may overrule the built-in answer in policy.yaml
// (apps.levels, internal/worker/tools).
//
// THE VOCABULARY IS THE ENGINE'S OWN PACKAGE, not a copy. core/airoute is
// the routing seam's shared vocabulary, imports nothing but the standard
// library, and is what the engine writes AppSessionStart.level from -- so a
// level this file reads is by construction a level the engine can send, and
// a rename there fails to compile here at the pin bump instead of refusing
// every session on every machine afterwards.
//
// A LEVEL NOBODY CHOSE IS NEVER RUN. An empty level runs the app at its own
// defaults, which is what every session did before the field existed; every
// other outcome is either a translation or a refusal with a sentence. The
// alternative -- falling back to the defaults for a word this cockpit cannot
// read -- produces a session that reports a model nobody asked for, in a
// record somebody later reads as a choice.

// ErrEmbeddingsLevel refuses the one level no app serves (design D10).
//
// An embedding has to come from the SAME embedder as the index it is
// written into, or it lands in a vector space nothing else in the cluster
// shares -- a failure no reader downstream can see. Neither app exposes an
// embedder at all, so this is not a level an app is bad at; it is one an app
// cannot be asked for.
var ErrEmbeddingsLevel = errors.New(`level "embeddings" never runs through an app: an embedding has to come from the embedder of the index it is written into, and neither Claude Code nor Codex exposes one`)

// Knobs are one app's own settings for a session, in the app's own words.
//
// A zero field is a knob LEFT ALONE, not a knob set to nothing: the app
// decides, exactly as it did before levels existed. That is what lets a
// table say "this level runs the account's default model at high effort"
// for an app whose model names this cockpit cannot know.
type Knobs struct {
	// Model is Claude Code's --model (an alias or a full name) or Codex's
	// `model`.
	Model string
	// Effort is Claude Code's --effort or Codex's model_reasoning_effort.
	Effort string
}

// Table maps a level to the knobs one app runs it at.
type Table map[string]Knobs

// AppLevels are the levels an app can serve, in the order a person reads
// them: every level the engine defines except embeddings.
func AppLevels() []string {
	out := make([]string, 0, 3)
	for _, l := range airoute.Levels() {
		if l != airoute.LevelEmbeddings {
			out = append(out, string(l))
		}
	}
	return out
}

// BuiltinLevels is the table this cockpit ships for the app a harness
// drives -- what every machine runs until its owner writes apps.levels.
//
// It is keyed by HARNESS WORD because that is the app's protocol, and both
// Codex harnesses share one table: `model` and model_reasoning_effort are
// Codex's words whichever way it is driven. A word this cockpit has no
// harness for gets no table, so every level refuses on it rather than
// running at defaults nobody picked.
//
// CLAUDE CODE names a model by ALIAS -- haiku, sonnet, opus -- which Claude
// Code resolves to the newest model of that tier itself, so the table does
// not go stale with every release. `fast` is Haiku with NO effort, because
// Haiku 4.5 takes none (the API rejects `effort` on it, and Claude Code drops
// the flag without a word, so passing one would be a knob that is silently
// never turned); `strong` is Sonnet at high; `reasoning` is Opus at xhigh,
// the setting Claude Code itself runs agentic coding at.
//
// CODEX sets the effort and NOT the model. Codex publishes no tier aliases:
// its catalogue is per account and its names change between releases (on
// 0.153.4 one account offers gpt-6-astra, gpt-5.6-sol, gpt-5.5 and others),
// so a model spelled here would be a claim this machine cannot keep for
// anybody else's account. The account's default model runs, at low, medium
// or high -- the three efforts every model on that catalogue advertises. An
// owner who wants a particular model per level writes it in policy.yaml.
func BuiltinLevels(harnessWord string) Table {
	switch harnessWord {
	case HarnessClaudeHeadless:
		return Table{
			string(airoute.LevelFast):      {Model: "haiku"},
			string(airoute.LevelStrong):    {Model: "sonnet", Effort: "high"},
			string(airoute.LevelReasoning): {Model: "opus", Effort: "xhigh"},
		}
	case HarnessCodexAppServer, HarnessCodexMCP:
		return Table{
			string(airoute.LevelFast):      {Effort: "low"},
			string(airoute.LevelStrong):    {Effort: "medium"},
			string(airoute.LevelReasoning): {Effort: "high"},
		}
	}
	return nil
}

// CheckAppLevel refuses a word that is not a level an app can serve: a word
// the engine does not define (in the engine's own sentence, which names all
// four) or the embeddings level (ErrEmbeddingsLevel).
func CheckAppLevel(level string) error {
	parsed, err := airoute.ParseLevel(level)
	if err != nil {
		return err
	}
	if parsed == airoute.LevelEmbeddings {
		return ErrEmbeddingsLevel
	}
	return nil
}

// ResolveLevel returns the knobs a table gives a level.
//
// Empty is no knobs and no error -- the app's own defaults. Every other word
// is either a level an app serves with an entry in the table, or a refusal:
// an undefined word, the embeddings level, or a defined level the table has
// no entry for. The last one refuses rather than running at the defaults
// because a table somebody wrote without that level is not a table that
// chose the defaults for it.
func ResolveLevel(level string, table Table) (Knobs, error) {
	if level == "" {
		return Knobs{}, nil
	}
	if err := CheckAppLevel(level); err != nil {
		return Knobs{}, err
	}
	knobs, ok := table[level]
	if !ok {
		return Knobs{}, fmt.Errorf("level %q has no translation on this machine", level)
	}
	return knobs, nil
}

// ResolveSessionKnobs overlays the explicitly requested fields only. Callers
// validate the result for the selected harness before starting a process.
func ResolveSessionKnobs(level string, table Table, requested Knobs) (Knobs, error) {
	knobs, err := ResolveLevel(level, table)
	if err != nil {
		return Knobs{}, err
	}
	if model := strings.TrimSpace(requested.Model); model != "" {
		knobs.Model = model
	}
	if effort := strings.TrimSpace(requested.Effort); effort != "" {
		knobs.Effort = effort
	}
	return knobs, nil
}

// claudeEfforts are the words `claude --effort` takes, from `claude --help`
// on 2.1.270 (verified 2026-09-13): "Effort level for the current session
// (low, medium, high, xhigh, max)".
//
// THE CHECK EXISTS BECAUSE CLAUDE CODE DOES NOT REFUSE. Given a word outside
// the set it warns on stderr ("Unknown --effort value 'extreme' ... ignoring
// it and using the default effort") and carries on, exit 0. A typo in an
// owner's policy would therefore run every session at a default nobody chose
// while the policy file claimed otherwise; refusing it here is the only
// refusal anybody will ever see.
var claudeEfforts = []string{"low", "medium", "high", "xhigh", "max"}

// knobModel is what a model name may look like on either app: it starts
// with a letter or a digit -- a leading dash would be a FLAG to Claude
// Code's parser, so `--model -p` would be two options rather than one --
// and it holds no whitespace. Claude Code's own names use brackets for a
// context suffix (`sonnet[1m]`), so those are allowed.
var knobModel = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/@+\[\]-]{0,199}$`)

// knobEffortWord is what an effort may look like where the app publishes no
// closed set. Codex takes "a non-empty reasoning effort value advertised by
// the model" (the app-server's ReasoningEffort, 0.153.4) and different models
// advertise different words -- low through max, and ultra on some -- so the
// check is the shape of a word, and the app is the judge of the word.
var knobEffortWord = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,31}$`)

// CheckKnobs refuses a value the harness's app would misread, in a sentence
// that names the app's own vocabulary.
//
// It runs on the built-in tables (a test holds them to it), on an owner's
// policy.yaml entries when the file is read, and in every harness's Start --
// so a value that reaches an app has passed it, whichever of the three
// produced it.
func CheckKnobs(harnessWord string, k Knobs) error {
	if k.Model != "" && !knobModel.MatchString(k.Model) {
		return fmt.Errorf("model %q is not a model name: it must start with a letter or a digit and contain no spaces", k.Model)
	}
	if k.Effort == "" {
		return nil
	}
	switch harnessWord {
	case HarnessClaudeHeadless:
		if !slices.Contains(claudeEfforts, k.Effort) {
			return fmt.Errorf("effort %q is not one Claude Code takes (it takes %s, and ignores any other word rather than refusing it)",
				k.Effort, strings.Join(claudeEfforts, ", "))
		}
	case HarnessCodexAppServer, HarnessCodexMCP:
		if !knobEffortWord.MatchString(k.Effort) {
			return fmt.Errorf("effort %q is not an effort word (Codex takes the words its models advertise, such as low, medium, high or xhigh)", k.Effort)
		}
	default:
		return fmt.Errorf("this cockpit has no harness %q to check an effort against", harnessWord)
	}
	return nil
}

// MergeLevels lays an owner's entries over a built-in table, level by level.
//
// An entry REPLACES its level's built-in entry WHOLE -- model and effort
// together. Merging field by field would run an owner's model at a built-in
// effort they never wrote: a hybrid neither side chose, and one nobody
// reading the policy file could predict. A level the owner did not list
// keeps the built-in entry.
func MergeLevels(base, override Table) Table {
	out := make(Table, len(base)+len(override))
	for level, k := range base {
		out[level] = k
	}
	for level, k := range override {
		out[level] = k
	}
	return out
}

// DescribeKnobs renders knobs the way the app is given them, for the line a
// session writes into its transcript and for `memql worker apps`.
//
// A Codex entry with no model says whose default runs -- the account's --
// because "model_reasoning_effort=low" alone reads as if the model had been
// chosen too.
func DescribeKnobs(harnessWord string, k Knobs) string {
	if k.Model == "" && k.Effort == "" {
		return "the app's own defaults"
	}
	if harnessWord == HarnessClaudeHeadless {
		var parts []string
		if k.Model != "" {
			parts = append(parts, "--model "+k.Model)
		}
		if k.Effort != "" {
			parts = append(parts, "--effort "+k.Effort)
		}
		return strings.Join(parts, " ")
	}
	switch {
	case k.Model == "":
		return "model_reasoning_effort=" + k.Effort + ", on the account's default model"
	case k.Effort == "":
		return "model " + k.Model + ", at the model's default effort"
	}
	return "model " + k.Model + ", model_reasoning_effort=" + k.Effort
}

// CheckResume refuses knobs a harness cannot apply to a session it RESUMES
// rather than starts -- an attach.
//
// Only the Codex MCP fallback has the problem. Claude Code passes the knobs
// to every process, --resume included, and the app-server's thread/resume
// takes the same overrides thread/start does. But an attach through the
// mcp-server continues with codex-reply from its very first call, and
// codex-reply declares prompt and threadId and nothing else: the knobs would
// never be sent, while the session said in its transcript that they had
// been. That is a level neither translated nor refused, which levels.go
// promises never to produce -- so it is refused, with the two ways out.
func CheckResume(harnessWord string, k Knobs) error {
	if harnessWord == HarnessCodexMCP && k != (Knobs{}) {
		return errors.New("codex mcp-server cannot set a level on a thread it resumes: codex-reply takes no configuration, " +
			"so the thread would keep the settings it started with -- attach without a level, or upgrade Codex to a version with `codex app-server`")
	}
	return nil
}

// knobs settles a spec's level into knobs for one harness: the spec's table
// when the caller passed one, the harness's built-in table otherwise, and
// never a value the harness's app would misread -- nor one it would never
// receive, on a resume the harness cannot configure.
//
// Every harness calls it FIRST in Start, before anything forks, so a level
// that cannot run costs a sentence and not a process.
func (s Spec) knobs(harnessWord string) (Knobs, error) {
	table := s.Levels
	if table == nil {
		table = BuiltinLevels(harnessWord)
	}
	knobs, err := ResolveSessionKnobs(s.Level, table, Knobs{Model: s.Model, Effort: s.Effort})
	if err != nil {
		return Knobs{}, err
	}
	if err := CheckKnobs(harnessWord, knobs); err != nil {
		return Knobs{}, err
	}
	if strings.TrimSpace(s.ResumeRef) != "" {
		if err := CheckResume(harnessWord, knobs); err != nil {
			return Knobs{}, err
		}
	}
	return knobs, nil
}
