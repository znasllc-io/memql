// repair.go implements Repair, the cluster-side repair verb on
// DeployControlService (memql#4209).
//
// # What a repair IS here, and what it is not
//
// The vision names update / repair / restore. Update and restore were covered
// (Deploy / Rollback / RollbackDeployment / RolloutAction); repair existed only
// as the VS Code extension's capability-script flow -- `cli.js repair`, which
// re-runs the INSTALL graph with the parameters the install receipt recorded
// (editors/vscode/src/install, scripts/install/graph/install.json). Read that
// graph and most of it is the OPERATOR'S MACHINE: place the pinned k3d /
// kubectl / mkcert binaries, edit the hosts file under sudo, install the local
// CA into the browser trust store, clone the stack into the pinned source
// directory, `k3d cluster create` against the host's Docker daemon, seed the
// bootstrap secrets from a key FILE on the operator's disk. None of that is
// reachable from a pod -- the identity node has no Docker socket, no sudo, no
// hosts file, no receipt, and the cluster it would recreate is the one it runs
// in. So the install graph is NOT what this verb runs, and it could not be.
//
// What the graph's cluster half does, once the cluster exists, is "apply the
// ArgoCD Application and wait for the workloads to become ready"
// (scripts/k3d/up.sh: apply_argocd_app + wait_for_workloads). THAT half has a
// cluster-side form, and it is the same form on both providers because the
// local cluster and the cloud reconcile through one Application on one
// ArgoCD (environment parity): ask ArgoCD to hard-refresh the manifests and
// run a sync with prune, then watch the Application until it reports synced
// AND healthy. It is the command deploy/argocd/README.md documents for an
// operator-triggered sync (`kubectl -n argocd patch app memql --type merge -p
// '{"operation":{"sync":{}}}'`), issued through the Executor from the identity
// node rather than by a person at a terminal. On the cloud the Application is
// on MANUAL sync, so this is the only thing that re-applies drift at all; on
// the local cluster it is the immediate form of the selfHeal the Application
// already carries. Nothing here changes version -- a repair that installs a
// different version is an upgrade wearing a repair's name (memql#3605).
//
// # Provider
//
// The provider is read off the concepts -- the v1:cluster:cluster row first
// (the installation's own statement of where it runs), then the newest
// v1:cluster:deployment record -- never off an environment branch (epic
// memql#3943). Both known providers, docker-local and azure, reconcile through
// the Application, so both have the repair above; an empty provider is the
// package's "nobody said" and takes the same default (azure) every other
// record here takes. Anything else has NO defined repair: the engine cannot
// claim the Application is that topology's reconciliation path, so the call
// is refused inside the ActionResult with the locked message
// "repair is not defined for this provider" (RepairUndefinedError) -- audited
// as a failure, with the audit id on the result on both transports -- rather
// than half-run.
//
// # Honest progress
//
// ok=true means accepted AND kicked off, not "repaired". The RPC writes a
// v1:cluster:deployment record at in_progress BEFORE the kick-off (notes
// prefixed repairNotePrefix, carrying the version in force), so the timeline
// the portal already renders carries the repair from its first instant; a
// kick-off that fails transitions that record to failed in the same call.
// After a successful kick-off a bounded watcher on this node observes the
// Application through the Executor until the operation it stamped
// (repairOperationInfoName = the record's id) has run and the Application is
// synced AND healthy -- succeeded -- or the operation failed, the Application
// went Degraded, the reads kept failing, or the ceiling passed -- failed. The
// terminal status is therefore always an OBSERVATION; nothing here asserts
// success from an exit code.
//
// The watcher lives here rather than in the deploy pack's automations because
// those observe a single instant on the in_progress edge and only for the
// azure provider (examples/deploypack/dsl/logic.memql); a repair on a local
// cluster would strand at in_progress forever, which is exactly the invented
// progress this verb must not produce. The record is created by an insert,
// which emits graph.node.created and not the graph.node.updated edge the pack
// triggers on, so the pack and this watcher never contend for one record.
package deploycontrol

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	"github.com/znasllc-io/memql/core/id"
)

