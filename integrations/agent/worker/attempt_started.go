package worker

import (
	"sync"

	nodev1 "github.com/znasllc-io/memql/component/node/gen"
)

// AttemptStarted is where a dispatch STARTED (Request.OnStarted): the machine
// whose first output reached the dispatcher, named as Result names it --
// MachineName is the display name its owner chose (ownerChosenName), never the
// hostname it reported, because the pipeline executor writes it into a step's
// log, which can reach a public check run -- with its connection as placement
// judged it, nil when no drops were read.
type AttemptStarted struct {
	WorkerId    string
	MachineName string
	Connection  *ConnectionHealth
}

// startedOn arranges one attempt's OnStarted call. The request's output
// callback is wrapped so the attempt's FIRST chunk, delivered here or relayed
// by a forward from the sibling holding the stream, calls OnStarted once and
// then is delivered. Output is the one proof, before the result, that the call
// started on this machine: a refusal before start sends none, so a machine
// that refused is never said to have started the call. The wrap is per
// attempt because each candidate is a different machine. Covered by
// TestTheFirstOutputSaysWhereTheStepStarted and, across the hop,
// TestTheFirstOutputAcrossTheHopSaysWhereTheCallStarted.
func startedOn(req Request, cand Candidate, health *ConnectionHealth) Request {
	onStarted := req.OnStarted
	if onStarted == nil {
		return req
	}
	started := AttemptStarted{WorkerId: cand.RegistrationId, MachineName: ownerChosenName(cand), Connection: health}
	next := req.OnStreamChunk
	var once sync.Once
	req.OnStreamChunk = func(chunk *nodev1.WorkerForwardStream) {
		once.Do(func() { onStarted(started) })
		if next != nil {
			next(chunk)
		}
	}
	return req
}
