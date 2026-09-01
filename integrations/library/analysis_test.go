package library

// analysis_test.go -- memql#4342.
//
// The stub in library_test.go models the DOCUMENT edit path; this one
// models the FILE path (rows, chunks, vectors, the index) and is shared
// by similarity_test.go and train_test.go.
//
// Two modelling decisions here are what make these tests evidence rather
// than agreement-with-themselves:
//
//   - CHUNK IDS ARE CANONICALISED ON WRITE, exactly as the engine does
//     (`v1:library:fileChunk:<shortId>`), and the vector store is keyed by
//     node id with similarTo JOINING the two. So a pass that composed the
//     wrong node id for the vector -- the single most invisible bug on
//     this path, since every write succeeds and search simply returns
//     nothing -- fails here.
//
//   - EVERY READ IS OWNER-GATED, the way the real queries are
//     (ownerUserId == actor.userId). A capability that forgot to run
//     under the file owner's actor gets no rows, rather than quietly
//     working against a stub that ignores identity.

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/auth"
	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/core/common"
	"google.golang.org/protobuf/types/known/structpb"
)

// --- the file-path stub engine -------------------------------------------

type libStub struct {
	// files keyed by BARE file id.
	files map[string]map[string]any
	// chunks keyed by CANONICAL node id.
	chunks map[string]map[string]any
	// vectors keyed by the node id libraryEmbedChunk was given -- the
	// join key. A vector whose key names no chunk is unreachable, which
	// is what models similarTo's inner join.
	vectors map[string]string
	// artifacts keyed by the stub's own derived id.
	artifacts map[string]map[string]any

	calls        []string
	ingests      []map[string]any
	auditEvents  []string
	summaryText  string
	summaryErr   error
	failNextCall string
}

func newLibStub() *libStub {
	return &libStub{
		files:     map[string]map[string]any{},
		chunks:    map[string]map[string]any{},
		vectors:   map[string]string{},
		artifacts: map[string]map[string]any{},
	}
}

// seedFile writes a v1:library:file row in its post-upload state.
func (s *libStub) seedFile(fileId, owner, name, mime, format string) {
	s.files[fileId] = map[string]any{
		"id":              fileId,
		"ownerUserId":     owner,
		"name":            name,
		"mimeType":        mime,
		"format":          format,
		"source":          "uploaded",
		"status":          "stored",
		"embeddingStatus": "none",
		"archived":        false,
	}
}

// seedPromotedArtifact writes the index row indexFileOnCreate would have
// promoted the file into, with a label already on it so a re-stamp that
// drops the carry-forward is visible.
func (s *libStub) seedPromotedArtifact(fileId, owner string, labels []string) string {
	sourceRef := conceptFile + ":" + fileId
	artifactId := libStubArtifactId(sourceRef)
	anyLabels := make([]any, len(labels))
	for i, l := range labels {
		anyLabels[i] = l
	}
	s.artifacts[artifactId] = map[string]any{
		"id":               artifactId,
		"sourceConceptRef": sourceRef,
		"ownerUserId":      owner,
		"lens":             "artifact",
		"kind":             "file",
		"source":           "uploaded",
		"title":            "seeded",
		"labels":           anyLabels,
		"archived":         false,
	}
	return artifactId
}

// libStubArtifactId is the stub's stand-in for createArtifact's
// hash-derived id: deterministic in sourceConceptRef, which is the one
// property the real derivation provides that this suite depends on (one
// index row per source ref, so a re-stamp lands on the same row).
func libStubArtifactId(sourceRef string) string { return "artifact:" + sourceRef }

// seedForeignChunk writes another user's chunk + vector directly, so a
// cross-user leak has something to leak.
func (s *libStub) seedForeignChunk(chunkId, owner, fileId, artifactId, text string) {
	nodeId := conceptFileChunk + ":" + chunkId
	s.chunks[nodeId] = map[string]any{
		"id":          nodeId,
		"ownerUserId": owner,
		"fileId":      fileId,
		"artifactId":  artifactId,
		"seq":         0,
		"text":        text,
	}
	s.vectors[nodeId] = text
	s.artifacts[artifactId] = map[string]any{
		"id":               artifactId,
		"sourceConceptRef": conceptFile + ":" + fileId,
		"ownerUserId":      owner,
		"lens":             "artifact",
		"kind":             "file",
		"source":           "uploaded",
		"title":            "somebody else's file",
	}
}

