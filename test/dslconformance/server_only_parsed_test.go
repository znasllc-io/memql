package dslconformance

import (
	"fmt"
	"github.com/znasllc-io/memql/dsl"
	"maps"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"sync"
	"testing"

	languageAst "github.com/znasllc-io/memql/component/language/ast"
	languageCompiler "github.com/znasllc-io/memql/component/language/compiler"
	"github.com/znasllc-io/memql/component/memql/dslimports"
)

// server_only_parsed_test.go -- memql#2875.
//
// The audit gates used to decide `@serverOnly` by REGEX over source, while the
// LOADER decides by PARSING and sets Function.ServerOnly. Those two can
// disagree, and the disagreement is FAIL-OPEN in the audit: a line beginning
// `@serverOnly` inside a multi-line annotation string, or inside a block
// comment opened on an `@`-line, satisfies the regex -- so the audit EXEMPTS
// the construct from per-row-authz classification and sdk/gen drops it from the
// client surface, while Function.ServerOnly stays false and NOTHING is enforced
// at runtime. #2861 (block comments in the DSL) made that more reachable.
//
// It is not attacker-reachable -- it requires authoring the construct in-repo.
// The reason to close it is that the classification test is the backstop that
// hard-fails on any new unclassified construct, and a construct that can slip
// out of that gate while ALSO having no runtime enforcement is the one shape
// the gate exists to make impossible.
//
// # This reads the PARSED tree, which is what the issue asked for
//
// An earlier attempt argued the audit could not read a parsed verdict from
// package `dsl` because `component/memql` imports `dsl`, so `dsl` cannot import
// back to load a function registry. The registry half of that is true and the
// conclusion was wrong: `component/memql/dslimports` is importable from here
// and ALREADY IS (test/dslconformance/filter_field_coverage_test.go, test/dslconformance/embed_test.go), and
// `dslimports.Load` hands back `Files map[path]*ast.File` -- the parsed AST for
// every file in the tree, in ~0.2s.
//
// The loader's rule is `hasFlagAttribute(def.Attributes, "serverOnly")`
// (component/memql/function_loader.go). serverOnlyConstructs applies exactly
// that rule to exactly the same parse, so "the gate exempted it" and "the
// runtime enforces it" cannot diverge by construction rather than merely
// failing a comparison. That is the difference between this and a parity test.

// serverOnlyKey identifies a construct for audit purposes. Name alone is not
// enough: two domains may declare the same construct name.
type serverOnlyKey struct {
	Path string
	Name string
}

var (
	serverOnlyOnce sync.Once
	serverOnlySet  map[serverOnlyKey]bool
	serverOnlyErr  error
	serverOnlySeen int
)

// serverOnlyConstructs returns the set of constructs carrying a REAL
// `@serverOnly` annotation, derived from the parsed AST rather than from source
// text.
//
// Cached: the parse is ~0.2s and several gates consult it.
func serverOnlyConstructs(t *testing.T) map[serverOnlyKey]bool {
	t.Helper()
	serverOnlyOnce.Do(func() {
		tree, err := dslimports.Load(dsl.Tree())
		if err != nil {
			// A tree that does not load is a different failure than a
			// divergence, and callers should say so rather than reporting an
			// empty @serverOnly set as "nothing is annotated".
			serverOnlyErr = err
			return
		}
		out := map[serverOnlyKey]bool{}
		seen := 0
		for path, file := range tree.Files {
			if file == nil {
				continue
			}
			for _, def := range file.Definitions {
				name, attrs, ok := declNameAndAttributes(def)
				if !ok {
					continue
				}
				seen++
				for _, a := range attrs {
					// Same rule as the loader's hasFlagAttribute: presence of
					// the attribute NAME. An `@serverOnly` inside a string
					// literal or a comment is not an attribute, so it never
					// appears here -- which is the whole point.
					if a != nil && a.Name == "serverOnly" {
						out[serverOnlyKey{Path: path, Name: name}] = true
					}
				}
			}
		}
		serverOnlySet = out
		serverOnlySeen = seen
	})
	if serverOnlyErr != nil {
		t.Fatalf("dslimports.Load: %v -- the tree did not parse, so the @serverOnly set cannot "+
			"be derived. Fix the parse failure; do not read this as 'no construct is annotated'.",
			serverOnlyErr)
	}
	if serverOnlySeen == 0 {
		t.Fatal("walked 0 named declarations -- every gate keyed on this set would then exempt " +
			"nothing and pass vacuously, which is the failure mode that made the previous " +
			"user-scope detector report a meaningless zero (#2799). Check declNameAndAttributes " +
			"against the AST definition types.")
	}
	// A CLONE: four gates now hold this, and one of them writing to it would
	// corrupt the others' verdicts. Cheap -- eleven entries.
	return maps.Clone(serverOnlySet)
}

// declNameAndAttributes pulls the name + annotations off any named top-level
// declaration.
//
// Every Attributes-bearing kind must be listed. The `default: return false` is
// a SILENT DROP -- Go's type switch has no exhaustiveness check -- so
// TestServerOnlyParsedSetCoversEveryAttributedDeclKind walks the real tree and
// fails on any node type carrying an `Attributes` field that this switch does
// not handle. Without it, the first five kinds were missing (ShapeDecl 87,
// SpecDecl 36, PromptDecl 25, ActionDecl 9, CapabilityDecl 8 = 165
// declarations), which is the exact class of hole this file exists to close.
//
// SeedDecl / ToolDecl / ProviderDecl / PolicyDecl are deliberately absent: they
// carry no Attributes field, so there is nothing to read.
func declNameAndAttributes(def languageAst.Node) (string, []*languageAst.Attribute, bool) {
	switch d := def.(type) {
	case *languageAst.FunctionDef:
		return d.Name, d.Attributes, true
	case *languageAst.ConceptDecl:
		return d.Name, d.Attributes, true
	case *languageAst.BuiltinDecl:
		return d.Name, d.Attributes, true
	case *languageAst.ShapeDecl:
		return d.Name, d.Attributes, true
	case *languageAst.SpecDecl:
		return d.Name, d.Attributes, true
	case *languageAst.PromptDecl:
		return d.Name, d.Attributes, true
	case *languageAst.ActionDecl:
		return d.Name, d.Attributes, true
	case *languageAst.CapabilityDecl:
		return d.Name, d.Attributes, true
	default:
		return "", nil, false
	}
}

