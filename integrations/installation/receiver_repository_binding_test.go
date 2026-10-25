package installation

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/deploycontrol"
)

const repositoryInventoryFixturePath = "api/v1/namespaces/argocd/secrets?labelSelector=argocd.argoproj.io%2Fsecret-type%3Drepository&limit=257"
const namedRepositoryFixturePath = "api/v1/namespaces/argocd/secrets/project-repository"

func namedRepositoryFixture(t *testing.T) *receiverFixture {
	t.Helper()
	f := newReceiverFixture(t)
	f.config.RepositoryScope = &receiverRepositoryScope{Profile: namedProjectRepositoryProfile, Bindings: []receiverRepositoryBinding{{Application: receiverNamed{"argocd", "memql"}, Secret: receiverNamed{"argocd", "project-repository"}}}}
	secret := f.put("v1", "Secret", "secrets", "argocd", "project-repository", map[string]any{"data": map[string]any{
		"url":      base64.StdEncoding.EncodeToString([]byte(" HTTPS://GITHUB.COM/ACME/PLATFORM.GIT ")),
		"project":  base64.StdEncoding.EncodeToString([]byte("default")),
		"password": base64.StdEncoding.EncodeToString([]byte("must-not-be-used-or-returned")),
	}})
	resourceMap(secret, "metadata")["labels"] = map[string]any{"argocd.argoproj.io/secret-type": "repository"}
	// The narrowed Kubernetes identity has no list permission. A list request
	// must fail, rather than being answered by a conveniently empty inventory.
	delete(f.objects, repositoryInventoryFixturePath)
	f.saveConfig()
	return f
}

func TestReceiverNamedRepositoryAcrossReplicasAndRotation(t *testing.T) {
	f := namedRepositoryFixture(t)
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		require.Equal(t, "Bearer named-reader", request.Header.Get("Authorization"))
		body, err := f.Do(request.Context(), request.Method, strings.TrimPrefix(request.URL.RequestURI(), "/"), "", nil)
		if err != nil {
			http.Error(w, "refused", http.StatusForbidden)
			return
		}
		_, _ = w.Write(body)
	}))
	defer server.Close()
	ctx := captureOperator(auth.RoleOwner, "operator")
	observe := func() *receiverSnapshot {
		t.Helper()
		// Separate receiver/API instances share only the authenticated server;
		// the replacement replica has no prior in-memory observation or cache.
		api := deploycontrol.NewClusterAPIWith(server.URL, "named-reader", server.Client())
		result, err := readReceiver(ctx, api, "memql", "receiver", f.factory(t))
		require.NoError(t, err)
		return result
	}
	first, peer := observe(), observe()
	require.Equal(t, "default", first.render.RepositoryProject)
	require.Equal(t, first.digest, peer.digest)
	require.Equal(t, first.invariantDigest, peer.invariantDigest)
	require.Zero(t, f.reads[repositoryInventoryFixturePath])
	require.Equal(t, 4, f.reads[namedRepositoryFixturePath], "each replica must independently read and reobserve the named Secret")
	meta := resourceMap(f.objects[namedRepositoryFixturePath], "metadata")
	meta["resourceVersion"] = "12"
	rotated := observe()
	require.NotEqual(t, first.digest, rotated.digest)
	require.NotEqual(t, first.invariantDigest, rotated.invariantDigest, "a saved preparation cannot recover across unseen credential rotation")
	meta["uid"] = "replacement-uid"
	replaced := observe()
	require.NotEqual(t, rotated.invariantDigest, replaced.invariantDigest)
	encoded, err := json.Marshal(first)
	require.NoError(t, err)
	require.JSONEq(t, `{}`, string(encoded))
	require.NotContains(t, fmt.Sprintf("%+v %#v", first, first), "must-not-be-used-or-returned")
}

