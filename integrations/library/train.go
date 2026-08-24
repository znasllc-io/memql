package library

// train.go -- libraryTrainFile (memql#4342, design D7 + 3.6).
//
// Upload and train are TWO ACTS. A file in the Library is the owner's the
// moment it lands, and nothing is trained by default; this is the second,
// deliberate act, and the record of it is a domain id appended to the
// file's own trainedIntoDomainIds. That list is the whole audit trail a
// person can see -- the auditEvent row below is the one an operator can.
//
// What it does: reconstructs the file's extracted text from the chunks
// the analysis pass wrote, hands it to integration.knowledge.ingest with
// `sourceRef: artifact:<artifactId>` so every knowledge chunk points back
// at the Library row it came from, and records the domain on the file.
//
// KNOWLEDGE CONCEPTS STAY WHERE THEY ARE (design D2). Training is a CALL
// INTO the knowledge integration, not a coupling of schemas: nothing here
// declares, reads or writes a knowledge-domain row, and v1:library:file
// carries the domain ids as a plain []string rather than a relationship
// precisely because the target concept is product-owned.
//
// WHICH IS ALSO WHY THE DOMAIN CHECK IS A SEAM. "The domain must be one
// the caller may write to" (3.6) is a question about a row this engine
// cannot see: v1:knowledge:knowledgeDomain is declared in NO .memql file
// in this tree, so there is no query to ask and no per-row authz tier to
// lean on. Rather than invent an answer, the decision is delegated to a
// DomainWriteAuthorizer the product wires, and the default with none
// wired is REFUSE -- with the one exception of a cluster owner, who is
// the operator by definition and is what keeps the capability
// exercisable on a fresh cluster.
//
// Deny-by-default is the correct direction here and the alternative is
// worse than it looks: integration.knowledge.ingest itself performs no
// authorization at all, so a permissive default would make this the
// shortest path in the product to writing into anyone's corpus, wearing
// the name of a feature that says it checks.

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	langparser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/core/id"
)

// knowledgeIngestSource is the provenance class every chunk this path
// writes is tagged with. `fileUpload` is the existing member of
// v1:knowledge:documentChunk.source's enum that means exactly this --
// user-uploaded document content -- and the citation registry and the
// dev-refresh cache filter both read it.
const knowledgeIngestSource = "fileUpload"

// minChunkSeamMatch is the shortest overlap joinChunkTexts will believe.
// Chunks overlap by ~180 characters, so a real seam is long; a handful of
// characters matching is coincidence (every English chunk pair "overlaps"
// on " the"), and acting on it would DELETE text from the reconstruction.
const minChunkSeamMatch = 24

// DomainWriteAuthorizer answers the one question this engine cannot:
// may this user write into this knowledge domain?
//
// The knowledge-domain concept is product-owned (design D2), so the
// product is the only party that can decide. A node with no authorizer
// wired refuses every non-operator train rather than guessing.
type DomainWriteAuthorizer interface {
	MayWriteKnowledgeDomain(ctx context.Context, userId, domainId string) (bool, error)
}

// DomainWriteAuthorizerFunc adapts a plain function to the interface.
type DomainWriteAuthorizerFunc func(ctx context.Context, userId, domainId string) (bool, error)

// MayWriteKnowledgeDomain implements DomainWriteAuthorizer.
func (f DomainWriteAuthorizerFunc) MayWriteKnowledgeDomain(ctx context.Context, userId, domainId string) (bool, error) {
	return f(ctx, userId, domainId)
}

// SetDomainWriteAuthorizer wires the product's domain-write decision.
func (i *Integration) SetDomainWriteAuthorizer(a DomainWriteAuthorizer) { i.domainAuthorizer = a }

