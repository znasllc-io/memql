package packages

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/packages/githubapp"
	"github.com/znasllc-io/memql/component/secret"
)

const twoInstallations = `{"total_count":2,"installations":[{"id":42,"account":{"id":100,"login":"acme","type":"Organization"},"repository_selection":"selected"},{"id":43,"account":{"id":200,"login":"alice","type":"User"},"repository_selection":"all"}]}`

func TestSourceConnectionRepositoryScopeAndPagination(t *testing.T) {
	t.Setenv(secret.EnvMasterKey, testMasterKey)
	hub := newGrantHub().body("/user/installations", http.StatusOK, twoInstallations)
	hub.on("/user/installations/42/repositories", func(r *http.Request) (int, string) {
		page := r.URL.Query().Get("page")
		if page == "2" {
			return http.StatusOK, `{"total_count":101,"repositories":[{"full_name":"acme/last","name":"last","html_url":"https://github.com/acme/last","owner":{"login":"acme"}}]}`
		}
		return http.StatusOK, `{"total_count":101,"repositories":[{"full_name":"acme/first","name":"first","html_url":"https://github.com/acme/first","owner":{"login":"acme"}}]}`
	})
	i, _, engine, d := grantHarness(t, hub, sealedGrantRow(t, grantRowOpts{}))
	engine.rows["query sourceConnectionById"] = []map[string]any{{"id": "connection", "ownerUserId": grantOwner, "credentialId": grantCredentialId, "installationId": "42", "status": "active"}}
	ctx := callerCtx(grantOwner)
	first, err := sourceRepositoriesForConnection(ctx, d, "connection", grantCredentialId, 1)
	if err != nil || first.NextPage != 2 || len(first.Repositories) != 1 || len(first.Installations) != 1 || first.Repositories[0].InstallationId != "42" {
		t.Fatalf("first page: %+v %v", first, err)
	}
	second, err := sourceRepositoriesForConnection(ctx, d, "connection", grantCredentialId, 2)
	if err != nil || second.NextPage != 0 || second.Repositories[0].FullName != "acme/last" {
		t.Fatalf("second page: %+v %v", second, err)
	}
	if hub.hits("/user/installations/43/repositories") != 0 {
		t.Fatal("selected source listed a sibling installation")
	}
	nodes, err := i.handleSourceInstallations(ctx, map[string]any{"credentialId": grantCredentialId}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(replyPayload(t, nodes)["installations"].([]any)) != 2 {
		t.Fatal("discovery must include both organization and personal installation")
	}
	for _, tc := range []struct{ name, owner, status, credential string }{
		{"foreign owner", "someone-else", "active", grantCredentialId},
		{"removed", grantOwner, "removed", grantCredentialId},
		{"forged grant", grantOwner, "active", "other-grant"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			engine.rows["query sourceConnectionById"][0]["ownerUserId"] = tc.owner
			engine.rows["query sourceConnectionById"][0]["status"] = tc.status
			before := len(hub.seen())
			_, err := sourceRepositoriesForConnection(ctx, d, "connection", tc.credential, 1)
			if RefusalCode(err) != CodeSourceConnectionUnavailable {
				t.Fatalf("expected unavailable, got %v", err)
			}
			if len(hub.seen()) != before {
				t.Fatal("invalid binding reached GitHub")
			}
		})
	}
}

func TestGrantFetchRechecksUserScopeBeforeCachedInstallationToken(t *testing.T) {
	hub := installedRepo(newGrantHub(), "acme", "widget")
	client := githubapp.New(grantAppConfig(t), githubapp.WithHTTPClient(&http.Client{Transport: hub}))
	grant := ResolvedCredential{Id: grantCredentialId, Kind: "github_app", Bearer: grantUserToken}
	if _, err := installationBearer(context.Background(), client, grant, "acme", "widget"); err != nil {
		t.Fatal(err)
	}
	if hub.bearerOn("/repos/acme/widget") != grantUserToken {
		t.Fatal("user access was not checked with the grant")
	}
	for _, tc := range []struct {
		name, installations string
		repoStatus          int
	}{
		{"another installation", `{"total_count":1,"installations":[{"id":99}]}`, 200},
		{"suspended", `{"total_count":1,"installations":[{"id":42,"suspended_at":"2026-01-01T00:00:00Z"}]}`, 200},
		{"repository inaccessible", installationsBody, 404},
	} {
		t.Run(tc.name, func(t *testing.T) {
			hub.body("/user/installations", http.StatusOK, tc.installations)
			hub.body("/repos/acme/widget", tc.repoStatus, `{}`)
			if _, err := installationBearer(context.Background(), client, grant, "acme", "widget"); RefusalCode(err) != "repository_not_accessible" {
				t.Fatalf("forged fetch admitted: %v", err)
			}
			if hub.hits("/app/installations/42/access_tokens") != 1 {
				t.Fatal("denied user scope reached token mint")
			}
		})
	}
}

func TestUserInstallationsReadsBeyondFirstHundred(t *testing.T) {
	hub := newGrantHub()
	hub.on("/user/installations", func(r *http.Request) (int, string) {
		if r.URL.Query().Get("page") == "2" {
			return 200, `{"total_count":101,"installations":[{"id":101,"account":{"id":200,"login":"last"}}]}`
		}
		rows := make([]string, 100)
		for j := range rows {
			rows[j] = fmt.Sprintf(`{"id":%d}`, j+1)
		}
		return 200, `{"total_count":101,"installations":[` + strings.Join(rows, ",") + `]}`
	})
	client := githubapp.New(grantAppConfig(t), githubapp.WithHTTPClient(&http.Client{Transport: hub}))
	installations, err := client.UserInstallations(context.Background(), grantUserToken)
	if err != nil || len(installations) != 101 || installations[100].Account.Login != "last" {
		t.Fatalf("installations=%v err=%v", installations, err)
	}
	if hub.hits("/user/installations") != 2 {
		t.Fatal("did not read precisely two installation pages")
	}
}
