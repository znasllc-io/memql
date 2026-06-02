// trainer_tools.go
//
// Go-backed handlers for three of the Trainer Agent's tools (Q3 / Q9):
//
//   integration.knowledge.webSearch  -- discover sources for a topic.
//   integration.knowledge.fetchUrl   -- fetch + extract readable text.
//   integration.knowledge.embedChunk -- mark a chunk retrievable.
//
// The other two trainer tools (writeKnowledgeChunk, markChunkSuperseded)
// are mutation-backed and route through the tool's @handler directly --
// they don't need a Go handler here.
//
// These are registered as additional capabilities on the existing
// knowledge integration (see capabilities.go). The DSL builtins that
// front them live in dsl/common/builtins.memql; the tools that bind to
// those builtins live in dsl/agents/tools/trainerTools.memql; and the
// dispatcher that invokes the Trainer with this tool set lives in
// integrations/planner/train_specialist_dispatch.go.
//
// STUB SURFACE (called out explicitly per the issue's pragmatism note):
//
//   - webSearch has NO web-search provider wired in-repo. Rather than
//     invent a network/search layer, it returns an empty result set
//     plus a note. The Trainer's prompt already instructs it to fall
//     back to fetchUrl on URLs it knows. Wiring a real search provider
//     (Brave / Bing / SerpAPI / Tavily) is a follow-up.
//
//   - fetchUrl is REAL: it does a bounded HTTP GET and extracts text.
//     This is the load-bearing grounding primitive -- the Trainer can
//     pull a known authoritative URL and distill it into chunks.
//
//   - embedChunk is REAL as of #645 (lazy embedding): it resolves the
//     chunk's text, computes a vector via the embedding provider, and
//     writes it to node_vectors keyed by the chunk id (vector_field
//     'content') so the chunk becomes retrievable by similarTo. The
//     handler lives in embed_domain.go alongside the domain-wide
//     embedDomainItems capability the Plan dispatcher drives.

package knowledge

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"regexp"
	"strings"
	"time"

	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
)

// Trainer-tool capability names (the suffix after
// "integration.knowledge."). Kept as constants so the capability
// registration and any tests reference one source of truth.
const (
	capWebSearch        = "webSearch"
	capFetchURL         = "fetchUrl"
	capEmbedChunk       = "embedChunk"
	capEmbedDomainItems = "embedDomainItems"
)

// fetchUrl limits. A bounded client + response cap keeps a Trainer run
// from stalling on a slow / huge page. Mirrors the workbench http_fetch
// guards (integrations/workbench/dispatch.go) without sharing state.
const (
	trainerFetchTimeout     = 30 * time.Second
	trainerFetchDialTimeout = 10 * time.Second
	trainerFetchMaxBytes    = 2 << 20 // 2 MiB cap on a fetched page
	trainerFetchMaxRedirect = 10
)

