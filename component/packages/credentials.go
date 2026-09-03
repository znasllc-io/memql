package packages

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
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
//   - DECRYPTION HAPPENS ONLY INSIDE A FETCH. No query returns plaintext, no
//     capability returns plaintext, and the one query returning ciphertext is
//     @serverOnly.

// CredentialResolver unseals a package's credential at the moment of a fetch.
//
// The whole D10 shape is in the signature: the caller holds a credential NAME
// and the package's OWNER, asks for the value at fetch time, and never stores
// what comes back. Resolution runs under the OWNER's actor rather than the
// caller's, so a cluster owner deploying somebody's package fetches under that
// package's own credential (correct: they are deploying that package), and a
// package naming another person's credential resolves zero rows and is
// refused by name.
type CredentialResolver func(ctx context.Context, credentialId, ownerUserId string) (string, error)

// The credential's declared host and statuses, mirroring the enum on
// v1:platform:sourceCredential.
const (
	credentialHostGitHub    = "github.com"
	credentialStatusActive  = "active"
	credentialStatusRevoked = "revoked"
)

// sourceCredentialConcept is the row kind the create handler mints ids for.
const sourceCredentialConcept = "v1:platform:sourceCredential"

// resolveCredential is the production CredentialResolver, and the ONE place in
// this package a credential is unsealed.
//
// The plaintext exists in exactly two locals: `token` below and whatever the
// caller binds the return value to. It is never wrapped into an error -- an
// error string reaches a log -- and secret.Decrypt's own errors name the key
// or the ciphertext, never the value, which is what makes THEM safe to wrap.
func (s *store) resolveCredential(ctx context.Context, credentialId, ownerUserId string) (string, error) {
	credentialId = strings.TrimSpace(credentialId)
	if credentialId == "" {
		return "", refuse(CodeCredentialNotFound, "this package names no credential to fetch under")
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
		return "", err
	}
	if row == nil {
		// Zero rows, and the sentence must not claim to know which of the
		// two reasons produced them: the owner-scoped read cannot tell a
		// credential that does not exist from one that belongs to somebody
		// else, and that is the design -- a package naming another person's
		// credential is refused by name, exactly like one naming nothing.
		return "", refuse(CodeCredentialNotFound,
			"this package fetches under credential %q, and the package's owner cannot read it: either it does not exist, or it belongs to somebody else -- a package naming another person's credential is refused by name. Add a credential of your own under Settings and switch this source to it, or clear the field if the repository is public.",
			credentialId)
	}
	if rowString(row, "status") == credentialStatusRevoked {
		return "", refuse(CodeCredentialRevoked,
			"the credential this package fetches under (%q on %s) was revoked. Sources fetching under it refuse at their next fetch until you switch them to another credential on the Source stop.",
			credentialId, rowString(row, "host"))
	}

	// THE PLAINTEXT LIVES IN THIS LOCAL, and in the caller's, and nowhere
	// else. It is returned rather than applied here so the fetcher and the
	// poll set their own bearer -- one resolver, two requests -- and it is
	// discarded with the request.
	token, derr := secret.Decrypt(rowString(row, "encryptedValue"))
	if derr != nil {
		return "", refuse(CodeSourceUnreadable,
			"credential %q could not be unsealed on this node: %v. Every node needs the same %s the credential was sealed under.",
			credentialId, derr, secret.EnvMasterKey)
	}
	if strings.TrimSpace(token) == "" {
		return "", refuse(CodeCredentialNotFound,
			"credential %q unsealed to an empty value, so this package cannot fetch under it", credentialId)
	}

	// The heartbeat, BEST EFFORT. A fetch that cannot record when it happened
	// is still a fetch, and refusing a deploy over bookkeeping would be the
	// wrong trade -- so a failure is a warning that names the credential and
	// the error, never the value, and the fetch proceeds.
	if terr := s.touchSourceCredential(readCtx, credentialId, time.Now().UTC()); terr != nil {
		s.log().Warn("packages: could not stamp lastUsedAt on a source credential",
			"component", "packages.credentials", "credential", credentialId, "err", terr)
	}
	return token, nil
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
func (i *Integration) handleSourceCredentialRevoke(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	deps, err := i.resolve()
	if err != nil {
		return nil, err
	}
	credentialId := strings.TrimSpace(stringArg(args, "credentialId"))
	if credentialId == "" {
		return nil, fmt.Errorf("packages: credentialId is required")
	}
	if err := deps.Store.revokeSourceCredential(ctx, credentialId); err != nil {
		return nil, err
	}
	return resultNode(map[string]any{
		"credentialId": credentialId,
		"status":       credentialStatusRevoked,
	}), nil
}
