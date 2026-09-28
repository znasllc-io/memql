//go:build agent || planner

package worker

// Cross-replica APP-DOOR calls (the planner/app-source design, section 3a).
//
// THE PROBLEM. An app session travels over the machine's WorkerService stream,
// which terminates on exactly ONE agent replica. The planner holds no streams
// at all, and a sibling agent replica holds none for this machine -- so before
// this file a route that put `app:claude-code` first passed over it on every
// planner call (triage, compile, compose), and on half of the agent's at the
// default two replicas. The owner's decision is that planner work MUST be able
// to use Claude Code or Codex when the route or an Ask pin says so.
//
// THE SHAPE is ModelForward's: stamp a request id, park, send to the replica
// named by the registration's connectedNodeId, route the answer back by id.
// What crosses is the RESOLVED CALL -- the app, the machine, the conversation,
// the schema -- never the prompt for a second resolution: one router, one
// decision record.
//
// WHAT CROSSES AND WHAT DOES NOT. Only the chat door forwards: one chat,
// structured or vision turn. A STEP HANDOVER (the session door) does not -- it
// runs as a session subrun on the replica whose delegate opens it, which is
// why the session door keeps its local-only predicate
// (memql.ProviderRegistry.AppSessionRefusalHere).
//
// WHO DECIDES WHAT. The sender chooses the machine from the persisted row; the
// RECEIVER re-decides everything it can know better: the verified authority,
// that the registration is the subject's own and not revoked, the APP GATE
// (the pin and the kill switch, read at the moment the session would open),
// and that it still holds the stream with the app allowed and signed in. The
// receiver never forwards again, so a call crosses at most once.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	memqlengine "github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/node"
	nodev1 "github.com/znasllc-io/memql/component/node/gen"
	workerservice "github.com/znasllc-io/memql/component/worker"
	"github.com/znasllc-io/memql/core/common"
	"github.com/znasllc-io/memql/core/id"
)

// appSessionMaxDuration bounds an INFERENCE call through an app, on the
// replica that runs it and on the replica waiting for it.
//
// Deliberately far shorter than defaultAppSessionMaxDuration (4h, for a
// delegated coding task): this is one prompt and one answer, and a turn that
// has not finished in ten minutes is a wedged harness rather than deep work.
// A caller waiting on an inference call has something waiting on it.
const appSessionMaxDuration = 10 * time.Minute

// appForwardGrace is how much longer the SENDER waits than the receiver's own
// ceiling. The receiver times the session out and answers; the grace is for
// that answer to cross back. Past it the sender stops waiting, so an answer
// that was lost cannot park a planner step for longer than the call could
// ever have run.
const appForwardGrace = 30 * time.Second

// Refusal codes this hop adds to the app door's vocabulary.
const (
	// AppForwardHolderUnreachable: the agent the registration names is not a
	// peer this node can send to. Nothing started.
	AppForwardHolderUnreachable = "app_holder_unreachable"
	// AppForwardHolderGone: the stream the call went out on ended before an
	// answer came back. The session may have run.
	AppForwardHolderGone = "app_holder_gone"
	// AppForwardTimeout: no answer within the ceiling plus the grace.
	AppForwardTimeout = "app_forward_timeout"
	// appForwardNotConfigured: the receiving replica has no app door.
	appForwardNotConfigured = "not_configured"
)

// streamSender is a PeerSender that can also say when the stream a request
// went out on ends. The production sender can (node.PeerManager.SendRequest);
// a sender that cannot is waited on until the caller's context or the
// sender's own deadline ends.
type streamSender interface {
	SendRequest(nodeId string, msg *nodev1.NodeClientMessage) (<-chan struct{}, error)
}

// -----------------------------------------------------------------------------
// The sending half
// -----------------------------------------------------------------------------

// AppForwardOutcome is the terminal answer of a forwarded app call.
type AppForwardOutcome struct {
	Result       memqlengine.AppCallResult
	ErrorCode    string
	ErrorMessage string
	// RefusedBeforeStart is the re-pick predicate (AppCallForwardResponse):
	// true only when no session reached the machine.
	RefusedBeforeStart bool
}

// Ok reports a clean answer.
func (o AppForwardOutcome) Ok() bool { return o.ErrorCode == "" && o.ErrorMessage == "" }

// appForwardCall is one parked app call.
type appForwardCall struct {
	resp chan *nodev1.AppCallForwardResponse
}

