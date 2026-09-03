package packages

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/packages/githubapp"
	"github.com/znasllc-io/memql/component/secret"
)

// credentials.go -- personal source credentials (epic memql#4885, D10).
//
// A package used to NAME a cluster-wide v1:platform:globalSecret, and that had
// two defects that were one defect: nothing in the OS could create one, and
// any package owner could name any secret, so the fetcher could be made to
// fetch under an operator's token by whoever knew its name. Now a credential
// is a row the PERSON owns, sealed once on the node that received the token,
// and resolved under the PACKAGE OWNER's actor -- so the name a package
// carries is only ever worth what its owner can read.
//
// Three things in this file are load-bearing and each has a test:
//
//   - THE TOKEN IS A FUNCTION-LOCAL for the length of one call, in the create
//     handler and again in the resolver. It lands in no row (the row holds
//     ciphertext), no log line (the logger is handed ids and errors, never the
//     value) and no reply (the reply is an id and a fingerprint).
//   - RESOLUTION IS OWNER-SCOPED BY CONSTRUCTION. The sealed read runs under
//     auth.ContextWithUserActor(ctx, <package owner>), through a query whose
//     only predicate is the owner term, so "does not exist" and "belongs to
//     somebody else" are the same zero rows and the same refusal.
//   - DECRYPTION HAPPENS ONLY INSIDE A FETCH, OR THE PROBE THAT STANDS IN FOR
//     ONE. No query returns plaintext, no capability returns plaintext, and
//     the one query returning ciphertext is @serverOnly. The probe (probe.go)
//     unseals through the same function to present the bearer to GitHub and
//     answers a typed reason, never the value -- and, unlike a fetch, stamps
//     no heartbeat (peekCredential).

// ResolvedCredential is one credential, unsealed, in the shape every caller
// that presents a bearer to GitHub needs.
//
// It carries the KIND as well as the bearer, and that is the whole reason it
// is a struct rather than the string it used to be (epic memql#4912). Under a
// pasted token a 401, a 403 and a 404 are one fact -- "this token does not
// reach that repository" -- because the cluster genuinely cannot tell them
// apart. Under a GitHub App grant it can: a 401 is the AUTHORIZATION being
// refused, whose repair is one click of Connect, and a 404 may be the app not
// being installed on that repository, whose repair is an installation link. A
// caller holding only a string cannot make either distinction, so it makes
// neither, and both facts arrive as "private, or not there".
//
// Nothing here is stored by any caller. Bearer is a function-local for the
// length of one request, exactly as the plain token was.
type ResolvedCredential struct {
	// Id is the row this came from, for a refusal that can name it.
	Id string
	// Kind is "token" or "github_app". An ABSENT kind on the row reads as
	// "token" -- every credential written before Connect existed carries no
	// value, and the pre-Connect behaviour is the reading that asserts least.
	Kind string
	// OwnerUserId is whose row it is, as stored. Read back rather than echoed
	// from the argument, because the argument is who the caller BELIEVED owns
	// it and this is what the read gate actually admitted.
	OwnerUserId string
	// Bearer is what goes in Authorization: Bearer. For a token credential it
	// is the pasted value; for a grant it is the person's USER token, already
	// refreshed if it had expired. Background work swaps it for an
	// installation token (C6) -- see installationBearer.
	Bearer string
	// Login and ExternalId identify the GitHub account behind a grant. Empty
	// for a token credential.
	Login      string
	ExternalId string
	// Installations is the grant's stored installation ids -- a DISPLAY CACHE
	// refreshed on owner-actor paths, never the thing a fetch decides by. The
	// fetcher asks GitHub which installation covers a repository, live,
	// because a stored list cannot answer for a repository added since.
	Installations []string
}

// IsGrant reports whether this is a GitHub App grant rather than a pasted
// token. One predicate rather than a `== "github_app"` at six call sites, so
// the absent-kind rule is written once.
func (r ResolvedCredential) IsGrant() bool { return r.Kind == credentialKindGithubApp }

// CredentialResolver unseals a package's credential at the moment of a fetch.
//
// The whole D10 shape is in the signature: the caller holds a credential NAME
// and the package's OWNER, asks for the value at fetch time, and never stores
// what comes back. Resolution runs under the OWNER's actor rather than the
// caller's, so a cluster owner deploying somebody's package fetches under that
// package's own credential (correct: they are deploying that package), and a
// package naming another person's credential resolves zero rows and is
// refused by name.
type CredentialResolver func(ctx context.Context, credentialId, ownerUserId string) (ResolvedCredential, error)