// trainResult is what the capability returns to the portal / the agent.
type trainResult struct {
	FileId               string   `json:"fileId"`
	ArtifactId           string   `json:"artifactId"`
	DomainId             string   `json:"domainId"`
	SourceRef            string   `json:"sourceRef"`
	Characters           int      `json:"characters"`
	AlreadyTrained       bool     `json:"alreadyTrained"`
	TrainedIntoDomainIds []string `json:"trainedIntoDomainIds"`
}

// handleTrainFile backs integration.library.trainFile -- the
// libraryTrainFile builtin (portal) and the artifactTrain agent tool.
func (i *Integration) handleTrainFile(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	if i == nil || i.engine == nil {
		return nil, fmt.Errorf("library.trainFile: integration not initialized")
	}
	fileId := strings.TrimSpace(asString(args["fileId"]))
	if fileId == "" {
		return nil, fmt.Errorf("library.trainFile: 'fileId' is required")
	}
	domainId := strings.TrimSpace(asString(args["domainId"]))
	if domainId == "" {
		return nil, fmt.Errorf("library.trainFile: 'domainId' is required")
	}

	access, _ := auth.AccessFromContext(ctx)
	actorUserId := actingUserId(ctx)
	if actorUserId == "" {
		return nil, fmt.Errorf("library.trainFile: no acting user on the request; refusing to train unattributed")
	}

	// The file gate is the read itself: libraryFileById filters on
	// ownerUserId == actor.userId, so someone else's file does not come
	// back and "not yours" is indistinguishable from "does not exist" --
	// the attachment-route precedent, and the right shape for an id a
	// caller can guess.
	file, err := i.loadFile(ctx, fileId)
	if err != nil {
		return nil, fmt.Errorf("library.trainFile: load file %q: %w", fileId, err)
	}
	if file == nil {
		return nil, fmt.Errorf("library.trainFile: file %q not found", fileId)
	}

	if err := i.assertMayWriteDomain(ctx, access, actorUserId, domainId); err != nil {
		return nil, err
	}

	artifact, ok := i.artifactForFile(ctx, fileId)
	if !ok {
		return nil, fmt.Errorf("library.trainFile: file %q has no Library index row yet; it cannot be attributed to an artifact", fileId)
	}
	artifactId := stringField(artifact, "id")

	chunks, err := i.fileChunks(ctx, fileId)
	if err != nil {
		return nil, fmt.Errorf("library.trainFile: read chunks of file %q: %w", fileId, err)
	}
	text := joinChunkTexts(chunks)
	if strings.TrimSpace(text) == "" {
		return nil, fmt.Errorf("library.trainFile: file %q has no extracted text to train on (status %q) -- only files the analysis pass could read can be trained",
			fileId, stringField(file, "status"))
	}

	sourceRef := artifactSourceRef(artifactId)
	if err := i.ingestIntoDomain(ctx, domainId, sourceRef, text); err != nil {
		return nil, fmt.Errorf("library.trainFile: %w", err)
	}

	// Read-then-merge, the label-builtin shape: appendLibraryFileTrainedDomain
	// takes the FULL list because MemQL has no array append, so the merge
	// has to happen here. Idempotent -- training into the same domain
	// twice re-ingests (the knowledge chunk ids are content-derived, so
	// that versions rather than duplicates) and writes no new domain id.
	current := stringSliceField(file, "trainedIntoDomainIds")
	merged, changed := mergeDomainAdd(current, domainId)
	if changed {
		if err := i.appendTrainedDomain(ctx, fileId, merged); err != nil {
			return nil, fmt.Errorf("library.trainFile: record the domain on the file: %w", err)
		}
	}

	i.auditTrain(ctx, access, actorUserId, fileId, artifactId, domainId, len(text))

	return wrapTrainResult(trainResult{
		FileId:               fileId,
		ArtifactId:           artifactId,
		DomainId:             domainId,
		SourceRef:            sourceRef,
		Characters:           len(text),
		AlreadyTrained:       !changed,
		TrainedIntoDomainIds: merged,
	})
}