// TestServerOnlyParsedSetMatchesTheTree pins the derived set against the tree,
// so a change that empties or inflates it is visible.
func TestServerOnlyParsedSetMatchesTheTree(t *testing.T) {
	set := serverOnlyConstructs(t)
	if len(set) == 0 {
		t.Fatal("the parsed tree reports ZERO @serverOnly constructs. Either the annotation has " +
			"no live users -- in which case check whether the enforcement in " +
			"component/memql/function_validator.go is still exercised by anything -- or the " +
			"attribute name changed, which would silently exempt nothing and gate nothing.")
	}
	want := map[serverOnlyKey]bool{
		// epic memql#4800. The accounts seed's existence probe. It runs from
		// the seedSelfAccount automation at system.startup, under the engine's
		// own system actor, before any person has signed in -- so actor.userId
		// is empty for the only caller it will ever have, and the composite
		// tier its five siblings in that file carry would evaluate against
		// nobody. That is not merely a read returning nothing: the automation
		// gates the CREATE on this read being empty, so a filter that always
		// answered empty would re-create v1:accounts:account:self on every
		// boot, which is exactly the clobbering D3 forbids. Caller-scoping is
		// impossible because there is no caller; @serverOnly is what keeps an
		// ungated projection of the contact fields off the wire, and the app
		// reads the same row through clientAccountById.
		{Path: "accounts/queries.memql", Name: "existingSelfAccount"}: true,

		// epic memql#4966, the work spine's promotion path. Both of these
		// write the engine's OWN EVIDENCE about a template -- the catalog key
		// that decides whether a later goal is served without a model, and the
		// reliability that decides how far a template has earned being replayed.
		// Caller-scoping is not the missing piece: the rows are already
		// owner-stamped and a caller can only reach their own constructs. What
		// is missing is that a caller may not make these ASSERTIONS at all. A
		// browser able to write goalSignature could point any goal at any
		// template of theirs and have compile replay it as an exact match; one
		// able to write reliability could promote an unproven template into the
		// path that skips verification. Both are the engine's judgement about
		// what has actually succeeded, and a tier cannot say "these are yours
		// and still not yours to write".
		{Path: "authoring/mutations.memql", Name: "recordConstructGoalSignature"}: true,

		// epic memql#4966. workRunsInFlight is the wait-and-abandon sweep's
		// read. (A2 briefly carried a closeWorkGoal beside it; epic A3's
		// updateWorkGoal is the same write with a wider argument list, so
		// there is one writer now and it carries A3's own entry.)
		//
		// Caller-scoping this read would not
		// merely be wrong, it would be SILENT. The sweep must see every
		// owner's parked runs: a person whose run is stranded on a replica
		// that has gone cannot resume it themselves, which is the entire
		// reason the sweep exists. Under an owner conjunct the automation's
		// actor matches nothing, the read answers ZERO ROWS AND NO ERROR, and
		// a sweep that resumes nothing is indistinguishable from a cluster
		// with nothing parked.
		{Path: "work/queries.memql", Name: "workRunsInFlight"}:                  true,
		{Path: "authoring/mutations.memql", Name: "recordConstructReliability"}: true,

		// epic memql#4819 / memql#4820 D15. The six campaign-lifecycle and
		// progress writers. Every one of them is reached ONLY through the
		// `campaign*` builtins, which do the authorization first -- an owned-tier
		// read of the campaign under the CALLER's own actor -- and then run the
		// preflight the send depends on. The header of dsl/campaigns/mutations.memql
		// has claimed since memql#3348 that they are "only ever reached through the
		// builtins", and until now NOTHING enforced it: a browser holding a
		// campaign id could flip its status to "sending" with no send job behind
		// it, or stamp counters that never happened, desyncing the row from the
		// engine. Caller-scoping is not the missing piece (they are already
		// owner-stamped and a caller can only reach their own rows); what is
		// missing is that a caller may not perform these transitions AT ALL,
		// which is what @serverOnly says and a tier cannot.
		{Path: "campaigns/mutations.memql", Name: "startCampaign"}:          true,
		{Path: "campaigns/mutations.memql", Name: "pauseCampaign"}:          true,
		{Path: "campaigns/mutations.memql", Name: "resumeCampaign"}:         true,
		{Path: "campaigns/mutations.memql", Name: "scheduleCampaign"}:       true,
		{Path: "campaigns/mutations.memql", Name: "updateCampaignProgress"}: true,
		{Path: "campaigns/mutations.memql", Name: "recordCampaignDelivery"}: true,
		// memql#4823. The open/click writer. Its HTTP callers are an
		// unauthenticated mail client fetching an image and an unauthenticated
		// browser following a redirect, so there is no actor to scope by and the
		// only authorization is the HMAC-signed token the handler verifies before
		// it derives a single argument. @serverOnly is the second wall: even
		// holding a valid token, nothing client-reachable can write this row.
		{Path: "campaigns/mutations.memql", Name: "recordEngagementEvent"}: true,
		// memql#4829. The engine's own account of what it made of an event-email
		// rule -- which bundle and construct it generated, whether activation
		// succeeded, and how many times the rule has fired. The value of these
		// fields is precisely that they are the ENGINE's observation rather than a
		// claim: a caller who could set status:"active" would be asserting a live
		// automation that does not exist, and one who could set firedCount would
		// be editing the evidence. The rule's FORM stays client-writable through
		// createEmailRule / updateEmailRule / setEmailRuleStatus.
		{Path: "campaigns/mutations.memql", Name: "recordEmailRuleGeneration"}: true,
		{Path: "campaigns/mutations.memql", Name: "recordEmailRuleFiring"}:     true,
		{Path: "identity/queries.memql", Name: "activeUsers"}:                  true,
		{Path: "identity/queries.memql", Name: "userByEmail"}:                  true,
		{Path: "identity/queries.memql", Name: "userByIdSystem"}:               true,
		// memql#3217. The complete-set sibling of activeUsers, behind the
		// startup per-user seed sweep. @serverOnly for activeUsers' reason
		// minus most of the exposure -- its projection is userIdRef (row.id
		// alone, no @pii field), but the read still enumerates every user in
		// the cluster and cannot be caller-scoped: it runs under the seed
		// materializer's system actor at boot, where there is no requesting
		// user for actor.userId to name.
		// memql#4301/#4302. The device-bound magic-link flow's by-id read and
		// its approval write. Both run BEFORE the caller is authenticated --
		// they are steps of signing in, not things a signed-in person does --
		// so actor.userId is empty for every legitimate caller and a
		// self-scoped filter would match zero rows. The row carries no user
		// pointer to scope by either: it names an email address, and on a
		// first-time registration no user row exists until the link is
		// consumed. What authorizes them is the memql_ml binding cookie,
		// compared against the row's bindingHash in Go before either runs.
		{Path: "identity/queries.memql", Name: "magicLinkRequestById"}:      true,
		{Path: "identity/mutations.memql", Name: "approveMagicLinkRequest"}: true,
		// memql#4304. The two magic-link hardening fields on v1:identity:user.
		// Both have an ADMIN caller acting on somebody else's row -- the
		// sign-in-policy RESET is the rescue path for a person who turned
		// links off and lost their passkey -- which actor.userId scoping
		// would refuse outright. The self-service caller is checked against
		// actor.userId in Go, alongside a precondition no row filter can
		// express: "holds at least one active passkey" is a fact on
		// v1:identity:identity, not on the row being written.
		{Path: "identity/mutations.memql", Name: "setUserSignInPolicy"}:     true,
		{Path: "identity/mutations.memql", Name: "setUserSharedMailbox"}:    true,
		{Path: "identity/queries.memql", Name: "usersForSeedSweep"}:         true,
		{Path: "identity/queries.memql", Name: "usersInDeletionCooldown"}:   true,
		{Path: "identity/queries.memql", Name: "usersScheduledForDeletion"}: true,
		// epic memql#4912 / memql#4913. GitHub Connect's server-held state:
		// one read and two writes over v1:identity:githubConnectState.
		//
		// Caller scoping has nobody to scope to at the moment that matters.
		// The read runs inside the callback GitHub redirects a BROWSER to,
		// carrying an OAuth code, a state value and no MemQL bearer of any
		// kind -- the person is mid-way through granting this cluster
		// authority at a different service, so actor.userId is empty and a
		// self-scoped filter would match zero rows and turn every completed
		// connect into connect_state_invalid.
		//
		// The two writes carry a `stateHash`, which IS the credential the
		// callback matches on: a client-reachable create would be a primitive
		// for minting a connect that lands a GitHub grant on an account of the
		// caller's choosing, and a client-reachable consume would let anyone
		// burn somebody else's in-flight connect. What bounds them instead is
		// a SHA-256 digest of 32 CSPRNG bytes, a ten-minute TTL, and a
		// Postgres advisory lock that spends the row exactly once.
		{Path: "identity/queries.memql", Name: "githubConnectStateByHash"}:    true,
		{Path: "identity/mutations.memql", Name: "createGithubConnectState"}:  true,
		{Path: "identity/mutations.memql", Name: "consumeGithubConnectState"}: true,
		{Path: "worker/queries.memql", Name: "runningPlansForUser"}:           true,
		// epic memql#4378, the SYNC RUNTIME's own bookkeeping. Eight
		// writers over two engine-owned concepts -- an outbox queue and a
		// health timeline -- and the argument is one argument, not eight.
		//
		// Caller scoping has nobody to scope to. These rows belong to the
		// DEPLOYMENT: an outbox entry records that a change has to reach
		// an external system, and a syncState row records how far behind
		// a mirror is. Neither has an owner field, neither is any user's,
		// and the only writer is the runtime itself, acting for no user
		// under the engine's operator identity. actor.userId is empty for
		// every legitimate caller and a self-scoped filter would match
		// nothing.
		//
		// What @serverOnly adds over the concepts' clusterOwner tier is
		// the WIRE: without it, eight typed methods for driving the
		// engine's internal delivery queue are generated into both SDKs.
		// The operator surface reaches them through the Data origins
		// page's own capabilities, which carry the owner/admin gate.
		{Path: "platform/mutations.memql", Name: "markOutboxDelivering"}: true,
		{Path: "platform/mutations.memql", Name: "markOutboxDelivered"}:  true,
		{Path: "platform/mutations.memql", Name: "markOutboxFailed"}:     true,
		{Path: "platform/mutations.memql", Name: "markOutboxDead"}:       true,
		{Path: "platform/mutations.memql", Name: "retryOutboxEntry"}:     true,
		{Path: "platform/mutations.memql", Name: "discardOutboxEntry"}:   true,
		{Path: "platform/mutations.memql", Name: "upsertSyncState"}:      true,
		{Path: "platform/mutations.memql", Name: "setSyncPaused"}:        true,
		// epic memql#4805. The custom-domain create, plus the six writes its
		// reconciliation sweep makes on an operator's behalf.
		//
		// createCustomDomain is server-only because of the TOKEN. The value it
		// writes is what a client publishes at `TXT _memql-verify.<hostname>`
		// to prove control of the name, so a caller who CHOOSES it proves
		// nothing -- one constant published under a thousand domains would
		// verify all of them. actor.userId scoping would assert the row belongs
		// to the caller, which is not the property in question and is not even
		// expressible: the concept is clusterOwner-tier and carries no owner
		// field. What must hold is that the token was minted rather than
		// supplied, and only the Go frame that minted it knows that. The
		// client-reachable surface is the `customDomainAdd` builtin -- the same
		// split sitePublishFromArtifact already uses.
		//
		// The six sweep writers are the outbox case above, exactly: the caller
		// is the engine's own reconciliation loop under its operator identity,
		// so actor.userId is empty for every legitimate caller and a
		// self-scoped filter matches nothing. What @serverOnly adds over the
		// tier is the WIRE -- these are the engine's account of what it saw in
		// DNS and at the API server, and a client-callable version is a way to
		// tell the Domains panel a domain is `live` without anything having
		// checked.
		//
		// removeCustomDomain is deliberately NOT here. It writes no credential
		// and decides nothing the tier has not already decided: an operator
		// asks for a binding to come down, and the sweep does the rest.
		{Path: "platform/mutations.memql", Name: "createCustomDomain"}:                true,
		{Path: "platform/mutations.memql", Name: "recordCustomDomainCheck"}:           true,
		{Path: "platform/mutations.memql", Name: "markCustomDomainVerified"}:          true,
		{Path: "platform/mutations.memql", Name: "recordCustomDomainIssuanceFailure"}: true,
		{Path: "platform/mutations.memql", Name: "recordCustomDomainIssuingProgress"}: true,
		{Path: "platform/mutations.memql", Name: "markCustomDomainLive"}:              true,
		{Path: "platform/mutations.memql", Name: "markCustomDomainRemoved"}:           true,
		// epic memql#4794. The packages pipeline's writers, the D11 feeds'
		// single write, and the two status setters behind the D10 archive
		// capabilities. They divide into two arguments and neither is
		// "caller-scoping was inconvenient".
		//
		// THE PIPELINE WRITERS -- openPackageDeployment, advance..., record...
		// Report, close..., recordPackageDeployedVersion, recordPackageName,
		// recordSitePackageOrigin -- run where THERE IS NO CALLER. A deploy
		// advances by stage handoffs across nodes, so actor.userId is empty on
		// the very path that has to write, and a caller-scoped form would open
		// a timeline nothing could advance. What keeps those rows visible to
		// the right people is not scoping but ownerUserId, copied off a
		// package row the STARTING caller had already read under their own
		// actor -- so it can never name a user that caller could not act as.
		// Separately, the thing worth preventing here is not cross-user reach
		// at all: it is a caller asserting "this deploy succeeded", or writing
		// a clean analysis report, about a package they genuinely own.
		// Caller-scoping bounds whose rows are touched, not what may be
		// claimed about them.
		//
		// THE TWO STATUS SETTERS -- setPackageStatus, setSiteStatus -- are the
		// clearest case, because the caller DOES own the row. Scoping them
		// would admit precisely the call D10 refuses: archiving without the
		// typed-name confirmation, and without the check that no deployable is
		// still serving. Both are conditions on the row's state rather than on
		// who is asking, so the capability that can evaluate them
		// (integration.packages) is the only caller, and this annotation is
		// what makes it the only one.
		//
		// recordPackageUpstreamVersion is the D11 feeds' whole write surface.
		// Its two legitimate callers are a scheduled automation with nobody
		// attached and the inbound webhook receiver, where the "caller" is
		// GitHub -- scoping refuses both and still permits the claim worth
		// refusing.
		{Path: "platform/mutations.memql", Name: "setPackageStatus"}:             true,
		{Path: "platform/mutations.memql", Name: "setSiteStatus"}:                true,
		{Path: "platform/mutations.memql", Name: "recordPackageDeployedVersion"}: true,
		{Path: "platform/mutations.memql", Name: "recordPackageName"}:            true,
		{Path: "platform/mutations.memql", Name: "recordPackageUpstreamVersion"}: true,
		// recordPackageDeployables is the PIPELINE's reading of the tree, and
		// it is a pipeline writer for the reason the ones above are: it runs
		// during a deploy, where actor.userId is empty on the path that has to
		// write. What it records is what the manifest DECLARES, and the OS
		// offers that list as "apps you can still deploy" -- so a
		// client-reachable form would let somebody invent deployables no
		// manifest contains and have the console offer to deploy them.
		// Caller-scoping does not address that either: the claim worth
		// refusing is about the CONTENTS of a package the caller already owns.
		{Path: "platform/mutations.memql", Name: "recordPackageDeployables"}:      true,
		{Path: "platform/mutations.memql", Name: "openPackageDeployment"}:         true,
		{Path: "platform/mutations.memql", Name: "advancePackageDeployment"}:      true,
		{Path: "platform/mutations.memql", Name: "recordPackageDeploymentReport"}: true,
		{Path: "platform/mutations.memql", Name: "recordPackageDeploymentScope"}:  true,
		{Path: "platform/mutations.memql", Name: "closePackageDeployment"}:        true,
		// epic memql#4900. Two more writes about a RUN rather than about a
		// person, and neither is caller-scopable for the reason the four
		// above are not: the value is a claim about what a node did.
		// heartbeatPackageDeployment says a node is still alive, which only
		// that node can know and which the abandoned sweep reads as evidence;
		// abandonPackageDeployment is the sweep's own close, running on a
		// schedule with nobody attached and across every owner, so a
		// self-scoped filter would refuse every row it exists to close.
		{Path: "platform/mutations.memql", Name: "heartbeatPackageDeployment"}: true,
		{Path: "platform/mutations.memql", Name: "abandonPackageDeployment"}:   true,
		// epic memql#4937. requestPackageDeploymentCancel FLAGS a run and ends
		// nothing -- the node running it closes the row at its next stage
		// boundary. Caller-scoping is not the fix, and for a sharper reason
		// than the two above: the capability packageCancelDeployment resolves
		// the deployment through the OWNER-SCOPED read first, so a caller who
		// cannot see the run is already refused by name before this is
		// reached. What @serverOnly adds over that is the WIRE -- the two
		// refusals a person can actually hit (the run is terminal, or it has
		// started rolling) are decisions about OTHER fields that a mutation
		// body cannot make, so a client-reachable form would let somebody flag
		// a run whose stage nothing had checked.
		{Path: "platform/mutations.memql", Name: "requestPackageDeploymentCancel"}: true,
		{Path: "platform/mutations.memql", Name: "recordSitePackageOrigin"}:        true,
		// epic memql#4885 (D10). Personal source credentials: the two engine
		// writes and the ONE read that returns ciphertext. None is a case of
		// "caller-scoping was inconvenient" -- all three are already
		// owner-scoped, and a scoped form would admit exactly the calls being
		// refused.
		//
		// createSourceCredential writes a SEALED value. The row is genuinely
		// the caller's (ownerUserId is stamped from actor.userId), so
		// caller-scoping bounds nothing here; what has to be withheld is the
		// ability to supply `encryptedValue` and `fingerprint` at all. Only
		// the Go frame that ran secret.Encrypt under MEMQL_MASTER_KEY can vouch
		// for that ciphertext -- a browser-composed one would read as a
		// configured credential and unseal to nothing, or to whatever the
		// browser chose -- so the sourceCredentialCreate capability is the one
		// way in, and it reaches this mutation under stamped internal origin
		// WITH the caller's own actor.
		//
		// touchSourceCredential is the ENGINE's own account of a fetch:
		// `lastUsedAt` says the fetcher or the poll unsealed the credential,
		// and a person stamping it on their own row would be equally wrong
		// and equally unscoped away. The poll also runs on a schedule with
		// nobody attached, where a self-scoped filter would refuse the only
		// legitimate caller.
		//
		// sourceCredentialSealedById is the read that RETURNS CIPHERTEXT. It
		// is owner-scoped already, and deliberately carries no cluster-owner
		// branch; what @serverOnly withholds is the wire -- a client-callable
		// projection of `encryptedValue` is a ciphertext oracle even for the
		// row's own owner, and nothing a person does needs it: decryption
		// happens only inside a fetch, under the package owner's actor.
		{Path: "platform/mutations.memql", Name: "createSourceCredential"}:   true,
		{Path: "platform/mutations.memql", Name: "touchSourceCredential"}:    true,
		{Path: "platform/queries.memql", Name: "sourceCredentialSealedById"}: true,
		// epic memql#4912. The GitHub App grant's six writes and reads, and
		// NOT ONE of them is caller-scoping avoided -- every filter here is
		// already owner-scoped, and createGithubAppGrant stamps the owner from
		// actor.userId. What @serverOnly withholds is the WIRE, for two
		// distinct reasons.
		//
		// The four writes all land SEALED CIPHERTEXT. Only the Go frame that
		// ran secret.Encrypt under MEMQL_MASTER_KEY can vouch for an
		// `encryptedValue` or a `refreshToken`, and that key exists on nodes
		// and must never exist on a laptop; a client-supplied one would be a
		// row that reads as connected and unseals to whatever the client
		// chose. refreshGithubAppGrantToken and recordGithubAppInstallations
		// add the touchSourceCredential argument on top: both are claims about
		// what the ENGINE observed, and the poll that makes them runs on a
		// schedule with nobody attached, so a self-scoped filter would refuse
		// the one path that legitimately calls them.
		//
		// The two reads carry sourceCredentialSealed, which projects both
		// sealed fields -- so they are the ciphertext oracle
		// sourceCredentialSealedById already argues against, and nothing a
		// person does needs them: a grant is unsealed only inside a fetch, a
		// poll, a probe or the connect callback, into a local that dies with
		// the call.
		{Path: "platform/mutations.memql", Name: "createGithubAppGrant"}:         true,
		{Path: "platform/mutations.memql", Name: "updateGithubAppGrant"}:         true,
		{Path: "platform/mutations.memql", Name: "refreshGithubAppGrantToken"}:   true,
		{Path: "platform/mutations.memql", Name: "recordGithubAppInstallations"}: true,
		{Path: "platform/queries.memql", Name: "githubAppGrantByExternalId"}:     true,
		{Path: "platform/queries.memql", Name: "githubAppGrantForCaller"}:        true,
		// memql#4270 / memql#4606 / memql#4601. The four writes of the
		// user-invitation lifecycle, and none is caller-scopable for the same
		// underlying reason: the row is not the caller's.
		//
		// createUserInvitation mints a credential for somebody who has no
		// account yet -- there is no ownership relation to filter on, and
		// actor.userId scoping would say "you may invite yourself". The real
		// decision is a ROLE question plus the cluster's REGISTRATION POLICY
		// (domain_restricted must match the allowlist; under open the link is
		// a courtesy), and the policy is not on the row, so it cannot be a
		// filter over it.
		//
		// revokeUserInvitation COULD be filtered on inviterId -- the field is
		// right there -- but that is the wrong policy: it would mean only the
		// original inviter may revoke, and the case revocation exists for is
		// an admin sending a link to the wrong address and an owner killing
		// it. Both take the owner/admin gate in adminops instead, one audited
		// event per call, refusals included.
		//
		// markUserInvitationAccepted is the third, and its argument is the
		// REDEMPTION side's rather than the admin side's. It runs before the
		// caller is authenticated -- it is a step of creating the account, not
		// something a signed-in person does -- so actor.userId is empty for
		// every legitimate caller and a self-scoped filter would match zero
		// rows for the only caller that ever runs it. The row's two user
		// pointers do not help either: inviterId names the wrong person, and
		// inviteeId is the field this write SETS, so a filter over it would
		// test the value being written. What authorizes it is possession of
		// the token, matched against tokenHash on the resolve path before it
		// runs. On the wire it would be a burn primitive against every
		// outstanding invitation on the cluster, because burning one is
		// exactly what it does -- that is the whole point of the write.
		// bindUserInvitation is the fourth, and on the wire it is the sharpest
		// of them: it writes a caller-supplied DIGEST that decides which
		// browser may accept. That is createGuestInvitation's
		// credential-forging shape rather than a new one, and no actor filter
		// reaches it -- the caller who must be refused is an ordinary
		// authenticated user, who satisfies any self-scoped test while binding
		// an invitation they merely know the id of to a cookie they hold. The
		// honest caller cannot be scoped either, for the reason above it: the
		// toucher of a /join link has not authenticated yet. The no-overwrite
		// rule that makes the binding worth anything is not expressible in a
		// mutation body at all -- no predicate, no filter, and update() is a
		// read-merge-write -- so it lives in Store.BindUserInvitation, inside
		// the advisory-lock critical section magic_link_gate.go provides.
		//
		// recordUserInvitationDelivery (memql#4587) is the fifth, and its
		// argument is a different shape from the four above: ownership is not
		// what is at stake. Caller-scoping restricts WHOSE rows may be written;
		// it cannot restrict WHAT may be claimed about them, and the value here
		// is a claim about the SERVER'S OWN I/O -- whether a mail provider
		// accepted the message. An operator scoped to their own invitations
		// could still stamp `sent` on one nobody sent, which would make the
		// pending list lie in exactly the direction the field exists to stop:
		// memql#4583 stayed invisible for its whole life precisely because a
		// never-delivered invitation read as a delivered one. There is also no
		// caller to scope TO -- the identity node writes this under internal
		// origin, immediately after the send it just attempted.
		// memql#4611. createOidcIdentity writes the (issuer, subject) pair that
		// makes a federated sign-in land on an existing user row, and
		// caller-scoping is not the fix because ownership is not what is at
		// stake -- the danger is the CONTENT of the link, not whose row gains
		// it. A client that could write credentials.subject for itself could
		// claim any upstream identity it liked, and the next federated sign-in
		// as that subject would resolve to the row that claimed it: account
		// takeover through a WRITE rather than through a sign-in. The pair may
		// only be written by the code that VERIFIED an id token carrying it,
		// and at that moment there is no caller to scope to -- the person has
		// not been admitted yet.
		{Path: "identity/mutations.memql", Name: "createOidcIdentity"}:           true,
		{Path: "identity/mutations.memql", Name: "createUserInvitation"}:         true,
		{Path: "identity/mutations.memql", Name: "recordUserInvitationDelivery"}: true,
		{Path: "identity/mutations.memql", Name: "bindUserInvitation"}:           true,
		{Path: "identity/mutations.memql", Name: "markUserInvitationAccepted"}:   true,
		{Path: "identity/mutations.memql", Name: "revokeUserInvitation"}:         true,
		// memql#2991. Caller-scoping is not merely hard here, it is
		// INEXPRESSIBLE: `update { id: args.userId; args.payload }` relates the
		// target to nothing, and no mutation in the tree carries a filter --
		// the grammar has no way to say "and the row is mine". An owner
		// predicate on the update's row selection is memql#2803 Phase 5.
		//
		// Meanwhile the row holds the caller's own cluster-wide `role`, which
		// is a plain settable enum, and a payload SPLAT names no fields so
		// validateMutationCallerArgs cannot see it. The one legitimate caller
		// is the identity admin server, which already authorizes in
		// admin/auth.go -- so origin gating puts the boundary where the
		// authorization already lives.
		{Path: "identity/mutations.memql", Name: "updateUser"}: true,
		// memql#2987, the node-token trio. All three were @public while
		// projecting identityFull -- which includes `credentials` -- and
		// nodeTokenIdentities had NO filter at all, so one unparameterised call
		// returned every node credential in the cluster to any authenticated
		// caller. @public enforces nothing; it only suppressed the classifier.
		//
		// Caller-scoping here does not merely fail, it names nobody: a
		// node_token row belongs to a cluster NODE, and its `userId` is the
		// synthetic bootstrap user the mint ran as, never a reader's id. So
		// `userId==actor.userId` returns zero rows for every real caller -- the
		// human admin on /admin/tokens, and the bootstrap handler, which has no
		// resolved actor at all because it runs BEFORE the node has an identity.
		// Origin is the only boundary that exists.
		//
		// Every caller is component/identity/store.go, allowlisted in
		// call_origin_conformance_test.go as server-initiated, so the internal
		// stamp these now require is legitimate rather than the request-derived
		// stamp refuted on memql#2989.
		{Path: "identity/queries.memql", Name: "nodeTokenIdentityByBinding"}: true,
		{Path: "identity/queries.memql", Name: "nodeTokenIdentities"}:        true,
		{Path: "identity/queries.memql", Name: "nodeTokenIdentityById"}:      true,
		// memql#4111, same argument as the node-token trio above and for the
		// same structural reason: this is read by an AUTH INTERCEPTOR, before
		// any actor exists. The caller is the voice-agent revocation gate
		// (component/grpc/voice_agent_revocation.go, via
		// component/identity/store.go's LookupVoiceAgentTokenIdentityById),
		// deciding whether a class="voice_agent" credential's identity row has
		// been soft-deleted. Caller-scoping is not merely inconvenient here --
		// there is provably no caller to scope to, since the question being
		// asked IS "may this bearer become a caller at all". `userId` on a
		// voice_agent_token row is the synthetic minting user, so
		// userId==actor.userId would match zero rows for every real request and
		// fail the credential open.
		// memql#3063. The item memql#2987 deferred and then closed: same shape
		// as the trio above -- caller-supplied id, no actor check, identityFull
		// projecting `credentials` (keyHash, registeredBy, lastSeenAt,
		// lastConnectedFromIP) -- so it was on the wire for any authenticated
		// caller who guessed a userId. Narrowing the projection was not
		// available (the Go parser reads the nested credentials struct and a
		// shape keys paths by their terminal segment), and userId==actor.userId
		// was not either (the revoke ownership check runs under
		// contextWithSystemActor). Its only caller is
		// component/identity/workertoken. That package is allowlisted in
		// call_origin_conformance_test.go NOT for pat's reason (server-initiated)
		// but as a request-derived exception whose precondition -- the userId is
		// always the authenticated caller's subject -- is asserted by
		// component/grpc/worker_token_caller_scope_test.go. See that entry.
		{Path: "identity/queries.memql", Name: "workerTokensForUser"}: true,
		// memql#3209. The row set behind the in-process agent registry
		// (component/memql/agents.go LoadFromRows). Caller-scoping is circular
		// in the #2800 sense: the registry is a process-wide catalog built ONCE
		// at startup under the seed materializer's system actor, so at the only
		// moment this query runs there is no requesting user for actor.userId to
		// name -- the filter would evaluate against an empty actor, return zero
		// rows, and silently disable every specialist.
		//
		// It is @serverOnly rather than merely unscoped because agentFull
		// projects `systemPrompt`: an unscoped catalog read of every user's
		// persona is admissible inside the server and not admissible on the
		// generated client SDK.
		//
		// Its sibling allAgents stays on the wire, paged and classified as
		// before -- this is an ADDITIONAL complete read, not a widening of that
		// one. Sole caller: component/memql/agents.go, which stamps
		// auth.ContextWithInternalOrigin.
		{Path: "agents/queries.memql", Name: "agentsForRegistry"}: true,
		// memql#3591. The credential read behind Store.HasClaimedOwner, which
		// answers "has this cluster's owner ever authenticated" during identity
		// boot. Caller-scoping is circular in the #2800 sense and in the same
		// window as activeUsers above: there is no requesting user at that
		// moment, so `userId==actor.userId` would match zero rows and report
		// every claimed cluster as unclaimed.
		//
		// The question exists because the env bootstrap now writes the owner USER
		// row -- so a passkey-enrolment link can be minted for a named owner --
		// and a row written that way is not a claim. The auto-bootstrap guard
		// therefore asks about CREDENTIALS rather than rows.
		//
		// @serverOnly rather than merely unscoped because the filter keys on a
		// caller-supplied userId with no caller check: on the wire it would let
		// any authenticated client enumerate how recoverable somebody else's
		// account is. Its self-scoped twin signInIdentitiesForSelf stays on the
		// wire and is unchanged. Sole caller: component/identity/store.go, which
		// stamps auth.ContextWithInternalOrigin.
		{Path: "identity/queries.memql", Name: "signInIdentitiesForUser"}: true,
		// memql#3716. The write that grants an OAuth client credentialed CORS
		// access to identity's cookie-bearing auth endpoints.
		//
		// actor.userId scoping does not merely fail here, it names NOBODY: a
		// v1:identity:oauthClient row has no owning user at all. It is minted by
		// an unauthenticated stranger at POST /register (RFC 7591 dynamic client
		// registration), so there is no owner field for actor.userId to be
		// compared against and a self-scoped filter would match zero rows for
		// every caller -- including the owner or admin the surface exists for.
		//
		// Which leaves the shape memql#2991 found on updateUser: a
		// caller-supplied target id plus a caller-supplied value, on a field
		// that decides who may read authenticated responses cross-origin.
		// Client-reachable, any authenticated writer could grant their own
		// origin over the ordinary mutation surface and the owner/admin gate in
		// component/identity/adminops would be decorative. Its one caller is
		// that package, which authorizes first and then stamps internal origin
		// (asserted behaviourally by adminops/cors_test.go's
		// TestSetCORSOriginsStampsInternalOrigin, so the gate cannot break the
		// surface it protects).
		{Path: "identity/mutations.memql", Name: "setOAuthClientCORSOrigins"}: true,

		// memql#3964 -- the owner recovery key, the whole break-glass surface.
		//
		// These five share ONE argument, and it is the argument the enrolment
		// token already records: caller-scoping here is not merely unhelpful,
		// it is circular. The recovery key exists precisely for an owner who
		// has lost every sign-in route, so the actor is EMPTY at the moment
		// these run. `userId==actor.userId` would compare the row's owner
		// against "" and match nothing -- which does not present as an error,
		// it presents as a break-glass credential that reports "invalid"
		// forever. That failure is invisible until the one day it matters,
		// which is the worst possible time to discover it.
		//
		// @serverOnly rather than only an undeclared tier, because there is a
		// second thing to say: no CLIENT has any business on this surface at
		// all. Barring it from the generated SDK means a browser cannot
		// enumerate recovery keys, cannot deactivate one, and cannot present a
		// keyHash it guessed. The redeem path reaches recoveryKeyByHash from
		// identity's own HTTP handler, after the per-IP limiter, having hashed
		// a bearer the caller proved possession of; the row id every write
		// below uses is resolved by that handler from the verified hash and is
		// never taken from a caller argument.
		//
		// activeRecoveryKeys and claimRecoveryKey have no human actor for a
		// different reason again: their callers are the identity node's own
		// boot-time mint invariant (memql#3965) and `memql recovery-key claim`
		// running inside the pod, both under the system actor.
		{Path: "identity/queries.memql", Name: "recoveryKeyByHash"}:           true,
		{Path: "identity/queries.memql", Name: "activeRecoveryKeys"}:          true,
		{Path: "identity/mutations.memql", Name: "createRecoveryKeyIdentity"}: true,
		{Path: "identity/mutations.memql", Name: "claimRecoveryKey"}:          true,
		{Path: "identity/mutations.memql", Name: "redeemRecoveryKey"}:         true,
		{Path: "identity/mutations.memql", Name: "deactivateRecoveryKey"}:     true,
		// memql#4258. The guest-invitation write path, declared for the first
		// time -- component/grpc/guest_handlers.go named all five and no .memql
		// file declared any of them, so every guest-invite write failed at
		// execute on a cluster running the embedded tree.
		//
		// Caller-scoping is not merely hard here, it names the wrong person on
		// three of the five and is impossible on the other two.
		//
		// All four invitation mutations write `tokenHash`, which IS the
		// credential: the guest-auth interceptor admits
		// `Authorization: Guest <token>` by hashing the presented bearer and
		// matching this column. On the wire, createGuestInvitation is a
		// credential-FORGING primitive -- any authenticated caller could mint a
		// row carrying a digest they chose and then present its preimage. No
		// actor filter fixes that, because the caller legitimately IS an
		// authenticated user; what gates the write is the inviter's
		// relationship to the SPACE, which is not a field on this concept.
		//
		// The two redemption-side ones cannot be caller-scoped at all: the
		// actor is the GUEST, who by definition has no v1:identity:user row for
		// actor.userId to name (the accept path runs under
		// contextWithSystemActor for exactly that reason). A self-scoped filter
		// would match zero rows for the only caller that ever runs them.
		//
		// Scoping to `inviterId` -- the one plausible filter -- is the wrong
		// POLICY rather than an unavailable one, and revokeUserInvitation right
		// beside them records the same finding: it would mean only the original
		// inviter can cancel, and the case cancellation exists for is precisely
		// the one where that is wrong.
		//
		// The authorization is real and it is upstream: the handlers resolve
		// the invitation, check status and expiry, and verify the caller before
		// any of these run. The boundary therefore sits where the authorization
		// already lives -- the argument setOAuthClientCORSOrigins makes above.
		// The chunked-upload session writes (memql#4782). NOT the
		// caller-has-no-user-row shape above: ownerUserId IS stamped from
		// actor.userId, and actor-scoping is fully in place. What a caller
		// must never author is blobPath -- a storage path composed
		// server-side from the verified actor and the engine-minted fileId,
		// where a forged value would point the complete step at ANOTHER
		// user's bytes -- and status, whose 'open' stamp is what stops a
		// completed session being re-opened and completed twice. The quota
		// and provenance checks also run in the handler BEFORE the create,
		// so a session row's existence implies both passed; client-reachable
		// versions would make every one of those gates optional.
		{Path: "library/mutations.memql", Name: "createUploadSession"}:   true,
		{Path: "library/mutations.memql", Name: "completeUploadSession"}: true,
		// The Materializer's three writers (epic memql#4977). The same
		// asset as the two pairs above and again not a caller-scoping
		// question: ownerUserId is stamped from actor.userId on all three,
		// so a caller can only ever reach their own rows, and the composite
		// owner tier decides which those are.
		//
		// What a caller must never author is the RECORD ITSELF.
		// `provenanceEmbedded` and `modelsUsed` on a composition ARE the
		// provenance -- the whole reason this concept exists is to say
		// truthfully what a file was made from and which models touched it
		// -- so a client-reachable version would be a provenance record its
		// own subject could write, which is a record that says nothing. The
		// same argument one concept along for `recordComposeRecipeRun`:
		// `runCount` and `lastRunAt` are the evidence a recipe works, read
		// by somebody deciding whether to trust one with a client's report.
		//
		// `createComposition` is the third and its argument is narrower: a
		// composition row is a claim that a materialization HAPPENED, and
		// its goalId, runId and resolved sources are facts only the
		// executor holds. A browser could write one perfectly, own it
		// perfectly, and be recording a run nothing ever performed.
		{Path: "compose/mutations.memql", Name: "createComposition"}:      true,
		{Path: "compose/mutations.memql", Name: "updateCompositionState"}: true,
		{Path: "compose/mutations.memql", Name: "recordComposeRecipeRun"}: true,
		// The file-version supersede pair (epic memql#4806, design D10) --
		// the same asset as the session pair above, one concept along.
		// actor-scoping is again fully in place and again not the question:
		// ownerUserId is stamped from actor.userId on both, and the target
		// file was resolved under that same actor before a byte moved. What
		// a caller must never author is blobUrl, the storage path a version
		// row carries -- a forged one would name another user's object and
		// GET /artifacts/{id}/content?version={n} would stream it. The head
		// mutation additionally stamps status 'analyzing' rather than
		// 'stored', which is what keeps a supersede from re-firing
		// indexFileOnCreate and wiping the artifact's labels (the memql#4288
		// hazard reached through the promotion path); a client-reachable
		// version would put that back in a caller's hands.
		{Path: "library/mutations.memql", Name: "createLibraryFileVersion"}: true,
		{Path: "library/mutations.memql", Name: "supersedeLibraryFileHead"}: true,
		// The membership half, in cognition because the SPACE is cognition's.
		// Same gate, different asset: `isGuest` is authorization-relevant, not
		// decoration, so a client-reachable version would let any authenticated
		// caller place a guest participant into any space by id and skip the
		// invitation entirely. It cannot be caller-scoped for the same reason
		// as its two redemption siblings -- the guest has no user row -- and it
		// exists separately from joinSpaceAsHuman precisely because that one
		// content-addresses the participant id on (space, user) and requires
		// the userId a guest does not have.
		// memql#4328. The two engine-internal halves of the authentication
		// ACTIVITY log, and neither is caller-scopable -- for opposite
		// reasons, which is why they are listed together.
		//
		// createAuthActivity records an authentication that ALREADY happened.
		// Every argument is an assertion about it: which session, which
		// credential, which token hash the rotation retired. The check that
		// belongs in front of it is "the identity service verified this
		// credential", which no filter over actor.userId can express -- and
		// caller-scoping it would be actively wrong, because the row's owner
		// is stamped from the actor the WRITER borrows (the session's user),
		// not from a requesting caller. A client-reachable version would let
		// anyone manufacture activity history, retiredTokenHash included,
		// which is the exact field reuse detection keys on.
		//
		// authActivityByRetiredHash is the reuse lookup (memql#4329). It runs
		// on the /auth/refresh path, which is UNAUTHENTICATED by construction
		// -- it is the request that mints the credential -- so at the moment
		// it runs there is no caller identity to scope to. Worse, scoping it
		// would defeat it: the question is "did ANY session retire this
		// hash", asked precisely because the presenter is not the person who
		// owns it. Exposing it to clients would hand out an oracle over other
		// people's tokens.
		{Path: "identity/mutations.memql", Name: "createAuthActivity"}:      true,
		{Path: "identity/queries.memql", Name: "authActivityByRetiredHash"}: true,
		// memql#4360. The three writes on a delegated app-session row, and the
		// argument they share is one this list has not carried before: the
		// caller IS the owner in every legitimate case, so caller-scoping does
		// not merely fail to help -- it admits exactly the write that must not
		// happen.
		//
		// createAppSession carries the WORKSPACE the consent card approved and
		// the label of the per-run back-channel credential. Scoping answers
		// "whose row is it", and the hazard is not whose: a legitimate owner
		// could open a session naming a directory nobody approved. Only the
		// server knows which workspace the card actually named.
		//
		// appendAppSessionTranscript writes EVIDENCE -- the record a later
		// reader consults to answer what an agent did on somebody's computer.
		// The person best placed to edit it is its owner, including for the
		// run whose behaviour is the reason anyone is reading it.
		//
		// endAppSession carries `billing` and `usage`, which feed the plan's
		// spend rollup and the AI ledger. A client-reachable version would let
		// a caller declare their own run subscription-covered and its token
		// count zero, which is the one write that can make the dollar ceiling
		// stop working -- and again, the caller who benefits is the owner.
		{Path: "worker/mutations.memql", Name: "createAppSession"}:           true,
		{Path: "worker/mutations.memql", Name: "appendAppSessionTranscript"}: true,
		{Path: "worker/mutations.memql", Name: "endAppSession"}:              true,
		// memql#4389. The connector's own writes, and the two halves of
		// the push channel. What they share is that the caller is a
		// CONNECTOR rather than a person, so actor.userId names nobody --
		// and each is a claim about the outside world that only the code
		// which talked to it can honestly make.
		//
		// queueComplianceJob / recordComplianceJob carry a Shopify
		// privacy request and its outcome. A client-callable queue would
		// let a caller schedule a purge of a merchant's whole mirror;
		// `done` is a claim that a legal obligation was met.
		//
		// setProductContentStatus records what the far end did with a
		// pushed row: `live` is a claim about Shopify's acceptance, and
		// only the drain worker that made the call can make it.
		//
		// markQuoteAccepted carries the Shopify draft-order GID the
		// connector created. A client-callable version would let a caller
		// assert an order that does not exist -- and the quote's whole
		// value is that its prices were locked into a real one.
		{Path: "shopify/overlay/mutations.memql", Name: "queueComplianceJob"}:  true,
		{Path: "shopify/overlay/mutations.memql", Name: "recordComplianceJob"}: true,
		{Path: "commerce/mutations.memql", Name: "setProductContentStatus"}:    true,
		{Path: "commerce/mutations.memql", Name: "markQuoteAccepted"}:          true,
		// epic memql#4434, the release-cut pair. One argument covers both, and it
		// is not "the caller is a machine" -- the caller here is a signed-in
		// OWNER, which is the shape this map usually refuses.
		//
		// What earns it is that each row asserts something the caller is not
		// entitled to SAY, and caller-scoping addresses a different question:
		//
		//   createReleaseCut's `requestedBy` is the authority the row carries.
		//   A mutation argument is whatever the caller typed, so a
		//   client-reachable create lets any owner write a colleague's name into
		//   the release history -- append-only, so uncorrectable. Scoping to
		//   actor.userId would assert the row belongs to the caller, which is
		//   true and beside the point: what must hold is that requestedBy IS the
		//   actor the Go owner wall admitted, and only that Go frame knows it.
		//
		//   updateReleaseCutStatus's `status` is a claim about a CONTAINER
		//   REGISTRY. images_available means a GHCR manifest was fetched, and
		//   the whole point of D5 is that the status is verified rather than
		//   assumed -- a client-callable version restores exactly the false
		//   green it exists to prevent. There is also nobody to scope to: the
		//   concept is clusterOwner-tier, so the row has no owner field.
		//
		// The gate that makes this safe is IN GO and runs first
		// (integrations/release: auth.IsOwner before any HTTP), pinned against a
		// real engine actor by owner_wall_test.go. The DSL half of the double
		// wall is `requiresOwner` on the releaseCuts query, which gates the READ
		// and by construction cannot gate a builtin.
		{Path: "cluster/mutations.memql", Name: "createReleaseCut"}:       true,
		{Path: "cluster/mutations.memql", Name: "updateReleaseCutStatus"}: true,

		// THE WORK JOURNAL (work spine A1, design record
		// docs/superpowers/specs/2026-09-05-work-spine-design.md section D).
		// Fifteen constructs, and they earn the annotation twice over.
		//
		// The WRITES are the engine's testimony about its own execution. A
		// run row, a step receipt, a model-call record and an approval are
		// each a claim about what the platform DID, and resume, replay and
		// recall consume them as fact. A client-reachable insert is a forged
		// execution, and caller-scoping does not touch that: an
		// `ownerUserId==actor.userId` conjunct makes the forgery correctly
		// attributed and no less a forgery -- a caller could still mark
		// their own failed step `done`, or write a `served: journal`
		// model-call row that replay then hands to a model-free run.
		//
		// The READS are server-only for a mechanical reason rather than a
		// judgement. The journal writes under a SYNTHETIC cluster actor
		// (component/automations/journal.go), and
		// rowauthz_nonprincipal_owner.go blanks the owner such an actor
		// would stamp -- so these rows have a present-and-empty owner and
		// an actor.userId conjunct matches ZERO of them. Scoping the reads
		// would return an empty journal on exactly the runs resume exists
		// for, which is worse than refusing: resume would re-run completed
		// steps and call it a clean resume. Admission is the composite
		// tier's cluster-owner escape; WHO may resume is decided by the
		// caller's handler before LoadRunJournal runs.
		//
		// The person-facing reads of the same rows are NOT here, and that
		// is the check on this block: workGoalsForOwner and
		// workApprovalsForOwner are @actor and caller-scoped, because a
		// goal and a decision ARE somebody's.
		{Path: "work/queries.memql", Name: "workRunById"}:             true,
		{Path: "work/queries.memql", Name: "workStepsForRun"}:         true,
		{Path: "work/queries.memql", Name: "workRunsForAutomation"}:   true,
		{Path: "work/queries.memql", Name: "workModelCallsForRun"}:    true,
		{Path: "work/queries.memql", Name: "workObservationsForRun"}:  true,
		{Path: "work/queries.memql", Name: "workApprovalById"}:        true,
		{Path: "work/mutations.memql", Name: "createWorkRun"}:         true,
		{Path: "work/mutations.memql", Name: "updateWorkRun"}:         true,
		{Path: "work/mutations.memql", Name: "createWorkStep"}:        true,
		{Path: "work/mutations.memql", Name: "updateWorkStep"}:        true,
		{Path: "work/mutations.memql", Name: "createWorkGoal"}:        true,
		{Path: "work/mutations.memql", Name: "updateWorkGoal"}:        true,
		{Path: "work/mutations.memql", Name: "createWorkModelCall"}:   true,
		{Path: "work/mutations.memql", Name: "createWorkObservation"}: true,
		{Path: "work/mutations.memql", Name: "createWorkApproval"}:    true,
		{Path: "work/mutations.memql", Name: "decideWorkApproval"}:    true,

		// THE PROVING SUITE'S PUBLISHED RECORD (epic memql#4993, design section
		// G). Caller-scoping is not available even in principle, for the reason
		// the skill edges below give and one of its own: a benchmark is a fact
		// about the DEPLOYMENT rather than about a person. v1:bench:run and
		// v1:bench:sample declare @rowAuthz(clusterOwner) and carry no owner
		// field at all, so there is no actor.userId for a filter to compare
		// against -- and inventing one, by stamping the caller as a run's owner,
		// would turn a cluster-wide measurement into a per-person one and give
		// every signed-in account its own private set of numbers.
		//
		// What barring these from the wire buys is the thing the numbers are
		// FOR. README and docs/public carry published claims marked with the
		// figure each rests on, and TestPublishedClaimsRestOnAScorecardNumber
		// fails the build when the committed scorecard stops agreeing. A
		// client-reachable write here is therefore a primitive for forging the
		// numbers the product is sold on, which is a sharper consequence than
		// the usual "somebody could write a row they should not".
		{Path: "bench/mutations.memql", Name: "createBenchRun"}:    true,
		{Path: "bench/mutations.memql", Name: "createBenchSample"}: true,

		// THE CAPABILITY GRAPH'S EDGES (work spine A1, spec section C). An
		// edge is EVIDENCE that two skills relate, gathered from runs the
		// engine executed and committed only by a run that succeeded -- so a
		// client-reachable write is a claim about executions the caller did
		// not observe, and selection would then read it as structural fact
		// beside its vector matches.
		//
		// Caller-scoping is not available even in principle here, and that is
		// the unusual part worth stating: v1:skills:skillEdge is
		// @rowAuthz(public, requiresIdentity), because the predefined catalog
		// is read by every signed-in person's agents. There is no owner
		// conjunct to narrow by, and adding one would hide the catalog the
		// concept exists to describe. `ownerUserId` on these rows is
		// provenance, never a scope.
		{Path: "skills/mutations.memql", Name: "createSkillEdge"}: true,
		{Path: "skills/mutations.memql", Name: "commitSkillEdge"}: true,
		{Path: "skills/mutations.memql", Name: "setSkillScripts"}: true,
		{Path: "skills/mutations.memql", Name: "reinforceSkill"}:  true,
	}
	for k := range want {
		if !set[k] {
			t.Errorf("%s in %s no longer carries @serverOnly in the PARSED tree. If that is "+
				"deliberate it needs a caller-scope filter instead -- each of these is "+
				"server-only because caller-scoping is circular or because it is a sweep "+
				"(#2800 / #2883).", k.Name, k.Path)
		}
	}
	for k := range set {
		if !want[k] {
			t.Errorf("%s in %s GAINED @serverOnly. That exempts it from per-row-authz "+
				"classification (the gate short-circuits on hasServerOnly) and drops it from the "+
				"generated SDK, so it needs the same argument the others carry: why is "+
				"caller-scoping impossible here? If the answer is good, add it to `want`.", k.Name, k.Path)
		}
	}
	// These are the live set, keyed by PATH AND NAME. The prose used to say
	// "these six" and went on saying it at nine and at ten -- a hardcoded count
	// in a comment beside a map that grows is a lie with a delay fuse, so it is
	// gone rather than corrected (memql#3063).
	// Collapsing to name-only would defeat the key's stated purpose -- two
	// domains may declare the same construct name, so `activeUsers` in a
	// different file would satisfy the assertion.
	//
	// The assertion is two-way, and the ADDITION direction is the riskier one.
	// A construct DISAPPEARING means it silently lost its origin gate. A
	// construct APPEARING is how an author silences the per-row-authz
	// classification gate, which short-circuits on hasServerOnly -- so a new
	// @serverOnly on a client-facing query must not land with zero test signal.
	// The count is the covered set, not the whole tree. Attributes-bearing kinds
	// only: FunctionDef 476, ConceptDecl 100, ShapeDecl 87, BuiltinDecl 74,
	// SpecDecl 36, PromptDecl 25, ActionDecl 9, CapabilityDecl 8 = 815 of 1091
	// definitions. SeedDecl (185), ToolDecl (43), ProviderDecl (41) and
	// PolicyDecl (7) carry no Attributes field, so there is nothing to read --
	// TestServerOnlyParsedSetCoversEveryAttributedDeclKind is what keeps that
	// honest rather than assumed.
	t.Logf("parsed tree: %d @serverOnly construct(s) across %d attributed declarations", len(set), serverOnlySeen)
}

