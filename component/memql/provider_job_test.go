package memql

import (
	"reflect"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/znasllc-io/memql/core/airoute"
)

// provider_job_test.go -- a vendor's text record says what it is for (design
// record docs/superpowers/specs/2026-11-21-vendor-accounts-and-api-keys-design.md,
// R1): the levels it serves and its rank among the records that serve them.
// The vendor selectors read nothing else (R2), so what a record may declare,
// and what is refused when it is loaded, is fixed here.

func textRecord(params map[string]any) ProviderConfig {
	return ProviderConfig{Name: "claudeTest", Type: "Anthropic", Model: "claude-test", Params: params}
}

func TestProviderLevelsAndRankAreRead(t *testing.T) {
	for name, tc := range map[string]struct {
		levels string
		want   []airoute.Level
	}{
		"one level":                {"strong", []airoute.Level{airoute.LevelStrong}},
		"two levels":               {"fast strong", []airoute.Level{airoute.LevelFast, airoute.LevelStrong}},
		"three levels":             {"fast strong reasoning", []airoute.Level{airoute.LevelFast, airoute.LevelStrong, airoute.LevelReasoning}},
		"written in another order": {"reasoning fast", []airoute.Level{airoute.LevelFast, airoute.LevelReasoning}},
		"with spare spaces":        {"  strong   reasoning ", []airoute.Level{airoute.LevelStrong, airoute.LevelReasoning}},
	} {
		t.Run(name, func(t *testing.T) {
			cfg := textRecord(map[string]any{"levels": tc.levels, "rank": float64(300)})
			if err := validateProviderJob(cfg); err != nil {
				t.Fatalf("a legal job was refused: %v", err)
			}
			if got := cfg.Levels(); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("Levels() = %v, want %v", got, tc.want)
			}
			for _, level := range []airoute.Level{airoute.LevelFast, airoute.LevelStrong, airoute.LevelReasoning} {
				declared := false
				for _, w := range tc.want {
					declared = declared || w == level
				}
				if cfg.ServesLevel(level) != declared {
					t.Errorf("ServesLevel(%s) = %v, want %v", level, cfg.ServesLevel(level), declared)
				}
			}
			if cfg.ServesLevel(airoute.LevelEmbeddings) {
				t.Error("a text record serves the embeddings level")
			}
			if cfg.Rank() != 300 {
				t.Errorf("Rank() = %d, want 300", cfg.Rank())
			}
		})
	}
	// A rank written as a whole number of either kind a loader can hand over.
	if got := textRecord(map[string]any{"rank": 7}).Rank(); got != 7 {
		t.Errorf("Rank() = %d for an int param, want 7", got)
	}
}

func TestProviderJobRefusals(t *testing.T) {
	embedding := ProviderConfig{Name: "embedTest", Type: "OpenAIEmbedding", Model: "embed", Modality: "embedding"}
	speech := ProviderConfig{Name: "speechTest", Type: "OpenAITTS", Model: "tts"}
	transcription := ProviderConfig{Name: "sttTest", Type: "OpenAISTT", Model: "stt"}
	base := ProviderConfig{Name: "anthropic", Type: "Anthropic", Base: true}
	fleet := ProviderConfig{Name: "fleet", Type: "Fleet"}
	with := func(cfg ProviderConfig, params map[string]any) ProviderConfig {
		cfg.Params = params
		return cfg
	}

	for name, tc := range map[string]struct {
		cfg  ProviderConfig
		says []string
	}{
		"a word that is not a level": {textRecord(map[string]any{"levels": "strong clever"}), []string{"claudeTest", "levels", `"clever"`}},
		"a level written twice":      {textRecord(map[string]any{"levels": "fast strong fast"}), []string{"claudeTest", "levels", `"fast"`, "twice"}},
		"an empty list":              {textRecord(map[string]any{"levels": "   "}), []string{"claudeTest", "levels", "empty"}},
		"the embeddings level":       {textRecord(map[string]any{"levels": "embeddings"}), []string{"claudeTest", "levels", `"embeddings"`}},
		"levels that are not words":  {textRecord(map[string]any{"levels": float64(3)}), []string{"claudeTest", "levels"}},
		"rank zero":                  {textRecord(map[string]any{"rank": float64(0)}), []string{"claudeTest", "rank", "1 to 1000"}},
		"rank above the range":       {textRecord(map[string]any{"rank": float64(1001)}), []string{"claudeTest", "rank", "1 to 1000"}},
		"a rank that is not whole":   {textRecord(map[string]any{"rank": 2.5}), []string{"claudeTest", "rank", "1 to 1000"}},
		"a rank in words":            {textRecord(map[string]any{"rank": "high"}), []string{"claudeTest", "rank", "1 to 1000"}},
		"levels on an embedding":     {with(embedding, map[string]any{"levels": "fast"}), []string{"embedTest", "levels", "text model"}},
		"rank on an embedding":       {with(embedding, map[string]any{"rank": float64(5)}), []string{"embedTest", "rank", "text model"}},
		"levels on a speech record":  {with(speech, map[string]any{"levels": "fast"}), []string{"speechTest", "levels", "text model"}},
		"levels on a transcriber":    {with(transcription, map[string]any{"levels": "fast"}), []string{"sttTest", "levels", "text model"}},
		"levels on a base":           {with(base, map[string]any{"levels": "fast"}), []string{"anthropic", "levels", "text model"}},
		"levels on the fleet record": {with(fleet, map[string]any{"levels": "fast"}), []string{"fleet", "levels", "text model"}},
	} {
		t.Run(name, func(t *testing.T) {
			err := validateProviderJob(tc.cfg)
			if err == nil {
				t.Fatal("the record was accepted")
			}
			for _, want := range tc.says {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("the refusal does not say %s: %v", want, err)
				}
			}
		})
	}
}

