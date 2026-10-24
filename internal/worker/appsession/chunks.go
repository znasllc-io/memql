package appsession

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"

	"github.com/znasllc-io/memql-cockpit/internal/worker/harness"
)

// chunks.go owns everything that leaves the session: the ordered chunks,
// the transcript that becomes an artifact, the usage the app reported
// about itself, and the single End.

// chunkSendAttempts is how many times one chunk is retried before the
// session gives up on the stream.
const chunkSendAttempts = 3

// chunkRetryDelay is the pause between those attempts.
const chunkRetryDelay = 200 * time.Millisecond

// openTranscript starts the on-disk transcript.
//
// It goes to a file rather than a buffer because a run can be an hour
// long and its output is not bounded by anything the cockpit controls;
// holding all of it in memory would make a chatty agent an OOM on
// somebody's laptop. It lives inside the session scaffolding, so it is
// excluded from the produced-file sweep and removed with the session.
func (s *session) openTranscript(workspace string) error {
	dir := filepath.Join(workspace, sessionScaffoldDir)
	if err := os.MkdirAll(dir, configDirMode); err != nil {
		return fmt.Errorf("app session: transcript: %w", err)
	}
	// Sessions of one run share their working files, but may overlap during
	// recovery or helper inference. A fixed name truncates another transcript
	// and lets its cleanup unlink this session's still-open file. Allocate an
	// exclusive private file; its actual path stays bound to this handle until
	// successful retention. A failed push keeps these exact bytes for recovery.
	f, err := os.CreateTemp(dir, "transcript-*.log")
	if err != nil {
		return fmt.Errorf("app session: transcript: %w", err)
	}
	s.mu.Lock()
	s.transcript = f
	s.mu.Unlock()
	return nil
}

// emitChunk records one piece of output and sends it.
//
// THE CLASSIFICATION IS THE HARNESS'S. What used to stand here was a
// "does this line parse as JSON" test over raw stdout, which was a
// parser for a format nobody promised to keep; the harness reads each
// app's own protocol and hands over a chunk already labelled `event`,
// `text`, `tool`, `stdout` or `stderr`. Reclassifying anything here
// would put a second opinion in front of the first one.
//
// The seq is taken ONCE, before the first send attempt, and reused on
// every retry. The engine drops out-of-order and duplicate chunks rather
// than appending them, so renumbering a retry does not "fix" anything --
// it produces a gap the reader cannot see and a record that no longer
// matches what the app printed.
//
// Narration is subject to limits.max_transcript_bytes, and this is the
// path narration takes. The one exemption is the recording (emitRecord).
// The structured answer used to be the other, while AppSessionEnd had no
// field for it; it rides result_json on the End now, which no transcript
// cap touches.
func (s *session) emitChunk(stream string, data []byte) error {
	return s.emit(stream, data, true)
}

// emitRecord sends one chunk of the RECORDING -- an action or the
// fingerprint (record.go, fingerprint.go) -- as an `event` chunk that
// limits.max_transcript_bytes never swallows.
//
// The limit bounds the NARRATION the engine keeps on the session row. The
// recording is what the app DID: dropping the fortieth call because the
// app was chatty about the first thirty-nine would record a session that
// stopped doing things halfway through, in the one record a later replay
// reads. It is still redacted, still written to the transcript artifact,
// and still numbered once and retried under that number.
func (s *session) emitRecord(data []byte) error {
	return s.emit(StreamEvent, data, false)
}

