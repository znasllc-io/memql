//go:build agent

package worker

// The engine-side implementation of the APP DOOR (epic memql#5096, task
// memql#5100, design D3).
//
// It is the AppInference twin of fleet_inference.go and sits beside it for the
// same reason: app sessions travel over the WorkerService stream, which
// terminates on the agent node, so the code that can open one is behind
// `//go:build agent` while the contract lives in component/memql.
//
// WHAT IT REUSES, AND WHY THAT IS THE DESIGN. Selection is the SAME fleet
// router the delegated-task path uses, and the run is the SAME SessionRunner:
// an app door and a delegated task are the same act -- opening a session on
// somebody's machine -- reached through two different front doors. A second
// selector here would be a second thing that disagrees with the first, and the
// disagreement would present as a machine that is selectable from a policy and
// unreachable from a task.
//
// THE CONSENT IS THE APP GATE (app_gate.go), the same one the session door and
// a delegated Task ask -- not preDispatchCheck. That gate answers "may this
// AGENT run a command on this user's computer", and an app session is not that
// question. The consent that matters here is the app `allowed` in the machine's
// own policy.yaml with somebody signed into it, on the owner's own machine,
// reached through a routing rule's chain or through a pin the owner made
// themselves -- a pin skips every rule, so one anybody else made is refused.
// Running the computer-use scope gate over a chat turn would refuse every call
// on a machine whose owner had not granted an agent shell access, for a call
// that runs no shell command. The kill switch, when the owner explicitly
// engaged it, closes this door as it closes the others.

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	memqlengine "github.com/znasllc-io/memql/component/memql"
	workerservice "github.com/znasllc-io/memql/component/worker"
	"github.com/znasllc-io/memql/core/common"
	"github.com/znasllc-io/memql/core/id"
)

// AppSurfacePrefix is how an app-served call names itself on the ledger's
// executionSurface: `app:<appId>@<registrationId>`.
//
// It is NOT `cockpit-app:<appId>`, which is what the delegated-task path
// writes. The two are different acts with different billing questions -- a
// task delegation is a unit of work the planner handed over, an inference call
// is one turn inside MemQL's own reasoning -- and one surface string for both
// would make the ledger unable to answer "how much of my subscription went on
// inference".
const AppSurfacePrefix = "app:"

// AppInference implements memqlengine.AppInference over the fleet router, the
// local registry and the session runner.
type AppInference struct {
	router   *Router
	store    FleetStore
	registry *workerservice.Registry
	runner   *workerservice.SessionRunner
	policies DelegationPolicyReader
	// prefs is where the app gate reads the owner's kill switch -- the
	// dispatcher's store, so this door and the session door read one graph.
	prefs  PreferencesReader
	logger *slog.Logger
	clock  func() time.Time
	// vision lands the images of a vision call in the session workspace
	// (issue memql#5523). NIL ON A NODE WITH NO BLOB STORAGE, and a vision
	// call then REFUSES rather than running without its images -- see
	// app_vision.go for why a prompt naming a file that is not there is the
	// worst of the available failures.
	vision *visionStager
	// forward reaches a machine whose stream a SIBLING agent replica holds
	// (AppCallForward, the planner/app-source design section 3a), and
	// selfNodeId keeps this replica out of its own target set. Nil on a
	// replica that cannot forward, which then sees only its own machines --
	// what every replica saw before the forward existed.
	forward    *ForwardRouter
	selfNodeId string
}

// NewAppInference builds the seam implementation from the dispatcher's
// existing parts, so there is one router and one registry in the process
// rather than a second set that could disagree.
func NewAppInference(
	d *Dispatcher,
	runner *workerservice.SessionRunner,
	policies DelegationPolicyReader,
	logger *slog.Logger,
) *AppInference {
	if d == nil || runner == nil {
		return nil
	}
	if logger == nil {
		logger = slog.Default()
	}
	a := &AppInference{
		router:   d.Router(),
		store:    d.FleetStore(),
		registry: d.Registry(),
		runner:   runner,
		policies: policies,
		logger:   logger,
		clock:    d.clock,
	}
	if d.store != nil {
		a.prefs = d.store
	}
	return a
}