func (s *libStub) Execute(ctx context.Context, query string) (*memql.ExecuteResult, error) {
	s.calls = append(s.calls, query)
	name, args := parseLibCall(query)
	if s.failNextCall != "" && s.failNextCall == name {
		s.failNextCall = ""
		return nil, fmt.Errorf("stub: simulated failure of %s", name)
	}
	actor := actorUserId(ctx)

	switch name {
	case "libraryFileById":
		fileId := asString(args["fileId"])
		row, ok := s.files[fileId]
		if !ok || row["ownerUserId"] != actor {
			return libBundle(nil), nil
		}
		return libBundle([]map[string]any{cloneRow(row)}), nil

	case "setLibraryFileStatus":
		fileId := asString(args["fileId"])
		row, ok := s.files[fileId]
		if !ok {
			return libBundle(nil), nil
		}
		// Read-merge, and ONLY the keys this call named: the mutation's
		// whole contract is that an absent optional argument is not
		// written, so a stub that wrote zero values for the missing ones
		// could not catch a pass that blanks a summary on its way to
		// `ready`.
		for _, key := range []string{"status", "summary", "embeddingStatus", "failureReason", "sha256"} {
			if v, present := args[key]; present {
				row[key] = v
			}
		}
		return libBundle(nil), nil

	case "createLibraryFileChunk":
		chunkId := asString(args["chunkId"])
		// The engine canonicalises `insert { id: args.chunkId }` to
		// {concept}:{shortId}; the vector join depends on that exact
		// spelling, so the stub reproduces it.
		nodeId := conceptFileChunk + ":" + chunkId
		s.chunks[nodeId] = map[string]any{
			"id":          nodeId,
			"ownerUserId": actor, // stamped from actor.userId, never args
			"fileId":      args["fileId"],
			"artifactId":  args["artifactId"],
			"seq":         args["seq"],
			"text":        args["text"],
			"tokenCount":  args["tokenCount"],
		}
		return libBundle(nil), nil

	case "libraryFileChunksForFile":
		fileId := asString(args["fileId"])
		out := []map[string]any{}
		for _, row := range s.chunks {
			if row["ownerUserId"] != actor || asString(row["fileId"]) != fileId {
				continue
			}
			out = append(out, cloneRow(row))
		}
		sort.Slice(out, func(a, b int) bool { return intField(out[a], "seq") > intField(out[b], "seq") })
		return libBundle(out), nil

	case "appendLibraryFileTrainedDomain":
		fileId := asString(args["fileId"])
		if row, ok := s.files[fileId]; ok {
			row["trainedIntoDomainIds"] = args["trainedIntoDomainIds"]
		}
		return libBundle(nil), nil

	case "libraryArtifactBySourceConceptRef":
		sourceRef := asString(args["sourceConceptRef"])
		for _, a := range s.artifacts {
			if asString(a["sourceConceptRef"]) == sourceRef && a["ownerUserId"] == actor {
				return libBundle([]map[string]any{cloneRow(a)}), nil
			}
		}
		return libBundle(nil), nil

	case "libraryArtifactById":
		artifactId := asString(args["artifactId"])
		a, ok := s.artifacts[artifactId]
		if !ok || a["ownerUserId"] != actor {
			return libBundle(nil), nil
		}
		return libBundle([]map[string]any{cloneRow(a)}), nil

	case "createArtifact":
		if err := validateCreateArtifactEnums(args); err != nil {
			return nil, err
		}
		sourceRef := asString(args["sourceConceptRef"])
		id := libStubArtifactId(sourceRef)
		// Full replace -- createArtifact's bare insert{} keeps only the
		// fields THIS call names (design D3).
		row := map[string]any{}
		for k, v := range args {
			row[k] = v
		}
		row["id"] = id
		s.artifacts[id] = row
		return libBundle(nil), nil

	case "libraryEmbedChunk":
		nodeId := asString(args["nodeId"])
		if nodeId == "" || asString(args["text"]) == "" {
			return nil, fmt.Errorf("stub: libraryEmbedChunk requires nodeId and text")
		}
		if asString(args["vectorField"]) != "content" {
			return nil, fmt.Errorf("stub: libraryEmbedChunk vectorField must be 'content', got %q", args["vectorField"])
		}
		s.vectors[nodeId] = asString(args["text"])
		return libBundle(nil), nil

	case "similarTo":
		return libBundle(s.rankChunks(asString(args["text"]), intField(args, "limit"))), nil

	case "knowledgeIngest":
		s.ingests = append(s.ingests, args)
		return libBundle(nil), nil

	case "createAuditEvent":
		s.auditEvents = append(s.auditEvents, query)
		return libBundle(nil), nil
	}
	return libBundle(nil), nil
}

