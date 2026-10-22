package app

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/znasllc-io/memql/core/id"
)

// Independent SQL sessions model different replicas. The database's statement
// timeout must still protect slow SQL without making ordinary lock contention
// discard a completed pipeline log before its Library window has elapsed.
func TestPipelineArtifactOwnerLockSurvivesStatementTimeoutWindow(t *testing.T) {
	_, first := pipelineScopeSQLStore(t)
	_, second := pipelineScopeSQLStore(t)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	owner := id.NewShortId()
	holder, err := first.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Rollback()
	if err := lockPipelineUploadOwner(ctx, holder, owner); err != nil {
		t.Fatal(err)
	}
	waiter, err := second.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer waiter.Rollback()
	if _, err := waiter.ExecContext(ctx, "SET LOCAL statement_timeout='30ms'"); err != nil {
		t.Fatal(err)
	}
	result := make(chan error, 1)
	go func() { result <- lockPipelineUploadOwner(ctx, waiter, owner) }()
	select {
	case err := <-result:
		t.Fatalf("contended lock returned before its holder released: %v", err)
	case <-time.After(150 * time.Millisecond):
	}
	if err := holder.Commit(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-result:
		if err != nil {
			t.Fatalf("waiting beyond statement timeout lost the transaction: %v", err)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	var value int
	if err := waiter.QueryRowContext(ctx, "SELECT 1").Scan(&value); err != nil || value != 1 {
		t.Fatalf("acquired transaction is unusable: %d %v", value, err)
	}
	if err := waiter.Commit(); err != nil {
		t.Fatal(err)
	}
}

func TestPipelineArtifactOwnerLockCancellationAndOwnerIsolation(t *testing.T) {
	_, first := pipelineScopeSQLStore(t)
	_, second := pipelineScopeSQLStore(t)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	holder, err := first.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Rollback()
	owner := id.NewShortId()
	if err := lockPipelineUploadOwner(ctx, holder, owner); err != nil {
		t.Fatal(err)
	}
	waiter, err := second.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer waiter.Rollback()
	// A different owner's work can proceed while this owner is busy.
	if err := lockPipelineUploadOwner(ctx, waiter, id.NewShortId()); err != nil {
		t.Fatal(err)
	}
	short, stop := context.WithTimeout(ctx, 50*time.Millisecond)
	defer stop()
	if err := lockPipelineUploadOwner(short, waiter, owner); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("wait ignored the caller's deadline: %v", err)
	}
	if err := waiter.Rollback(); err != nil {
		t.Fatal(err)
	}
	if err := lockPipelineUploadOwner(ctx, waiter, owner); !errors.Is(err, sql.ErrTxDone) {
		t.Fatalf("SQL failure was retried or hidden: %v", err)
	}
}
