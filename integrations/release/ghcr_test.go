package release

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/auth"
	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	"github.com/znasllc-io/memql/component/memql"
	"google.golang.org/protobuf/types/known/structpb"
)

// ghcr_test.go -- present / absent / errored, and the rule that they stay three.
//
// The single property this file exists to hold is that a failed CHECK never
// becomes a claim about the ARTIFACT. Two collapses are available and both are
// wrong in a way that looks like working code:
//
//   errored -> absent  reports a perfectly good release as unbuilt every time
//                      the registry has a bad minute, and an operator acts on
//                      it by cutting again.
//   errored -> present would mark a FAILED build deployable, which is the
//                      false green the whole design of D5 is arranged around.
//
// So the errored case gets its own assertions: the error is surfaced, the
// status is the row's PREVIOUS value, and NOTHING is written.

// fakeRegistry is a scriptable OCI registry, including the anonymous token
// dance -- which is exercised rather than bypassed, because that dance is the
// part most likely to break when GHCR changes and the part a mock would erase.
type fakeRegistry struct {
	// present maps "<repository>:<tag>" to existence.
	present map[string]bool
	// failWith, when non-zero, is returned for every manifest request
	// after the token dance -- the "the check itself failed" case.
	failWith int
	// tokenIssued records that the dance actually happened.
	tokenIssued bool
	server      *httptest.Server
}

