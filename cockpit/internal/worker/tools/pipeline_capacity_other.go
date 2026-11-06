//go:build !darwin && !linux

package tools

import "errors"

var errPipelineBusy = errors.New("build capacity is occupied")
var errPipelineUnreconciled = errors.New("interrupted build needs reconciliation")
var errPipelineCapacityUnsupported = errors.New("build capacity admission is supported only on macOS and Linux")

func hasPipelineAttempt() (bool, error) { return false, nil }

type pipelineReservation struct {
	dirty bool
	use   pipelineSlotUse
}
type pipelineAttemptRecord struct {
	Execution, Container, DockerID, DockerEndpoint, Workspace, Network string
	Services                                                           []string
}

func acquirePipelineReservation() (*pipelineReservation, error) {
	return nil, errPipelineCapacityUnsupported
}
func acquirePipelineSlot(int, pipelineSlotUse) (*pipelineReservation, error) {
	return nil, errPipelineCapacityUnsupported
}
func claimInterruptedPipelineSlots(*pipelineReservation) ([]*pipelineReservation, error) {
	return nil, errPipelineCapacityUnsupported
}
func pipelineInterruptedAttempt() bool                 { return true }
func (*pipelineReservation) others() []pipelineSlotUse { return nil }
func (*pipelineReservation) read() (pipelineAttemptRecord, error) {
	return pipelineAttemptRecord{}, errPipelineUnreconciled
}
func (*pipelineReservation) begin(pipelineAttemptRecord) error { return errPipelineUnreconciled }
func (*pipelineReservation) cleanupReady(string) error         { return errPipelineUnreconciled }
func (*pipelineReservation) clear() error                      { return errPipelineUnreconciled }
func (*pipelineReservation) close()                            {}
