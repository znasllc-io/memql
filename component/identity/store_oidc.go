package identity

import (
	"context"
	"fmt"
	"strings"

	"github.com/znasllc-io/memql/component/auth"
	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
)

// UPSTREAM-PROVIDER LINKS (memql#4611).
//
// The row these read and write stores NO SECRET: the credential lives at the
// provider, and this cluster holds only the (issuer, subject) pair the provider
// asserted. What it buys is the thing federation gets wrong when it is missing
// -- a returning person lands on their EXISTING user row rather than a
// duplicate, and keeps doing so after a rename, an address change, or an
// address being reassigned to somebody else.

// OidcLinkRow is one upstream-provider link.
type OidcLinkRow struct {
	IdentityId string
	UserId     string
	Issuer     string
	Subject    string
	Email      string
	Active     bool
}

// LookupOidcLink resolves the link for one (issuer, subject) pair, or nil.
//
// ISSUER IS PART OF THE KEY because a subject is only unique within its issuer.
// Without it, a cluster that ever changed provider -- or was pointed at a second
// one -- could match a subject from one directory against a row created by
// another.
func (s *Store) LookupOidcLink(ctx context.Context, issuer, subject string) (*OidcLinkRow, error) {
	issuer = strings.TrimRight(strings.TrimSpace(issuer), "/")
	subject = strings.TrimSpace(subject)
	if issuer == "" || subject == "" {
		// Not an error: an incomplete pair matches nothing, and the caller's
		// next step (register) is the right one. Returning an error here would
		// turn "we have never seen this person" into a sign-in failure.
		return nil, nil
	}
	query := fmt.Sprintf(`query oidcIdentityBySubject(issuer: %s, subject: %s)`,
		dslJSONString(issuer), dslJSONString(subject))
	nodes, err := s.executeAndExtractInternal(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("identity.store: lookup oidc link: %w", err)
	}
	if len(nodes) == 0 || nodes[0] == nil {
		return nil, nil
	}
	return oidcLinkFromNode(nodes[0]), nil
}

// CreateOidcLink records that a user signs in through an upstream provider.
//
// Called ONLY by the code that has verified an id token carrying this pair. The
// mutation is @serverOnly for that reason: a client able to write
// credentials.subject could claim any upstream identity it liked, and the next
// federated sign-in as that subject would resolve to the row that claimed it.
func (s *Store) CreateOidcLink(ctx context.Context, identityId, userId, label, issuer, subject, email string) error {
	if strings.TrimSpace(identityId) == "" || strings.TrimSpace(userId) == "" {
		return fmt.Errorf("identity.store: CreateOidcLink: identityId and userId required")
	}
	issuer = strings.TrimRight(strings.TrimSpace(issuer), "/")
	if issuer == "" || strings.TrimSpace(subject) == "" {
		return fmt.Errorf("identity.store: CreateOidcLink: issuer and subject required")
	}
	if strings.TrimSpace(label) == "" {
		label = issuer
	}
	query := fmt.Sprintf(
		`mutation createOidcIdentity(identityId: %s, userId: %s, label: %s, issuer: %s, subject: %s, email: %s)`,
		dslJSONString(identityId), dslJSONString(userId), dslJSONString(label),
		dslJSONString(issuer), dslJSONString(subject), dslJSONString(email))
	// INTERNAL ORIGIN, STAMPED AT THE CALL. createOidcIdentity is @serverOnly,
	// and the engine refuses such a construct unless the context carries
	// internal origin -- so without this the call cannot succeed, on any
	// cluster, ever. Named here rather than left to executeAndExtractInternal's
	// wrapper because TestEveryGoCallerOfAServerOnlyConstructStampsInternalOrigin
	// reads the FILE, and a stamp it cannot see is a stamp the next reader
	// cannot see either.
	if _, err := s.executeAndExtract(auth.ContextWithInternalOrigin(ctx), query); err != nil {
		return fmt.Errorf("identity.store: create oidc link: %w", err)
	}
	return nil
}

func oidcLinkFromNode(node *memqlv1.MemoryNode) *OidcLinkRow {
	if node == nil {
		return nil
	}
	g := newFieldGetter(node)
	row := &OidcLinkRow{
		IdentityId: firstNonEmpty(g.str("id"), node.GetId()),
		UserId:     g.str("userId"),
		Active:     g.boolField("active"),
	}
	// credentials is a nested object; the field getter reads scalars, so the
	// pair is read off the struct directly.
	if node.Payload != nil {
		if v, ok := node.Payload.GetFields()["credentials"]; ok && v != nil {
			if sv := v.GetStructValue(); sv != nil {
				f := sv.GetFields()
				row.Issuer = strings.TrimSpace(f["issuer"].GetStringValue())
				row.Subject = strings.TrimSpace(f["subject"].GetStringValue())
				row.Email = strings.TrimSpace(f["email"].GetStringValue())
			}
		}
	}
	return row
}