// A record that declares neither is legal: it resolves by name, and the vendor
// selectors pass it over (Task 2). A bundle's own record, written before this
// rule, keeps loading.
func TestARecordWithNoJobIsLegal(t *testing.T) {
	for name, cfg := range map[string]ProviderConfig{
		"a text record":     textRecord(map[string]any{"contextWindow": float64(200000)}),
		"with no params":    textRecord(nil),
		"an embedding":      {Name: "embedTest", Type: "OpenAIEmbedding", Modality: "embedding"},
		"a base":            {Name: "anthropic", Type: "Anthropic", Base: true},
		"levels on its own": textRecord(map[string]any{"levels": "strong"}),
		"rank on its own":   textRecord(map[string]any{"rank": float64(10)}),
	} {
		t.Run(name, func(t *testing.T) {
			if err := validateProviderJob(cfg); err != nil {
				t.Fatalf("refused: %v", err)
			}
		})
	}
	bare := textRecord(nil)
	if bare.Levels() != nil || bare.Rank() != 0 || bare.ServesLevel(airoute.LevelStrong) {
		t.Fatalf("a record with no job reads as having one: levels %v, rank %d", bare.Levels(), bare.Rank())
	}
}

// The check runs when a record is LOADED, after its base is known, and a
// refused record is reported the way the loader reports any other: a skip on
// the load report, which strict boot refuses to start on. It is not
// registered, so it cannot serve by name with a job nothing validated.
func TestALoadRefusesARecordWithABadJobAndKeepsItsNeighbours(t *testing.T) {
	tree := fstest.MapFS{
		"providers/memql.toml": &fstest.MapFile{Data: []byte("memql = \"1.0\"\nedition = \"2026\"\n")},
		"providers/providers.memql": &fstest.MapFile{Data: []byte(`
@base
@vendor("Anthropic")
provider anthropic {
  auth { }
}

@extends("anthropic")
@model("claude-good")
provider claudeGood {
  params {
    levels "strong reasoning"
    rank 500
  }
}

@extends("anthropic")
@model("claude-bad")
provider claudeBad {
  params {
    levels "strong clever"
    rank 400
  }
}
`)},
	}
	reg := newProviderRegistry()
	report := newLoadReport()
	if _, err := loadUnifiedProvidersFromTree(tree, false, nil, reg, report); err != nil {
		t.Fatalf("load: %v", err)
	}
	if _, ok := reg.Entry("claudeBad"); ok {
		t.Fatal("a record with a job the loader refuses was registered")
	}
	good, ok := reg.Entry("claudeGood")
	if !ok {
		t.Fatal("the record beside it was not registered")
	}
	if !reflect.DeepEqual(good.Config.Levels(), []airoute.Level{airoute.LevelStrong, airoute.LevelReasoning}) || good.Config.Rank() != 500 {
		t.Fatalf("claudeGood loaded as levels %v, rank %d", good.Config.Levels(), good.Config.Rank())
	}
	if !report.HasProblems() {
		t.Fatal("the load report holds no problem, so strict boot would start on a refused record")
	}
	if detail := report.Detail(); !strings.Contains(detail, "claudeBad") || !strings.Contains(detail, `"clever"`) {
		t.Fatalf("the report does not name the record and the word: %s", detail)
	}
}

