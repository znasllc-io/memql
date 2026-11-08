package worker

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
)

func TestUpdateReservationRefusesBusyAndPreservesRegistry(t *testing.T) {
	fleet, path := controlFixture(t)
	before, _ := os.ReadFile(path)
	runner := &Runner{update: &fleet.update}
	fleet.managed["a"] = &managedHome{runner: runner}
	runner.activeCalls.Add(1)
	if err := fleet.controlUpdate("prepare-update", os.Getpid()); err == nil || !strings.Contains(err.Error(), "busy") {
		t.Fatalf("busy reservation: %v", err)
	}
	if fleet.LocalStatus().UpdatePending {
		t.Fatal("failed preparation must not reserve admission")
	}
	runner.activeCalls.Add(-1)
	if err := fleet.controlUpdate("prepare-update", os.Getpid()+1); err == nil {
		t.Fatal("a stale process ID acquired a reservation")
	}
	if err := fleet.controlUpdate("prepare-update", os.Getpid()); err != nil {
		t.Fatal(err)
	}
	if !fleet.LocalStatus().UpdatePending {
		t.Fatal("prepared update not reported")
	}
	if err := fleet.controlUpdate("prepare-update", os.Getpid()); err == nil {
		t.Fatal("a second updater extended the first reservation")
	}
	if err := fleet.controlUpdate("cancel-update", os.Getpid()); err != nil {
		t.Fatal(err)
	}
	if fleet.LocalStatus().UpdatePending {
		t.Fatal("cancellation left admission reserved")
	}
	after, _ := os.ReadFile(path)
	if string(before) != string(after) {
		t.Fatal("update reservation edited saved enrollment or connection policy")
	}
}

func TestUpdateReservationAnswersEveryStartAndKeepsPings(t *testing.T) {
	fleet, _ := controlFixture(t)
	if err := fleet.controlUpdate("prepare-update", os.Getpid()); err != nil {
		t.Fatal(err)
	}
	runner := &Runner{update: &fleet.update, logger: quietLogger()}
	recorder := &recordingStream{}
	conn := &Connection{conn: recorder}
	messages := []*memqlv1.WorkerServerMessage{
		{Payload: &memqlv1.WorkerServerMessage_ToolDispatch{ToolDispatch: &memqlv1.ToolDispatch{CallId: "tool"}}},
		{Payload: &memqlv1.WorkerServerMessage_AppSessionStart{AppSessionStart: &memqlv1.AppSessionStart{SessionId: "session"}}},
		{Payload: &memqlv1.WorkerServerMessage_AppSignInStart{AppSignInStart: &memqlv1.AppSignInStart{SignInId: "signin"}}},
		{Payload: &memqlv1.WorkerServerMessage_ModelCallStart{ModelCallStart: &memqlv1.ModelCallStart{RequestId: "model"}}},
		{Payload: &memqlv1.WorkerServerMessage_ModelPullStart{ModelPullStart: &memqlv1.ModelPullStart{RequestId: "pull", Model: "test"}}},
		{Payload: &memqlv1.WorkerServerMessage_Ping{Ping: &memqlv1.Ping{RequestId: "ping"}}},
	}
	for _, msg := range messages {
		if err := runner.handleMessage(context.Background(), conn, msg); err != nil {
			t.Fatal(err)
		}
	}
	got := recorder.messages()
	if len(got) != len(messages) {
		t.Fatalf("%d replies for %d requests", len(got), len(messages))
	}
	if got[0].GetToolResult().GetFailure().GetErrorCode() != "worker_updating" ||
		got[1].GetAppSessionEnd().GetSessionId() != "session" || got[1].GetAppSessionEnd().GetError() == "" ||
		got[2].GetAppSignInUpdate().GetState() != "failed" ||
		got[3].GetModelCallEnd().GetErrorCode() != "worker_updating" ||
		got[4].GetModelPullEnd().GetOk() || got[4].GetModelPullEnd().GetError() == "" ||
		got[5].GetPong().GetRequestId() != "ping" {
		t.Fatalf("incorrect update refusals or ping reply: %v", got)
	}
	if runner.busy() {
		t.Fatal("a refused start became active work")
	}
}

func TestUpdateReservationExpiresAfterUpdaterDisappears(t *testing.T) {
	fleet, _ := controlFixture(t)
	// A dead updater cannot strand this machine. Expired leases admit the
	// next start without needing a timer goroutine or a saved-policy edit.
	fleet.update.until = time.Now().Add(-time.Second)
	runner := &Runner{update: &fleet.update, logger: quietLogger()}
	recorder := &recordingStream{}
	conn := &Connection{conn: recorder}
	msg := &memqlv1.WorkerServerMessage{Payload: &memqlv1.WorkerServerMessage_ToolDispatch{ToolDispatch: &memqlv1.ToolDispatch{CallId: "after-expiry"}}}
	if err := runner.handleMessage(context.Background(), conn, msg); err != nil {
		t.Fatal(err)
	}
	runner.active.Wait()
	got := recorder.messages()
	if len(got) != 1 || got[0].GetToolResult().GetFailure().GetErrorCode() != "no_tools_configured" {
		t.Fatalf("expired update still blocked normal dispatch: %v", got)
	}
	if err := fleet.controlUpdate("prepare-update", os.Getpid()); err != nil {
		t.Fatalf("expired lease prevented retry: %v", err)
	}
}

func TestUpdateReservationCannotOvertakeAnAdmittedCall(t *testing.T) {
	fleet, _ := controlFixture(t)
	held := &heldTools{started: make(chan string, 1)}
	runner := &Runner{update: &fleet.update, logger: quietLogger(), tools: held}
	fleet.managed["a"] = &managedHome{runner: runner}
	conn := &Connection{conn: &recordingStream{}}
	ctx, cancel := context.WithCancel(context.Background())
	defer func() { cancel(); runner.active.Wait() }()
	if err := runner.handleMessage(ctx, conn, toolDispatch("in-flight", time.Minute)); err != nil {
		t.Fatal(err)
	}
	held.awaitStart(t, "in-flight")
	if err := fleet.controlUpdate("prepare-update", os.Getpid()); err == nil {
		t.Fatal("update overtook admitted work")
	}
	cancel()
	runner.active.Wait()
	if err := fleet.controlUpdate("prepare-update", os.Getpid()); err != nil {
		t.Fatalf("finished work prevented update: %v", err)
	}
	if err := runner.handleMessage(context.Background(), conn, toolDispatch("too-late", time.Minute)); err != nil {
		t.Fatal(err)
	}
	select {
	case <-held.started:
		t.Fatal("new work started after update reservation")
	default:
	}
}
