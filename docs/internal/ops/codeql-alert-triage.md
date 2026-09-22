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
record of how its open alerts were resolved, a round at a time.

**Round one (2026-08-26) -- twenty-six alerts:** the 23 open on `main`, plus 3
that arrived with the local-models-fleet epic. **Six were real and are fixed in
code. Twenty were false positives.** All six fixes cleared the analysis --
alerts #1003-#1008 are `state=fixed`, not dismissed -- and the twenty carry a
dismissal reason each.

**Round two (2026-08-31) -- three alerts** (#1032-#1034, memql#4777). All three
were real and all three are fixed in code; nothing was dismissed, so they
appear under [Fixed in code](#fixed-in-code) only.

**Round three (2026-09-06) -- forty-five alerts** (#1086-#1130, all raised
that day). **Forty-three are fixed in code and two are dismissed**, false
positives of the shape round one recorded. Forty-two of the forty-three are
ONE defect: a test helper that built a regex from a shop hostname, reached
from twenty-one sites and flagged by two rules at each -- so the count is a
fact about how the queries report, not about how much was wrong.

This file deliberately does NOT record a count of what is open now. That is a
live fact with a one-line query (see [Re-triaging](#re-triaging)), and a
sentence here saying `main` is clean goes quietly false the next time the suite
runs -- which is exactly what happened between the two rounds above.

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

### `go/incorrect-integer-conversion` x6 (high)

Round one, x4: `integrations/shopify/store.go` (`mapInt`) and
`component/worker/delegation_probe.go` (`intFromRow`). Round two, x2:
`integrations/agents/factory.go` (`intFromAnyLoose`). All three narrowed
`int64` and `float64` to `int` bare.

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

**This rule is the repo's recurring one, and what recurs is not the bug -- it
is the SWEEP.** Four rounds have now fixed it: `integrations/library`
(2026-06-19, alerts #301/#302), `integrations/dailyspace` (2026-07-17),
`integrations/shopify` + `component/worker` (2026-08-26), and
`integrations/agents` (2026-08-31). Each round fixed exactly the site CodeQL
named. `intFromAnyLoose` was written on 2026-05-21 and so PREDATES every one of
them: the oldest instance in the tree survived three rounds of fixing this
exact defect, because no round asked what else looked like it.

**Why a green scan is not the same as a clean tree here.** The query is
taint-driven -- it flags a narrowing only where it can trace the value back to
a `strconv.Parse*`. Every site is the same shape, a small `func …(v any) int`
decoding a JSON-ish payload field, and the ones with no traceable parser
upstream are invisible to it. Measured on 2026-08-31, after this round:

```bash
# every `case float64:` / `case int64:` arm returning a bare int(x)
git grep -n -A2 -E '^\s+case (float64|int64):$' -- '*.go' ':!*_test.go' \
  | grep -E 'return int\(|= int\(' | grep -v clamp
```

**22 files, 47 arms, none of them flagged.** They were deliberately NOT swept
in round two: the ask was the open alerts, and a 22-file change is a different
review. The number above is a measurement to re-take, not a fact to trust --
run the command.

The check when adding another such helper is therefore not "is this value big",
and not "did the scan stay green" -- it is that a bare `int(x)` in a `float64`
or `int64` arm is the defect, whatever the field means and whoever can prove
the value's provenance.

#### Round five closed the class, and became a gate (memql#4779)

Re-measured 2026-09-01: still exactly **22 files, 47 arms**. All 47 are swept,
and so are 5 `case json.Number:` arms that dropped `.Int64()`'s error and
converted anyway -- the same defect one level down, which the grep above cannot
see at all.

The seven copies became one: [`core/num`](../../../core/num/num.go), which
offers the three answers as NAMED choices (`Clamp*` saturates, `*OrZero`
returns 0, `*Or` returns the caller's default) because forcing one semantic
would have silently changed behaviour at four of the seven. Every collapsed
site keeps the answer it had. What did change is the BOUND: three of them
clamped at `math.MaxInt32` and called it "the worst-case int width", which is a
portability proxy rather than a domain rule -- on the only platforms this
repository builds for it protected nothing and truncated a legitimate 5 GB size
to 2147483647.

The gate is `TestEveryPayloadNarrowingCarriesAnAnswer`
(`numeric_narrowing_test.go`, root package). It parses every tracked non-test
Go file and fails on a `float64` / `int64` / `json.Number` arm that narrows the
arm's own value without declaring an answer, in a closed vocabulary
(`SATURATE` / `ZERO` / `DEFAULT` / `GUARDED`). It sweeps wider than the grep in
two ways that mattered: it follows the value one hop (`n, _ := v.Int64()`), and
it counts `float64 -> int64` as a narrowing, which found 33 further sites --
among them seven files whose integrality test was `float64(int64(n)) == n`, an
expression whose result is UNDEFINED for an out-of-range `n`. On amd64 it
answered "not a whole number", which is the safe direction and is why nobody
noticed; `num.WholeInt64` is exact, total, and converts nothing.

Two of those 33 were not merely undefined but wrong in output.
`component/language/compiler/automation_generator.go` rendered a whole float
too large for an `int` as `%d.0` of the integer indefinite value, so a `1e30`
literal compiled into generated MemQL source as `-9223372036854775808.0`. And
`integrations/workbench/dispatch.go` clamped `maxBytes` at the top and not the
bottom, so a negative one reached `make([]byte, cap+1)` and panicked the
dispatcher -- reachable from a tool argument, with no narrowing required.

**Honest limits of the gate**, since a checker that hides what it could not
examine makes its own pass a claim about the tool: it is syntactic and sees one
shape. A value that reached a local variable outside its case clause, a
`case int:` arm, a bare `v.(float64)` assertion, and
`uint32(x.GetNumberValue())` with no case clause near it are all invisible to
it. The sharpest of those is
`component/identity/webauthn/store.go`'s `signCount`, a replay-detection
counter narrowed `float64 -> uint32`; it is recorded here rather than gated,
because a detector wide enough to catch it also catches every protobuf field
width in the tree.

`factory.go`'s value is `maxSkills`, a per-role cap handed to the
`agentFactoryAnalyze` prompt, and it makes the cost of the wrong saturation
concrete. Truncation there does not merely lose magnitude, it INVERTS the
ranking the field exists to express -- a role declaring 2^32+1 presents to the
model as the most restrictive in the catalog rather than the least -- and
saturating to zero would have read as "this role may hold no skills at all".
Saturating to `math.MaxInt` is the only one of the three that preserves the
order. Measured, not reasoned: against the previous conversion the new tests
report `-9223372036854775808` on amd64, because Go leaves the out-of-range
float conversion implementation-defined and the hardware answers with the
integer indefinite value.

### `js/superfluous-trailing-arguments` x1 (warning)

`clients/os/test/setup.ts`. The jsdom shim installed as the global
`ResizeObserver` declared no constructor, while the real API takes a
`ResizeObserverCallback`.

**The alert pointed at the wrong file, and that is the interesting part.**
CodeQL resolves the global to the shim, so the finding landed on the correct
production call in `clients/os/src/wallpaper/MemoryField.tsx` --
`new ResizeObserver(resize)` -- as passing a superfluous argument. A gap in a
TEST double is not privately incomplete; it becomes a claim about application
code that is right.

The shim now declares the parameters and `implements ResizeObserver`. Both
halves of that were checked rather than assumed: renaming a method fails the
build (`TS2420`), and dropping the constructor parameter does NOT, because
TypeScript accepts a signature with fewer parameters anywhere one with more is
wanted. So `implements` gates the instance side only, and the comment in the
file says so instead of claiming a guard that is not there.

### `js/incomplete-hostname-regexp` x21 + `js/regex/missing-regexp-anchor` x21 (high)

`clients/os/test/stores/stores.test.tsx`, round three. One helper found a
store's list line with `new RegExp(domain)` over a shop hostname, and six call
sites wrote the same regex inline (`/acme-widgets.myshopify.com/`). Both rules
fire per CALL SITE, because the literal flows into the constructor at each one
-- so a single helper accounted for forty-two alerts, and the count says
nothing about how much code was wrong.

The defect is precision, not security, and it is real: the dots were
unescaped and nothing was anchored, so the matcher also took
`acme-widgetsXmyshopify.com` and `not-acme-widgets.myshopify.com`, and a test
that matches more than it means is one that passes on the wrong row. The row's
accessible name is the domain FOLLOWED BY its subline and state chips (the kit
`Row` names its button by content, not by `aria-label`), so an exact string
cannot match it and an end anchor cannot either. The fix is a predicate rather
than a better regex -- `name.startsWith(domain)`, in one `storeLine` helper
every site now goes through. There is no regex left for either query to read,
which is the honest way for a finding to close: not by escaping enough of it
to satisfy the pattern the query looks for.

### `go/allocation-size-overflow` x1 (high)

`integrations/work/fork.go`, `mergedVariables`:
`make(map[string]any, len(base)+len(overrides))`. The sum of two in-memory
map lengths cannot overflow on any platform this repository builds for, which
is the argument under every earlier dismissal of this rule here -- but the
hint was also simply WRONG. An override mostly restates a key the base already
carries, so the honest capacity is `len(base)`, and sizing for that takes the
arithmetic out of the allocation as a side effect. Fixed rather than dismissed
because the fix is the better code. `TestMergedVariablesLayersOverridesPerKey`
pins the merge on its own, including the property the handler test could not
see: the result is a NEW map, never a write into the source row's own.

---

## Dismissed as false positives

### `go/weak-sensitive-data-hashing` x10 (high)

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

**Since round one, eight more of the same shape** -- six dismissed between
rounds, each with its reason on the alert, and two in round three:

| Alert | Site | What is hashed |
|---|---|---|
| #1041 | `component/campaigns/delivery_id.go` | `(campaign, recipient)` -- the delivery row id, derived the way the DSL derives it |
| #1074 | `component/work/approval.go` | the artifact fingerprint an approval is a decision about |
| #1076 | `component/compose/write.go` | the bytes of a composed document -- `v1:library:file.sha256`, a dedup hint |
| #1083, #1084, #1085 | `component/emailrules/{fire,activate}.go`, `core/id/id.go` | `id.Combine` -- a content-addressed id from two ids |
| #1129 | `component/memql/authoring_catalog.go` | `CatalogKey` -- a construct's canonicalized SOURCE, the name-independent dedup signature |
| #1128 | `core/common/modelcall.go` | `ModelRequest.Hash` -- the journal's replay key over provider, model, settings, messages, tools and schema |
| #1133 | `component/memql/app_provider.go` | `AppCallFingerprint` -- the app door's loop-breaker key over `(app, conversation)`, the twin of #1027-#1029 |
| #1137 | `component/router/evidence.go` | `ProposalHash` -- the digest of the RENDERED rule source an approval is a decision about, the twin of #1074 |

**#1137 (epic memql#5146) is #1074's twin and the pairing is the argument.**
Both digest an ARTIFACT A PERSON IS APPROVING -- there, a command or a patch;
here, the routing rule the evidence fold proposes. The hash exists so that
approving cannot carry to different text: resume compares it, and a rule
edited after approval must fail that comparison. A salted KDF gives a
different digest for identical input, which does not harden the check, it
deletes it -- every resume would refuse, including the honest ones.

It hashes the RENDERED SOURCE rather than the form that produced it,
deliberately, so a renderer change moves the text out from under an approval
that would otherwise still verify. That is the one failure the hash exists to
prevent and the one nobody would notice.

What round three adds is the SOURCE the query traced, which no dismissal
before it named. Every path into #1128 and #1129 starts at one of three
values -- `envAnthropicAPIKey` and the `apiKey` argument of `providerKeySet`
in `component/memql/provider_config_write.go`, and the SMTP password slot in
`integrations/email/emailconfig.go` -- and reaches the digest 190 to 327
taint steps later, through the engine's `map[string]any` row plumbing. Two of
the three are the NAMES of environment variables, not secrets; the third is a
pasted vendor key that is sealed into a `globalSecret` row and never read
back. That is a path any string in any row can take, and it says nothing
about what the two functions digest: a construct's text and a model request's
content, each into a lookup key.

#1133 arrived with the app-door epic (memql#5096) and is the same function as
`FleetCallFingerprint`, one door over: `GuardLocalModelCall` is the chokepoint
for a call that has no `*http.Client`, and the fingerprint is what lets the
loop breaker notice a runaway. The two must hash the same shape or a runaway
through the app door would be invisible to a breaker that only recognises
fleet calls -- which is why it is a copy of that function rather than a
different one, and why it earns the same dismissal.

One phrasing in the dismissal comments between rounds should not be repeated:
"no password exists in this system to hash". An SMTP relay password does
exist. The claim that holds is the one below -- neither site is ever handed a
user-chosen secret to VERIFY, and a user-chosen secret reaching either hash
would still be a real finding.

**#1211 and #1212 (epic memql#5327) are #1134 and #1133 RENUMBERED, not new
findings** -- and the renumbering is the trap. Alert fingerprints include
position, so editing either file shifts the line and GitHub raises a fresh
alert number for the same defect while the old dismissal stays attached to the
old number. A reader who checks "is this rule already triaged" by alert number
concludes it is not.

What did change is what the two functions hash: design D13 added the ACTING
USER to both fingerprints, because two people asking the same question on one
replica were sharing a loop-breaker key and one person's runaway tripped the
breaker for the other. That is worth stating rather than passing over, since
the section's own rule is that the property is the thing to preserve:

- a user id is not a user-CHOSEN secret, so the entropy argument below is
  untouched -- it is an identifier the cluster assigned, never verified
  against a stored digest and never compared to anything;
- the fingerprint is still only a map key with a short window, still never
  stored, still never logged;
- and the reason it is hashed rather than concatenated is unchanged: the
  conversation is in there too, and a key built by concatenation would put
  prompt text in a map key.

If either fingerprint ever becomes something that is STORED, or compared
against a stored value, this dismissal is void and the alerts are real.

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