const (
	// repairNotePrefix marks a v1:cluster:deployment record as a repair. The
	// concept has no `kind` field and adding one re-keys the generated SDKs;
	// the notes field is free-form operator text, and the prefix is the one
	// convention a timeline reader needs to tell a repair from a deploy.
	repairNotePrefix = "repair:"

	// repairUndefinedMessage is the locked refusal text for a provider with no
	// defined repair (the issue's own wording).
	repairUndefinedMessage = "repair is not defined for this provider"

	// repairOperationInfoName is the name of the ArgoCD Operation.info entry a
	// repair stamps on the sync it triggers; the value is the repair record's
	// deploymentId. ArgoCD echoes the executed Operation on
	// status.operationState, which is what lets the watcher tell this sync
	// apart from the Application's previous one.
	repairOperationInfoName = "memql.io/repair"

	// repairInitiator is the ArgoCD Operation.initiatedBy.username the sync
	// carries, so an operator reading the Application's history can see the
	// console, not a person at a terminal, asked for it.
	repairInitiator = "memql-deploy-console"

	defaultRepairPollInterval = 5 * time.Second
	defaultRepairCeiling      = 15 * time.Minute

	// repairObservationFailureBudget is how many CONSECUTIVE failed reads of
	// the Application the watcher tolerates before resolving the record to
	// failed. One failure is a blip; five in a row over the poll interval is a
	// kubectl that no longer works.
	repairObservationFailureBudget = 5
)

// Result-detail reasons a refused or failed repair carries on
// ActionResult.details["reason"], so a caller can branch without parsing the
// message.
const (
	repairReasonUndefinedProvider = "repair_undefined_for_provider"
	repairReasonAlreadyRunning    = "repair_in_progress"
	repairReasonSyncInProgress    = "sync_in_progress"
	repairReasonKickoffFailed     = "kickoff_failed"
	repairReasonRecordFailed      = "record_failed"
)

// repairProviders is the closed set of providers with a defined repair. Both
// reconcile through the one ArgoCD Application; the empty string is the
// package-wide "nobody said" that every other record here reads as azure.
var repairProviders = map[string]bool{"": true, "azure": true, "docker-local": true}

// RepairUndefinedError is the typed refusal for a provider with no defined
// repair. It is returned INSIDE the ActionResult (ok=false, audited as a
// failure, the audit id on the result) rather than as a gRPC status, matching
// how Deploy reports a provider it cannot ship to; errors.As recovers it from
// the failure reason a test or a log reader holds.
type RepairUndefinedError struct {
	Provider string
}

func (e *RepairUndefinedError) Error() string {
	return fmt.Sprintf("%s: this installation reports provider %q, and a repair is defined only for "+
		"docker-local and azure (both reconcile through ArgoCD application %q)",
		repairUndefinedMessage, e.Provider, argoApplication)
}

// repairDefinedForProvider reports whether Repair has a defined effect for
// the installation's provider.
func repairDefinedForProvider(provider string) bool {
	return repairProviders[strings.TrimSpace(provider)]
}

// repairSyncPatch renders the merge patch RunRepair applies to the
// Application: an explicit sync Operation with prune, attributed to the
// console and stamped with the repair record's id. It is what the README's
// `-p '{"operation":{"sync":{}}}'` becomes with provenance attached; the
// shape is ArgoCD's own Operation type.
func repairSyncPatch(marker string) (string, error) {
	marker = strings.TrimSpace(marker)
	if marker == "" {
		return "", fmt.Errorf("repair sync patch: marker (the repair record id) is required")
	}
	patch := map[string]any{
		"operation": map[string]any{
			"initiatedBy": map[string]any{"username": repairInitiator, "automated": false},
			"info":        []map[string]string{{"name": repairOperationInfoName, "value": marker}},
			"sync":        map[string]any{"prune": true},
		},
	}
	raw, err := json.Marshal(patch)
	if err != nil {
		return "", fmt.Errorf("repair sync patch: %w", err)
	}
	return string(raw), nil
}

// -----------------------------------------------------------------------------
// The RPC
// -----------------------------------------------------------------------------