// ForwardAppCall sends one resolved app-door call to the replica holding the
// machine's stream and waits for its answer.
//
// Every failure before the envelope leaves this node is REFUSED BEFORE START,
// because nothing ran. Every failure after it leaves is reported as having
// possibly run unless the RECEIVER says otherwise: a session that started has
// spent somebody's subscription and may have acted in its workspace, so the
// same call is not re-run on a second machine.
func (r *ForwardRouter) ForwardAppCall(
	ctx context.Context,
	nodeId string,
	registrationId string,
	ownerUserId string,
	req memqlengine.AppCallRequest,
	timeout time.Duration,
) (AppForwardOutcome, error) {
	if r == nil || r.sender == nil {
		return AppForwardOutcome{RefusedBeforeStart: true}, ErrNoPeerForNode
	}
	// A CALLER THAT HAS ALREADY GIVEN UP SENDS NOTHING. Not a request, and so
	// not the cancel that would have had to catch it on the holder: a request
	// sent with a done context put a Claude Code session on somebody's laptop
	// for nobody, whenever that cancel lost the race. The realistic path here
	// is a structured fallback reaching this source after a slow first Source
	// used up the deadline.
	if err := ctx.Err(); err != nil {
		return AppForwardOutcome{
			ErrorCode:          ForwardCallerCancelled,
			ErrorMessage:       "the caller gave up before the call was sent: " + err.Error(),
			RefusedBeforeStart: true,
		}, err
	}
	// THE MANDATORY ASSERTION (memql#3205), re-asserted from the context for
	// the reason ForwardDispatch states. This envelope carries the acting
	// user's prompt and opens a session on their machine, so its absence
	// refuses before start: the route moves on rather than guessing whose
	// call this is.
	authority, ok := auth.ForwardedAuthorityFromContext(ctx)
	if !ok {
		r.logger.Warn("app call forward: no forwarded authority bound to the call context; refusing to forward",
			"registration_id", registrationId, "target_node_id", nodeId)
		return AppForwardOutcome{
			ErrorCode:          "no_forwarded_authority",
			ErrorMessage:       "forwarding an app call requires the assertion this node accepted; none is bound to the call context",
			RefusedBeforeStart: true,
		}, nil
	}
	timeout = appCallTimeout(ctx, timeout)

	requestId := id.NewShortId()
	call := &appForwardCall{resp: make(chan *nodev1.AppCallForwardResponse, 1)}
	r.appMu.Lock()
	if r.appInflight == nil {
		r.appInflight = make(map[string]*appForwardCall)
	}
	r.appInflight[requestId] = call
	r.appMu.Unlock()
	defer func() {
		r.appMu.Lock()
		delete(r.appInflight, requestId)
		r.appMu.Unlock()
	}()

	env := appCallEnvelope(requestId, registrationId, ownerUserId, req, timeout,
		node.ForwardedAuthorityToProto(authority, r.SelfNodeId(), r.SelfNodeType()))
	msg := &nodev1.NodeClientMessage{
		MessageId: id.NewShortId(),
		Payload:   &nodev1.NodeClientMessage_AppCallForwardRequest{AppCallForwardRequest: env},
	}

	// A REQUEST THAT DID NOT LEAVE IS REPORTED AS NOT SENT. The stream-aware
	// transport also hands back the stream the request went out on, which is
	// how a holder that goes away mid-call is noticed instead of waited out.
	var streamDone <-chan struct{}
	if ss, ok := r.sender.(streamSender); ok {
		done, err := ss.SendRequest(nodeId, msg)
		if err != nil {
			return AppForwardOutcome{
				ErrorCode:          AppForwardHolderUnreachable,
				ErrorMessage:       err.Error(),
				RefusedBeforeStart: true,
			}, ErrNoPeerForNode
		}
		streamDone = done
	} else if !r.sender.Send(nodeId, msg) {
		return AppForwardOutcome{ErrorCode: AppForwardHolderUnreachable, RefusedBeforeStart: true}, ErrNoPeerForNode
	}

	wait, stop := context.WithTimeout(ctx, timeout+appForwardGrace)
	defer stop()
	select {
	case resp := <-call.resp:
		return appOutcomeFromWire(resp), nil
	case <-streamDone:
		// AN ANSWER THAT ARRIVED IS AN ANSWER. The dialer's receive goroutine
		// hands the holder's response to call.resp and only then sees the
		// stream end, so when both are ready select picks one at random -- and
		// picking this case threw Claude Code's work away and had the route
		// spend the call again on its next Source.
		select {
		case resp := <-call.resp:
			return appOutcomeFromWire(resp), nil
		default:
		}
		// No answer can come back on a stream that ended. NOT refused: the
		// session may well have started on the machine.
		return AppForwardOutcome{
			ErrorCode:    AppForwardHolderGone,
			ErrorMessage: "the connection to " + nodeId + ", the agent holding the machine's stream, ended before it answered; the session may have run there",
		}, nil
	case <-wait.Done():
		// Best-effort cancel, so the machine stops the session: a caller that
		// gave up must never leave an agent running on somebody's laptop.
		reason := "caller_cancelled"
		if ctx.Err() == nil {
			reason = "sender_timeout"
		}
		r.sendCancel(nodeId, &nodev1.NodeClientMessage{
			MessageId: id.NewShortId(),
			Payload: &nodev1.NodeClientMessage_AppCallForwardCancel{
				AppCallForwardCancel: &nodev1.AppCallForwardCancel{RequestId: requestId, Reason: reason},
			},
		})
		if err := ctx.Err(); err != nil {
			return AppForwardOutcome{}, err
		}
		return AppForwardOutcome{
			ErrorCode:    AppForwardTimeout,
			ErrorMessage: fmt.Sprintf("no answer from %s, the agent holding the machine's stream, within %s", nodeId, timeout+appForwardGrace),
		}, nil
	}
}