// rankChunks models similarTo: a JOIN of the vector store onto the node
// store (so a vector keyed by an id no chunk carries is unreachable),
// scored, ordered, and capped -- WITH NO OWNER FILTER, because the real
// operator has none. Scoping is the capability's job, and a stub that
// scoped here would prove nothing about it.
//
// The score is word overlap rather than a cosine: deterministic, and it
// makes "the right artifact first" a statement about the pass having
// embedded the right text under the right key.
func (s *libStub) rankChunks(query string, limit int) []map[string]any {
	if limit <= 0 {
		limit = 5
	}
	terms := strings.Fields(strings.ToLower(query))
	scored := []map[string]any{}
	for nodeId, text := range s.vectors {
		chunk, ok := s.chunks[nodeId]
		if !ok {
			continue // the join finds nothing -- an orphan vector
		}
		hay := strings.ToLower(text)
		hits := 0
		for _, t := range terms {
			if strings.Contains(hay, t) {
				hits++
			}
		}
		if hits == 0 {
			continue
		}
		row := cloneRow(chunk)
		row["_similarity"] = float64(hits) / float64(len(terms))
		scored = append(scored, row)
	}
	sort.Slice(scored, func(a, b int) bool {
		sa, sb := scored[a]["_similarity"].(float64), scored[b]["_similarity"].(float64)
		if sa != sb {
			return sa > sb
		}
		return asString(scored[a]["id"]) < asString(scored[b]["id"])
	})
	if len(scored) > limit {
		scored = scored[:limit]
	}
	return scored
}

func (s *libStub) InvokeAI(_ context.Context, templateId string, _ map[string]any) (any, error) {
	if templateId != "docSummary" {
		return nil, fmt.Errorf("stub: unexpected prompt %q", templateId)
	}
	if s.summaryErr != nil {
		return nil, s.summaryErr
	}
	return s.summaryText, nil
}

func (s *libStub) RegisterIntegration(memql.IntegrationProvider) error { return nil }
func (s *libStub) InvokeAIStructured(context.Context, string, map[string]any, string, json.RawMessage, bool) (string, error) {
	return "", nil
}
func (s *libStub) RenderPrompt(string, map[string]any) (string, error) { return "", nil }
func (s *libStub) ChatStreamProvider() common.ChatStreamProvider       { return nil }
func (s *libStub) ChatStreamProviderByName(string) common.ChatStreamProvider {
	return nil
}
func (s *libStub) ChatStreamWithToolsProviderByName(string) common.ChatStreamWithToolsProvider {
	return nil
}
func (s *libStub) ToolDefinitionsForNames([]string) []common.ToolDefinition { return nil }
func (s *libStub) ExecuteToolByName(context.Context, string, map[string]any) (string, error) {
	return "", nil
}
func (s *libStub) ResolveSkills(context.Context, []string) (memql.SkillBundle, error) {
	return memql.SkillBundle{}, nil
}

