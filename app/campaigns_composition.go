package app

import (
	"context"
	"errors"
	"strings"

	"github.com/znasllc-io/memql/component/campaigns"
	"github.com/znasllc-io/memql/component/memql"
	workspine "github.com/znasllc-io/memql/integrations/work"
)

// campaigns_composition.go -- what starting, cancelling and retrying an email
// composition take from outside component/campaigns (compose_start.go): the
// work runtime's goals and the engine's app doors.
//
//   - The goal: composeEmailRun opened as a direct goal of the caller's, on the
//     Apps route at level strong, chosen by the caller (the route choice a goal
//     carries is the one its owner made: memql.GoalRouteChoice), so it runs on
//     a signed-in app of the caller's own machines whatever route the
//     conversation that asked uses, and never on a local model. requestedVia is
//     email. The precedent is the work runtime's own catalog research
//     (integrations/work/model_catalog_research.go).
//   - The cancel: the work runtime's cancelGoal, the one the builtin of that
//     name runs, as the caller.
//   - The app check: any app door of the caller's that a call can reach from
//     this node -- on a stream this replica holds or through the agent that
//     holds it (AppDoor.Reachable). Not Runnable, which is local-only and so
//     always false on a bff, where a client's call arrives.
//
// EACH IS RESOLVED WHEN A CALL IS MADE, not when it is installed: the campaigns
// worker is built in engineAndBus, before integrationsCore materializes the
// work plug-in, and the app doors are installed later still, in the cluster
// phase. A part that is missing then is refused by name.

const (
	// compositionRoute and compositionLevel are where a composition runs.
	compositionRoute = memql.RouteSourcePrefix + memql.RouteIdApps
	compositionLevel = "strong"

	// compositionAutomation is the template a composition's goal runs
	// (dsl/campaigns/automations.memql).
	compositionAutomation = "composeEmailRun"
)

var errNoCompositionWork = errors.New("this node has no work runtime to open or cancel an email composition's goal")

// compositionGoalOpener opens a goal whose template is known: the work
// integration's OpenDirectGoal.
type compositionGoalOpener interface {
	OpenDirectGoal(ctx context.Context, g workspine.DirectGoal) (goalId, runId string, err error)
}

// compositionAppDoors lists a person's app doors: the provider registry's
// AppDoors.
type compositionAppDoors interface {
	AppDoors(ctx context.Context, actingUserId string) ([]memql.AppDoor, error)
}

// compositionGoalStarter implements campaigns.GoalStarter.
type compositionGoalStarter struct {
	opener func() compositionGoalOpener
}

// StartComposition opens composeEmailRun for the template as a goal of
// g.OwnerUserID's on route:apps at level strong, chosen by that owner.
func (s *compositionGoalStarter) StartComposition(ctx context.Context, g campaigns.CompositionGoal) (string, error) {
	var opener compositionGoalOpener
	if s != nil && s.opener != nil {
		opener = s.opener()
	}
	if opener == nil {
		return "", errNoCompositionWork
	}
	owner := strings.TrimSpace(g.OwnerUserID)
	choice, err := memql.ParseRouteChoice(compositionRoute, compositionLevel)
	if err != nil {
		return "", err
	}
	choice.By = owner
	var accounts []string
	if account := strings.TrimSpace(g.AccountID); account != "" {
		accounts = []string{account}
	}
	goalID, _, err := opener.OpenDirectGoal(memql.ContextWithGoalRouteChoice(ctx, choice), workspine.DirectGoal{
		OwnerUserId:    owner,
		Statement:      g.Statement,
		AutomationName: compositionAutomation,
		RequestedVia:   "email",
		AccountIds:     accounts,
		Input:          map[string]any{"templateId": g.TemplateID},
	})
	return goalID, err
}

// compositionGoalCanceller implements campaigns.GoalCanceller.
type compositionGoalCanceller struct {
	cancel func() procedureCapability
}

// CancelGoal runs the work runtime's cancelGoal for the goal, as the caller.
func (c *compositionGoalCanceller) CancelGoal(ctx context.Context, goalID, reason string) error {
	var cancel procedureCapability
	if c != nil && c.cancel != nil {
		cancel = c.cancel()
	}
	if cancel == nil {
		return errNoCompositionWork
	}
	_, err := cancel(ctx, map[string]any{"goalId": goalID, "reason": reason}, 0)
	return err
}

// compositionAppReachability implements campaigns.AppReachability.
type compositionAppReachability struct {
	doors func() compositionAppDoors
}

// HasReachableApp reports whether any of the person's app doors can be reached
// from this node now. A node with no app doors reaches none.
func (r *compositionAppReachability) HasReachableApp(ctx context.Context, userID string) (bool, error) {
	var doors compositionAppDoors
	if r != nil && r.doors != nil {
		doors = r.doors()
	}
	if doors == nil {
		return false, nil
	}
	list, err := doors.AppDoors(ctx, userID)
	if err != nil {
		return false, err
	}
	for _, door := range list {
		if door.Reachable() {
			return true, nil
		}
	}
	return false, nil
}

// campaignCompositionSeams are the three, each resolving what it needs off
// the app when it is called.
func (a *App) campaignCompositionSeams() (campaigns.GoalStarter, campaigns.GoalCanceller, campaigns.AppReachability) {
	starter := &compositionGoalStarter{opener: func() compositionGoalOpener {
		// Checked before it becomes an interface: a nil *Integration in one
		// is not a nil opener.
		if work := a.lookupWorkIntegration(); work != nil {
			return work
		}
		return nil
	}}
	canceller := &compositionGoalCanceller{cancel: func() procedureCapability {
		return procedureCapabilityByName(a.engine, "work", "cancelGoal")
	}}
	reach := &compositionAppReachability{doors: func() compositionAppDoors {
		if a.engine == nil {
			return nil
		}
		if providers := a.engine.Providers(); providers != nil {
			return providers
		}
		return nil
	}}
	return starter, canceller, reach
}

// wireCampaignComposition installs the composition's seams on the campaigns
// worker, on every node: a client's Write it, Cancel and Retry arrive on a bff,
// and Ask's assistant starts a composition wherever its turn runs.
func (a *App) wireCampaignComposition(worker *campaigns.Worker) {
	if worker == nil {
		return
	}
	worker.SetCompositionGoals(a.campaignCompositionSeams())
}
