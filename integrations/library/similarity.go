package library

// similarity.go -- librarySimilarArtifacts (memql#4342, design 3.2).
//
// "Search my Library by meaning." The chunks the analysis pass embedded
// are the substrate; this is the read over them, and it does three things
// the raw `similarTo` operator does not:
//
//  1. SCOPES THE RESULT TO THE CALLER. similarTo is concept-agnostic and
//     runs one hand-rolled statement over "MemoryNodes" joined to
//     node_vectors -- it applies no per-row authorization at all, because
//     it passes through neither the parser nor the filter path. So every
//     hit is checked here against the caller's own userId, and then again
//     structurally: the artifact each surviving hit folds up to is
//     re-read through libraryArtifactById, which IS owner-gated, and a
//     row that does not come back is dropped. Two gates, because the
//     first is a payload compare and the second is the engine's own.
//
//  2. FOLDS CHUNKS UP TO ARTIFACTS. A relevant file matches on several of
//     its chunks, and a person searching their Library wants the file
//     once, at its best chunk's score -- not five rows of the same PDF.
//     The chunk carries artifactId precisely so this fold needs no join.
//
//  3. ACCEPTS EITHER A QUERY OR A NEIGHBOUR. `text` is the search box;
//     `artifactId` is "more like this one", which resolves the named
//     artifact's own words (its summary, else its first chunk) and uses
//     them as the query -- with the seed artifact excluded from its own
//     results, since "this file is like itself" is noise.
//
// THE OVER-FETCH IS DELIBERATE AND IS THE ONE APPROXIMATION HERE. Because
// similarTo cannot be told whose rows to consider, its top-K is drawn
// from EVERY user's chunks; asking it for `limit` rows and then filtering
// would let a busy neighbour's files crowd the caller out of their own
// search. So the candidate pool is widened well past the answer size and
// filtered down. That bounds the failure rather than eliminating it: a
// caller whose best hit sits below the pool's cut-off still misses it.
// Fixing it properly needs an owner-aware vector read, which is a change
// to integrations/similarity, not to this file.

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	langparser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/memql"
)

const (
	// defaultSimilarLimit is what a caller who names no limit gets --
	// the same default integrations/similarity uses for similarTo.
	defaultSimilarLimit = 5

	// maxSimilarLimit caps what a caller may ask for. The fold means each
	// returned artifact costs one extra owner-gated read, so an unbounded
	// limit is an unbounded query fan-out on a user-facing path.
	maxSimilarLimit = 50

	// similarCandidateFactor / MinCandidates / MaxCandidates size the
	// over-fetch described in the file header. The factor is generous
	// because two independent effects eat the pool: other users' chunks,
	// and the caller's OWN chunks folding many-to-one onto few artifacts.
	similarCandidateFactor = 20
	similarMinCandidates   = 100
	similarMaxCandidates   = 500

	// snippetLimit trims the matched chunk text carried back with a hit.
	// A chunk is ~1800 characters; the portal renders a preview line, and
	// an agent tool result should not spend a page of context per hit.
	snippetLimit = 320
)

// similarArtifactHit is one folded result: an artifact, the best score
// any of its chunks scored, and enough of that chunk to show why.
type similarArtifactHit struct {
	ArtifactId string   `json:"artifactId"`
	FileId     string   `json:"fileId"`
	Score      float64  `json:"score"`
	Seq        int      `json:"seq"`
	Snippet    string   `json:"snippet"`
	Title      string   `json:"title"`
	Kind       string   `json:"kind"`
	Summary    string   `json:"summary"`
	Labels     []string `json:"labels"`
}

