package memql

import (
	"context"
	"fmt"
	"strings"

	"github.com/znasllc-io/memql/component/auth"
)

// A SOURCE CREDENTIAL IS THE ENGINE'S ACCOUNT OF A VALUE IT SEALED, AND ONLY
// SERVER CODE WRITES IT.
//
// v1:platform:sourceCredential declares @rowAuthz(owner="ownerUserId",
// clusterOwner), which answers WHOSE row it is: the write guard admits its
// owner and a cluster owner, and the owner stamp makes a new row its creator's.
// It does not answer what a person's own write may put on their row, and every
// field but the revocation is a fact only the engine can vouch for:
//
//   - encryptedValue and refreshToken are sealed under the cluster's master
//     key, which exists on nodes and nowhere else;
//   - kind, host, login, externalId and installationIds are what the engine
//     learned about the value from the provider, and what it then does with
//     it follows from them;
//   - fingerprint, label, expiresAt and lastUsedAt describe a value and a use
//     the person never performed.
//
// The writers are @serverOnly for that reason -- createSourceCredential,
// createGithubAppGrant, updateGithubAppGrant, refreshGithubAppGrantToken,
// recordGithubAppInstallations and touchSourceCredential, each reached under
// internal origin by component/packages or the connect callback. But
// @serverOnly decides who may CALL a named construct. The raw insert() literal
// names none, and neither does the update{} body of an inline or authored
// mutation; each reaches executeWrite under the caller's own actor, where the
// tier admits the row's owner. The sibling concepts were already held to this
// (v1:platform:sourceConnection and v1:platform:externalConnection,
// source_connection_write.go); this is the same rule for the credential they
// fetch under.
//
// THE RULE, for a write without internal origin:
//
//   - it does not CREATE a row: a credential comes to exist only through the
//     capability that seals it;
//   - on a stored row it may change `status` to "revoked" and stamp
//     `revokedAt`, and nothing else. That is revokeSourceCredential, the one
//     write that is the person's own, and a cluster owner's through the tier's
//     escape. The transition is one-way for a person: a reconnect is server
//     code, and it is what makes a grant active again.
//
// KEYED ON A CHANGE AND ON ORIGIN, the shape of the registration consent guard
// (worker_sharing_write_guard.go). The read-merge carries every field through
// a revoke unchanged, so a field is judged only when the row about to be
// written differs from the stored one. And only internal origin passes: no
// caller -- the owner and a cluster owner included -- has a reason to write
// these fields any other way than through the capability that produced them.
//
// FUTURE FIELDS. A field added to the concept is server-written until somebody
// decides otherwise: TestEverySourceCredentialFieldHasAWriter fails the build
// on a declared field this file does not name.
//
// Regression tests: platform_source_credential_write_db_test.go.

const conceptPlatformSourceCredential = "v1:platform:sourceCredential"

// sourceCredentialServerFields are the fields of a source credential only
// server code writes. Mirrors dsl/platform/concepts.memql.
var sourceCredentialServerFields = []string{
	"kind",
	"host",
	"label",
	"encryptedValue",
	"fingerprint",
	"lastUsedAt",
	"login",
	"externalId",
	"refreshToken",
	"expiresAt",
	"installationIds",
}

// The three fields with a rule of their own: who owns the row, and the
// revocation a person may make.
const (
	sourceCredentialOwnerField     = "ownerUserId"
	sourceCredentialStatusField    = "status"
	sourceCredentialRevokedAtField = "revokedAt"
	sourceCredentialRevoked        = "revoked"
)

// validateSourceCredentialServerOnly refuses a write to a source credential
// that creates a row, or changes anything but its revocation, without internal
// origin.
//
// Called by executeWrite on the final row, after the read-merge and the owner
// stamps and before canonicalization: prior is the stored row (nil on a
// create), final the row about to be written. The owner is compared tolerantly
// for that reason -- the stored value is canonical and a stamp writes it bare.
func validateSourceCredentialServerOnly(ctx context.Context, prior, final map[string]any) error {
	if auth.OriginFromContext(ctx).IsInternal() {
		return nil
	}
	if prior == nil {
		return fmt.Errorf(
			"%s: create refused -- the row is server-written. A source credential comes to exist only "+
				"through the capability that seals its value under the cluster's key (sourceCredentialCreate "+
				"for a pasted token, the GitHub connect callback for a grant), which writes under internal "+
				"origin. See component/memql/platform_source_credential_write_guard.go",
			conceptPlatformSourceCredential)
	}
	for _, field := range sourceCredentialServerFields {
		if payloadFieldChanged(prior, final, field) {
			return errSourceCredentialWrite(field)
		}
	}
	if registrationOwnerChanged(prior, final) {
		return errSourceCredentialWrite(sourceCredentialOwnerField)
	}
	revoked := strings.TrimSpace(stringFromAny(final[sourceCredentialStatusField])) == sourceCredentialRevoked
	if payloadFieldChanged(prior, final, sourceCredentialStatusField) && !revoked {
		return errSourceCredentialWrite(sourceCredentialStatusField)
	}
	if payloadFieldChanged(prior, final, sourceCredentialRevokedAtField) && !revoked {
		return errSourceCredentialWrite(sourceCredentialRevokedAtField)
	}
	return nil
}

func errSourceCredentialWrite(field string) error {
	return fmt.Errorf(
		"%s: write to `%s` refused -- the field is server-written. A source credential's value, what it "+
			"is for and what it reaches are written by the capability that sealed it, under internal "+
			"origin; a person's own write may revoke the credential (revokeSourceCredential) and nothing "+
			"else, and a revoked one is replaced, not reactivated. A raw insert() or an inline update{} "+
			"never consults a writer's @serverOnly, so the row is judged here. See "+
			"component/memql/platform_source_credential_write_guard.go",
		conceptPlatformSourceCredential, field)
}
