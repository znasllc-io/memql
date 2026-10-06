package knowledge

import (
	"context"
	"errors"
	"github.com/znasllc-io/memql/component/automations/workflowhost"
	"reflect"
	"testing"
)

func TestSeedRecipeSeparatesProviderFailureFromOptionalChunkFailure(t *testing.T) {
	for _, providerFails := range []bool{false, true} {
		calls := []string{}
		probe := func(name string, value any, err error) workflowhost.Operation {
			return func(context.Context, map[string]any) (any, error) { calls = append(calls, name); return value, err }
		}
		providerErr := error(nil)
		if providerFails {
			providerErr = errors.New("no embedder")
		}
		chunk := map[string]any{"kind": "principle", "title": "example", "body": "body", "keyTerms": []any{"example"}}
		_, err := workflowhost.Run(context.Background(), "knowledgeSeedDomainWorkflow", map[string]any{
			"domainId": "domain", "domainName": "Example", "domainDescription": "Example corpus", "category": "general", "tier": "B", "broadSurvey": false, "articles": []any{},
		}, workflowhost.Options{Operations: map[string]workflowhost.Operation{
			"knowledgeGenerateSeedChunks":  probe("generate", []any{chunk}, nil),
			"knowledgeResolveSeedEmbedder": probe("provider", nil, providerErr),
			"knowledgeStoreSeedChunk":      probe("write", nil, errors.New("write refused")),
			"knowledgeStampSeed":           probe("stamp", nil, nil),
			"knowledgeFetchSeedArticle":    probe("fetch", nil, nil), "knowledgeSplitSeedText": probe("split", nil, nil),
		}})
		want := []string{"generate", "provider", "write", "write", "stamp"}
		if providerFails {
			want = []string{"generate", "provider"}
			if !errors.Is(err, providerErr) {
				t.Fatalf("provider error lost: %v", err)
			}
		} else if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(calls, want) {
			t.Fatalf("providerFails=%v calls=%v want=%v", providerFails, calls, want)
		}
	}
}

func TestTierCRecipeDoesNotGenerateModelContent(t *testing.T) {
	generated, stored := 0, 0
	noop := func(context.Context, map[string]any) (any, error) { return nil, nil }
	_, err := workflowhost.Run(context.Background(), "knowledgeSeedDomainWorkflow", map[string]any{
		"domainId": "domain", "domainName": "Example", "domainDescription": "Example corpus", "category": "general", "tier": "C", "broadSurvey": false, "articles": []any{},
	}, workflowhost.Options{Operations: map[string]workflowhost.Operation{
		"knowledgeGenerateSeedChunks": func(context.Context, map[string]any) (any, error) { generated++; return nil, nil },
		"knowledgeStoreSeedChunk": func(_ context.Context, a map[string]any) (any, error) {
			stored++
			if a["seedSource"] != "tier-c-placeholder" {
				t.Fatalf("unexpected content: %v", a)
			}
			return nil, nil
		},
		"knowledgeResolveSeedEmbedder": noop, "knowledgeStampSeed": noop, "knowledgeFetchSeedArticle": noop, "knowledgeSplitSeedText": noop,
	}})
	if err != nil || generated != 0 || stored != 1 {
		t.Fatalf("err=%v generated=%d stored=%d", err, generated, stored)
	}
}
