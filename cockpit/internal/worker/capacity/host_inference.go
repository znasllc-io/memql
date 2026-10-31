package capacity

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"
)

// InferenceActivityFile is separate from the lock inode: process death releases
// the kernel lock, but does not establish that a local runtime stopped its work.
const InferenceActivityFile = "inference-active.json"

// InferenceFenceBound is how long a local model call is taken to run when its
// record names no end of its own: a record an older Cockpit wrote, which held
// no time, or one whose write did not complete. It ends that long after its
// file was written.
//
// No Cockpit limit bounds a call's length: modelcall.DefaultTimeout (10
// minutes) is the envelope's default deadline, and an envelope may name a
// longer one. So this is a chosen bound, 30 minutes, three times that default.
// A record this Cockpit writes names the call's own deadline and does not use
// it.
const InferenceFenceBound = 30 * time.Minute

// inferenceRecordVersion is the record that names its end. Version 1, written
// by older Cockpits, named none.
const inferenceRecordVersion = 2

var ErrInferenceUnreconciled = errors.New("a local model call may still be running on this machine")

// inferenceRecord is the fence file's contents. Attempt is a random identity
// that completion compares byte for byte, so a call removes only its own
// record. Until is when the call cannot still be running. Nothing else is
// kept: no prompt, model, endpoint, credential or process id.
type inferenceRecord struct {
	Version int    `json:"version"`
	Attempt string `json:"attempt"`
	Until   string `json:"until,omitempty"`
}

// InferenceRecord is the contents of a new fence for the call identified by
// attempt, in force until until.
func InferenceRecord(attempt string, until time.Time) ([]byte, error) {
	return json.Marshal(inferenceRecord{Version: inferenceRecordVersion, Attempt: attempt, Until: until.UTC().Format(time.RFC3339Nano)})
}

// CheckInferenceIdle answers nil when no local model call can still be using
// this machine, and ErrInferenceUnreconciled, naming when the fence ends, while
// one may. The caller holds pipeline.lock shared, so a model call may be
// writing its own record at the same moment: a record past its end reads as
// idle and is left in place. A caller that excludes every writer uses
// CheckInferenceIdleExclusive, which also removes it.
//
// WHY A FENCE THAT ENDS. A runtime can go on generating after its caller stops
// waiting for it (a cancel, a superseded run, a dropped stream, the idle
// watchdog, a worker that died), and nothing on this side sees it stop. Only a
// confirmed terminal response clears the record early (the runtime caller
// removes its own witness). Otherwise the record holds until the time it
// names, the call's own deadline, past which the call's request has been cut
// off. A fence that only success could clear turned one routine cancel into a
// machine refusing pipeline steps, workbenches and model calls until somebody
// removed the file by hand. The headroom checks still measure what is actually
// free, so a generation still finishing after its deadline is bounded by them.
//
// WHAT IS NEVER INTERPRETED. A record that is not a regular file (a symlink, a
// directory) is a fence that never ends and is never removed: this Cockpit
// never writes one, so nothing says when it ends. A record that names no time
// (an older Cockpit's, or a write that did not complete) ends
// InferenceFenceBound after its file was last written. Never infer completion
// from a worker PID.
//
// TestAnInferenceFenceEndsAtTheTimeItNames,
// TestAnInferenceRecordWithNoTimeEndsTheBoundAfterItWasWritten and, through a
// real cancelled call, modelcall's
// TestACancelledCallFencesTheMachineUntilItsDeadline cover it.
func CheckInferenceIdle(root string) error {
	return checkInferenceIdle(root, false)
}

// CheckInferenceIdleExclusive is CheckInferenceIdle for a caller that excludes
// every writer of the record: it holds inference.lock, which every model call
// holds from before it writes its record until after it removes it, or
// pipeline.lock exclusively, which no model call can hold beside it. Only then
// can a record past its end be removed by its path without the risk of
// removing a record a call has just written.
func CheckInferenceIdleExclusive(root string) error {
	return checkInferenceIdle(root, true)
}

func checkInferenceIdle(root string, removeStale bool) error {
	path := filepath.Join(root, InferenceActivityFile)
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect retained inference operation: %w", err)
	}
	if !info.Mode().IsRegular() {
		return ErrInferenceUnreconciled
	}
	end, err := inferenceEnd(path, info)
	if err != nil {
		return err
	}
	if !time.Now().After(end) {
		return fmt.Errorf("%w (until %s at the latest)", ErrInferenceUnreconciled, end.UTC().Format(time.RFC3339))
	}
	if !removeStale {
		return nil
	}
	// The same file that was read, or nothing: a record replaced since is not
	// this one, and is read again by the next caller.
	current, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil || !os.SameFile(info, current) {
		return ErrInferenceUnreconciled
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove an inference record past its end: %w", err)
	}
	return nil
}

// inferenceEnd is when the record at path stops being in force: the time it
// names, or, for one that names none, InferenceFenceBound after its file was
// last written. A file replaced between the two looks is not interpreted.
func inferenceEnd(path string, info os.FileInfo) (time.Time, error) {
	file, err := os.Open(path)
	if err != nil {
		return time.Time{}, fmt.Errorf("read retained inference operation: %w", err)
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !os.SameFile(info, opened) {
		return time.Time{}, ErrInferenceUnreconciled
	}
	data, err := io.ReadAll(io.LimitReader(file, 4096))
	if err != nil {
		return time.Time{}, fmt.Errorf("read retained inference operation: %w", err)
	}
	var record inferenceRecord
	if json.Unmarshal(data, &record) == nil && record.Version == inferenceRecordVersion {
		if until, err := time.Parse(time.RFC3339, record.Until); err == nil {
			return until, nil
		}
	}
	return opened.ModTime().Add(InferenceFenceBound), nil
}