// Repair re-converges this installation onto its committed overlay
// (memql#4209): see the file comment for what that is and is not.
//
// Owner-only (authorizeOwner) -- the RollbackDeployment floor, not Deploy's:
// a repair mutates a running cluster with no Git revert trail. The gate runs
// FIRST; there is no argument to validate after it (RepairRequest is empty).
// Every refusal after the gate -- undefined provider, a repair already in
// flight, a sync already running on the Application, a kick-off that failed
// -- is an ActionResult with ok=false and a reason, audited as a failure with
// its id on the result, so the caller can quote it on either transport.
func (s *Service) Repair(ctx context.Context, _ *memqlv1.RepairRequest) (*memqlv1.ActionResult, error) {
	detail := map[string]any{"application": argoApplication}
	act, err := s.authorizeOwner(ctx, "repair", detail)
	if err != nil {
		return nil, err
	}

	provider, providerSource := s.resolveInstallationProvider(ctx)
	detail["provider"] = provider
	detail["providerSource"] = providerSource
	if !repairDefinedForProvider(provider) {
		return s.finishWrite(ctx, "repair", act, detail, "", &RepairUndefinedError{Provider: provider},
			map[string]string{"reason": repairReasonUndefinedProvider, "provider": provider}), nil
	}
	if provider == "" {
		// The package-wide default for a record with no stamped provider
		// (deploymentProvider), so the repair record reads like its neighbours.
		provider = deploymentProvider()
	}

	// One repair per node at a time. The guard is held from here until the
	// watcher resolves the record, so two presses of the button do not mint
	// two records chasing one sync.
	if !s.acquireRepair() {
		active := s.activeRepair()
		return s.finishWrite(ctx, "repair", act, detail, "",
			fmt.Errorf("a repair is already in progress on this node (deployment %s); poll its record for the outcome", active),
			map[string]string{"reason": repairReasonAlreadyRunning, "deploymentId": active}), nil
	}

	// Cross-replica guard: the Application itself says whether an operation
	// is already running, whichever node (or person) started it. A read
	// failure is NOT a refusal -- the kick-off below fails with the same
	// cause, which is the honest error to report.
	if obs, oerr := s.observeRepair(ctx); oerr == nil && obs.operationRunning() {
		s.releaseRepair()
		return s.finishWrite(ctx, "repair", act, detail, "",
			fmt.Errorf("a sync operation is already %s on application %s; wait for it to finish (poll GetDeploymentStatus)",
				obs.phase, argoApplication),
			map[string]string{"reason": repairReasonSyncInProgress, "phase": obs.phase}), nil
	}

	// The record FIRST, so the timeline carries the repair from its first
	// instant and a failed kick-off has somewhere to land.
	version, digest := s.repairTargetVersion(ctx)
	detail["version"] = version
	deploymentID, createErr := s.createRepairDeployment(ctx, version, digest, provider, act)
	if createErr != nil {
		s.releaseRepair()
		return s.finishWrite(ctx, "repair", act, detail, "", createErr,
			map[string]string{"reason": repairReasonRecordFailed, "provider": provider}), nil
	}
	detail["deploymentId"] = deploymentID
	s.setActiveRepair(deploymentID)

	out, runErr := s.exec.RunRepair(ctx, deploymentID)
	if runErr != nil {
		// The sync never started, so the record's honest terminal state is
		// known now rather than after a watcher timed out waiting for it.
		s.transitionDeployment(ctx, deploymentID, "failed")
		s.releaseRepair()
		return s.finishWrite(ctx, "repair", act, detail, out, runErr, map[string]string{
			"reason":       repairReasonKickoffFailed,
			"deploymentId": deploymentID,
			"provider":     provider,
			"status":       "failed",
		}), nil
	}

	// From here the watcher owns the record and the guard. It is handed the
	// RPC's context for its VALUES -- the caller's identity -- not its
	// lifetime; see goWatchRepair.
	s.startRepairWatch(ctx, deploymentID)

	return s.finishWrite(ctx, "repair", act, detail, asyncRepairAck(deploymentID), nil, map[string]string{
		"deploymentId": deploymentID,
		"version":      version,
		"imageDigest":  digest,
		"provider":     provider,
		"application":  argoApplication,
		"status":       "in_progress",
		"async":        "true",
	}), nil
}

// asyncRepairAck mirrors asyncDeployAck for the repair kick-off: ok=true means
// "accepted + kicked off", and the repair record carries the terminal status.
func asyncRepairAck(deploymentID string) string {
	return fmt.Sprintf("repair kicked off: ArgoCD application %s is re-syncing from the committed overlay; "+
		"repair record %s -> in_progress, resolved to succeeded or failed as the reconciliation is observed. "+
		"Poll the deployment concept / GetDeploymentStatus for resolution.", argoApplication, deploymentID)
}

// -----------------------------------------------------------------------------
// Provider + version resolution (off the concepts, never off the environment)
// -----------------------------------------------------------------------------

