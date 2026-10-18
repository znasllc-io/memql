package worker

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"runtime"
	"strings"
	"sync/atomic"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	sdkworker "github.com/znasllc-io/memql/sdk/go/worker"

	"github.com/znasllc-io/memql-cockpit/internal/worker/apps"
	"github.com/znasllc-io/memql-cockpit/internal/worker/capacity"
	"github.com/znasllc-io/memql-cockpit/internal/worker/hardware"
	"github.com/znasllc-io/memql-cockpit/internal/worker/models"
	"github.com/znasllc-io/memql-cockpit/internal/worker/tools"
)

// Connection wraps the bidi gRPC stream the worker maintains against
// the cluster. Transport (dial / TLS / stream opener) is handled by
// the SDK's worker module; this struct adds the worker-protocol
// lifecycle on top: Register / RegisterAck capture, heartbeat /
// tool-result helpers, and the registration metadata the runner
// reads after Connect.
type Connection struct {
	conn   stream
	logger *slog.Logger

	RegistrationId string
	OwnerUserId    string
	RegisteredAt   time.Time

	// ModelFingerprint is the advertised model label set this connection
	// registered with. The runner compares a later discovery against it
	// to decide whether re-advertising is worth a reconnect -- which is
	// the only way to re-advertise, because the engine's stream handler
	// accepts Register exactly once, at the handshake.
	ModelFingerprint string

	// AdvertisedPipelines fingerprints the allow flag and repository scope
	// reported by the machine's live policy when this connection registered. Bound at Register like the rest of the
	// advertisement, so the runner compares the live policy against it.
	AdvertisedPipelines string
	AdvertisedWorkbench bool

	// cancel ends the context this connection's stream was opened on (the
	// runner opens each stream on its own); Close uses it when a graceful
	// close cannot finish.
	cancel context.CancelFunc
	// closing is set once the runner has decided to close this connection
	// to re-register, so a later evaluation on the same heartbeat loop does
	// not decide -- and say -- it all again.
	closing atomic.Bool
}

// closeGrace is how long Close waits for the graceful half-close before it
// cancels the stream's context. Ordinarily the half-close takes one frame's
// write; this bound exists for the one case that does not end on its own.
// A variable only so a test can shorten it.
var closeGrace = 5 * time.Second

// stream is what Connection needs from the SDK's connection: the three
// calls the worker protocol makes on it.
//
// An interface rather than the SDK's concrete type so a test can put a
// recorder where the stream would be and assert the wire shape of a REPLY
// -- a Pong, a ModelPullEnd -- the way buildRegister and buildHeartbeat
// let it assert the shape of a message the worker originates. The SDK's
// own stream field is unexported and it ships no fake, so this is the
// seam. Connect always fills it with the SDK; nothing else does.
type stream interface {
	Send(msg *memqlv1.WorkerClientMessage) error
	Recv() (*memqlv1.WorkerServerMessage, error)
	Close()
}

// The SDK's connection is what fills the seam in production, and this is
// the compile-time record of it: the lock that serializes every write on
// the gRPC stream lives in (*sdkworker.Connection).Send, and the seam's
// Send is that method.
var _ stream = (*sdkworker.Connection)(nil)

// dialSDK opens the SDK's stream to the cluster: dial, TLS, token, stream
// open -- everything before the first worker-protocol message. It is the
// Runner's default stream opener; a test puts a scripted stream there
// instead and drives the REAL handshake and loop against it. The Runner
// then runs handshake: Register, and the RegisterAck's metadata.
func dialSDK(ctx context.Context, cfg Config, logger *slog.Logger) (stream, error) {
	endpoint, useTLS, err := sdkworker.ParseClusterURL(cfg.ClusterURL)
	if err != nil {
		return nil, err
	}
	sdkConn, err := sdkworker.Dial(ctx, sdkworker.DialConfig{
		Endpoint: endpoint,
		UseTLS:   useTLS,
		Token:    cfg.Token,
		Logger:   logger,
	})
	if err != nil {
		return nil, err
	}
	return sdkConn, nil
}

