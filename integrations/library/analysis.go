package library

// analysis.go -- the Library file analysis pass (memql#4342, design 3.4).
//
// One upload, one pass. When bytes land through POST /artifacts the
// handler answers 201 immediately and hands the file here; everything
// below then runs DETACHED, on a background context, exactly the shape
// component/server/attachment_handler.go's runAnalysisAsync established:
// HTTP cancellation must not abort analysis, because the row it is
// updating outlives the request.
//
// What the pass does, in order:
//
//	stored -> analyzing -> (extract -> summarize -> chunk -> embed) -> ready
//	                    \-> failed, with the reason ON THE ROW
//
// and for a type nothing can read: stored -> ready, no chunks, no
// summary. That is a terminal SUCCESS, not a degraded failure -- design
// 3.4 stores any MIME type and says so ("unknown types are stored
// opaquely with status: ready and no chunks").
//
// THREE THINGS HERE ARE LOAD-BEARING AND EASY TO GET WRONG.
//
//  1. The pass runs under the FILE OWNER'S ACTOR, never a system actor.
//     Every mutation it calls is owner-acted (dsl/library/mutations.memql
//     stamps ownerUserId from actor.userId), so a system actor would
//     write chunks owned by nobody -- and ownerUserId is the per-row
//     authz key on v1:library:fileChunk, so "owned by nobody" means
//     "readable by nobody" on the similarity path. auth.ContextWithUserActor
//     is the same borrow the campaigns drain worker takes for a campaign
//     owner.
//
//  2. Promotion happens EXACTLY ONCE, so the index must be re-stamped
//     here. indexFileOnCreate carries @filter(payload.status == "stored")
//     -- deliberately, so a second promotion cannot wipe the artifact's
//     labels -- which means every status transition this pass writes is
//     INVISIBLE to the index. The summary would never reach the Library
//     list, so the pass re-versions the artifact row itself, carrying
//     labels + archived forward through currentArtifactCarryForward the
//     way touchArtifact does. createArtifact's body is a bare insert{}:
//     a field this call omits is ABSENT from the new version, not
//     inherited (the memql#4288 hazard).
//
//  3. Embedding rides the ONE existing write-path. The chunk vector goes
//     out through integration.embedding.store (reached from the DSL as
//     libraryEmbedChunk), not through a second hand-rolled INSERT into
//     node_vectors. The engine's own harness/action embed hooks dispatch
//     the same capability for the same reason.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"time"

	langparser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/workjournal"
	"github.com/znasllc-io/memql/core/id"
	"github.com/znasllc-io/memql/integrations/knowledge"
)

const (
	// conceptFile / conceptFileChunk are the canonical concept ids of the
	// two rows this pass writes. Used to compose the canonical node ids
	// the artifact index (sourceConceptRef) and node_vectors (the vector's
	// row key) are keyed by -- both of which live INSIDE the process
	// boundary, where ids are canonical `{concept}:{shortId}` and never
	// bare (component/memql/wire_bareids.go: bare-ification happens at the
	// gRPC + tool-loop seams only).
	conceptFile      = "v1:library:file"
	conceptFileChunk = "v1:library:fileChunk"

	// analysisChunkSize / analysisChunkOverlap are the knowledge splitter's
	// defaults, restated here as the values this pass PASSES rather than
	// relied on implicitly: design 3.2 pins Library chunking to them
	// ("about 1800 characters, 180 overlap"), and knowledge.Chunk applies
	// its own defaults only for a non-positive argument.
	analysisChunkSize    = 1800
	analysisChunkOverlap = 180

	// summaryInputLimit caps what the summariser sees. docSummary asks for
	// 2-3 sentences, and a 40 MB text file would otherwise be sent whole to
	// a chat provider on every upload -- the cost bound design 3.4 asks for
	// ("embedding cost is bounded by file size") applies to the summary
	// call too, which is the ONE unbounded-by-construction call in the pass.
	summaryInputLimit = 12000
)

// TextExtractor is the narrow half of component/fileprocessor.Processor
// this pass needs: bytes plus a MIME type in, plain text out. Declared as
// a local interface rather than importing the processor type so a test
// can supply three lines of fixture, and so integrations/library does not
// take a hard dependency on which extractor a node happens to wire.
type TextExtractor interface {
	Extract(ctx context.Context, mimeType string, data []byte) (string, error)
}

