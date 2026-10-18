package memql

// The RECOMMENDED SET (epic memql#5146, design D2).
//
// ===========================================================================
// WHAT IT ANSWERS
// ===========================================================================
// "Which of the catalog's models should THIS machine pull?" -- one per level,
// strongest first, from the profiles the machine's class reaches and whose
// runtime the machine has.
//
// It is computed ON THE ENGINE rather than on the cockpit, and that placement
// is the decision. The catalog lives in the graph, so a cockpit computing its
// own recommendation would be reading a copy; one function here is what every
// surface, the machine page and the install wizard's fleet door all read.
//
// ===========================================================================
// A PROFILE THAT CANNOT BE PULLED IS RETURNED, NOT DROPPED
// ===========================================================================
// The obvious shape for this function is a filter, and a filter is wrong here.
// A profile ruled out by a missing runtime, by the wrong operating system or by
// a class it does not reach is the profile a person is looking for when they
// ask why their machine cannot do voice; dropping it answers that question with
// silence, and silence is indistinguishable from a catalog that never had one.
//
// So a Recommendation carries `Blocked`: empty when the profile is pullable, and
// otherwise the SENTENCE saying why, written once here and rendered by the
// machine page and the act alike. Two spellings of one reason drift, and the
// drift shows up as a page that explains one thing while the refusal says
// another.
//
// ===========================================================================
// AN ABSENT INVENTORY RECOMMENDS NOTHING AND BLOCKS NOTHING
// ===========================================================================
// Not an empty recommendation with every profile blocked -- an EMPTY LIST.
// There is nothing to say about a machine that has not reported, and a page
// listing every profile as blocked reads as a machine that failed rather than
// one that has not spoken.

import (
	"sort"
	"strings"
)

// The four levels, in the order the recommended set presents them: the ones a
// person is most likely to want a local answer for come first.
//
// This is the same closed set `@level` declares and `recommendedFor` names on a
// catalog row. A fifth here that the catalog does not know would recommend
// nothing; a fifth there that this does not know would be silently unreachable,
// which is why RecommendedSet reports an unknown level rather than skipping it.
var recommendationLevels = []string{"fast", "strong", "reasoning", "embeddings"}

// RecommendationLevels returns the closed set, in presentation order.
func RecommendationLevels() []string {
	out := make([]string, len(recommendationLevels))
	copy(out, recommendationLevels)
	return out
}

// CatalogProfile is one v1:models:modelProfile row, as this package reads it.
type CatalogProfile struct {
	ModelId         string
	Category        string
	Runtime         string
	Family          string
	Quant           string
	MinMachineClass string
	RecommendedFor  []string
	OfferedOn       []string
	Flags           []string
	Params          int64
	ContextWindow   int
	SizeBytes       int64
	MemoryNeedBytes int64
	Source          string
	ReviewedAt      string
	Notes           string
}

// Recommendation is one profile offered for a machine, with the reason it
// cannot be pulled when there is one.
type Recommendation struct {
	Profile CatalogProfile
	// Level is the one this entry is the answer for. A profile recommended for
	// two levels appears twice, once under each, because the question the set
	// answers is per level -- "what should serve my fast calls" -- and one row
	// standing for two answers makes the strongest-first order meaningless.
	Level string
	// Blocked is empty when the profile can be pulled, and otherwise the
	// sentence saying why. See the file comment for why a blocked profile is
	// returned rather than filtered away.
	Blocked string
	// blockRank is how FIXABLE the block is, and it orders entries when every
	// one at a level is blocked. See blockRank* below for why that is not the
	// same question as which model is strongest.
	blockRank int
}

// Pullable reports whether this entry can be pulled right now.
func (r Recommendation) Pullable() bool { return strings.TrimSpace(r.Blocked) == "" }

