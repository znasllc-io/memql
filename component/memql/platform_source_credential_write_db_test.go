package memql

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/uptrace/bun"

	"github.com/znasllc-io/memql/component/auth"
)

// WHO WRITES A SOURCE CREDENTIAL'S ROW, against a real engine and a real
// Postgres.
//
// v1:platform:sourceCredential is a person's sealed token for a private source.
// Its row tier answers WHOSE row it is: the owner, or a cluster owner. This
// holds the other question, which the tier cannot: WHAT a person's own write
// may put on their row. Every field but the revocation is the engine's account
// of something only the engine did -- sealed a value under the cluster's key,
// refreshed it, asked GitHub which installations a grant reaches -- so:
//
//   - a row comes to exist only through server code, under internal origin;
//   - on a stored row, a write without internal origin may revoke it and
//     nothing else;
//   - the engine's own writers (the capability that seals a pasted token, the
//     connect callback, the fetcher's refresh and heartbeat) are unchanged.
//
// The named writers are @serverOnly, which decides who may CALL them. It does
// not decide the raw insert literal, which names no construct; this does.
//
// Postgres-gated like its neighbours; CI's db-tests lane sets
// MEMQL_REQUIRE_DB=1, so a skip there is a failure rather than a green.

const (
	sourceCredentialConceptForTest = "v1:platform:sourceCredential"
	// sourceCredentialWriteRefusal is the phrase the write rule's refusal
	// carries, so a write refused for another reason does not read as the rule
	// working.
	sourceCredentialWriteRefusal = "server-written"
)

// sourceCredentialFixture is one person and the two kinds of credential server
// code can land for them: a pasted token, and a GitHub App grant.
type sourceCredentialFixture struct {
	suffix string
	// person is the credentials' owner; server is the same actor with internal
	// origin stamped, which is how every engine writer of this concept arrives.
	person, server context.Context
	personId       string
	tokenId        string
	grantId        string
}

func newSourceCredentialFixture(t *testing.T, eng *MemQLEngine, tag string) sourceCredentialFixture {
	t.Helper()
	suffix := uniqueSuffix("source-credential-" + tag)
	personId := "source-credential-person-" + suffix
	f := sourceCredentialFixture{
		suffix:   suffix,
		personId: personId,
		person:   rankActorCtx(personId, auth.RoleWriter),
		tokenId:  "source-credential-token-" + suffix,
		grantId:  "source-credential-grant-" + suffix,
	}
	f.server = auth.ContextWithInternalOrigin(f.person)

	_, err := runSiteMutation(t, f.server, eng, "createSourceCredential", map[string]any{
		"credentialId": f.tokenId, "host": "github.com", "label": "pasted token",
		"encryptedValue": "sealed-by-the-server-token", "fingerprint": "...oken",
	})
	require.NoError(t, err, "server code lands a sealed token for a person: the sourceCredentialCreate path")
	_, err = runSiteMutation(t, f.server, eng, "createGithubAppGrant", map[string]any{
		"credentialId": f.grantId, "host": "github.com", "label": "GitHub",
		"encryptedValue": "sealed-by-the-server-grant", "fingerprint": "...rant",
		"refreshToken": "sealed-refresh-by-the-server", "expiresAt": "2030-01-01T00:00:00Z",
		"login": "octocat", "externalId": "583231", "installationIds": []string{"1001"},
	})
	require.NoError(t, err, "server code lands a grant for a person: the connect callback's path")
	return f
}

func (f sourceCredentialFixture) stored(id string) string {
	return sourceCredentialConceptForTest + ":" + id
}

func storedSourceCredential(t *testing.T, db *bun.DB, f sourceCredentialFixture, id string) map[string]any {
	t.Helper()
	return latestPayload(t, context.Background(), db, sourceCredentialConceptForTest, f.stored(id))
}

