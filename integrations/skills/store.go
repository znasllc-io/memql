package skills

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	langparser "github.com/znasllc-io/memql/component/language/parser"
	skillgraph "github.com/znasllc-io/memql/component/skills"
)

// store.go -- the engine-backed reads and writes behind the composition.
//
// Kept apart from runscript.go on purpose: everything there is decided from
// arguments and a Surface's answers, which is what lets the rules be tested
// with fakes and no engine. This file is the only part that knows MemQL
// exists.

// Executor is the engine seam. Narrow, and `any` on the result for the reason
// component/workjournal's is: nothing here reads a result's concrete type, so
// taking the engine's own would buy a dependency and nothing else.
type Executor interface {
	Execute(ctx context.Context, query string) (any, error)
	// ExecuteRows runs a read and hands back the rows as decoded maps. The
	// engine's result type differs by call site, so the adapter that knows it
	// does the decoding.
	ExecuteRows(ctx context.Context, query string) ([]map[string]any, error)
}

// BlobFetcher streams a stored blob. Same shape as the Library analysis
// pass's, and satisfied by the same Azure client.
type BlobFetcher interface {
	DownloadStreamURL(ctx context.Context, blobURL string) (io.ReadCloser, error)
}

// maxScriptFetchBytes bounds what is read out of the Library for a script.
// It is MaxVerifiableBytes plus one byte: reading exactly one more than the
// ceiling is how "too large" is DETECTED rather than silently truncated, and
// a truncated script that hashed to something else would be reported as a
// corruption that never happened.
const maxScriptFetchBytes = MaxVerifiableBytes + 1

// EngineStore reads skills and Library artifacts through the engine, under
// whatever actor the calling context carries.
//
// THE CALLER'S ACTOR, NEVER A BORROWED ONE. A script runs on somebody's
// workbench or somebody's machine, so which skills and which artifacts the
// CALLER may reach is the whole authorization question -- and stamping a
// wider actor here would answer it with the engine's own reach. `skillById`
// is public-tier so every signed-in caller sees the catalog; the Library
// reads are owner-tiered, so an artifact that is not the caller's simply is
// not found.
type EngineStore struct {
	engine  Executor
	fetcher BlobFetcher
	writer  *skillgraph.Writer
}

func NewEngineStore(engine Executor, fetcher BlobFetcher) *EngineStore {
	if engine == nil {
		return nil
	}
	return &EngineStore{engine: engine, fetcher: fetcher, writer: skillgraph.NewWriter(engine)}
}

// SkillScripts reads one skill's scripts.
//
// AN ABSENT SKILL AND AN UNREADABLE ONE ARE ONE ANSWER, which is not a
// shortcut: row admission returns zero rows rather than an error, so the
// engine itself cannot tell them apart and neither can this.
func (s *EngineStore) SkillScripts(ctx context.Context, skillID string) (SkillScripts, bool, error) {
	if s == nil || s.engine == nil {
		return SkillScripts{}, false, fmt.Errorf("skills: no engine")
	}
	rows, err := s.engine.ExecuteRows(ctx, fmt.Sprintf("query skillById(skillId: %s)",
		langparser.QuoteString(skillID)))
	if err != nil {
		return SkillScripts{}, false, err
	}
	if len(rows) == 0 {
		return SkillScripts{}, false, nil
	}
	row := rows[0]
	out := SkillScripts{
		SkillID: stringField(row, "id"),
		Slug:    stringField(row, "slug"),
		// ABSENT READS AS ACTIVE. `active` defaults true on the concept and a
		// projected row that omits it is one that never had it written, not
		// one that was deactivated -- reading absence as "off" would make
		// every seeded catalog row unrunnable.
		Active:  row["active"] != false,
		Scripts: scriptsFrom(row["scripts"]),
	}
	if out.SkillID == "" {
		out.SkillID = skillID
	}
	return out, true, nil
}

// SetScripts writes the merged list back.
//
// THROUGH component/skills's WRITER, not through the engine here.
// `setSkillScripts` is @serverOnly, and origin defaults to CLIENT -- so the
// write needs an internal-origin stamp, and the stamp is allowlisted per
// package. This package also serves the caller-scoped Library reads runScript
// makes, so an entry for it would put the stamp within reach of those; the
// writer package exists for exactly these three writes and nothing else.
func (s *EngineStore) SetScripts(ctx context.Context, skillID string, scripts []Script) error {
	if s == nil || s.writer == nil {
		return fmt.Errorf("skills: no graph writer is wired, so a captured script cannot be recorded")
	}
	encoded, err := json.Marshal(scripts)
	if err != nil {
		return fmt.Errorf("skills: encode scripts: %w", err)
	}
	return s.writer.SetScripts(ctx, skillID, encoded)
}