// ---- the order a vendor selector answers in (R2) ----

// jobRecord registers a vendor text record with a job and a price.
func jobRecord(reg *ProviderRegistry, name, vendor, levels string, rank int, in, out float64) {
	entry := vendorRecord(name, vendor, true, "")
	entry.Config.Params = map[string]any{"contextWindow": float64(200000)}
	if levels != "" {
		entry.Config.Params["levels"] = levels
	}
	if rank != 0 {
		entry.Config.Params["rank"] = float64(rank)
	}
	if in != 0 || out != 0 {
		entry.Config.Params["inputCostPerMillion"] = in
		entry.Config.Params["outputCostPerMillion"] = out
	}
	reg.setEntry(entry)
}

// catalogForTest is two vendors' models, registered in the order given so a
// test can build the same catalog two ways.
func catalogForTest(order ...string) *ProviderRegistry {
	reg := newProviderRegistry()
	add := map[string]func(){
		"opus":   func() { jobRecord(reg, "opus", VendorAnthropic, "strong reasoning", 900, 5, 25) },
		"sonnet": func() { jobRecord(reg, "sonnet", VendorAnthropic, "fast strong", 600, 2, 10) },
		"haiku":  func() { jobRecord(reg, "haiku", VendorAnthropic, "fast", 300, 0.1, 0.5) },
		"astra":  func() { jobRecord(reg, "astra", VendorOpenAI, "strong reasoning", 900, 8, 40) },
		"luna":   func() { jobRecord(reg, "luna", VendorOpenAI, "fast", 300, 0.1, 0.5) },
		"legacy": func() { jobRecord(reg, "legacy", VendorOpenAI, "", 0, 1, 2) },
		"free":   func() { jobRecord(reg, "free", VendorOpenAI, "fast", 100, 0, 0) },
	}
	for _, name := range order {
		add[name]()
	}
	return reg
}

func TestStrongestIsRankOrderAtTheLevel(t *testing.T) {
	reg := catalogForTest("haiku", "luna", "sonnet", "astra", "opus", "legacy", "free")
	for level, want := range map[airoute.Level][]string{
		// Highest rank first; a tie is broken by name, so two replicas agree.
		airoute.LevelReasoning: {"astra", "opus"},
		airoute.LevelStrong:    {"astra", "opus", "sonnet"},
		airoute.LevelFast:      {"sonnet", "haiku", "luna", "free"},
	} {
		got, _ := reg.VendorRecordsFor(level, VendorSelectorStrongest)
		if !reflect.DeepEqual(got, want) {
			t.Errorf("strongest at %s = %v, want %v", level, got, want)
		}
	}
}

func TestCheapestIsPriceOrderAtTheLevel(t *testing.T) {
	reg := catalogForTest("haiku", "luna", "sonnet", "astra", "opus", "legacy", "free")
	for level, want := range map[airoute.Level][]string{
		// Lowest input plus output first; ties by name; a record with no
		// price is last and never reads as free.
		airoute.LevelFast:      {"haiku", "luna", "sonnet", "free"},
		airoute.LevelStrong:    {"sonnet", "opus", "astra"},
		airoute.LevelReasoning: {"opus", "astra"},
	} {
		got, _ := reg.VendorRecordsFor(level, VendorSelectorCheapest)
		if !reflect.DeepEqual(got, want) {
			t.Errorf("cheapest at %s = %v, want %v", level, got, want)
		}
	}
}