// TestServerOnlyParsedSetIgnoresSmuggledAnnotations is the failing-first half.
//
// It asserts the property that makes the parsed verdict better than the regex:
// an `@serverOnly` that is not an ANNOTATION -- because it sits inside a
// multi-line string, or inside a block comment -- must not appear in the set.
//
// Both fixtures are the REACHABLE spellings, verified by parsing them rather
// than by hand-reasoning. An earlier version of this test used
// "/*\n@serverOnly\n*/\nquery ..." which does not reach the audit at all: the
// preamble walk breaks on `*/`, so no gate ever saw it. The reachable
// block-comment form opens the comment on an `@`-line so it lands IN the
// preamble.
func TestServerOnlyParsedSetIgnoresSmuggledAnnotations(t *testing.T) {
	for _, tc := range []struct {
		name, src string
		want      bool
	}{
		// POSITIVE CONTROL first. Without it every case below passes on a
		// parser that ignores annotations entirely, which is the tautology this
		// test would otherwise be.
		{
			"a real annotation IS honoured",
			"@serverOnly\nconcept probeRealAnnotation {\n  a string\n}\n",
			true,
		},
		{
			"inside a multi-line annotation string",
			"@description(\"note\n@serverOnly\")\nconcept probeSmuggledViaString {\n  a string\n}\n",
			false,
		},
		{
			"inside a block comment opened on an @-line",
			"@description(\"a\") /*\n@serverOnly */\nconcept probeSmuggledViaComment {\n  a string\n}\n",
			false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			file, err := parseOneFileForAudit(tc.src)
			if err != nil {
				t.Fatalf("fixture did not parse (%v).\nA fixture that does not parse cannot "+
					"register, so it proves nothing about the audit -- fix the fixture rather "+
					"than deleting the case.", err)
			}

			var got bool
			var decls int
			for _, def := range file.Definitions {
				_, attrs, ok := declNameAndAttributes(def)
				if !ok {
					continue
				}
				decls++
				for _, a := range attrs {
					if a != nil && a.Name == "serverOnly" {
						got = true
					}
				}
			}
			if decls != 1 {
				t.Fatalf("fixture yielded %d named declarations, want 1 -- the case is not "+
					"measuring what it says", decls)
			}
			if got != tc.want {
				t.Errorf("parsed @serverOnly = %v, want %v (%s)", got, tc.want, tc.name)
			}

			// The CONTRAST that justifies the parsed verdict: for the smuggled
			// shapes the old source regex says YES where the parser says no.
			// That gap is the fail-open the issue reported -- the audit exempts
			// the construct while nothing enforces it at runtime.
			byRegex := smuggledServerOnlyRe.MatchString(tc.src)
			if !tc.want && !byRegex {
				t.Errorf("the source regex does NOT match %s either, so this case demonstrates "+
					"no divergence and guards nothing. Either the shape stopped being reachable "+
					"or the fixture is wrong -- check before deleting the case.", tc.name)
			}
		})
	}
}