// The credential's declared host, kinds and statuses, mirroring the enums on
// v1:platform:sourceCredential.
const (
	credentialHostGitHub    = "github.com"
	credentialStatusActive  = "active"
	credentialStatusRevoked = "revoked"

	// credentialKindToken is the pasted value, and it is also what an ABSENT
	// kind means. The field is deliberately not required on the concept: every
	// row written before Connect carries no value, and nothing rewrites a row
	// to add one.
	credentialKindToken = "token"
	// credentialKindGithubApp is an authorization grant from the GitHub App.
	credentialKindGithubApp = "github_app"
)

// refreshMargin is how long before its stated expiry a user token is refreshed
// rather than used. A token that expires mid-fetch is a deploy that fails
// halfway with a 401, and a refresh costs one round trip on the node that
// already holds the sealed row.
const refreshMargin = time.Minute

// sourceCredentialConcept is the row kind the create handler mints ids for.
const sourceCredentialConcept = "v1:platform:sourceCredential"

// resolveCredential is the production CredentialResolver for a FETCH: the
// unseal, plus the lastUsedAt heartbeat, because a fetch is what the heartbeat
// records.
func (s *store) resolveCredential(ctx context.Context, credentialId, ownerUserId string) (ResolvedCredential, error) {
	return s.unsealCredential(ctx, credentialId, ownerUserId, true)
}

// peekCredential is the resolver for a PROBE (epic memql#4885, D11): the same
// read under the same rule, the same two refusals, and NO heartbeat. A probe
// is a question, and a question is not a use -- a lastUsedAt that moved on
// every keystroke in the Source stop would make "last used" mean "last
// looked at", which is the fact the Sources group is there to show.
func (s *store) peekCredential(ctx context.Context, credentialId, ownerUserId string) (ResolvedCredential, error) {
	return s.unsealCredential(ctx, credentialId, ownerUserId, false)
}

// unsealCredential is the ONE place in this package a credential is unsealed;
// the two resolvers above differ only in `touch`.
//
// The plaintext exists in exactly two locals: `token` below and whatever the
// caller binds the return value to. It is never wrapped into an error -- an
// error string reaches a log -- and secret.Decrypt's own errors name the key
// or the ciphertext, never the value, which is what makes THEM safe to wrap.
func (s *store) unsealCredential(ctx context.Context, credentialId, ownerUserId string, touch bool) (ResolvedCredential, error) {
	credentialId = strings.TrimSpace(credentialId)
	if credentialId == "" {
		return ResolvedCredential{}, refuse(CodeCredentialNotFound, "this package names no credential to fetch under")
	}

	// THE PACKAGE OWNER'S AUTHORITY, BORROWED -- component/campaigns' pattern
	// and openDeployment's: the owner value is read off a package row the
	// STARTING caller already resolved under their own actor through the
	// composite tier, so it can never name a user that caller could not act
	// as. An EMPTY owner is the cluster-owned package, and there is nobody to
	// borrow: the read runs under the caller's own actor, which means a
	// cluster owner deploying the deployment's own package fetches under a
	// credential they themselves hold -- and nobody else can read a
	// cluster-owned package at all.
	readCtx := ctx
	if owner := strings.TrimSpace(ownerUserId); owner != "" {
		readCtx = auth.ContextWithUserActor(ctx, owner)
	}

	row, err := s.sourceCredentialSealedById(readCtx, credentialId)
	if err != nil {
		return ResolvedCredential{}, err
	}
	if row == nil {
		// Zero rows, and the sentence must not claim to know which of the
		// two reasons produced them: the owner-scoped read cannot tell a
		// credential that does not exist from one that belongs to somebody
		// else, and that is the design -- a package naming another person's
		// credential is refused by name, exactly like one naming nothing.
		return ResolvedCredential{}, refuse(CodeCredentialNotFound,
			"this package fetches under credential %q, and the package's owner cannot read it: either it does not exist, or it belongs to somebody else -- a package naming another person's credential is refused by name. Add a credential of your own under Settings and switch this source to it, or clear the field if the repository is public.",
			credentialId)
	}
	if rowString(row, "status") == credentialStatusRevoked {
		return ResolvedCredential{}, refuse(CodeCredentialRevoked,
			"the credential this package fetches under (%q on %s) was revoked. Sources fetching under it refuse at their next fetch until you switch them to another credential on the Source stop.",
			credentialId, rowString(row, "host"))
	}

	// THE PLAINTEXT LIVES IN THIS LOCAL, and in the caller's, and nowhere
	// else. It is returned rather than applied here so the fetcher and the
	// poll set their own bearer -- one resolver, two requests -- and it is
	// discarded with the request.
	token, derr := secret.Decrypt(rowString(row, "encryptedValue"))
	if derr != nil {
		return ResolvedCredential{}, refuse(CodeSourceUnreadable,
			"credential %q could not be unsealed on this node: %v. Every node needs the same %s the credential was sealed under.",
			credentialId, derr, secret.EnvMasterKey)
	}
	if strings.TrimSpace(token) == "" {
		return ResolvedCredential{}, refuse(CodeCredentialNotFound,
			"credential %q unsealed to an empty value, so this package cannot fetch under it", credentialId)
	}

	resolved := ResolvedCredential{
		Id:            credentialId,
		Kind:          credentialKind(row),
		OwnerUserId:   rowString(row, "ownerUserId"),
		Bearer:        token,
		Login:         rowString(row, "login"),
		ExternalId:    rowString(row, "externalId"),
		Installations: rowStrings(row, "installationIds"),
	}

	// A GRANT's user token expires in eight hours, so the ordinary path
	// through here is a refresh nobody watches (C6). It runs under readCtx --
	// the OWNER's actor, the same authority the sealed read ran under -- so
	// the write lands on a row that actor may write, and a refresh can never
	// reach a row the read would not have returned.
	if resolved.IsGrant() {
		refreshed, rerr := s.refreshGrantIfExpired(readCtx, row, resolved.Bearer)
		if rerr != nil {
			return ResolvedCredential{}, rerr
		}
		resolved.Bearer = refreshed
	}

	// The heartbeat, BEST EFFORT, and only for a fetch. A fetch that cannot
	// record when it happened is still a fetch, and refusing a deploy over
	// bookkeeping would be the wrong trade -- so a failure is a warning that
	// names the credential and the error, never the value, and the fetch
	// proceeds. A probe skips it entirely: the unseal above is the whole of
	// what it needed, and it writes nothing.
	if touch {
		if terr := s.touchSourceCredential(readCtx, credentialId, time.Now().UTC()); terr != nil {
			s.log().Warn("packages: could not stamp lastUsedAt on a source credential",
				"component", "packages.credentials", "credential", credentialId, "err", terr)
		}
	}
	return resolved, nil
}