// resolveInstallationProvider reads the installation's provider off the graph:
// the v1:cluster:cluster row first, then the newest v1:cluster:deployment
// record that carries one. Returns the provider and where it came from
// ("cluster", "deployment", or "none"); "" with "none" when nothing says.
func (s *Service) resolveInstallationProvider(ctx context.Context) (provider, source string) {
	if s == nil || s.engine == nil {
		return "", "none"
	}
	if p := s.clusterRowField(ctx, "provider"); p != "" {
		return p, "cluster"
	}
	if p := s.newestDeploymentField(ctx, "provider"); p != "" {
		return p, "deployment"
	}
	return "", "none"
}

// repairTargetVersion is the version a repair record carries: the version in
// force, which is what currentVersion already resolves for CutVersion
// (highest succeeded record, else the overlay's promoted version), falling
// back to the version the cluster row reports. Best-effort metadata, like the
// digest: a repair changes no version, so an empty value is an unknown, not
// a fault.
func (s *Service) repairTargetVersion(ctx context.Context) (version, digest string) {
	version, _, _ = s.currentVersion(ctx)
	if version == "" {
		version = s.clusterRowField(ctx, "version")
	}
	return version, s.resolveImageDigest(version)
}

// clusterRowField reads one payload field off the v1:cluster:cluster
// singleton (existingCluster), "" when there is no row or the read fails.
func (s *Service) clusterRowField(ctx context.Context, field string) string {
	if s == nil || s.engine == nil {
		return ""
	}
	res, err := s.engine.Execute(ctx, "existingCluster()")
	if err != nil {
		if s.logger != nil {
			s.logger.Warn("deploycontrol: read cluster row failed", "error", err, "field", field)
		}
		return ""
	}
	if res == nil || res.Bundle == nil {
		return ""
	}
	for _, node := range res.Bundle.Nodes {
		if node == nil || node.Payload == nil {
			continue
		}
		if v := strings.TrimSpace(node.Payload.GetFields()[field].GetStringValue()); v != "" {
			return v
		}
	}
	return ""
}

// newestDeploymentField reads one payload field off the newest
// v1:cluster:deployment record (by row createdAt) that carries a non-empty
// value for it. Same query currentVersion uses; "" when nothing does.
func (s *Service) newestDeploymentField(ctx context.Context, field string) string {
	if s == nil || s.engine == nil {
		return ""
	}
	clusterID := strings.TrimSpace(os.Getenv("MEMQL_CLUSTER_ID"))
	query := "deploymentsForCluster(" + renderDeploymentArgs(map[string]any{"clusterId": clusterID}) + ")"
	res, err := s.engine.Execute(ctx, query)
	if err != nil || res == nil || res.Bundle == nil {
		return ""
	}
	var (
		best     string
		bestTime time.Time
		have     bool
	)
	for _, node := range res.Bundle.Nodes {
		if node == nil || node.Payload == nil {
			continue
		}
		v := strings.TrimSpace(node.Payload.GetFields()[field].GetStringValue())
		if v == "" {
			continue
		}
		at := node.GetCreatedAt().AsTime()
		if !have || at.After(bestTime) {
			best, bestTime, have = v, at, true
		}
	}
	return best
}

// createRepairDeployment writes the repair's v1:cluster:deployment row at
// in_progress: the version + digest in force, the resolved provider, the
// operator, and a note that marks it as a repair. Returns the new
// deploymentId. A missing engine or a failed write is a real error: the
// record is the caller's only progress source, so a repair without one would
// be the invented progress this verb exists to avoid.
func (s *Service) createRepairDeployment(ctx context.Context, version, digest, provider string, act actor) (string, error) {
	if s == nil || s.engine == nil {
		return "", fmt.Errorf("no engine wired: cannot persist repair record")
	}
	deploymentID := id.NewShortId()
	who := act.email
	if who == "" {
		who = act.userID
	}
	note := fmt.Sprintf("%s re-sync of ArgoCD application %s from the committed overlay (version %s), initiated by %s",
		repairNotePrefix, argoApplication, orUnknown(version), who)
	args := map[string]any{
		"deploymentId": deploymentID,
		"status":       "in_progress",
		"version":      version,
		"imageDigest":  digest,
		"provider":     provider,
		"region":       strings.TrimSpace(os.Getenv("MEMQL_REGION")),
		"clusterId":    strings.TrimSpace(os.Getenv("MEMQL_CLUSTER_ID")),
		"triggeredBy":  act.userID,
		"notes":        note,
	}
	query := "createDeployment(" + renderDeploymentArgs(args) + ")"
	if _, err := s.engine.Execute(ctx, query); err != nil {
		return "", fmt.Errorf("create repair deployment record: %w", err)
	}
	return deploymentID, nil
}