// parseLibCall handles BOTH call shapes this integration emits: the
// named-argument form (`mutation setLibraryFileStatus(fileId: "x", ...)`)
// and the object-profile form (`similarTo({"text": "..."})`) that
// @args(profile="object") builtins take. parseCall (library_test.go)
// covers only the first -- its key pattern requires an unquoted
// identifier before the colon, so a JSON key never matches it and every
// argument would silently arrive empty.
func parseLibCall(q string) (string, map[string]any) {
	open := strings.IndexByte(q, '(')
	if open <= 0 {
		return "", map[string]any{}
	}
	name := strings.TrimSpace(q[:open])
	if fields := strings.Fields(name); len(fields) > 0 {
		name = fields[len(fields)-1]
	}
	body := strings.TrimSpace(q[open+1:])
	if strings.HasPrefix(body, "{") {
		end := strings.LastIndex(body, "}")
		if end > 0 {
			args := map[string]any{}
			if err := json.Unmarshal([]byte(body[:end+1]), &args); err == nil {
				return name, args
			}
		}
	}
	_, args := parseCall(q)
	return name, args
}

func cloneRow(row map[string]any) map[string]any {
	out := make(map[string]any, len(row))
	for k, v := range row {
		out[k] = v
	}
	return out
}

func libBundle(rows []map[string]any) *memql.ExecuteResult {
	nodes := make([]*memqlv1.MemoryNode, 0, len(rows))
	for _, r := range rows {
		fields := map[string]any{}
		for k, v := range r {
			fields[k] = v
		}
		st, err := structpb.NewStruct(fields)
		if err != nil {
			// Loud rather than silent: a value the wire cannot carry means
			// the stub is modelling something the engine could not do.
			panic(fmt.Sprintf("libStub: row not representable on the wire: %v (%v)", err, r))
		}
		nodes = append(nodes, &memqlv1.MemoryNode{
			Id:      asString(r["id"]),
			Concept: asString(r["concept"]),
			Payload: st,
		})
	}
	return &memql.ExecuteResult{Bundle: &memqlv1.GraphBundle{Nodes: nodes}}
}

// --- fixtures -------------------------------------------------------------

type fixedExtractor struct {
	text string
	err  error
}

func (f fixedExtractor) Extract(context.Context, string, []byte) (string, error) {
	return f.text, f.err
}

// birdsText is long enough for the 1800/180 splitter to produce several
// overlapping chunks, so the chunking, the per-chunk embed and the
// overlap-aware rejoin are all exercised on real input rather than on a
// single-chunk degenerate case.
func birdsText() string {
	var b strings.Builder
	for i := range 12 {
		fmt.Fprintf(&b, "Paragraph %d. The kingfisher hunts from a perch above slow water and dives "+
			"straight down; the heron stalks the shallows instead, one slow step at a time. "+
			"Both are fish eaters, and both are quiet about it. Kingfishers nest in tunnels "+
			"dug into a river bank, which is why a collapsing bank costs a whole season.\n\n", i)
	}
	return b.String()
}

// analyzedFile runs the real pass over a text fixture and returns the
// stub it ran against, so a test can assert on what actually landed.
func analyzedFile(t *testing.T, s *libStub, i *Integration, fileId, owner string) {
	t.Helper()
	if err := i.AnalyzeFile(context.Background(), AnalyzeFileParams{
		FileId:      fileId,
		OwnerUserId: owner,
		Name:        "birds.txt",
		MimeType:    "text/plain",
		Data:        []byte("ignored -- the extractor is the fixture"),
	}); err != nil {
		t.Fatalf("AnalyzeFile: %v", err)
	}
}

// newAnalysisIntegration builds the integration with a fixture extractor
// and a zero-wait artifact poll (tests must not sleep).
func newAnalysisIntegration(s *libStub, text string) *Integration {
	i := NewIntegration(s)
	i.SetExtractor(fixedExtractor{text: text})
	i.SetArtifactPoll(1, 0)
	return i
}

// --- the analysis pass ----------------------------------------------------

