package pipelinesteps

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"mime"
	"os"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/znasllc-io/memql/component/deploycontrol"
	pl "github.com/znasllc-io/memql/component/pipelines"
)

// settle.go -- a decided or cancelled step settled: its log read to the end,
// its files into the owner's Library, its outcome persisted on the Job (split
// out of runner.go).

// ---------------------------------------------------------------------------
// Settling a step
// ---------------------------------------------------------------------------

// decision is a terminal observation as the step's result: its status, exit
// code, failure and times, and nothing else yet.
func (s *step) decision(obs Observation, pod *Pod, job Job) pl.StepResult {
	dec := pl.StepResult{Status: pl.OutcomeSucceeded, ExitCode: obs.ExitCode}
	if obs.Phase == PhaseFailed {
		dec.Status = pl.OutcomeFailed
	}
	if obs.Failure != nil {
		message := obs.Failure.Message
		if obs.Failure.Code == cmp.Or(strings.TrimSpace(s.run.DeadlineCode), pl.CodeStepTimeout) {
			message += s.shortened(job)
		}
		dec.Failure = &pl.Failure{Code: obs.Failure.Code, Message: cutBytes(s.mask(message), failureMaxBytes)}
	}
	dec.StartedAt, dec.FinishedAt = stepTimes(pod)
	if dec.FinishedAt == "" {
		dec.FinishedAt = s.r.nowText()
	}
	return dec
}

// shortened is what a deadline failure adds when the step's Job was given
// less than its timeout, the rest spent before the Job was created (ruling
// R31): the deadline the Job reports is not the one the step declared.
func (s *step) shortened(job Job) string {
	ads, full := job.Spec.ActiveDeadlineSeconds, int64(s.run.TimeoutSeconds)
	if ads == nil || *ads >= full {
		return ""
	}
	return fmt.Sprintf(", what was left of its %s timeout once it had waited %s to start (a wait for a free slot under the pipelines ceiling counts)",
		time.Duration(full)*time.Second, time.Duration(full-*ads)*time.Second)
}

// record writes the decision on the Job, so a runner that adopts the step
// before its outcome is persisted settles it the same way.
func (s *step) record(dec pl.StepResult) {
	body, err := json.Marshal(dec)
	if err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(s.ctx), quickCallTimeout)
	defer cancel()
	if _, err := s.r.kube.AnnotateJob(ctx, s.jobName, map[string]string{AnnotObservation: string(body)}, ""); err != nil {
		s.log.Warn("pipelines: how the step ended could not be recorded on its Job", "error", err)
	}
}

// settle turns a decided step into its outcome: the log read to its end, the
// clone's and -- for a failed step -- the services' last words archived, the
// log and the artifacts stored in the owner's Library, the outcome persisted
// on the Job. A cancel arriving now changes none of it.
//
// Each phase has a deadline of its own, and none can spend another's: a
// Library that does not answer costs the step its files, never the outcome
// recorded on the Job (step 9), which is what answers a reply lost with this
// replica.
func (s *step) settle(dec pl.StepResult, pod *Pod, f *follower) pl.StepResult {
	if pod != nil {
		if f == nil {
			// It ended between two polls: its log is read now.
			f = s.follow(pod.Metadata.Name, true)
		}
		f.drain(s.r.drainTimeout)
	} else if f != nil {
		f.stop()
	}
	// An adopted step that printed nothing after the cursor has had no seam:
	// the notice goes after what was replayed.
	s.noteReattach(!s.replayedHead)
	if pod != nil {
		s.tails(pod.Metadata.Name, dec.Status == pl.OutcomeFailed)
	}

	res := dec
	res.Where = s.where()
	notes := s.newNotes()
	libCtx, cancelLib := s.libraryContext()
	defer cancelLib()
	cr := s.storeLog(libCtx, &res, notes)
	s.storeArtifacts(libCtx, cr, &res, notes)
	s.fitOutcome(&res, notes)
	persistCtx, cancelPersist := s.persistContext()
	defer cancelPersist()
	s.persist(persistCtx, res)
	s.log.Info("pipelines: the step is settled", "status", res.Status, "exitCode", res.ExitCode)
	return res
}

// libraryContext is the deadline the owner's Library is written under: no
// cancel ends it, and no other phase shares it.
func (s *step) libraryContext() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(s.ctx), s.r.libraryTimeout)
}

