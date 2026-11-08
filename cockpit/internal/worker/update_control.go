package worker

import (
	"errors"
	"os"
	"sync"
	"time"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
)

// A downloaded and verified update reserves only the brief replacement and
// restart window. The lease disappears on restart/cancel and expires if the
// updater crashes; it never changes a saved connection or grants consent.
const updateAdmissionLease = 2 * time.Minute

type updateAdmission struct {
	mu        sync.Mutex
	until     time.Time
	finishing bool
	shutdown  func()
}

func (f *Fleet) controlUpdate(action string, pid int) error {
	if pid <= 0 || pid != os.Getpid() {
		return errors.New("The worker changed. Check its status and try the update again.")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.update.mu.Lock()
	defer f.update.mu.Unlock()
	if f.update.finishing {
		return errors.New("This worker is already shutting down for its reserved update.")
	}
	if action == "cancel-update" {
		f.update.until = time.Time{}
		return nil
	}
	if action != "prepare-update" {
		return errors.New("unknown update action")
	}
	if time.Now().Before(f.update.until) {
		return errors.New("An update is already preparing to restart this worker. Try again shortly.")
	}
	// Every runner uses this same lock to admit work and register it as
	// active. Include a disabled home still stopping its previous work.
	for _, home := range f.managed {
		if home.runner != nil && home.runner.busy() {
			return errors.New("The worker is busy. Finish active work, then try the update again.")
		}
	}
	f.update.until = time.Now().Add(updateAdmissionLease)
	return nil
}

// finishUpdate shuts down only the process that reserved this update. A
// service-manager restart names a job, so a replacement process could accept
// work between a status check and that restart. This PID-bound control runs in
// the reserved worker instead. Admission stays closed after acceptance even
// if shutdown takes longer than the original lease or the reply is lost.
func (f *Fleet) finishUpdate(pid int) (func(), error) {
	if pid <= 0 || pid != os.Getpid() {
		return nil, errors.New("The worker changed. No replacement worker was stopped.")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.update.mu.Lock()
	defer f.update.mu.Unlock()
	if f.update.finishing {
		return nil, errors.New("This worker is already shutting down for its reserved update.")
	}
	if !time.Now().Before(f.update.until) {
		return nil, errors.New("The update reservation expired. Prepare the worker again before restarting.")
	}
	if f.update.shutdown == nil {
		return nil, errors.New("This worker cannot finish an update through its local control.")
	}
	for _, home := range f.managed {
		if home.runner != nil && home.runner.busy() {
			return nil, errors.New("The worker is busy. No work was stopped.")
		}
	}
	f.update.finishing = true
	return f.update.shutdown, nil
}

func startsWorkerWork(msg *memqlv1.WorkerServerMessage) bool {
	switch msg.GetPayload().(type) {
	case *memqlv1.WorkerServerMessage_ToolDispatch,
		*memqlv1.WorkerServerMessage_AppSessionStart,
		*memqlv1.WorkerServerMessage_AppSignInStart,
		*memqlv1.WorkerServerMessage_ModelCallStart,
		*memqlv1.WorkerServerMessage_ModelPullStart:
		return true
	}
	return false
}

// Each refused start gets its normal terminal reply so the cluster never
// waits for work that was not admitted. Existing cancellation and heartbeat
// traffic continues while the service manager prepares the restart.
func refuseUpdating(conn *Connection, msg *memqlv1.WorkerServerMessage) error {
	const reason = "MemQL Cockpit is updating. Try again after the worker reconnects."
	switch payload := msg.GetPayload().(type) {
	case *memqlv1.WorkerServerMessage_ToolDispatch:
		return conn.SendToolResult(payload.ToolDispatch.GetCallId(), nil, &memqlv1.Failure{ErrorCode: "worker_updating", ErrorMessage: reason})
	case *memqlv1.WorkerServerMessage_AppSessionStart:
		return conn.SendAppSessionEnd(&memqlv1.AppSessionEnd{SessionId: payload.AppSessionStart.GetSessionId(), ExitCode: 1, Error: reason})
	case *memqlv1.WorkerServerMessage_AppSignInStart:
		return conn.SendAppSignInUpdate(&memqlv1.AppSignInUpdate{SignInId: payload.AppSignInStart.GetSignInId(), State: "failed", Error: reason})
	case *memqlv1.WorkerServerMessage_ModelCallStart:
		return conn.SendModelCallEnd(&memqlv1.ModelCallEnd{RequestId: payload.ModelCallStart.GetRequestId(), FinishReason: "error", ErrorCode: "worker_updating", Error: reason})
	case *memqlv1.WorkerServerMessage_ModelPullStart:
		return conn.SendModelPullEnd(&memqlv1.ModelPullEnd{RequestId: payload.ModelPullStart.GetRequestId(), Model: payload.ModelPullStart.GetModel(), Error: reason})
	}
	return nil
}
