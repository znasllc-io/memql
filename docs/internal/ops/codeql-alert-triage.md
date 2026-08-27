---
title: CodeQL alert triage -- what was fixed, what was dismissed, and why
audience: ops
status: stable
area: ops
sinceVersion: 0.21.0
owner: znas
---

# CodeQL alert triage

The default CodeQL suite runs on every push to `main`. This is the standing
record of how its open alerts were resolved. Twenty-six were triaged: the 23
open on `main`, plus 3 that arrived with the local-models-fleet epic.

**Six were real and are fixed in code. Twenty were false positives.** All six
fixes cleared the analysis -- alerts #1003-#1008 are `state=fixed`, not
dismissed -- and the twenty carry a dismissal reason each. `main` has no open
code-scanning alert.

Dismissals live on the alert itself (GitHub keeps them across re-scans of the
same finding). This file exists because a dismissal reason is one sentence and
the reasoning is not -- and because the NEXT reader's instinct on seeing
"SHA-256 used for password hashing, dismissed" will be to re-open it.

> **A dismissal is a claim about the code, and the code moves.** Each section
> below names the property that makes the alert wrong. If a change breaks that
> property, the dismissal is void and the alert is real again -- so the
> property, not the dismissal, is the thing to preserve.

---

## Fixed in code

### `go/request-forgery` -- Shopify Admin endpoint (critical)

`integrations/shopify/admin.go`. `AdminEndpoint` interpolated `store.Domain`
straight into `https://%s/admin/api/%s/graphql.json` with no validation, and
the composed URL is then sent that store's Admin API access token.

**The row IS the whole exploit.** `domain` is the one field of
`v1:shopify:store` an operator types by hand, and nothing else had to be wrong:
a row naming `attacker.example` mailed the token to whoever owned the name, and
one naming `169.254.169.254` or an in-cluster service turned the connector into
a probe of the cluster's own network. The `apiVersion` field was equally raw
and lands in a path segment.

Fixed by `NormalizeShopDomain` / `normalizeAPIVersion`
(`integrations/shopify/urlsafety.go`): the host must be a bare
`<shop>.myshopify.com`, the version a `YYYY-MM` quarter or `unstable`, and
`AdminEndpoint` now returns an error rather than a string. Applied at seed time
too (`SeedStoreFromEnv`), so a mistyped variable is a refusal at boot rather
than a store that reads as configured in the portal and fails every call.

Refusing rather than repairing is deliberate: a domain that is not a shop host
cannot be corrected into the one the operator meant, and rewriting it would
mirror somebody else's catalog under this store's id.

### `go/request-forgery` -- Shopify bulk download (critical)

`integrations/shopify/backfill.go`. `streamBulk` fetched whatever URL the
bulk-operation status carried.

The root cause is the one above -- the status came from a host named on the
store row -- so the first control is `NormalizeShopDomain`. The second is
`checkBulkDownloadURL`: https, no credentials, and, where the host is a literal
address, a public one. The host is deliberately **not** allowlisted, because
Shopify serves bulk results from object storage and which bucket host that is
stays theirs to change; an allowlist would break a backfill on a vendor's
routine migration.

`bulkDownloadClient` re-applies the same check on every redirect. A guard on
the first hop only is the usual way a guard like this is defeated, and
`TestABulkDownloadRedirectIntoTheClusterIsRefused` drives a real redirect over
TLS rather than asserting about the policy -- over plaintext the refusal lands
on the scheme instead, which passes while proving nothing.

`AdminClient.downloadGuard` is a test seam, present for the same reason the
neighbouring `endpoint` seam is: an `httptest` server is plaintext on loopback,
exactly what the guard refuses. `TestTheBulkDownloadGuardIsConsulted` removes
the seam and checks the refusal returns, so the seam cannot become the way
`streamBulk` quietly stops consulting the guard.

### `go/incorrect-integer-conversion` x4 (high)

`integrations/shopify/store.go` (`mapInt`) and
`component/worker/delegation_probe.go` (`intFromRow`). Both narrowed `int64`
and `float64` to `int` bare.

Both wider cases are reachable with a value `int` cannot hold: JSON decodes
every number to `float64`, a mirrored Shopify field is whatever that vendor's
API said, and a worker's capability cap is whatever a cockpit reported. A bare
conversion wraps on a 32-bit build and is implementation-defined for an
out-of-range float. A concurrency cap that wrapped negative reads as "this
machine can run nothing" and takes a signed-in worker out of the fleet with
nothing to show why.

Now saturating, which keeps the sign and the ordering -- all a count or a limit
is read for. `NaN` has no ordering and so no clamp: it becomes the same zero an
absent field does.

---

## Dismissed as false positives

### `go/weak-sensitive-data-hashing` x9 (high)

The query flags SHA-256 reached by a value it has classified as a password.
**None of the nine sites hash a password, or any credential at all.** They are:

| Site | What is hashed |
|---|---|
| `component/memql/outbox_append.go` | `(concept, rowId, version, target)` -- the idempotency key |
| `component/campaigns/roster.go` | a walk cursor, for a fixed-length claim key |
| `component/campaigns/digest.go` | a normalized email address -- the suppression row's id |
| `component/safety/approval.go` x2 | an approval fingerprint over command / URL / method / args |
| `component/memql/runtime_evaluator.go` | the DSL `hash()` builtin |
| `component/memql/mutation_templates.go` x2 | the same builtin, in the template evaluator |
| `component/identity/magiclink/verifier.go` | `(userId, email)` -- a deterministic identity id |
| `component/memql/ai_guard_fleet.go` x3 | `(model, kind, conversation)` -- the loop-breaker fingerprint |

The last three arrived with the local-models-fleet epic (memql#4694) rather
than with this pass, and are the same shape as the identical-request breaker
in `ai_guard.go`: a fingerprint exists so that two identical calls hash the
same, which is the property a deliberately slow, deliberately salted hash is
designed to destroy.

**The property that makes them wrong:** every one is a DETERMINISTIC
IDENTIFIER, not a stored secret. The digest is the lookup key, the row id, or
the value being concatenated. A computationally expensive hash is not a
hardening here, it is a different function: it would break determinism, break
every existing row id, and make the DSL `hash()` builtin unusable.

Note the sites that are NOT in this list and would be a different answer:
MemQL does store SHA-256 digests of bearer credentials (worker tokens, PATs,
invitation and enrolment tokens, magic-link binding nonces). Those are
256-bit random tokens, where a digest is the correct choice and a slow KDF
buys nothing, because there is no guessable input to iterate over. The
distinction to preserve is **entropy of the input**, not the algorithm: a
user-CHOSEN secret reaching SHA-256 would be a real finding.

### `go/cookie-secure-not-set` x7 (medium)

Every site sets `Secure: s.cookieSecure()`, which is
`strings.HasPrefix(cfg.BaseURL, "https://")`. CodeQL cannot prove a computed
value and flags it.

`component/identity/http/oidc_start.go` already carries the argument in a
comment: the identity service is reachable over http only under the documented
insecure-transport escape, and a `Secure` cookie over http is never sent back
-- so hard-coding `true` would harden nothing real and break local development,
while leaving every deployment unchanged. `requireSecureRequest` is the actual
control; the attribute follows the transport.

**The property that makes them wrong:** `cookieSecure()` is derived from
`BaseURL`, which is derived from `MEMQL_DOMAIN`. A cloud install is https, so
the attribute is `true` there. Change `cookieSecure` to return a constant, or
introduce a cookie that sets `Secure` from anything else, and re-triage.

### `go/unvalidated-url-redirection` x1 (medium)

`component/identity/web/redirect_authenticated.go`. The redirect target reaches
`http.Redirect` from the request, which is what the query sees.

It is validated first. `pickOAuthCtx` returns `matched=false` unless
`ClientAllowsRedirectURI` -> `clientAllowsRedirectURI` accepts the URI against a
registered client's `RedirectURIs`, and that matcher is **exact string match**
plus one structured exception (RFC 8252 §7.3: a loopback redirect registered
without a port matches any port). No wildcards, no prefix matching, no
path-only matching. An unmatched URI falls through to the sign-in form.

**The property that makes them wrong:** `clientAllowsRedirectURI` is
exact-match. If it ever grows a prefix, suffix or wildcard rule, this becomes a
real open redirect and the dismissal is void.

---

## Why the bulk download is not host-allowlisted

Worth recording because the obvious hardening was considered and rejected, and
the next person to look at `checkBulkDownloadURL` will wonder why it checks a
SHAPE rather than a name.

Shopify serves bulk results from object storage, and which bucket host that is
stays theirs to change. An allowlist would turn a vendor's routine migration
into a backfill that stops working -- loudly, but as an outage caused by a
hardcoded constant. So the check requires https, no credentials, and a public
host where the host is a literal address, and leaves the host itself open.

That is not the only thing in front of the request. In order:
`NormalizeShopDomain` means the status carrying the URL can only have come
from a real shop host; `checkBulkDownloadURL` applies the shape check; and
`bulkDownloadClient` re-applies it to every redirect hop, because a guard on
the first hop only is the usual way a guard like this is defeated.

**A note on what this section used to say.** While the fix was in flight it
predicted that CodeQL would keep flagging the download, on the reasoning that
the query cannot recognise a custom validator as a barrier -- and a transient
alert on the pull request's merge ref (#1030) was dismissed on that basis. The
prediction was wrong: on `main` the finding closed as `state=fixed` along with
the other five. The lesson is worth keeping even though the dismissal was not:
**an alert's fate is something to read after the scan, not something to
conclude from how the query works.**

---

## Re-triaging

```bash
# the open set, by rule
gh api -X GET /repos/znasllc-io/memql/code-scanning/alerts -f state=open --paginate \
  | jq -r 'group_by(.rule.id)[] | "\(length)\t\(.[0].rule.security_severity_level)\t\(.[0].rule.id)"'

# one alert in full
gh api /repos/znasllc-io/memql/code-scanning/alerts/<n> | jq '.most_recent_instance'
```

A dismissal is per-alert and survives re-scans of the same finding; the same
pattern in NEW code raises a new alert, which is the intent. Alert line numbers
are relative to `main` at scan time -- read the file at `origin/main`, not at
your branch, or the excerpt will be from somewhere else entirely.