// smuggledServerOnlyRe is the pattern the audit gates used before this change.
// It exists only so the tests above can show the DIVERGENCE the parsed verdict
// removes; nothing in the audit path consults it.
var smuggledServerOnlyRe = regexp.MustCompile(`(?m)^@serverOnly\b`)

// parseOneFileForAudit parses a fixture through the entry point the TREE LOAD
// uses -- languageCompiler.ParseFileSource, which applies the full rewrite chain
// (StripNonProceduralBlocks + NormaliseAll).
//
// It used to call languageParser.ParseFile, the bare lexer/parser, under a
// comment claiming it was "the same entry point the tree load uses". That was
// false, and the two demonstrably disagree: `@serverOnly` above an
// `automation a {...}` is a parse ERROR to ParseFile and a successful
// FunctionDef with serverOnly=true to ParseFileSource. A fixture validated on
// the bare parser is validated on a path production never runs.
func parseOneFileForAudit(src string) (*languageAst.File, error) {
	return languageCompiler.ParseFileSource(src)
}

// TestServerOnlyParsedSetCoversEveryAttributedDeclKind is the exhaustiveness
// check the type switch cannot give us.
//
// It reflects over every node in the real tree and fails on any type that has an
// `Attributes []*ast.Attribute` field but is not handled by
// declNameAndAttributes. Reflection is the right tool ONLY here: the walk itself
// stays an explicit switch (fast, obvious), and this test is what makes the
// switch's completeness checkable rather than assumed.
func TestServerOnlyParsedSetCoversEveryAttributedDeclKind(t *testing.T) {
	tree, err := dslimports.Load(dsl.Tree())
	if err != nil {
		t.Fatalf("dslimports.Load: %v", err)
	}

	seen := map[string]int{}
	uncovered := map[string]int{}
	for _, file := range tree.Files {
		if file == nil {
			continue
		}
		for _, def := range file.Definitions {
			typeName := fmt.Sprintf("%T", def)
			seen[typeName]++

			_, _, covered := declNameAndAttributes(def)
			if covered {
				continue
			}
			// Not covered -- does it carry Attributes anyway?
			v := reflect.Indirect(reflect.ValueOf(def))
			if v.Kind() != reflect.Struct {
				continue
			}
			f := v.FieldByName("Attributes")
			if f.IsValid() && f.Kind() == reflect.Slice {
				uncovered[typeName]++
			}
		}
	}

	if len(seen) == 0 {
		t.Fatal("walked 0 definitions; this test would pass vacuously")
	}

	names := make([]string, 0, len(uncovered))
	for n := range uncovered {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		t.Errorf("%s carries an Attributes field and appears %d time(s) in the tree, but "+
			"declNameAndAttributes does not handle it -- so every construct of that kind is "+
			"SILENTLY EXCLUDED from the @serverOnly audit. Add a case for it.", n, uncovered[n])
	}

	kinds := make([]string, 0, len(seen))
	for n := range seen {
		kinds = append(kinds, fmt.Sprintf("%s=%d", n, seen[n]))
	}
	sort.Strings(kinds)
	t.Logf("declaration kinds in the tree: %s", strings.Join(kinds, " "))
}