// AnalyzeFileParams is what the upload route hands over. Data is the
// bytes as uploaded -- the pass reads them from memory rather than
// re-fetching the blob, because the handler already holds them and a
// re-download would make analysis depend on storage being reachable a
// second time.
type AnalyzeFileParams struct {
	// FileId is the v1:library:file row id -- the value the handler
	// passed to createLibraryFile. Bare or canonical, both resolve.
	FileId string
	// ArtifactId is the index row indexFileOnCreate promoted the file
	// into, when the caller already resolved it. OPTIONAL: an empty value
	// makes the pass wait for the promotion itself, which is the case the
	// bounded poll in awaitArtifactId exists for. Supplying it when known
	// removes that wait -- and with it the only race on this path.
	ArtifactId string
	// OwnerUserId is the v1:identity:user.id the upload ran as. The
	// whole pass borrows this identity; a blank value is refused rather
	// than silently attributing the writes to nobody.
	OwnerUserId string
	// Name is the (sanitised) original filename. Used as the summary
	// prompt's title, nothing else.
	Name string
	// MimeType is the type as uploaded / sniffed. Decides whether this
	// file has a known reader at all.
	MimeType string
	// Data is the uploaded bytes -- present on the one-shot path, NIL on
	// the chunked path (memql#4782), where no handler ever held the file
	// and the pass streams the committed blob instead.
	Data []byte
	// BlobUrl is where the committed bytes live, for the chunked case
	// above. Ignored when Data is present and the hash is known.
	BlobUrl string
	// Sha256 is the hash as the upload route computed it, or "" when
	// nobody has measured it yet -- the chunked case, which is the pass's
	// cue to stream the blob once and stamp what it measures (design D10).
	Sha256 string
}

// BlobFetcher opens the stored blob as a stream -- the one capability the
// pass needs for chunked files (hashing always; extraction when the type
// is readable). *azureblob.AzureBlobUploader implements it.
type BlobFetcher interface {
	DownloadStreamURL(ctx context.Context, blobURL string) (io.ReadCloser, error)
}

// SetBlobFetcher wires the blob stream. Optional, like the extractor: with
// none configured a chunked file keeps an absent sha256 -- "not measured"
// -- rather than failing, because a missing fetcher is an operator
// condition and not a property of the file.
func (i *Integration) SetBlobFetcher(f BlobFetcher) { i.blobFetcher = f }

// analysisMaxFetchBytes bounds how much of a fetched blob is held for
// EXTRACTION. The hash has no bound -- it streams -- but extraction needs
// the bytes in memory, and a blob past this size is treated as opaque
// (ready, no chunks) rather than silently truncated into a wrong summary.
const analysisMaxFetchBytes int64 = 100 << 20

// SetExtractor wires the text extractor. A node with none configured
// treats every type as unreadable (status ready, no chunks) rather than
// failing uploads -- the bytes are durable either way, and a missing
// extractor is an operator condition, not a property of the file.
func (i *Integration) SetExtractor(e TextExtractor) { i.extractor = e }

// SetLogger wires the pass's logger. Optional: the pass falls back to
// slog.Default().
func (i *Integration) SetLogger(l *slog.Logger) { i.logger = l }

// SetArtifactPoll overrides how long the pass waits for indexFileOnCreate
// to promote the file before giving up on writing chunks. Exists for
// tests, which must not sleep.
func (i *Integration) SetArtifactPoll(attempts int, interval time.Duration) {
	i.artifactPollAttempts = attempts
	i.artifactPollInterval = interval
}

// SetWorkJournal wires the work spine's journal (spec section G). OPTIONAL:
// a nil journal is the same shape as no journal, so a node without one runs
// the pass exactly as it did before the spine existed.
//
// The journal is what makes this pass VISIBLE. Before it, the only account
// of an analysis was the file row's own `status` flickering
// stored -> analyzing -> ready, which says that something happened and
// nothing about what: which stage cost the time, whether the summariser was
// reached, how many chunks embedded. Those are steps of a run, and the
// Training app reads them as one.
func (i *Integration) SetWorkJournal(j *workjournal.Journal) {
	if i == nil {
		return
	}
	i.journal = j
}

func (i *Integration) log() *slog.Logger {
	if i.logger != nil {
		return i.logger
	}
	return slog.Default()
}

// StartFileAnalysis dispatches AnalyzeFile on a detached goroutine and
// returns immediately. THE ENTRY POINT THE UPLOAD ROUTE CALLS: the 201
// goes out first, the row catches up.
//
// context.Background(), not the request context, for runAnalysisAsync's
// reason -- the client hanging up mid-upload must not leave a file parked
// at `analyzing` forever. Errors are logged and land on the row as
// status=failed + failureReason; nothing bubbles back, because by the
// time the pass runs there is no longer a response to put it in.
func (i *Integration) StartFileAnalysis(params AnalyzeFileParams) {
	go func() {
		if err := i.AnalyzeFile(context.Background(), params); err != nil {
			i.log().Warn("library: file analysis failed",
				"fileId", params.FileId, "mimeType", params.MimeType, "error", err)
		}
	}()
}

