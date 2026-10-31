package memql

import (
	"context"
	"encoding/json"

	"github.com/znasllc-io/memql/core/common"
	"github.com/znasllc-io/memql/core/costestimate"
)

// Consume the provider's response fields, never the requested model. Pointers
// distinguish omitted usage from an explicit zero. Unknown cache-write durations
// remain unknown and produce an estimate range instead of an invented split.
func reportOpenAIUsage(ctx context.Context, raw string) {
	var response struct {
		Model string `json:"model"`
		Usage *struct {
			Input   *int64 `json:"prompt_tokens"`
			Output  *int64 `json:"completion_tokens"`
			Details struct {
				Cached *int64 `json:"cached_tokens"`
			} `json:"prompt_tokens_details"`
		} `json:"usage"`
	}
	if json.Unmarshal([]byte(raw), &response) != nil {
		return
	}
	if response.Usage == nil || response.Usage.Input == nil || response.Usage.Output == nil {
		// Preserve a model-only response without inventing token coverage. An
		// empty later stream chunk must not erase the completed usage receipt.
		prior := common.ReadProviderUsage(ctx)
		if !prior.Usage.Known && response.Model != "" {
			common.ReportProviderUsage(ctx, common.ProviderUsage{Model: response.Model, Vendor: "openai"})
		}
		return
	}
	common.ReportProviderUsage(ctx, common.ProviderUsage{Model: response.Model, Vendor: "openai", Usage: costestimate.Usage{
		Input: *response.Usage.Input, Output: *response.Usage.Output, CacheRead: response.Usage.Details.Cached, Known: true}})
}

func reportAnthropicUsage(ctx context.Context, raw string) {
	var response struct {
		Model   string          `json:"model"`
		Usage   json.RawMessage `json:"usage"`
		Message json.RawMessage `json:"message"`
	}
	if json.Unmarshal([]byte(raw), &response) != nil {
		return
	}
	if len(response.Message) > 0 {
		reportAnthropicUsage(ctx, string(response.Message))
		return
	}
	var u struct {
		Input    *int64 `json:"input_tokens"`
		Output   *int64 `json:"output_tokens"`
		Read     *int64 `json:"cache_read_input_tokens"`
		Write    *int64 `json:"cache_creation_input_tokens"`
		Creation struct {
			Short *int64 `json:"ephemeral_5m_input_tokens"`
			Long  *int64 `json:"ephemeral_1h_input_tokens"`
		} `json:"cache_creation"`
	}
	if json.Unmarshal(response.Usage, &u) != nil {
		return
	}
	report := common.ReadProviderUsage(ctx)
	// message_delta omits input; retain the message_start receipt. Anthropic input
	// excludes reads and writes, unlike OpenAI, so normalize exactly once here.
	if u.Input != nil {
		report = common.ProviderUsage{Model: response.Model, Vendor: "anthropic", Usage: costestimate.Usage{Input: *u.Input, CacheRead: u.Read, CacheWriteShort: u.Creation.Short, CacheWriteLong: u.Creation.Long, Known: u.Output != nil}}
		if u.Read != nil {
			report.Usage.Input += *u.Read
		}
		if u.Write != nil {
			report.Usage.Input += *u.Write
		}
		if u.Write != nil && *u.Write == 0 {
			zero := int64(0)
			report.Usage.CacheWriteShort = &zero
			report.Usage.CacheWriteLong = &zero
		}
	}
	if u.Output != nil {
		report.Usage.Output = *u.Output
		if report.Vendor == "anthropic" {
			report.Usage.Known = true
		}
	}
	if response.Model != "" {
		report.Model = response.Model
	}
	if report.Vendor == "anthropic" {
		common.ReportProviderUsage(ctx, report)
	}
}