// handshake runs Register / RegisterAck over an open stream and returns
// the live connection, or closes the stream and returns why it failed.
func handshake(ctx context.Context, s stream, cfg Config, inventory []apps.Info, modelInv models.Inventory, hw hardware.Inventory, pipelines tools.PipelinesPolicy, logger *slog.Logger) (*Connection, error) {
	c := &Connection{
		conn:   s,
		logger: logger,
	}
	if err := c.register(ctx, cfg, inventory, modelInv, hw, pipelines); err != nil {
		c.Close()
		return nil, err
	}
	return c, nil
}

// RegisterRefusedError is the cluster answering the handshake with a
// RegisterError instead of a RegisterAck: it heard this machine and said
// no. Typed, rather than folded into a string, because the runner treats
// a refusal differently from a cluster it could not reach -- retrying a
// refusal every few seconds asks the same question of the same answer
// (memql-cockpit#427).
type RegisterRefusedError struct {
	// Code is the engine's stable code (register_failed,
	// register_required).
	Code string
	// Message is the engine's own sentence, passed through unchanged.
	Message string
}

func (e *RegisterRefusedError) Error() string {
	return fmt.Sprintf("worker.register: %s: %s", e.Code, e.Message)
}

// LabelPipelines / LabelPipelinesAllowed are the label a machine whose
// owner allows CI pipeline steps registers with (memql#5494). The engine's
// router requires exactly pipelines=allowed of any machine it sends a
// pipeline step to, so nothing about a laptop is a default place for CI.
const (
	LabelPipelines        = "pipelines"
	LabelPipelinesAllowed = "allowed"
)

// withPipelinesLabel returns labels with the pipelines label set exactly
// when this machine's policy allows pipeline steps, never editing the map it
// was given.
//
// THE POLICY WINS over worker.yaml's own labels here, the opposite of the
// model labels' rule. An operator label claiming pipelines=allowed on a
// machine whose policy.yaml does not would route every step to a machine
// that then refuses it -- and a refusal by the worker fails the step rather
// than moving it to another machine.
func withPipelinesLabel(labels map[string]string, allowed bool) map[string]string {
	out := make(map[string]string, len(labels)+1)
	for k, v := range labels {
		out[k] = v
	}
	delete(out, LabelPipelines)
	if allowed {
		out[LabelPipelines] = LabelPipelinesAllowed
	}
	return out
}

// buildRegister assembles the worker-protocol Register handshake
// message. Pulled out of register() so tests can assert the wire
// shape -- in particular that capability_descriptor_json always
// satisfies the server-side validation rules (memql#1331: raw size,
// schemaVersion, action-name pattern) -- without a live stream.
func buildRegister(cfg Config, inventory []apps.Info, modelInv models.Inventory, hw hardware.Inventory, pipelines tools.PipelinesPolicy) *memqlv1.Register {
	hostname, _ := os.Hostname()
	// Local models (memql-cockpit#361). They ride the EXISTING
	// registration mechanism -- `model:<id>` and `runtime:<kind>` labels
	// plus the MODEL capability -- because the engine derives its routing
	// from labels and a second channel would be a second thing to
	// disagree with the first. A machine that offers none contributes
	// nothing here: no capability, no labels, no concurrency entry.
	modelReg := modelRegistrationFor(modelInv)
	labels := mergeModelLabels(cfg.Labels, modelReg.Labels)
	// The owner's consent to CI pipeline steps (memql#5494), from the live
	// policy. Also a copy, so the machine id below never lands in the
	// config's own map.
	labels = withPipelinesLabel(labels, pipelines.Allow)
	if mid, err := machineIDFor(cfg); err == nil && mid != "" {
		if labels == nil {
			labels = map[string]string{}
		}
		labels[LabelMachineId] = mid
	}
	register := &memqlv1.Register{
		Name:         cfg.Name,
		Capabilities: withModelCapability(cfg.Capabilities, modelReg.Capability),
		Labels:       labels,
		Concurrency:  withModelConcurrency(cfg.Concurrency, modelReg.Capability, modelReg.Concurrency),
		Platform: &memqlv1.PlatformInfo{
			Os:       runtime.GOOS,
			Arch:     runtime.GOARCH,
			Hostname: hostname,
		},
		Permissions: probePermissions(),
		Version:     cockpitVersion(),
		BuildTag:    cockpitBuildTag(),
		// Local apps (memql-cockpit#346). The engine derives `app:<id>`
		// routing labels from entries that are BOTH allowed and signed
		// in, and has no other way to learn any of this -- it cannot
		// dial this machine.
		Apps: appsToProto(inventory),
		// And how each is driven (memql-cockpit#444), which the engine
		// reads to keep structured calls and follow-ups off a harness
		// that cannot take them.
		AppDescriptors: appDescriptorsToProto(inventory),
	}
	// Capability descriptor (memql-cockpit#166): the same JSON the
	// workerComputer.capabilities action returns, sent up front so
	// the server knows the action surface at registration time. The
	// proto field is optional -- on the (never-expected) marshal
	// failure we register without it rather than fail the handshake;
	// the server treats omission as valid.
	if descJSON, err := tools.CapabilityDescriptorJSONFor(pipelines.RepositoryScopes()); err == nil {
		register.CapabilityDescriptorJson = descJSON
	}
	applyWorkbenchAdvertisement(register, cfg.workbenchReady)
	registerHardware(register, hw)
	return register
}