// TestAnalyzeFile_TextFixtureYieldsEmbeddedChunksAndComplete is the
// headline claim: a readable file comes out of the pass chunked, with a
// vector per chunk, at status ready / embeddingStatus complete.
func TestAnalyzeFile_TextFixtureYieldsEmbeddedChunksAndComplete(t *testing.T) {
	s := newLibStub()
	s.summaryText = "Two fish-eating birds and how they hunt."
	s.seedFile("file-1", "user-a", "birds.txt", "text/plain", "text")
	s.seedPromotedArtifact("file-1", "user-a", []string{"nature"})
	i := newAnalysisIntegration(s, birdsText())

	analyzedFile(t, s, i, "file-1", "user-a")

	if len(s.chunks) < 2 {
		t.Fatalf("the pass wrote %d chunk(s); the fixture is several times the 1800-character "+
			"chunk size, so a single chunk means the splitter was not applied", len(s.chunks))
	}
	for nodeId := range s.chunks {
		if _, ok := s.vectors[nodeId]; !ok {
			t.Fatalf("chunk %s has no vector -- every chunk must be embedded through "+
				"libraryEmbedChunk, keyed by the chunk's CANONICAL node id, or search finds "+
				"nothing for a file whose every write succeeded.\n  vectors: %v", nodeId, s.vectors)
		}
	}
	file := s.files["file-1"]
	if got := asString(file["status"]); got != "ready" {
		t.Fatalf("status = %q, want \"ready\"", got)
	}
	if got := asString(file["embeddingStatus"]); got != "complete" {
		t.Fatalf("embeddingStatus = %q, want \"complete\" -- every chunk embedded", got)
	}
	if got := asString(file["summary"]); got != s.summaryText {
		t.Fatalf("summary = %q, want the summariser's answer %q", got, s.summaryText)
	}
	if _, present := file["failureReason"]; present {
		t.Fatalf("failureReason was written on a successful pass: %v", file["failureReason"])
	}
	// Chunks must carry the artifact so a hit folds up with no second read.
	for _, chunk := range s.chunks {
		if asString(chunk["artifactId"]) == "" {
			t.Fatalf("chunk %v carries no artifactId", chunk)
		}
		if asString(chunk["ownerUserId"]) != "user-a" {
			t.Fatalf("chunk ownerUserId = %q, want \"user-a\" -- the pass must run under the "+
				"file owner's actor, or the chunk is unreadable by its own owner",
				chunk["ownerUserId"])
		}
	}
}

// TestAnalyzeFile_ReStampsTheIndexWithTheSummaryAndKeepsLabels is the
// memql#4288/#4340 hazard reached from the analysis path.
//
// indexFileOnCreate filters on status == "stored", so it fires once and
// never again: if the pass does not re-stamp the index itself, the
// summary it just wrote never reaches the Library list. And because
// createArtifact's body is a bare insert{}, a re-stamp that forgets the
// index-only fields does not merely fail to add -- it ERASES the labels
// the owner put on the row.
func TestAnalyzeFile_ReStampsTheIndexWithTheSummaryAndKeepsLabels(t *testing.T) {
	s := newLibStub()
	s.summaryText = "Two fish-eating birds and how they hunt."
	s.seedFile("file-1", "user-a", "birds.txt", "text/plain", "text")
	artifactId := s.seedPromotedArtifact("file-1", "user-a", []string{"nature", "field-notes"})
	i := newAnalysisIntegration(s, birdsText())

	analyzedFile(t, s, i, "file-1", "user-a")

	row := s.artifacts[artifactId]
	if got := asString(row["summary"]); got != s.summaryText {
		t.Fatalf("the artifact index still shows summary %q after analysis wrote %q -- "+
			"indexFileOnCreate cannot see a status transition, so the pass has to re-stamp "+
			"the row itself", got, s.summaryText)
	}
	labels := stringSliceField(row, "labels")
	if len(labels) != 2 || labels[0] != "nature" || labels[1] != "field-notes" {
		t.Fatalf("labels = %v after the re-stamp, want [nature field-notes] -- createArtifact's "+
			"bare insert{} drops what the call omits, so the re-stamp must carry the "+
			"index-only fields forward", labels)
	}
	if boolField(row, "archived") {
		t.Fatalf("the re-stamp archived an unarchived artifact")
	}
	if got := asString(row["kind"]); got != "file" {
		t.Fatalf("kind = %q after the re-stamp, want \"file\"", got)
	}
}

