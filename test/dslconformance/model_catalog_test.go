package dslconformance

import (
	"io"
	"sort"
	"strconv"
	"strings"
	"testing"

	memqlengine "github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/core/dslfs"
	"github.com/znasllc-io/memql/dsl"
)

// The curated model catalog's fixture gates (epic memql#5137, task #5138).
//
// THESE READ THROUGH dsl.Tree(), NOT off the filesystem, and the distinction is
// the point. dsl/embed.go's `go:embed` directive is an explicit domain list, so
// a new domain that is not named there is a directory the compiler copies
// nowhere: every file in it parses when you read it by hand and none of it
// exists at runtime. Reading the same tree the loader reads is what makes a
// missing `all:models` fail here rather than in a cluster.
//
// What they do NOT cover: a construct that parses and is then DROPPED by the
// unified loader (an unresolved import, a reserved field name). That is
// `cmd/memqllint`'s job -- it runs the engine's own Init over the corpus and CI
// runs it -- and duplicating it here would be a second, weaker implementation
// of a check that already exists.

// machineClasses is the closed set from the concept, in ascending order. The
// ORDER is load-bearing: minMachineClass is a FLOOR, so an entry at 16
// satisfies a 64 GB machine and the coverage check is a comparison rather than
// an equality. Written out rather than sorted from the seeds, because a set
// derived from the data cannot detect the data missing a value.
var machineClasses = []string{"16", "24", "32", "64", "128"}

// catalogLevels is the closed level set epic memql#5127 owns. Restated here on
// purpose: this gate asserts the catalog answers every level, so taking the
// list from the router's own registry would make it vacuously true the moment
// a level was dropped from both places at once.
var catalogLevels = []string{"fast", "strong", "reasoning", "embeddings"}

var catalogCategories = map[string]bool{
	"text": true, "reasoning": true, "omni": true, "vision": true,
	"audioIn": true, "audioOut": true, "imageGen": true, "videoGen": true,
	"embeddings": true,
}

var catalogRuntimes = map[string]bool{
	"ollama": true, "mlx": true, "whispercpp": true, "nemo": true,
	"kokoro": true, "mflux": true, "comfyui": true, "openai-compatible": true, "cloud": true,
}

var catalogFlags = map[string]bool{
	"structured": true, "tools": true, "thinking": true, "vision": true,
	"audioIn": true, "audioOut": true, "imageGen": true, "streaming": true,
}

// seededProfile is one `seed modelProfile` block, reduced to the fields these
// gates judge.
type seededProfile struct {
	name            string
	modelID         string
	category        string
	runtime         string
	minMachineClass string
	dimensions      string
	flags           []string
	recommendedFor  []string
	offeredOn       []string
	quant           string
	contextWindow   int
	params          int64
	memoryNeedBytes int64
}