func TestARecordAtAnotherLevelIsNotChosen(t *testing.T) {
	reg := catalogForTest("haiku", "opus")
	got, passed := reg.VendorRecordsFor(airoute.LevelReasoning, VendorSelectorStrongest)
	if !reflect.DeepEqual(got, []string{"opus"}) {
		t.Fatalf("reasoning = %v, want the one record that declares it", got)
	}
	if passed["haiku"] != "serves fast, not reasoning" {
		t.Fatalf("haiku was passed over with %q, want what it serves and what was asked", passed["haiku"])
	}
	if _, listed := passed["opus"]; listed {
		t.Fatal("the chosen record is also listed as passed over")
	}
}

func TestARecordWithNoLevelIsPassedOverAndStillResolvesByName(t *testing.T) {
	reg := catalogForTest("legacy", "opus")
	for _, selector := range []string{VendorSelectorStrongest, VendorSelectorCheapest} {
		got, passed := reg.VendorRecordsFor(airoute.LevelStrong, selector)
		if !reflect.DeepEqual(got, []string{"opus"}) {
			t.Fatalf("%s = %v, want the record that declares a level", selector, got)
		}
		if passed["legacy"] != "declares no level, so a vendor selector does not choose it" {
			t.Fatalf("legacy was passed over with %q", passed["legacy"])
		}
	}
	if _, ok := reg.Entry("legacy"); !ok {
		t.Fatal("a record with no level no longer resolves by name")
	}
}

// Records that are not a vendor's text model are neither chosen nor reported:
// a selector was never about them.
func TestOnlyAVendorsTextRecordsAreASelectorsBusiness(t *testing.T) {
	reg := catalogForTest("opus")
	reg.setEntry(&ProviderConfigEntry{Config: ProviderConfig{Name: "embed", Type: "OpenAIEmbedding", Modality: "embedding"}, Available: true})
	reg.setEntry(&ProviderConfigEntry{Config: ProviderConfig{Name: "anthropic", Type: "Anthropic", Base: true}})
	got, passed := reg.VendorRecordsFor(airoute.LevelStrong, VendorSelectorStrongest)
	if !reflect.DeepEqual(got, []string{"opus"}) || len(passed) != 0 {
		t.Fatalf("got %v, passed over %v; want opus and nothing else said", got, passed)
	}
}

// The same catalog, built in two orders, answers in one order: nothing in the
// answer depends on the order records were registered or on map iteration.
func TestTwoReplicasOrderAlike(t *testing.T) {
	one := catalogForTest("haiku", "luna", "sonnet", "astra", "opus", "legacy", "free")
	two := catalogForTest("free", "legacy", "opus", "astra", "sonnet", "luna", "haiku")
	for _, selector := range []string{VendorSelectorStrongest, VendorSelectorCheapest} {
		for _, level := range []airoute.Level{airoute.LevelFast, airoute.LevelStrong, airoute.LevelReasoning} {
			for range 20 {
				a, passedA := one.VendorRecordsFor(level, selector)
				b, passedB := two.VendorRecordsFor(level, selector)
				if !reflect.DeepEqual(a, b) || !reflect.DeepEqual(passedA, passedB) {
					t.Fatalf("%s at %s: %v / %v, and passed over %v / %v", selector, level, a, b, passedA, passedB)
				}
			}
		}
	}
}

// A level that is not one of the three has no vendor text model declared for
// it, by the loader's own rule, and a selector answers nothing for it.
func TestASelectorAnswersNothingForALevelNoRecordMayDeclare(t *testing.T) {
	reg := catalogForTest("haiku", "opus")
	for _, level := range []airoute.Level{airoute.LevelEmbeddings, ""} {
		if got, _ := reg.VendorRecordsFor(level, VendorSelectorStrongest); len(got) != 0 {
			t.Errorf("level %q = %v, want nothing", level, got)
		}
	}
}

// The chain cloud consent walks means the same thing by "strongest": rank,
// then declared window, then name.
func TestConsentChainIsRankOrder(t *testing.T) {
	reg := catalogForTest("haiku", "luna", "sonnet", "astra", "opus", "legacy", "free")
	// legacy declares no rank and the widest window: a window does not outrank
	// a rank, and it orders the records that declare none.
	legacy, _ := reg.Entry("legacy")
	legacy.Config.Params["contextWindow"] = float64(1000000)
	want := []string{"astra", "opus", "sonnet", "haiku", "luna", "free", "legacy"}
	if got := reg.FederatedByStrength(); !reflect.DeepEqual(got, want) {
		t.Fatalf("FederatedByStrength = %v, want %v", got, want)
	}
}