// SetVisionStager wires the blob surface a vision call needs.
//
// A SETTER rather than a constructor argument, because the blob store is
// resolved in the transport phase (app/transport_agent.go) and this seam is
// built in the integrations phase -- the same ordering that makes
// SetAttachmentUploader a setter on the workbench integration. Left unset, the
// door still serves chat and structured chat and refuses vision by name.
func (a *AppInference) SetVisionStager(engine visionEngine, uploader visionBlobUploader, container string) {
	if a == nil {
		return
	}
	a.vision = newVisionStager(engine, uploader, container, a.logger, a.clock)
}

// SetForward wires the cross-replica app-call hop: calls for a machine held by
// another agent are forwarded there, and the doors report such machines as
// Forwardable. A SETTER for SetVisionStager's reason -- the forward is built in
// the cluster phase (app/cluster_worker.go), after this seam.
func (a *AppInference) SetForward(forward *ForwardRouter, selfNodeId string) {
	if a == nil {
		return
	}
	a.forward = forward
	a.selfNodeId = strings.TrimSpace(selfNodeId)
}

// Doors reports which apps this user could reach right now.
//
// AN EMPTY ACTING USER REACHES NOTHING, and that is the one place this differs
// from the fleet. A fleet call with no acting user is SYSTEM work and runs on
// machines whose owners opted in; an app session has no such opt-in, because
// the session's back-channel credential's subject is a PERSON and system work
// has no person to name. So system work has no app door, rather than a shared
// one -- narrower, which is the safe direction.
func (a *AppInference) Doors(ctx context.Context, actingUserId string) ([]memqlengine.AppDoor, error) {
	if a == nil || a.store == nil {
		return nil, nil
	}
	if strings.TrimSpace(actingUserId) == "" {
		return nil, nil
	}
	machines, err := a.store.WorkersForOwner(ctx, actingUserId)
	if err != nil {
		return nil, err
	}
	return a.projectDoors(machines), nil
}

// projectDoors turns registrations into one entry per KNOWN app id, placing
// each machine LOCAL when this replica holds its stream and FORWARDABLE when a
// sibling agent does and this replica can forward there.
func (a *AppInference) projectDoors(machines []Candidate) []memqlengine.AppDoor {
	return projectAppDoors(machines, a.clock(), func(c Candidate) (bool, bool) {
		if a.holdsStream(c) {
			return true, false
		}
		return false, canForwardTo(c, a.forward, a.selfNodeId)
	})
}

// holdsStream reports whether THIS replica holds the machine's stream.
//
// Reading the registry rather than `connectedNodeId` answers the question that
// actually matters -- can this process open a session on it right now --
// rather than the question the row can answer. A machine held elsewhere is
// reached by forwarding one call (SetForward); a step handover is never
// forwarded, which is why a door built from such a machine is Reachable and
// not Runnable.
func (a *AppInference) holdsStream(c Candidate) bool {
	if a.registry == nil {
		return false
	}
	return a.registry.WorkerById(c.RegistrationId) != nil
}

// AppOrder reads the owner's preferred app order off their delegation policy.
func (a *AppInference) AppOrder(ctx context.Context, actingUserId string) ([]string, error) {
	if a == nil {
		return nil, nil
	}
	return appOrderFor(ctx, a.policies, actingUserId)
}

