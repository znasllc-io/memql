package pipelines

// ReleaseBuildRequest names a selected source. Empty ExpectedSHA and binding
// fields are allowed only for the first read; effects require the full binding.
// CredentialID is a nonsecret source identity, never a bearer or token.
type ReleaseBuildRequest struct {
	PipelineID, SourceID, CredentialID, Repository, Branch, ExpectedSHA, Tag string
	InstallationID, RepositoryID                                             int64
}

// ReleaseBuildSource is fresh provider evidence, not authority supplied by a
// caller. ProofSHA identifies where checks actually ran; it may be the exact
// tree-equivalent head of the PR which produced SHA, never an arbitrary commit.
type ReleaseBuildSource struct {
	OwnerUserID, PipelineID, SourceID, CredentialID, Repository, Branch string
	HeadSHA, SHA, TreeSHA, ProofSHA                                     string
	InstallationID, RepositoryID                                        int64
	ProofPullRequest                                                    int
	TagExists                                                           bool
	TagSHA                                                              string
	Checks                                                              []ReleaseBuildCheck
	ExistingRun                                                         *ReleaseBuildRun
}

type ReleaseBuildCheck struct {
	Context                 string
	AppID                   int64
	SHA, Status, Conclusion string
}

// ReleaseBuildRun is the native run's identity and lifecycle only. A projection
// never carries a worker credential, raw policy snapshot or command payload.
type ReleaseBuildRun struct {
	ID, OwnerUserID, PipelineID, Repository, SHA, Tag string
	Mode, Event, Status, Conclusion, WorkRunID        string
	Attempt                                           int
	CancelRequested                                   bool
	// RerunOf is the attempt this one re-runs; empty for attempt 1.
	//
	// Both re-run fields are omitted when empty. Saved-release journals store
	// this type inside the selection their digest covers, and an older engine
	// decodes it strictly, so a run without re-run facts must encode exactly
	// as it did before these fields existed. The JSON keys stay the Go names:
	// the journal's SQL fence reads run.Chain (integrations/release/build_ledger.go).
	RerunOf string `json:"RerunOf,omitempty"`
	// Chain lists the earlier attempts of the same release run key, newest
	// first, read through rerunOf. It is nil for attempt 1 and for a chain
	// that cannot be followed back to attempt 1 inside its run key, so a saved
	// release adopts a re-run only through an unbroken chain
	// (integrations/release/build_adopt.go).
	Chain []ReleaseBuildAttempt `json:"Chain,omitempty"`
}

// ReleaseBuildAttempt is one earlier attempt's lifecycle: enough to decide
// whether a later attempt of the same release may replace it.
type ReleaseBuildAttempt struct {
	ID                 string
	Attempt            int
	Status, Conclusion string
	CancelRequested    bool
}