// AnalyzeFile runs the pass synchronously. Exported alongside
// StartFileAnalysis so a caller that wants to wait (a test, a
// re-analysis command) can, and so the detached wrapper stays three lines.
//
// The returned error is the pass's own bookkeeping failure -- it does NOT
// mean the file was left in an unknown state. Every failure path this
// function takes writes status=failed with a reason a person can act on
// FIRST; the error is what the caller logs, the row is what the owner
// sees.
func (i *Integration) AnalyzeFile(ctx context.Context, params AnalyzeFileParams) (err error) {
	if i == nil || i.engine == nil {
		return fmt.Errorf("library.analyzeFile: integration not initialized")
	}
	fileId := strings.TrimSpace(params.FileId)
	if fileId == "" {
		return fmt.Errorf("library.analyzeFile: fileId is required")
	}
	owner := strings.TrimSpace(params.OwnerUserId)
	if owner == "" {
		// withUserActor passes the context through UNCHANGED for a blank
		// owner, so proceeding would attribute every chunk to whatever
		// actor happened to be on the background context (none), and the
		// rows would be unreadable by their own owner. Refuse loudly.
		return fmt.Errorf("library.analyzeFile: ownerUserId is required -- the pass runs as the file's owner")
	}
	ctx = withUserActor(ctx, owner)

	// --- the pass as a system-origin goal and run (spec section G) ---
	//
	// Opened here, after the owner is resolved and before anything is read,
	// so every later write has a home and the Training app's feed shows the
	// work from the moment it starts rather than when it finishes.
	//
	// THE STEPS ARE THREE, NOT FOUR. The design names the stages as
	// "extract, chunk, embed, summarize", and chunking and embedding are one
	// step here because the loop below interleaves them -- it writes a chunk
	// row and embeds that chunk before moving to the next. Recording them as
	// two sequential steps would assert an ordering this code does not have,
	// and a journal that says the chunk stage finished before the embed
	// stage began would be describing a different program.
	run, runErr := i.journal.Begin(ctx, workjournal.Work{
		OwnerUserID:  owner,
		Template:     AnalysisTemplate,
		Statement:    analysisStatement(params.Name),
		GoalKey:      fileId,
		Input:        analysisInput(params),
		RequestedVia: "library",
		Steps: []workjournal.StepDecl{
			{Key: "extract", Kind: workjournal.KindDeterministic},
			// REASONING, because it reaches the docSummary prompt. That is
			// the derived-kind rule of spec section B, and recording it
			// honestly is what lets a later reader ask which stages cost a
			// model call without re-deriving it from the code.
			{Key: "summarize", Kind: workjournal.KindReasoning},
			{Key: "index", Kind: workjournal.KindDeterministic},
		},
	})
	if runErr != nil {
		// The journal is a record of the work, not the work. An analysis
		// that cannot be journalled still runs.
		i.log().Warn("library: the analysis run could not be opened", "fileId", fileId, "error", runErr)
	}
	outcome := map[string]any{}
	defer func() {
		if err != nil {
			run.Failed(ctx, "analysis_failed", err.Error())
			return
		}
		run.Succeeded(ctx, outcome)
	}()

	// --- the chunked case: fetch what the handler never held (memql#4782) ---
	//
	// One stream serves both needs: the hash is fed by a TeeReader while
	// extraction's bytes accumulate under a bound, so the blob is read ONCE
	// whether or not the type is readable. A hash the upload route already
	// computed is never re-measured, and an opaque type with a known hash
	// never opens the stream at all.
	// blobBacked marks the CHUNKED shape (memql#4782): the caller held no
	// bytes and says where the committed blob lives. Legacy callers that
	// pass neither Data nor BlobUrl keep the original behaviour -- the
	// extractor decides, and its failure carries the reason.
	blobBacked := len(params.Data) == 0 && strings.TrimSpace(params.BlobUrl) != ""
	data := params.Data
	measuredHash := ""
	if blobBacked && (params.Sha256 == "" || i.canExtract(params.MimeType)) {
		if i.blobFetcher != nil {
			fetched, hash := i.fetchAndHash(ctx, params.BlobUrl, i.canExtract(params.MimeType))
			data = fetched
			if params.Sha256 == "" {
				measuredHash = hash
			}
		} else if params.Sha256 == "" {
			i.log().Warn("library: no blob fetcher wired; a chunked file keeps an absent sha256",
				"fileId", fileId)
		}
	}

	// An unreadable type is a terminal SUCCESS with no chunks (design
	// 3.4). Checked before the `analyzing` transition so an opaque upload
	// never flickers through a status that promises work nobody is doing.
	// The same path serves a blob-backed READABLE type whose bytes could
	// not be fetched or were too large to hold: the file is stored and
	// downloadable either way, and `ready` with no chunks is the honest
	// summary of that.
	if !i.canExtract(params.MimeType) || (blobBacked && len(data) == 0) {
		if err := i.setFileStatus(ctx, fileId, fileStatusUpdate{
			status:          "ready",
			embeddingStatus: "complete",
			sha256:          measuredHash,
		}); err != nil {
			return fmt.Errorf("library.analyzeFile: mark opaque file ready: %w", err)
		}
		i.restampArtifact(ctx, fileId)
		// SKIPPED, not omitted. A step the template declares and this run did
		// not need is written as skipped so the run's steps still add up to
		// its declared order -- a missing row and a skipped one look
		// identical to a reader, and only one of them is true.
		skipped := "this file type is stored and downloadable, and there is no text in it to read"
		run.Step(ctx, "extract").Skipped(ctx, skipped)
		run.Step(ctx, "summarize").Skipped(ctx, skipped)
		run.Step(ctx, "index").Skipped(ctx, skipped)
		outcome["readable"] = false
		outcome["chunks"] = 0
		return nil
	}

	if err := i.setFileStatus(ctx, fileId, fileStatusUpdate{status: "analyzing", sha256: measuredHash}); err != nil {
		return fmt.Errorf("library.analyzeFile: mark analyzing: %w", err)
	}

	extractStep := run.Step(ctx, "extract")
	text, extractErr := i.extractor.Extract(ctx, params.MimeType, data)
	if extractErr != nil {
		extractStep.Failed(ctx, "extract_failed", extractErr.Error())
		return i.failFile(ctx, fileId, fmt.Sprintf(
			"could not read the contents of this %s file: %v", params.MimeType, extractErr))
	}
	if strings.TrimSpace(text) == "" {
		extractStep.Failed(ctx, "no_text", "the file yielded no text")
		// A type we CAN read that yielded nothing is the
		// password-protected-PDF / image-only-scan case design 3.1 names
		// as the model failure ("could not extract text from a
		// password-protected PDF"). Reporting it as `ready` would tell the
		// owner their file is searchable when it is not.
		return i.failFile(ctx, fileId,
			"no text could be extracted from this file -- it may be image-only, empty or password-protected")
	}
	extractStep.Done(ctx, map[string]any{"characters": len(text)})

	// Best-effort, never fatal: a summariser outage must not cost the
	// owner their chunks. An empty summary is simply not written.
	//
	// The step records which of those happened. A summary that is absent
	// because no provider answered and one that is absent because the
	// document had nothing to say are the same empty string on the file row,
	// and only the step can tell them apart.
	summarizeStep := run.Step(ctx, "summarize")
	summary := i.summarize(ctx, params.Name, text)
	summarizeStep.Done(ctx, map[string]any{"summarized": summary != "", "characters": len(summary)})

	// The chunk rows carry artifactId so a similarity hit folds straight
	// up to the Library row. That id comes from the promotion the
	// indexFileOnCreate automation runs, which is ASYNCHRONOUS with
	// respect to this pass -- so when the caller has not already resolved
	// it, wait, bounded, rather than doing a single read that races on a
	// fast upload.
	indexStep := run.Step(ctx, "index")
	artifactId := strings.TrimSpace(params.ArtifactId)
	if artifactId == "" {
		resolved, ok := i.awaitArtifactId(ctx, fileId)
		if !ok {
			indexStep.Failed(ctx, "artifact_not_indexed", "the Library index row did not appear in time")
			return i.failFile(ctx, fileId,
				"this file was stored but has not appeared in the Library index yet, so it could not be indexed for search")
		}
		artifactId = resolved
	}

	chunks := knowledge.Chunk(text, analysisChunkSize, analysisChunkOverlap)
	embedded := 0
	for seq, chunkText := range chunks {
		chunkId := chunkIdFor(fileId, seq, chunkText)
		nodeId, err := i.writeChunk(ctx, chunkWrite{
			chunkId:    chunkId,
			fileId:     fileId,
			artifactId: artifactId,
			seq:        seq,
			text:       chunkText,
		})
		if err != nil {
			indexStep.Failed(ctx, "chunk_write_failed", fmt.Sprintf("chunk %d of %d failed to save", seq+1, len(chunks)))
			return i.failFile(ctx, fileId, fmt.Sprintf(
				"this file could not be indexed for search (chunk %d of %d failed to save)", seq+1, len(chunks)))
		}
		if err := i.embedChunk(ctx, nodeId, chunkText); err != nil {
			// A failed embed is PARTIAL, not failed: the chunk row is
			// durable and a later re-embed backfills it. The file is still
			// readable and downloadable; only "search by meaning" is
			// incomplete, and embeddingStatus is the field that says so.
			i.log().Warn("library: chunk embed failed; file will be partially searchable",
				"fileId", fileId, "chunkId", chunkId, "seq", seq, "error", err)
			continue
		}
		embedded++
	}

	indexStep.Done(ctx, map[string]any{"chunks": len(chunks), "embedded": embedded})

	if err := i.setFileStatus(ctx, fileId, fileStatusUpdate{
		status:          "ready",
		summary:         summary,
		embeddingStatus: embeddingStatusFor(len(chunks), embedded),
	}); err != nil {
		return fmt.Errorf("library.analyzeFile: mark ready: %w", err)
	}
	i.restampArtifact(ctx, fileId)
	outcome["readable"] = true
	outcome["chunks"] = len(chunks)
	outcome["embedded"] = embedded
	outcome["summarized"] = summary != ""
	outcome["artifactId"] = artifactId

	i.log().Info("library: file analysis complete",
		"fileId", fileId, "artifactId", artifactId,
		"chunks", len(chunks), "embedded", embedded, "summarized", summary != "")
	return nil
}

