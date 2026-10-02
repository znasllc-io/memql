package email

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/core/common"
)

// getOperation follows no provider-supplied URL. Credentials are signed for the
// saved resource endpoint and client-selected UUID only. Succeeded means ACS
// finished processing; recipient delivery still needs separate provider feedback.
func (s *ACSSender) getOperation(ctx context.Context, operationID string) (string, time.Duration, error) {
	if !azureUUID.MatchString(operationID) {
		return "", 0, fmt.Errorf("invalid Azure operation identity")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.cfg.Endpoint+"/emails/operations/"+operationID+"?api-version="+acsEmailAPIVersion, nil)
	if err != nil {
		return "", 0, err
	}
	s.sign(req, nil)
	response, err := s.client.Do(req)
	if err != nil {
		return "", time.Minute, fmt.Errorf("Azure send status is temporarily unavailable")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 64<<10))
		return "", 5 * time.Minute, fmt.Errorf("Azure has not confirmed this send status")
	}
	var value struct {
		ID     string `json:"id"`
		Status string `json:"status"`
	}
	if json.NewDecoder(io.LimitReader(response.Body, 64<<10)).Decode(&value) != nil || value.ID != operationID {
		return "", time.Minute, fmt.Errorf("Azure returned an invalid send receipt")
	}
	state := map[string]string{"NotStarted": "accepted", "Running": "running", "Succeeded": "succeeded", "Failed": "failed", "Canceled": "canceled"}[value.Status]
	if state == "" {
		return "", time.Minute, fmt.Errorf("Azure returned an unknown processing status")
	}
	wait := parseRetryAfter(response.Header.Get("Retry-After"))
	if wait < 15*time.Second {
		wait = 15 * time.Second
	}
	if wait > 15*time.Minute {
		wait = 15 * time.Minute
	}
	return state, wait, nil
}

func operationPending(status string) bool {
	return status == "submitting" || status == "unknown" || status == "accepted" || status == "running"
}

// PollOperations is a bounded pass over persisted receipts. Every record is
// locked and re-read, so two workers, a lost response, or a process restart all
// converge on the same operation without another POST.
func (i *Integration) PollOperations(ctx context.Context) error {
	if i == nil || i.azure == nil || i.azure.store == nil {
		return nil
	}
	a := i.azure
	store := &storedACSOperations{connection: a.store}
	rows, err := store.rows(ctx, "emailPendingSendOperations", map[string]any{"dueBefore": a.clock().Format(time.RFC3339Nano)})
	if err != nil {
		return err
	}
	for _, row := range rows {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err = a.pollOperation(ctx, store, memql.BareShortId(fmt.Sprint(row["id"]))); err != nil {
			return err
		}
	}
	return nil
}

func (a *azureSetup) pollOperation(ctx context.Context, store acsOperationStore, key string) error {
	release, err := store.lock(ctx, key)
	if err != nil {
		return err
	}
	defer release()
	op, prior, found, err := store.read(ctx, key)
	if err != nil || !found {
		return err
	}
	now := a.clock()
	if !operationPending(op.Status) || now.Before(op.NextPollAt) {
		return nil
	}
	// Retain the receipt indefinitely, but bound provider polling. A missing or
	// expired provider operation is unknown, never proof of non-submission.
	if !now.Before(op.SubmittedAt.Add(24 * time.Hour)) {
		op.Status, op.Detail = "unconfirmed", "Azure processing could not be confirmed within 24 hours. This message will not be submitted again."
	} else {
		op.NextPollAt = now.Add(5 * time.Minute)
		resolved, resolveErr := a.sender(ctx, op.AccountID)
		sender, ok := resolved.(*ACSSender)
		if resolveErr != nil || !ok || sender.cfg.Endpoint != op.Endpoint {
			op.Detail = "Reconnect the original organization's Azure resource to check this send."
		} else {
			if a.operationClient != nil {
				sender.client = a.operationClient
			}
			sender.now = a.clock
			request, cancel := context.WithTimeout(ctx, 20*time.Second)
			status, wait, pollErr := sender.getOperation(request, op.ID)
			cancel()
			op.NextPollAt = now.Add(wait)
			if pollErr != nil {
				op.Detail = pollErr.Error()
			} else {
				op.Status, op.Detail, op.CheckedAt = status, "", now
				if status == "failed" || status == "canceled" {
					op.Detail = "Azure did not complete processing. This is not a recipient bounce."
				}
			}
		}
	}
	_, err = store.write(ctx, key, op, prior)
	return err
}

// OperationPoller is cluster transport maintenance. It uses no editor/Cockpit
// worker and no process-local receipt state. Shared record locks are the claim.
type OperationPoller struct {
	resolve     func() *Integration
	logger      *slog.Logger
	start, stop sync.Once
	mu          sync.Mutex
	cancel      context.CancelFunc
	ready, done chan struct{}
	running     atomic.Bool
}

func NewOperationPoller(resolve func() *Integration, logger *slog.Logger) *OperationPoller {
	if logger == nil {
		logger = slog.Default()
	}
	return &OperationPoller{resolve: resolve, logger: logger, ready: make(chan struct{}), done: make(chan struct{})}
}
func (p *OperationPoller) Start(_ context.Context) {
	p.start.Do(func() {
		ctx, cancel := context.WithCancel(context.Background())
		p.mu.Lock()
		p.cancel = cancel
		p.mu.Unlock()
		p.running.Store(true)
		close(p.ready)
		go func() {
			defer close(p.done)
			defer p.running.Store(false)
			ticker := time.NewTicker(30 * time.Second)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					if i := p.resolve(); i != nil {
						pass, cancel := context.WithTimeout(ctx, 25*time.Second)
						err := i.PollOperations(pass)
						cancel()
						if err != nil && ctx.Err() == nil {
							p.logger.Warn("Azure email receipt reconciliation delayed", "error", err)
						}
					}
				}
			}
		}()
	})
}
func (p *OperationPoller) Stop(_ context.Context) {
	p.stop.Do(func() {
		p.mu.Lock()
		cancel := p.cancel
		p.mu.Unlock()
		if cancel != nil {
			cancel()
			<-p.done
		}
	})
}
func (p *OperationPoller) IsRunning() bool                     { return p.running.Load() }
func (p *OperationPoller) Order() int                          { return 12 }
func (p *OperationPoller) ComponentName() common.ComponentName { return "email.operations" }
func (p *OperationPoller) Ready() <-chan struct{}              { return p.ready }