// handleSimilarArtifacts backs integration.library.similarArtifacts --
// the librarySimilarArtifacts builtin (portal, through the generated SDK)
// and the artifactSearch agent tool.
func (i *Integration) handleSimilarArtifacts(ctx context.Context, args map[string]any, target int) ([]memorynodes.MemoryNode, error) {
	if i == nil || i.engine == nil {
		return nil, fmt.Errorf("library.similarArtifacts: integration not initialized")
	}
	actorUserId := actingUserId(ctx)
	if actorUserId == "" {
		// Every gate below is "is this row the caller's?", so an
		// unattributed call cannot be answered safely -- and answering it
		// as "nobody's rows" would silently return an empty result that
		// reads like "you have nothing similar".
		return nil, fmt.Errorf("library.similarArtifacts: no acting user on the request; refusing to search unattributed")
	}

	limit := resolveLimit(args["limit"], target)
	seedArtifactId := strings.TrimSpace(asString(args["artifactId"]))
	queryText := strings.TrimSpace(asString(args["text"]))
	if queryText == "" && seedArtifactId != "" {
		derived, err := i.seedTextForArtifact(ctx, seedArtifactId)
		if err != nil {
			return nil, fmt.Errorf("library.similarArtifacts: %w", err)
		}
		queryText = derived
	}
	if queryText == "" {
		return nil, fmt.Errorf("library.similarArtifacts: pass 'text' to search by, or 'artifactId' to find artifacts like an existing one")
	}

	candidates, err := i.similarChunks(ctx, queryText, candidateCount(limit))
	if err != nil {
		return nil, fmt.Errorf("library.similarArtifacts: %w", err)
	}

	best := foldChunksToArtifacts(candidates, actorUserId, seedArtifactId)
	sortHitsByScore(best)

	out := make([]memorynodes.MemoryNode, 0, limit)
	for _, hit := range best {
		if len(out) >= limit {
			break
		}
		// The owner-gated re-read. A hit whose artifact does not resolve
		// under the caller's own actor is dropped rather than returned
		// without its title -- the payload compare above is a filter, this
		// is the engine's own admission decision.
		row, ok := i.artifactUnderActor(ctx, hit.ArtifactId)
		if !ok {
			continue
		}
		hit.Title = stringField(row, "title")
		hit.Kind = stringField(row, "kind")
		hit.Summary = stringField(row, "summary")
		hit.Labels = stringSliceField(row, "labels")
		node, err := hitNode(hit)
		if err != nil {
			return nil, err
		}
		out = append(out, node)
	}
	return out, nil
}

// resolveLimit reads the caller's limit, tolerating every numeric shape
// the DSL arg parser and a JSON round-trip produce, and clamps it.
// `target` is the engine's own result-count hint (the `N` in a targeted
// call); it is used only when the caller named no limit at all.
func resolveLimit(raw any, target int) int {
	limit, ok := intArg(raw)
	if !ok || limit <= 0 {
		if target > 0 {
			limit = target
		} else {
			limit = defaultSimilarLimit
		}
	}
	if limit > maxSimilarLimit {
		limit = maxSimilarLimit
	}
	return limit
}

// candidateCount sizes the over-fetch pool (see the file header).
func candidateCount(limit int) int {
	n := limit * similarCandidateFactor
	if n < similarMinCandidates {
		n = similarMinCandidates
	}
	if n > similarMaxCandidates {
		n = similarMaxCandidates
	}
	return n
}

// similarChunks runs the shared vector operator over the Library's chunk
// concept. Reached through the DSL builtin rather than a second pgvector
// statement of our own: one retrieval path, and the staged-data gate
// integrations/similarity applies stays in front of it.
func (i *Integration) similarChunks(ctx context.Context, text string, limit int) ([]map[string]any, error) {
	// `builtin <name>(...)` -- the builtin invocation form (see
	// embedChunk in analysis.go for why the two obvious spellings fail).
	q := fmt.Sprintf(`builtin similarTo(text: %s, concept: %s, limit: %d)`,
		langparser.QuoteString(text),
		langparser.QuoteString(conceptFileChunk),
		limit,
	)
	raw, err := i.engine.Execute(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("similarity search: %w", err)
	}
	return extractRows(raw), nil
}

// foldChunksToArtifacts is the whole of the scoping + fold, kept as a
// pure function so a test can state the property directly: chunks
// belonging to anyone but actorUserId never appear, and each artifact
// appears once at its best chunk's score.
func foldChunksToArtifacts(chunks []map[string]any, actorUserId, excludeArtifactId string) []similarArtifactHit {
	byArtifact := map[string]similarArtifactHit{}
	for _, chunk := range chunks {
		if stringField(chunk, "ownerUserId") != actorUserId {
			continue
		}
		artifactId := stringField(chunk, "artifactId")
		if artifactId == "" || artifactId == excludeArtifactId {
			continue
		}
		score := floatField(chunk, "_similarity")
		if existing, ok := byArtifact[artifactId]; ok && existing.Score >= score {
			continue
		}
		byArtifact[artifactId] = similarArtifactHit{
			ArtifactId: artifactId,
			FileId:     stringField(chunk, "fileId"),
			Score:      score,
			Seq:        intField(chunk, "seq"),
			Snippet:    snippet(stringField(chunk, "text")),
		}
	}
	out := make([]similarArtifactHit, 0, len(byArtifact))
	for _, hit := range byArtifact {
		out = append(out, hit)
	}
	return out
}

