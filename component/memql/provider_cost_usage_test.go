package memql

import (
	"context"
	"github.com/znasllc-io/memql/core/common"
	"testing"
)

func TestProviderUsageUsesServedModelAndExplicitCacheCounts(t *testing.T) {
	ctx := common.WithProviderUsage(context.Background())
	reportOpenAIUsage(ctx, `{"model":"gpt-6.1-sol","usage":{"prompt_tokens":120,"completion_tokens":7,"prompt_tokens_details":{"cached_tokens":80}}}`)
	got := common.ReadProviderUsage(ctx)
	if got.Model != "gpt-6.1-sol" || !got.Usage.Known || got.Usage.Input != 120 || *got.Usage.CacheRead != 80 || got.Usage.CacheWriteShort != nil {
		t.Fatalf("unexpected receipt: %+v", got)
	}
	// A later chunk with no usage cannot erase the provider's receipt.
	reportOpenAIUsage(ctx, `{"model":"requested-alias"}`)
	if common.ReadProviderUsage(ctx).Model != got.Model {
		t.Fatal("missing usage overwrote receipt")
	}
	retry := common.WithProviderUsage(ctx)
	if common.ReadProviderUsage(retry).Usage.Known {
		t.Fatal("retry borrowed previous attempt usage")
	}
}

func TestAnthropicUsageNormalizesInputOnceAcrossStream(t *testing.T) {
	ctx := common.WithProviderUsage(context.Background())
	reportAnthropicUsage(ctx, `{"type":"message_start","message":{"model":"claude-sonnet-5-5","usage":{"input_tokens":20,"output_tokens":1,"cache_read_input_tokens":80,"cache_creation_input_tokens":30,"cache_creation":{"ephemeral_5m_input_tokens":10,"ephemeral_1h_input_tokens":20}}}}`)
	reportAnthropicUsage(ctx, `{"type":"message_delta","usage":{"output_tokens":11}}`)
	got := common.ReadProviderUsage(ctx)
	if got.Model != "claude-sonnet-5-5" || got.Usage.Input != 130 || got.Usage.Output != 11 || *got.Usage.CacheWriteLong != 20 {
		t.Fatalf("unexpected normalized receipt: %+v", got)
	}
	empty := common.WithProviderUsage(context.Background())
	reportAnthropicUsage(empty, `{"usage":{"output_tokens":11}}`)
	if common.ReadProviderUsage(empty).Usage.Known {
		t.Fatal("output-only delta invented input coverage")
	}
}

func TestProviderModelDoesNotInventMissingUsage(t *testing.T) {
	ctx := common.WithProviderUsage(context.Background())
	reportOpenAIUsage(ctx, `{"model":"gpt-6.1-sol"}`)
	if got := common.ReadProviderUsage(ctx); got.Model != "gpt-6.1-sol" || got.Usage.Known {
		t.Fatalf("model-only receipt: %+v", got)
	}
	ctx = common.WithProviderUsage(context.Background())
	reportAnthropicUsage(ctx, `{"model":"claude-sonnet-5-5","usage":{"input_tokens":20}}`)
	if common.ReadProviderUsage(ctx).Usage.Known {
		t.Fatal("missing output priced as zero")
	}
	reportAnthropicUsage(ctx, `{"usage":{"output_tokens":3}}`)
	if got := common.ReadProviderUsage(ctx); !got.Usage.Known || got.Usage.Input != 20 || got.Usage.Output != 3 {
		t.Fatalf("partial stream receipt: %+v", got)
	}
}