// register sends the Register message and waits for the RegisterAck.
func (c *Connection) register(ctx context.Context, cfg Config, inventory []apps.Info, modelInv models.Inventory, hw hardware.Inventory, pipelines tools.PipelinesPolicy) error {
	register := buildRegister(cfg, inventory, modelInv, hw, pipelines)
	c.ModelFingerprint = advertisedFingerprint(modelInv.Labels())

	c.AdvertisedPipelines = pipelines.AdvertisementFingerprint()
	c.AdvertisedWorkbench = cfg.workbenchReady
	if err := c.Send(&memqlv1.WorkerClientMessage{
		Payload: &memqlv1.WorkerClientMessage_Register{Register: register},
	}); err != nil {
		if !errors.Is(err, io.EOF) {
			return fmt.Errorf("worker.register: send: %w", err)
		}
		// io.EOF FROM A SEND MEANS "ASK RECV WHY". grpc-go's contract:
		// when the server has already ended the stream, SendMsg returns
		// io.EOF and the real status is only available from RecvMsg. A
		// cluster refusing this machine's token ends the stream at the
		// door, so returning the EOF here would report "send: EOF" for
		// what is actually Unauthenticated -- and the runner could not
		// tell a revoked token from a network blip.
		if _, rerr := c.Recv(); rerr != nil && !errors.Is(rerr, io.EOF) {
			return fmt.Errorf("worker.register: %w", rerr)
		}
		return fmt.Errorf("worker.register: send: %w", err)
	}

	resp, err := c.Recv()
	if err != nil {
		return fmt.Errorf("worker.register: recv: %w", err)
	}
	if errMsg := resp.GetRegisterError(); errMsg != nil {
		return &RegisterRefusedError{Code: errMsg.GetCode(), Message: errMsg.GetMessage()}
	}
	ack := resp.GetRegisterAck()
	if ack == nil {
		return errors.New("worker.register: server returned no ack")
	}
	c.RegistrationId = ack.GetRegistrationId()
	c.OwnerUserId = ack.GetOwnerUserId()
	if ts := ack.GetRegisteredAt(); ts != nil {
		c.RegisteredAt = ts.AsTime()
	}
	if c.logger != nil {
		c.logger.Info("worker registered with cluster",
			"registration_id", c.RegistrationId,
			"owner_user_id", c.OwnerUserId,
			"models_offered", len(modelInv.Advertised()),

			"pipelines_allowed", pipelines.Allow,
		)
	}
	return nil
}

