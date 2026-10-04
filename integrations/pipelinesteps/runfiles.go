package pipelinesteps

import (
	"context"
	"errors"
	"fmt"
	"mime"
	"os"
	"path"
	"strings"
	"unicode/utf8"

	pl "github.com/znasllc-io/memql/component/pipelines"
)

// runfiles.go -- a step's files on their way to its owner's Library (epic
// memql#5478, #5495): the archived log and each artifact, named the same way
// whichever surface ran the step. What the Library does not keep is a note
// beside the step, never a failure of it: the step ran, and how it ended is
// already decided.

// runLogMimeType is the archived log's media type.
const runLogMimeType = "text/plain; charset=utf-8"

const (
	// runNoteNameBytes bounds one entry name a note quotes, and
	// runNoteBytes the whole note: an archive's names are the step's to
	// choose and unbounded (a 97 KB archive of long names once made a 33 MB
	// error string), and a note rides the step's result.
	runNoteNameBytes = 256
	runNoteBytes     = 2 << 10
)

// runLogFileName is the Library name of a step's log: its key, "stage.step"
// or "stage.step#i", with every byte outside [A-Za-z0-9._-] made '-', and
// ".log".
func runLogFileName(stepKey string) string {
	var b strings.Builder
	for i := 0; i < len(stepKey); i++ {
		c := stepKey[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '.', c == '_', c == '-':
			b.WriteByte(c)
		default:
			b.WriteByte('-')
		}
	}
	return b.String() + ".log"
}

// runArtifactFileName is an artifact's Library name: its path in the working
// copy, every "/" made "__", so dist/report.json is dist__report.json.
func runArtifactFileName(p string) string { return strings.ReplaceAll(p, "/", "__") }

// runFileMimeType is a file's media type by its extension, or
// application/octet-stream when the extension says nothing.
func runFileMimeType(p string) string {
	if t := mime.TypeByExtension(path.Ext(p)); t != "" {
		return t
	}
	return "application/octet-stream"
}

// stepFiles files one step's log and artifacts in its owner's Library,
// bound to the work run and step, and gathers the notes about what it could
// not keep.
type stepFiles struct {
	library LibraryStore
	run     StepRun
	// mask is applied to every note: a note quotes paths and errors, and
	// either can carry what the step printed.
	mask  func(string) string
	notes []pl.Failure
}

func (s *stepFiles) note(format string, args ...any) {
	s.notes = append(s.notes, pl.Failure{Code: pl.CodeArtifactMissing, Message: s.mask(fmt.Sprintf(format, args...))})
}

// store files one RunFile and answers its id, or "" with a note saying why.
func (s *stepFiles) store(ctx context.Context, name, mimeType string, data []byte, what string) string {
	if s.library == nil {
		s.note("%s was not stored in the Library: this node has no Library store", what)
		return ""
	}
	stored, err := s.library.StoreRunFile(ctx, RunFile{
		OwnerUserID: s.run.OwnerUserID,
		WorkRunID:   s.run.WorkRunID,
		StepKey:     s.run.StepKey,
		Name:        name,
		MimeType:    mimeType,
		Bytes:       data,
	})
	switch {
	case err != nil:
		s.note("%s was not stored in the Library: %v", what, err)
	case strings.TrimSpace(stored.Omitted) != "":
		s.note("%s was not stored in the Library: %s", what, stored.Omitted)
	case strings.TrimSpace(stored.FileID) == "":
		s.note("%s was not stored in the Library, which answered no file id", what)
	default:
		return stored.FileID
	}
	return ""
}

// storeLog files the archived log.
func (s *stepFiles) storeLog(ctx context.Context, archivePath string) string {
	data, err := os.ReadFile(archivePath)
	if err != nil {
		s.note("the step's log was not stored in the Library: its archive could not be read: %v", err)
		return ""
	}
	return s.store(ctx, runLogFileName(s.run.StepKey), runLogMimeType, data, "the step's log")
}

// storeArtifacts files each artifact and answers the ids of those kept, in
// order.
func (s *stepFiles) storeArtifacts(ctx context.Context, files []ArtifactFile) []string {
	var ids []string
	for _, f := range files {
		if id := s.store(ctx, runArtifactFileName(f.Path), runFileMimeType(f.Path), f.Bytes, "the artifact "+f.Path); id != "" {
			ids = append(ids, id)
		}
	}
	return ids
}

// extractedArtifacts reads an artifact archive within cap and answers the
// files to store and the declared paths nothing matched. tooLarge is the
// archive past its limits: nothing is kept. Every other problem is a note.
func (s *stepFiles) extractedArtifacts(tgz []byte, declared []string, cap int64) (files []ArtifactFile, missing []string, tooLarge bool) {
	files, missing, err := ExtractArtifacts(tgz, declared, cap)
	var skipped *SkippedEntriesError
	switch {
	case err == nil:
	case errors.As(err, &skipped):
		s.skippedNote(skipped)
	case errors.Is(err, ErrArtifactsTooLarge):
		return nil, nil, true
	default:
		s.note("the step's artifact archive could not be read, so no artifact was stored: %v", err)
		return nil, nil, false
	}
	return files, missing, false
}

// missingNote is one note naming every declared artifact path that matched
// nothing, in declared order.
func (s *stepFiles) missingNote(declared, missing []string) {
	if len(missing) == 0 {
		return
	}
	gone := map[string]bool{}
	for _, m := range missing {
		gone[m] = true
	}
	var named []string
	for _, d := range declared {
		if gone[d] {
			named = append(named, d)
			delete(gone, d)
		}
	}
	for _, m := range missing {
		if gone[m] {
			named = append(named, m)
			delete(gone, m)
		}
	}
	s.note("the declared artifact path(s) %s matched no file in the step's working copy", strings.Join(named, ", "))
}

// skippedNote names the entries an archive's extraction skipped, each name
// masked and then cut to runNoteNameBytes, and the note to runNoteBytes.
func (s *stepFiles) skippedNote(e *SkippedEntriesError) {
	var b strings.Builder
	b.WriteString("artifact entries were skipped: ")
	for i, entry := range e.Entries {
		part := fmt.Sprintf("%q (%s)", clipToRunes(s.mask(entry.Name), runNoteNameBytes), entry.Reason)
		if i > 0 {
			part = ", " + part
		}
		if b.Len()+len(part) > runNoteBytes {
			fmt.Fprintf(&b, ", and %d more", len(e.Entries)-i+e.More)
			s.notes = append(s.notes, pl.Failure{Code: pl.CodeArtifactMissing, Message: b.String()})
			return
		}
		b.WriteString(part)
	}
	if e.More > 0 {
		fmt.Fprintf(&b, ", and %d more", e.More)
	}
	s.notes = append(s.notes, pl.Failure{Code: pl.CodeArtifactMissing, Message: b.String()})
}

// clipToRunes cuts s to at most n bytes, on a rune boundary.
func clipToRunes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := n
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "..."
}
