package pipelinerun

import (
	"context"
	"fmt"
	"strings"

	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/pipelines"
	"github.com/znasllc-io/memql/core/logger"
)

// open.go -- the one way a run comes into existence (decisions 1, 14, 15).
//
// A delivery, the poll and a re-run all arrive here, and here is where they
// become ONE run per (repository, SHA, mode, event) of a pipeline: the dedup
// read and the create are a single critical section under the run key's
// gate, read fresh, so a redelivered webhook, or a webhook and a poll for the
// same head, open exactly one run and exactly one check run (Review Focus 1).
// Only a re-run opens a second attempt of a key.
//
// THE DEDUP IS PER PIPELINE. A repository has one active pipeline (connect
// refuses a second, pipeline_already_connected), but a pipeline disconnected
// and replaced by another source's keeps its runs, and those carry the same
// keys: a predecessor's run of a head is not this pipeline's, and must not
// stop it opening its own.

// Opening is what one cause asks a pipeline to open.
type Opening struct {
	Event pipelines.Event
	// Mode is the event's mode when empty (pipelines.ModeFor); a re-run
	// carries its original's.
	Mode        pipelines.Mode
	SHA         string
	BaseSHA     string
	Branch      string
	Title       string
	PullRequest int
	// Fork is a pull request whose head lives in another repository (D6):
	// opened already refused, with a failing check run, and never queued.
	Fork           bool
	HeadRepository string
	ReleaseTag     string
	// Version is MEMQL_VERSION; empty derives it (pipelines.Version). A
	// re-run carries its original's, which for a release is the tag.
	Version    string
	Trigger    string
	RerunOf    string
	DeliveryID string
}

// OpenResult says what an opening did. Run is the run opened, or the newest
// existing run of the key when nothing was opened.
type OpenResult struct {
	Run    Run
	Opened bool
}

// open opens o on p, or finds the run that already answers it, minting the
// check run's token itself.
func (i *Integration) open(ctx context.Context, d Deps, p Pipeline, o Opening) (OpenResult, error) {
	return i.openWithToken(ctx, d, p, o, "")
}

// openWithToken is open under an installation token its caller already
// minted for p's repository -- the poll's, a release's -- so an opening costs
// no second mint; "" mints one here.
//
// THE ONE CALL TO GITHUB UNDER A GATE IS HERE, and it is the check-run
// CREATE. The dedup read, the check run's create and the run row's create are
// one critical section under the run key's gate, because a check run on
// GitHub cannot be taken back: two openers that each read "no run" and each
// created one would leave two check runs of one name on the commit, visible
// and permanent, where two run rows would merely be a duplicate the derived
// id collapses. And the row is created already naming its check run, so a
// driver claiming the run the moment its created event arrives finds a
// complete row rather than racing the opener to a check run of its own.
// Everything else GitHub is asked -- the token above all, several calls
// through the owner's grant -- is asked BEFORE the gate, which then holds its
// connection for one create, not for a mint. When GitHub refuses the create
// (403) or no token could be had, the run opens anyway with checkRunState
// saying so.
//
// A pull request's new head SUPERSEDES the pull request's earlier runs
// (supersede.go): they are read before the gate, while the new run does not
// exist yet, and asked to stop once the gate is released -- and only when
// GitHub, asked then, names this opening's commit as the pull request's head.
// The head read is a call to GitHub and each ask takes that run's own gate and
// may write its check run, so neither happens under the open gate.
func (i *Integration) openWithToken(ctx context.Context, d Deps, p Pipeline, o Opening, token string) (OpenResult, error) {
	if d.Store == nil {
		return OpenResult{}, errNoStore
	}
	sha := strings.ToLower(strings.TrimSpace(o.SHA))
	if sha == "" {
		return OpenResult{}, fmt.Errorf("pipelines: a run of %s needs a commit, and none was named", p.Repository)
	}
	mode := o.Mode
	if mode == "" {
		m, ok := pipelines.ModeFor(o.Event)
		if !ok {
			return OpenResult{}, fmt.Errorf("pipelines: event %q opens no run", o.Event)
		}
		mode = m
	}
	version := o.Version
	if version == "" {
		version = pipelines.Version(o.Event, sha, o.ReleaseTag)
	}
	key := pipelines.RunKey(p.Repository, sha, mode, o.Event)

	// Asked first, fresh and with no gate held: a redelivery, a retried
	// trigger or a poll that sees a head a webhook already opened is
	// answered by the run that exists, at the cost of a read rather than a
	// mint. The answer can only become MORE true -- runs are never deleted --
	// so a stale "no" costs only the gated read below, which decides.
	keyed, err := d.Store.RunsForKey(memql.ContextWithFreshRead(ctx), key)
	if err != nil {
		return OpenResult{}, err
	}
	if run, ok := answeredBy(runsOf(keyed, p.ID), o); ok {
		return OpenResult{Run: run}, nil
	}

	var tokenErr error
	if token == "" {
		token, tokenErr = checkRunToken(ctx, d, p)
	}
	// Before the run exists, so a run another opener creates meanwhile is
	// never this opening's to stop (supersede.go).
	earlier := supersedable(ctx, d, p, o, mode)

	var result OpenResult
	err = d.gate(ctx, OpenGateKey(key), func(gctx context.Context) error {
		keyed, err := d.Store.RunsForKey(memql.ContextWithFreshRead(gctx), key)
		if err != nil {
			return err
		}
		runs := runsOf(keyed, p.ID)
		if run, ok := answeredBy(runs, o); ok {
			result = OpenResult{Run: run}
			return nil
		}
		newest, attempts := newestAttempt(runs)
		if o.Trigger == TriggerRerun && attempts > 0 && !newest.Finished() {
			result = OpenResult{Run: newest}
			return ErrRunInProgress
		}

		now := d.now()
		run := Run{
			ID:          RunIDFor(p.ID, key, attempts+1),
			OwnerUserID: p.OwnerUserID,
			AccountID:   p.AccountID,
			PipelineID:  p.ID,
			Repository:  p.Repository,
			SHA:         sha,
			Mode:        mode,
			Event:       o.Event,
			RunKey:      key,
			Attempt:     attempts + 1,
			Trigger:     o.Trigger,
			RerunOf:     bareID(o.RerunOf),
			DeliveryID:  strings.TrimSpace(o.DeliveryID),
			PullRequest: o.PullRequest,
			HeadBranch:  strings.TrimSpace(o.Branch),
			BaseSHA:     strings.ToLower(strings.TrimSpace(o.BaseSHA)),
			Title:       strings.TrimSpace(o.Title),
			Version:     version,
			Status:      StatusQueued,
			QueuedAt:    now,
		}
		if o.Fork {
			// D6: a stranger's code is refused, never queued -- and the
			// check run FAILS (decision 15), because neutral would satisfy
			// a required check.
			run.Status = StatusCompleted
			run.Conclusion = ConclusionRefused
			run.RefusalCode = pipelines.CodeForkRefused
			run.RefusalMessage = forkDetail(p, o)
			run.FinishedAt = now
		}

		// The create, and nothing else of GitHub's: see above.
		w := writeCheckRun(gctx, d, p, run, ReportFor(p, run, nil), token, tokenErr)
		run.CheckRunID, run.CheckRunState = w.ID, w.State
		if w.Note != nil {
			run.Notes = withNote(run.Notes, *w.Note)
		}
		if w.Err != nil {
			d.Logger.Warn("pipelines: the run opens without a written check run",
				"component", "pipelinerun", logger.Subject(RunConcept, run.ID), "pipeline", p.ID,
				"repository", p.Repository, "checkRunState", w.State, "error", w.Err)
		}
		if err := d.Store.CreateRun(gctx, run); err != nil {
			return err
		}
		result = OpenResult{Run: run, Opened: true}
		return nil
	})
	if err == nil && result.Opened {
		// The gate is released: only now may GitHub be asked for the pull
		// request's head and a run be asked to stop. A failed mint is no token.
		if tokenErr != nil {
			token = ""
		}
		i.supersede(ctx, d, result.Run, earlier, token)
	}
	return result, err
}

