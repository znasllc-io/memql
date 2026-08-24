package release

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// ghcr.go -- "do the images for this version exist yet", asked of the registry.
//
// WHY THE REGISTRY AND NOT THE WORKFLOW RUN. Two reasons, and the second is the
// one that decided it. First, the Actions list API does not expose a run's
// dispatch INPUTS, so there is no way to tell which run was cutting WHICH
// version -- matching by time is a guess. Second and more important: a run can
// fail after the Release published, and it can fail partway through, leaving
// some node images built and others not. The question an operator actually has
// is "can I deploy this version", and only the registry answers that. Verify
// the artifact, not the action.
//
// ANONYMOUSLY, BECAUSE THE IMAGES ARE PUBLIC. GHCR speaks the standard OCI
// token dance: an unauthenticated manifest request gets a 401 carrying a
// WWW-Authenticate realm, you fetch a pull-scoped token from that realm, and
// you retry. That is exactly what stackPin.ts does for the same images. No
// credential is involved, which matters: the release token is scoped to
// Contents on one repository and has no registry scope at all, so a check that
// needed one would fail on a correctly-scoped token.
//
// AN ERROR IS NOT AN ABSENCE. This file's whole contract is that the three
// outcomes stay three: present, absent, and "the check itself failed". A
// registry that cannot be reached knows nothing about whether an image exists,
// and collapsing that into "absent" would report a working release as
// unbuilt -- or, if collapsed the other way, report a failed build as
// deployable. The caller shows the error and leaves the row alone.

// registryHost is GHCR. A field on the checker rather than a constant used
// inline, for the same reason the GitHub base URL is: the tests point it at a
// fake, and CI never talks to a real registry.
const registryHost = "https://ghcr.io"

// checkedNodeImages is the representative set a version is judged on.
//
// REPRESENTATIVE RATHER THAN EXHAUSTIVE, and the choice is deliberate. Nine
// node types ship from one workflow over one matrix, so a run that produced
// these three produced the rest; checking all nine would triple the round
// trips to raise confidence that the matrix did not partially succeed, which
// is not the failure mode this check exists to catch (that one is "the run
// failed", and it fails the whole matrix).
//
// The three are chosen to span the build's variety rather than to be the first
// three alphabetically: identity is the auth node, bff is the default build
// with no tag, and agent carries the heaviest dependency set. A matrix that
// broke for one build shape and not another shows up here.
var checkedNodeImages = []string{"identity", "bff", "agent"}

// imageRepository composes the GHCR repository path for one node type.
//
// DERIVED FROM MEMQL_RELEASE_REPO's OWNER, so there is exactly one place an
// organization name enters this package -- the variable an operator seeded.
// A second literal here would be a second thing to keep in step, and the
// engine may carry no organization literal at all.
func imageRepository(repo repoRef, nodeType string) string {
	return fmt.Sprintf("%s/%s-%s", strings.ToLower(repo.Owner), strings.ToLower(repo.Name), nodeType)
}

// RegistryChecker asks a registry whether manifests exist.
type RegistryChecker struct {
	base string
	http *http.Client
}

// NewRegistryChecker builds a checker against ghcr.io.
func NewRegistryChecker() *RegistryChecker {
	return &RegistryChecker{base: registryHost, http: &http.Client{Timeout: 30 * time.Second}}
}

// WithBaseURL points the checker at another registry. Used by the tests.
func (r *RegistryChecker) WithBaseURL(base string) *RegistryChecker {
	r.base = strings.TrimSuffix(base, "/")
	return r
}

// ImageStatus is one image's answer.
type ImageStatus struct {
	Repository string
	Present    bool
}

// CheckResult is the honest three-way answer for a whole version.
type CheckResult struct {
	Images []ImageStatus
	// AllPresent is true only when every checked image was found. It is
	// meaningless when Err is set, and callers must consult Err first --
	// which is why the zero value is false rather than true.
	AllPresent bool
	// Err is set when the CHECK failed, as opposed to the images being
	// absent. The caller reports it and changes nothing.
	Err error
}

// Check asks the registry for every image's manifest at the bare version.
//
// THE BARE VERSION, not the tag: memql#4061's two-conventions rule says git
// tags carry the leading v and image tags do not, and
// dispatch-engine-images-on-release.yml strips it before dispatching. Passing
// the tag form here asks for an image tag nothing ever pushed, which would
// report every release as unbuilt.
func (r *RegistryChecker) Check(ctx context.Context, repo repoRef, v version) CheckResult {
	out := CheckResult{AllPresent: true}
	for _, nodeType := range checkedNodeImages {
		repository := imageRepository(repo, nodeType)
		present, err := r.manifestExists(ctx, repository, v.bare())
		if err != nil {
			// One failed check invalidates the whole answer. Not
			// "the other two were present, so probably yes" --
			// that is the guess this design refuses to make.
			return CheckResult{Images: out.Images, Err: err}
		}
		out.Images = append(out.Images, ImageStatus{Repository: repository, Present: present})
		if !present {
			out.AllPresent = false
		}
	}
	return out
}