// ReadArtifact fetches a Library artifact's bytes.
//
// TWO READS AND A STREAM. The artifact row is an INDEX and owns no content
// (memql#693); the bytes live on the `v1:library:file` it points at through
// `sourceConceptRef`. Both reads run under the caller's actor, so an artifact
// that is not theirs is not found at either hop.
//
// THE HASH IS RECOMPUTED HERE and never taken off the row. `file.sha256` is a
// dedup hint that is legitimately ABSENT for a chunked upload the analysis
// pass has not reached yet, and "the field was empty" must not become "the
// script is whatever arrived".
func (s *EngineStore) ReadArtifact(ctx context.Context, artifactID string) (ScriptBytes, error) {
	if s == nil || s.engine == nil {
		return ScriptBytes{}, fmt.Errorf("skills: no engine")
	}
	if s.fetcher == nil {
		return ScriptBytes{}, fmt.Errorf("no blob fetcher is wired on this node, so a script's bytes cannot be read")
	}
	fileID, err := s.fileIDFor(ctx, artifactID)
	if err != nil {
		return ScriptBytes{}, err
	}
	rows, err := s.engine.ExecuteRows(ctx, fmt.Sprintf("query libraryFileById(fileId: %s)",
		langparser.QuoteString(fileID)))
	if err != nil {
		return ScriptBytes{}, err
	}
	if len(rows) == 0 {
		return ScriptBytes{}, fmt.Errorf("the file behind artifact %s is not readable here", artifactID)
	}
	blobURL := stringField(rows[0], "blobUrl")
	if blobURL == "" {
		return ScriptBytes{}, fmt.Errorf("the file behind artifact %s names no stored bytes", artifactID)
	}
	body, err := s.fetcher.DownloadStreamURL(ctx, blobURL)
	if err != nil {
		return ScriptBytes{}, fmt.Errorf("reading the stored bytes: %w", err)
	}
	defer func() { _ = body.Close() }()
	data, err := io.ReadAll(io.LimitReader(body, maxScriptFetchBytes))
	if err != nil {
		return ScriptBytes{}, fmt.Errorf("reading the stored bytes: %w", err)
	}
	return ScriptBytes{Data: data, Sha256: sha256Hex(data), Name: stringField(rows[0], "name")}, nil
}

// fileIDFor resolves an artifact id to the file it indexes. A caller that
// already holds a FILE id may pass it directly -- the two id spaces are
// distinguishable by their concept prefix, and a skill authored against a
// file rather than its index entry should work rather than fail on a
// technicality.
func (s *EngineStore) fileIDFor(ctx context.Context, artifactID string) (string, error) {
	if strings.Contains(artifactID, ":library:file:") {
		return artifactID, nil
	}
	rows, err := s.engine.ExecuteRows(ctx, fmt.Sprintf("query libraryArtifactById(artifactId: %s)",
		langparser.QuoteString(artifactID)))
	if err != nil {
		return "", err
	}
	if len(rows) == 0 {
		return "", fmt.Errorf("artifact %s is not readable here -- it does not exist, or it is not yours", artifactID)
	}
	ref := stringField(rows[0], "sourceConceptRef")
	if ref == "" {
		return "", fmt.Errorf("artifact %s indexes no file", artifactID)
	}
	return ref, nil
}

// ---------------------------------------------------------------------------

func scriptsFrom(raw any) []Script {
	list, ok := raw.([]any)
	if !ok {
		return nil
	}
	out := make([]Script, 0, len(list))
	for _, item := range list {
		entry, ok := item.(map[string]any)
		if !ok {
			continue
		}
		s := Script{
			Platform:   stringField(entry, "platform"),
			ArtifactID: stringField(entry, "artifactId"),
			Entry:      stringField(entry, "entry"),
		}
		if schema, ok := entry["argsSchema"].(map[string]any); ok {
			s.ArgsSchema = schema
		}
		// An entry naming no artifact is DROPPED rather than carried: it can
		// never be shipped, and keeping it would make `no_script_for_platform`
		// fire as `script_unreadable` one step later, which points at the
		// Library for an authoring mistake.
		if s.ArtifactID != "" {
			out = append(out, s)
		}
	}
	return out
}

func stringField(row map[string]any, key string) string {
	v, _ := row[key].(string)
	return strings.TrimSpace(v)
}