// DispatchAppCall implements node.WorkerForwardResponseSink for the terminal
// app-call answer.
func (r *ForwardRouter) DispatchAppCall(resp *nodev1.AppCallForwardResponse) {
	if r == nil || resp == nil {
		return
	}
	r.appMu.Lock()
	call, ok := r.appInflight[resp.GetRequestId()]
	r.appMu.Unlock()
	if !ok {
		// A response whose request is gone is the ordinary shape of a call
		// this node timed out or cancelled; it is not an error.
		r.logger.Debug("app call forward response for an unknown request id",
			"request_id", resp.GetRequestId(), "error_code", resp.GetErrorCode())
		return
	}
	select {
	case call.resp <- resp:
	default:
		r.logger.Warn("app call forward response dropped (channel full)", "request_id", resp.GetRequestId())
	}
}

// appCallTimeout is how long the holder may run one forwarded call: the
// inference ceiling, or the caller's own remaining deadline when that is
// nearer.
//
// Carried in the envelope so that even a cancel that never arrives -- the
// holder's connection to this node dropped, say -- stops the session when the
// caller would have stopped waiting, rather than at the ten-minute ceiling on
// somebody's subscription.
func appCallTimeout(ctx context.Context, timeout time.Duration) time.Duration {
	if timeout <= 0 || timeout > appSessionMaxDuration {
		timeout = appSessionMaxDuration
	}
	if deadline, ok := ctx.Deadline(); ok {
		if left := time.Until(deadline); left < timeout {
			timeout = left
		}
	}
	return timeout
}

// -----------------------------------------------------------------------------
// The receiving half
// -----------------------------------------------------------------------------

// AppCallServer is the holding agent's app door as the forward handler sees
// it: run one call on a machine this replica holds. The agent build's
// AppInference implements it; a replica without one refuses before start.
type AppCallServer interface {
	// ServeForwardedAppCall runs req on the named machine. refusedBeforeStart
	// is true only when no session reached the machine. It never forwards
	// again: a call crosses the mesh at most once.
	ServeForwardedAppCall(ctx context.Context, registrationId string, req memqlengine.AppCallRequest) (res memqlengine.AppCallResult, refusedBeforeStart bool, err error)
}

// SetAppCallServer installs this replica's app door behind the forward
// handler. Left unset on a replica with no app door, which then refuses every
// forwarded app call before start.
func (h *ForwardHandler) SetAppCallServer(s AppCallServer) {
	if h == nil {
		return
	}
	h.appMu.Lock()
	defer h.appMu.Unlock()
	h.apps = s
}

