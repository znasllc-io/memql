package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"time"
)

// The installed builtin has no caller idempotency key. Keep a durable local
// intent even after failure or a crash-before-call gap; absence of a receipt is
// never permission to replay. This is a guard for this operator directory, not
// a replacement for the server gate or cross-operator reconciliation.
func executeFailedOnlyOnce(ctx context.Context, dir string, o options, invoke func() (any, error)) (any, error) {
	return executeFailedOnlyWithSync(ctx, dir, o, invoke, syncIntentDirectory)
}

func syncIntentDirectory(path string) error {
	d, err := os.Open(path)
	if err != nil {
		return err
	}
	err = d.Sync()
	closeErr := d.Close()
	if err != nil {
		return err
	}
	return closeErr
}

func executeFailedOnlyWithSync(ctx context.Context, dir string, o options, invoke func() (any, error), syncDirectory func(string) error) (any, error) {
	if _, err := selectedRerun(o); err != nil {
		return nil, err
	}
	if o.op != "rerun" || rerunMode(o) != rerunFailedOnly || !o.execute {
		return nil, errors.New("failed-only intent requires explicit execution")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	// Only one directory below the existing executable parent is supported.
	if err := os.Mkdir(dir, 0700); err != nil && !os.IsExist(err) {
		return nil, errors.New("cannot prepare rerun intent directory")
	}
	// Persist the directory entry itself, including the first invocation. A
	// durable marker in a directory whose parent entry can disappear is not a
	// durable replay guard.
	if err := syncDirectory(filepath.Dir(dir)); err != nil {
		return nil, errors.New("cannot persist intent parent directory; no native call made")
	}
	path := filepath.Join(dir, o.run+".json")
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return nil, errors.New("rerun intent already exists or cannot be reserved; reconcile native runs and GitHub checks, never blindly replay")
	}
	// Any subsequent error intentionally leaves the marker in place.
	err = json.NewEncoder(f).Encode(map[string]any{
		"state": "intent-recorded", "sourceRunId": o.run, "failedOnly": true,
		"nativeCall": rerunCall(o), "engineCommit": failedOnlyEngineCommit,
		"recordedAt": time.Now().UTC().Format(time.RFC3339Nano),
	})
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil || closeErr != nil {
		return nil, errors.New("cannot persist rerun intent; no native call made")
	}
	if err := syncDirectory(dir); err != nil {
		return nil, errors.New("cannot persist intent directory; no native call made")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return invoke() // Exactly one call, including errors and uncertain responses.
}