// AnalysisTemplate is the run's `automationName` -- the deterministic
// template this pass IS. It is exported because it is a CONTRACT with the
// Training app, which filters its feed by it: every run this pass writes
// carries this value and no other run does, so the app shows analyses
// without narrowing a query somebody else owns.
const AnalysisTemplate = "libraryAnalyzeFile"

// analysisStatement is the goal in a person's words. It names the file
// because that is what the goal is about, and it is the only place the name
// appears on the spine -- the run keys by id.
func analysisStatement(name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return "Read an uploaded file"
	}
	return "Read " + name
}

// analysisInput is the run's input envelope, and the key the Training app
// reads. `fileId` is always present; `artifactId` is present only when the
// caller already resolved it, because the index row is promoted
// asynchronously and an empty string is not an answer.
func analysisInput(params AnalyzeFileParams) map[string]any {
	in := map[string]any{"fileId": strings.TrimSpace(params.FileId)}
	if name := strings.TrimSpace(params.Name); name != "" {
		in["name"] = name
	}
	if mime := strings.TrimSpace(params.MimeType); mime != "" {
		in["mimeType"] = mime
	}
	if artifact := strings.TrimSpace(params.ArtifactId); artifact != "" {
		in["artifactId"] = artifact
	}
	return in
}

// fetchAndHash streams the stored blob ONCE: the whole stream feeds the
// sha256, and -- when wantBytes -- up to analysisMaxFetchBytes accumulate
// for extraction. A blob past the bound keeps hashing to the end but
// returns nil bytes, so the caller treats it as opaque rather than
// summarising a truncation. Any failure returns ("", nil): the hash is a
// fact or it is absent, never a partial.
func (i *Integration) fetchAndHash(ctx context.Context, blobURL string, wantBytes bool) ([]byte, string) {
	rc, err := i.blobFetcher.DownloadStreamURL(ctx, blobURL)
	if err != nil {
		i.log().Warn("library: blob fetch for analysis failed", "blobUrl", blobURL, "error", err)
		return nil, ""
	}
	defer func() { _ = rc.Close() }()

	h := sha256.New()
	if !wantBytes {
		if _, err := io.Copy(h, rc); err != nil {
			i.log().Warn("library: blob hash stream failed", "blobUrl", blobURL, "error", err)
			return nil, ""
		}
		return nil, hex.EncodeToString(h.Sum(nil))
	}

	buf, err := io.ReadAll(io.TeeReader(io.LimitReader(rc, analysisMaxFetchBytes+1), h))
	if err != nil {
		i.log().Warn("library: blob fetch stream failed", "blobUrl", blobURL, "error", err)
		return nil, ""
	}
	if int64(len(buf)) > analysisMaxFetchBytes {
		// Too large to extract; finish the hash over the remainder and
		// hand back no bytes.
		if _, err := io.Copy(h, rc); err != nil {
			i.log().Warn("library: blob hash tail failed", "blobUrl", blobURL, "error", err)
			return nil, ""
		}
		return nil, hex.EncodeToString(h.Sum(nil))
	}
	return buf, hex.EncodeToString(h.Sum(nil))
}