// Call runs one inference turn through an app session.
//
// The whole conversation becomes ONE prompt, because that is the shape the
// harnesses take: `claude -p <prompt>` and a Codex turn are given text, not a
// message array. Flattening here rather than pretending otherwise keeps the
// lie out of the wire -- and the roles are labelled in the flattened text so
// the app can still see who said what.
//
// A MACHINE THIS REPLICA HOLDS IS PREFERRED, and one held by a sibling agent
// is reached by forwarding THIS call there (the planner/app-source design,
// section 3a): with two agent replicas, which one serves the turn and which
// one holds the laptop is a coin flip, and losing it used to read as "no
// machine can run this" for a laptop the user could see was on.
func (a *AppInference) Call(ctx context.Context, req memqlengine.AppCallRequest) (memqlengine.AppCallResult, error) {
	if a == nil || a.runner == nil {
		return memqlengine.AppCallResult{},
			fmt.Errorf("%w: no app sessions on this node", memqlengine.ErrAppUnavailable)
	}
	owner := strings.TrimSpace(req.ActingUserId)
	if auth.ActsForNoPerson(ctx, owner) {
		// Refused rather than widened, exactly as the model-call path
		// refuses a blank acting user: an app session's credential names a
		// person, and there is no person here -- nor behind a system actor's
		// non-empty id, which is what a scheduled automation carries.
		return memqlengine.AppCallResult{}, &memqlengine.AppUnavailable{AppId: req.AppId, NoOwner: true}
	}
	if !workerservice.IsKnownAppId(req.AppId) {
		return memqlengine.AppCallResult{}, &memqlengine.AppUnavailable{
			AppId:     req.AppId,
			LastError: fmt.Sprintf("%q is not an app this engine drives", req.AppId),
		}
	}

	// THE APP GATE, before a machine is chosen -- the same consent the session
	// door asks, with the pin the router bound on this resolution. A refusal
	// reads as a shut door (ErrAppUnavailable), so a chat chain moves on to
	// its next source exactly as it does for a laptop that is asleep, and the
	// refusal's own code says which it was. A forwarded call meets the gate
	// again on the holder, which reads the switch at the moment it opens.
	if refusal := admitAppSession(ctx, a.prefs, a.logger, owner, req.Pin); refusal != nil {
		return memqlengine.AppCallResult{}, fmt.Errorf("%w: %w", memqlengine.ErrAppUnavailable, refusal)
	}

	w, remote, refusal := a.selectMachine(ctx, owner, req)
	if w != nil {
		res, _, err := a.callOnWorker(ctx, w, owner, req)
		return res, err
	}
	if len(remote) > 0 && a.forward != nil {
		return forwardAppCall(ctx, a.forward, a.logger, owner, req, remote, refusal.Considered, refusal.Total)
	}
	return memqlengine.AppCallResult{}, refusal
}

// ServeForwardedAppCall implements AppCallServer: the RECEIVING end of the
// app-call hop, on the replica holding the machine's stream.
//
// It asks everything a local call asks, here, where the answers are live: the
// app gate (the pin and the owner's kill switch, read now), that this replica
// still holds the stream, and that the app is still allowed and signed in
// there. Every refusal before the session starts says so, so the sender may
// try the owner's next machine. It NEVER forwards again: a call crosses the
// mesh at most once, so two replicas can never pass one call back and forth.
func (a *AppInference) ServeForwardedAppCall(ctx context.Context, registrationId string, req memqlengine.AppCallRequest) (memqlengine.AppCallResult, bool, error) {
	if a == nil || a.runner == nil {
		return memqlengine.AppCallResult{}, true,
			fmt.Errorf("%w: no app sessions on this node", memqlengine.ErrAppUnavailable)
	}
	owner := strings.TrimSpace(req.ActingUserId)
	if owner == "" || !workerservice.IsKnownAppId(req.AppId) {
		return memqlengine.AppCallResult{}, true, &memqlengine.AppUnavailable{
			AppId:     req.AppId,
			LastError: "a forwarded app call needs an acting user and an app this engine drives",
		}
	}
	if refusal := admitAppSession(ctx, a.prefs, a.logger, owner, req.Pin); refusal != nil {
		return memqlengine.AppCallResult{}, true, fmt.Errorf("%w: %w", memqlengine.ErrAppUnavailable, refusal)
	}
	var w *workerservice.Worker
	if a.registry != nil {
		w = a.registry.WorkerById(registrationId)
	}
	if w == nil {
		// The row the sender read said this replica holds the stream; it
		// does not any more. Ordinary -- the laptop reconnected elsewhere --
		// and re-pickable.
		return memqlengine.AppCallResult{}, true, &memqlengine.AppUnavailable{
			AppId:      req.AppId,
			Considered: map[string]string{registrationId: "this replica no longer holds its stream"},
			Total:      1,
		}
	}
	if why := appMachineRefusal(w, owner, req.AppId); why != "" {
		return memqlengine.AppCallResult{}, true, &memqlengine.AppUnavailable{
			AppId: req.AppId, Considered: map[string]string{registrationId: why}, Total: 1,
		}
	}
	if req.Schema != nil {
		if d, ok := w.AppDescriptor(req.AppId); ok && !d.StructuredResult {
			return memqlengine.AppCallResult{}, true, &memqlengine.AppUnavailable{
				AppId:      req.AppId,
				Considered: map[string]string{registrationId: "its harness (" + d.Harness + ") cannot return a structured answer"},
				Total:      1,
			}
		}
	}
	res, started, err := a.callOnWorker(ctx, w, owner, req)
	return res, !started, err
}

