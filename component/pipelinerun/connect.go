package pipelinerun

import (
	"context"
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strings"

	"github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/packages"
	"github.com/znasllc-io/memql/component/pipelines"
)

// connect.go -- a source's one pipeline (decisions 8, 12, 14).
//
// Connecting proves three things before it writes anything: the caller owns
// the source; the source's GitHub grant reaches the repository (a token is
// minted through it, which runs verifyGrantRepository); and the repository
// declares a pipeline block that validates. A pipeline that could never
// compile is never connected.

// maxManifestBytes caps the manifest read at connect.
const maxManifestBytes = 1 << 20

// ConnectRequest is pipelinesConnect's arguments.
type ConnectRequest struct {
	PackageID string
	Delivery  string
	// Compute empty is cluster: nothing about a laptop is a default.
	Compute pipelines.Compute
	// SecretNames are globalSecret NAMES -- the allowlist. Never values.
	SecretNames []string
}

// ConnectResult is what a connect answers.
type ConnectResult struct {
	PipelineID  string
	Repository  string
	Delivery    string
	Compute     pipelines.Compute
	Stages      []string
	Reconnected bool
}

// secretNameRe is the rule pipelines.Validate holds a step's secret to, and
// the allowlist is held to the same one: a name the compile would refuse
// could never be resolved, so allowing it would be a promise nothing keeps.
var secretNameRe = regexp.MustCompile(`^[A-Z][A-Z0-9_]{0,127}$`)

// reservedSecretPrefix is the platform's own environment; a secret under it
// never resolves, so the allowlist refuses it at the door too.
const reservedSecretPrefix = "MEMQL_"

func (i *Integration) handleConnect(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	names, _ := stringListArg(args, "secretNames")
	res, err := i.Connect(handlerContext(ctx), ConnectRequest{
		PackageID:   stringArg(args, "packageId"),
		Delivery:    stringArg(args, "delivery"),
		Compute:     pipelines.Compute(stringArg(args, "compute")),
		SecretNames: names,
	})
	if err != nil {
		return nil, err
	}
	return resultNode(map[string]any{
		"pipelineId":  res.PipelineID,
		"repository":  res.Repository,
		"delivery":    res.Delivery,
		"compute":     string(res.Compute),
		"stages":      res.Stages,
		"reconnected": res.Reconnected,
	}), nil
}