func TestReceiverNamedRepositoryRefusesUnprovedScope(t *testing.T) {
	for _, fault := range []string{"missing", "foreign URL", "foreign project", "global binding", "wrong label", "wrong name", "wrong namespace", "missing UID", "missing RV", "deleting", "malformed URL", "malformed project", "rotated during observation", "replaced during observation", "cancelled"} {
		t.Run(fault, func(t *testing.T) {
			f := namedRepositoryFixture(t)
			secret := f.objects[namedRepositoryFixturePath]
			meta, data := resourceMap(secret, "metadata"), resourceMap(secret, "data")
			ctx := captureOperator(auth.RoleOwner, "operator")
			switch fault {
			case "missing":
				delete(f.objects, namedRepositoryFixturePath)
			case "foreign URL":
				data["url"] = base64.StdEncoding.EncodeToString([]byte("https://github.com/other/private.git"))
			case "foreign project":
				data["project"] = base64.StdEncoding.EncodeToString([]byte("other-project"))
			case "global binding":
				delete(data, "project")
			case "wrong label":
				meta["labels"] = map[string]any{"argocd.argoproj.io/secret-type": "repo-creds"}
			case "wrong name":
				meta["name"] = "different"
			case "wrong namespace":
				meta["namespace"] = "different"
			case "missing UID":
				delete(meta, "uid")
			case "missing RV":
				delete(meta, "resourceVersion")
			case "deleting":
				meta["deletionTimestamp"] = "2026-10-01T00:00:00Z"
			case "malformed URL":
				data["url"] = "private-invalid-base64"
			case "malformed project":
				data["project"] = "private-invalid-base64"
			case "rotated during observation", "replaced during observation":
				f.before = func(path string, count int) {
					if path == namedRepositoryFixturePath && count == 2 {
						if fault == "rotated during observation" {
							meta["resourceVersion"] = "12"
						} else {
							meta["uid"] = "replacement"
						}
					}
				}
			case "cancelled":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			}
			result, err := readReceiver(ctx, f, "memql", "receiver", f.factory(t))
			require.Error(t, err)
			require.Nil(t, result)
			require.NotContains(t, err.Error(), "private-invalid")
			require.NotContains(t, err.Error(), "must-not-be-used")
			require.Zero(t, f.reads[repositoryInventoryFixturePath], "named refusal must never expand access or fall back to inventory")
		})
	}
}

func TestReceiverNamedRepositoryProfileCoversEveryApplication(t *testing.T) {
	apps := []receiverApplication{{Key: "engine", Namespace: "argocd", Name: "memql"}, {Key: "product", Namespace: "argocd", Name: "product"}}
	makeScope := func() *receiverRepositoryScope {
		return &receiverRepositoryScope{Profile: namedProjectRepositoryProfile, Bindings: []receiverRepositoryBinding{
			{Application: receiverNamed{"argocd", "memql"}, Secret: receiverNamed{"argocd", "engine-repository"}},
			{Application: receiverNamed{"argocd", "product"}, Secret: receiverNamed{"argocd", "product-repository"}},
		}}
	}
	require.NoError(t, makeScope().validate(apps))
	for _, fault := range []string{"unknown profile", "missing member", "duplicate member", "extra member", "foreign application", "foreign namespace", "unsafe name"} {
		t.Run(fault, func(t *testing.T) {
			scope := makeScope()
			switch fault {
			case "unknown profile":
				scope.Profile = "named-global"
			case "missing member":
				scope.Bindings = scope.Bindings[:1]
			case "duplicate member":
				scope.Bindings[1] = scope.Bindings[0]
			case "extra member":
				scope.Bindings = append(scope.Bindings, scope.Bindings[0])
			case "foreign application":
				scope.Bindings[1].Application.Name = "other"
			case "foreign namespace":
				scope.Bindings[0].Secret.Namespace = "other"
			case "unsafe name":
				scope.Bindings[0].Secret.Name = "../secret"
			}
			require.Error(t, scope.validate(apps))
		})
	}
	// The validation is part of receiver admission, not just a helper used by tests.
	f := namedRepositoryFixture(t)
	f.config.RepositoryScope.Profile = "unknown"
	f.saveConfig()
	_, err := readReceiver(captureOperator(auth.RoleOwner, "operator"), f, "memql", "receiver", f.factory(t))
	require.Error(t, err)
	require.Zero(t, f.reads[namedRepositoryFixturePath])
	require.Zero(t, f.reads[repositoryInventoryFixturePath])
}