// credentialKind reads the row's kind, answering "token" for an ABSENT value.
//
// The default is the whole reason `kind` is not a required field: every
// credential stored before Connect existed carries no value, nothing rewrites
// a row to add one, and reading an absent kind as anything other than the
// pre-Connect behaviour would change what every one of those rows means on the
// day this shipped.
func credentialKind(row map[string]any) string {
	if k := rowString(row, "kind"); k != "" {
		return k
	}
	return credentialKindToken
}

// refreshGrantIfExpired renews a grant's user token when its stated expiry has
// passed, and answers the token to present.
//
// An ABSENT expiresAt is left alone rather than treated as expired. A GitHub
// App whose user tokens do not expire writes no value, and refreshing on every
// call would spend a refresh token -- which rotates on use -- for nothing. If
// such a token really is dead, GitHub answers 401 and the caller reads that as
// reconnect_required, which is the same repair by a slower route.
func (s *store) refreshGrantIfExpired(ctx context.Context, row map[string]any, current string) (string, error) {
	expiresAt := strings.TrimSpace(rowString(row, "expiresAt"))
	if expiresAt == "" {
		return current, nil
	}
	expiry, perr := time.Parse(time.RFC3339, expiresAt)
	if perr != nil {
		// An expiry this node cannot read is not a reason to refuse a fetch
		// and not a reason to spend a refresh token: the stored token is
		// presented, and GitHub is the authority on whether it still works.
		return current, nil
	}
	if time.Now().UTC().Add(refreshMargin).Before(expiry.UTC()) {
		return current, nil
	}

	credentialId := rowString(row, "id")
	if s.github == nil || !s.github.Configured() {
		// The grant is real and this node cannot renew it. Refusing by name
		// rather than presenting the expired token: an operator's missing
		// configuration must not reach a person as "reconnect your GitHub",
		// which is a repair that would not work.
		return "", refuse(CodeGithubAppNotConfigured,
			"credential %q is a GitHub App grant and this cluster has no GitHub App configured, so its token cannot be renewed. An operator sets %s.",
			credentialId, strings.Join(s.github.Missing(), ", "))
	}
	sealedRefresh := rowString(row, "refreshToken")
	refreshToken := ""
	if sealedRefresh != "" {
		plain, derr := secret.Decrypt(sealedRefresh)
		if derr != nil {
			return "", refuse(CodeSourceUnreadable,
				"credential %q's refresh token could not be unsealed on this node: %v. Every node needs the same %s the grant was sealed under.",
				credentialId, derr, secret.EnvMasterKey)
		}
		refreshToken = plain
	}

	set, rerr := s.github.RefreshUserToken(ctx, refreshToken)
	if rerr != nil {
		if errors.Is(rerr, githubapp.ErrNotConfigured) {
			return "", refuse(CodeGithubAppNotConfigured,
				"credential %q is a GitHub App grant and this cluster has no GitHub App configured. An operator sets %s.",
				credentialId, strings.Join(s.github.Missing(), ", "))
		}
		if errors.Is(rerr, githubapp.ErrReauthorize) {
			return "", refuse(CodeReconnectRequired,
				"GitHub no longer accepts this cluster's authorization for credential %q. Reconnect GitHub in Settings -- it is one click and nothing to type; sources fetching under it refuse until then.",
				credentialId)
		}
		// Anything else is GitHub being unreachable or unwell, which is not
		// the person's problem and must not be filed as one.
		return "", refuse(CodeSourceUnreadable,
			"this cluster could not renew credential %q's GitHub token: %v", credentialId, rerr)
	}

	// The rotation is written BEFORE the token is used, and both halves go in
	// one write: a refresh that stored the access token and dropped the
	// rotated refresh token would work for eight hours and then be
	// unrenewable, which reaches a person as a connection that breaks
	// overnight for no reason they can see.
	sealedValue, fingerprint, serr := secret.Encrypt(set.AccessToken)
	if serr != nil {
		return "", fmt.Errorf("packages: seal the renewed GitHub token: %w", serr)
	}
	nextRefresh := sealedRefresh
	if strings.TrimSpace(set.RefreshToken) != "" {
		sealedNext, _, nerr := secret.Encrypt(set.RefreshToken)
		if nerr != nil {
			return "", fmt.Errorf("packages: seal the rotated GitHub refresh token: %w", nerr)
		}
		nextRefresh = sealedNext
	}
	if werr := s.recordRefreshedGrantToken(ctx, grantTokenSeed{
		CredentialId:   credentialId,
		EncryptedValue: sealedValue,
		Fingerprint:    fingerprint,
		RefreshToken:   nextRefresh,
		ExpiresAt:      set.ExpiresAt,
	}); werr != nil {
		return "", werr
	}
	return set.AccessToken, nil
}

