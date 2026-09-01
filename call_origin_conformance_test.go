package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// TestOnlyAllowlistedPackagesStampInternalOrigin makes component/auth's
// call_origin.go rule enforceable instead of advisory (memql#2889).
//
// The rule it polices: internal origin is the ONE thing that opens the
// @serverOnly gate, and that gate is what stands between the wire and six
// read-only constructs that return directory-grade PII -- primaryEmail, phone,
// birthdate, role, group membership, suspension state -- for any user id or
// email a caller names, plus enumeration of the entire active-user table.
// call_origin.go says "never call it in a request handler on a context derived
// from an inbound request", and until now nothing checked that.
//
// WHY THIS PARSES RATHER THAN GREPS. A grep for the symbol over-reports, and
// not only on comments. Three real examples on this tree:
//
//   - component/auth/call_origin.go -- the doc comment stating the rule;
//   - component/grpc/server.go -- a comment explaining why the wire path has
//     none, which a naive gate would read as the wire path having one;
//   - component/language/annotations/registry.go:92 -- a STRING LITERAL, the
//     user-facing description of the @serverOnly annotation. Stripping
//     comments would not have caught that one.
//
// Every one is prose about the rule. A gate that cannot tell prose from a call
// either fails on its own documentation or needs a file allowlist that defeats
// the point. This also resolves import aliases, because several files import
// component/auth under a name of their own.
//
// WHY AN ALLOWLIST AND NOT A DENYLIST OF WIRE PACKAGES. A denylist protects
// the packages someone thought of. The population that may legitimately stamp
// internal origin is small, stable, and known; anything joining it is a change
// worth a reviewer looking at deliberately. A new package that starts stamping
// fails here by default, which is the direction the failure should point.
//
// WHAT THIS DOES NOT CATCH. Stated precisely, because an over-claimed
// guarantee is worse than a modest one. These limits were enumerated in PR
// #2933, a parallel solution to memql#2889 by session saa954c that did not
// land; they are its work, carried over because they are true of this gate too
// and were the better half of that PR.
//
//   - A new caller INSIDE an already-allowlisted package. Granularity is
//     per-package, and component/memql and app are large. Within those, this
//     is documentation rather than a gate.
//   - Laundering through an exported, context-returning wrapper in an
//     allowlisted package. None exists today -- component/automations'
//     originForSource is unexported -- but exporting one would open a hole
//     this cannot see.
//     (The identity-admin gate was listed here as a third gap: the gate is the
//     only thing making its allowlist entry safe, and nothing asserted it.
//     memql#2934 closed that for the templ console's HTTP routes, and
//     memql#3324 moved the writes to component/identity/adminops and moved the
//     assertion with them -- gate_test.go drives EVERY operation with every
//     role and asserts the engine is not reached below owner/admin, which is a
//     tighter statement than "every route is registered with `gated`" because
//     it is about the code path rather than the registration. memql#3380 then
//     retired the last templ surface, /admin/deployments, once a deploy call
//     could cross the mesh to the identity node; route_gate_test.go stays,
//     now covering the sign-in routes and the /admin/ signpost that remain.)
//
// WHY IT CATCHES MORE THAN THE COMPILER WOULD. go/parser ignores build
// constraints, so this sees files no CI lane compiles. app/integrations_identity.go
// is `//go:build identity` and does stamp internal origin (line 622); per
// memql#2903 the tagged lanes did not `go test` at all, so a violation added to
// a tagged file would have reached main unexamined. That gap is closed --
// #2903 added the mcp and identity lanes plus scripts/citags, a gate that fails
// the build if a tagged suite ever loses its lane again -- but this check is
// still the broader one: it sees files under EVERY tag, including tags no lane
// will ever run (clustere2e, telnyx_live), because go/parser ignores build
// constraints entirely.
func TestOnlyAllowlistedPackagesStampInternalOrigin(t *testing.T) {
	// Directory -> why that package may stamp internal origin.
	//
	// Every entry is a package doing SERVER-INITIATED work: boot, migration,
	// reconciliation, or a system query on behalf of no caller. None of them
	// is a request handler.
	//
	// Of these, all but component/automations apply the stamp INLINE as the
	// argument to a single Execute, so the marked context dies at that call
	// and its blast radius is one query. component/automations is the one that
	// produces a context which propagates into further dispatch -- which is
	// exactly where memql#2879 found a live inheritance hole, and why it now
	// stamps client explicitly on the untrusted branch rather than passing the
	// parent through.
	allowed := map[string]string{
		"app":                         "identity integration wiring at boot; no request in scope",
		"component/auth":              "defines the stamp, and resolves an identity from claims before any actor exists",
		"component/automations":       "trusted automation dispatch; the untrusted branch stamps CLIENT (memql#2879)",
		"component/identity":          "identity store internals, server-initiated",
		"component/identity/adminops": "identity-admin write surface -- REQUEST-DERIVED, one of the two exceptions here; its precondition (every path is downstream of the owner/admin gate in the same function) is asserted by component/identity/adminops/gate_test.go, memql#3324",
		"component/identity/pat":      "personal-access-token store, server-initiated",
		// The custom-domain reconciliation sweep (epic memql#4805). SERVER-
		// INITIATED, and not one of the request-derived exceptions: the caller
		// is a scheduled automation, not an HTTP handler, and the six writes it
		// stamps for are the engine's own account of what it saw in DNS and at
		// the API server.
		//
		// The stamp is not a widening. Every one of those mutations is
		// @serverOnly over a clusterOwner-tier concept, so what internal origin
		// buys is the ability to call them AT ALL -- origin defaults to CLIENT
		// and the function validator refuses. Without it the sweep fails every
		// write with a WARN in the log and every binding sits in `pending_dns`
		// forever, which is the shape of bug that survives a release.
		//
		// It is deliberately NOT stamped on the two reads the `customDomainAdd`
		// capability makes: those run under the CALLER's own actor
		// (Store.callerRows), because "which deployables may YOU act on" is a
		// different question from "which exist", and stamping there would hand
		// a caller-scoped read the engine's escape.
		"integrations/customdomain": "the custom-domain reconciliation sweep, server-initiated; its six @serverOnly writers are refused without it",
		// REQUEST-DERIVED, and the FOURTH exception. Ownership transfer
		// (memql#4838): one stamp, in reassignRow, on the single Execute that
		// writes the new owner onto one row.
		//
		// WHY IT CANNOT USE THE CLUSTER-OWNER ESCAPE INSTEAD, which is the
		// obvious alternative and would need no entry here. It would work
		// today and stop working exactly when this feature is needed: the
		// rank-strict tier WITHDRAWS that escape for a peer-owned row (epic
		// memql#4832, D3), and a peer-owned row an owner cannot write is the
		// only reason transfer exists. The failure would be a transfer that
		// silently moves nothing on the one concept that required it.
		//
		// WHY THE REQUEST-DERIVED SHAPE IS THE SAFE ONE HERE, on the same
		// terms as component/identity/adminops above: every path that reaches
		// the stamp is downstream of the cluster-owner gate in the SAME
		// function (handleTransferRowOwnership refuses a non-owner before any
		// row is read), and the stamp is applied INLINE as the argument to one
		// Execute, so the marked context dies at that call. That precondition
		// is asserted rather than asserted-in-prose:
		// integrations/identity/internal_origin_precondition_test.go.
		//
		// The audit write beside it is deliberately NOT stamped:
		// createAuditEvent is not @serverOnly and the actor it records is this
		// caller, so the ordinary path admits it.
		"integrations/identity": "ownership transfer (memql#4838) -- one inline stamp, downstream of a cluster-owner gate; rank-strict withdraws the cluster-owner escape it would otherwise use",
		// REQUEST-DERIVED, and the THIRD exception. The redeem path
		// (component/identity/http/webauthn_recovery.go) calls Store.Resolve on
		// an UNAUTHENTICATED request context -- the shape call_origin.go warns
		// about in as many words. The other two callers are plainly
		// server-initiated: the boot invariant and `memql recovery-key claim`,
		// neither with a request in scope.
		//
		// Added because the ENTIRE break-glass surface is @serverOnly -- both
		// reads and all four writes -- and the store stamped none of it, so
		// every call was refused and the credential was inert. Not degraded:
		// no cluster minted an owner recovery key, claim exited 1, and redeem
		// could not resolve a presented key. Every gate was green while that
		// was true, which is why TestEveryGoCallerOfAServerOnlyConstructStampsInternalOrigin
		// (test/dslconformance) now asks the converse question this list
		// cannot: not "may this package stamp" but "does the caller that MUST
		// stamp actually do so".
		//
		// What earns the exception is the ARGUMENT, and it is a different
		// argument from workertoken's below -- so it gets a different check,
		// not a copied one:
		//   - recoveryKeyByHash filters on a DIGEST of a secret the caller had
		//     to present, and Resolve computes that digest itself rather than
		//     accepting one. Naming a row is a possession proof, not an
		//     identifier a caller can choose; there is nothing to enumerate.
		//     That is why this needs no analogue of
		//     component/grpc/worker_token_caller_scope_test.go -- there is no
		//     caller-supplied id to scope.
		//   - activeRecoveryKeys DOES take a caller-supplied userId, and no
		//     wire path reaches it: the invariant and the CLI resolve the owner
		//     themselves, and adminops.RotateRecoveryKey sits downstream of the
		//     owner/admin gate asserted by component/identity/adminops/gate_test.go.
		//   - The rows carry no directory PII -- a recovery-key row is a hash,
		//     a binding and some timestamps. Never the plaintext: Row has
		//     nowhere to put one.
		//
		// The stamp lands on a context scoped to ONE call and is structurally
		// unable to escape: executeServerOnly returns a RESULT, never a
		// context, so no later frame can inherit the mark. Pinned by
		// component/identity/recoverykey/store_internal_origin_test.go, which
		// drives all six operations with a client-origin context, and by
		// component/memql's TestRecoveryKeyConstructsAreServerOnlyAndInternalOriginPasses,
		// which asserts the engine refuses client origin and admits internal.
		// A CONNECTOR, and the first of that class (epic memql#4378). The
		// argument is different from every entry above and is worth stating
		// rather than borrowing.
		//
		// Every v1:shopify:* concept declares @origin("shopify"), which makes
		// each a MIRROR: the engine refuses every write to one that does not
		// come from the shopify connector. Since memql#4389 the connector
		// generates no mutations at all -- the runtime performs mirror
		// writes from the MirrorWrites it returns -- so what needs internal
		// origin here is the raw insert that write renders, plus the
		// @serverOnly store and compliance-queue mutations the connector
		// owns outright.
		//
		// SERVER-INITIATED, with no request in scope. A Shopify webhook is
		// STAGED as a v1:platform:inboundRequest row by the HTTP edge and
		// worked afterwards by an automation; the connector never sees the
		// request, only the row. The scheduled reconcile has no request at
		// all.
		//
		// WHAT THE STAMP CAN REACH IS BOUNDED BY THE MIRROR, NOT ONLY BY THE
		// STAMP, which is what makes this narrower than the entries above:
		// connectorContext stamps the CONNECTOR ACTOR alongside it, and that
		// actor is admitted by row admission to the concepts naming
		// "shopify" and to nothing else -- including the undeclared concepts
		// that admit everyone.
		//
		// THAT BOUND USED TO BE TRIVIAL AND IS NOT ANY MORE, and saying so is
		// the point of this paragraph rather than leaving the old sentence
		// standing. J0 shipped a three-field product index -- handle,
		// availableForSale, present -- so a leaked context reached nothing
		// anyone would want. memql#4389 mirrors 65 Shopify types and four
		// commerce origins, and v1:shopify:customer is in that set. The
		// justification is therefore no longer "the data is trivial". It is
		// that the reach is exactly the data this connector already pulls
		// from Shopify under an Admin token it already holds, and not one row
		// beyond it: a leaked stamp buys nothing the connector's own
		// credential did not already buy, and buys it nowhere else in the
		// graph.
		//
		// The stamp is applied inline on a context this package constructs
		// and dies at the one Execute it is passed to.
		// The DATA-ORIGINS RUNTIME (epic memql#4378). Server-initiated on
		// every path, with no request in scope on any of them:
		//
		//   - the outbox drain worker is a poll loop on a timer;
		//   - the backfill and reconciliation runners are operator- or
		//     schedule-driven;
		//   - the inbound dispatcher works a v1:platform:inboundRequest ROW
		//     that the HTTP edge staged and an automation handed over. It
		//     never sees the request -- by the time it runs, the signature
		//     has been checked, the body has been persisted, and the socket
		//     is long closed.
		//
		// What it stamps for: its own bookkeeping. The outbox queue and the
		// health timeline are clusterOwner-tier concepts whose mutations are
		// @serverOnly precisely because they belong to the deployment rather
		// than to any user, so the runtime has to present internal origin to
		// write the rows it exists to write. The stamp is applied in ONE
		// place -- OperatorContext, alongside the operator AccessContext --
		// on a context this package constructs per call.
		//
		// Mirror writes do NOT go through it. Those run under the CONNECTOR
		// actor, a narrower credential admitted only to the concepts naming
		// that connector; the two are stamped separately and the call sites
		// say which is in scope.
		"component/datasync": "the data-origins runtime -- server-initiated bookkeeping over its own clusterOwner-tier queue and health rows; mirror writes use the narrower connector actor instead (epic memql#4378)",
		// The RELEASE CUTTER (epic memql#4434). REQUEST-DERIVED, and the
		// fourth exception -- stated rather than borrowed, because the caller
		// here is neither a connector nor a boot path: it is a signed-in human
		// clicking a button in the portal, which is the shape call_origin.go
		// warns about in as many words.
		//
		// WHAT EARNS IT is a precondition that is ASSERTED rather than merely
		// stated here: every path that reaches the stamp is downstream of the
		// Go owner wall, in the same function, before any network call. That
		// is the adminops shape, and it gets the adminops treatment --
		// integrations/release/owner_wall_test.go drives a non-owner actor
		// through both capabilities and asserts the engine is never reached,
		// against a REAL engine actor rather than a mock.
		//
		// WHAT IT REACHES is narrower than every entry above, and the
		// narrowness is structural rather than incidental. Three constructs:
		// createReleaseCut, updateReleaseCutStatus and createAuditEvent. None
		// of them returns a row to a caller, so there is no directory-grade
		// PII here and nothing to enumerate -- the widest thing a leaked
		// context could do is append to an append-only release history that
		// an owner may already append to. The one READ it stamps
		// (Store.CutByVersion, over the owner-gated releaseCuts query) runs on
		// behalf of a caller the wall has already admitted as an owner, so the
		// stamp buys it nothing the caller did not already have.
		//
		// The stamp is applied INLINE in executeServerOnly, which returns a
		// RESULT and never a context -- so no later frame can inherit the mark
		// and open a different @serverOnly construct, which is the memql#2989
		// escalation. One seam, for the identity store's reason: a stamp
		// hand-rolled per call site is a stamp the next call site copies
		// slightly wrong, and this gate polices packages rather than sites.
		"integrations/release":           "cutting a release of MemQL itself -- REQUEST-DERIVED, earned by every stamped path sitting downstream of the Go owner wall in the same function (asserted by integrations/release/owner_wall_test.go), and bounded to two append-only bookkeeping writes plus an audit event (epic memql#4434)",
		"integrations/shopify":           "the shopify CONNECTOR -- server-initiated mirror writes; its @serverOnly mutations exist because the concept is a mirror, and the connector actor stamped beside internal origin bounds the reach to that mirror alone (epic memql#4378)",
		"component/identity/recoverykey": "break-glass recovery-key store -- REQUEST-DERIVED on the redeem path; earned by the argument being a digest of a presented secret rather than a caller-chosen id, asserted by component/identity/recoverykey/store_internal_origin_test.go",
		// REQUEST-DERIVED, and the SECOND exception -- not, as the first draft
		// of this entry said, "a credential store whose reads are
		// server-initiated by construction, same as the pat entry above". The
		// #3072 review caught that wording: ListForUser's only caller is a
		// request handler (handleRevokeWorkerToken, on s.stream.Context()), so
		// this is the shape memql#2989 refused and test/dslconformance/server_only_parsed_test.go
		// names as refuted. An allowlist entry whose stated reason is wrong is
		// worse than no entry -- the reason is what the next reader trusts, and
		// "server-initiated" invites a future caller to pass anything.
		//
		// Added when workerTokensForUser became @serverOnly (memql#3063): it
		// projects identityFull, so the row carries keyHash, registeredBy,
		// lastSeenAt and lastConnectedFromIP, behind a filter keyed on a
		// caller-supplied userId with no actor check.
		//
		// Two facts earn the exception, and both are ASSERTED, not merely
		// stated here:
		//   - the stamp lands on a context scoped to that one query, never the
		//     caller's, so it cannot open other @serverOnly constructs for the
		//     rest of the request (the memql#2989 escalation) --
		//     component/identity/workertoken/store_internal_origin_test.go;
		//   - every call site passes the AUTHENTICATED caller's subject rather
		//     than a request payload field. Without that, the exposure #3063
		//     closed reopens underneath the annotation with the annotation, the
		//     stamp test, and this gate all still green (measured during the
		//     #3072 review) -- component/grpc/worker_token_caller_scope_test.go,
		//     the analogue of the route_gate_test.go memql#2934 made
		//     component/identity/admin carry for exactly this reason.
		"component/identity/workertoken": "worker-token store -- REQUEST-DERIVED; precondition (userId is always the authenticated caller's subject) asserted by component/grpc/worker_token_caller_scope_test.go, memql#3063",
		// REQUEST-DERIVED, the recoverykey/workertoken shape: a package that
		// exists to hold ONE stamp, with nothing else in it. The @serverOnly
		// pair it satisfies (createUploadSession / completeUploadSession)
		// guards blobPath -- a storage path a caller must never author,
		// because a forged one points the complete step at another user's
		// bytes -- and the 'open' status stamp that stops a completed
		// session being re-driven. Three asserted facts earn the entry, all
		// in component/server/uploadsession/store_internal_origin_test.go:
		// the constructs really are @serverOnly in the loaded registry (the
		// stamp is required, not decorative); no Store method returns a
		// context (the memql#2989 escalation cannot happen); and the
		// rendered writes never name ownerUserId or status, while the READ
		// is deliberately unstamped so row admission stays the per-chunk
		// owner check.
		"component/server/uploadsession": "chunked-upload session store -- REQUEST-DERIVED; preconditions (stamp is required by @serverOnly, dies inside one call, and the writes never name an owner) asserted by component/server/uploadsession/store_internal_origin_test.go, memql#4782",
		// REQUEST-DERIVED, the uploadsession shape one concept along: a
		// package holding ONE stamp and nothing else. The @serverOnly pair
		// it satisfies (createLibraryFileVersion / supersedeLibraryFileHead)
		// guards blobUrl for the same reason -- a caller-authored storage
		// path would name another user's object, and GET
		// /artifacts/{id}/content?version={n} would stream it -- plus the
		// head's 'analyzing' status stamp, which is what stops a supersede
		// re-firing indexFileOnCreate and wiping the artifact's labels.
		// Asserted in component/server/fileversion/store_internal_origin_test.go:
		// the constructs really are @serverOnly in the loaded registry; no
		// Store method returns a context; and no rendered write names
		// ownerUserId. This package holds no reads AT ALL -- every version
		// read runs unstamped through component/server's own store, under
		// the caller's actor -- which is the strongest form of the third
		// property its sibling asserts.
		"component/server/fileversion": "library file-version supersede store -- REQUEST-DERIVED; preconditions (stamp is required by @serverOnly, dies inside one call, no write names an owner, and the package holds no reads) asserted by component/server/fileversion/store_internal_origin_test.go, memql#4806",
		"component/memql":              "seed materialiser and authoring capability store, both boot-time",
		// SERVER-INITIATED, not request-derived -- the same class as
		// integrations/agent/worker below rather than the three exceptions
		// above, and the distinction is worth stating because this package
		// sits next to a wire surface.
		//
		// The three app-session mutations are @serverOnly (memql#4360), and
		// OriginClient is the zero value, so without the stamp every session
		// row write is refused with only a WARN -- sessions that never appear,
		// evidence at a log level nobody watches.
		//
		// What makes it server-initiated: nothing here takes a caller-supplied
		// identifier off a wire request. The session id is engine-minted, the
		// worker id is resolved from the registry by the engine's own
		// selection, the transcript is chunk data arriving on a stream the
		// COCKPIT opened outward, and ownerUserId comes from the planner's
		// resolution of the Task's owner. WorkerService's own request path
		// (component/worker/server.go) stamps nothing and must not: it is on
		// the wire-path deny list below, and the registration writes it makes
		// go through mutations that are not @serverOnly.
		//
		// The stamp is scoped to ONE Execute call and cannot escape --
		// appSessionWriteContext's result is a local in each store method and
		// is never returned, so no later frame inherits the mark. That is the
		// memql#2989 escalation shape, and it is asserted rather than asserted
		// here: component/worker/appsession_store_test.go.
		"component/worker": "app-session row writes -- server-initiated; the ids are engine-minted and the payload arrives on a stream the cockpit opened, with no caller-supplied identifier in scope (memql#4360)",
		// SERVER-INITIATED (epic memql#4794). The packages pipeline advances
		// a deployment by writing its own row, six times, across stage
		// handoffs that frequently land on a different node from the one the
		// person clicked on -- so on the write path there is no caller in
		// scope at all, and actor.userId is empty. Every one of those
		// mutations is @serverOnly and OriginClient is the zero value, so
		// without the stamp each advance is refused with only a WARN: a
		// deploy that appears to hang with nothing in the timeline.
		//
		// What makes it server-initiated rather than request-derived: no
		// caller-supplied identifier is in scope on any write. The deployment
		// id is engine-minted, ownerUserId is COPIED off the package row the
		// starting caller already read under their OWN actor (so it cannot
		// name a user that caller could not act as), and the report is the
		// output of the offline analysis rather than anything a caller sent.
		//
		// The stamp is scoped to ONE Execute call and cannot escape: it is
		// applied inline inside store.writeInternal and the marked context is
		// never returned, so no later frame inherits it -- the memql#2879
		// shape. Reads are deliberately UNSTAMPED and run under the caller's
		// own actor, which is what keeps row admission the composite tier's
		// decision. Both asserted in
		// component/packages/internal_origin_test.go.
		"component/packages":        "package deploy pipeline -- server-initiated; stage advances happen after cross-node handoffs with no caller in scope, and every id is engine-minted (memql#4794)",
		"integrations/agent/worker": "worker store, server-initiated",
		"integrations/dailyspace":   "scheduled space provisioning, no caller in scope",
	}

	// The wire path. These must never appear, allowlist or not: every handler
	// under them runs on a context derived from an inbound request, which is
	// precisely the case call_origin.go forbids. Listed explicitly so the
	// failure names the rule rather than just "not in the allowlist".
	wirePath := []string{"component/grpc", "component/node"}

	out, err := exec.Command("git", "ls-files", "-z", "*.go").Output()
	if err != nil {
		t.Fatalf("git ls-files: %v", err)
	}

	fset := token.NewFileSet()
	seen := map[string][]string{} // dir -> "file:line"

	for _, rel := range strings.Split(string(out), "\x00") {
		if rel == "" || strings.HasSuffix(rel, "_test.go") {
			// Test files are excluded deliberately: call_origin_test.go must
			// construct an internally-stamped context to test the override,
			// and a gate that forbade that would forbid testing the thing it
			// polices.
			continue
		}
		file, perr := parser.ParseFile(fset, rel, nil, 0)
		if perr != nil {
			continue // not buildable on its own (build tags etc.); not this gate's business
		}

		// Resolve however THIS file refers to component/auth.
		local := map[string]bool{}
		for _, imp := range file.Imports {
			path, uerr := strconv.Unquote(imp.Path.Value)
			if uerr != nil || path != "github.com/znasllc-io/memql/component/auth" {
				continue
			}
			switch {
			case imp.Name == nil:
				local["auth"] = true
			case imp.Name.Name == ".":
				local[""] = true // dot-import: the call is unqualified
			default:
				local[imp.Name.Name] = true
			}
		}
		// Inside component/auth itself the call is unqualified.
		if file.Name.Name == "auth" && strings.HasPrefix(rel, "component/auth/") {
			local[""] = true
		}
		if len(local) == 0 {
			continue
		}

		// Match any REFERENCE to the symbol, not only a call in Fun position.
		//
		// Matching calls was the obvious implementation and it was trivially
		// defeatable: `f := auth.ContextWithInternalOrigin` and then `f(ctx)`
		// is not a CallExpr naming the symbol, so four separate bypass forms
		// placed in component/grpc produced no output at all from this gate.
		// A gate on a security invariant that a function value walks past is
		// not a gate.
		//
		// The anti-grep rationale survives intact: comments and string
		// literals are neither SelectorExpr nor Ident, so the three prose
		// cases in the header are still not matched. The one thing that must
		// be skipped is the declaration itself, or component/auth reports its
		// own definition.
		// The declaring identifier, by POINTER identity.
		//
		// The obvious spelling -- returning true from ast.Inspect on the
		// FuncDecl -- is a no-op: true means "descend into children", and
		// FuncDecl.Name is a child, so the declaration matched anyway. That
		// was not cosmetic. It made component/auth permanently present in
		// `seen`, so the stale-allowlist check below could never fire for it
		// (verified: deleting component/auth's only real use still passed),
		// and it propped up the len(seen)==0 vacuity guard with a match that
		// exists as long as the function does.
		//
		// Returning false would skip the declaration AND its body, which is
		// wrong in general -- a function may legitimately reference the symbol
		// inside itself.
		declIdent := map[*ast.Ident]bool{}
		for _, d := range file.Decls {
			if fn, isFunc := d.(*ast.FuncDecl); isFunc && fn.Name.Name == "ContextWithInternalOrigin" {
				declIdent[fn.Name] = true
			}
		}

		ast.Inspect(file, func(n ast.Node) bool {
			var (
				matched bool
				pos     token.Pos
			)
			switch ref := n.(type) {
			case *ast.SelectorExpr:
				pkg, isIdent := ref.X.(*ast.Ident)
				matched = isIdent && local[pkg.Name] && ref.Sel.Name == "ContextWithInternalOrigin"
				pos = ref.Pos()
				if matched {
					// Stop descending. Sel is an *ast.Ident with the same
					// name, so in a file that ALSO dot-imports component/auth
					// (making local[""] true) the Ident arm below would record
					// the same reference a second time. Not reachable on this
					// tree -- no file both aliases and dot-imports -- but the
					// double-count is silent, and a doubled position list
					// reads as two violations where there is one. Credit
					// PR #2933, which guarded this from the start.
					dir := filepath.Dir(rel)
					seen[dir] = append(seen[dir], fset.Position(pos).String())
					return false
				}
			case *ast.Ident:
				matched = local[""] && ref.Name == "ContextWithInternalOrigin" && !declIdent[ref]
				pos = ref.Pos()
			}
			if matched {
				dir := filepath.Dir(rel)
				seen[dir] = append(seen[dir], fset.Position(pos).String())
			}
			return true
		})
	}

	if len(seen) == 0 {
		t.Fatal("no ContextWithInternalOrigin calls found anywhere -- this gate has stopped resolving them and would now pass vacuously")
	}

	for _, dir := range sortedKeys(seen) {
		for _, banned := range wirePath {
			if dir == banned || strings.HasPrefix(dir, banned+"/") {
				t.Errorf("%s stamps internal origin:\n  %s\nEvery handler here runs on a context derived from an inbound request. "+
					"component/auth/call_origin.go forbids that outright, and internal origin is the only thing that opens the "+
					"@serverOnly gate -- so this would expose directory-grade PII for any user the caller names.",
					dir, strings.Join(seen[dir], "\n  "))
			}
		}
		if _, ok := allowed[dir]; !ok {
			t.Errorf("%s stamps internal origin but is not in the allowlist:\n  %s\nIf that is deliberate, add it with the reason it does server-initiated work. "+
				"If the context it stamps is derived from an inbound request, it is a security defect, not an allowlist gap.",
				dir, strings.Join(seen[dir], "\n  "))
		}
	}

	// A stale entry is a gate that has quietly stopped covering anything.
	for _, dir := range sortedKeys(allowed) {
		if _, ok := seen[dir]; !ok {
			t.Errorf("stale allowlist entry %q -- it no longer stamps internal origin, so remove it. "+
				"Entries that cover nothing make the list look more considered than it is.", dir)
		}
	}
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