// assertMayWriteDomain applies the gate described in the file header:
// the operator always may, the product's authorizer decides for everyone
// else, and no authorizer means no.
//
// The refusal names the seam rather than the user's mistake, because in
// the no-authorizer case the caller did nothing wrong -- the cluster is
// not wired to answer.
func (i *Integration) assertMayWriteDomain(ctx context.Context, access *auth.AccessContext, actorUserId, domainId string) error {
	if access.IsClusterOwner() {
		return nil
	}
	if i.domainAuthorizer == nil {
		return fmt.Errorf("library.trainFile: this cluster has no knowledge-domain authorizer wired, so it cannot confirm you may write to %q; refusing rather than training into a domain nobody vouched for", domainId)
	}
	ok, err := i.domainAuthorizer.MayWriteKnowledgeDomain(ctx, actorUserId, domainId)
	if err != nil {
		return fmt.Errorf("library.trainFile: could not check write access to knowledge domain %q: %w", domainId, err)
	}
	if !ok {
		return fmt.Errorf("library.trainFile: you do not have write access to knowledge domain %q", domainId)
	}
	return nil
}

// ingestIntoDomain calls integration.knowledge.ingest through its shipped
// builtin. chunkSize / overlap are passed explicitly at the same values
// the Library used, so the knowledge corpus is split the same way the
// search index was rather than at whatever the ingest default happens to
// be on the day.
func (i *Integration) ingestIntoDomain(ctx context.Context, domainId, sourceRef, text string) error {
	// `builtin <name>(...)` -- the builtin invocation form (see embedChunk
	// in analysis.go for why the two obvious spellings fail).
	q := fmt.Sprintf(
		"builtin knowledgeIngest(domainId: %s, text: %s, source: %s, sourceRef: %s, chunkSize: %d, overlap: %d)",
		langparser.QuoteString(domainId),
		langparser.QuoteString(text),
		langparser.QuoteString(knowledgeIngestSource),
		langparser.QuoteString(sourceRef),
		analysisChunkSize,
		analysisChunkOverlap,
	)
	if _, err := i.engine.Execute(ctx, q); err != nil {
		return fmt.Errorf("ingest into knowledge domain %q: %w", domainId, err)
	}
	return nil
}

// appendTrainedDomain writes the merged list back.
func (i *Integration) appendTrainedDomain(ctx context.Context, fileId string, domains []string) error {
	q := fmt.Sprintf("mutation appendLibraryFileTrainedDomain(fileId: %s, trainedIntoDomainIds: %s)",
		langparser.QuoteString(fileId),
		quoteStringArray(domains),
	)
	_, err := i.engine.Execute(ctx, q)
	return err
}

// artifactSourceRef is the provenance tag every knowledge chunk this path
// writes carries: `artifact:<artifactId>` (design 3.6). Written from the
// bare artifact id so the tag reads the same wherever it is rendered.
func artifactSourceRef(artifactId string) string {
	return "artifact:" + artifactId
}

// mergeDomainAdd appends a domain id if absent. Compares trimmed, for
// mergeLabelAdd's reason: a value written whitespace-padded through some
// other path must still be recognised as the same domain rather than
// added a second time.
func mergeDomainAdd(domains []string, domainId string) (merged []string, changed bool) {
	for _, d := range domains {
		if strings.TrimSpace(d) == domainId {
			return domains, false
		}
	}
	return append(slices.Clone(domains), domainId), true
}