// canExtract reports whether this node can read this MIME type at all.
// A node with no extractor wired answers no for everything, which routes
// the upload down the opaque path (ready, no chunks) rather than failing
// it -- the file is stored and downloadable either way.
func (i *Integration) canExtract(mimeType string) bool {
	if i.extractor == nil {
		return false
	}
	return knownExtractableMIME(mimeType)
}

// knownExtractableMIME is the set of types the pass will attempt. It
// mirrors component/fileprocessor.SupportedMIMETypes rather than calling
// it, so integrations/library does not take a module dependency on the
// processor just to ask a question about a string; the sets are kept in
// step by TestKnownExtractableMIMEMatchesTheProcessor.
//
// Deliberately a PRE-check rather than "try it and see": the processor
// returns a plain error for an unsupported type, and treating that error
// as a failure would stamp `failed` on every opaque upload -- which is
// exactly the state design 3.4 says an unknown type must NOT reach.
func knownExtractableMIME(mimeType string) bool {
	switch normalizeMIME(mimeType) {
	case "application/pdf",
		"application/vnd.openxmlformats-officedocument.wordprocessingml.document",
		"text/plain",
		"text/markdown",
		"image/jpeg",
		"image/png",
		"image/gif",
		"image/webp":
		return true
	}
	return false
}

// normalizeMIME strips parameters and lowercases, matching how the
// processor itself normalises before dispatching.
func normalizeMIME(mimeType string) string {
	mimeType = strings.TrimSpace(strings.ToLower(mimeType))
	if idx := strings.IndexByte(mimeType, ';'); idx >= 0 {
		mimeType = strings.TrimSpace(mimeType[:idx])
	}
	return mimeType
}