// loadSeededProfiles reads dsl/models/seeds.memql out of the embedded tree and
// returns one entry per seed block. It fails the test rather than returning
// empty when the file is absent: an empty slice would make every gate below
// pass over nothing, which is the failure mode a coverage gate exists to
// prevent.
func loadSeededProfiles(t *testing.T) []seededProfile {
	t.Helper()

	tree := dsl.Tree()
	paths, err := dslfs.WalkMemqlFiles(tree)
	if err != nil {
		t.Fatalf("WalkMemqlFiles: %v", err)
	}
	var src string
	for _, p := range paths {
		if !strings.HasSuffix(p, "models/seeds.memql") {
			continue
		}
		f, openErr := tree.Open(p)
		if openErr != nil {
			t.Fatalf("open %s: %v", p, openErr)
		}
		raw, readErr := io.ReadAll(f)
		_ = f.Close()
		if readErr != nil {
			t.Fatalf("read %s: %v", p, readErr)
		}
		src = string(raw)
		break
	}
	if src == "" {
		t.Fatal("models/seeds.memql is not in the embedded DSL tree -- check that dsl/embed.go's go:embed directive names all:models")
	}

	var out []seededProfile
	var cur *seededProfile
	for _, line := range strings.Split(src, "\n") {
		trimmed := strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(trimmed, "seed modelProfile "):
			name := strings.TrimSpace(strings.TrimSuffix(
				strings.TrimPrefix(trimmed, "seed modelProfile "), "{"))
			out = append(out, seededProfile{name: name})
			cur = &out[len(out)-1]
		case trimmed == "}":
			cur = nil
		case cur != nil:
			key, value, ok := strings.Cut(trimmed, ":")
			if !ok {
				continue
			}
			key = strings.TrimSpace(key)
			value = strings.TrimSpace(value)
			switch key {
			case "modelId":
				cur.modelID = unquote(value)
			case "category":
				cur.category = unquote(value)
			case "runtime":
				cur.runtime = unquote(value)
			case "minMachineClass":
				cur.minMachineClass = unquote(value)
			case "dimensions":
				cur.dimensions = value
			case "quant":
				cur.quant = unquote(value)
			case "contextWindow":
				cur.contextWindow, _ = strconv.Atoi(value)
			case "params":
				cur.params, _ = strconv.ParseInt(value, 10, 64)
			case "memoryNeedBytes":
				cur.memoryNeedBytes, _ = strconv.ParseInt(value, 10, 64)
			case "flags":
				cur.flags = unquoteList(value)
			case "recommendedFor":
				cur.recommendedFor = unquoteList(value)
			case "offeredOn":
				cur.offeredOn = unquoteList(value)
			}
		}
	}
	if len(out) == 0 {
		t.Fatal("models/seeds.memql declares no `seed modelProfile` blocks")
	}
	return out
}

func unquote(v string) string {
	return strings.Trim(strings.TrimSpace(v), `"`)
}

