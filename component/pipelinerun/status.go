package pipelinerun

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/pipelines"
)

// status.go -- the `pipelines` readiness item's self-report (Task 8 declares
// the module with evaluator "integration:pipelines").
//
// The readiness evaluator calls integration.pipelines.status with
// {probe: false} under the cluster's own evaluation actor and reads ONE entry
// of the envelope -- {name, state, settings[{source}], credentials[{present}]}
// (component/memql/readiness/integration.go): `configured` and `unhealthy`
// configure the module, a touched setting makes it partial, and anything
// else leaves it unconfigured.
//
// WHAT IT CHECKS, none of it a network call:
//
//   - the GitHub App: this cluster has one (from githubconnect's resolution,
//     which the packages client reads at each call);
//   - a repository connected: at least one ACTIVE pipeline, whoever owns it
//     (pipelinesActive, a server-only read of one row: the question is about
//     the cluster, and the person-facing read would answer for whichever
//     owner asked -- under the evaluation actor, nobody);
//   - the runner: this node has a step executor registered
//     (pipelines.CurrentExecutor) -- the substrate's, epic memql#5478. The
//     module is hosted on agent nodes, which is where one registers.
//
// WHAT IT DOES NOT CHECK, stated rather than implied: whether the app holds
// `checks: write`. That is a fact per INSTALLATION, and the evidence is on the
// runs: a 403 records checkRunState "refused" and the
// pipeline_check_permission_missing note on every run it touches, which is
// where the repair is named.

// statusSourceSet and statusSourceUnset are a setting's source in the
// envelope: set or not. The evaluator reads anything but "" and "unset" as a
// setup somebody started.
const (
	statusSourceSet   = "configured"
	statusSourceUnset = "unset"
)

func (i *Integration) handleStatus(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	if err := statusAuthorized(ctx); err != nil {
		return nil, err
	}
	report := i.Status(ctx)
	raw, err := json.Marshal(map[string]any{
		"checkedAt":    time.Now().UTC().Format(time.RFC3339),
		"probed":       boolArg(args, "probe"),
		"integrations": []StatusReport{report},
	})
	if err != nil {
		return nil, fmt.Errorf("pipelines.status: %w", err)
	}
	return []memorynodes.MemoryNode{{
		ID:        "integrationStatus",
		Concept:   "integration:pipelines:status",
		Type:      memorynodes.NodeTypeObject,
		CreatedAt: time.Now().UTC(),
		Payload:   raw,
	}}, nil
}

// StatusReport is the pipelines entry of the integration-status envelope.
type StatusReport struct {
	Name        string          `json:"name"`
	State       string          `json:"state"`
	Detail      string          `json:"detail"`
	Settings    []StatusSetting `json:"settings"`
	Credentials []StatusSetting `json:"credentials"`
}

// StatusSetting is one setting or credential slot: its name, where it stands,
// and -- for a credential -- whether a value is present. Never the value.
type StatusSetting struct {
	Name    string `json:"name"`
	Source  string `json:"source,omitempty"`
	Present bool   `json:"present"`
	Purpose string `json:"purpose"`
}

// Status reports this node's pipelines setup: configured when the cluster has
// a GitHub App, a repository is connected and this node has a runner; what is
// missing, in words, otherwise.
func (i *Integration) Status(ctx context.Context) StatusReport {
	d := i.snapshot()
	app := d.GitHub != nil && d.GitHub.Configured()
	runner := pipelines.CurrentExecutor() != nil
	connected := false
	if d.Store != nil {
		active, err := d.Store.PipelinesActive(ctx)
		if err != nil {
			// Unknown is not connected: the report must not call the module
			// set up on a read that did not answer.
			d.Logger.Warn("pipelines: the readiness report could not read whether any repository is connected",
				"component", "pipelinerun", "error", err)
		} else {
			connected = len(active) > 0
		}
	}

	setting := func(name string, set bool, purpose string) StatusSetting {
		s := StatusSetting{Name: name, Source: statusSourceUnset, Purpose: purpose}
		if set {
			s.Source, s.Present = statusSourceSet, true
		}
		return s
	}
	report := StatusReport{
		Name: IntegrationName,
		Settings: []StatusSetting{
			setting("githubApp", app, "The GitHub App pipelines read repositories and report check runs through."),
			setting("repository", connected, "A repository connected: at least one source's pipeline, active, whose checks this cluster runs."),
			setting("runner", runner, "The step runner registered on this node, which executes a pipeline's steps."),
		},
		Credentials: []StatusSetting{},
	}
	if app && connected && runner {
		report.State = "configured"
		report.Detail = "This cluster has a GitHub App and a connected repository, and this node can run steps. Check runs need the app's checks: write permission on each installation; a run that could not write one says so."
		return report
	}
	var missing []string
	if !app {
		missing = append(missing, "this cluster has no GitHub App, so no pipeline can be connected or report a check run")
	}
	if !connected {
		missing = append(missing, "no repository is connected: connect a source's pipeline to run its checks")
	}
	if !runner {
		missing = append(missing, "this node has no step runner, so a run's command steps fail pipeline_runner_unavailable")
	}
	report.State = "needs_configuration"
	report.Detail = "Not set up yet: " + strings.Join(missing, "; ") + "."
	return report
}

// statusAuthorized fails closed: no caller is nobody. The report carries no
// secret, but it describes the deployment's shape, so it is for an owner,
// developer or admin -- and for the readiness evaluator, which asks as the
// cluster's own owner-role actor.
func statusAuthorized(ctx context.Context) error {
	ac, ok := auth.AccessFromContext(ctx)
	if !ok || ac == nil {
		return fmt.Errorf("pipelines.status: no authenticated caller")
	}
	switch ac.Role {
	case auth.RoleOwner, auth.RoleDeveloper, auth.RoleAdmin:
		return nil
	}
	return fmt.Errorf("pipelines.status: role %q may not read integration configuration (owner, developer or admin required)", string(ac.Role))
}