// callOnWorker runs one call on a machine this replica holds. started reports
// whether a session was opened on it -- the line after which nothing is
// re-pickable.
func (a *AppInference) callOnWorker(ctx context.Context, w *workerservice.Worker, owner string, req memqlengine.AppCallRequest) (res memqlengine.AppCallResult, started bool, err error) {
	// A CALLER THAT HAS GIVEN UP OPENS NOTHING. Asked first, before anything
	// is staged, and again at the point of no return below: a session opened
	// with a done context runs until the machine notices, on the owner's
	// subscription, for nobody.
	if err := ctx.Err(); err != nil {
		return memqlengine.AppCallResult{}, false, &callerGone{appId: req.AppId, err: err}
	}

	// CAN THIS REPLICA STAGE AT ALL (issue memql#5523) -- asked before
	// anything is written, because it is a CONFIGURATION fact and costs
	// nothing to answer. A node with no blob storage refuses the call here
	// rather than opening a session and only then discovering the images have
	// nowhere to land. It is asked HERE, on the replica that runs the
	// session, rather than before selection: a call forwarded to a sibling
	// lands its images in the SIBLING's session, so it is the sibling's
	// storage that matters.
	//
	// The STAGE itself follows: that one costs a Library write and a
	// promotion wait, and paying it for a call about to be refused is the
	// waste this split avoids.
	if len(req.Images) > 0 {
		if ok, why := a.vision.ready(); !ok {
			return memqlengine.AppCallResult{}, false, &memqlengine.AppVisionStagingFailed{
				AppId: req.AppId, Images: len(req.Images), Staged: 0, Reason: why,
			}
		}
	}

	schema := ""
	if req.Schema != nil {
		schema = string(req.Schema.Schema)
	}
	sessionId := "v1:worker:appSession:" + id.NewShortId()

	// THE IMAGES, LANDED BEFORE THE SESSION STARTS (issue memql#5523).
	//
	// Ordered here -- after the machine is selected, before the run spec is
	// built -- for two reasons. A stage costs a Library write and a promotion
	// wait, so doing it before selection would pay for a call that is about to
	// be refused for having no machine; and the prompt cannot be composed
	// until the filenames exist, because the names in the prompt ARE the names
	// the files landed under.
	//
	// A REFUSAL, not a degraded call. Every other failure on this path answers
	// "no machine can run this"; this one answers "the image did not land",
	// and they are different remedies. What must never happen is the third
	// option: sending the prompt anyway, which has the app answer about an
	// image it never saw.
	var staged []stagedVisionInput
	if len(req.Images) > 0 {
		landed, err := a.vision.Stage(ctx, owner, sessionId, req.Images)
		if err != nil {
			// Release what DID land. A half-staged turn is refused, so the
			// files it wrote are inputs to nothing and must not be left in
			// the owner's Files app.
			a.vision.Release(ctx, owner, landed)
			return memqlengine.AppCallResult{}, false, &memqlengine.AppVisionStagingFailed{
				AppId: req.AppId, Images: len(req.Images), Staged: len(landed), Reason: err.Error(),
			}
		}
		staged = landed
		// ARCHIVED WHEN THE SESSION ENDS, which is the lifecycle half of the
		// owner's decision: a staged input belonged to one turn. Deferred so
		// it runs on every exit -- an answered turn, a refused one, and a
		// panic alike; a leaked input is a file the person never chose to keep
		// and cannot tell from one they did.
		defer a.vision.Release(ctx, owner, staged)
	}

	spec := workerservice.RunSpec{
		SessionId:   sessionId,
		OwnerUserId: owner,
		App:         req.AppId,
		Kind:        workerservice.AppSessionKindRun,
		// The prompt NAMES each landed file, by the name it landed under.
		// visionPromptWithInputs is handed the stage's own result rather than
		// deriving the names a second time -- one literal per file, or the
		// two derivations disagree the day either changes.
		Prompt:         visionPromptWithInputs(flattenMessages(req.Messages), staged),
		ResponseSchema: schema,
		// EMPTY, ALWAYS: the machine chooses (AppSessionStart.workspace). An
		// inference turn is one session and nothing outlives it, so it
		// belongs in a directory of the machine's own choosing, whose
		// lifetime the machine owns. A directory the engine named under the
		// owner's workspaceRoot would be one nobody removes (the machine
		// cannot tell it from a run's, which later steps reuse), holding a
		// copy of every image the turn was shown. The root names a RUN's
		// directory, on the session door.
		Workspace:   "",
		RunId:       req.RunId,
		StepId:      req.StepId,
		MaxDuration: appSessionMaxDuration,
		// The CHAT door carries the level too (epic memql#5391, design D8).
		// The same app on the same machine should not answer a `fast` turn at
		// the effort a `reasoning` one asked for merely because this door
		// serves a turn rather than a step.
		Level: req.Level,
		// The model a policy or a person pinned with `app:<id>:<model>`, and
		// a person's explicit effort (epic memql#5414, design D20). Both
		// override the level's knobs on the far side; empty lets it decide.
		Model:  req.Model,
		Effort: req.Effort,
		// Library artifacts the cockpit pulls into the workspace before the
		// run. Empty for an ordinary chat turn; a vision turn's staged images
		// are appended, which is the whole of what the wire needed for this
		// feature -- AppSessionStart.inputs already carried artifact ids and
		// the landing filename was already the engine's to choose (design D11).
		Inputs: appendStagedInputs(req.Inputs, staged),
		// ONE MODEL CALL the step at RunId/StepId made, not the step's work:
		// the recording names the calling run and claims nothing on its step
		// (worker.RecordingOpen.ModelCall).
		ModelCall: true,
	}

	// Asked again here, at the point of no return: staging images can take a
	// while, and a caller that gave up meanwhile still opens nothing (its
	// staged inputs are released by the deferred Release above).
	if err := ctx.Err(); err != nil {
		return memqlengine.AppCallResult{}, false, &callerGone{appId: req.AppId, err: err}
	}

	// FROM HERE ON, NOTHING IS RE-PICKABLE: the session row is written and
	// the start goes on the wire, so the call may have run on the machine.
	result, err := a.runner.Run(ctx, w, spec, nil)
	surface := AppSurfacePrefix + req.AppId + "@" + result.WorkerId
	if err != nil {
		return memqlengine.AppCallResult{ExecutionSurface: surface, MachineLabel: w.Name}, true,
			fmt.Errorf("%w: %s on %s: %v", memqlengine.ErrAppUnavailable, req.AppId, w.Name, err)
	}

	content, err := answerFrom(result, req.Schema != nil)
	if err != nil {
		return memqlengine.AppCallResult{ExecutionSurface: surface, MachineLabel: w.Name}, true, err
	}
	return memqlengine.AppCallResult{
		Content: content,
		// WHAT THE APP REPORTED, verbatim (design D9). The chat door's own
		// provider reads these back through ServedModel for the decision row.
		Model:  result.Model,
		Effort: result.Effort,
		Usage: memqlengine.AppUsage{
			InputTokens:  result.Usage.InputTokens,
			OutputTokens: result.Usage.OutputTokens,
			Known:        result.Usage.Known,
		},
		ExecutionSurface: surface,
		MachineLabel:     w.Name,
		// The runner already derived this from what the app reported about
		// its own subscription; it is never re-derived here, because two
		// derivations of "who paid" would eventually disagree.
		Billing: appBilling(result.Billing),
	}, true, nil
}