// trainerFetchClient is the bounded http.Client used by fetchUrl.
// Independent of http.DefaultClient so the limits stay tight.
var trainerFetchClient = &http.Client{
	Timeout: trainerFetchTimeout,
	Transport: &http.Transport{
		DialContext: (&net.Dialer{
			Timeout:   trainerFetchDialTimeout,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		TLSHandshakeTimeout:   trainerFetchDialTimeout,
		ResponseHeaderTimeout: trainerFetchDialTimeout,
		ExpectContinueTimeout: 1 * time.Second,
		MaxIdleConns:          8,
		IdleConnTimeout:       60 * time.Second,
	},
	CheckRedirect: func(_ *http.Request, via []*http.Request) error {
		if len(via) >= trainerFetchMaxRedirect {
			return fmt.Errorf("stopped after %d redirects", trainerFetchMaxRedirect)
		}
		return nil
	},
}

// webSearchHandler is the STUB web-search tool. No search provider is
// wired in-repo, so it returns an empty result set + a note instead of
// inventing a network layer. The Trainer falls back to fetchUrl.
//
// When a provider IS wired (follow-up), this handler issues the query
// and returns []{url, title, snippet}; the return shape below is the
// contract that future wiring fills in.
func (i *Integration) webSearchHandler(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	query, _ := args["query"].(string)
	if strings.TrimSpace(query) == "" {
		return nil, fmt.Errorf("knowledge.webSearch: query is required")
	}
	if i.Logger != nil {
		i.Logger.Info("knowledge.webSearch: STUB invoked (no search provider wired)",
			"query", query)
	}
	body, _ := json.Marshal(map[string]any{
		"query":   query,
		"results": []any{},
		"stub":    true,
		"note":    "webSearch has no search provider wired in-repo yet. Use fetchUrl on a URL you already know to ground a chunk. (Follow-up: wire a search provider.)",
	})
	return []memorynodes.MemoryNode{{
		ID:        "trainer-websearch-result",
		Concept:   "integration:knowledge:webSearch",
		Type:      memorynodes.NodeTypeObject,
		CreatedAt: time.Now().UTC(),
		Payload:   body,
	}}, nil
}

// fetchUrlHandler does a bounded HTTP GET and extracts readable text.
// This is the Trainer's real grounding primitive -- it pulls a known
// authoritative source and the Trainer distills it into chunks (citing
// the URL as sourceRef).
func (i *Integration) fetchUrlHandler(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	rawURL, _ := args["url"].(string)
	rawURL = strings.TrimSpace(rawURL)
	if rawURL == "" {
		return nil, fmt.Errorf("knowledge.fetchUrl: url is required")
	}
	if !strings.HasPrefix(rawURL, "http://") && !strings.HasPrefix(rawURL, "https://") {
		return nil, fmt.Errorf("knowledge.fetchUrl: url must be http or https")
	}

	reqCtx, cancel := context.WithTimeout(ctx, trainerFetchTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, fmt.Errorf("knowledge.fetchUrl: build request: %w", err)
	}
	req.Header.Set("User-Agent", "memql-trainer-agent/1.0 (+knowledge-seeding)")
	req.Header.Set("Accept", "text/html,text/plain,application/xhtml+xml,*/*")

	resp, err := trainerFetchClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("knowledge.fetchUrl: GET %s: %w", rawURL, err)
	}
	defer resp.Body.Close()

	limited := io.LimitReader(resp.Body, trainerFetchMaxBytes+1)
	raw, err := io.ReadAll(limited)
	if err != nil {
		return nil, fmt.Errorf("knowledge.fetchUrl: read body: %w", err)
	}
	truncated := false
	if len(raw) > trainerFetchMaxBytes {
		raw = raw[:trainerFetchMaxBytes]
		truncated = true
	}

	contentType := resp.Header.Get("Content-Type")
	text := extractReadableText(string(raw), contentType)

	if i.Logger != nil {
		i.Logger.Info("knowledge.fetchUrl: fetched",
			"url", rawURL, "status", resp.StatusCode,
			"bytes", len(raw), "textLen", len(text), "truncated", truncated)
	}

	body, _ := json.Marshal(map[string]any{
		"url":         rawURL,
		"status":      resp.StatusCode,
		"contentType": contentType,
		"text":        text,
		"truncated":   truncated,
	})
	return []memorynodes.MemoryNode{{
		ID:        "trainer-fetchurl-result",
		Concept:   "integration:knowledge:fetchUrl",
		Type:      memorynodes.NodeTypeObject,
		CreatedAt: time.Now().UTC(),
		Payload:   body,
	}}, nil
}

// embedChunkHandler now lives in embed_domain.go (#645): the real
// embed-and-store path plus the domain-wide embedDomainItems capability
// the Plan dispatcher drives.

// --- readable-text extraction -------------------------------------------

var (
	scriptStyleRe = regexp.MustCompile(`(?is)<(script|style|noscript|head)[^>]*>.*?</(script|style|noscript|head)>`)
	tagRe         = regexp.MustCompile(`(?s)<[^>]+>`)
	wsRe          = regexp.MustCompile(`[ \t]+`)
	blankLinesRe  = regexp.MustCompile(`\n{3,}`)
)

// extractReadableText reduces an HTML (or plain-text) body to readable
// text. Deliberately simple -- strips script/style/head + tags, collapses
// whitespace. Good enough to feed the Trainer's reasoning model, which
// distills rather than parses structure. Plain-text bodies pass through
// untouched (no tags to strip).
func extractReadableText(body, contentType string) string {
	if strings.Contains(strings.ToLower(contentType), "text/plain") {
		return strings.TrimSpace(body)
	}
	out := scriptStyleRe.ReplaceAllString(body, " ")
	out = tagRe.ReplaceAllString(out, " ")
	out = htmlUnescapeBasic(out)
	out = wsRe.ReplaceAllString(out, " ")
	// Normalize line breaks: collapse runs of blank lines.
	out = strings.ReplaceAll(out, "\r\n", "\n")
	out = blankLinesRe.ReplaceAllString(out, "\n\n")
	return strings.TrimSpace(out)
}

// htmlUnescapeBasic handles the handful of entities common in extracted
// prose. Avoids pulling in golang.org/x/net/html for a stub-level path.
func htmlUnescapeBasic(s string) string {
	r := strings.NewReplacer(
		"&amp;", "&",
		"&lt;", "<",
		"&gt;", ">",
		"&quot;", `"`,
		"&#39;", "'",
		"&apos;", "'",
		"&nbsp;", " ",
	)
	return r.Replace(s)
}
