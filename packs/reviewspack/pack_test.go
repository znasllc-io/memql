package reviewspack_test

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"log/slog"
	"strings"
	"testing"

	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/memql"
	memqldsl "github.com/znasllc-io/memql/dsl"
	reviewspack "github.com/znasllc-io/memql/packs/reviewspack"
)

const uniqueDomain = "reviewspacktest"

func TestReviewsPackLoadsAndExtends(t *testing.T) {
	logger := slog.Default()
	memqldsl.RegisterTree(uniqueDomain, reviewspack.Tree())
	t.Cleanup(func() { memqldsl.UnregisterTree(uniqueDomain) })

	if _, err := memqldsl.Tree().Open(uniqueDomain + "/concepts.memql"); err != nil {
		t.Fatalf("pack concepts.memql not reachable via dsl.Tree(): %v", err)
	}
	if _, err := memql.LoadUnifiedConcepts(logger); err != nil {
		t.Fatalf("LoadUnifiedConcepts failed: %v", err)
	}
	for _, id := range []string{"v1:reviews:review", "v1:reviews:moderationAction", "v1:reviews:reviewSettings"} {
		if _, err := memorynodes.DefaultRegistry().Get(id); err != nil {
			t.Fatalf("pack concept %q MUST be registered after LoadUnifiedConcepts: %v", id, err)
		}
	}
}

func TestClosedCriterionEnum(t *testing.T) {
	raw, err := fs.ReadFile(reviewspack.Tree(), "concepts.memql")
	if err != nil {
		t.Fatal(err)
	}
	src := string(raw)
	if strings.Contains(src, `"other"`) || strings.Contains(src, "enum(\"other\"") || strings.Contains(src, ", \"other\"") {
		t.Fatal("criterion enum must not include other")
	}
	for _, line := range strings.Split(src, "\n") {
		trim := strings.TrimSpace(line)
		if strings.HasPrefix(trim, "//") || strings.HasPrefix(trim, "///") {
			continue
		}
		if strings.HasPrefix(trim, "sentiment ") {
			t.Fatalf("sentiment must not be a field: %s", trim)
		}
	}
	for _, c := range []string{"spam", "profanity", "harassment", "off_topic", "illegal", "copyright"} {
		if !strings.Contains(src, `"`+c+`"`) {
			t.Fatalf("closed criterion %q missing from concepts.memql", c)
		}
	}
}

// THE LEGACY MODERATION CAPABILITY IS GONE. It accepted any signed-in caller
// and persisted nothing; a decision is recorded only through reviewsDecide.
func TestTheLegacyModerationCapabilityIsGone(t *testing.T) {
	provider, err := reviewspack.NewProvider(memql.PluginContext{Logger: slog.Default()})
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range provider.Capabilities() {
		if c.Name == "recordModerationAction" {
			t.Fatal("recordModerationAction is still a capability of the pack")
		}
	}
	raw, err := fs.ReadFile(reviewspack.Tree(), "builtins.memql")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "reviewsRecordModerationAction") {
		t.Fatal("reviewsRecordModerationAction is still declared")
	}
}

func TestExportIncludesImageBytesNotURLs(t *testing.T) {
	if _, err := reviewspack.ExportImages([]any{
		map[string]any{"name": "a.jpg", "url": "https://cdn.example/a.jpg"},
	}); err == nil {
		t.Fatal("URL-only image must be refused")
	}
	files, err := reviewspack.ExportImages([]any{
		map[string]any{"name": "a.jpg", "bytes": []byte{0xff, 0xd8, 0xff}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 || len(files[0].Bytes) == 0 || files[0].Name != "a.jpg" {
		t.Fatalf("files=%+v", files)
	}
	provider, err := reviewspack.NewProvider(memql.PluginContext{Logger: slog.Default()})
	if err != nil {
		t.Fatal(err)
	}
	var handler func(context.Context, map[string]any, int) ([]memorynodes.MemoryNode, error)
	for _, c := range provider.Capabilities() {
		if c.Name == "exportReview" {
			handler = c.Handler
		}
	}
	nodes, err := handler(context.Background(), map[string]any{
		"reviewId": "r1",
		"images":   []any{map[string]any{"name": "a.jpg", "bytes": "ffd8ff"}},
	}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(nodes[0].Payload), "https://") {
		t.Fatal("export payload leaked a URL")
	}
	var payload map[string]any
	if err := json.Unmarshal(nodes[0].Payload, &payload); err != nil {
		t.Fatal(err)
	}
	rawFiles, _ := payload["files"].([]any)
	if len(rawFiles) != 1 {
		t.Fatalf("files=%v", payload["files"])
	}
}

func TestPublicDisplayToggleIsData(t *testing.T) {
	provider, err := reviewspack.NewProvider(memql.PluginContext{Logger: slog.Default()})
	if err != nil {
		t.Fatal(err)
	}
	var handler func(context.Context, map[string]any, int) ([]memorynodes.MemoryNode, error)
	for _, c := range provider.Capabilities() {
		if c.Name == "setPublicDisplay" {
			handler = c.Handler
		}
	}
	// PER STORE (epic memql#5532, design D9). A toggle with no store is
	// REFUSED rather than defaulted: the id used to be one literal for the
	// whole cluster, so flipping reviews on for a live storefront also
	// flipped them on for the development store a candidate was being
	// exercised against, and for every other merchant this cluster serves.
	if _, err := handler(context.Background(), map[string]any{"publicDisplay": true}, 0); err == nil {
		t.Fatal("a display toggle naming no store must be refused")
	}
	// The per-store row, and the merge that keeps every other setting, are
	// pinned where a reader can answer the current row: settings_test.go
	// (TestTheDisplayToggleIsPerStore, TestTheDisplayToggleKeepsEveryOtherSetting).
}

func TestReviewsPackContractCompat(t *testing.T) {
	if err := memql.CheckPluginContractCompat(reviewspack.ContractVersion); err != nil {
		t.Fatal(err)
	}
	reg := memql.PluginRegistration{Name: reviewspack.Domain, RequiresContractVersion: reviewspack.ContractVersion}
	if err := reg.ValidateContract(); err != nil {
		t.Fatal(err)
	}
}

func TestReviewsPackTreeParses(t *testing.T) {
	tree := reviewspack.Tree()
	err := fs.WalkDir(tree, ".", func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() || !strings.HasSuffix(path, ".memql") {
			return nil
		}
		raw, readErr := fs.ReadFile(tree, path)
		if readErr != nil {
			t.Errorf("%s: read: %v", path, readErr)
			return nil
		}
		rewritten, rewriteErr := parser.NormaliseAll(string(raw))
		if rewriteErr != nil {
			t.Errorf("%s: rewrite: %v", path, rewriteErr)
			return nil
		}
		if _, parseErr := parser.ParseFile(rewritten); parseErr != nil && !errors.Is(parseErr, parser.ErrEmptyInput) {
			t.Errorf("%s: parse: %v", path, parseErr)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk pack tree: %v", err)
	}
}
