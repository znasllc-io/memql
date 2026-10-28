package packages

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/znasllc-io/memql/component/secret"
)

func TestRepositoryAuthorityDoesNotTurnVisibilityIntoWritePermission(t *testing.T) {
	t.Setenv(secret.EnvMasterKey, testMasterKey)
	for _, permissions := range []string{`{}`, `{"pull":true}`, `{"triage":true,"pull":true}`} {
		t.Run(permissions, func(t *testing.T) {
			hub := installedRepo(newGrantHub(), "acme", "widget")
			hub.body("/repos/acme/widget", http.StatusOK, `{"id":123,"full_name":"acme/widget","permissions":`+permissions+`}`)
			i, _, _, _ := grantHarness(t, hub, sealedGrantRow(t, grantRowOpts{}))
			authority, err := i.RepositoryAccess(context.Background(), grantCredentialId, grantOwner, "acme", "widget")
			if err != nil || authority.CanWrite || authority.RepositoryID != 123 {
				t.Fatalf("read authority = %+v, %v", authority, err)
			}
			token, _, err := i.RepositoryWriterToken(context.Background(), grantCredentialId, grantOwner, "acme", "widget")
			if token != "" || RefusalCode(err) != "repository_write_not_authorized" {
				t.Fatalf("read-only connection obtained a writer: error=%v", err)
			}
			if hub.hits("/app/installations/42/access_tokens") != 0 {
				t.Fatal("a token was minted before repository write authority was established")
			}
		})
	}
}

func TestRepositoryWriterRevalidatesAndNarrowsEveryMint(t *testing.T) {
	t.Setenv(secret.EnvMasterKey, testMasterKey)
	for _, permission := range []string{"push", "maintain", "admin"} {
		t.Run(permission, func(t *testing.T) {
			hub := installedRepo(newGrantHub(), "acme", "widget")
			hub.body("/repos/acme/widget", http.StatusOK, `{"id":123,"full_name":"acme/widget","permissions":{"`+permission+`":true}}`)
			i, _, _, _ := grantHarness(t, hub, sealedGrantRow(t, grantRowOpts{}))
			token, installation, err := i.RepositoryWriterToken(context.Background(), grantCredentialId, grantOwner, "acme", "widget")
			if err != nil || token == "" || installation != 42 {
				t.Fatalf("authorized mint failed: installation=%d error=%v", installation, err)
			}
			var body struct {
				Repositories []string          `json:"repositories"`
				Permissions  map[string]string `json:"permissions"`
			}
			bodies := hub.bodies()
			if err := json.Unmarshal([]byte(bodies[len(bodies)-1]), &body); err != nil {
				t.Fatal(err)
			}
			if len(body.Repositories) != 1 || body.Repositories[0] != "widget" || len(body.Permissions) != 2 || body.Permissions["contents"] != "write" || body.Permissions["pull_requests"] != "write" {
				t.Fatalf("writer scope = %+v", body)
			}
			hub.body("/repos/acme/widget", http.StatusOK, `{"id":123,"full_name":"acme/widget","permissions":{"pull":true}}`)
			token, _, err = i.RepositoryWriterToken(context.Background(), grantCredentialId, grantOwner, "acme", "widget")
			if token != "" || RefusalCode(err) != "repository_write_not_authorized" || hub.hits("/app/installations/42/access_tokens") != 1 {
				t.Fatalf("cached token survived authority revocation: %v", err)
			}
		})
	}
}