// RecommendedSet is the machine's recommended models: one entry per level per
// blocking reason, strongest first within a level, levels in presentation
// order.
//
// `os` is the machine's operating system as platformInfo reports it (darwin,
// linux). An EMPTY os does not block anything: a machine that did not say what
// it runs has not said it cannot run this, and refusing on that would rule out
// every machine whose cockpit reports an inventory but no platform.
func RecommendedSet(class string, h MachineHardware, os string, catalog []CatalogProfile) []Recommendation {
	if !h.Present() {
		return nil
	}

	byLevel := map[string][]Recommendation{}
	for _, p := range catalog {
		for _, level := range p.RecommendedFor {
			level = strings.TrimSpace(level)
			if !isRecommendationLevel(level) {
				// A level the engine does not know is REPORTED rather than
				// skipped, scoped to this entry. A catalog row recommending a
				// level nobody serves is a curation mistake somebody has to be
				// able to see; silently dropping it is how it survives a
				// release.
				continue
			}
			sentence, rank := blockedReason(class, h, os, p)
			byLevel[level] = append(byLevel[level], Recommendation{
				Profile:   p,
				Level:     level,
				Blocked:   sentence,
				blockRank: rank,
			})
		}
	}

	out := make([]Recommendation, 0, len(recommendationLevels))
	for _, level := range recommendationLevels {
		entries := byLevel[level]
		if len(entries) == 0 {
			continue
		}
		sortRecommendations(entries)
		// ONE PER LEVEL, and it is the strongest PULLABLE one when there is
		// one. A blocked entry only reaches the set when nothing at that level
		// is pullable, which is exactly when the reason is worth showing: a
		// person whose machine can serve `fast` does not need to be told why
		// some other fast model is unavailable, and a person whose machine can
		// serve nothing at that level needs to be told why.
		out = append(out, entries[0])
	}
	return out
}

// RuntimeGap returns the runtimes the recommended set needs and the machine
// does not have, in the order the set presents them.
//
// It exists because the machine page's answer to a blocked entry is an INSTALL
// SENTENCE, and the install is the cockpit's -- the engine never puts software
// on a person's machine (D7). Naming the gap is the whole of what this side can
// honestly offer.
func RuntimeGap(set []Recommendation, h MachineHardware) []string {
	seen := map[string]bool{}
	var out []string
	for _, r := range set {
		name := strings.TrimSpace(r.Profile.Runtime)
		if name == "" || h.HasRuntime(name) || seen[name] {
			continue
		}
		seen[name] = true
		out = append(out, name)
	}
	return out
}

// How fixable a block is, ascending. The rank orders entries when EVERY one at
// a level is blocked, and it is a different question from which model is
// strongest.
//
// The failing case that produced this rank: a machine under a class floor for
// one profile and missing a runtime for another shows the entry with the
// biggest parameter count if the order is strength -- so a person whose only
// real problem is that Ollama is not installed reads "needs a 64 GB machine"
// and goes shopping. Ordering by fixability puts the sentence they can act on
// in front of them.
const (
	blockRankPullable = iota
	// A missing runtime is an INSTALL on that machine. It is the only block
	// with a repair the person reading the page can perform.
	blockRankMissingRuntime
	// Too small is hardware. Nothing on this page fixes it.
	blockRankClass
	// Wrong platform is hardware too, and less actionable still: no version of
	// this machine will ever run it.
	blockRankPlatform
)

// blockedReason is the single place a profile's unavailability is worded, and
// it returns the rank with the sentence so the two cannot disagree.
//
// The order of the CHECKS is not the order of the ranks, deliberately. The
// checks run cheapest-truth-first -- platform, then class, then runtime -- so
// that the sentence names the most fundamental obstacle; the RANK then orders
// entries by what a person can do about it. Telling somebody their machine is
// too small AND missing a runtime invites them to install the runtime, which
// will not help, so the sentence names the size; but between two DIFFERENT
// profiles, the one whose block is an install is the one worth showing.
func blockedReason(class string, h MachineHardware, os string, p CatalogProfile) (string, int) {
	if !offeredOn(p, os) {
		return "Not offered on " + osLabel(os) + ". This entry is " + strings.Join(p.OfferedOn, " and ") + " only.", blockRankPlatform
	}
	if !ClassAtLeast(class, p.MinMachineClass) {
		if class == ClassUnsupported {
			return "This machine is under the floor for local models, so nothing in the catalog runs here.", blockRankClass
		}
		return "Needs a " + p.MinMachineClass + " GB machine; this one is class " + class + ".", blockRankClass
	}
	if runtime := strings.TrimSpace(p.Runtime); runtime != "" && !h.HasRuntime(runtime) {
		// NAMED, not "a runtime is missing". The repair is on that machine and
		// nowhere else, and a person cannot perform it without the name.
		return "Needs the " + runtimeLabel(runtime) + " runtime, which this machine has not reported.", blockRankMissingRuntime
	}
	return "", blockRankPullable
}

