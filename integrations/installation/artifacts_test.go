package installation

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/znasllc-io/memql/component/pipelines"
	"github.com/znasllc-io/memql/integrations/argocd"
	"github.com/znasllc-io/memql/integrations/ociregistry"
)

type admissionFixture struct {
	server             *httptest.Server
	config             artifactAdmissionConfig
	before, after      argocd.RenderedRevision
	old, next          pipelines.VerifiedPublishedRelease
	mu                 sync.Mutex
	bodies             map[string][]byte
	media              map[string]string
	reads              map[string]int
	oldLayer, newLayer string
}

func admissionSHA(body []byte) string { return fmt.Sprintf("sha256:%x", sha256.Sum256(body)) }

func (f *admissionFixture) image(label string) (string, string) {
	layer := []byte("complete immutable image layer " + label)
	config, _ := json.Marshal(map[string]any{"os": "linux", "architecture": "arm64", "rootfs": map[string]any{"type": "layers", "diff_ids": []string{admissionSHA(layer)}}})
	descriptor := func(body []byte, media string) map[string]any {
		return map[string]any{"mediaType": media, "digest": admissionSHA(body), "size": len(body)}
	}
	media := "application/vnd.oci.image.manifest.v1+json"
	manifest, _ := json.Marshal(map[string]any{"schemaVersion": 2, "mediaType": media, "config": descriptor(config, "application/vnd.oci.image.config.v1+json"), "layers": []any{descriptor(layer, "application/vnd.oci.image.layer.v1.tar")}})
	for _, body := range [][]byte{layer, config, manifest} {
		f.bodies[admissionSHA(body)] = body
	}
	f.media[admissionSHA(manifest)] = media
	return admissionSHA(manifest), admissionSHA(layer)
}

func signAdmissionRelease(t *testing.T, release pipelines.PublishedRelease) pipelines.VerifiedPublishedRelease {
	t.Helper()
	pub, key, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	body, err := pipelines.SignPublishedRelease(release, "fixture-key", key)
	require.NoError(t, err)
	verified, err := pipelines.VerifyPublishedRelease(body, release.Publisher, map[string]ed25519.PublicKey{"fixture-key": pub})
	require.NoError(t, err)
	return verified
}

func newAdmissionFixture(t *testing.T) *admissionFixture {
	t.Helper()
	f := &admissionFixture{bodies: map[string][]byte{}, media: map[string]string{}, reads: map[string]int{}}
	old, oldLayer := f.image("old")
	next, newLayer := f.image("new")
	f.oldLayer, f.newLayer = oldLayer, newLayer
	f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			t.Errorf("admission attempted registry write: %s", r.Method)
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		if r.URL.Path == "/v2/" {
			return
		}
		parts := strings.Split(r.URL.Path, "/")
		if len(parts) != 6 || parts[1] != "v2" || parts[2] != "memql" || parts[3] != "engine" || (parts[4] != "manifests" && parts[4] != "blobs") {
			w.WriteHeader(404)
			return
		}
		digest := parts[5]
		body, found := f.bodies[digest]
		if !found {
			w.WriteHeader(404)
			return
		}
		w.Header().Set("Content-Type", f.media[digest])
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		w.Header().Set("Docker-Content-Digest", digest)
		if r.Method == http.MethodGet {
			f.reads[digest]++
			_, _ = w.Write(body)
		}
	}))
	t.Cleanup(f.server.Close)
	ref := func(digest string) string {
		return strings.TrimPrefix(f.server.URL, "http://") + "/memql/engine@" + digest
	}
	pub, err := publishedFixture(t).Release()
	require.NoError(t, err)
	pub.Components[0].Version = "0.24.0"
	pub.Components[0].Artifacts[0].ImageDigest = old
	pub.Components[0].Artifacts[0].Locations[0].Origin = f.server.URL
	f.old = signAdmissionRelease(t, pub)
	pub.Components[0].Version, pub.Components[0].Commit = "0.25.0", strings.Repeat("e", 40)
	pub.Components[0].Artifacts[0].ImageDigest = next
	f.next = signAdmissionRelease(t, pub)
	f.before = renderInventoryFixture(t, strings.Repeat("a", 40), resourceManifests(ref(old)))
	f.after = renderInventoryFixture(t, strings.Repeat("b", 40), resourceManifests(ref(next)))
	f.config = artifactAdmissionConfig{ConfigurationDigest: "memql-id:" + strings.Repeat("a", 64), Platform: "linux/arm64", Bindings: resourceBindings(), Registries: []ociregistry.Target{{Origin: f.server.URL, Repository: "memql/engine", AllowLoopbackHTTP: true}}}
	return f
}

