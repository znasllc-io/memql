// Package repositoryauthority journals bounded change requests and revalidates
// maintainer delegation at effects. Installed DSL selects the route and recipe;
// neither source manifests nor callers select an owner or manufacture approval.
package repositoryauthority

import (
	"context"
	"database/sql"
	"github.com/znasllc-io/memql/component/automations/workflowhost"
	"github.com/znasllc-io/memql/component/packages"
	"time"
)

const ResolverVariable = "MEMQL_REPOSITORY_AUTHORITY_RESOLVER"
const BindingsVariable = "MEMQL_REPOSITORY_AUTHORITY_BINDINGS"
const ReviewType = "repository-change"
const OperationPropose = "propose_change"
const RequestCapability = "app:pipelines/requests"

type Source struct{ ID, OwnerUserID, CredentialID, Repository string }
type Request struct {
	SourceID        string   `json:"sourceId"`
	RequestID       string   `json:"requestId"`
	Statement       string   `json:"statement"`
	Operation       string   `json:"operation"`
	Paths           []string `json:"paths"`
	ParentRequestID string   `json:"parentRequestId,omitempty"`
}

// Transfer is request-only. The receiving host supplies its authenticated
// requester and trusted authority route. No owner, template, or decision travels.
type Transfer struct {
	RequestID       string   `json:"requestId"`
	Repository      string   `json:"repository"`
	RepositoryID    int64    `json:"repositoryId"`
	Statement       string   `json:"statement"`
	Operation       string   `json:"operation"`
	Paths           []string `json:"paths"`
	ParentRequestID string   `json:"parentRequestId,omitempty"`
}
type Receipt struct {
	Statement       string   `json:"statement"`
	Paths           []string `json:"paths"`
	ID              string   `json:"id"`
	Repository      string   `json:"repository"`
	RepositoryID    int64    `json:"repositoryId"`
	State           string   `json:"state"`
	Reason          string   `json:"reason,omitempty"`
	GoalID          string   `json:"goalId,omitempty"`
	RunID           string   `json:"runId,omitempty"`
	ApprovalID      string   `json:"approvalId,omitempty"`
	ExpiresAt       string   `json:"expiresAt"`
	ParentRequestID string   `json:"parentRequestId,omitempty"`
}
type Access struct {
	SourceID     string `json:"sourceId"`
	Repository   string `json:"repository"`
	RepositoryID int64  `json:"repositoryId"`
	CanWrite     bool   `json:"canWrite"`
	CanRequest   bool   `json:"canRequest"`
	Route        string `json:"route"`
}
type GoalRequest struct {
	Key, OwnerUserID, Template, Statement string
	Input, Subject                        map[string]any
	Immediate                             bool
	BeforeRun                             func(context.Context, string, string) error
}
type GoalReceipt struct{ GoalID, RunID, ApprovalID string }
type Decision struct{ State, Reason, GoalID, RunID, RequestID, Fingerprint string }

// Authorization is a short-lived native observation, not a serializable grant.
// Effect adapters must call AuthorizeEffect immediately before each write.
type Authorization struct {
	EffectID        string
	EffectCreatedAt time.Time
	Source          Source
	RepositoryID    int64
	RequestID       string
}

type Deps struct {
	DB *sql.DB
	// Source with trustedBinding=false MUST enforce the caller's row visibility.
	// true is only used for an ID selected by the trusted installed resolver.
	Source           func(context.Context, string, bool) (Source, error)
	RepositoryAccess func(context.Context, Source) (packages.RepositoryAuthority, error)
	SystemVariable   func(context.Context, string) (string, error)
	WorkflowSource   workflowhost.SourceLoader
	OpenGoal         func(context.Context, GoalRequest) (GoalReceipt, error)
	// Decision reads the exact approval under its stored owner, validating its
	// goal/run linkage. Missing, cancelled, ambiguous or revoked answers deny.
	Decision func(context.Context, string, string) (Decision, error)
	// ExecutionActive freshly verifies the bound goal/run is live, open and
	// uncancelled. Context alone is not proof that a recovered run may still act.
	ExecutionActive func(context.Context, string, string, string) error
	// RemoteBinding fingerprints the operator-selected connection and its
	// authenticated remote requester, without storing credential material.
	// Both transports MUST revalidate that binding on their actual connection.
	RemoteBinding func(context.Context, string) (string, error)
	RemoteRequest func(context.Context, string, string, Transfer) (Receipt, error)
	RemoteStatus  func(context.Context, string, string, string) (Receipt, error)
	Read          func(context.Context, Authorization, []string) (RepositorySnapshot, error)
	Propose       func(context.Context, Authorization, Proposal) (ProposalReceipt, error)
	Now           func() time.Time
}

type Replacement struct {
	Path         string `json:"path"`
	BeforeSHA256 string `json:"beforeSHA256"`
	Content      string `json:"content"`
}
type Proposal struct {
	RequestID    string        `json:"requestId"`
	ExpectedHead string        `json:"expectedHead"`
	BaseBranch   string        `json:"baseBranch"`
	Title        string        `json:"title"`
	Body         string        `json:"body"`
	Files        []Replacement `json:"files"`
}
type ProposalReceipt struct {
	State          string `json:"state"`
	Branch         string `json:"branch,omitempty"`
	Commit         string `json:"commit,omitempty"`
	PullRequestURL string `json:"pullRequestUrl,omitempty"`
}

type RepositorySnapshot struct {
	ExpectedHead string        `json:"expectedHead"`
	BaseBranch   string        `json:"baseBranch"`
	Files        []Replacement `json:"files"`
}
