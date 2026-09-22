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
// WHAT IT DOES NOT REUSE: preDispatchCheck. That gate answers "may this AGENT
// run a command on this user's computer", and an inference call is not that
// question -- it is MemQL asking a model for an answer, on hardware the user
// pointed at it by signing in. The consent that matters here is the one
// already given: the app is `allowed` in the machine's own policy.yaml and
// somebody signed into it. Running the computer-use scope gate over a chat
// turn would refuse every call on a machine whose owner had not granted an
// agent shell access, for a call that runs no shell command.

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

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

// appSessionMaxDuration bounds an INFERENCE call through an app.
//
// Deliberately far shorter than defaultAppSessionMaxDuration (4h, for a
// delegated coding task): this is one prompt and one answer, and a turn that
// has not finished in ten minutes is a wedged harness rather than deep work.
// A caller waiting on an inference call has something waiting on it.
const appSessionMaxDuration = 10 * time.Minute

// AppInference implements memqlengine.AppInference over the fleet router, the
// local registry and the session runner.
type AppInference struct {
	router   *Router
	store    FleetStore
	registry *workerservice.Registry
	runner   *workerservice.SessionRunner
	policies DelegationPolicyReader
	logger   *slog.Logger
	clock    func() time.Time
	// vision lands the images of a vision call in the session workspace
	// (issue memql#5523). NIL ON A NODE WITH NO BLOB STORAGE, and a vision
	// call then REFUSES rather than running without its images -- see
	// app_vision.go for why a prompt naming a file that is not there is the
	// worst of the available failures.
	vision *visionStager
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
	return &AppInference{
		router:   d.Router(),
		store:    d.FleetStore(),
		registry: d.Registry(),
		runner:   runner,
		policies: policies,
		logger:   logger,
		clock:    d.clock,
	}
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

// projectDoors turns registrations into one entry per KNOWN app id.
//
// Every id in the engine's closed set gets a door, even one no machine
// reports: a door with no machines behind it is what lets a refusal say "you
// have not signed into Codex anywhere" rather than saying nothing at all. Its
// Runnable() is false, so it is never selected.
func (a *AppInference) projectDoors(machines []Candidate) []memqlengine.AppDoor {
	now := a.clock()
	byApp := map[string]*memqlengine.AppDoor{}
	for _, id := range workerservice.KnownAppIds() {
		byApp[id] = &memqlengine.AppDoor{AppId: id}
	}

	for _, m := range machines {
		online := workerservice.IsOnline(m.LastSeenAt, m.RevokedAt, now)
		local := a.holdsStream(m)
		for _, app := range m.Apps {
			door, known := byApp[app.Id]
			if !known {
				// An app id outside the engine's closed set. Stored on the
				// registration, never driven -- so a newer cockpit reporting
				// one never makes the engine attempt a protocol it lacks.
				continue
			}
			if !app.Runnable() {
				continue
			}
			// A descriptor the machine did not send leaves both harness
			// capabilities TRUE. Absent is "this cockpit predates the field",
			// not a declared no, and reading silence as a refusal would take
			// structured answers away from every machine that has not
			// upgraded.
			structured, followUps := true, true
			harness := ""
			if d, ok := descriptorFor(m.AppDescriptors, app.Id); ok {
				structured, followUps, harness = d.StructuredResult, d.FollowUps, d.Harness
			}
			door.Machines = append(door.Machines, memqlengine.AppMachine{
				RegistrationId:   m.RegistrationId,
				Name:             m.Name,
				DisplayName:      m.DisplayName,
				Online:           online,
				Harness:          harness,
				StructuredResult: structured,
				FollowUps:        followUps,
				Subscription:     app.Subscription,
				LocalStream:      local,
			})
		}
	}

	out := make([]memqlengine.AppDoor, 0, len(byApp))
	for _, id := range workerservice.KnownAppIds() {
		out = append(out, *byApp[id])
	}
	return out
}

// holdsStream reports whether THIS replica holds the machine's stream.
//
// The app-session envelope has no cross-node forward yet (design section 8),
// so a machine on a sibling replica is skipped during selection rather than
// failing the run. Reading the registry rather than `connectedNodeId` answers
// the question that actually matters -- can this process open a session on it
// right now -- rather than the question the row can answer.
func (a *AppInference) holdsStream(c Candidate) bool {
	if a.registry == nil {
		return false
	}
	return a.registry.WorkerById(c.RegistrationId) != nil
}

// descriptorFor is DescriptorFor over the candidate's persisted descriptors.
func descriptorFor(descriptors []workerservice.AppDescriptor, appId string) (workerservice.AppDescriptor, bool) {
	return workerservice.DescriptorFor(descriptors, appId)
}

// AppOrder reads the owner's preferred app order off their delegation policy.
func (a *AppInference) AppOrder(ctx context.Context, actingUserId string) ([]string, error) {
	if a == nil || a.policies == nil || strings.TrimSpace(actingUserId) == "" {
		return nil, nil
	}
	policy, err := a.policies.DelegationPolicy(ctx, actingUserId)
	if err != nil {
		return nil, err
	}
	if !policy.Found {
		return nil, nil
	}
	return policy.AppOrder, nil
}

// Call runs one inference turn through an app session.
//
// The whole conversation becomes ONE prompt, because that is the shape the
// harnesses take: `claude -p <prompt>` and a Codex turn are given text, not a
// message array. Flattening here rather than pretending otherwise keeps the
// lie out of the wire -- and the roles are labelled in the flattened text so
// the app can still see who said what.
func (a *AppInference) Call(ctx context.Context, req memqlengine.AppCallRequest) (memqlengine.AppCallResult, error) {
	if a == nil || a.runner == nil {
		return memqlengine.AppCallResult{},
			fmt.Errorf("%w: no app sessions on this node", memqlengine.ErrAppUnavailable)
	}
	owner := strings.TrimSpace(req.ActingUserId)
	if owner == "" {
		// Refused rather than widened, exactly as the model-call path
		// refuses a blank acting user: an app session's credential names a
		// person, and there is no person here.
		return memqlengine.AppCallResult{}, &memqlengine.AppUnavailable{
			AppId:     req.AppId,
			LastError: "an app call needs an acting user; system work has no app door",
		}
	}
	if !workerservice.IsKnownAppId(req.AppId) {
		return memqlengine.AppCallResult{}, &memqlengine.AppUnavailable{
			AppId:     req.AppId,
			LastError: fmt.Sprintf("%q is not an app this engine drives", req.AppId),
		}
	}

	// CAN THIS REPLICA STAGE AT ALL (issue memql#5523) -- asked before a
	// machine is chosen, because it is a CONFIGURATION fact and needs no
	// machine to answer. A node with no blob storage refuses the call here
	// rather than selecting a machine, opening a session and only then
	// discovering the images have nowhere to land.
	//
	// The STAGE itself stays after selection, below: that one costs a Library
	// write and a promotion wait, and paying it for a call about to be refused
	// for having no machine is the waste this split avoids.
	if len(req.Images) > 0 {
		if ok, why := a.vision.ready(); !ok {
			return memqlengine.AppCallResult{}, &memqlengine.AppVisionStagingFailed{
				AppId: req.AppId, Images: len(req.Images), Staged: 0, Reason: why,
			}
		}
	}

	w, refusal := a.selectMachine(ctx, owner, req)
	if refusal != nil {
		return memqlengine.AppCallResult{}, refusal
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
			return memqlengine.AppCallResult{}, &memqlengine.AppVisionStagingFailed{
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
		RunId:          req.RunId,
		StepId:         req.StepId,
		MaxDuration:    appSessionMaxDuration,
		// The CHAT door carries the level too (epic memql#5391, design D8).
		// The same app on the same machine should not answer a `fast` turn at
		// the effort a `reasoning` one asked for merely because this door
		// serves a turn rather than a step.
		Level: req.Level,
		// Library artifacts the cockpit pulls into the workspace before the
		// run. Empty for an ordinary chat turn; a vision turn's staged images
		// are appended, which is the whole of what the wire needed for this
		// feature -- AppSessionStart.inputs already carried artifact ids and
		// the landing filename was already the engine's to choose (design D11).
		Inputs: appendStagedInputs(req.Inputs, staged),
	}

	result, err := a.runner.Run(ctx, w, spec, nil)
	surface := AppSurfacePrefix + req.AppId + "@" + result.WorkerId
	if err != nil {
		return memqlengine.AppCallResult{ExecutionSurface: surface},
			fmt.Errorf("%w: %s on %s: %v", memqlengine.ErrAppUnavailable, req.AppId, w.Name, err)
	}

	content, err := answerFrom(result, req.Schema != nil)
	if err != nil {
		return memqlengine.AppCallResult{ExecutionSurface: surface}, err
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
	}, nil
}

// selectMachine picks a machine that can run the app, through the fleet
// router, and returns the typed refusal naming what was considered when none
// can.
func (a *AppInference) selectMachine(ctx context.Context, owner string, req memqlengine.AppCallRequest) (*workerservice.Worker, *memqlengine.AppUnavailable) {
	if a.router == nil || a.registry == nil {
		return nil, &memqlengine.AppUnavailable{
			AppId:     req.AppId,
			LastError: "no fleet router on this node",
		}
	}
	plan, err := a.router.Plan(ctx, owner, workerservice.CapabilityHeadless, nil, nil)
	if err != nil {
		return nil, &memqlengine.AppUnavailable{AppId: req.AppId, LastError: err.Error()}
	}

	considered := map[string]string{}
	for _, cand := range plan.Candidates {
		w := a.registry.WorkerById(cand.RegistrationId)
		if w == nil {
			considered[cand.RegistrationId] = "its stream is held by another replica"
			continue
		}
		if !w.RunsApp(req.AppId) {
			considered[cand.RegistrationId] = req.AppId + " is not allowed and signed in"
			continue
		}
		if req.Schema != nil {
			if d, ok := w.AppDescriptor(req.AppId); ok && !d.StructuredResult {
				considered[cand.RegistrationId] = "its harness (" + d.Harness + ") cannot return a structured answer"
				continue
			}
		}
		return w, nil
	}
	for id, why := range plan.Rejected {
		considered[id] = why
	}
	return nil, &memqlengine.AppUnavailable{
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
	return result.Transcript, nil
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