// sortHitsByScore orders best-first. Ties break on artifactId so the
// order is total and a test can assert on it -- Go's map iteration is
// randomised, and a partial order over hits would make the first result
// non-deterministic exactly when two files are equally relevant.
func sortHitsByScore(hits []similarArtifactHit) {
	sort.Slice(hits, func(a, b int) bool {
		if hits[a].Score != hits[b].Score {
			return hits[a].Score > hits[b].Score
		}
		return hits[a].ArtifactId < hits[b].ArtifactId
	})
}

func snippet(text string) string {
	runes := []rune(strings.TrimSpace(text))
	if len(runes) <= snippetLimit {
		return string(runes)
	}
	return strings.TrimSpace(string(runes[:snippetLimit])) + "..."
}

// seedTextForArtifact resolves the words to search BY when the caller
// named a neighbour rather than typing a query. Prefers the artifact's
// summary (a sentence about the whole file) and falls back to its first
// chunk (the file's opening). Both reads are owner-gated, so "more like
// this" over someone else's artifact resolves to nothing and is refused
// by name rather than quietly searching for an empty string.
func (i *Integration) seedTextForArtifact(ctx context.Context, artifactId string) (string, error) {
	row, ok := i.artifactUnderActor(ctx, artifactId)
	if !ok {
		return "", fmt.Errorf("artifact %q not found", artifactId)
	}
	if summary := strings.TrimSpace(stringField(row, "summary")); summary != "" {
		return summary, nil
	}
	sourceRef := stringField(row, "sourceConceptRef")
	if !strings.HasPrefix(sourceRef, conceptFile+":") {
		return "", fmt.Errorf("artifact %q has no readable text to search by", artifactId)
	}
	chunks, err := i.fileChunks(ctx, memql.BareShortId(sourceRef))
	if err != nil {
		return "", fmt.Errorf("read chunks of artifact %q: %w", artifactId, err)
	}
	for _, chunk := range chunks {
		if text := strings.TrimSpace(stringField(chunk, "text")); text != "" {
			return text, nil
		}
	}
	return "", fmt.Errorf("artifact %q has not been indexed for search yet", artifactId)
}

// artifactUnderActor reads one artifact index row under the CALLER'S own
// actor -- libraryArtifactById is gated on ownerUserId == actor.userId,
// so a row belonging to anyone else simply does not come back. Distinct
// from loadArtifactUnderOwner (library.go), which reads under a system
// actor deliberately because the label capabilities act on the row's own
// owner; here the caller is the authority and must stay so.
func (i *Integration) artifactUnderActor(ctx context.Context, artifactId string) (map[string]any, bool) {
	q := fmt.Sprintf(`query libraryArtifactById(artifactId: %s)`, langparser.QuoteString(artifactId))
	raw, err := i.engine.Execute(ctx, q)
	if err != nil {
		return nil, false
	}
	rows := extractRows(raw)
	if len(rows) == 0 {
		return nil, false
	}
	return rows[0], true
}

// actingUserId reads the acting user off the ACCESS context -- the same
// surface the engine's own resolveActorReference reads for `actor.userId`,
// so a capability's idea of who is calling cannot drift from the filter's.
func actingUserId(ctx context.Context) string {
	ac, ok := auth.AccessFromContext(ctx)
	if !ok || ac == nil {
		return ""
	}
	return strings.TrimSpace(ac.UserId)
}

// floatField reads a numeric payload field, tolerating the shapes a
// JSON/structpb round-trip produces. `_similarity` arrives as float64
// through protobuf, and as any numeric kind from an in-process map.
func floatField(m map[string]any, key string) float64 {
	switch v := m[key].(type) {
	case float64:
		return v
	case float32:
		return float64(v)
	case int:
		return float64(v)
	case int64:
		return float64(v)
	case json.Number:
		f, err := v.Float64()
		if err != nil {
			return 0
		}
		return f
	}
	return 0
}

func hitNode(hit similarArtifactHit) (memorynodes.MemoryNode, error) {
	payload, err := json.Marshal(hit)
	if err != nil {
		return memorynodes.MemoryNode{}, fmt.Errorf("library.similarArtifacts: marshal hit: %w", err)
	}
	return memorynodes.MemoryNode{
		ID:        fmt.Sprintf("library:similar:%s:%d", hit.ArtifactId, time.Now().UnixNano()),
		Concept:   resultConcept,
		Type:      memorynodes.NodeTypeObject,
		CreatedAt: time.Now().UTC(),
		Payload:   payload,
	}, nil
}
