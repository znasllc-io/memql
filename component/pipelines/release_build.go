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
}