// embeddingStatusFor maps (chunks written, chunks embedded) onto the
// concept's enum. `none` covers the no-chunks case; `complete` needs
// every chunk vectorised, because the field is read as "can search by
// meaning answer for this whole file".
func embeddingStatusFor(chunks, embedded int) string {
	switch {
	case chunks == 0 || embedded == 0:
		return "none"
	case embedded >= chunks:
		return "complete"
	default:
		return "partial"
	}
}

// summarize runs the shipped docSummary prompt over the extracted text.
// Best-effort by contract: every failure returns "" and the caller simply
// does not write the field. Never fatal -- an outage at the chat provider
// must not cost the owner a searchable file.
func (i *Integration) summarize(ctx context.Context, title, text string) string {
	content := text
	if len(content) > summaryInputLimit {
		content = content[:summaryInputLimit]
	}
	raw, err := i.engine.InvokeAI(ctx, "docSummary", map[string]any{
		"title":   title,
		"content": content,
	})
	if err != nil {
		i.log().Warn("library: file summary unavailable", "title", title, "error", err)
		return ""
	}
	return strings.TrimSpace(asSummaryText(raw))
}

// asSummaryText normalises InvokeAI's `any` return. Mirrors the decode
// integrations/cognition's ai_responder does: a prose prompt usually
// returns a string, but a provider that answered with an object should
// not silently become an empty summary.
func asSummaryText(raw any) string {
	switch v := raw.(type) {
	case string:
		return v
	case map[string]any:
		if s, ok := v["text"].(string); ok {
			return s
		}
		if s, ok := v["content"].(string); ok {
			return s
		}
	}
	return ""
}

// fileStatusUpdate names the optional halves of setLibraryFileStatus.
// Every field but status is OMITTED from the call when blank -- the
// mutation is a read-merge and an absent optional argument is not
// written, which is what lets `ready` land without blanking a summary or
// re-stating an embedding status the caller has no opinion on.
type fileStatusUpdate struct {
	status          string
	summary         string
	embeddingStatus string
	failureReason   string
	// sha256 is written only when THIS pass measured it (memql#4782, D10):
	// a chunked upload lands hash-absent, and the pass streams the
	// committed blob once to stamp it. Never re-stated for a hash the
	// upload route already computed.
	sha256 string
}

func (i *Integration) setFileStatus(ctx context.Context, fileId string, u fileStatusUpdate) error {
	parts := []string{
		fmt.Sprintf("fileId: %s", langparser.QuoteString(fileId)),
		fmt.Sprintf("status: %s", langparser.QuoteString(u.status)),
	}
	if u.summary != "" {
		parts = append(parts, fmt.Sprintf("summary: %s", langparser.QuoteString(u.summary)))
	}
	if u.embeddingStatus != "" {
		parts = append(parts, fmt.Sprintf("embeddingStatus: %s", langparser.QuoteString(u.embeddingStatus)))
	}
	if u.failureReason != "" {
		parts = append(parts, fmt.Sprintf("failureReason: %s", langparser.QuoteString(u.failureReason)))
	}
	if u.sha256 != "" {
		parts = append(parts, fmt.Sprintf("sha256: %s", langparser.QuoteString(u.sha256)))
	}
	_, err := i.engine.Execute(ctx, fmt.Sprintf("mutation setLibraryFileStatus(%s)", strings.Join(parts, ", ")))
	return err
}

// failFile is the single terminal-failure path: the reason lands ON THE
// ROW (design 3.1 -- "the person who uploaded the file is the one who
// needs to see it"), the index is re-stamped so the Library stops showing
// the file as freshly stored, and the caller gets an error to log.
//
// Never a silent partial: a pass that gives up says why, in the same
// write that says it gave up.
func (i *Integration) failFile(ctx context.Context, fileId, reason string) error {
	if err := i.setFileStatus(ctx, fileId, fileStatusUpdate{
		status:        "failed",
		failureReason: reason,
	}); err != nil {
		return fmt.Errorf("library.analyzeFile: %s (and the failure could not be recorded: %w)", reason, err)
	}
	i.restampArtifact(ctx, fileId)
	return fmt.Errorf("library.analyzeFile: %s", reason)
}

// chunkWrite bundles one createLibraryFileChunk call.
type chunkWrite struct {
	chunkId    string
	fileId     string
	artifactId string
	seq        int
	text       string
}