// HandleForwardedAppCall serves an inbound AppCallForwardRequest.
//
// WHAT IT CHECKS HERE, before the app door sees the call: the authority
// verifies, the envelope's owner hint agrees with it, and the registration is
// the verified subject's OWN and not revoked. App sessions are never lent --
// sharing a machine lends its models, not its signed-in apps, its subscription
// or its shell -- so this is verifyRegistration and deliberately not the model
// path's shared check. The app gate and the live-stream checks are the app
// door's (ServeForwardedAppCall), because they are the same ones a local call
// asks.
func (h *ForwardHandler) HandleForwardedAppCall(
	ctx context.Context,
	req *nodev1.AppCallForwardRequest,
	send func(*nodev1.NodeServerMessage) error,
) {
	requestId := req.GetRequestId()
	cctx, cancel := context.WithCancel(ctx)
	h.appMu.Lock()
	if h.appInflight == nil {
		h.appInflight = make(map[string]context.CancelFunc)
	}
	h.appInflight[requestId] = cancel
	apps := h.apps
	h.appMu.Unlock()
	defer func() {
		h.appMu.Lock()
		delete(h.appInflight, requestId)
		h.appMu.Unlock()
		cancel()
	}()

	authority := node.ForwardedAuthorityFromProto(req.GetAuthority())
	access, err := auth.VerifyForwardedAuthority(authority, time.Now())
	if err != nil {
		h.logger.Warn("app call forward: refused an envelope whose authority did not verify",
			"request_id", requestId, "registration_id", req.GetRegistrationId(), "error", err)
		h.sendAppRefusal(send, requestId, "forwarded_authority_refused", err.Error())
		return
	}
	cctx = auth.BindForwardedContext(cctx, authority.Principal().Claims, access, authority)

	owner := strings.TrimSpace(access.UserId)
	if owner == "" {
		h.sendAppRefusal(send, requestId, "forwarded_authority_refused",
			"the verified authority names no subject, so there is no owner to check the machine against")
		return
	}
	if hint := strings.TrimSpace(req.GetOwnerUserId()); hint != "" && !sameSubject(hint, owner) {
		h.logger.Warn("app call forward: envelope owner does not match the verified authority",
			"request_id", requestId, "envelope_owner", hint, "authority_subject", owner)
		h.sendAppRefusal(send, requestId, "owner_mismatch",
			"the envelope's owner does not match the verified authority's subject")
		return
	}
	registrationId := req.GetRegistrationId()
	if err := h.verifyRegistration(cctx, owner, registrationId); err != nil {
		h.sendAppRefusal(send, requestId, "registration_refused", err.Error())
		return
	}
	if apps == nil {
		h.sendAppRefusal(send, requestId, appForwardNotConfigured,
			"this replica has no app door to run the call through")
		return
	}

	timeout := time.Duration(req.GetTimeoutSec()) * time.Second
	if timeout <= 0 || timeout > appSessionMaxDuration {
		timeout = appSessionMaxDuration
	}
	callCtx, ccancel := context.WithTimeout(cctx, timeout)
	defer ccancel()

	// THE CALLER MAY ALREADY HAVE GONE. The node layer ends this context when
	// the call's cancel arrives, including one read before this goroutine ran
	// (component/node/forward_inflight.go), and the envelope's timeout is the
	// caller's own deadline. Either way nothing has opened yet, so the answer
	// is a refusal before start rather than a session for nobody. The app
	// door asks again at its own point of no return (callOnWorker).
	if err := callCtx.Err(); err != nil {
		h.sendAppRefusal(send, requestId, ForwardCallerCancelled,
			"the caller gave up before the session opened: "+err.Error())
		return
	}

	// The ACTING USER is the verified subject, never the envelope's hint.
	res, refused, err := apps.ServeForwardedAppCall(callCtx, registrationId, appCallFromEnvelope(owner, req))
	if err != nil {
		code, message := appRefusalWire(err)
		h.sendApp(send, &nodev1.AppCallForwardResponse{
			RequestId:          requestId,
			ErrorCode:          code,
			ErrorMessage:       message,
			RefusedBeforeStart: refused,
			ExecutionSurface:   res.ExecutionSurface,
			MachineLabel:       res.MachineLabel,
		})
		return
	}
	h.sendApp(send, &nodev1.AppCallForwardResponse{
		RequestId:        requestId,
		Ok:               true,
		Content:          res.Content,
		Model:            res.Model,
		Effort:           res.Effort,
		InputTokens:      res.Usage.InputTokens,
		OutputTokens:     res.Usage.OutputTokens,
		UsageKnown:       res.Usage.Known,
		ExecutionSurface: res.ExecutionSurface,
		MachineLabel:     res.MachineLabel,
		Billing:          res.Billing,
	})
}

// CancelForwardedAppCall stops an in-flight forwarded app call; the session
// on the machine is cancelled with it.
func (h *ForwardHandler) CancelForwardedAppCall(_ context.Context, requestId string) {
	h.appMu.Lock()
	cancel, ok := h.appInflight[requestId]
	if ok {
		delete(h.appInflight, requestId)
	}
	h.appMu.Unlock()
	if cancel != nil {
		cancel()
	}
}

func (h *ForwardHandler) sendAppRefusal(send func(*nodev1.NodeServerMessage) error, requestId, code, msg string) {
	h.sendApp(send, &nodev1.AppCallForwardResponse{
		RequestId:          requestId,
		ErrorCode:          code,
		ErrorMessage:       msg,
		RefusedBeforeStart: true,
	})
}

func (h *ForwardHandler) sendApp(send func(*nodev1.NodeServerMessage) error, resp *nodev1.AppCallForwardResponse) {
	if err := send(&nodev1.NodeServerMessage{
		MessageId:   id.NewShortId(),
		CorrelateTo: resp.GetRequestId(),
		Payload:     &nodev1.NodeServerMessage_AppCallForwardResponse{AppCallForwardResponse: resp},
	}); err != nil {
		h.logger.Warn("app call forward: response send failed",
			"request_id", resp.GetRequestId(), "error_code", resp.GetErrorCode(), "error", err)
	}
}