// Send writes a single message on the worker side of the stream.
//
// THIS IS THE ONE SEAM. Every Send* helper below routes through here,
// and this is the only call to the SDK connection's Send in the
// repository (TestEveryWorkerWriteGoesThroughConnectionSend). The SDK's
// Send holds the lock that serializes every write on the gRPC stream --
// grpc-go forbids SendMsg from two goroutines on one stream, and this
// worker writes from the heartbeat goroutine, the recv goroutine (Pong),
// every tool dispatch, every model call's deltas and keepalives, every
// pull's progress and every app session's chunks at once. The lock lives
// in the SDK rather than here because the SDK owns the stream: a second
// lock at this layer would protect nothing the SDK's does not, and would
// have to be re-invented by every other worker host.
func (c *Connection) Send(msg *memqlv1.WorkerClientMessage) error {
	if c == nil || c.conn == nil {
		return errors.New("worker: connection is closed")
	}
	return c.conn.Send(msg)
}

// Recv blocks until the next inbound message lands.
func (c *Connection) Recv() (*memqlv1.WorkerServerMessage, error) {
	if c == nil || c.conn == nil {
		return nil, errors.New("worker: connection is closed")
	}
	return c.conn.Recv()
}

// SendPong answers one of the cluster's Pings (epic memql#5218, D11).
//
// sent_at is the Ping's own, echoed VERBATIM: the agent measures the round
// trip against its own clock and never reads the echo for the figure, so
// nothing this machine's clock says can shape the number. received_at is
// this machine's clock and is informational for the same reason.
func (c *Connection) SendPong(requestID string, sentAt *timestamppb.Timestamp) error {
	return c.Send(&memqlv1.WorkerClientMessage{
		Payload: &memqlv1.WorkerClientMessage_Pong{
			Pong: &memqlv1.Pong{
				RequestId:  requestID,
				SentAt:     sentAt,
				ReceivedAt: timestamppb.Now(),
			},
		},
	})
}

// SendModelPullProgress emits one observation of a pull in flight.
//
// Nothing is numbered and nothing is deduplicated, unlike the delta and
// chunk families: a pull's counters are per LAYER and legitimately go
// backwards, and the engine delivers every observation for that reason.
func (c *Connection) SendModelPullProgress(p *memqlv1.ModelPullProgress) error {
	return c.Send(&memqlv1.WorkerClientMessage{
		Payload: &memqlv1.WorkerClientMessage_ModelPullProgress{ModelPullProgress: p},
	})
}

// SendModelPullEnd closes a pull on the wire.
func (c *Connection) SendModelPullEnd(end *memqlv1.ModelPullEnd) error {
	return c.Send(&memqlv1.WorkerClientMessage{
		Payload: &memqlv1.WorkerClientMessage_ModelPullEnd{ModelPullEnd: end},
	})
}

// SendHeartbeat emits a Heartbeat envelope carrying the current app
// inventory.
//
// apps_present is ALWAYS true here, and that is load-bearing. proto3
// cannot tell an empty repeated field from an absent one, so the engine
// reads apps_present=false as "this build does not report apps" and
// leaves the stored inventory alone -- correct for an older cockpit, and
// wrong for this one on a machine that just uninstalled its last app.
// Once a build supports the field, every beat asserts the full truth,
// including "none".
func (c *Connection) SendHeartbeat(active uint32, perCap map[string]uint32, inventory []apps.Info, hw *hardware.Inventory, report *capacity.Report, setup *memqlv1.SetupProgress) error {
	beat := buildHeartbeat(active, perCap, inventory, hw)
	// Capacity rides every beat that has one. A beat without one leaves the
	// engine's last report in place until it ages out.
	beat.Capacity = capacity.ToProto(report)
	// So does the installer's report (memql-cockpit#541), on the same terms:
	// a beat without one leaves the engine's stored report alone.
	beat.Setup = setup
	return c.Send(&memqlv1.WorkerClientMessage{
		Payload: &memqlv1.WorkerClientMessage_Heartbeat{
			Heartbeat: beat,
		},
	})
}