// Connect connects, or reconnects, the pipeline of one of the caller's
// sources.
func (i *Integration) Connect(ctx context.Context, req ConnectRequest) (ConnectResult, error) {
	caller, err := personFrom(ctx)
	if err != nil {
		return ConnectResult{}, err
	}
	d := i.snapshot()
	if d.Store == nil {
		return ConnectResult{}, errNoStore
	}
	if d.GitHub == nil {
		return ConnectResult{}, errNoGitHub
	}

	packageID := strings.TrimSpace(req.PackageID)
	if packageID == "" {
		return ConnectResult{}, fmt.Errorf("pipelines: packageId is required")
	}
	delivery := strings.ToLower(strings.TrimSpace(req.Delivery))
	if delivery != DeliveryWebhook && delivery != DeliveryPoll {
		return ConnectResult{}, fmt.Errorf("pipelines: delivery is %q; it is webhook (GitHub delivers to this cluster) or poll (this cluster asks GitHub every minute)", req.Delivery)
	}
	compute := pipelines.Compute(strings.ToLower(strings.TrimSpace(string(req.Compute))))
	switch compute {
	case "":
		compute = pipelines.ComputeCluster
	case pipelines.ComputeCluster, pipelines.ComputeClusterAndFleet:
	default:
		return ConnectResult{}, fmt.Errorf("pipelines: compute is %q; it is %s or %s", req.Compute, pipelines.ComputeCluster, pipelines.ComputeClusterAndFleet)
	}
	secretNames, refusal := allowlist(req.SecretNames)
	if refusal != nil {
		return ConnectResult{}, refusal
	}

	// THE CALLER'S OWN READ of the source: a package they cannot read
	// answers nothing, and "not there" and "not yours" are the same zero
	// rows -- the sentence must not claim to know which.
	pkg, err := d.Store.PackageForCaller(ctx, packageID)
	if err != nil {
		return ConnectResult{}, err
	}
	if pkg == nil {
		return ConnectResult{}, &packages.Refusal{Code: packages.CodeSourceUnreadable,
			Detail: fmt.Sprintf("no source %q is readable by you", packageID)}
	}
	// A cluster owner reads every source; only its owner connects one,
	// because every token the pipeline holds is minted through the owner's
	// grant.
	if !sameID(pkg.OwnerUserID, caller) {
		return ConnectResult{}, ErrNotOwner
	}
	if pkg.Status != "" && pkg.Status != "active" {
		return ConnectResult{}, &packages.Refusal{Code: packages.CodeSourceUnreadable,
			Detail: "this source is archived; restore it before connecting a pipeline to it"}
	}
	if pkg.SourceKind != "repo" {
		return ConnectResult{}, &packages.Refusal{Code: packages.CodeSourceHostUnsupported,
			Detail: "a pipeline runs on a GitHub repository source, and this source is a zip from the Library: add the repository as a source to connect its pipeline"}
	}
	repository, err := githubRepository(pkg.RepoURL)
	if err != nil {
		return ConnectResult{}, err
	}
	if pkg.CredentialID == "" {
		return ConnectResult{}, &packages.Refusal{Code: packages.CodeCredentialNotFound,
			Detail: "this source names no GitHub connection, and a pipeline's checks are minted only through its owner's: switch the source to your GitHub connection first"}
	}
	// One pipeline per repository, asked BEFORE any request leaves the
	// cluster so a second source learns it at once; asked again under the
	// repository's gate before the write, which is the answer that counts.
	if refusal, err := alreadyConnected(ctx, d, repository, pkg.ID); err != nil || refusal != nil {
		return ConnectResult{}, firstErr(err, refusal)
	}

	// THE GRANT: minting proves it still reaches the repository, and names
	// the installation deliveries must arrive through.
	token, installationID, err := d.GitHub.InstallationToken(ctx, pkg.CredentialID, pkg.OwnerUserID, repository)
	if err != nil {
		return ConnectResult{}, err
	}
	info, err := d.GitHub.Repository(ctx, token, repository)
	if err != nil {
		return ConnectResult{}, fmt.Errorf("pipelines: reading %s: %w", repository, err)
	}
	branch := strings.TrimSpace(info.DefaultBranch)
	if branch == "" {
		return ConnectResult{}, fmt.Errorf("pipelines: GitHub names no default branch for %s", repository)
	}
	head, _, err := d.GitHub.BranchHead(ctx, token, repository, branch)
	if err != nil {
		return ConnectResult{}, fmt.Errorf("pipelines: reading %s's head: %w", branch, err)
	}
	tree, err := d.GitHub.Tree(ctx, token, repository, head, func(p string) bool { return p == pipelines.ManifestPath }, maxManifestBytes)
	if err != nil {
		return ConnectResult{}, fmt.Errorf("pipelines: reading %s at %s: %w", pipelines.ManifestPath, branch, err)
	}
	manifest, err := packages.ReadManifest(tree)
	if err != nil {
		return ConnectResult{}, err
	}
	if manifest.Pipeline == nil {
		return ConnectResult{}, pipelines.Refuse(pipelines.CodeNotDeclared, "",
			"%s at the head of %s declares no pipeline block, so there is nothing to connect.", pipelines.ManifestPath, branch)
	}
	if refusal := pipelines.Validate(manifest.Pipeline); refusal != nil {
		return ConnectResult{}, refusal
	}
	// Consent, of EVERY step whatever event would plan it: a step naming a
	// need on a cluster-only pipeline, or a secret this allowlist does not
	// hold, is refused now rather than on the first push that reaches it.
	if refusal := pipelines.Consent(manifest.Pipeline, compute, secretNames); refusal != nil {
		return ConnectResult{}, refusal
	}

	p := Pipeline{
		ID:             PipelineIDFor(pkg.ID),
		OwnerUserID:    pkg.OwnerUserID,
		AccountID:      pkg.AccountID,
		PackageID:      pkg.ID,
		Name:           manifest.Name,
		Repository:     repository,
		DefaultBranch:  branch,
		InstallationID: installationID,
		CredentialID:   pkg.CredentialID,
		Delivery:       delivery,
		Compute:        compute,
		SecretNames:    secretNames,
	}
	var (
		reconnected bool
		movedFrom   string
	)
	// ONE gate, the repository's (ids.go), and every GitHub read above
	// already done: two sources connecting one repository at once serialize
	// here and the second finds the first, and the pipeline row's other
	// writers -- a disconnect, the poll's heads, the timings merge -- take the
	// same key, so nothing here nests.
	err = d.gate(ctx, RepositoryGateKey(repository), func(gctx context.Context) error {
		fresh := memql.ContextWithFreshRead(gctx)
		refusal, err := alreadyConnected(fresh, d, repository, pkg.ID)
		if err != nil || refusal != nil {
			return firstErr(err, refusal)
		}
		existing, err := d.Store.PipelineByID(fresh, p.ID)
		if err != nil {
			return err
		}
		reconnected = existing != nil
		if existing != nil && existing.Repository != p.Repository {
			// The source now points at another repository: what the poll
			// saw there is not a baseline here. Cleared BEFORE the row
			// names the new repository, so no poll ever reads the new
			// repository beside the old one's heads and diffs one against
			// the other.
			movedFrom = existing.Repository
			if len(existing.Heads) > 0 {
				if err := d.Store.UpdatePipeline(gctx, existing.OwnerUserID, p.ID, PipelinePatch{Heads: ptr(map[string]string{})}); err != nil {
					return err
				}
			}
		}
		return d.Store.CreatePipeline(gctx, p)
	})
	if err != nil {
		return ConnectResult{}, err
	}
	if movedFrom != "" {
		// The fence for the repository the pipeline left. A poll that read
		// the row before the move writes its heads back under the OLD
		// repository's key, which the section above did not hold; this one
		// does, after it, so such a write lands before it and is cleared
		// here, or after it and finds the row on another repository and
		// writes nothing (writeHeads' compare-and-swap).
		err = d.gate(ctx, RepositoryGateKey(movedFrom), func(gctx context.Context) error {
			current, err := d.Store.PipelineByID(memql.ContextWithFreshRead(gctx), p.ID)
			if err != nil || current == nil || current.Repository != p.Repository || len(current.Heads) == 0 {
				return err
			}
			return d.Store.UpdatePipeline(gctx, current.OwnerUserID, current.ID, PipelinePatch{Heads: ptr(map[string]string{})})
		})
		if err != nil {
			return ConnectResult{}, err
		}
	}

	stages := make([]string, 0, len(manifest.Pipeline.Stages))
	for _, st := range manifest.Pipeline.Stages {
		stages = append(stages, st.Name)
	}
	return ConnectResult{
		PipelineID: p.ID, Repository: repository, Delivery: delivery, Compute: compute,
		Stages: stages, Reconnected: reconnected,
	}, nil
}