func TestASourceCredentialComesToExistOnlyThroughServerCode(t *testing.T) {
	eng, db, _ := sharedReadMergeEngine(t)
	if eng == nil {
		return // skipped: no database
	}
	f := newSourceCredentialFixture(t, eng, "create")

	// The control: both rows exist, owned by the person, because server code
	// wrote them.
	for _, id := range []string{f.tokenId, f.grantId} {
		row := storedSourceCredential(t, db, f, id)
		require.Contains(t, fmt.Sprint(row["ownerUserId"]), f.personId, "the server-written row is not the person's")
		require.Equal(t, "active", row["status"])
	}

	callers := []struct {
		name string
		ctx  context.Context
	}{
		{"the person", f.person},
		{"a developer", rankActorCtx("source-credential-developer-"+f.suffix, auth.RoleDeveloper)},
		// Keyed on origin and not on rank: no caller has a row of this concept
		// to create any other way than through the capability that seals it.
		{"a cluster owner", clusterOwnerCtx("source-credential-owner-" + f.suffix)},
	}
	for _, caller := range callers {
		t.Run(caller.name, func(t *testing.T) {
			id := fmt.Sprintf("source-credential-raw-%s-%s", strings.ReplaceAll(caller.name, " ", "-"), f.suffix)
			err := rawInsertFor(t, caller.ctx, eng, sourceCredentialConceptForTest, id, map[string]any{
				"host": "github.com", "label": "composed by the caller",
				"encryptedValue": "composed-by-the-caller", "fingerprint": "...ller", "status": "active",
			})
			require.Error(t, err, "%s created a source credential row with the raw literal", caller.name)
			require.Contains(t, err.Error(), sourceCredentialWriteRefusal, "%s was refused, but not by the source credential rule", caller.name)
			assertRowCount(t, db, sourceCredentialConceptForTest, id, 0)
		})
	}
}

func TestASourceCredentialsFieldsAreServerWritten(t *testing.T) {
	eng, db, _ := sharedReadMergeEngine(t)
	if eng == nil {
		return // skipped: no database
	}
	f := newSourceCredentialFixture(t, eng, "fields")

	// Every field but the revocation, each with a value the stored row does not
	// carry. Named one at a time: the read-merge keeps what a delta omits, so
	// each of these is a write that would change exactly one field.
	deltas := map[string]any{
		"encryptedValue":  "composed-by-the-caller",
		"refreshToken":    "refresh-composed-by-the-caller",
		"fingerprint":     "...ller",
		"host":            "example.test",
		"kind":            "token",
		"label":           "renamed by the caller",
		"lastUsedAt":      "2031-02-03T04:05:06Z",
		"expiresAt":       "2031-02-03T04:05:06Z",
		"login":           "somebody-else",
		"externalId":      "999999",
		"installationIds": []string{"1001", "2002"},
	}
	before := storedSourceCredential(t, db, f, f.grantId)

	for field, value := range deltas {
		t.Run("the person/"+field, func(t *testing.T) {
			err := rawInsertFor(t, f.person, eng, sourceCredentialConceptForTest, f.grantId, map[string]any{field: value})
			require.Error(t, err, "the person wrote `%s` onto their own source credential", field)
			require.Contains(t, err.Error(), sourceCredentialWriteRefusal, "the write of `%s` was refused, but not by the source credential rule", field)
			require.Equal(t, before, storedSourceCredential(t, db, f, f.grantId), "the refused write of `%s` changed the stored row", field)
		})
	}

	// The same for a cluster owner, whom the row tier admits to the row: the
	// tier says whose row it is, never what may be written onto it.
	owner := clusterOwnerCtx("source-credential-owner-" + f.suffix)
	for _, field := range []string{"encryptedValue", "host", "installationIds"} {
		t.Run("a cluster owner/"+field, func(t *testing.T) {
			err := rawInsertFor(t, owner, eng, sourceCredentialConceptForTest, f.grantId, map[string]any{field: deltas[field]})
			require.Error(t, err, "a cluster owner wrote `%s` onto somebody's source credential", field)
			require.Contains(t, err.Error(), sourceCredentialWriteRefusal)
			require.Equal(t, before, storedSourceCredential(t, db, f, f.grantId))
		})
	}
	t.Run("a cluster owner/ownerUserId", func(t *testing.T) {
		err := rawInsertFor(t, owner, eng, sourceCredentialConceptForTest, f.grantId, map[string]any{"ownerUserId": "source-credential-somebody-else-" + f.suffix})
		require.Error(t, err, "a cluster owner moved a source credential to another person with the raw literal")
		require.Contains(t, err.Error(), sourceCredentialWriteRefusal)
		require.Equal(t, before, storedSourceCredential(t, db, f, f.grantId))
	})

	// THE ENGINE'S OWN WRITERS STILL LAND: a refresh, the installations a
	// delivery reported, the heartbeat, and a reconnect.
	t.Run("server code", func(t *testing.T) {
		_, err := runSiteMutation(t, f.server, eng, "refreshGithubAppGrantToken", map[string]any{
			"credentialId": f.grantId, "encryptedValue": "sealed-again-by-the-server", "fingerprint": "...gain",
			"refreshToken": "sealed-refresh-again", "expiresAt": "2030-06-01T00:00:00Z",
		})
		require.NoError(t, err, "the engine's refresh of a grant")
		_, err = runSiteMutation(t, f.server, eng, "recordGithubAppInstallations", map[string]any{
			"credentialId": f.grantId, "installationIds": []string{"1001", "3003"},
		})
		require.NoError(t, err, "the webhook's account of a grant's installations")
		_, err = runSiteMutation(t, f.server, eng, "touchSourceCredential", map[string]any{
			"credentialId": f.tokenId, "lastUsedAt": "2030-03-04T05:06:07Z",
		})
		require.NoError(t, err, "the fetcher's heartbeat")
		_, err = runSiteMutation(t, f.server, eng, "updateGithubAppGrant", map[string]any{
			"credentialId": f.grantId, "encryptedValue": "sealed-on-reconnect", "fingerprint": "...nect",
			"login": "octocat-renamed", "installationIds": []string{"1001"},
		})
		require.NoError(t, err, "the connect callback's update of a grant")

		grant := storedSourceCredential(t, db, f, f.grantId)
		require.Equal(t, "sealed-on-reconnect", grant["encryptedValue"])
		require.Equal(t, "octocat-renamed", grant["login"])
		require.Equal(t, []any{"1001"}, grant["installationIds"])
		require.Equal(t, "2030-03-04T05:06:07Z", storedSourceCredential(t, db, f, f.tokenId)["lastUsedAt"])
	})
}