// writeChunk inserts one chunk row and returns the CANONICAL node id the
// vector must be keyed by.
//
// The id is composed from the short id rather than taken from the write's
// echo, and normalised through memql.BareShortId first so the composition
// is idempotent: BareShortId leaves a bare value untouched and strips one
// canonical prefix, so `conceptFileChunk + ":" + BareShortId(x)` is the
// canonical id whether x arrived bare or canonical. Getting this wrong is
// invisible until search time -- node_vectors would carry a key that
// joins to no row, and similarTo would simply return nothing for a file
// whose every chunk embedded successfully.
func (i *Integration) writeChunk(ctx context.Context, c chunkWrite) (string, error) {
	q := fmt.Sprintf(
		"mutation createLibraryFileChunk(chunkId: %s, fileId: %s, artifactId: %s, seq: %d, text: %s, tokenCount: %d)",
		langparser.QuoteString(c.chunkId),
		langparser.QuoteString(c.fileId),
		langparser.QuoteString(c.artifactId),
		c.seq,
		langparser.QuoteString(c.text),
		approxTokens(c.text),
	)
	if _, err := i.engine.Execute(ctx, q); err != nil {
		return "", err
	}
	return conceptFileChunk + ":" + memql.BareShortId(c.chunkId), nil
}

// embedChunk dispatches the ONE embedding write-path
// (integration.embedding.store, reached through the libraryEmbedChunk
// builtin) for a chunk that is already durable. vectorField is `content`,
// which is what similarTo's join requires -- its statement filters
// `nv.vector_field = 'content'`, so a vector stored under any other field
// name is written, kept, and never matched.
//
// A BUILTIN IS NOT CALLED THE WAY A MUTATION IS. The invocation form is
// `builtin <name>(k: v, ...)` -- the spelling sdk/go/client's generated
// builders emit. The bare `<name>(k: v)` form is refused with "requires a
// JSON object argument", and the object-literal `<name>({...})` form the
// error text suggests has been refused by the function surface since
// memql#2335, so the two obvious spellings are both wrong and only the
// third works. Measured, not assumed: dsl_calls_test.go runs this exact
// rendering through the real front end.
func (i *Integration) embedChunk(ctx context.Context, nodeId, text string) error {
	q := fmt.Sprintf(
		`builtin libraryEmbedChunk(nodeId: %s, text: %s, concept: %s, vectorField: "content")`,
		langparser.QuoteString(nodeId),
		langparser.QuoteString(text),
		langparser.QuoteString(conceptFileChunk),
	)
	_, err := i.engine.Execute(ctx, q)
	return err
}

// awaitArtifactId resolves the index row indexFileOnCreate promoted this
// file into, waiting a bounded time for it.
//
// The wait exists because promotion is an AUTOMATION on graph.node.created
// and this pass starts from the same upload: on a fast local write the
// first read genuinely can precede the promotion. It is bounded rather
// than indefinite because a file that never promotes must fail with a
// reason, not park at `analyzing` forever.
func (i *Integration) awaitArtifactId(ctx context.Context, fileId string) (string, bool) {
	attempts, interval := i.artifactPollAttempts, i.artifactPollInterval
	if attempts <= 0 {
		attempts = defaultArtifactPollAttempts
	}
	if interval <= 0 {
		interval = defaultArtifactPollInterval
	}
	for attempt := range attempts {
		if row, ok := i.artifactForFile(ctx, fileId); ok {
			if artifactId := stringField(row, "id"); artifactId != "" {
				return artifactId, true
			}
		}
		if attempt == attempts-1 {
			break
		}
		select {
		case <-ctx.Done():
			return "", false
		case <-time.After(interval):
		}
	}
	return "", false
}

const (
	defaultArtifactPollAttempts = 10
	defaultArtifactPollInterval = 300 * time.Millisecond
)