// appRefusalWire names an app-door failure for the wire: its stable code and
// its message. A GATE refusal crosses as its own code and its own message, so
// the sender can rebuild exactly the refusal a local call would have got.
func appRefusalWire(err error) (code, message string) {
	var gate *appGateRefusal
	if errors.As(err, &gate) {
		return gate.code, gate.message
	}
	// A VISION-STAGING failure crosses as its code and its reason, and is
	// rebuilt as the same type on the sender (forwardAppCall): "the image did
	// not land" must not arrive reading as "no machine can run this app".
	var vision *memqlengine.AppVisionStagingFailed
	if errors.As(err, &vision) {
		return vision.Code(), vision.Reason
	}
	var coded interface{ Code() string }
	if errors.As(err, &coded) {
		return coded.Code(), err.Error()
	}
	if errors.Is(err, memqlengine.ErrAppUnavailable) {
		return memqlengine.AppRefusalCode, err.Error()
	}
	return "app_call_failed", err.Error()
}

// -----------------------------------------------------------------------------
// The wire
// -----------------------------------------------------------------------------

// appCallEnvelope renders one resolved call for the wire. Only the role and
// the text of each message cross: an app takes ONE prompt, and the receiver
// flattens them exactly as a local call does.
func appCallEnvelope(requestId, registrationId, ownerUserId string, req memqlengine.AppCallRequest, timeout time.Duration, authority *nodev1.ForwardedAuthority) *nodev1.AppCallForwardRequest {
	env := &nodev1.AppCallForwardRequest{
		RequestId:      requestId,
		RegistrationId: registrationId,
		OwnerUserId:    ownerUserId,
		AppId:          req.AppId,
		Purpose:        req.Purpose,
		RunId:          req.RunId,
		StepId:         req.StepId,
		Model:          req.Model,
		Level:          req.Level,
		Effort:         req.Effort,
		Inputs:         append([]string(nil), req.Inputs...),
		Pin:            &nodev1.AppCallPin{Pinned: req.Pin.Pinned, By: req.Pin.By},
		TimeoutSec:     envelopeTimeoutSec(timeout),
		Authority:      authority,
	}
	for _, m := range req.Messages {
		env.Messages = append(env.Messages, &nodev1.AppCallMessage{Role: m.Role, Content: m.Content})
	}
	if req.Schema != nil {
		env.Schema = &nodev1.AppCallSchema{
			Name:        req.Schema.Name,
			Description: req.Schema.Description,
			SchemaJson:  append([]byte(nil), req.Schema.Schema...),
			Strict:      req.Schema.Strict,
		}
	}
	for _, img := range req.Images {
		env.Images = append(env.Images, &nodev1.AppCallImage{MimeType: img.MimeType, Data: img.Data})
	}
	return env
}

// envelopeTimeoutSec renders a timeout in whole seconds for the wire, rounded
// UP and never below one: a caller with 400ms left must not reach the holder
// as "no timeout", which the receiver reads as the ten-minute ceiling.
func envelopeTimeoutSec(timeout time.Duration) int32 {
	secs := (timeout + time.Second - 1) / time.Second
	if secs < 1 {
		secs = 1
	}
	if secs > time.Duration(appSessionMaxDuration/time.Second) {
		secs = time.Duration(appSessionMaxDuration / time.Second)
	}
	return int32(secs)
}

// appCallFromEnvelope is appCallEnvelope's inverse on the receiver, with the
// acting user taken from the VERIFIED authority.
func appCallFromEnvelope(actingUserId string, env *nodev1.AppCallForwardRequest) memqlengine.AppCallRequest {
	req := memqlengine.AppCallRequest{
		ActingUserId: actingUserId,
		AppId:        env.GetAppId(),
		Purpose:      env.GetPurpose(),
		RunId:        env.GetRunId(),
		StepId:       env.GetStepId(),
		Model:        env.GetModel(),
		Level:        env.GetLevel(),
		Effort:       env.GetEffort(),
		Inputs:       append([]string(nil), env.GetInputs()...),
		Pin:          memqlengine.AppDoorPin{Pinned: env.GetPin().GetPinned(), By: env.GetPin().GetBy()},
	}
	for _, m := range env.GetMessages() {
		req.Messages = append(req.Messages, common.ChatMessage{Role: m.GetRole(), Content: m.GetContent()})
	}
	if s := env.GetSchema(); s != nil {
		req.Schema = &common.StructuredSchema{
			Name:        s.GetName(),
			Description: s.GetDescription(),
			Schema:      append([]byte(nil), s.GetSchemaJson()...),
			Strict:      s.GetStrict(),
		}
	}
	for _, img := range env.GetImages() {
		req.Images = append(req.Images, common.VisionContent{MimeType: img.GetMimeType(), Data: img.GetData()})
	}
	return req
}