func TestReceiverNamedProjectDuplicatesCannotChangeConsumedScope(t *testing.T) {
	f := namedRepositoryFixture(t)
	secret := f.objects[namedRepositoryFixturePath]
	// A duplicate/global/foreign registration is outside this reader's grant.
	// Its credentials are irrelevant to the sole consumed value: any matching
	// project registration selected by Argo returns the same project. This is
	// a native observation fixture, not a claim of a live Argo transport test.
	f.objects[repositoryInventoryFixturePath] = map[string]any{"apiVersion": "v1", "kind": "SecretList", "metadata": map[string]any{"resourceVersion": "10"}, "items": []any{secret, secret}}
	ctx := captureOperator(auth.RoleOwner, "operator")
	first, err := readReceiver(ctx, f, "memql", "receiver", f.factory(t))
	require.NoError(t, err)
	require.Equal(t, "default", first.render.RepositoryProject)
	require.Zero(t, f.reads[repositoryInventoryFixturePath])
	// Credential fields are never decoded by this observer or serialized into
	// the render request. The independently scoped native clone grant owns that.
	resourceMap(secret, "data")["password"] = "deliberately-not-base64"
	resourceMap(secret, "metadata")["resourceVersion"] = "12"
	second, err := readReceiver(ctx, f, "memql", "receiver", f.factory(t))
	require.NoError(t, err)
	require.Equal(t, first.render, second.render)
	require.NotEqual(t, first.invariantDigest, second.invariantDigest)
	// Legacy configuration still requires a complete unambiguous inventory.
	f.config.RepositoryScope = nil
	f.saveConfig()
	_, err = readReceiver(ctx, f, "memql", "receiver", f.factory(t))
	require.ErrorContains(t, err, "ambiguous")
	require.Positive(t, f.reads[repositoryInventoryFixturePath])
}

func TestReceiverNamedRepositoryGroupUsesEachActualProject(t *testing.T) {
	f := namedRepositoryFixture(t)
	f.config.FormatVersion = 2
	f.config.Application = receiverNamed{}
	f.config.Applications = []receiverApplication{{Key: "engine", Namespace: "argocd", Name: "memql"}, {Key: "product", Namespace: "argocd", Name: "product"}}
	f.config.ImageBindings[0].Application = "engine"
	f.config.RepositoryScope.Bindings = append(f.config.RepositoryScope.Bindings, receiverRepositoryBinding{Application: receiverNamed{"argocd", "product"}, Secret: receiverNamed{"argocd", "product.repository"}})
	clone := func(value map[string]any) map[string]any {
		body, err := json.Marshal(value)
		require.NoError(t, err)
		var out map[string]any
		require.NoError(t, json.Unmarshal(body, &out))
		return out
	}
	app := clone(f.objects["apis/argoproj.io/v1alpha1/namespaces/argocd/applications/memql"])
	resourceMap(app, "spec")["project"] = "product"
	f.put("argoproj.io/v1alpha1", "Application", "applications", "argocd", "product", app)
	f.put("argoproj.io/v1alpha1", "AppProject", "appprojects", "argocd", "product", clone(f.objects["apis/argoproj.io/v1alpha1/namespaces/argocd/appprojects/default"]))
	secret := f.put("v1", "Secret", "secrets", "argocd", "product.repository", clone(f.objects[namedRepositoryFixturePath]))
	resourceMap(secret, "metadata")["uid"] = "uid-product-repository"
	resourceMap(secret, "metadata")["labels"] = map[string]any{"argocd.argoproj.io/secret-type": "repository"}
	resourceMap(secret, "data")["project"] = base64.StdEncoding.EncodeToString([]byte("product"))
	f.saveConfig()
	ctx := captureOperator(auth.RoleOwner, "operator")
	result, err := readReceiver(ctx, f, "memql", "receiver", f.factory(t))
	require.NoError(t, err)
	require.Len(t, result.applications, 2)
	require.Equal(t, "default", result.applications[0].render.RepositoryProject)
	require.Equal(t, "product", result.applications[1].render.RepositoryProject)
	require.Zero(t, f.reads[repositoryInventoryFixturePath])
	// A secret valid for one group member is never borrowed for another.
	f.config.RepositoryScope.Bindings[1].Secret.Name = "project-repository"
	f.saveConfig()
	_, err = readReceiver(ctx, f, "memql", "receiver", f.factory(t))
	require.ErrorContains(t, err, "repository and project")
}