func (f *admissionFixture) scope(t *testing.T) *artifactAdmission {
	t.Helper()
	s, err := newArtifactAdmission(context.Background(), resourceAPIFixture(), f.config, f.old, f.next, f.before, f.after)
	require.NoError(t, err)
	return s
}

func observeAdmission(t *testing.T, scope *artifactAdmission) []artifactObservation {
	t.Helper()
	obligations := scope.requirementsForWorkflow()
	observations := make([]artifactObservation, len(obligations))
	errors := make([]error, len(obligations))
	var group sync.WaitGroup
	for i, want := range obligations {
		group.Add(1)
		go func() {
			defer group.Done()
			observations[i], errors[i] = scope.check(context.Background(), want.Key)
		}()
	}
	group.Wait()
	for _, err := range errors {
		require.NoError(t, err)
	}
	return observations
}

func TestArtifactAdmissionIncludesUnchangedInfrastructureAndOwnsConfiguration(t *testing.T) {
	f := newAdmissionFixture(t)
	f.mu.Lock()
	infra, layer := f.image("unchanged-infrastructure")
	f.mu.Unlock()
	base := strings.TrimPrefix(f.server.URL, "http://") + "/memql/engine@"
	old, err := f.old.Release()
	require.NoError(t, err)
	next, err := f.next.Release()
	require.NoError(t, err)
	manifests := func(digest string) []string {
		m := resourceManifests(base + digest)
		m[0] = strings.Replace(m[0], `"containers":[`, `"initContainers":[{"name":"infrastructure","image":"`+base+infra+`"}],"containers":[`, 1)
		return m
	}
	f.before = renderInventoryFixture(t, strings.Repeat("a", 40), manifests(old.Components[0].Artifacts[0].ImageDigest))
	f.after = renderInventoryFixture(t, strings.Repeat("b", 40), manifests(next.Components[0].Artifacts[0].ImageDigest))
	scope := f.scope(t)
	require.Len(t, scope.requirementsForWorkflow(), 3, "same infrastructure image in both renders needs one proof, not zero")
	// A caller changing its configuration after construction cannot redirect a
	// previously scoped read or drop a workload from the frozen obligation set.
	f.config.Registries[0].Origin = "https://changed.invalid"
	f.config.Bindings[0].Artifact = "changed"
	_, err = scope.seal(observeAdmission(t, scope))
	require.NoError(t, err)
	f.mu.Lock()
	defer f.mu.Unlock()
	require.Equal(t, 1, f.reads[layer])
	formatted := fmt.Sprintf("%+v %#v", scope, scope)
	require.NotContains(t, formatted, f.server.URL)
}

func TestArtifactAdmissionFreshReceiverReconstructsAndReadsBothSides(t *testing.T) {
	f := newAdmissionFixture(t)
	first := f.scope(t)
	wants := first.requirementsForWorkflow()
	require.Len(t, wants, 2)
	wants[0].Image = "caller-mutated"
	require.NotEqual(t, wants, first.requirementsForWorkflow())
	observed := observeAdmission(t, first)
	evidence, err := first.seal(observed)
	require.NoError(t, err)
	require.Equal(t, f.config.ConfigurationDigest, evidence.configuration)
	require.Equal(t, f.old.Digest(), evidence.rollback)
	require.Equal(t, f.next.Digest(), evidence.candidate)
	require.True(t, evidence.expires.After(evidence.observed))
	second := f.scope(t) // another receiver has none of the first receiver's observations
	require.Equal(t, first.digest, second.digest)
	_, err = second.seal(observed)
	require.Error(t, err, "a recovered scope must obtain fresh observations")
	_, err = second.seal(observeAdmission(t, second))
	require.NoError(t, err)
	f.mu.Lock()
	defer f.mu.Unlock()
	require.Equal(t, 2, f.reads[f.oldLayer])
	require.Equal(t, 2, f.reads[f.newLayer])
}