// appOutcomeFromWire projects the terminal answer.
func appOutcomeFromWire(resp *nodev1.AppCallForwardResponse) AppForwardOutcome {
	out := AppForwardOutcome{
		ErrorCode:          resp.GetErrorCode(),
		ErrorMessage:       resp.GetErrorMessage(),
		RefusedBeforeStart: resp.GetRefusedBeforeStart(),
		Result: memqlengine.AppCallResult{
			ExecutionSurface: resp.GetExecutionSurface(),
			MachineLabel:     resp.GetMachineLabel(),
		},
	}
	if !resp.GetOk() && out.ErrorCode == "" {
		// An answer that says neither ok nor why is still not an answer.
		out.ErrorCode = "app_call_failed"
	}
	if resp.GetOk() {
		out.Result.Content = resp.GetContent()
		out.Result.Model = resp.GetModel()
		out.Result.Effort = resp.GetEffort()
		out.Result.Usage = memqlengine.AppUsage{
			InputTokens:  resp.GetInputTokens(),
			OutputTokens: resp.GetOutputTokens(),
			Known:        resp.GetUsageKnown(),
		}
		out.Result.Billing = resp.GetBilling()
	}
	return out
}

// -----------------------------------------------------------------------------
// Selection and the call, shared by both senders
// -----------------------------------------------------------------------------

// projectAppDoors turns registrations into one door per KNOWN app id, with
// each machine placed by `place`: local (this replica holds its stream) or
// forwardable (another agent does, and this node can forward to it).
//
// Every id in the engine's closed set gets a door, even one no machine
// reports: a door with no machines behind it is what lets a refusal say "you
// have not signed into Codex anywhere" rather than saying nothing at all.
func projectAppDoors(machines []Candidate, now time.Time, place func(Candidate) (local, forwardable bool)) []memqlengine.AppDoor {
	byApp := map[string]*memqlengine.AppDoor{}
	for _, appId := range workerservice.KnownAppIds() {
		byApp[appId] = &memqlengine.AppDoor{AppId: appId}
	}
	for _, m := range machines {
		online := workerservice.IsOnline(m.LastSeenAt, m.RevokedAt, now)
		local, forwardable := place(m)
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
				Forwardable:      forwardable,
			})
		}
	}
	out := make([]memqlengine.AppDoor, 0, len(byApp))
	for _, appId := range workerservice.KnownAppIds() {
		out = append(out, *byApp[appId])
	}
	return out
}

// descriptorFor is DescriptorFor over the candidate's persisted descriptors.
func descriptorFor(descriptors []workerservice.AppDescriptor, appId string) (workerservice.AppDescriptor, bool) {
	return workerservice.DescriptorFor(descriptors, appId)
}

// canForwardTo reports whether an app call can be forwarded to the replica
// holding c's stream from a node whose id is self: there is a forward, the row
// names a holder, and the holder is not this node.
func canForwardTo(c Candidate, forward *ForwardRouter, self string) bool {
	if forward == nil || !workerservice.StreamHeld(c.ConnectedNodeId, c.RevokedAt) {
		return false
	}
	return strings.TrimSpace(c.ConnectedNodeId) != strings.TrimSpace(self)
}

// appCandidateRefusal says why the PERSISTED row c cannot carry owner's call
// to appId, or "". It is appMachineRefusal's question asked of the row, for a
// machine this node has no live handle on; the holder asks the live
// registration again before anything opens.
func appCandidateRefusal(c Candidate, owner string, req memqlengine.AppCallRequest, now time.Time) string {
	if strings.TrimSpace(c.OwnerUserId) != "" && !sameSubject(c.OwnerUserId, owner) {
		return "it is not " + strings.TrimSpace(owner) + "'s machine; an app session runs only on its owner's machines"
	}
	if !workerservice.IsOnline(c.LastSeenAt, c.RevokedAt, now) {
		return "offline"
	}
	runs := false
	for _, app := range c.Apps {
		if app.Id == req.AppId && app.Runnable() {
			runs = true
			break
		}
	}
	if !runs {
		return req.AppId + " is not allowed and signed in"
	}
	if req.Schema != nil {
		if d, ok := descriptorFor(c.AppDescriptors, req.AppId); ok && !d.StructuredResult {
			return "its harness (" + d.Harness + ") cannot return a structured answer"
		}
	}
	return ""
}