// ---------------------------------------------------------------------------
// The two capabilities
// ---------------------------------------------------------------------------

// handleSourceCredentialCreate seals a token and lands it as the CALLER's
// credential.
//
// Why a capability and not the browser calling createSourceCredential: a
// secret cannot be sealed in a browser. secret.Encrypt runs under
// MEMQL_MASTER_KEY, a key that exists on nodes and must never exist on a
// laptop, so the plaintext crosses the wire once, here, over the same
// TLS-terminated stream every other call uses, and is never sent back. The
// same argument integrations/email's configure capability makes for a vendor
// key, and the same rule about the value: trimmed, never inspected further,
// never logged.
func (i *Integration) handleSourceCredentialCreate(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	deps, err := i.resolve()
	if err != nil {
		return nil, err
	}

	// An empty actor is REFUSED rather than stamped with nothing. The
	// mutation stamps ownerUserId from actor.userId, and a row owned by
	// nobody is readable by nobody -- including the person who just typed
	// the token in, who would see a green save and a credential that resolves
	// for no package. Every wire call carries an actor; this is the
	// fail-closed answer for the paths that do not.
	actor := actorFromContext(ctx)
	if strings.TrimSpace(actor.UserId) == "" {
		return nil, fmt.Errorf("packages: a source credential belongs to the person who stores it, and this call carries no actor")
	}

	host, herr := normalizeCredentialHost(stringArg(args, "host"))
	if herr != nil {
		return nil, herr
	}
	label := strings.TrimSpace(stringArg(args, "label"))
	if label == "" {
		return nil, fmt.Errorf("packages: label is required -- it is how the credential is told apart in your Sources list")
	}

	// The VALUE is trimmed but never inspected further, and never logged. A
	// token that failed because of a trailing newline is a real support
	// case; a token in a log line is a worse one.
	token := strings.TrimSpace(stringArg(args, "token"))
	if token == "" {
		return nil, fmt.Errorf("packages: token is required")
	}
	ciphertext, fingerprint, serr := secret.Encrypt(token)
	if serr != nil {
		// Never wrap the plaintext into an error: this string reaches a log.
		// secret.Encrypt's own errors name the key, never the value.
		return nil, fmt.Errorf("packages: seal the credential: %w", serr)
	}

	// Under the CALLER's ctx, deliberately. writeInternal stamps ORIGIN (the
	// mutation is @serverOnly) and leaves the actor alone, so `ownerUserId:
	// actor.userId` inside the mutation resolves to the person who asked --
	// which is the owner stamp the composite tier compares every later read
	// against.
	credentialId := newRowId(sourceCredentialConcept)
	if err := deps.Store.createSourceCredential(ctx, credentialSeed{
		CredentialId:   credentialId,
		Host:           host,
		Label:          label,
		EncryptedValue: ciphertext,
		Fingerprint:    fingerprint,
	}); err != nil {
		return nil, err
	}
	return resultNode(map[string]any{
		"credentialId": credentialId,
		"fingerprint":  fingerprint,
	}), nil
}