func orUnknown(v string) string {
	if strings.TrimSpace(v) == "" {
		return "unknown"
	}
	return v
}

// -----------------------------------------------------------------------------
// The one-at-a-time guard
// -----------------------------------------------------------------------------

func (s *Service) acquireRepair() bool {
	s.repairMu.Lock()
	defer s.repairMu.Unlock()
	if s.repairBusy {
		return false
	}
	s.repairBusy = true
	s.repairActive = ""
	return true
}

func (s *Service) setActiveRepair(deploymentID string) {
	s.repairMu.Lock()
	defer s.repairMu.Unlock()
	s.repairActive = deploymentID
}

func (s *Service) activeRepair() string {
	s.repairMu.Lock()
	defer s.repairMu.Unlock()
	return s.repairActive
}

// releaseRepair frees the guard. Called on every refusal after acquireRepair
// and by the watcher when the record is resolved.
func (s *Service) releaseRepair() {
	s.repairMu.Lock()
	defer s.repairMu.Unlock()
	s.repairBusy = false
	s.repairActive = ""
}

// -----------------------------------------------------------------------------
// Observation
// -----------------------------------------------------------------------------

// repairObservation is the slice of the Application's status the repair path
// reads: the sync + health verdicts GetDeploymentStatus already maps, plus
// the operation state those do not carry -- its phase, its message, and the
// info entries of the Operation that produced it (where the repair marker
// lands).
type repairObservation struct {
	phase   string
	message string
	synced  bool
	healthy bool
	health  string
	sync    string
	marker  string
}

// argoOperationJSON mirrors the slice of `kubectl -n argocd get app <name>
// -o json` the repair path consumes. Kept separate from status.go's
// argoAppJSON because that one is the DeploymentStatus mapping and this one
// reads the Operation echo that mapping has no field for.
type argoOperationJSON struct {
	Status struct {
		Sync struct {
			Status string `json:"status"`
		} `json:"sync"`
		Health struct {
			Status string `json:"status"`
		} `json:"health"`
		OperationState struct {
			Phase     string `json:"phase"`
			Message   string `json:"message"`
			Operation struct {
				Info []struct {
					Name  string `json:"name"`
					Value string `json:"value"`
				} `json:"info"`
			} `json:"operation"`
		} `json:"operationState"`
	} `json:"status"`
}

// parseRepairObservation maps raw Application JSON into a repairObservation.
// Empty input is a zero observation (no operation, not synced, not healthy),
// matching MapArgoStatus's treatment of a missing Application.
func parseRepairObservation(raw []byte) (repairObservation, error) {
	if len(raw) == 0 {
		return repairObservation{}, nil
	}
	var app argoOperationJSON
	if err := json.Unmarshal(raw, &app); err != nil {
		return repairObservation{}, fmt.Errorf("deploycontrol: parse argo app json: %w", err)
	}
	obs := repairObservation{
		phase:   app.Status.OperationState.Phase,
		message: app.Status.OperationState.Message,
		sync:    app.Status.Sync.Status,
		health:  app.Status.Health.Status,
		synced:  app.Status.Sync.Status == "Synced",
		healthy: app.Status.Health.Status == "Healthy",
	}
	for _, info := range app.Status.OperationState.Operation.Info {
		if info.Name == repairOperationInfoName {
			obs.marker = strings.TrimSpace(info.Value)
		}
	}
	return obs, nil
}

// operationRunning reports whether the Application has an operation in
// flight -- ArgoCD's Running or Terminating phases.
func (o repairObservation) operationRunning() bool {
	return o.phase == "Running" || o.phase == "Terminating"
}

// observeRepair reads the Application through the Executor.
func (s *Service) observeRepair(ctx context.Context) (repairObservation, error) {
	raw, err := s.exec.KubectlJSON(ctx, "-n", argoNamespace, "get", "app", argoApplication, "-o", "json")
	if err != nil {
		return repairObservation{}, err
	}
	return parseRepairObservation(raw)
}