// persistContext is the window the outcome is recorded on the Job in, retries
// included: no cancel ends it, and nothing spent before it is taken from it.
func (s *step) persistContext() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(s.ctx), persistTimeout)
}

// tails archives the last words of the clone and, when the step failed, of
// each service -- as notes in the archive, never in the store, whose live view
// is the step's own output. The kubelet stops every service after every step,
// so their last words only say something when the step failed.
func (s *step) tails(pod string, failed bool) {
	s.tail(pod, ContainerClone, "the clone", cloneTailLines)
	if !failed {
		return
	}
	for _, name := range sortedKeys(s.run.Services) {
		s.tail(pod, ServicePrefix+name, "service "+name, serviceTailLines)
	}
}

func (s *step) tail(pod, container, who string, lines int) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(s.ctx), quickCallTimeout)
	defer cancel()
	out, err := s.r.kube.TailLog(ctx, pod, container, lines)
	switch {
	case errors.Is(err, ErrContainerNotStarted):
		s.capture.Note(fmt.Sprintf("memql: %s never started, so it left no output", who))
	case err != nil:
		s.capture.Note(fmt.Sprintf("memql: %s's output could not be read: %s", who, apiMessage(err)))
	case strings.TrimSpace(out) != "":
		s.capture.Note(fmt.Sprintf("memql: %s's output, its last %d lines:", who, lines))
		s.capture.Note(out)
	}
}

// fitOutcome keeps the outcome within outcomeMaxBytes, so the API server never
// refuses it as too large (fix round 1, minor 13), and fills in its notes. The
// artifact file ids give way first, from the end -- the files stay in the
// owner's Library all the same, bound to the run and the step -- and the Go
// timings next, by package path; each cut is said in a note at the head of
// the list. The log's file id is never cut. What remains is bounded where it
// is made, and fits.
func (s *step) fitOutcome(res *pl.StepResult, notes *noteList) {
	res.Notes = notes.list()
	if outcomeBytes(*res) <= outcomeMaxBytes {
		return
	}
	if ids := res.ArtifactFileIDs; len(ids) > 0 {
		fits := keepMost(res, notes, len(ids), func(r *pl.StepResult, k int) string {
			r.ArtifactFileIDs = ids[:k:k]
			if k == 0 {
				r.ArtifactFileIDs = nil
			}
			return fmt.Sprintf("%d of the step's %d artifact file ids were left out of its outcome, which must fit in a Job annotation; "+
				"the files are in the owner's Library all the same", len(ids)-k, len(ids))
		})
		if fits {
			return
		}
	}
	if timings := res.Timings; len(timings) > 0 {
		paths := sortedKeys(timings)
		keepMost(res, notes, len(paths), func(r *pl.StepResult, k int) string {
			r.Timings = nil
			if k > 0 {
				r.Timings = make(map[string]float64, k)
				for _, p := range paths[:k] {
					r.Timings[p] = timings[p]
				}
			}
			return fmt.Sprintf("the Go test timings of %d of the step's %d passing packages were left out of its outcome, which must fit in a Job annotation",
				len(paths)-k, len(paths))
		})
	}
}

// keepMost cuts one of the outcome's lists to the most of its n items that
// fit, with the note cut answers at the head of the notes, and says whether
// the outcome fits now. cut keeps the first k items in r. Fewer items are
// never more bytes, so the most that fit is searched for.
func keepMost(res *pl.StepResult, notes *noteList, n int, cut func(r *pl.StepResult, k int) string) bool {
	try := func(k int) (pl.StepResult, noteList, bool) {
		r, trial := *res, *notes
		trial.addFirst(pl.CodeArtifactMissing, cut(&r, k))
		r.Notes = trial.list()
		return r, trial, outcomeBytes(r) <= outcomeMaxBytes
	}
	k := max(sort.Search(n+1, func(k int) bool { _, _, fits := try(k); return !fits })-1, 0)
	r, trial, fits := try(k)
	*res, *notes = r, trial
	return fits
}

// outcomeBytes is the outcome's size as persist encodes it.
func outcomeBytes(res pl.StepResult) int {
	body, err := json.Marshal(res)
	if err != nil {
		return 0 // persist reports it
	}
	return len(body)
}