// TestAnalyzeFile_ReStampKeepsAnArchivedRowArchived is the other
// direction: analysing a file whose artifact the owner already archived
// must not bring it back into the Library.
func TestAnalyzeFile_ReStampKeepsAnArchivedRowArchived(t *testing.T) {
	s := newLibStub()
	s.seedFile("file-1", "user-a", "birds.txt", "text/plain", "text")
	artifactId := s.seedPromotedArtifact("file-1", "user-a", []string{"nature"})
	s.artifacts[artifactId]["archived"] = true
	i := newAnalysisIntegration(s, birdsText())

	analyzedFile(t, s, i, "file-1", "user-a")

	if !boolField(s.artifacts[artifactId], "archived") {
		t.Fatalf("archived = false after analysing a file whose artifact was archived -- the "+
			"re-stamp resurrected a row the owner threw away.\n  row: %v", s.artifacts[artifactId])
	}
}

// TestAnalyzeFile_UnknownTypeIsReadyWithNoChunks: design 3.4 stores any
// MIME type, and an opaque one is a terminal SUCCESS. Reporting it as
// `failed` would put a red state on a file that uploaded perfectly.
func TestAnalyzeFile_UnknownTypeIsReadyWithNoChunks(t *testing.T) {
	s := newLibStub()
	s.seedFile("file-1", "user-a", "archive.zip", "application/zip", "other")
	s.seedPromotedArtifact("file-1", "user-a", nil)
	i := newAnalysisIntegration(s, "never reached")

	if err := i.AnalyzeFile(context.Background(), AnalyzeFileParams{
		FileId: "file-1", OwnerUserId: "user-a", Name: "archive.zip", MimeType: "application/zip",
	}); err != nil {
		t.Fatalf("AnalyzeFile on an opaque type: %v", err)
	}

	if got := asString(s.files["file-1"]["status"]); got != "ready" {
		t.Fatalf("status = %q for an unreadable type, want \"ready\" -- the bytes are stored "+
			"and downloadable; nothing failed", got)
	}
	if len(s.chunks) != 0 {
		t.Fatalf("an opaque type produced %d chunk(s); it must produce none", len(s.chunks))
	}
	if _, present := s.files["file-1"]["failureReason"]; present {
		t.Fatalf("failureReason set on an opaque upload: %v", s.files["file-1"]["failureReason"])
	}
}

// TestAnalyzeFile_ExtractionFailureCarriesTheReason: "failed carries the
// reason" (design 3.1) -- and it must be on the ROW, because the person
// who uploaded the file is the one who needs to see it.
func TestAnalyzeFile_ExtractionFailureCarriesTheReason(t *testing.T) {
	s := newLibStub()
	s.seedFile("file-1", "user-a", "locked.pdf", "application/pdf", "pdf")
	s.seedPromotedArtifact("file-1", "user-a", nil)
	i := NewIntegration(s)
	i.SetExtractor(fixedExtractor{err: fmt.Errorf("pdf is encrypted")})
	i.SetArtifactPoll(1, 0)

	err := i.AnalyzeFile(context.Background(), AnalyzeFileParams{
		FileId: "file-1", OwnerUserId: "user-a", Name: "locked.pdf", MimeType: "application/pdf",
	})
	if err == nil {
		t.Fatalf("AnalyzeFile returned nil for a file it could not read")
	}

	file := s.files["file-1"]
	if got := asString(file["status"]); got != "failed" {
		t.Fatalf("status = %q after an extraction failure, want \"failed\"", got)
	}
	reason := asString(file["failureReason"])
	if reason == "" {
		t.Fatalf("status is failed with no failureReason -- never a silent partial: a pass " +
			"that gives up says why, in the same write that says it gave up")
	}
	if !strings.Contains(reason, "pdf is encrypted") {
		t.Fatalf("failureReason = %q, want it to carry the underlying cause", reason)
	}
}