// buildHeartbeat assembles the Heartbeat message. Pulled out of
// SendHeartbeat for the same reason buildRegister is pulled out of
// register: the wire shape is assertable without a live stream.
func buildHeartbeat(active uint32, perCap map[string]uint32, inventory []apps.Info, hw *hardware.Inventory) *memqlv1.Heartbeat {
	beat := &memqlv1.Heartbeat{
		Ts:                       timestamppb.Now(),
		ActiveCallsTotal:         active,
		ActiveCallsPerCapability: perCap,
		Apps:                     appsToProto(inventory),
		AppsPresent:              true,
	}
	heartbeatHardware(beat, hw)
	heartbeatPermissions(beat, probePermissions())
	return beat
}

// A nil snapshot is an ordinary beat between scans. HardwarePresent must
// remain false so the engine leaves the previous inventory alone.
func heartbeatHardware(beat *memqlv1.Heartbeat, hw *hardware.Inventory) {
	if hw == nil {
		return
	}
	beat.Hardware = hardwareToProto(hw)
	beat.HardwarePresent = true
}

// Registration carries the first scan; every tenth heartbeat refreshes it.
func registerHardware(register *memqlv1.Register, inv hardware.Inventory) {
	register.Hardware = hardwareToProto(&inv)
}

func hardwareToProto(inv *hardware.Inventory) *memqlv1.HardwareInventory {
	hw := &memqlv1.HardwareInventory{
		Chip:          inv.Chip,
		MemoryBytes:   inv.MemoryBytes,
		CpuCores:      uint32(inv.CPUCores),
		OsVersion:     inv.OSVersion,
		DiskFreeBytes: inv.DiskFreeBytes,
	}
	if inv.GPU != nil {
		hw.Gpu = &memqlv1.GpuInfo{
			Name:      inv.GPU.Name,
			VramBytes: inv.GPU.VRAMBytes,
			Backend:   inv.GPU.Backend,
		}
	}
	for _, runtime := range inv.Runtimes {
		hw.Runtimes = append(hw.Runtimes, &memqlv1.RuntimeInfo{Name: runtime.Name, Version: runtime.Version})
	}
	if !inv.ReportedAt.IsZero() {
		hw.ReportedAt = timestamppb.New(inv.ReportedAt)
	}
	return hw
}

// SendAppSessionChunk emits one piece of app-session output.
//
// seq is assigned by the session and passed through unchanged, including
// on a retry: the engine drops out-of-order and duplicate chunks rather
// than appending them, so renumbering a resend would open a gap in the
// transcript that no later reader could detect.
func (c *Connection) SendAppSessionChunk(sessionID, stream string, data []byte, seq uint64) error {
	return c.Send(&memqlv1.WorkerClientMessage{
		Payload: &memqlv1.WorkerClientMessage_AppSessionChunk{
			AppSessionChunk: &memqlv1.AppSessionChunk{
				SessionId: sessionID,
				Stream:    stream,
				Data:      data,
				Seq:       seq,
			},
		},
	})
}

// SendAppSessionEnd closes an app session on the wire.
func (c *Connection) SendAppSessionEnd(end *memqlv1.AppSessionEnd) error {
	return c.Send(&memqlv1.WorkerClientMessage{
		Payload: &memqlv1.WorkerClientMessage_AppSessionEnd{AppSessionEnd: end},
	})
}

// SendModelCallDelta emits one piece of generated output.
//
// seq is assigned by the call and passed through unchanged. The engine
// drops out-of-order and duplicate deltas rather than appending them, so
// a renumbering here would open a gap in the generation that no later
// reader could detect.
func (c *Connection) SendModelCallDelta(requestID string, seq uint64, content string, keepalive bool) error {
	return c.Send(&memqlv1.WorkerClientMessage{
		Payload: &memqlv1.WorkerClientMessage_ModelCallDelta{
			ModelCallDelta: &memqlv1.ModelCallDelta{
				RequestId: requestID,
				Seq:       seq,
				Content:   content,
				Keepalive: keepalive,
			},
		},
	})
}