func TestAPersonStillRevokesTheirOwnSourceCredential(t *testing.T) {
	eng, db, _ := sharedReadMergeEngine(t)
	if eng == nil {
		return // skipped: no database
	}
	f := newSourceCredentialFixture(t, eng, "revoke")

	// Somebody else's revoke is the row tier's to refuse, as it was.
	other := rankActorCtx("source-credential-other-"+f.suffix, auth.RoleWriter)
	_, err := runSiteMutation(t, other, eng, "revokeSourceCredential", map[string]any{"credentialId": f.tokenId})
	require.Error(t, err, "another person revoked a credential that is not theirs")
	require.Contains(t, err.Error(), "row-authz", "another person's revoke was refused, but not by the row tier")
	require.Equal(t, "active", storedSourceCredential(t, db, f, f.tokenId)["status"])

	// The person's own, by the mutation the sourceCredentialRevoke capability
	// issues under their actor.
	before := storedSourceCredential(t, db, f, f.tokenId)
	_, err = runSiteMutation(t, f.person, eng, "revokeSourceCredential", map[string]any{"credentialId": f.tokenId})
	require.NoError(t, err, "a person could not revoke their own source credential")
	after := storedSourceCredential(t, db, f, f.tokenId)
	require.Equal(t, "revoked", after["status"])
	require.NotEmpty(t, after["revokedAt"], "the revoke did not stamp when")
	for field, value := range before {
		if field == "status" || field == "revokedAt" {
			continue
		}
		require.Equal(t, value, after[field], "the revoke changed `%s`", field)
	}

	// REVOCATION IS ONE-WAY for a person: the row stays as history and a new
	// credential is added. A reconnect, which is server code, is what makes a
	// grant active again.
	err = rawInsertFor(t, f.person, eng, sourceCredentialConceptForTest, f.tokenId, map[string]any{"status": "active"})
	require.Error(t, err, "the person reactivated a revoked source credential with the raw literal")
	require.Contains(t, err.Error(), sourceCredentialWriteRefusal)
	require.Equal(t, "revoked", storedSourceCredential(t, db, f, f.tokenId)["status"])

	// A cluster owner ends somebody's credential -- offboarding -- through the
	// same mutation, by the row tier's explicit escape.
	owner := clusterOwnerCtx("source-credential-owner-" + f.suffix)
	_, err = runSiteMutation(t, owner, eng, "revokeSourceCredential", map[string]any{"credentialId": f.grantId})
	require.NoError(t, err, "a cluster owner could not revoke a person's source credential")
	grant := storedSourceCredential(t, db, f, f.grantId)
	require.Equal(t, "revoked", grant["status"])
	require.Contains(t, fmt.Sprint(grant["ownerUserId"]), f.personId, "a cluster owner's revoke moved the row to themselves")
	require.Equal(t, "sealed-by-the-server-grant", grant["encryptedValue"])
}