// emit is the one send path emitChunk and emitRecord share; capped says
// whether limits.max_transcript_bytes applies.
//
// ONE CHUNK AT A TIME, numbered and sent under sendMu -- the transcript
// line included, so the artifact reads in the order the stream does. The
// cost is that a send retrying holds the others back, which is the point:
// a chunk that overtook it would make the engine drop it.
func (s *session) emit(stream string, data []byte, capped bool) error {
	if len(data) == 0 {
		return nil
	}
	clean := s.redact.apply(data)

	s.sendMu.Lock()
	defer s.sendMu.Unlock()

	// The transcript artifact carries the FULL output, whatever the
	// engine's row-level cap is. That is the division of labour the
	// engine documents: it keeps a bounded transcript and marks it
	// truncated, and the complete one is expected to be an artifact.
	s.mu.Lock()
	transcript := s.transcript
	s.mu.Unlock()
	if transcript != nil {
		_, _ = fmt.Fprintf(transcript, "[%s] %s", stream, clean)
		if len(clean) > 0 && clean[len(clean)-1] != '\n' {
			_, _ = transcript.Write([]byte("\n"))
		}
	}

	if capped && s.transcriptCapReached(int64(len(clean))) {
		return nil
	}

	s.seqMu.Lock()
	s.seq++
	seq := s.seq
	s.seqMu.Unlock()

	var lastErr error
	for attempt := range chunkSendAttempts {
		if err := s.sender.SendAppSessionChunk(s.id, stream, clean, seq); err != nil {
			lastErr = err
			if attempt+1 < chunkSendAttempts {
				time.Sleep(chunkRetryDelay)
			}
			continue
		}
		return nil
	}
	return lastErr
}

// transcriptCapReached applies limits.max_transcript_bytes to what is
// STREAMED, and says so once when it bites.
//
// The limit bounds what the engine will keep on the session row, so
// sending past it spends bandwidth on bytes that get dropped. Stopping
// silently, though, would leave a reader believing the run went quiet --
// so the cap emits one notice naming itself and pointing at the artifact
// that does have the rest. The caller holds sendMu, so the notice is sent
// here directly and in its turn, never through emit.
func (s *session) transcriptCapReached(n int64) bool {
	max := s.start.GetLimits().GetMaxTranscriptBytes()
	if max <= 0 {
		return false
	}
	s.seqMu.Lock()
	if s.capped {
		s.seqMu.Unlock()
		return true
	}
	s.streamed += n
	if s.streamed <= max {
		s.seqMu.Unlock()
		return false
	}
	s.capped = true
	s.seq++
	seq := s.seq
	s.seqMu.Unlock()

	notice := fmt.Sprintf("[memql] live transcript truncated at limits.max_transcript_bytes (%d); "+
		"the full transcript is pushed to the Library as an artifact at the end of this session\n", max)
	_ = s.sender.SendAppSessionChunk(s.id, StreamStderr, []byte(notice), seq)
	return true
}

// recordTurn folds one turn's result into what the End will carry.
//
// Called once per turn, in the loop, before the next one starts, so
// there is no interleaving to reason about beyond the lock.
func (s *session) recordTurn(res harness.TurnResult) {
	s.usageMu.Lock()
	defer s.usageMu.Unlock()

	if ref := strings.TrimSpace(res.AppSessionRef); ref != "" {
		// The app's own session id, so a later kind=attach resumes THIS
		// conversation. A harness that lost it would have quietly turned
		// a conversation into a series of strangers.
		s.appRef = ref
	}

	// The LAST turn's answer, replacing rather than merging. A follow-up
	// that answered in prose must not inherit the previous turn's
	// structured result: the End describes the turn that just ended, and
	// carrying an older answer forward would report an answer to a
	// question nobody asked.
	s.result = res.ResultJSON

	// What the app SAID served it (design D9), as a PAIR from the last
	// turn that named a model. A turn that said nothing -- one that died
	// before its result, say -- does not erase what an earlier turn said,
	// because silence is not a report that the model changed. But a model
	// is never joined to an effort from a different turn: a later turn that
	// names only a model reports that model at an effort nobody stated.
	if m := strings.TrimSpace(res.Model); m != "" {
		s.servedModel = m
		s.servedEffort = strings.TrimSpace(res.Effort)
	}

	if !res.Usage.Known {
		s.usageGaps++
		return
	}
	if s.usage == nil {
		s.usage = &memqlv1.AppSessionUsage{}
	}
	// SUMMED across turns, because the End reports the session and the
	// session is every turn in it. Taking only the last one would
	// under-report a five-turn conversation by four turns, in a ledger
	// somebody bills from.
	s.usage.InputTokens += res.Usage.InputTokens
	s.usage.OutputTokens += res.Usage.OutputTokens
	s.usage.CostUsd += res.Usage.CostUSD
}