// SendModelCallEnd closes a model call on the wire.
func (c *Connection) SendModelCallEnd(end *memqlv1.ModelCallEnd) error {
	return c.Send(&memqlv1.WorkerClientMessage{
		Payload: &memqlv1.WorkerClientMessage_ModelCallEnd{ModelCallEnd: end},
	})
}

// SendToolStream emits one piece of a running tool call's output
// (memql#5494: a pipeline step's stdout and stderr).
//
// Every chunk of a call goes out BEFORE its ToolResult: the engine relays a
// chunk only while the call is pending and drops one that arrives after the
// result (component/worker handleToolStream). The dispatcher returns only
// once its output is sent, and the runner sends the result after that.
func (c *Connection) SendToolStream(chunk *memqlv1.ToolStream) error {
	return c.Send(&memqlv1.WorkerClientMessage{
		Payload: &memqlv1.WorkerClientMessage_ToolStream{ToolStream: chunk},
	})
}

// SendToolResult emits a ToolResult envelope.
func (c *Connection) SendToolResult(callId string, success *memqlv1.Success, failure *memqlv1.Failure) error {
	res := &memqlv1.ToolResult{CallId: callId}
	if success != nil {
		res.Payload = &memqlv1.ToolResult_Success{Success: success}
	} else if failure != nil {
		res.Payload = &memqlv1.ToolResult_Failure{Failure: failure}
	}
	return c.Send(&memqlv1.WorkerClientMessage{
		Payload: &memqlv1.WorkerClientMessage_ToolResult{ToolResult: res},
	})
}

// Close ends the stream and the underlying SDK connection.
//
// GRACEFULLY WHEN IT CAN: the SDK takes CloseSend under its send lock, so
// the cluster reads a clean end of the stream rather than a reset -- and
// that half-close waits for a Send already in flight. A Send blocked on
// flow control, behind a cluster that is alive but has stopped reading
// this stream, would hold it for as long as the cluster stays that way:
// keepalive never fires for a peer that still acks pings. So the wait is
// bounded by closeGrace, and past it the stream's own context is
// cancelled, which ends that Send and lets the close finish. The context
// is cancelled on the way out regardless, so it is never leaked.
func (c *Connection) Close() {
	if c == nil || c.conn == nil {
		return
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		c.conn.Close()
	}()
	select {
	case <-done:
	case <-time.After(closeGrace):
		if c.cancel != nil {
			c.cancel()
		}
		<-done
	}
	if c.cancel != nil {
		c.cancel()
	}
}

// SetVersion tells the worker which version to register as.
//
// The binary's version is stamped at build time into `main.version` by
// `-ldflags -X`, and this package cannot read that -- so main hands it
// over at startup. Without this the worker registered a SECOND,
// hand-maintained constant, which had already drifted a minor version
// behind the one `memql --version` printed.
//
// That drift is the same defect TestVersionIsSettableByLdflags exists to
// prevent, one layer further out: the number the cluster stores for a
// machine, shows on /machines, and reads when deciding whether a cockpit
// is new enough to drive an app. A plausible-but-wrong version there is
// invisible in exactly the way a plausible-but-wrong version in
// `--version` was.
func SetVersion(v string) {
	if strings.TrimSpace(v) == "" {
		return
	}
	cockpitVersionValue = v
}

// cockpitVersionValue defaults to the VERSION file's contents so a build
// that never calls SetVersion -- a test, or `go run` -- reports something
// truthful rather than empty.
var cockpitVersionValue = "0.20.0"

func cockpitVersion() string { return cockpitVersionValue }
func cockpitBuildTag() string {
	if buildTagOverride != "" {
		return buildTagOverride
	}
	return "headless"
}

// buildTagOverride is set at link/init time by the computeruse-tagged
// platform layer to "computeruse". The default build leaves it empty so
// cockpitBuildTag() reports "headless".
var buildTagOverride string