// forwardAppCall tries each candidate held by another agent, in the fleet
// router's order, and returns the first answer.
//
// A candidate refused BEFORE START is recorded in considered and the next is
// tried; a GATE refusal stops the walk, because it is about the owner and the
// next machine would meet the same gate. A failure AFTER a session started is
// returned as it is: the session may have run, and running it on a second
// machine is a second spend rather than a retry. Every failure reads as a
// shut door (ErrAppUnavailable), so the caller's route moves on to its next
// source -- except a vision-staging refusal, which comes back as the
// AppVisionStagingFailed a local call returns.
func forwardAppCall(
	ctx context.Context,
	forward *ForwardRouter,
	logger *slog.Logger,
	owner string,
	req memqlengine.AppCallRequest,
	candidates []Candidate,
	considered map[string]string,
	total int,
) (memqlengine.AppCallResult, error) {
	if considered == nil {
		considered = map[string]string{}
	}
	lastErr := ""
	for _, c := range candidates {
		// A caller that has given up tries no further machine: sending one
		// would be a session on somebody's laptop for nobody.
		if ctxErr := ctx.Err(); ctxErr != nil {
			return memqlengine.AppCallResult{},
				fmt.Errorf("%w: %s: the caller gave up before %s was tried: %w", memqlengine.ErrAppUnavailable, req.AppId, c.Label(), ctxErr)
		}
		holder := strings.TrimSpace(c.ConnectedNodeId)
		out, err := forward.ForwardAppCall(ctx, holder, c.RegistrationId, owner, req, appSessionMaxDuration)
		if err == nil && out.Ok() {
			res := out.Result
			if res.MachineLabel == "" {
				res.MachineLabel = c.Label()
			}
			return res, nil
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return memqlengine.AppCallResult{ExecutionSurface: out.Result.ExecutionSurface},
				fmt.Errorf("%w: %s on %s: %w", memqlengine.ErrAppUnavailable, req.AppId, c.Label(), ctxErr)
		}
		reason := appForwardReason(out, err, holder)
		if isAppGateCode(out.ErrorCode) {
			return memqlengine.AppCallResult{}, fmt.Errorf("%w: %w", memqlengine.ErrAppUnavailable,
				&appGateRefusal{code: out.ErrorCode, message: out.ErrorMessage})
		}
		// A VISION-STAGING FAILURE comes back as the type a local call
		// returns, and stops the walk: the images land in the HOLDER's
		// storage, so the owner's next machine on the same holder meets the
		// same problem, and a storage fault must not end up reading as "no
		// machine can run this app" (memqlengine.AppVisionStagingRefusalCode).
		if out.ErrorCode == memqlengine.AppVisionStagingRefusalCode {
			return memqlengine.AppCallResult{}, &memqlengine.AppVisionStagingFailed{
				AppId:  req.AppId,
				Images: len(req.Images),
				Reason: strings.TrimSpace(out.ErrorMessage + " (on " + c.Label() + ", answered by " + holder + ")"),
			}
		}
		if out.RefusedBeforeStart || errors.Is(err, ErrNoPeerForNode) {
			considered[c.RegistrationId] = reason
			lastErr = reason
			if logger != nil {
				logger.Debug("app call forward: machine refused before start, trying the next",
					"app", req.AppId, "machine", c.RegistrationId, "holder", holder, "reason", reason)
			}
			continue
		}
		return memqlengine.AppCallResult{ExecutionSurface: out.Result.ExecutionSurface},
			fmt.Errorf("%w: %s on %s (through %s): %s", memqlengine.ErrAppUnavailable, req.AppId, c.Label(), holder, reason)
	}
	return memqlengine.AppCallResult{}, &memqlengine.AppUnavailable{
		AppId:      req.AppId,
		Considered: considered,
		Total:      total,
		LastError:  lastErr,
	}
}

// appForwardReason renders one failed forward in an operator's terms, naming
// the holding agent: the repair for an unreachable holder is on that node.
func appForwardReason(out AppForwardOutcome, err error, holder string) string {
	switch {
	case out.ErrorCode == AppForwardHolderUnreachable || errors.Is(err, ErrNoPeerForNode):
		return "the agent holding its stream (" + holder + ") is not reachable from this node"
	case out.ErrorCode == "no_forwarded_authority":
		// Refused HERE, before anything was sent: nobody answered.
		return out.ErrorCode + ": " + out.ErrorMessage
	case out.ErrorMessage != "" && out.ErrorCode != "" && !strings.HasPrefix(out.ErrorMessage, out.ErrorCode):
		return out.ErrorCode + ": " + out.ErrorMessage + " (answered by " + holder + ")"
	case out.ErrorMessage != "":
		return out.ErrorMessage + " (answered by " + holder + ")"
	case out.ErrorCode != "":
		return out.ErrorCode + " (answered by " + holder + ")"
	case err != nil:
		return err.Error()
	}
	return "no answer from " + holder
}

// -----------------------------------------------------------------------------
// The planner's app door
// -----------------------------------------------------------------------------