// callerGone is an app-door call refused before its session opened because
// its caller had already given up. It reads as a shut door
// (ErrAppUnavailable) AND as the caller's own cancellation, and crosses the
// hop under ForwardCallerCancelled.
type callerGone struct {
	appId string
	err   error
}

func (e *callerGone) Error() string {
	return fmt.Sprintf("%v: %s: the caller gave up before the session opened: %v",
		memqlengine.ErrAppUnavailable, e.appId, e.err)
}

// Code is the stable tag the hop carries it under.
func (e *callerGone) Code() string { return ForwardCallerCancelled }

func (e *callerGone) Unwrap() []error { return []error{memqlengine.ErrAppUnavailable, e.err} }

// selectMachine picks a machine this replica holds that can run the app,
// through the fleet router. When none can, it returns the machines a SIBLING
// agent holds that could -- in the router's order, for the forward -- and the
// typed refusal naming what was considered.
func (a *AppInference) selectMachine(ctx context.Context, owner string, req memqlengine.AppCallRequest) (*workerservice.Worker, []Candidate, *memqlengine.AppUnavailable) {
	if a.router == nil || a.registry == nil {
		return nil, nil, &memqlengine.AppUnavailable{
			AppId:     req.AppId,
			LastError: "no fleet router on this node",
		}
	}
	plan, err := a.router.Plan(ctx, owner, workerservice.CapabilityHeadless, nil, nil)
	if err != nil {
		return nil, nil, &memqlengine.AppUnavailable{AppId: req.AppId, LastError: err.Error()}
	}

	now := a.clock()
	considered := map[string]string{}
	var remote []Candidate
	for _, cand := range plan.Candidates {
		w := a.registry.WorkerById(cand.RegistrationId)
		if w == nil {
			// Held by another replica: a candidate for the FORWARD, judged
			// on its row. The holder asks the live registration again.
			switch why := appCandidateRefusal(cand, owner, req, now); {
			case why != "":
				considered[cand.RegistrationId] = why
			case !canForwardTo(cand, a.forward, a.selfNodeId):
				considered[cand.RegistrationId] = "its stream is held by another replica"
			default:
				considered[cand.RegistrationId] = "its stream is held by " + cand.ConnectedNodeId
				remote = append(remote, cand)
			}
			continue
		}
		if why := appMachineRefusal(w, owner, req.AppId); why != "" {
			considered[cand.RegistrationId] = why
			continue
		}
		if req.Schema != nil {
			if d, ok := w.AppDescriptor(req.AppId); ok && !d.StructuredResult {
				considered[cand.RegistrationId] = "its harness (" + d.Harness + ") cannot return a structured answer"
				continue
			}
		}
		return w, nil, nil
	}
	for id, why := range plan.Rejected {
		considered[id] = why
	}
	return nil, remote, &memqlengine.AppUnavailable{
		AppId:      req.AppId,
		Considered: considered,
		Total:      plan.Total,
	}
}