func unquoteList(v string) []string {
	v = strings.TrimSpace(v)
	v = strings.TrimPrefix(v, "[")
	v = strings.TrimSuffix(v, "]")
	if strings.TrimSpace(v) == "" {
		return nil
	}
	parts := strings.Split(v, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if s := unquote(p); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// classIndex maps a machine class to its position in the ascending order, or
// -1. Comparing by index rather than by parsed integer keeps the ordering in
// one place: the closed set above.
func classIndex(class string) int {
	for i, c := range machineClasses {
		if c == class {
			return i
		}
	}
	return -1
}

// TestEveryLevelHasAnEntryPerMachineClass is the catalog's honesty gate.
//
// A level with no entry at a machine class is a person told "your machine can
// run this" by a page that has nothing to name -- and the failure surfaces as
// an empty recommendation with no explanation, which reads as a broken page
// rather than as a gap in curation.
//
// minMachineClass is a FLOOR, so an entry qualifies for every class at or above
// its own. That is why one 16 GB entry per level is enough and why adding a
// duplicate row per class would be the wrong fix.
func TestEveryLevelHasAnEntryPerMachineClass(t *testing.T) {
	profiles := loadSeededProfiles(t)
	for _, level := range catalogLevels {
		for _, class := range machineClasses {
			want := classIndex(class)
			found := false
			for _, p := range profiles {
				floor := classIndex(p.minMachineClass)
				if floor < 0 || floor > want {
					continue
				}
				for _, r := range p.recommendedFor {
					if r == level {
						found = true
						break
					}
				}
				if found {
					break
				}
			}
			if !found {
				t.Errorf("no catalog entry recommended for level %q at machine class %s GB", level, class)
			}
		}
	}
}

// TestSeedsAreValidAgainstTheClosedSets fails on a category, runtime, machine
// class, flag or recommended level outside the concept's closed sets.
//
// The loader already refuses an out-of-set `enum` field, so the enum arms here
// are belt-and-braces. The arms that are NOT are `flags`, `recommendedFor` and
// `offeredOn`: all three are []string, so the loader accepts any word at all,
// and a typo'd "vison" is a capability the Fleet page will never match against
// a machine label and will never report as missing either.
func TestSeedsAreValidAgainstTheClosedSets(t *testing.T) {
	profiles := loadSeededProfiles(t)

	// A NON-DEGENERACY GUARD, before the assertions rather than after.
	//
	// Each arm below iterates a LIST field, and a list field can go empty
	// across the whole corpus one curation decision at a time -- at which point
	// the arm still runs, still passes, and checks nothing. That is not a
	// missing test: the assertion is right there and looks correct, which is
	// exactly why nobody re-reads it.
	//
	// The empty case is only the degenerate end of the real problem, which is a
	// fixture set that has gone UNIFORM on the dimension the code branches on.
	// So the guard fails FIRST, naming the field, rather than letting a green
	// run stand for a check that happened.
	var flagged, levelled, platformed int
	for _, p := range profiles {
		if len(p.flags) > 0 {
			flagged++
		}
		if len(p.recommendedFor) > 0 {
			levelled++
		}
		if len(p.offeredOn) > 0 {
			platformed++
		}
	}
	for _, tc := range []struct {
		field string
		n     int
	}{
		{"flags", flagged},
		{"recommendedFor", levelled},
		{"offeredOn", platformed},
	} {
		if tc.n == 0 {
			t.Fatalf("no seed carries a %s value, so this test's %s arm would pass over nothing", tc.field, tc.field)
		}
	}

	for _, p := range profiles {
		if !catalogCategories[p.category] {
			t.Errorf("%s: category %q is outside the closed set", p.name, p.category)
		}
		if !catalogRuntimes[p.runtime] {
			t.Errorf("%s: runtime %q is outside the closed set", p.name, p.runtime)
		}
		if classIndex(p.minMachineClass) < 0 {
			t.Errorf("%s: minMachineClass %q is outside the closed set", p.name, p.minMachineClass)
		}
		for _, f := range p.flags {
			if !catalogFlags[f] {
				t.Errorf("%s: flag %q is outside the closed set -- a flag nothing matches is a capability that is silently never offered", p.name, f)
			}
		}
		for _, level := range p.recommendedFor {
			if !contains(catalogLevels, level) {
				t.Errorf("%s: recommendedFor %q is not one of the four levels", p.name, level)
			}
		}
		for _, os := range p.offeredOn {
			if os != "macos" && os != "linux" {
				t.Errorf("%s: offeredOn %q is neither macos nor linux", p.name, os)
			}
		}
	}
}

// TestEveryEmbeddingsEntryDeclaresItsWidth is the gate the embedder binding
// rests on (memql#5142).
//
// The binding creates one node_vectors_<dims> table per width, so an embeddings
// profile with no width cannot be bound to at all. Left unchecked, the failure
// is a binding activation that creates node_vectors_0 -- a table pgvector will
// refuse an index on, three layers from the seed that caused it.
func TestEveryEmbeddingsEntryDeclaresItsWidth(t *testing.T) {
	profiles := loadSeededProfiles(t)
	seen := 0
	for _, p := range profiles {
		if p.category != "embeddings" {
			continue
		}
		seen++
		if p.dimensions == "" || p.dimensions == "0" {
			t.Errorf("%s (%s): an embeddings entry must declare `dimensions` -- the binding cannot create its vector table without the width", p.name, p.modelID)
		}
	}
	if seen == 0 {
		t.Fatal("no embeddings entries in the catalog: this gate passed over nothing")
	}
}

// TestStreamingIsAFlagAndNoCategory pins D1's ruling.
//
// Streaming is true of models in every category, so a category for it would put
// one model in two rows -- which is exactly the shape the vendor provider
// records lose in this epic. Without this gate the shape comes back the first
// time somebody adds a streaming model and reaches for the field that looks
// like it is asking.
func TestStreamingIsAFlagAndNoCategory(t *testing.T) {
	profiles := loadSeededProfiles(t)
	streamingSeen := false
	for _, p := range profiles {
		if p.category == "streaming" {
			t.Errorf("%s: `streaming` is a flag, never a category", p.name)
		}
		if contains(p.flags, "streaming") {
			streamingSeen = true
		}
	}
	if !streamingSeen {
		t.Fatal("no entry carries the streaming flag: this gate passed over nothing")
	}
}

// TestVideoGenIsRecommendedNowhere pins program decision 11: video generation
// is in the catalog so an operator can see it was considered, and is a default
// nowhere. A recommendedFor entry on one of these would put a multi-minute GPU
// job behind a level a chat turn resolves.
func TestVideoGenIsRecommendedNowhere(t *testing.T) {
	profiles := loadSeededProfiles(t)
	seen := 0
	for _, p := range profiles {
		if p.category != "videoGen" {
			continue
		}
		seen++
		if len(p.recommendedFor) != 0 {
			t.Errorf("%s: a videoGen entry is a default nowhere, so recommendedFor must be empty (got %v)", p.name, p.recommendedFor)
		}
		if contains(p.offeredOn, "macos") {
			t.Errorf("%s: videoGen is a Linux GPU job (program decision 11); offeredOn must not name macos", p.name)
		}
	}
	if seen == 0 {
		t.Fatal("no videoGen entries in the catalog: this gate passed over nothing")
	}
}

// TestModelIdsAreUniquePerCategory allows one model id to appear in two
// categories -- gemma4:e4b is both a reasoning model and an audio door, which
// is a real property of the weights -- while refusing the same id twice in one
// category, which is a duplicated row rather than a second purpose.
func TestModelIdsAreUniquePerCategory(t *testing.T) {
	profiles := loadSeededProfiles(t)
	seen := map[string]string{}
	for _, p := range profiles {
		key := p.category + "\x00" + p.modelID
		if prior, ok := seen[key]; ok {
			t.Errorf("%s duplicates %s: model id %q already has a %q entry", p.name, prior, p.modelID, p.category)
			continue
		}
		seen[key] = p.name
	}
}

// TestCatalogCategoriesAreCovered reports which of the nine categories have no
// entry. `vision` is deliberately empty -- D2's ruling is that the text models
// see, so a separate vision pull is a second copy of weights the fleet already
// has -- and the exemption is named here rather than left as a silent gap, so
// that a category emptying out for any OTHER reason still fails.
func TestCatalogCategoriesAreCovered(t *testing.T) {
	deliberatelyEmpty := map[string]string{
		"vision": "D2: the text entries carry vision; a separate vision model would be a second copy of weights the fleet already holds",
	}

	profiles := loadSeededProfiles(t)
	populated := map[string]bool{}
	for _, p := range profiles {
		populated[p.category] = true
	}

	var missing []string
	for category := range catalogCategories {
		if populated[category] {
			continue
		}
		if _, ok := deliberatelyEmpty[category]; ok {
			continue
		}
		missing = append(missing, category)
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Errorf("catalog categories with no entry and no recorded reason: %v", missing)
	}
}

func contains(haystack []string, needle string) bool {
	for _, h := range haystack {
		if h == needle {
			return true
		}
	}
	return false
}

// residentBudgetShare is how much of a class a recommended set may occupy
// when every one of its models is loaded at once: nine tenths, the rest
// being the runtime's own overhead and the KV cache growth a long call
// brings.
const residentBudgetShare = 0.9

// recommendedInstalledSet exercises the engine against the embedded catalog. A
// second implementation of its ordering would let the gate pass while the
// actual recommended set selects a different model.
func recommendedInstalledSet(profiles []seededProfile, class, platform string, runtimes []string) map[string]seededProfile {
	var catalog []memqlengine.CatalogProfile
	byID := map[string]seededProfile{}
	for _, p := range profiles {
		catalog = append(catalog, memqlengine.CatalogProfile{
			ModelId: p.modelID, Category: p.category, Runtime: p.runtime,
			MinMachineClass: p.minMachineClass, RecommendedFor: p.recommendedFor,
			OfferedOn: p.offeredOn, Params: p.params, Quant: p.quant, ContextWindow: p.contextWindow,
		})
		byID[p.modelID] = p
	}
	hardware := memqlengine.MachineHardware{
		MemoryBytes: 128 << 30,
	}
	for _, runtime := range runtimes {
		hardware.Runtimes = append(hardware.Runtimes, memqlengine.MachineRuntime{Name: runtime, Version: "1"})
	}
	set := map[string]seededProfile{}
	for _, r := range memqlengine.RecommendedSet(class, hardware, platform, catalog) {
		if r.Pullable() {
			set[r.Level] = byID[r.Profile.ModelId]
		}
	}
	return set
}

func TestRecommendedCatalogMatchesTheClassTable(t *testing.T) {
	profiles := loadSeededProfiles(t)
	for _, runtimes := range [][]string{{"ollama"}, {"ollama", "mlx", "whispercpp", "nemo", "kokoro", "mflux", "comfyui"}} {
		for _, platform := range []string{"linux", "macos"} {
			for class, textModel := range map[string]string{
				"16": "qwen3.5:9b", "24": "qwen3.8:27b", "32": "qwen3.8:27b",
				"64": "qwen3.8:27b-q8_0", "128": "qwen3.8:27b-q8_0",
			} {
				set := recommendedInstalledSet(profiles, class, platform, runtimes)
				for _, level := range catalogLevels {
					want := textModel
					if level == "embeddings" {
						want = "qwen3-embedding:0.6b"
					}
					if got := set[level].modelID; got != want {
						t.Errorf("%s class %s %s: got %q, want %q", platform, class, level, got, want)
					}
				}
			}
		}
	}
}

func TestActiveEmbedderReservationCoversMeasuredRuntime(t *testing.T) {
	// RTX 4090, Ollama 0.33.3, num_ctx=8192, q8_0 KV, 2026-09-08:
	// /api/ps reported this GPU allocation after a successful 1024-dimension
	// embedding. It includes compute buffers, which a weights+KV estimate
	// omitted. This floor is a measurement, independent of the chosen margin.
	const measuredGPUBytes int64 = 2_416_873_308
	for _, p := range loadSeededProfiles(t) {
		if p.modelID != "qwen3-embedding:0.6b" {
			continue
		}
		if p.memoryNeedBytes < measuredGPUBytes {
			t.Fatalf("active embedder reserves %d bytes, below the measured 8K runtime allocation of %d bytes including compute buffers", p.memoryNeedBytes, measuredGPUBytes)
		}
		return
	}
	t.Fatal("active embedder is missing from the embedded catalog")
}

// TestEveryClassRecommendedSetFitsResident is the 2026-09-08 record's D6 as
// a gate: the models a class recommends across its four levels fit LOADED
// TOGETHER inside nine tenths of the class, on both platforms.
//
// Ollama evicts and reloads when a set does not fit, and a seventeen-gigabyte
// reload every time a call's level flips is seconds of latency that read as
// the platform being slow. The failure this catches is the quiet one: a
// curation that adds a strong pick to a class without noticing that the fast
// and reasoning picks already fill it. The sum is over DISTINCT ids, since
// one model serving two levels is loaded once.
//
// memoryNeedBytes is the resident figure at a working context, not the
// download size, and a zero on a recommended Ollama entry fails here too:
// a zero would make this gate pass over nothing.
func TestEveryClassRecommendedSetFitsResident(t *testing.T) {
	profiles := loadSeededProfiles(t)
	for _, runtimes := range [][]string{{"ollama"}, {"ollama", "mlx", "whispercpp", "nemo", "kokoro", "mflux", "comfyui"}} {
		for _, platform := range []string{"linux", "macos"} {
			for _, class := range machineClasses {
				gb, err := strconv.Atoi(class)
				if err != nil {
					t.Fatalf("machine class %q is not a number", class)
				}
				budget := int64(float64(gb) * float64(1<<30) * residentBudgetShare)
				set := recommendedInstalledSet(profiles, class, platform, runtimes)
				distinct := map[string]int64{}
				var names []string
				for level, p := range set {
					if p.memoryNeedBytes <= 0 {
						t.Errorf("%s/%s GB: the %s pick %s states no memoryNeedBytes, which would make this gate vacuous", platform, class, level, p.modelID)
					}
					if _, seen := distinct[p.modelID]; !seen {
						names = append(names, p.modelID)
					}
					distinct[p.modelID] = p.memoryNeedBytes
				}
				var total int64
				for _, b := range distinct {
					total += b
				}
				if total > budget {
					t.Errorf("%s/%s GB: the recommended set %v needs %.1f GB resident together; the class holds %.1f GB at %.0f%% -- it would thrash between models",
						platform, class, names, float64(total)/1e9, float64(budget)/1e9, residentBudgetShare*100)
				}
			}
		}
	}
}