func TestArtifactAdmissionRefusesIncompleteExpiredOrReboundEvidence(t *testing.T) {
	f := newAdmissionFixture(t)
	scope := f.scope(t)
	proofs := observeAdmission(t, scope)
	for _, fault := range []string{"omitted", "duplicate", "forged", "swapped proof", "changed scope", "future", "expired"} {
		t.Run(fault, func(t *testing.T) {
			p := append([]artifactObservation(nil), proofs...)
			s := *scope
			switch fault {
			case "omitted":
				p = p[:1]
			case "duplicate":
				p[1] = p[0]
			case "forged":
				p[0].image = &ociregistry.AvailableImage{}
			case "swapped proof":
				p[0].image = p[1].image
			case "changed scope":
				p[0].scope = "memql-id:" + strings.Repeat("f", 64)
			case "future":
				p[0].completed = time.Now().Add(time.Minute)
			case "expired":
				s.created = time.Now().Add(-40 * time.Minute)
				p[0].started = time.Now().Add(-31 * time.Minute)
			}
			evidence, err := s.seal(p)
			require.Error(t, err)
			require.Empty(t, evidence.digest)
		})
	}
	f.config.ConfigurationDigest = "memql-id:" + strings.Repeat("b", 64)
	changed := f.scope(t)
	require.NotEqual(t, scope.digest, changed.digest)
	_, err := changed.check(context.Background(), proofs[0].key)
	require.Error(t, err)
}

func TestArtifactAdmissionDoesNotTrustPinnedButUnavailableRollback(t *testing.T) {
	for _, side := range []string{"rollback", "candidate"} {
		t.Run(side, func(t *testing.T) {
			f := newAdmissionFixture(t)
			scope := f.scope(t)
			f.mu.Lock()
			if side == "rollback" {
				delete(f.bodies, f.oldLayer)
			} else {
				delete(f.bodies, f.newLayer)
			}
			f.mu.Unlock()
			failed := 0
			for _, want := range scope.requirementsForWorkflow() {
				_, err := scope.check(context.Background(), want.Key)
				if err != nil {
					failed++
				}
			}
			require.Equal(t, 1, failed)
		})
	}
}

func TestArtifactAdmissionRequiresActualSignedRollbackAndConfiguredTransport(t *testing.T) {
	for _, fault := range []string{"wrong rollback", "missing registry", "wrong repository", "wrong transport", "wrong platform", "duplicate registry", "unbound image"} {
		t.Run(fault, func(t *testing.T) {
			f := newAdmissionFixture(t)
			switch fault {
			case "wrong rollback":
				f.old = f.next
			case "missing registry":
				f.config.Registries = nil
			case "wrong repository":
				f.config.Registries[0].Repository = "other/image"
			case "wrong transport":
				f.config.Registries[0].Origin = strings.Replace(f.server.URL, "http:", "https:", 1)
			case "wrong platform":
				f.config.Platform = "linux/amd64"
			case "duplicate registry":
				f.config.Registries = append(f.config.Registries, f.config.Registries[0])
			case "unbound image":
				f.config.Bindings = nil
			}
			_, err := newArtifactAdmission(context.Background(), resourceAPIFixture(), f.config, f.old, f.next, f.before, f.after)
			require.Error(t, err)
			f.mu.Lock()
			defer f.mu.Unlock()
			require.Empty(t, f.reads, "refused scope must not contact a registry")
		})
	}
}