// reportedUsage returns what the app told us, or an explicit
// known=false.
//
// The zero-with-known=false case is the important one and is built
// deliberately rather than left nil: it is the difference between "this
// run was free" and "nobody knows what this run cost", and only one of
// those is true.
//
// ONE SILENT TURN MAKES THE WHOLE SESSION UNKNOWN. A sum missing a turn
// is not a smaller measurement, it is an under-measurement recorded as a
// measurement -- the same failure as an estimate, arrived at politely.
// The counts still travel, because they cost nothing and the engine
// ignores them when known is false.
func (s *session) reportedUsage() *memqlv1.AppSessionUsage {
	s.usageMu.Lock()
	defer s.usageMu.Unlock()
	if s.usage == nil {
		return &memqlv1.AppSessionUsage{Known: false}
	}
	return &memqlv1.AppSessionUsage{
		InputTokens:  s.usage.InputTokens,
		OutputTokens: s.usage.OutputTokens,
		CostUsd:      s.usage.CostUsd,
		Known:        s.usageGaps == 0,
	}
}

// pushOutputs uploads what the run produced plus the full transcript, and
// returns the artifact ids.
func (s *session) pushOutputs(ctx context.Context) ([]string, error) {
	s.mu.Lock()
	library, transcript, before, workspace, written := s.library, s.transcript, s.before, s.workspace, s.written
	s.transcript = nil
	s.mu.Unlock()

	transcriptPath := ""
	if transcript != nil {
		transcriptPath = transcript.Name()
		_ = transcript.Close()
	}
	if library == nil {
		return nil, nil
	}

	// The session's own context is already cancelled by the time this
	// runs on the cancel path, and the outputs of a cancelled run are
	// still worth keeping -- an hour of somebody's subscription went
	// into them. So the push gets its own bounded context.
	pushCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), libraryTimeout)
	defer cancel()

	var ids []string
	var failures []string
	manifest := outputManifest{Version: 2, SessionID: s.id}

	// The RESOLVED workspace, not the start's: a start that named none ran
	// in a directory this machine chose, and what the app made there is
	// output all the same.
	if workspace != "" && before != nil {
		produced, skipped := collectProduced(workspace, before, written)
		manifest.Omitted = append(manifest.Omitted, skipped...)
		if len(skipped) > 0 {
			failures = append(failures, "some output candidates were not retained; see the output manifest")
		}
		for _, note := range skipped {
			// Never a silent truncation: a bounded list that reads as
			// complete is worse than a shorter one that says so.
			s.logger.Warn("app session output not pushed", "file", note)
			_ = s.emitChunk(StreamStderr, []byte("[memql] not pushed to the Library: "+note+"\n"))
		}
		producedIDs, pushFailures := s.pushProduced(pushCtx, library, produced, &manifest)
		ids = append(ids, producedIDs...)
		failures = append(failures, pushFailures...)
	}

	if transcriptPath != "" {
		name := "app-session-" + sanitizeLedgerName(s.id) + "-transcript.log"
		if id, pushErr := library.Push(pushCtx, transcriptPath, name); pushErr != nil {
			failures = append(failures, pushErr.Error())
		} else {
			ids = append(ids, id)
			manifest.TranscriptID = id
			_ = os.Remove(transcriptPath)
			_ = os.Remove(filepath.Dir(transcriptPath))
		}
	}
	if len(manifest.Files) > 0 || len(manifest.Omitted) > 0 || len(failures) > 0 {
		manifest.Failures = append([]string(nil), failures...)
		body, err := json.Marshal(manifest)
		if err == nil {
			var id string
			id, err = library.PushBytes(pushCtx, s.redact.apply(body), "app-session-"+sanitizeLedgerName(s.id)+"-outputs.json")
			if err == nil {
				ids = append(ids, id)
			}
		}
		if err != nil {
			failures = append(failures, "output manifest: "+err.Error())
		}
	}

	if len(failures) > 0 {
		return ids, fmt.Errorf("app session: pushing outputs to the Library failed: %s",
			strings.Join(failures, "; "))
	}
	return ids, nil
}

