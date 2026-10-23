//go:build !darwin && !linux

package workbench

import "errors"

var ErrBusy = errors.New("fleet workbench capacity is occupied")
var ErrInterrupted = errors.New("fleet workbench recovery is required")
var errUnsupported = errors.New("Fleet workbenches require macOS or Linux")

type reservation struct{}
type record struct{ Name, Key, DockerID string }

func capacityRoot() (string, error)                       { return "", errUnsupported }
func reserve(string) (*reservation, error)                { return nil, errUnsupported }
func reserveAt(string, int) (*reservation, error)         { return nil, errUnsupported }
func (*reservation) previous() (record, error)            { return record{}, errUnsupported }
func (*reservation) admission() ([]record, func(), error) { return nil, nil, errUnsupported }
func (*reservation) save(record) error                    { return errUnsupported }
func (*reservation) clear() error                         { return errUnsupported }
func (*reservation) close()                               {}
func HasInterrupted(string) (bool, error)                 { return false, errUnsupported }
