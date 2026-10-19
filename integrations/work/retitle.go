package work

import (
	"context"
	"fmt"
	"unicode/utf8"

	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/core/common"
)

// retitle.go -- retitleRun (design record
// docs/superpowers/specs/2026-10-19-ask-message-routing-design.md, "The goal's
// title").
//
// A person's later message can change what a running goal is about ("actually,
// I'm in Phoenix"), and the goal's card then carries the new title. The title is
// v1:work:run.title, a field of its own: classification.workTitle stays the
// intake decision, because compile writes classification whole when it
// finishes, and a retitle kept inside it would be gone.

// maxRunTitleRunes bounds v1:work:run.title. The title labels a card and names
// the goal on a person's message ("To Weather in Phoenix"), so it is a phrase.
// The limit counts characters, not bytes: a title in any script gets the same
// room.
const maxRunTitleRunes = 80

// handleRetitleRun sets the title a person's later message gave one of their
// runs.
//
// THE CALLER MUST BE A PERSON, not a job. A title change is a reading of what
// somebody just said, so a run that is executing -- or an actor with no person
// behind it -- may not make one, whatever it is told by its own model.
//
// THE RUN MUST BE THE CALLER'S, decided by the owned read and by nothing else,
// before any lock is taken: a person who cannot read a run is refused without
// being able to hold a lock keyed on its id.
func (i *Integration) handleRetitleRun(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	ac, err := requirePrincipal(ctx)
	if err != nil {
		return nil, err
	}
	if run, running := common.RunFromContext(ctx); ac.Synthetic || ac.Unranked || (running && run.RunId != "") {
		return nil, fmt.Errorf("work: a title change needs a person's message, not a running job")
	}
	runId := argString(args, "runId")
	if runId == "" {
		return nil, fmt.Errorf("work: retitleRun needs a runId")
	}
	// argString trims, so a title of only whitespace is empty here.
	title := argString(args, "title")
	if n := utf8.RuneCountInString(title); n == 0 || n > maxRunTitleRunes {
		return nil, fmt.Errorf("work: a run title is 1 to %d characters, not %d", maxRunTitleRunes, n)
	}

	st := i.store()
	run, err := st.runForOwner(ctx, runId)
	if err != nil {
		return nil, err
	}
	if run == nil {
		return nil, fmt.Errorf("work: no run %q is readable by this caller", runId)
	}

	// The row is read, merged and written again, and the replica doing it is not
	// the one that wrote the row last: a lock around the read and the write, a
	// read that skips this node's result cache, and a version stamped after the
	// one that was read (memql#5431), each alone measured insufficient. The key
	// is the run's bare id, so a bare spelling and a canonical one contend for
	// the same lock.
	release, err := i.decisionGate(ctx, "run-title-"+memql.BareShortId(runId))
	if err != nil {
		return nil, err
	}
	defer release()
	ctx = memql.ContextWithFreshRead(ctx)
	run, err = st.runForOwner(ctx, runId)
	if err != nil {
		return nil, err
	}
	if run == nil {
		return nil, fmt.Errorf("work: no run %q is readable by this caller", runId)
	}

	// The write runs AS the owner: the guard ignores the clusterOwner arm, and
	// the value came off the run this caller just read under their own actor, so
	// it can never name somebody they could not act as. It lands on the row's own
	// id, the one that was read, whichever way the caller spelled it.
	owner := rowString(run, "ownerUserId")
	rowId := rowString(run, "id")
	if err := st.updateRun(ownerActor(ctx, owner), rowId, map[string]any{
		"title":       title,
		"versionTime": rfc(workRowVersionAfter(run["createdAt"], i.clock())),
	}); err != nil {
		return nil, err
	}
	return i.resultNode(map[string]any{"runId": memql.BareShortId(rowId), "title": title}), nil
}