// The artifact list alone cannot establish completeness. Keep the selection
// and transfer receipt in MemQL beside the journal, including explicit gaps.
type outputManifest struct {
	Version      int              `json:"version"`
	SessionID    string           `json:"sessionId"`
	TranscriptID string           `json:"transcriptArtifactId,omitempty"`
	Archives     []outputArchive  `json:"archives,omitempty"`
	Files        []outputArtifact `json:"files,omitempty"`
	Omitted      []string         `json:"omitted,omitempty"`
	Failures     []string         `json:"failures,omitempty"`
}
type outputArtifact struct {
	Path         string `json:"path"`
	Bytes        int64  `json:"bytes"`
	ArtifactID   string `json:"artifactId"`
	SHA256       string `json:"sha256"`
	ArchiveEntry string `json:"archiveEntry,omitempty"`
}
type outputArchive struct {
	ArtifactID string `json:"artifactId"`
	Bytes      int64  `json:"bytes"`
	SHA256     string `json:"sha256"`
}

// sendEnd closes the session on the wire, exactly once.
//
// exit_code is the app's REAL code. The engine reads a non-zero exit as a
// FAILED run rather than an ended one, so normalising a 2 to a 1 -- or
// worse, to a 0 -- misfiles the outcome in a record that is read back
// later by people deciding whether the thing worked.
//
// result_json is the LAST turn's structured answer, set whatever the exit
// code says: a harness can answer the schema and still exit non-zero, and
// the proto keeps the answer apart from the failure so the one part of a
// failed run we can read is not thrown away with it. No answer is an
// empty field, never a synthesised `{}` -- that would read downstream as
// "the app answered nothing", which bills and retries differently from
// "the app answered in prose".
//
// model and effort are what the APP REPORTED serving (see recordTurn),
// never the level's knobs: the engine records them as the model and effort
// that SERVED, and a copy of the request would record as measured something
// nobody measured. Empty is the app saying nothing, which the engine files
// as unknown.
func (s *session) sendEnd(code int, message string, artifacts []string) {
	s.usageMu.Lock()
	ref := s.appRef
	result := s.result
	model, effort := s.servedModel, s.servedEffort
	s.usageMu.Unlock()
	if strings.TrimSpace(ref) == "" {
		ref = s.start.GetAppSessionRef()
	}

	end := &memqlv1.AppSessionEnd{
		SessionId:           s.id,
		ExitCode:            int32(code),
		Usage:               s.reportedUsage(),
		AppSessionRef:       ref,
		ProducedArtifactIds: artifacts,
		Error:               s.redact.apply2(message),
		ResultJson:          string(result),
		Model:               model,
		Effort:              effort,
	}
	if err := s.sender.SendAppSessionEnd(end); err != nil {
		s.logger.Warn("app session end could not be sent", "error", err, "session_error", end.GetError())
		return
	}
	attrs := []any{
		"exit_code", code,
		"served_model", model,
		"served_effort", effort,
		"artifacts", len(artifacts),
		"usage_known", end.GetUsage().GetKnown(),
	}
	if end.GetError() == "" {
		s.logger.Info("app session ended", attrs...)
		return
	}
	// THE REASON, not the fact of one. The End carries it to the engine,
	// but whoever is debugging THIS machine reads its log, and a line
	// saying only that there was an error sends them to a cluster they may
	// not be able to query. Redacted exactly as the End is -- it is the
	// End's own text.
	s.logger.Warn("app session ended with an error", append(attrs, "error", end.GetError())...)
}
