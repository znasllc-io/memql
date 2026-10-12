package app

import (
	"context"
	"errors"

	"github.com/znasllc-io/memql/component/database"
)

// campaignRecipientDigestSweep is the sweep the campaigns worker runs after a
// rollout (memql#6022, F9): it puts the address digest on recipients written
// without one, so the signup and the email rules stop having to compare addresses
// for them. The pass is database.BackfillRecipientEmailDigests; the worker owns
// the schedule (component/campaigns/digest_sweep.go), the application owns the
// database handle.
//
// A NAMED method, for the reason campaignEmailSender is one: an anonymous function
// inside engineAndBus would renumber every later `engineAndBus$N` in the
// architecture model.
//
// The handle is resolved per pass: the worker is wired before the database phase
// has necessarily finished, and a pass that finds no handle yet is skipped and
// tried again an hour on.
func (a *App) campaignRecipientDigestSweep(ctx context.Context) (int, error) {
	if a.db == nil {
		return 0, errors.New("the database component is not wired")
	}
	bunDB := a.db.BunDB()
	if bunDB == nil {
		return 0, errors.New("the database is not connected")
	}
	return database.BackfillRecipientEmailDigests(ctx, bunDB, a.Logger)
}