// TestAnalyzeFile_EmptyExtractionFails is the password-protected-PDF /
// image-only-scan case: a type we CAN read that yielded nothing. Calling
// that `ready` would tell the owner their file is searchable when it is
// not.
func TestAnalyzeFile_EmptyExtractionFails(t *testing.T) {
	s := newLibStub()
	s.seedFile("file-1", "user-a", "scan.pdf", "application/pdf", "pdf")
	s.seedPromotedArtifact("file-1", "user-a", nil)
	i := newAnalysisIntegration(s, "   \n  ")

	if err := i.AnalyzeFile(context.Background(), AnalyzeFileParams{
		FileId: "file-1", OwnerUserId: "user-a", Name: "scan.pdf", MimeType: "application/pdf",
	}); err == nil {
		t.Fatalf("AnalyzeFile returned nil for a readable type that produced no text")
	}
	file := s.files["file-1"]
	if got := asString(file["status"]); got != "failed" {
		t.Fatalf("status = %q, want \"failed\"", got)
	}
	if asString(file["failureReason"]) == "" {
		t.Fatalf("no failureReason recorded")
	}
}

// TestAnalyzeFile_SummaryOutageStillEmbeds: the summariser is best-effort
// by contract. An outage at the chat provider must not cost the owner a
// searchable file.
func TestAnalyzeFile_SummaryOutageStillEmbeds(t *testing.T) {
	s := newLibStub()
	s.summaryErr = fmt.Errorf("provider unavailable")
	s.seedFile("file-1", "user-a", "birds.txt", "text/plain", "text")
	s.seedPromotedArtifact("file-1", "user-a", nil)
	i := newAnalysisIntegration(s, birdsText())

	analyzedFile(t, s, i, "file-1", "user-a")

	if got := asString(s.files["file-1"]["status"]); got != "ready" {
		t.Fatalf("status = %q after a summariser outage, want \"ready\"", got)
	}
	if len(s.vectors) == 0 {
		t.Fatalf("no chunks embedded after a summariser outage -- the summary is best-effort, " +
			"the chunks are not")
	}
	if _, present := s.files["file-1"]["summary"]; present {
		t.Fatalf("an empty summary was WRITTEN (%v); setLibraryFileStatus omits absent optional "+
			"arguments precisely so a later transition cannot blank an earlier one",
			s.files["file-1"]["summary"])
	}
}

// TestAnalyzeFile_RefusesWithoutAnOwner. withUserActor passes the context
// through unchanged for a blank owner, so proceeding would attribute
// every chunk to nobody -- and ownerUserId is the per-row authz key, so
// those chunks would be unreadable by anyone including their owner.
func TestAnalyzeFile_RefusesWithoutAnOwner(t *testing.T) {
	s := newLibStub()
	s.seedFile("file-1", "", "birds.txt", "text/plain", "text")
	i := newAnalysisIntegration(s, birdsText())

	err := i.AnalyzeFile(context.Background(), AnalyzeFileParams{
		FileId: "file-1", MimeType: "text/plain", Name: "birds.txt",
	})
	if err == nil {
		t.Fatalf("AnalyzeFile ran with no ownerUserId; it must refuse rather than write " +
			"unattributed rows")
	}
	if len(s.chunks) != 0 {
		t.Fatalf("%d chunk(s) written on an unattributed pass", len(s.chunks))
	}
}

// TestAnalyzeFile_NoIndexRowFailsWithAReason. A chunk needs the artifact
// id, so a file that never promoted cannot be indexed -- and must say so
// rather than park at `analyzing` forever.
func TestAnalyzeFile_NoIndexRowFailsWithAReason(t *testing.T) {
	s := newLibStub()
	s.seedFile("file-1", "user-a", "birds.txt", "text/plain", "text")
	// deliberately no seedPromotedArtifact
	i := newAnalysisIntegration(s, birdsText())

	if err := i.AnalyzeFile(context.Background(), AnalyzeFileParams{
		FileId: "file-1", OwnerUserId: "user-a", Name: "birds.txt", MimeType: "text/plain",
	}); err == nil {
		t.Fatalf("AnalyzeFile returned nil for a file with no index row")
	}
	file := s.files["file-1"]
	if got := asString(file["status"]); got != "failed" {
		t.Fatalf("status = %q, want \"failed\" -- a file cannot be left at \"analyzing\"", got)
	}
	if asString(file["failureReason"]) == "" {
		t.Fatalf("no failureReason recorded for an un-promoted file")
	}
}