func (i *Integration) handleDisconnect(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	p, err := i.Disconnect(handlerContext(ctx), stringArg(args, "pipelineId"))
	if err != nil {
		return nil, err
	}
	return resultNode(map[string]any{"pipelineId": p.ID, "status": p.Status}), nil
}

// Disconnect stops one of the caller's pipelines opening runs. The row and its
// runs stay; connecting again reactivates the same pipeline.
func (i *Integration) Disconnect(ctx context.Context, pipelineID string) (Pipeline, error) {
	if _, err := personFrom(ctx); err != nil {
		return Pipeline{}, err
	}
	d := i.snapshot()
	if d.Store == nil {
		return Pipeline{}, errNoStore
	}
	pipelineID = strings.TrimSpace(pipelineID)
	if pipelineID == "" {
		return Pipeline{}, fmt.Errorf("pipelines: pipelineId is required")
	}
	// Owner-scoped: someone else's pipeline is no pipeline.
	p, err := d.Store.PipelineForOwner(ctx, pipelineID)
	if err != nil {
		return Pipeline{}, err
	}
	if p == nil {
		return Pipeline{}, fmt.Errorf("pipelines: you have no pipeline %q", pipelineID)
	}
	// The repository's gate, the one every writer of the pipeline row takes.
	err = d.gate(ctx, RepositoryGateKey(p.Repository), func(gctx context.Context) error {
		return d.Store.UpdatePipeline(gctx, p.OwnerUserID, p.ID, PipelinePatch{Status: ptr(PipelineDisconnected)})
	})
	if err != nil {
		return Pipeline{}, err
	}
	p.Status = PipelineDisconnected
	return *p, nil
}