// persist writes the outcome on the Job, unconditionally, before the Run
// answers: a reply lost now is answered again from the Job. ctx is its own
// window (persistContext): what the step's other phases spent is never taken
// from it.
func (s *step) persist(ctx context.Context, res pl.StepResult) {
	body, err := json.Marshal(res)
	if err != nil {
		s.log.Error("pipelines: the step's outcome cannot be encoded", "error", err)
		return
	}
	for attempt := 1; ; attempt++ {
		_, err = s.r.kube.AnnotateJob(ctx, s.jobName, map[string]string{AnnotOutcome: string(body)}, "")
		if err == nil || deploycontrol.IsNotFound(err) || !transient(err) || attempt >= apiAttempts || !sleepCtx(ctx, s.r.cfg.PollInterval) {
			break
		}
	}
	switch {
	case err == nil:
	case deploycontrol.IsNotFound(err):
		s.log.Info("pipelines: the step's Job was deleted before its outcome was recorded on it")
	default:
		s.log.Error("pipelines: the step's outcome could not be recorded on its Job; a reply lost now cannot be answered again", "error", err)
	}
}

// storeLog closes the capture and stores its archive in the owner's Library,
// filling in the result's log fields.
func (s *step) storeLog(ctx context.Context, res *pl.StepResult, notes *noteList) CaptureResult {
	cr, err := s.capture.Close()
	if err != nil {
		notes.add(pl.CodeArtifactMissing, "the step's log archive could not be written whole, so its Library copy may stop early: "+err.Error())
	}
	res.LogTail, res.LogLines, res.LogCapped = cr.Tail, cr.Lines, cr.StoreCapped
	switch {
	case cr.StoreCapped && s.headLost:
		notes.add(pl.CodeLogCapped, fmt.Sprintf("the live log of this step stops at %d lines; the log archived to the Library "+
			"starts where the node's log did when this replica re-attached to the step", s.r.cfg.LogStoreMaxLines))
	case cr.StoreCapped:
		notes.add(pl.CodeLogCapped, fmt.Sprintf("the live log of this step stops at %d lines; the complete log is archived to the Library", s.r.cfg.LogStoreMaxLines))
	}
	body, err := os.ReadFile(cr.ArchivePath)
	s.removeArchive()
	if err != nil {
		notes.add(pl.CodeArtifactMissing, "the step's log was not stored in the Library: its archive could not be read: "+err.Error())
		return cr
	}
	if s.run.GoTimings {
		s.goTimings(res, body, notes)
	}
	res.LogFileID = s.store(ctx, logFileName(s.run.StepKey), archiveMIME, body, "the step's log", notes)
	return cr
}

// goTimings reads each passing Go package's wall time out of the step's
// archived log (pl.ParseGoTestOutput, the seam's one reader), for a step that
// runs Go tests. A log it cannot read is a note: the timings only steer how
// later runs are sharded, so they never fail a step.
func (s *step) goTimings(res *pl.StepResult, archive []byte, notes *noteList) {
	timings, err := pl.ParseGoTestOutput(bytes.NewReader(archive))
	switch {
	case err != nil:
		notes.add(pl.CodeArtifactMissing, "the step's Go test timings could not be read from its log: "+err.Error())
	case len(timings) > 0:
		res.Timings = timings
	}
}

// storeArtifacts stores the step's artifacts, one Library file each, named by
// their path.
func (s *step) storeArtifacts(ctx context.Context, cr CaptureResult, res *pl.StepResult, notes *noteList) {
	if cr.ArtifactNote != nil {
		s.artifactFact(res, notes, *cr.ArtifactNote)
	}
	if cr.Artifacts == nil {
		return
	}
	files, missing, err := ExtractArtifacts(cr.Artifacts, s.run.Artifacts, s.r.cfg.ArtifactMaxBytes)
	var skipped *SkippedEntriesError
	switch {
	case errors.Is(err, ErrArtifactsTooLarge):
		s.artifactFact(res, notes, pl.Failure{Code: pl.CodeArtifactTooLarge,
			Message: "the step's artifacts were not stored: " + strings.TrimPrefix(err.Error(), pl.CodeArtifactTooLarge+": ")})
		return
	case errors.As(err, &skipped):
		notes.add(pl.CodeArtifactMissing, s.skippedNote(skipped))
	case err != nil:
		notes.add(pl.CodeArtifactMissing, "the step's artifacts were not stored: "+err.Error())
		return
	}
	for _, p := range missing {
		notes.add(pl.CodeArtifactMissing, "the declared artifact path "+s.quote(p)+" matched no file")
	}
	for _, f := range files {
		if id := s.store(ctx, artifactFileName(f.Path), artifactMIME(f.Path), f.Bytes, "the artifact "+s.quote(f.Path), notes); id != "" {
			res.ArtifactFileIDs = append(res.ArtifactFileIDs, id)
		}
	}
}