// TestKnownExtractableMIMEMatchesTheProcessor keeps the pass's pre-check
// honest. The pass decides "opaque vs readable" BEFORE calling the
// extractor (so an unsupported type reaches `ready`, not `failed`), which
// means this list and component/fileprocessor's must agree -- a type the
// processor gained but this list did not would silently be stored opaque
// forever.
func TestKnownExtractableMIMEMatchesTheProcessor(t *testing.T) {
	// Restated from component/fileprocessor.SupportedMIMETypes. The
	// duplication is the point: this test is what fails when the two
	// diverge.
	for _, mime := range []string{
		"application/pdf",
		"application/vnd.openxmlformats-officedocument.wordprocessingml.document",
		"text/plain",
		"text/markdown",
		"image/jpeg",
		"image/png",
		"image/gif",
		"image/webp",
	} {
		if !knownExtractableMIME(mime) {
			t.Errorf("knownExtractableMIME(%q) = false; the processor supports it, so the pass "+
				"would store it opaquely and never index it", mime)
		}
		if !knownExtractableMIME(strings.ToUpper(mime) + "; charset=utf-8") {
			t.Errorf("knownExtractableMIME did not normalise %q (case + parameters)", mime)
		}
	}
	for _, mime := range []string{"application/zip", "video/mp4", "", "application/octet-stream"} {
		if knownExtractableMIME(mime) {
			t.Errorf("knownExtractableMIME(%q) = true; an unreadable type must take the opaque "+
				"path rather than be attempted and fail", mime)
		}
	}
}

// TestEmbeddingStatusFor pins the three states the field promises.
func TestEmbeddingStatusFor(t *testing.T) {
	cases := []struct {
		chunks, embedded int
		want             string
	}{
		{0, 0, "none"},
		{3, 0, "none"},
		{3, 1, "partial"},
		{3, 3, "complete"},
	}
	for _, c := range cases {
		if got := embeddingStatusFor(c.chunks, c.embedded); got != c.want {
			t.Errorf("embeddingStatusFor(%d, %d) = %q, want %q", c.chunks, c.embedded, got, c.want)
		}
	}
}

// ownerContext is the access context a real request carries.
func ownerContext(userId string) context.Context {
	return auth.ContextWithUserActor(context.Background(), userId)
}

// clusterOwnerContext carries the operator role -- ContextWithUserActor
// stamps RoleWriter, never owner, so a test that needs the operator must
// build the access context itself.
func clusterOwnerContext(userId string) context.Context {
	ctx := auth.ContextWithUserActor(context.Background(), userId)
	return auth.ContextWithAccess(ctx, &auth.AccessContext{UserId: userId, Role: auth.RoleOwner})
}

// TestAnalyzeFile_UsesAPreResolvedArtifactId. The upload handler
// (memql#4341) resolves the promoted index row before it detaches and
// passes it in; the pass must use that rather than re-reading and racing
// the automation. Asserted by making the read UNAVAILABLE (the seeded
// artifact belongs to somebody else, so libraryArtifactBySourceConceptRef
// returns nothing under this owner) and checking the pass still indexes.
func TestAnalyzeFile_UsesAPreResolvedArtifactId(t *testing.T) {
	s := newLibStub()
	s.seedFile("file-1", "user-a", "birds.txt", "text/plain", "text")
	// Promoted under a DIFFERENT owner, so the pass's own lookup misses.
	s.seedPromotedArtifact("file-1", "somebody-else", nil)
	i := newAnalysisIntegration(s, birdsText())

	if err := i.AnalyzeFile(context.Background(), AnalyzeFileParams{
		FileId:      "file-1",
		ArtifactId:  "artifact-handed-over",
		OwnerUserId: "user-a",
		Name:        "birds.txt",
		MimeType:    "text/plain",
	}); err != nil {
		t.Fatalf("AnalyzeFile with a pre-resolved artifactId: %v", err)
	}
	if len(s.chunks) == 0 {
		t.Fatalf("no chunks written; the pass re-read the index instead of using the id it was given")
	}
	for _, chunk := range s.chunks {
		if got := asString(chunk["artifactId"]); got != "artifact-handed-over" {
			t.Fatalf("chunk artifactId = %q, want the id the caller handed over", got)
		}
	}
}