// joinChunkTexts reconstructs a file's extracted text from its stored
// chunks.
//
// The chunks OVERLAP by design (~180 characters, so a passage reads
// whole in whichever chunk retrieves it), which means a naive join would
// repeat every seam -- and knowledge.ingest re-chunks whatever it is
// given, so those repetitions would become duplicated knowledge chunks.
// So for each chunk after the first, the longest prefix that the text so
// far already ENDS with is dropped, and only the remainder is appended.
//
// The bias is deliberate and one-directional: when no seam is found the
// chunk is appended whole after a newline. A missed seam repeats a little
// text; a wrongly-guessed one would silently DELETE some. minChunkSeamMatch
// is what keeps a coincidental short match from being believed.
//
// (Reading the bytes back and re-extracting would be exact, but the
// blob-download seam is not on the Plugin SDK surface this integration
// receives -- and a file whose chunks are gone has nothing to train on
// either way.)
func joinChunkTexts(chunks []map[string]any) string {
	var b strings.Builder
	for _, chunk := range chunks {
		text := stringField(chunk, "text")
		if strings.TrimSpace(text) == "" {
			continue
		}
		if b.Len() == 0 {
			b.WriteString(text)
			continue
		}
		acc := b.String()
		if overlap := seamLength(acc, text); overlap > 0 {
			b.WriteString(text[overlap:])
			continue
		}
		b.WriteString("\n")
		b.WriteString(text)
	}
	return b.String()
}

// seamLength returns the length of the longest prefix of next that acc
// ends with, searching down from the chunker's overlap window and
// stopping at minChunkSeamMatch. Zero means "no seam I am willing to act
// on".
func seamLength(acc, next string) int {
	max := analysisChunkOverlap * 2
	if max > len(next) {
		max = len(next)
	}
	if max > len(acc) {
		max = len(acc)
	}
	for n := max; n >= minChunkSeamMatch; n-- {
		if strings.HasSuffix(acc, next[:n]) {
			return n
		}
	}
	return 0
}

// auditTrain appends the operator-visible record. Best-effort: the
// training already happened and is already recorded on the file, so a
// failed audit write is logged rather than rolled back into an error that
// would tell the caller their train failed when it did not.
//
// targetType is deliberately NOT set: its enum is the identity concepts'
// (user / session / identity / ...) and a Library file is none of them.
// The file and the domain travel in targetId + detail instead, which is
// what those fields are for.
func (i *Integration) auditTrain(ctx context.Context, access *auth.AccessContext, actorUserId, fileId, artifactId, domainId string, characters int) {
	eventId := string(id.New().MustFromMap(map[string]any{
		"kind":     "libraryTrainFile",
		"fileId":   fileId,
		"domainId": domainId,
		"at":       time.Now().UTC().Format(time.RFC3339Nano),
	}))
	actorEmail, actorRole := "", ""
	if access != nil {
		actorEmail = access.PrimaryEmail
		actorRole = string(access.Role)
	}
	q := fmt.Sprintf(
		`mutation createAuditEvent(eventId: %s, occurredAt: %s, category: "data", action: "library_file_trained", actorUserId: %s, actorEmail: %s, actorRole: %s, targetId: %s, outcome: "success", detail: {fileId: %s, artifactId: %s, domainId: %s, characters: %d})`,
		langparser.QuoteString(eventId),
		langparser.QuoteString(time.Now().UTC().Format(time.RFC3339)),
		langparser.QuoteString(actorUserId),
		langparser.QuoteString(actorEmail),
		langparser.QuoteString(actorRole),
		langparser.QuoteString(fileId),
		langparser.QuoteString(fileId),
		langparser.QuoteString(artifactId),
		langparser.QuoteString(domainId),
		characters,
	)
	if _, err := i.engine.Execute(ctx, q); err != nil {
		i.log().Warn("library: train audit event not written",
			"fileId", fileId, "domainId", domainId, "error", err)
	}
}

func wrapTrainResult(r trainResult) ([]memorynodes.MemoryNode, error) {
	payload, err := json.Marshal(r)
	if err != nil {
		return nil, fmt.Errorf("library: marshal train result: %w", err)
	}
	return []memorynodes.MemoryNode{{
		ID:        fmt.Sprintf("library:train:%s:%d", r.FileId, time.Now().UnixNano()),
		Concept:   resultConcept,
		Type:      memorynodes.NodeTypeObject,
		CreatedAt: time.Now().UTC(),
		Payload:   payload,
	}}, nil
}