func newFakeRegistry(t *testing.T, present map[string]bool) *fakeRegistry {
	t.Helper()
	r := &fakeRegistry{present: present}
	mux := http.NewServeMux()
	mux.HandleFunc("/token", func(w http.ResponseWriter, _ *http.Request) {
		r.tokenIssued = true
		writeJSON(w, http.StatusOK, map[string]any{"token": "anonymous-pull-token"})
	})
	mux.HandleFunc("/v2/", func(w http.ResponseWriter, req *http.Request) {
		if req.Header.Get("Authorization") == "" {
			// The challenge names its own realm, which is what the
			// client is required to read rather than hardcode.
			w.Header().Set("WWW-Authenticate",
				fmt.Sprintf(`Bearer realm="%s/token",service="fake.registry"`, r.server.URL))
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if r.failWith != 0 {
			w.WriteHeader(r.failWith)
			return
		}
		// /v2/<repository>/manifests/<tag>
		rest := strings.TrimPrefix(req.URL.Path, "/v2/")
		repository, tag, ok := strings.Cut(rest, "/manifests/")
		if !ok {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if r.present[repository+":"+tag] {
			w.Header().Set("Content-Type", "application/vnd.oci.image.index.v1+json")
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	})
	r.server = httptest.NewServer(mux)
	t.Cleanup(r.server.Close)
	return r
}

// rowEngine answers the releaseCuts read with one row and records writes.
type rowEngine struct {
	memql.IntegrationEngineAccess
	row     map[string]any
	calls   []string
	origins []auth.CallOrigin
}

func (e *rowEngine) Execute(ctx context.Context, call string) (*memql.ExecuteResult, error) {
	e.calls = append(e.calls, call)
	e.origins = append(e.origins, auth.OriginFromContext(ctx))
	// The status path reads ONE row by id (releaseCutByVersion), not the
	// paginated history list -- see Store.CutByVersion for why. The fake
	// matches the call the package actually composes, so a change to that
	// call surfaces here rather than being silently absorbed.
	if !strings.HasPrefix(call, "query releaseCutByVersion") {
		return &memql.ExecuteResult{}, nil
	}
	if e.row == nil {
		return &memql.ExecuteResult{Bundle: &memqlv1.GraphBundle{}}, nil
	}
	payload, err := structpb.NewStruct(e.row)
	if err != nil {
		return nil, err
	}
	return &memql.ExecuteResult{Bundle: &memqlv1.GraphBundle{
		Nodes: []*memqlv1.MemoryNode{{Id: "v1:cluster:releaseCut:" + asString(e.row["version"]), Payload: payload}},
	}}, nil
}

func (e *rowEngine) writes() []string {
	var out []string
	for _, c := range e.calls {
		if strings.Contains(c, "updateReleaseCutStatus") {
			out = append(out, c)
		}
	}
	return out
}

// statusIntegration wires a fake registry and a one-row engine.
func statusIntegration(t *testing.T, reg *fakeRegistry, row map[string]any) (*Integration, *rowEngine) {
	t.Helper()
	engine := &rowEngine{row: row}
	i := NewIntegration(slog.New(slog.DiscardHandler), engine, resolver{
		env: func(name string) string {
			switch name {
			case RepoVariableName:
				return "Acme/Widget"
			case SecretName:
				return "token"
			}
			return ""
		},
	})
	i.registry = NewRegistryChecker().WithBaseURL(reg.server.URL)
	return i, engine
}

// dispatchedRow is a cut that has published and not yet been verified.
func dispatchedRow(version string) map[string]any {
	return map[string]any{
		"version":      version,
		"status":       "dispatched",
		"dispatchedAt": "2026-08-24T00:00:00Z",
	}
}

// allPresent builds the map for a version whose whole image set exists.
func allPresent(tag string) map[string]bool {
	out := map[string]bool{}
	for _, nodeType := range checkedNodeImages {
		out[fmt.Sprintf("acme/widget-%s:%s", nodeType, tag)] = true
	}
	return out
}

func TestStatusPresentMovesTheRowToImagesAvailable(t *testing.T) {
	reg := newFakeRegistry(t, allPresent("1.2.3"))
	i, engine := statusIntegration(t, reg, dispatchedRow("v1.2.3"))

	out, err := i.Status(ownerCtx(), "v1.2.3")
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if out.Status != "images_available" {
		t.Fatalf("status = %q, want images_available", out.Status)
	}
	if out.CheckError != "" {
		t.Fatalf("a clean check reported an error: %s", out.CheckError)
	}
	writes := engine.writes()
	if len(writes) != 1 {
		t.Fatalf("expected one status write, got %d: %v", len(writes), writes)
	}
	if !strings.Contains(writes[0], `status: "images_available"`) {
		t.Fatalf("the write does not move the row: %s", writes[0])
	}
	if !reg.tokenIssued {
		t.Fatal("the anonymous token dance never happened, so it is untested")
	}
}

func TestStatusAbsentLeavesTheRowDispatchedWithAnAge(t *testing.T) {
	// One image missing is enough: a partial matrix is not a deployable
	// version.
	present := allPresent("1.2.3")
	delete(present, fmt.Sprintf("acme/widget-%s:1.2.3", checkedNodeImages[len(checkedNodeImages)-1]))
	reg := newFakeRegistry(t, present)
	i, engine := statusIntegration(t, reg, dispatchedRow("v1.2.3"))

	out, err := i.Status(ownerCtx(), "v1.2.3")
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if out.Status != "dispatched" {
		t.Fatalf("status = %q, want dispatched", out.Status)
	}
	if out.Age == "" {
		t.Fatal("an absent build must render an age -- it is the whole signal for 'is this stuck'")
	}
	if writes := engine.writes(); len(writes) != 0 {
		t.Fatalf("an unchanged status was written back: %v", writes)
	}
	// The per-image detail is what tells an operator WHICH build failed.
	missing := 0
	for _, img := range out.Images {
		if !img.Present {
			missing++
		}
	}
	if missing != 1 {
		t.Fatalf("expected exactly one missing image in the detail, got %d: %+v", missing, out.Images)
	}
}

func TestStatusErroredShowsTheErrorAndGuessesNothing(t *testing.T) {
	// THE CENTRAL CASE. A registry that answers 500 knows nothing about
	// whether the images exist, so neither do we.
	reg := newFakeRegistry(t, allPresent("1.2.3"))
	reg.failWith = http.StatusInternalServerError
	i, engine := statusIntegration(t, reg, dispatchedRow("v1.2.3"))

	out, err := i.Status(ownerCtx(), "v1.2.3")
	if err != nil {
		// The CHECK failing is not the CALL failing: the operator gets
		// an answer that says "I could not tell", which is more useful
		// than an error with no row context.
		t.Fatalf("a failed check should be reported, not returned as an error: %v", err)
	}
	if out.CheckError == "" {
		t.Fatal("a failed check reported no error, so it is indistinguishable from a clean absence")
	}
	if out.Status != "dispatched" {
		t.Fatalf("status = %q; a failed check must leave the row's PREVIOUS status, not guess", out.Status)
	}
	if writes := engine.writes(); len(writes) != 0 {
		t.Fatalf("a failed check wrote to the row: %v", writes)
	}
}

func TestStatusErroredNeverReadsAsPresent(t *testing.T) {
	// The other collapse, checked from the other side: a registry that
	// errors while every image DOES exist must still not report
	// images_available. This is the false green D5 is built to prevent.
	reg := newFakeRegistry(t, allPresent("1.2.3"))
	reg.failWith = http.StatusBadGateway
	i, _ := statusIntegration(t, reg, dispatchedRow("v1.2.3"))

	out, _ := i.Status(ownerCtx(), "v1.2.3")
	if out.Status == "images_available" {
		t.Fatal("a check that errored reported the images as available")
	}
}

func TestStatusRefusesAVersionThisClusterNeverCut(t *testing.T) {
	// A hand-cut version has no row here and never will. Reporting that as
	// absent images would be a claim about a registry nobody asked about.
	reg := newFakeRegistry(t, allPresent("9.9.9"))
	i, _ := statusIntegration(t, reg, nil)

	_, err := i.Status(ownerCtx(), "v9.9.9")
	if got := RefusalCode(err); got != CodeVersionNotCut {
		t.Fatalf("refusal = %q, want %q (error: %v)", got, CodeVersionNotCut, err)
	}
}

func TestStatusAcceptsEitherSpellingOfTheVersion(t *testing.T) {
	// An operator reading a row sees v1.2.3; one reading a registry sees
	// 1.2.3. Refusing either would refuse the same version for being
	// written the way the other screen writes it.
	for _, spelling := range []string{"v1.2.3", "1.2.3", " v1.2.3 "} {
		reg := newFakeRegistry(t, allPresent("1.2.3"))
		i, _ := statusIntegration(t, reg, dispatchedRow("v1.2.3"))
		out, err := i.Status(ownerCtx(), spelling)
		if err != nil {
			t.Fatalf("%q was refused: %v", spelling, err)
		}
		if out.Version != "v1.2.3" || out.BareVersion != "1.2.3" {
			t.Fatalf("%q normalized to %+v", spelling, out)
		}
	}
}

// TestTheRegistryIsAskedForTheBAREVersion is memql#4061's two-conventions rule,
// checked at the one seam where getting it wrong is invisible.
//
// Git tags carry the leading v and image tags do not --
// dispatch-engine-images-on-release.yml strips it before dispatching. Asking
// the registry for "v1.2.3" requests an image tag nothing ever pushed, so
// EVERY release would report as unbuilt, forever, with no error anywhere.
func TestTheRegistryIsAskedForTheBAREVersion(t *testing.T) {
	var asked []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path == "/token" {
			writeJSON(w, http.StatusOK, map[string]any{"token": "t"})
			return
		}
		asked = append(asked, req.URL.Path)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	checker := NewRegistryChecker().WithBaseURL(server.URL)
	v, _ := normalizeVersion("v1.2.3")
	res := checker.Check(context.Background(), repoRef{Owner: "Acme", Name: "Widget"}, v)
	if res.Err != nil {
		t.Fatalf("check: %v", res.Err)
	}
	if len(asked) == 0 {
		t.Fatal("no manifest was requested, so this asserts nothing")
	}
	for _, path := range asked {
		if strings.Contains(path, "/manifests/v1.2.3") {
			t.Fatalf("the registry was asked for the TAG form: %s", path)
		}
		if !strings.Contains(path, "/manifests/1.2.3") {
			t.Fatalf("the registry was asked for something unexpected: %s", path)
		}
	}
}

// TestImageRepositoryDerivesFromTheConfiguredRepo pins D5's "no second
// literal" rule. The engine may carry no organization name, so the GHCR path
// has to come from MEMQL_RELEASE_REPO's owner -- and lowercased, because
// registries reject an uppercase path segment while GitHub happily has one.
func TestImageRepositoryDerivesFromTheConfiguredRepo(t *testing.T) {
	got := imageRepository(repoRef{Owner: "AcmeCorp", Name: "Widget"}, "bff")
	if got != "acmecorp/widget-bff" {
		t.Fatalf("image repository = %q, want acmecorp/widget-bff", got)
	}
}

// TestNoOrganizationLiteralInThePackage is the product-neutrality check at the
// level that matters here.
//
// TestEngineIsProductNeutral holds a banned-NAMES list, so it cannot notice a
// new organization arriving under a name nobody thought to ban. This asks the
// structural question instead: does any non-test file in this package contain a
// slash-separated owner/name literal that would work as a repository? The only
// legitimate answer is none -- every one comes from the operator's variable.
func TestNoOrganizationLiteralInThePackage(t *testing.T) {
	// The check is over what the package COMPOSES rather than over its
	// source text, because a doc comment naming an example repository is
	// fine and a Go string used as a default is not. So: with nothing
	// seeded, the package must refuse rather than reach anywhere.
	i := NewIntegration(slog.New(slog.DiscardHandler), &recordingEngine{}, resolver{env: func(string) string { return "" }})
	_, err := i.Cut(ownerCtx(), CutRequest{Bump: "patch"})
	if got := RefusalCode(err); got != CodeReleaseRepoUnconfigured {
		t.Fatalf("with nothing configured the package resolved a repository anyway (%v) -- it carries a compiled-in default", err)
	}
	_, err = i.Status(ownerCtx(), "v1.2.3")
	if got := RefusalCode(err); got != CodeReleaseRepoUnconfigured {
		t.Fatalf("the status path carries a compiled-in default: %v", err)
	}
}

// TestParseChallengeReadsTheRealmTheRegistryNames covers the durability
// argument in ghcr.go: a hardcoded token endpoint turns a registry-side change
// into a check that reports every release as unverifiable.
func TestParseChallengeReadsTheRealmTheRegistryNames(t *testing.T) {
	realm, service := parseChallenge(`Bearer realm="https://ghcr.io/token",service="ghcr.io",scope="repository:x:pull"`)
	if realm != "https://ghcr.io/token" || service != "ghcr.io" {
		t.Fatalf("realm=%q service=%q", realm, service)
	}
	if r, _ := parseChallenge("Basic realm=\"x\""); r != "" {
		t.Fatalf("a non-Bearer challenge produced a realm: %q", r)
	}
}

func TestStatusWritesUnderInternalOrigin(t *testing.T) {
	// Same class as the cut's: updateReleaseCutStatus is @serverOnly, and
	// so is the releaseCuts read this path makes. Without the stamp the
	// read returns ZERO ROWS -- which is indistinguishable from "no such
	// version" and would make every status check refuse with
	// version_not_cut.
	reg := newFakeRegistry(t, allPresent("1.2.3"))
	i, engine := statusIntegration(t, reg, dispatchedRow("v1.2.3"))
	if _, err := i.Status(ownerCtx(), "v1.2.3"); err != nil {
		t.Fatalf("status: %v", err)
	}
	if len(engine.origins) == 0 {
		t.Fatal("no graph call was made, so this asserts nothing")
	}
	for idx, origin := range engine.origins {
		if origin != auth.OriginInternal {
			t.Errorf("call %d (%s) ran under %v, want internal", idx, engine.calls[idx], origin)
		}
	}
}

// jsonRoundTrip is a guard on the payload the DSL caller actually receives:
// the outcome has to survive encoding, because that is what the capability
// returns.
func TestStatusOutcomeSurvivesJSON(t *testing.T) {
	reg := newFakeRegistry(t, allPresent("1.2.3"))
	i, _ := statusIntegration(t, reg, dispatchedRow("v1.2.3"))
	out, err := i.Status(ownerCtx(), "v1.2.3")
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	encoded, err := json.Marshal(out)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var decoded StatusOutcome
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if decoded.Status != out.Status || len(decoded.Images) != len(out.Images) {
		t.Fatalf("round trip lost fields: %+v vs %+v", decoded, out)
	}
}