// artifactFact records what became of the artifacts by the class of its
// code: a note changes nothing; a failure-class code fails the step even when
// its command succeeded -- the command's exit code stays as it was, and an
// earlier typed failure keeps its place.
func (s *step) artifactFact(res *pl.StepResult, notes *noteList, f pl.Failure) {
	if class, _ := pl.ClassOf(f.Code); class != pl.ClassFailure {
		notes.add(f.Code, f.Message)
		return
	}
	res.Status = pl.OutcomeFailed
	if res.Failure == nil {
		res.Failure = &pl.Failure{Code: f.Code, Message: cutBytes(s.mask(f.Message), failureMaxBytes)}
	}
}

// skippedNote names the entries the extractor refused, within a note's
// bounds: the names are the step's, masked, and each cut to
// entryNameMaxBytes.
func (s *step) skippedNote(e *SkippedEntriesError) string {
	total := len(e.Entries) + e.More
	var b strings.Builder
	if total == 1 {
		b.WriteString("1 entry of the step's artifacts was not stored: ")
	} else {
		fmt.Fprintf(&b, "%d entries of the step's artifacts were not stored: ", total)
	}
	const room = len(", and 99999 more")
	listed := 0
	for _, entry := range e.Entries {
		name := s.mask(entry.Name)
		if cut := cutBytes(name, entryNameMaxBytes); cut != name {
			name = cut + "..."
		}
		item := strconv.Quote(name) + " (" + entry.Reason + ")"
		if listed > 0 {
			item = ", " + item
		}
		if b.Len()+len(item)+room > noteMaxBytes {
			break
		}
		b.WriteString(item)
		listed++
	}
	if rest := total - listed; rest > 0 {
		fmt.Fprintf(&b, ", and %d more", rest)
	}
	return b.String()
}

// store stores one file in the owner's Library and answers its id, or "" with
// a note saying why it was not stored: never a failure of the step.
func (s *step) store(ctx context.Context, name, mimeType string, body []byte, what string, notes *noteList) string {
	if s.r.library == nil {
		notes.add(pl.CodeArtifactMissing, what+" was not stored: this workbench node has no Library to store it in")
		return ""
	}
	got, err := s.r.library.StoreRunFile(ctx, RunFile{
		OwnerUserID: s.run.OwnerUserID, WorkRunID: s.run.WorkRunID, StepKey: s.run.StepKey,
		Name: name, MimeType: mimeType, Bytes: body,
	})
	switch {
	case err != nil:
		// The name is the step's, and the Library's error may quote it: the
		// node's log gets them masked, like the note.
		s.log.Warn("pipelines: a step's file could not be stored in the Library", "file", s.mask(name), "error", s.mask(err.Error()))
		notes.add(pl.CodeArtifactMissing, what+" was not stored in the Library: "+err.Error())
	case got.Omitted != "":
		notes.add(pl.CodeArtifactMissing, what+" was not stored in the Library: "+got.Omitted)
	case got.FileID == "":
		notes.add(pl.CodeArtifactMissing, what+" was not stored in the Library: it answered no file id")
	default:
		return got.FileID
	}
	return ""
}

// ---------------------------------------------------------------------------
// Cancelled, failed, refused
// ---------------------------------------------------------------------------

// abandon answers a Run whose context ended: the agent cancelled the step. A
// persisted outcome stands -- a cancel after it changes nothing, and nothing
// rewrites it. Otherwise the Job and its Secret are deleted and the step is
// cancelled, with what this Run captured archived.
func (s *step) abandon(pod *Pod) pl.StepResult {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(s.ctx), quickCallTimeout)
	defer cancel()
	if job, err := s.r.kube.GetJob(ctx, s.jobName); err == nil {
		if out, ok := persisted(job); ok {
			s.discardCapture()
			return out
		}
	}
	if err := s.r.kube.DeleteJob(ctx, s.jobName); err != nil {
		s.log.Warn("pipelines: the cancelled step's Job could not be deleted; its TTL will", "error", err)
	}
	if err := s.r.kube.DeleteSecret(ctx, SecretName(s.jobName)); err != nil {
		s.log.Warn("pipelines: the cancelled step's Secret could not be deleted", "error", err)
	}
	s.log.Info("pipelines: the step was cancelled; its Job and Secret are deleted")
	return s.cancelled(pod, "the step was cancelled")
}