// artifactForFile reads the artifact index row promoted from a file,
// through libraryArtifactBySourceConceptRef -- the declared-payload-field
// read, never a Go-side re-derivation of createArtifact's hash-based id.
func (i *Integration) artifactForFile(ctx context.Context, fileId string) (map[string]any, bool) {
	q := fmt.Sprintf(`query libraryArtifactBySourceConceptRef(sourceConceptRef: %s)`,
		langparser.QuoteString(fileSourceRef(fileId)))
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

// fileSourceRef composes the sourceConceptRef indexFileOnCreate wrote:
// the file row's CANONICAL id. Idempotent in the same way writeChunk's id
// composition is -- a caller holding either spelling gets the canonical
// one back.
func fileSourceRef(fileId string) string {
	return conceptFile + ":" + memql.BareShortId(fileId)
}

// restampArtifact re-versions the file's artifact index row so the
// Library reflects what analysis learned.
//
// THIS IS NOT OPTIONAL BOOKKEEPING. indexFileOnCreate filters on
// status == "stored", so it fires once, at creation, and never again --
// which means the summary this pass wrote, and the status the owner is
// looking at, reach the index only through here.
//
// Everything is carried forward from the two rows that hold it: the file
// row for the file's own facts, currentArtifactCarryForward for the two
// INDEX-ONLY fields (labels, archived) that have no counterpart on the
// file and would otherwise be erased by createArtifact's bare insert{}.
// A failed carry-forward read SKIPS the re-stamp entirely rather than
// writing zero values over real ones -- the stale summary is the
// documented price, resurrecting an archived row or wiping labels is not.
func (i *Integration) restampArtifact(ctx context.Context, fileId string) {
	file, err := i.loadFile(ctx, fileId)
	if err != nil || file == nil {
		i.log().Warn("library: artifact re-stamp skipped, file row unreadable",
			"fileId", fileId, "error", err)
		return
	}
	sourceRef := fileSourceRef(stringField(file, "id"))
	// Skip entirely when the index row does not exist YET. Two of the
	// three callers (the opaque path and failFile) reach here without
	// having waited for indexFileOnCreate, so on a fast upload the
	// promotion can still be in flight -- and createArtifact would happily
	// CREATE the row from here, racing the automation that is about to
	// write it. currentArtifactCarryForward cannot answer this on its own:
	// it deliberately collapses "no row yet" and "a row with nothing to
	// carry" into the same zero value, because for its original caller
	// they are the same thing.
	if _, promoted := i.artifactForFile(ctx, fileId); !promoted {
		return
	}
	carry, ok := i.currentArtifactCarryForward(ctx, sourceRef)
	if !ok {
		i.log().Warn("library: artifact re-stamp skipped, index row unreadable",
			"fileId", fileId, "sourceConceptRef", sourceRef)
		return
	}
	// The write itself lives in library.go beside the other two artifact-index
	// writers, so every createArtifact statement in this package is in one file.
	if err := i.writeFileArtifact(ctx, sourceRef, file, carry); err != nil {
		i.log().Warn("library: artifact re-stamp failed", "fileId", fileId, "error", err)
	}
}

// fileArtifactSource / fileArtifactTitle / fileArtifactFormat mirror the
// coalescing indexFileOnCreate does in its step args (`?? "other"`,
// `?? "Untitled file"`). Restated in Go because the re-stamp is the
// promotion's continuation and must not disagree with it: an empty
// PRESENT enum argument is refused by createArtifact's arg validation,
// which is the failure these three exist to make impossible.
func fileArtifactSource(source string) string {
	if strings.TrimSpace(source) == "" {
		return "uploaded"
	}
	return source
}

func fileArtifactTitle(name string) string {
	if strings.TrimSpace(name) == "" {
		return "Untitled file"
	}
	return name
}

func fileArtifactFormat(format string) string {
	if strings.TrimSpace(format) == "" {
		return "other"
	}
	return format
}

// loadFile reads one v1:library:file row under the CALLER'S actor.
// libraryFileById is gated on ownerUserId == actor.userId, so a context
// carrying anyone but the owner simply gets no row -- which is the whole
// of the authorization for every path that starts here.
func (i *Integration) loadFile(ctx context.Context, fileId string) (map[string]any, error) {
	q := fmt.Sprintf(`query libraryFileById(fileId: %s)`, langparser.QuoteString(fileId))
	raw, err := i.engine.Execute(ctx, q)
	if err != nil {
		return nil, err
	}
	rows := extractRows(raw)
	if len(rows) == 0 {
		return nil, nil
	}
	return rows[0], nil
}

// fileChunks reads a file's chunk rows under the caller's actor, in
// file order. libraryFileChunksForFile sorts newest-first (the shared
// keyset-pagination shape), so the seq sort here is what puts the text
// back in reading order.
func (i *Integration) fileChunks(ctx context.Context, fileId string) ([]map[string]any, error) {
	q := fmt.Sprintf(`query libraryFileChunksForFile(fileId: %s)`, langparser.QuoteString(fileId))
	raw, err := i.engine.Execute(ctx, q)
	if err != nil {
		return nil, err
	}
	rows := extractRows(raw)
	sortRowsBySeq(rows)
	return rows, nil
}

func sortRowsBySeq(rows []map[string]any) {
	for a := 1; a < len(rows); a++ {
		for b := a; b > 0 && intField(rows[b], "seq") < intField(rows[b-1], "seq"); b-- {
			rows[b], rows[b-1] = rows[b-1], rows[b]
		}
	}
}

// chunkIdFor derives a stable BARE short id for one chunk, keyed by the
// file, the position and the text -- knowledge.ingest's chunkIdFor shape,
// for its reason: a re-run of the pass over the same file lands on the
// same ids and versions the same rows rather than doubling the file's
// chunk count, and a chunk whose TEXT changed gets a new id rather than
// silently overwriting the vector of different content.
//
// Bare, with no colon-composed prefix: validateShortId rejects a shortId
// containing colons unless it is concept-prefixed, and the hash already
// carries every uniqueness factor.
func chunkIdFor(fileId string, seq int, text string) string {
	return string(id.New().MustFromMap(map[string]any{
		"kind":   "libraryFileChunk",
		"fileId": fileId,
		"seq":    seq,
		"text":   text,
	}))
}

// approxTokens is the same character-length heuristic the knowledge
// chunker records on its own rows. Advisory: nothing reads it as a
// correctness signal.
func approxTokens(text string) int {
	if text == "" {
		return 0
	}
	return len([]rune(text)) / 4
}