// answeredBy is the existing run of o's key that answers o, when one does:
// for a delivery or the poll, the key's newest attempt -- a redelivery, or a
// poll and a webhook for one head, are one run; for a re-run, only an attempt
// this same delivery already opened -- a re-request delivered twice. A re-run
// that no attempt answers opens the next one.
//
// A FORK'S REFUSAL ANSWERS ONLY A FORK. One head SHA can be a fork's pull
// request and a same-repository one -- the fork's branch pushed here and
// opened again -- and so one run key. When every run of the key is a fork's
// refusal, a same-repository opening opens the next attempt rather than
// being answered by a refusal that ran nothing: otherwise the commit's
// required check would fail for good with nothing ever run. Once any other
// run answers the key, it answers a fork's opening too: the fork's head is
// a commit this repository already runs, and a refused run beside it would
// put a failing check run on the commit next to the real one.
func answeredBy(runs []Run, o Opening) (Run, bool) {
	newest, attempts := newestAttempt(runs)
	if attempts == 0 {
		return Run{}, false
	}
	if o.Trigger != TriggerRerun {
		if !o.Fork && onlyForkRefusals(runs) {
			return Run{}, false
		}
		return newest, true
	}
	if id := strings.TrimSpace(o.DeliveryID); id != "" {
		for _, r := range runs {
			if r.DeliveryID == id {
				return r, true
			}
		}
	}
	return Run{}, false
}

// onlyForkRefusals reports whether every run of a key is a fork's refusal --
// none of them ran anything.
func onlyForkRefusals(runs []Run) bool {
	for _, r := range runs {
		if r.RefusalCode != pipelines.CodeForkRefused {
			return false
		}
	}
	return len(runs) > 0
}

// runsOf is the runs that are pipelineID's -- compared by bare short id,
// because pipelineId is a relationship and reads back canonical.
func runsOf(runs []Run, pipelineID string) []Run {
	var out []Run
	for _, r := range runs {
		if sameID(r.PipelineID, pipelineID) {
			out = append(out, r)
		}
	}
	return out
}

// newestAttempt is the highest attempt of a key's runs and that run; 0 when
// the key has none. A row that carries no attempt counts as the first, so the
// next one opened beside it is never a second attempt 1.
func newestAttempt(runs []Run) (Run, int) {
	var newest Run
	attempts := 0
	for _, r := range runs {
		a := max(r.Attempt, 1)
		if a > attempts || (a == attempts && r.QueuedAt.After(newest.QueuedAt)) {
			newest, attempts = r, a
		}
	}
	return newest, attempts
}

// forkDetail is the refusal a fork's run carries: what is wrong, in the
// person's terms. The remedy is the check run's own line (pipelines'
// refusal copy), so it is not repeated here.
func forkDetail(p Pipeline, o Opening) string {
	pr := "This pull request"
	if o.PullRequest > 0 {
		pr = fmt.Sprintf("Pull request #%d", o.PullRequest)
	}
	head := strings.TrimSpace(o.HeadRepository)
	if head == "" {
		return pr + "'s head repository no longer exists, so nobody can vouch for its code, and it is not run."
	}
	return fmt.Sprintf("%s comes from %s, a repository other than %s. A pull request from a fork runs somebody else's code, and is not run here.", pr, head, p.Repository)
}