// answerFrom picks the text a caller gets back from a finished session.
//
// A STRUCTURED CALL TAKES THE STRUCTURED RESULT AND NOTHING ELSE. Falling back
// to the transcript would hand a JSON parser a page of an agent's narration,
// and the parse failure would name the schema rather than the harness that
// never produced one. A harness that was asked for a structured answer and did
// not give one has not answered, and saying so is the useful outcome.
func answerFrom(result workerservice.RunResult, structured bool) (string, error) {
	if structured {
		if len(result.Result) == 0 {
			return "", fmt.Errorf("%w: %s ended without the structured answer it was asked for",
				memqlengine.ErrAppUnavailable, result.SessionId)
		}
		if !json.Valid(result.Result) {
			return "", fmt.Errorf("%w: %s returned a structured answer that is not JSON",
				memqlengine.ErrAppUnavailable, result.SessionId)
		}
		return string(result.Result), nil
	}
	if len(result.Result) > 0 {
		// A harness that produced a structured answer for a plain chat turn
		// has still answered; its own text is the better reading than the
		// transcript, which carries the whole session's narration.
		return string(result.Result), nil
	}
	// The app's own output. The transcript also carries stderr -- the
	// cockpit's note of which model the level ran as -- which is a record
	// for a reader, not part of the reply.
	return result.Answer, nil
}

// flattenMessages renders a conversation as the single prompt the harnesses
// take. Roles are labelled rather than dropped: the app cannot see a message
// array, but it can read who said what.
func flattenMessages(messages []common.ChatMessage) string {
	var b strings.Builder
	for _, m := range messages {
		if strings.TrimSpace(m.Content) == "" {
			continue
		}
		if b.Len() > 0 {
			b.WriteString("\n\n")
		}
		switch strings.ToLower(m.Role) {
		case "system":
			b.WriteString("[system]\n")
		case "assistant":
			b.WriteString("[assistant]\n")
		case "user", "":
			// The unlabelled default: a single-turn prompt is by far the
			// common case and labelling it adds noise the app then has to
			// read past.
		default:
			b.WriteString("[" + m.Role + "]\n")
		}
		b.WriteString(m.Content)
	}
	return b.String()
}

// appBilling maps the runner's billing onto the engine's vocabulary. It never
// answers "metered": MemQL was not billed for a call it did not make, so the
// only honest answers are "subscription" and "unknown".
func appBilling(billing string) string {
	if billing == workerservice.BillingSubscription {
		return workerservice.BillingSubscription
	}
	return workerservice.BillingUnknown
}

var _ memqlengine.AppInference = (*AppInference)(nil)