// RemoteAppInference is the app door on a node that holds no WorkerService
// streams at all -- the planner. Every machine is reached through the agent
// its registration names, so it has no registry, no session runner and no app
// gate of its own: the gate is the holder's, asked where the session opens.
//
// It mirrors NewRemoteFleetInference, and for the same reason: planner work
// makes model calls, and a route that names a source the planner could never
// open would be a route that says one thing and does another on every call.
type RemoteAppInference struct {
	router     *Router
	store      FleetStore
	policies   DelegationPolicyReader
	forward    *ForwardRouter
	selfNodeId string
	logger     *slog.Logger
	clock      func() time.Time
}

// NewRemoteAppInference builds the planner's app door over its fleet store,
// the owner's delegation policy (for the `app:*` order) and its existing
// forward to the agents.
func NewRemoteAppInference(store FleetStore, policies DelegationPolicyReader, forward *ForwardRouter, selfNodeId string, logger *slog.Logger) *RemoteAppInference {
	if logger == nil {
		logger = slog.Default()
	}
	return &RemoteAppInference{
		router:     NewRouter(store, logger, time.Now),
		store:      store,
		policies:   policies,
		forward:    forward,
		selfNodeId: selfNodeId,
		logger:     logger,
		clock:      time.Now,
	}
}

// Doors reports which apps this user could reach from here: every machine is
// FORWARDABLE and none is LOCAL, so the chat door opens and a step handover
// never does (memql.AppDoor.Runnable is local-only).
//
// An empty acting user reaches nothing, for AppInference.Doors' reason: an app
// session's credential names a person, and system work has none to name.
func (a *RemoteAppInference) Doors(ctx context.Context, actingUserId string) ([]memqlengine.AppDoor, error) {
	if a == nil || a.store == nil || strings.TrimSpace(actingUserId) == "" {
		return nil, nil
	}
	machines, err := a.store.WorkersForOwner(ctx, actingUserId)
	if err != nil {
		return nil, err
	}
	return projectAppDoors(machines, a.clock(), func(c Candidate) (bool, bool) {
		return false, canForwardTo(c, a.forward, a.selfNodeId)
	}), nil
}

// AppOrder reads the owner's preferred app order off their delegation policy,
// the same read the agent's app door makes: one Route must pick the same app
// for `app:*` whichever node resolves it, or the planner runs Claude Code in
// triage and the agent Codex in the reply for the same owner.
func (a *RemoteAppInference) AppOrder(ctx context.Context, actingUserId string) ([]string, error) {
	if a == nil {
		return nil, nil
	}
	return appOrderFor(ctx, a.policies, actingUserId)
}

// appOrderFor is the owner's delegationPolicy.appOrder, or nil when there is
// no reader, no acting user or no policy row -- `app:*` then takes the
// engine's own closed order.
func appOrderFor(ctx context.Context, policies DelegationPolicyReader, actingUserId string) ([]string, error) {
	if policies == nil || strings.TrimSpace(actingUserId) == "" {
		return nil, nil
	}
	policy, err := policies.DelegationPolicy(ctx, actingUserId)
	if err != nil {
		return nil, err
	}
	if !policy.Found {
		return nil, nil
	}
	return policy.AppOrder, nil
}

// Call forwards one app-door call to the agent holding a machine that can run
// it, trying the owner's machines in the fleet router's order.
func (a *RemoteAppInference) Call(ctx context.Context, req memqlengine.AppCallRequest) (memqlengine.AppCallResult, error) {
	if a == nil || a.forward == nil {
		return memqlengine.AppCallResult{},
			fmt.Errorf("%w: this node has no forward to the agent holding the machine", memqlengine.ErrAppUnavailable)
	}
	owner := strings.TrimSpace(req.ActingUserId)
	if owner == "" {
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
	plan, err := a.router.Plan(ctx, owner, workerservice.CapabilityHeadless, nil, nil)
	if err != nil {
		return memqlengine.AppCallResult{}, &memqlengine.AppUnavailable{AppId: req.AppId, LastError: err.Error()}
	}
	now := a.clock()
	considered := map[string]string{}
	var remote []Candidate
	for _, c := range plan.Candidates {
		if why := appCandidateRefusal(c, owner, req, now); why != "" {
			considered[c.RegistrationId] = why
			continue
		}
		if !canForwardTo(c, a.forward, a.selfNodeId) {
			considered[c.RegistrationId] = "no agent this node can forward to holds its stream"
			continue
		}
		remote = append(remote, c)
	}
	for regId, why := range plan.Rejected {
		considered[regId] = why
	}
	if len(remote) == 0 {
		return memqlengine.AppCallResult{}, &memqlengine.AppUnavailable{
			AppId: req.AppId, Considered: considered, Total: plan.Total,
		}
	}
	return forwardAppCall(ctx, a.forward, a.logger, owner, req, remote, considered, plan.Total)
}

var _ memqlengine.AppInference = (*RemoteAppInference)(nil)