// manifestExists HEADs one manifest, performing the anonymous token dance when
// the registry asks for it.
func (r *RegistryChecker) manifestExists(ctx context.Context, repository, tag string) (bool, error) {
	status, header, err := r.headManifest(ctx, repository, tag, "")
	if err != nil {
		return false, err
	}
	if status == http.StatusUnauthorized {
		token, tokenErr := r.anonymousToken(ctx, header.Get("WWW-Authenticate"), repository)
		if tokenErr != nil {
			return false, tokenErr
		}
		status, _, err = r.headManifest(ctx, repository, tag, token)
		if err != nil {
			return false, err
		}
	}
	switch {
	case status >= 200 && status < 300:
		return true, nil
	case status == http.StatusNotFound:
		// The one honest absence: the repository exists and this tag is
		// not in it, which is what "the build has not finished" looks
		// like.
		return false, nil
	case status == http.StatusUnauthorized, status == http.StatusForbidden:
		// After the token dance, this means the images are NOT public --
		// a real configuration fact, and not something to read as
		// absence.
		return false, refuse(CodeRegistryCheckFailed,
			"the registry refused an anonymous read of %s (HTTP %d). These images are expected to be public; a private registry cannot be checked without a pull credential.",
			repository, status)
	default:
		return false, refuse(CodeRegistryCheckFailed,
			"the registry returned HTTP %d for %s:%s, so whether the image exists is unknown.",
			status, repository, tag)
	}
}

// headManifest issues one HEAD, optionally bearing a token.
func (r *RegistryChecker) headManifest(ctx context.Context, repository, tag, token string) (int, http.Header, error) {
	endpoint := fmt.Sprintf("%s/v2/%s/manifests/%s", r.base, repository, url.PathEscape(tag))
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, endpoint, nil)
	if err != nil {
		return 0, nil, refuse(CodeRegistryCheckFailed, "could not build the registry request: %v", err)
	}
	// Both media types, because the workflow builds multi-arch images and a
	// client that accepts only the single-image manifest gets a 404 for an
	// index that exists -- an absence that is not one.
	req.Header.Set("Accept", strings.Join([]string{
		"application/vnd.oci.image.index.v1+json",
		"application/vnd.oci.image.manifest.v1+json",
		"application/vnd.docker.distribution.manifest.list.v2+json",
		"application/vnd.docker.distribution.manifest.v2+json",
	}, ", "))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := r.http.Do(req)
	if err != nil {
		return 0, nil, refuse(CodeRegistryCheckFailed, "the registry could not be reached: %v", err)
	}
	defer resp.Body.Close()
	return resp.StatusCode, resp.Header, nil
}

// anonymousToken performs the OCI token dance against the realm the registry
// named in its WWW-Authenticate challenge.
//
// THE REALM COMES FROM THE CHALLENGE rather than being hardcoded. GHCR's token
// endpoint has moved before, and a hardcoded one turns a registry-side change
// into a check that reports every release as unverifiable. Reading the realm
// the registry itself names is both the spec's answer and the durable one.
func (r *RegistryChecker) anonymousToken(ctx context.Context, challenge, repository string) (string, error) {
	realm, service := parseChallenge(challenge)
	if realm == "" {
		return "", refuse(CodeRegistryCheckFailed,
			"the registry asked for authentication without naming a token realm, so an anonymous check is not possible.")
	}
	endpoint := realm + "?scope=" + url.QueryEscape("repository:"+repository+":pull")
	if service != "" {
		endpoint += "&service=" + url.QueryEscape(service)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return "", refuse(CodeRegistryCheckFailed, "could not build the registry token request: %v", err)
	}
	resp, err := r.http.Do(req)
	if err != nil {
		return "", refuse(CodeRegistryCheckFailed, "the registry's token endpoint could not be reached: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", refuse(CodeRegistryCheckFailed,
			"the registry's token endpoint returned HTTP %d, so an anonymous check is not possible.", resp.StatusCode)
	}
	var decoded struct {
		Token       string `json:"token"`
		AccessToken string `json:"access_token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&decoded); err != nil {
		return "", refuse(CodeRegistryCheckFailed, "the registry's token reply could not be read: %v", err)
	}
	// Registries differ about which field carries it; both are in the spec.
	if decoded.Token != "" {
		return decoded.Token, nil
	}
	if decoded.AccessToken != "" {
		return decoded.AccessToken, nil
	}
	return "", refuse(CodeRegistryCheckFailed, "the registry's token reply carried no token.")
}

// parseChallenge reads realm and service out of a Bearer WWW-Authenticate
// header: `Bearer realm="https://ghcr.io/token",service="ghcr.io",scope="..."`.
func parseChallenge(header string) (realm, service string) {
	rest := strings.TrimSpace(header)
	if !strings.HasPrefix(strings.ToLower(rest), "bearer ") {
		return "", ""
	}
	rest = rest[len("bearer "):]
	for _, part := range strings.Split(rest, ",") {
		key, value, ok := strings.Cut(strings.TrimSpace(part), "=")
		if !ok {
			continue
		}
		value = strings.Trim(strings.TrimSpace(value), `"`)
		switch strings.ToLower(strings.TrimSpace(key)) {
		case "realm":
			realm = value
		case "service":
			service = value
		}
	}
	return realm, service
}
