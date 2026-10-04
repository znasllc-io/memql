package pipelinerun

import "errors"

// The plain errors this package answers with. A refusal a person can act on
// carries a catalogued code instead (a *pipelines.Refusal, or a
// component/packages *Refusal for a fact about the source or its grant); these
// are the answers the catalogue has no code for, each a sentence that names
// what to do.
var (
	// ErrNotOwner is a person acting on a source, pipeline or run they can
	// read but do not own -- a cluster owner reading a colleague's source.
	// A pipeline runs under its owner's GitHub grant, so only the owner may
	// connect one.
	ErrNotOwner = errors.New("pipelines: only the source's owner can do this; a pipeline runs under its owner's GitHub connection")
	// ErrNoCaller is a person-facing act reached with no person behind it.
	ErrNoCaller = errors.New("pipelines: no signed-in person is making this request")
	// ErrRunNotFound is a run no row answers for.
	ErrRunNotFound = errors.New("pipelines: no such run")
	// ErrRunFinished is an act that needs a run still going, on one that
	// already concluded.
	ErrRunFinished = errors.New("pipelines: this run has already finished")
	// ErrRunInProgress is a re-run of a run key whose newest attempt has
	// not finished: a second attempt of a run still going is two runs of one
	// commit, and the first one's check run would be overwritten mid-flight.
	ErrRunInProgress = errors.New("pipelines: this commit's run has not finished yet; cancel it or wait for it before running it again")
	// ErrTreeTooLarge is a repository whose kept files exceed the cap a
	// tree read was given.
	ErrTreeTooLarge = errors.New("pipelines: the repository's files exceed what a pipeline reads")
	// ErrClientOrigin is the trigger or the poll reached by anything but
	// the engine's own automations. Both read every owner's pipelines and
	// open runs that write check runs on GitHub, and a builtin is callable
	// by any signed-in client, so each refuses a call that did not arrive
	// with internal origin -- before it reads anything.
	ErrClientOrigin = errors.New("pipelines: this capability is driven by the engine's own automations and refuses a call from a client")
	// ErrDeliveryUnverified is a staged GitHub delivery whose signature the
	// inbound receiver did not verify: a source configured to sign nothing.
	// An unsigned body is anybody's, and never opens a run.
	ErrDeliveryUnverified = errors.New("pipelines: this delivery's signature was not verified, and an unsigned delivery never opens a run; configure the github inbound source with the GitHub App's webhook secret")

	errNoGate   = errors.New("pipelines: no cross-replica gate is wired on this node, so no run can be opened or changed safely")
	errNoStore  = errors.New("pipelines: no store is wired on this node")
	errNoGitHub = errors.New("pipelines: no GitHub client is wired on this node")
)