// normalizeCredentialHost admits the one host this cluster fetches from.
//
// Refused rather than stored for a host nothing here speaks to: a credential
// for gitlab.com would sit in the Sources list looking usable and be
// presented to nothing, because parseGitHubRepo refuses the URL before any
// credential is consulted. The two spellings GitHub answers on collapse to
// the one the probe and the fetcher match against.
func normalizeCredentialHost(raw string) (string, error) {
	host := strings.ToLower(strings.TrimSpace(raw))
	host = strings.TrimPrefix(host, "https://")
	host = strings.TrimSuffix(host, "/")
	switch host {
	case credentialHostGitHub, "www." + credentialHostGitHub:
		return credentialHostGitHub, nil
	}
	return "", refuse(CodeSourceHostUnsupported,
		"%q is not a host this cluster fetches sources from -- only github.com today, or upload a zip of the tree instead; the two source forms are interchangeable.",
		strings.TrimSpace(raw))
}

// handleSourceCredentialRevoke flips one of the caller's credentials to
// revoked. Under the caller's actor and NOT stamped: the mutation is an
// ordinary owned one, and the write guard -- not this handler -- is what
// decides that the caller owns the row (or is a cluster owner).
//
// FOR A GRANT IT ALSO DISCONNECTS AT GITHUB (epic memql#4912, A.6), and the
// ORDER is the point: GitHub first, the row second, and a GitHub-side failure
// does not stop the row. The person asked to disconnect, and the local row is
// what actually stops every fetch, poll and probe on this cluster -- so
// refusing the disconnect because GitHub was unreachable would leave the
// cluster still fetching under an authorization the person believes they
// ended. The reply says which halves happened.
func (i *Integration) handleSourceCredentialRevoke(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	deps, err := i.resolve()
	if err != nil {
		return nil, err
	}
	credentialId := strings.TrimSpace(stringArg(args, "credentialId"))
	if credentialId == "" {
		return nil, fmt.Errorf("packages: credentialId is required")
	}

	remote := deps.revokeAtGitHub(ctx, credentialId)
	if err := deps.Store.revokeSourceCredential(ctx, credentialId); err != nil {
		return nil, err
	}
	return resultNode(map[string]any{
		"credentialId": credentialId,
		"status":       credentialStatusRevoked,
		// remoteRevoked is FALSE for every pasted token, which is correct
		// rather than a failure: there is nothing at GitHub to revoke for a
		// value somebody typed in, and the person revokes it at GitHub
		// themselves if they want to.
		"remoteRevoked": remote,
	}), nil
}

// revokeAtGitHub ends the authorization at GitHub for a grant, and answers
// whether it did.
//
// EVERY FAILURE IS A WARNING AND A FALSE, never an error. It reads the sealed
// row under the CALLER's actor, which means a cluster owner revoking somebody
// else's credential reads zero rows and skips the remote half entirely -- the
// honest outcome, since the token they would revoke is not theirs to hold and
// the local revoke is the part that matters. A token credential returns false
// with nothing attempted.
func (d *Deps) revokeAtGitHub(ctx context.Context, credentialId string) bool {
	if d.PeekCredentials == nil || d.GitHubApp == nil || !d.GitHubApp.Configured() {
		return false
	}
	grant, err := d.PeekCredentials(ctx, credentialId, actorFromContext(ctx).UserId)
	if err != nil || !grant.IsGrant() {
		return false
	}
	if rerr := d.GitHubApp.RevokeGrant(ctx, grant.Bearer); rerr != nil {
		d.log().Warn("packages: could not end a GitHub App authorization at GitHub; the local credential is revoked regardless",
			"component", "packages.credentials", "credential", credentialId, "err", rerr)
		return false
	}
	return true
}
