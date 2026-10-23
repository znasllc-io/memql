//go:build !darwin && !linux

package tools

import "errors"

var errPipelineBusy = errors.New("build capacity is occupied")
var errPipelineUnreconciled = errors.New("interrupted build needs reconciliation")

func hasPipelineAttempt() (bool, error) { return false, nil }

type pipelineReservation struct{ dirty bool }
type pipelineAttemptRecord struct {
	Execution, Container, DockerID, Workspace, Network string
	Services                                           []string
}

func acquirePipelineReservation() (*pipelineReservation, error) {
	return nil, errors.New("build capacity admission is supported only on macOS and Linux")
}
func (*pipelineReservation) read() (pipelineAttemptRecord, error) {
	return pipelineAttemptRecord{}, errPipelineUnreconciled
}
func (*pipelineReservation) begin(pipelineAttemptRecord) error { return errPipelineUnreconciled }
func (*pipelineReservation) cleanupReady(string) error         { return errPipelineUnreconciled }
func (*pipelineReservation) clear() error                      { return errPipelineUnreconciled }
func (*pipelineReservation) close()                            {}
