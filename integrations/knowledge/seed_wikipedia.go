package knowledge

// Tier-C authoritative-source ingest via Wikipedia.
//
// Per docs/planning/knowledge-seeder.md: Tier C domains (clinical
// medicine, surgical specialties, etc.) are deemed too high-stakes
// for LLM-generated baseline content. The original design called
// for them to ship with a single "upload your own authoritative
// content" placeholder. This file implements the second path:
// fetch + chunk + embed Wikipedia article content for Tier C
// domains that have an article mapping (tierCWikipediaArticles in
// seed.go).
//
// Why Wikipedia?
//   - It's the most accessible authoritative-ish source we can
//     pull without per-source license negotiation. CC-BY-SA license
//     covers our use IF we attribute (we encode the article title
//     in seedSource so retrieval can surface it).
//   - Medical / surgical / clinical articles on Wikipedia are
//     generally high-quality (heavy editor scrutiny, citations) --
//     not authoritative for clinical decisions, but better than
//     LLM hallucination for educational content.
//   - The Wikipedia REST API exposes plain-text article extracts +
//     section content via simple HTTP calls -- no API key required.
//
// What we DON'T do (worth being explicit):
//   - Don't fetch full article + parse wikitext. We pull the
//     summary extract + a handful of leading sections. Enough
//     for retrieval-friendly chunks; avoids the rabbit hole of
//     full wikitext rendering.
//   - Don't follow Wikipedia's "main article" links. Each
//     configured article is fetched standalone.
//   - Don't deduplicate chunks across multiple articles within
//     the same domain -- the embed loop catches near-duplicates
//     at retrieval time.
//   - Don't try to update on Wikipedia revision changes. That's
//     a freshness-via-recipe-bump play (see Phase 2 freshness
//     check in trainAgent).

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// wikipediaUserAgent is what we send in the User-Agent header.
// Wikipedia's API policy asks for a contact / project identifier
// so abusive callers can be rate-limited / contacted; we identify
// ourselves as memQL with the project URL.
const wikipediaUserAgent = "memQL/1.0 (https://github.com/znasllc-io/memql; knowledge-seeder)"

// wikipediaSummaryAPI is the REST API endpoint that returns a
// page summary -- title, extract (plain-text intro), description.
// Stable, simple, doesn't need an API key.
const wikipediaSummaryAPI = "https://en.wikipedia.org/api/rest_v1/page/summary/%s"

// wikipediaContentAPI returns the full article content as parsed
// HTML / plain-text sections. Used to expand beyond the summary
// extract when we want richer chunks.
const wikipediaContentAPI = "https://en.wikipedia.org/w/api.php?action=query&prop=extracts&explaintext=1&exsectionformat=plain&format=json&titles=%s"

// wikipediaArticleSummary is the shape we extract from the REST
// summary endpoint. Other fields exist on the response; we ignore
// what we don't need.
type wikipediaArticleSummary struct {
	Title       string `json:"title"`
	Description string `json:"description"`
	Extract     string `json:"extract"`
	ContentURLs struct {
		Desktop struct {
			Page string `json:"page"`
		} `json:"desktop"`
	} `json:"content_urls"`
}

// wikipediaContentResponse is the shape of the action API extract
// response. Pages map keyed by page id (or "-1" if missing).
type wikipediaContentResponse struct {
	Query struct {
		Pages map[string]struct {
			Title   string `json:"title"`
			Extract string `json:"extract"`
			Missing string `json:"missing"`
		} `json:"pages"`
	} `json:"query"`
}

// wikipediaHTTPClient is the shared client. 30-second timeout
// because Wikipedia is generally fast but we don't want a hung
// connection to stall the entire seeder run.
var wikipediaHTTPClient = &http.Client{
	Timeout: 30 * time.Second,
}

// fetchWikipediaArticle pulls the title + extract + canonical URL
// for a given article. Combines the summary endpoint (for the
// intro paragraph) with the content endpoint (for the rest).
// Returns a single concatenated text body suitable for chunking.
//
// The article name is the human-readable title -- "General surgery"
// not "general_surgery". Wikipedia normalises common variants
// internally so case + hyphenation are flexible, but exact title
// matches are most reliable.
//
// Returns the title, body text, and canonical URL. Returns an
// error if the article doesn't exist or the API is unreachable.
func fetchWikipediaArticle(ctx context.Context, articleName string) (title, body, canonicalURL string, err error) {
	encoded := url.PathEscape(strings.ReplaceAll(articleName, " ", "_"))

	// Step 1: fetch the REST summary (intro paragraph + canonical URL).
	summary, err := fetchWikipediaSummary(ctx, encoded)
	if err != nil {
		return "", "", "", fmt.Errorf("fetch summary for %q: %w", articleName, err)
	}
	if strings.TrimSpace(summary.Extract) == "" {
		return "", "", "", fmt.Errorf("article %q has empty extract (likely a disambiguation or redirect page)", articleName)
	}

	// Step 2: fetch the action API extract (full article plain text).
	// Best-effort -- if this fails we still return the summary
	// extract alone.
	fullText, err := fetchWikipediaFullExtract(ctx, encoded)
	if err != nil {
		// Fall back to summary-only.
		return summary.Title, summary.Extract, summary.ContentURLs.Desktop.Page, nil
	}

	// Combine: summary extract first (it's usually the cleanest
	// intro), then full article body. Strip duplicates if the
	// summary text is a prefix of the full extract.
	if !strings.HasPrefix(strings.TrimSpace(fullText), strings.TrimSpace(summary.Extract)) {
		body = summary.Extract + "\n\n" + fullText
	} else {
		body = fullText
	}
	body = cleanWikipediaText(body)

	return summary.Title, body, summary.ContentURLs.Desktop.Page, nil
}

