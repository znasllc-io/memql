package repositoryauthority

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/automations/workflowhost"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/packages"
	"github.com/znasllc-io/memql/core/num"
	"path"
	"regexp"
	"strings"
)

const routeContract = "repository.change-route.v1"
const recipeContract = "repository.change-recipe.v1"

var identifier = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,199}$`)
var repositoryName = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)

type route struct {
	SourceID, Template, Remote string
	RemoteBinding              string
	TTLSeconds                 int
	Snapshot                   map[string]any
	Recipe                     map[string]any
}

func person(ctx context.Context, verb, resource string) (string, error) {
	a, ok := auth.AccessFromContext(ctx)
	if !ok || a == nil || auth.ActsForNoPerson(ctx, a.UserId) || a.IsAnonymousActor() || a.IsConnector() {
		return "", errors.New("repository change requires an authenticated person")
	}
	if resource != "" {
		s, ok := auth.SubjectFromContext(ctx)
		if !ok || !auth.CapableFor(ctx, s, verb, resource) {
			return "", errors.New("repository change capability is not granted")
		}
	}
	return memql.BareShortId(a.UserId), nil
}
func (i *Integration) resolveRoute(ctx context.Context, s Source, canWrite bool) (route, error) {
	var r route
	d := i.deps()
	if d.SystemVariable == nil {
		return r, errors.New("repository authority routing is not configured")
	}
	name, err := d.SystemVariable(memql.ContextWithFreshRead(ctx), ResolverVariable)
	if err != nil {
		return r, err
	}
	if name == "" {
		return r, errors.New("repository authority resolver is not configured")
	}
	raw, err := d.SystemVariable(memql.ContextWithFreshRead(ctx), BindingsVariable)
	if err != nil {
		return r, err
	}
	bindings := map[string]any{}
	if raw != "" && (len(raw) > 65536 || json.Unmarshal([]byte(raw), &bindings) != nil) {
		return r, errors.New("repository authority bindings must be a bounded operator object")
	}
	snapshot, err := workflowhost.Capture(name, routeContract, d.WorkflowSource, nil)
	if err != nil {
		return r, err
	}
	answer, err := workflowhost.RunSnapshot(ctx, snapshot, routeContract, map[string]any{"repository": s.Repository, "sourceId": s.ID, "sourceOwnerId": s.OwnerUserID, "requesterCanWrite": canWrite, "bindings": bindings}, workflowhost.Options{})
	if err != nil {
		return r, err
	}
	object, ok := answer.(map[string]any)
	if !ok {
		return r, errors.New("repository authority resolver must return an object")
	}
	for key := range object {
		if key != "sourceId" && key != "template" && key != "remote" && key != "ttlSeconds" {
			return r, fmt.Errorf("unknown authority routing field %s", key)
		}
	}
	r = route{SourceID: str(object["sourceId"]), Template: str(object["template"]), Remote: str(object["remote"]), TTLSeconds: integer(object["ttlSeconds"]), Snapshot: snapshot.Map()}
	if r.TTLSeconds < 1 || r.TTLSeconds > 7*24*3600 || r.Template == "" || !identifier.MatchString(r.Template) || (r.Remote == "" && !identifier.MatchString(r.SourceID)) || (r.Remote != "" && !identifier.MatchString(r.Remote)) {
		return r, errors.New("repository authority route requires an explicit source or peer, installed template and bounded expiry")
	}
	if r.Remote != "" {
		if d.RemoteBinding == nil {
			return r, errors.New("repository peer identity is unavailable")
		}
		r.RemoteBinding, err = d.RemoteBinding(ctx, r.Remote)
		if err != nil {
			return r, err
		}
		if !sha256hex.MatchString(r.RemoteBinding) {
			return r, errors.New("repository peer did not establish an immutable connection identity")
		}
	} else {
		// An approved repository recipe has a narrow native vocabulary. This
		// preflights all branches and captures children; a renamed or edited
		// recipe revokes prior delegation at the next read/effect check.
		refuse := func(context.Context, map[string]any) (any, error) {
			return nil, errors.New("recipe preflight cannot execute operations")
		}
		recipe, err := workflowhost.Capture(r.Template, recipeContract, d.WorkflowSource, map[string]workflowhost.Operation{
			"repositoryChangeContext": refuse, "ai": refuse, "repositoryChangePropose": refuse,
		})
		if err != nil {
			return r, fmt.Errorf("repository change recipe is outside its bounded contract: %w", err)
		}
		r.Recipe = recipe.Map()
	}
	return r, nil
}
func validateRequest(r Request) error {
	if !identifier.MatchString(r.RequestID) || len(r.Statement) < 1 || len(r.Statement) > 16000 || r.Operation != OperationPropose || len(r.Paths) == 0 || len(r.Paths) > 100 {
		return errors.New("request needs a stable requestId, statement, propose_change operation and explicit paths")
	}
	if r.ParentRequestID != "" && !identifier.MatchString(r.ParentRequestID) {
		return errors.New("invalid parent request identity")
	}
	return validatePaths(r.Paths, 100)
}
func validatePaths(paths []string, max int) error {
	if len(paths) == 0 || len(paths) > max {
		return errors.New("path scope exceeds its bounded count")
	}
	seen := map[string]bool{}
	for _, p := range paths {
		clean := strings.TrimSuffix(p, "/")
		if len(p) > 500 || clean == "" || clean == "." || path.Clean(clean) != clean || strings.HasPrefix(clean, "/") || strings.HasPrefix(clean, "../") || strings.ContainsAny(clean, "\\\x00\r\n*?[]") || seen[p] {
			return errors.New("paths must be unique relative files or directory prefixes without traversal or wildcards")
		}
		for _, segment := range strings.Split(clean, "/") {
			if strings.EqualFold(segment, ".git") {
				return errors.New("repository metadata is outside change scope")
			}
		}
		seen[p] = true
	}
	return nil
}
func containsPaths(allowed, actual []string) bool {
	if len(actual) == 0 {
		return false
	}
	for _, p := range actual {
		if validateRequest(Request{RequestID: "effect", Statement: "effect", Operation: OperationPropose, Paths: []string{p}}) != nil {
			return false
		}
		found := false
		for _, a := range allowed {
			if a == p || (strings.HasSuffix(a, "/") && strings.HasPrefix(p, a)) {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}
func str(v any) string      { s, _ := v.(string); return s }
func same(a, b string) bool { return memql.BareShortId(a) == memql.BareShortId(b) }
func (i *Integration) access(ctx context.Context, s Source) (packages.RepositoryAuthority, error) {
	d := i.deps()
	if d.RepositoryAccess == nil {
		return packages.RepositoryAuthority{}, errors.New("repository provider authority is unavailable")
	}
	a, e := d.RepositoryAccess(memql.ContextWithFreshRead(ctx), s)
	if e == nil && (a.RepositoryID <= 0 || !strings.EqualFold(a.Repository, s.Repository)) {
		e = errors.New("provider did not establish this stable repository identity")
	}
	return a, e
}

func integer(v any) int {
	switch n := v.(type) {
	case int:
		return n
	case int64:
		return num.Int64OrZero(n)
	case float64:
		return num.Float64OrZero(n)
	}
	return 0
}