// alreadyConnected refuses a repository another source already runs -- an
// ACTIVE pipeline of it under a different package -- because two pipelines
// on one repository would write two same-named check runs on every commit.
// The same package's own pipeline is a reconnect, never a refusal, and a
// disconnected one is history that blocks nothing. The read is the
// server-only kind on purpose: the other pipeline is somebody else's, so the
// refusal says that one exists and nothing about whose it is.
func alreadyConnected(ctx context.Context, d Deps, repository, packageID string) (*pipelines.Refusal, error) {
	active, err := d.Store.PipelinesForRepository(ctx, repository)
	if err != nil {
		return nil, err
	}
	for _, other := range active {
		if other.Active() && !sameID(other.PackageID, packageID) {
			return pipelines.Refuse(pipelines.CodeAlreadyConnected, "",
				"%s already has a pipeline, connected from another source. A repository has one pipeline, so its checks are reported once per commit.", repository), nil
		}
	}
	return nil, nil
}

// firstErr is err, else refusal as an error, else nil -- never a typed nil
// inside a non-nil error.
func firstErr(err error, refusal *pipelines.Refusal) error {
	if err != nil {
		return err
	}
	if refusal != nil {
		return refusal
	}
	return nil
}

// allowlist validates and normalizes the secret names a pipeline may resolve:
// trimmed, deduplicated, sorted, each a name a step could legally reference.
func allowlist(names []string) ([]string, *pipelines.Refusal) {
	seen := map[string]bool{}
	out := []string{}
	for _, raw := range names {
		name := strings.TrimSpace(raw)
		if name == "" || seen[name] {
			continue
		}
		if strings.HasPrefix(name, reservedSecretPrefix) {
			return nil, pipelines.Refuse(pipelines.CodeSecretInvalid, "secretNames",
				"%q is in the platform's own MEMQL_ namespace, which a pipeline's secrets never shadow.", name)
		}
		if !secretNameRe.MatchString(name) {
			return nil, pipelines.Refuse(pipelines.CodeSecretInvalid, "secretNames",
				"%q is not a secret name a step can reference: use upper-case letters, digits and underscores, starting with a letter.", name)
		}
		seen[name] = true
		out = append(out, name)
	}
	sort.Strings(out)
	return out, nil
}

// githubRepository is a source's repository URL as "owner/name", lower-cased,
// refused unless it is on github.com -- the rule Deployables' fetch holds a
// source to, so a pipeline reaches exactly the repositories a deploy does.
func githubRepository(repoURL string) (string, error) {
	raw := strings.TrimSpace(repoURL)
	if raw == "" {
		return "", &packages.Refusal{Code: packages.CodeSourceUnreadable, Detail: "this source declares no repository URL"}
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", &packages.Refusal{Code: packages.CodeSourceUnreadable, Detail: fmt.Sprintf("%q is not a URL this cluster can read", raw)}
	}
	host := strings.ToLower(u.Hostname())
	if host != "github.com" && host != "www.github.com" {
		return "", &packages.Refusal{Code: packages.CodeSourceHostUnsupported,
			Detail: fmt.Sprintf("%q is on %q; pipelines run on GitHub repositories only", raw, u.Hostname())}
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(parts) < 2 || parts[0] == "" || parts[1] == "" {
		return "", &packages.Refusal{Code: packages.CodeSourceUnreadable,
			Detail: fmt.Sprintf("%q does not name an owner and a repository (expected https://github.com/<owner>/<repo>)", raw)}
	}
	return normalizeRepository(parts[0] + "/" + strings.TrimSuffix(parts[1], ".git")), nil
}

// personFrom is the person a person-facing act is for: a signed-in principal,
// never an anonymous, connector or synthetic actor. A borrowed actor
// (auth.ContextWithUserActor) is a person -- the one it was borrowed for.
func personFrom(ctx context.Context) (string, error) {
	ac, ok := auth.AccessFromContext(ctx)
	if !ok || ac == nil || strings.TrimSpace(ac.UserId) == "" {
		return "", ErrNoCaller
	}
	if ac.IsAnonymousActor() || ac.IsConnector() || ac.Synthetic {
		return "", ErrNoCaller
	}
	return strings.TrimSpace(ac.UserId), nil
}