func fetchWikipediaSummary(ctx context.Context, encodedTitle string) (*wikipediaArticleSummary, error) {
	apiURL := fmt.Sprintf(wikipediaSummaryAPI, encodedTitle)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", wikipediaUserAgent)
	req.Header.Set("Accept", "application/json")

	resp, err := wikipediaHTTPClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("wikipedia summary HTTP %d", resp.StatusCode)
	}
	bodyBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	var out wikipediaArticleSummary
	if err := json.Unmarshal(bodyBytes, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func fetchWikipediaFullExtract(ctx context.Context, encodedTitle string) (string, error) {
	apiURL := fmt.Sprintf(wikipediaContentAPI, encodedTitle)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiURL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", wikipediaUserAgent)
	req.Header.Set("Accept", "application/json")

	resp, err := wikipediaHTTPClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("wikipedia content HTTP %d", resp.StatusCode)
	}
	bodyBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	var out wikipediaContentResponse
	if err := json.Unmarshal(bodyBytes, &out); err != nil {
		return "", err
	}
	for _, page := range out.Query.Pages {
		if page.Missing != "" {
			return "", fmt.Errorf("article missing")
		}
		return page.Extract, nil
	}
	return "", fmt.Errorf("no pages in response")
}

// cleanWikipediaText strips Wikipedia-isms that survive the API's
// plain-text rendering: the trailing "References" and "External
// links" sections, parenthetical citations, multiple consecutive
// blank lines.
func cleanWikipediaText(text string) string {
	// Cut at known trailing-section headers (these are usually
	// bibliographic and add no retrieval value).
	cutoffs := []string{
		"\n== References ==",
		"\n== External links ==",
		"\n== See also ==",
		"\n== Further reading ==",
		"\n== Notes ==",
	}
	for _, c := range cutoffs {
		if idx := strings.Index(text, c); idx > 0 {
			text = text[:idx]
		}
	}
	// Collapse 3+ consecutive newlines to 2.
	multiNL := regexp.MustCompile(`\n{3,}`)
	text = multiNL.ReplaceAllString(text, "\n\n")
	return strings.TrimSpace(text)
}

// writeTierCWikipediaChunks fetches each configured Wikipedia
// article for the domain, chunks it via the existing knowledge
// chunker, embeds, and stores. Each chunk carries
// seedSource = "wikipedia:<article title>" + tier = "C" so retrieval
// metadata makes the source obvious.
//
// On a per-article fetch failure we log + continue with the next
// article. Returns the total chunks written across all articles.
func (i *Integration) writeTierCWikipediaChunks(
	ctx context.Context,
	d StandardDomain,
	articles []string,
	recipeVersion string,
) (int, error) {
	if len(articles) == 0 {
		return 0, fmt.Errorf("writeTierCWikipediaChunks: no articles configured for %q", d.ID)
	}
	provider, err := i.embeddingProvider(defaultProvider)
	if err != nil {
		return 0, fmt.Errorf("resolve embedding provider %q: %w", defaultProvider, err)
	}
	partition := i.resolvePartition(ctx)

	written := 0
	chunkIndex := 0

	for _, articleName := range articles {
		title, body, canonicalURL, err := fetchWikipediaArticle(ctx, articleName)
		if err != nil {
			i.Logger.Warn("knowledge.tierC.wikipedia: fetch failed",
				"domainId", d.ID, "article", articleName, "err", err)
			continue
		}
		if strings.TrimSpace(body) == "" {
			i.Logger.Warn("knowledge.tierC.wikipedia: empty body",
				"domainId", d.ID, "article", articleName)
			continue
		}

		// Chunk via the existing knowledge integration's chunker
		// (paragraph + sentence-boundary aware).
		chunks := Chunk(body, defaultChunkSize, defaultOverlap)
		if len(chunks) == 0 {
			continue
		}
		i.Logger.Info("knowledge.tierC.wikipedia: ingesting article",
			"domainId", d.ID, "article", articleName, "chunkCount", len(chunks))

		// Wikipedia content carries an attribution requirement (CC-BY-SA).
		// We encode the title + canonical URL in the seedSource and as
		// a metadata header in the chunk body so retrieval-time UIs can
		// surface attribution.
		seedSource := "wikipedia:" + title

		for _, chunkText := range chunks {
			if strings.TrimSpace(chunkText) == "" {
				chunkIndex++
				continue
			}

			chunk := seedChunk{
				Kind:     "factExample",
				Title:    fmt.Sprintf("%s: %s (Wikipedia)", d.Name, title),
				Body:     fmt.Sprintf("Source: Wikipedia article \"%s\" (%s; CC-BY-SA).\n\n%s", title, canonicalURL, chunkText),
				KeyTerms: []string{"wikipedia", title, d.Name},
			}
			if err := i.storeSeedChunk(ctx, partition, d, recipeVersion, chunkIndex, chunk, seedSource, "llmSeeded", provider); err != nil {
				i.Logger.Warn("knowledge.tierC.wikipedia: chunk write failed",
					"domainId", d.ID, "article", articleName, "chunkIndex", chunkIndex, "err", err)
				chunkIndex++
				continue
			}
			written++
			chunkIndex++
		}
	}

	return written, nil
}
