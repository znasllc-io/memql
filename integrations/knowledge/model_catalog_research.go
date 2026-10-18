package knowledge

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"slices"
	"strings"
	"time"

	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/core/common"
	"github.com/znasllc-io/memql/integrations/knowledge/seeddata"
)

type catalogFinding struct {
	ModelID  string   `json:"modelId"`
	Category string   `json:"category"`
	Runtime  string   `json:"runtime"`
	Summary  string   `json:"summary"`
	Sources  []string `json:"sources"`
}
type catalogResearchReport struct {
	Summary  string           `json:"summary"`
	Findings []catalogFinding `json:"findings"`
}

// Research never writes operational model profiles. These are unverified
// candidates, including cloud discoveries, with no recommendation authority.
func parseCatalogResearchReport(raw string) (catalogResearchReport, error) {
	var report catalogResearchReport
	if len(raw) > 65536 {
		return report, fmt.Errorf("catalog report exceeds its size bound")
	}
	if err := json.Unmarshal([]byte(raw), &report); err != nil {
		return report, fmt.Errorf("catalog research must return valid JSON: %w", err)
	}
	if strings.TrimSpace(report.Summary) == "" || len(report.Summary) > 2000 || len(report.Findings) > 20 {
		return report, fmt.Errorf("catalog research needs a short summary and at most 20 findings")
	}
	seen := map[string]bool{}
	for _, f := range report.Findings {
		if strings.TrimSpace(f.ModelID) == "" || len(f.ModelID) > 160 || seen[f.ModelID] || len(f.Summary) > 600 || strings.TrimSpace(f.Summary) == "" {
			return report, fmt.Errorf("invalid or duplicate model finding")
		}
		seen[f.ModelID] = true
		if !slices.Contains([]string{"text", "reasoning", "omni", "vision", "audioIn", "audioOut", "imageGen", "videoGen", "embeddings"}, f.Category) || !slices.Contains([]string{"ollama", "mlx", "whispercpp", "nemo", "kokoro", "mflux", "comfyui", "openai-compatible", "cloud"}, f.Runtime) {
			return report, fmt.Errorf("unsupported finding category or runtime")
		}
		if len(f.Sources) == 0 || len(f.Sources) > 4 {
			return report, fmt.Errorf("each finding needs 1–4 source URLs")
		}
		for _, source := range f.Sources {
			u, err := url.Parse(source)
			if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || len(source) > 2048 {
				return report, fmt.Errorf("catalog sources must be public HTTPS URLs")
			}
		}
	}
	return report, nil
}

func (i *Integration) handleModelCatalogResearchTurn(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	run, ok := common.RunFromContext(ctx)
	if !ok || !run.IsRun() || !run.Routing.NoPaidProviders || !strings.HasPrefix(run.Routing.Source, "fleet:") {
		return nil, fmt.Errorf("Catalog research requires a local-only work run with paid providers disabled")
	}
	te, ok := i.engine.(trainerEngine)
	if !ok {
		return nil, fmt.Errorf("bounded research harness unavailable")
	}
	var method []string
	for _, chunk := range seeddata.Release() {
		if chunk.SourceRef == "training:model-catalog" {
			method = append(method, chunk.Text)
		}
	}
	if len(method) == 0 {
		return nil, fmt.Errorf("model catalog research knowledge is missing")
	}
	text, err := te.InvokeAIChatWithFilteredTools(ctx, "modelCatalogResearch", map[string]any{"catalog": args["catalog"], "method": strings.Join(method, "\n\n"), "today": time.Now().UTC().Format("2006-01-02")}, []string{"webSearch", "fetchUrl"})
	if err != nil {
		return nil, err
	}
	report, err := parseCatalogResearchReport(text)
	if err != nil {
		return nil, err
	}
	payload, err := json.Marshal(map[string]any{"runId": run.RunId, "summary": report.Summary, "findings": report.Findings})
	if err != nil {
		return nil, err
	}
	return []memorynodes.MemoryNode{{ID: "model-catalog-research", Concept: "v1:knowledge:result", Type: memorynodes.NodeTypeObject, CreatedAt: time.Now().UTC(), Payload: payload}}, nil
}