// The login a release train lands on is read live with the person's own
// token, after the same access check a write needs: a connection that lost
// the repository names nobody.
func TestRepositoryAccessReportsTheLogin(t *testing.T) {
	t.Setenv(secret.EnvMasterKey, testMasterKey)
	hub := installedRepo(newGrantHub(), "acme", "widget")
	hub.body("/repos/acme/widget", http.StatusOK, `{"id":123,"full_name":"acme/widget","permissions":{"push":true}}`)
	hub.body("/user", http.StatusOK, `{"login":"Ada-GH","id":9}`)
	i, _, _, _ := grantHarness(t, hub, sealedGrantRow(t, grantRowOpts{}))
	login, err := i.RepositoryOwnerLogin(context.Background(), grantCredentialId, grantOwner, "acme", "widget")
	if err != nil || login != "Ada-GH" {
		t.Fatalf("RepositoryOwnerLogin = %q, %v; want Ada-GH", login, err)
	}

	lost := installedRepo(newGrantHub(), "acme", "widget")
	lost.body("/repos/acme/widget", http.StatusNotFound, `{"message":"Not Found"}`)
	lost.body("/user", http.StatusOK, `{"login":"Ada-GH","id":9}`)
	i, _, _, _ = grantHarness(t, lost, sealedGrantRow(t, grantRowOpts{}))
	if login, err := i.RepositoryOwnerLogin(context.Background(), grantCredentialId, grantOwner, "acme", "widget"); err == nil || login != "" {
		t.Fatalf("a connection that cannot see the repository named %q", login)
	}
	if lost.hits("/user") != 0 {
		t.Errorf("the login was read before repository access was established")
	}
}

func TestRepositoryAuthorityRequiresTheProvidersExactStableIdentity(t *testing.T) {
	t.Setenv(secret.EnvMasterKey, testMasterKey)
	for _, body := range []string{
		`{"full_name":"acme/widget","permissions":{"admin":true}}`,
		`{"id":123,"full_name":"other/widget","permissions":{"admin":true}}`,
	} {
		hub := installedRepo(newGrantHub(), "acme", "widget")
		hub.body("/repos/acme/widget", http.StatusOK, body)
		i, _, _, _ := grantHarness(t, hub, sealedGrantRow(t, grantRowOpts{}))
		token, _, err := i.RepositoryWriterToken(context.Background(), grantCredentialId, grantOwner, "acme", "widget")
		if token != "" || RefusalCode(err) != "repository_identity_unavailable" || hub.hits("/app/installations/42/access_tokens") != 0 {
			t.Fatalf("unconfirmed identity obtained a writer: %v", err)
		}
	}
}

// A release train boards a pull request on its pipeline's check, so its token
// must read checks -- a private repository's installation token cannot without
// it -- and name the App that minted it, so the train counts only that App's
// check. It is still a writer: the same fresh authority check refuses a
// connection that can only read.
func TestRepositoryTrainTokenReadsChecksAndNamesTheApp(t *testing.T) {
	t.Setenv(secret.EnvMasterKey, testMasterKey)
	hub := installedRepo(newGrantHub(), "acme", "widget")
	hub.body("/repos/acme/widget", http.StatusOK, `{"id":123,"full_name":"acme/widget","permissions":{"push":true}}`)
	i, _, _, _ := grantHarness(t, hub, sealedGrantRow(t, grantRowOpts{}))
	token, installation, appID, err := i.RepositoryTrainToken(context.Background(), grantCredentialId, grantOwner, "acme", "widget")
	if err != nil || token == "" || installation != 42 || appID != 1234567 {
		t.Fatalf("RepositoryTrainToken = token %t, installation %d, app %d, %v", token != "", installation, appID, err)
	}
	var body struct {
		Repositories []string          `json:"repositories"`
		Permissions  map[string]string `json:"permissions"`
	}
	bodies := hub.bodies()
	if err := json.Unmarshal([]byte(bodies[len(bodies)-1]), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Repositories) != 1 || body.Repositories[0] != "widget" || len(body.Permissions) != 3 ||
		body.Permissions["contents"] != "write" || body.Permissions["pull_requests"] != "write" || body.Permissions["checks"] != "read" {
		t.Fatalf("train token scope = %+v", body)
	}
	hub.body("/repos/acme/widget", http.StatusOK, `{"id":123,"full_name":"acme/widget","permissions":{"pull":true}}`)
	if token, _, _, err := i.RepositoryTrainToken(context.Background(), grantCredentialId, grantOwner, "acme", "widget"); token != "" || RefusalCode(err) != "repository_write_not_authorized" {
		t.Fatalf("a read-only connection obtained a train token: %v", err)
	}
}