// -----------------------------------------------------------------------------
// The watcher
// -----------------------------------------------------------------------------

// goWatchRepair is the default startRepairWatch: it runs watchRepair on a
// goroutine, writes the terminal status, notifies the test seam, and releases
// the guard.
//
// The goroutine runs on context.WithoutCancel(rpcCtx), and that choice is
// load-bearing. The RPC has already been answered, so its LIFETIME must not
// bound the watch (a stream context ends with the browser session, a unary
// one with the reply); but its VALUES must travel -- they carry the caller's
// identity, and the engine refuses a mutation that arrives with no actor
// (component/memql/executor.go mutationActor). A watcher on a bare
// context.Background would have had its terminal write refused and left every
// repair record stranded at in_progress, which is precisely the invented
// progress this verb exists not to produce. Carrying the initiating owner's
// identity is also the honest attribution: the terminal transition is the
// outcome of the repair that owner asked for.
func (s *Service) goWatchRepair(rpcCtx context.Context, deploymentID string) {
	base := context.WithoutCancel(rpcCtx)
	go func() {
		defer s.releaseRepair()
		ctx, cancel := context.WithTimeout(base, s.repairCeiling)
		defer cancel()
		status, reason := s.watchRepair(ctx, deploymentID)

		// The write gets its own budget: the watch context may have expired,
		// and an expired context is no reason to leave the record stranded.
		writeCtx, writeCancel := context.WithTimeout(base, 30*time.Second)
		defer writeCancel()
		s.transitionDeployment(writeCtx, deploymentID, status)
		if s.logger != nil {
			if status == "succeeded" {
				s.logger.Info("deploycontrol: repair resolved", "deployment_id", deploymentID, "status", status)
			} else {
				s.logger.Warn("deploycontrol: repair resolved", "deployment_id", deploymentID, "status", status, "reason", reason)
			}
		}
		if s.onRepairResolved != nil {
			s.onRepairResolved(deploymentID, status, reason)
		}
	}()
}

// watchRepair observes the Application until the repair identified by
// deploymentID can be resolved, and returns the terminal status with the
// reason for a failure. It does not write; goWatchRepair does.
//
// The rules, in the order they are checked on every observation:
//
//   - the read failed: count it; repairObservationFailureBudget consecutive
//     failures resolve failed (a kubectl that stopped working is not going
//     to start).
//   - the observed operation is not ours (marker mismatch): the controller
//     has not picked the repair up yet, or is still running the previous
//     operation -- keep waiting. This is what stops a stale "Succeeded" from
//     the Application's last sync being read as this repair's verdict.
//   - ours and Failed / Error: failed, with ArgoCD's own message.
//   - ours and Succeeded: succeeded once the Application is ALSO synced and
//     Healthy; Degraded is failed (a sync that applied cleanly onto workloads
//     that then could not start is not a repaired cluster); Progressing keeps
//     waiting.
//   - the ceiling passed: failed, naming the last thing observed.
func (s *Service) watchRepair(ctx context.Context, deploymentID string) (status, reason string) {
	ticker := time.NewTicker(s.repairPoll)
	defer ticker.Stop()

	failures := 0
	var last repairObservation
	for {
		obs, err := s.observeRepair(ctx)
		if err != nil {
			failures++
			if failures >= repairObservationFailureBudget {
				return "failed", fmt.Sprintf("could not observe application %s in %d consecutive reads: %v",
					argoApplication, failures, err)
			}
		} else {
			failures = 0
			last = obs
			if obs.marker == deploymentID {
				switch obs.phase {
				case "Failed", "Error":
					return "failed", fmt.Sprintf("ArgoCD sync %s: %s", obs.phase, obs.message)
				case "Succeeded":
					switch {
					case obs.synced && obs.healthy:
						return "succeeded", ""
					case obs.health == "Degraded":
						return "failed", fmt.Sprintf("ArgoCD sync succeeded but application %s is Degraded (sync=%s)",
							argoApplication, obs.sync)
					}
				}
			}
		}

		select {
		case <-ctx.Done():
			return "failed", fmt.Sprintf("application %s did not reach synced+healthy within %s "+
				"(last observed: phase=%q sync=%q health=%q marker=%q)",
				argoApplication, s.repairCeiling, last.phase, last.sync, last.health, last.marker)
		case <-ticker.C:
		}
	}
}