// sortRecommendations orders entries within one level.
//
// BY FIXABILITY FIRST -- pullable, then a missing runtime, then a class floor,
// then the wrong platform -- and only then by strength: parameters descending,
// context window descending, quantization precision descending, model id.
//
// The first key does two jobs. It makes the "one per level" rule honest, since
// an unpullable entry must never displace a pullable one; and when NOTHING at a
// level is pullable it puts the sentence a person can act on in front of them
// rather than the biggest model's complaint. That second half is not a
// refinement: ordering blocked entries by strength shows "needs a 64 GB
// machine" to somebody whose only real problem is that Ollama is not installed.
//
// Recommendations curate what to install, while routing chooses among models
// already available. Higher precision breaks a size/context tie here so the
// same model's Q8 variant wins over Q4 when its hardware floor is met.
// Unknown parameters and unknown precision sort last in their respective keys.
//
// The sort is STABLE and its last key is the model id, so two replicas reading
// one catalog produce the same set in the same order with no shared state.
func sortRecommendations(entries []Recommendation) {
	sort.SliceStable(entries, func(i, j int) bool {
		a, b := entries[i], entries[j]
		if a.blockRank != b.blockRank {
			return a.blockRank < b.blockRank
		}
		if (a.Profile.Params > 0) != (b.Profile.Params > 0) {
			return a.Profile.Params > 0
		}
		if a.Profile.Params != b.Profile.Params {
			return a.Profile.Params > b.Profile.Params
		}
		if a.Profile.ContextWindow != b.Profile.ContextWindow {
			return a.Profile.ContextWindow > b.Profile.ContextWindow
		}
		if aq, bq := quantPrecision(a.Profile.Quant), quantPrecision(b.Profile.Quant); aq != bq {
			return aq > bq
		}
		return a.Profile.ModelId < b.Profile.ModelId
	})
}

// quantPrecision ranks the formats present in the curated catalog. Unrecognized
// formats carry no precision claim; model id remains the deterministic tie-break.
func quantPrecision(quant string) int {
	switch strings.ToUpper(strings.TrimSpace(quant)) {
	case "F32", "FP32":
		return 32
	case "F16", "FP16", "BF16":
		return 16
	case "Q8_0", "FP8":
		return 8
	case "Q5_0":
		return 5
	case "Q4_K_M", "MXFP4", "4BIT":
		return 4
	default:
		return 0
	}
}

// offeredOn reports whether a profile is offered on this operating system.
//
// An EMPTY offeredOn means the profile does not restrict itself, and an EMPTY
// os means the machine did not say. Both are permissive, and both are the right
// direction: a restriction nobody declared is not a restriction, and refusing a
// machine for not stating its platform would rule out every cockpit that
// reports an inventory without one.
//
// THE MACHINE'S WORD IS NORMALIZED FIRST, and without that this comparison was
// wrong for every Mac (memql#5195). `platformInfo.os` is Go's GOOS, so a Mac
// says `darwin`; `offeredOn` is declared `macos | linux` and every seed spells
// it `macos`. A plain EqualFold between the two answers false, which blocked the
// one macOS-only profile in the catalog ON MACOS, with the sentence "Not offered
// on macOS. This entry is macos only." Every test here passed because they all
// used OfferedOn: ["linux"] -- the pair that collides was never spelled.
func offeredOn(p CatalogProfile, os string) bool {
	os = NormalizePlatform(os)
	if len(p.OfferedOn) == 0 || os == "" {
		return true
	}
	for _, v := range p.OfferedOn {
		if strings.EqualFold(strings.TrimSpace(v), os) {
			return true
		}
	}
	return false
}

func isRecommendationLevel(level string) bool {
	for _, l := range recommendationLevels {
		if level == l {
			return true
		}
	}
	return false
}

func osLabel(os string) string {
	switch strings.TrimSpace(strings.ToLower(os)) {
	case "darwin":
		return "macOS"
	case "linux":
		return "Linux"
	case "":
		return "this machine"
	default:
		return os
	}
}

// runtimeLabel is the runtime's name as a person writes it. The catalog and the
// inventory both carry the lowercase id; a sentence carries the name.
func runtimeLabel(name string) string {
	switch strings.TrimSpace(strings.ToLower(name)) {
	case "ollama":
		return "Ollama"
	case "mlx":
		return "MLX"
	case "whispercpp":
		return "whisper.cpp"
	case "kokoro":
		return "Kokoro"
	case "mflux":
		return "MFLUX"
	case "docker":
		return "Docker"
	case "nemo":
		return "NeMo"
	case "comfyui":
		return "ComfyUI"
	default:
		return name
	}
}