// vanished answers a Run whose Job was deleted under it with no cancel of its
// own: another replica cancelled the run.
func (s *step) vanished(pod *Pod) pl.StepResult {
	s.log.Info("pipelines: the step's Job was deleted under this runner: its run was cancelled elsewhere")
	return s.cancelled(pod, "the step's Job was deleted while it ran: its run was cancelled")
}

func (s *step) cancelled(pod *Pod, why string) pl.StepResult {
	res := pl.StepResult{
		Status: pl.OutcomeCancelled, ExitCode: -1,
		Failure: &pl.Failure{Code: pl.CodeStepCancelled, Message: why},
		Where:   s.where(),
	}
	res.StartedAt, _ = stepTimes(pod)
	res.FinishedAt = s.r.nowText()
	if s.capture != nil {
		s.noteReattach(!s.replayedHead)
		s.capture.Note("memql: " + why)
		notes := s.newNotes()
		ctx, cancel := s.libraryContext()
		s.storeLog(ctx, &res, notes)
		cancel()
		res.Notes = notes.list()
	}
	return res
}

func (s *step) newNotes() *noteList { return &noteList{mask: s.mask} }

// stepTimes are when the step container started and ended, as RFC 3339.
func stepTimes(pod *Pod) (started, finished string) {
	cs := podContainer(pod, false, ContainerStep)
	switch {
	case cs == nil:
	case cs.State.Terminated != nil:
		return rfc3339(cs.State.Terminated.StartedAt), rfc3339(cs.State.Terminated.FinishedAt)
	case cs.State.Running != nil:
		return rfc3339(cs.State.Running.StartedAt), ""
	}
	return "", ""
}

func rfc3339(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

// logFileName is the Library name of a step's archived log: the step key --
// "stage.step" or "stage.step#i" -- with every byte outside [A-Za-z0-9._-]
// made a dash.
func logFileName(stepKey string) string {
	b := []byte(stepKey)
	for i, c := range b {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '.' || c == '_' || c == '-') {
			b[i] = '-'
		}
	}
	return string(b) + ".log"
}

// artifactFileName is an artifact's Library name: its path, every slash a
// double underscore.
func artifactFileName(p string) string { return strings.ReplaceAll(p, "/", "__") }

// artifactMIME is an artifact's content type, by its extension.
func artifactMIME(p string) string {
	if t := mime.TypeByExtension(path.Ext(p)); t != "" {
		return t
	}
	return "application/octet-stream"
}

// cutBytes is s cut to at most n bytes, where a rune starts.
func cutBytes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := n
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}

// noteList collects a result's notes within their bounds: at most maxNotes,
// each at most noteMaxBytes and all together notesMaxBytes; past the bounds
// they are counted in one last note.
type noteList struct {
	mask  func(string) string
	notes []pl.Failure
	bytes int
	more  int
	code  string
}

func (n *noteList) add(code, message string) {
	message = cutBytes(n.mask(message), noteMaxBytes)
	if len(n.notes) >= maxNotes || n.bytes+len(message) > notesMaxBytes {
		if n.more == 0 {
			n.code = code
		}
		n.more++
		return
	}
	n.notes = append(n.notes, pl.Failure{Code: code, Message: message})
	n.bytes += len(message)
}

// addFirst puts a note at the head of the list, within the same bounds: a
// note it pushes off the end is counted with the rest left out.
func (n *noteList) addFirst(code, message string) {
	message = cutBytes(n.mask(message), noteMaxBytes)
	n.notes = append([]pl.Failure{{Code: code, Message: message}}, n.notes...)
	n.bytes += len(message)
	for len(n.notes) > 1 && (len(n.notes) > maxNotes || n.bytes > notesMaxBytes) {
		last := n.notes[len(n.notes)-1]
		n.notes, n.bytes = n.notes[:len(n.notes)-1], n.bytes-len(last.Message)
		if n.more == 0 {
			n.code = last.Code
		}
		n.more++
	}
}

func (n *noteList) list() []pl.Failure {
	if n.more == 0 {
		return n.notes
	}
	more := "1 more note was left out"
	if n.more > 1 {
		more = fmt.Sprintf("%d more notes were left out", n.more)
	}
	return append(n.notes, pl.Failure{Code: n.code, Message: more})
}
