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
