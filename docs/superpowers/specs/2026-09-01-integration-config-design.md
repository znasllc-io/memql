# Integration Configuration from OS Settings (SP4) -- Design

- **Date:** 2026-09-01
- **Status:** approved (program decision P6 in
  [2026-09-01-email-campaigns-program-design.md](2026-09-01-email-campaigns-program-design.md)
  is the authority this spec elaborates; every fork below records the
  choice that was made and why)
- **Scope:** a declared per-integration **config manifest** and a shared
  resolution helper in `integrations/`; one new key on
  `component/envregistry`'s `ManifestEntry` plus the strictness that
  makes it real; a **status capability** contract generalized from
  `integration.email.status`; a server-side **set** builtin per the
  `providerKeySet` pattern; and the **Integrations** section in the OS
  Settings app (`clients/os/src/apps/settings/`), gated
  owner-or-developer. Runbook updates to
  [env-vars.md](../../public/operate/env-vars.md).
- **The wave this belongs to:** Phase 2 of the email-campaigns program,
  deliberately BEFORE the Campaigns app so that app ships with the
  configure-from-Settings story rather than retrofitting it. Two tasks,
  two PRs -- **T6** (#4825, engine) and **T7** (#4826, OS) -- stated on
  the epic and on both task issues.
- **Follow-ups noted, not built here:** per-partition configuration (the
  scope exists in the loader and has zero entries today); a rotation
  scheduler; configuration history or an audit timeline per key
  (`v1:identity:auditEvent` records the write, and a per-key timeline is
  a different feature); importing a whole `.env` from the browser;
  configuring the boot envelope from anywhere (C4 is a permanent no).

## Why

A fresh cluster boots with nothing configured, and today the only way to
change that is an operator with a shell, a `--env-file`, and
`MEMQL_MASTER_KEY`. Everything needed to do better already exists and is
scattered: `integrations/email/lazy.go` resolves credentials from
`v1:platform:globalVariable` / `globalSecret` rows at use time,
`integration.email.status` reports slot by slot what resolved and from
where, `component/memql/provider_config_write.go` seals a vendor key
server-side and writes it, and the env registry knows every variable's
name, component and default. None of it is a MODEL, so each integration
hand-rolls its own version and they disagree.

They disagree in ways an operator cannot predict. `integrations/email`
resolves **env first, then rows**; `integrations/release` resolves
**globalSecret, then globalVariable, then env** -- the exact opposite
order, in the same binary. So "I set the row and it did not take effect"
and "I set the row and it overrode my env" are both true statements about
this cluster depending on which integration you were configuring, and
nothing anywhere says which you were in.

The second reason is that a feature which is merely unconfigured must
never look broken. The engine already refuses honestly at the campaigns
preflight -- "no email sender is registered on this node" -- and the
Settings surface is what turns that sentence into an action.

## Locked decisions

| # | Decision | Choice |
|---|---|---|
| C1 | A config manifest per integration | Each configurable integration DECLARES its configuration: for every key, the env-var name, whether it is a secret or a plain variable, which legacy alias is still accepted, what functionality it unlocks, and which keys form a **lane** that must resolve together. The manifest is the single source for the resolver, the status report and the Settings form. Today those three are three hand-written copies inside `integrations/email` alone |
| C2 | One resolution ladder, and it is env-first | **env -> `globalVariable` / `globalSecret` -> the integration's disabled state.** Env wins because it is the production posture: a cluster whose credentials come from Kubernetes Secrets must not have them overridden by a row somebody wrote in a browser. The email ladder is therefore the model and `integrations/release`'s inverted order is the defect; converging it is part of T6 |
| C3 | A lane resolves WHOLESALE from one source | A lane split across env and rows does not resolve. `integrations/email` already enforces this (`laneComplete`, with a test named for it) and it is the property that stops a half-migrated credential producing a client id from Kubernetes and a client secret from a row nobody remembers writing. The status report says which source a lane came from, singular |
| C4 | The boot envelope is DECLARED, not inferred | `component/envregistry`'s `ManifestEntry` gains one key -- `runtime: bootEnvelope \| configurable` -- and the Settings surface reads it. It is NOT inferred from `scope`, which is a storage-locus axis (`node` / `global` / `partition`) rather than a mutability one and today marks 464 of 513 entries `node`. Inferring would either hide configurable keys or offer fields that cannot work; the env-vars doc's own criterion -- "putting any of them in a concept would be circular, the process can't reach the concept without them" -- is a JUDGEMENT, and a judgement belongs in the manifest where a reviewer sees it |
| C5 | The new key must be enforced or it is decoration | `LoadManifestFromBytes` calls plain `yaml.Unmarshal` with no `KnownFields(true)`, and the manifest already carries three `secret: true` entries that are **silently discarded** because no such field exists on `ManifestEntry`. T6 declares the field AND makes the decoder strict, or the new key joins them. Strictness is the change that turns "I set it and nothing happened" into a parse error |
| C6 | Status is one capability with a stable contract | Every configurable integration registers a `status` capability returning the shape `integration.email.status` already returns: `Registered`, `Capabilities`, `Configured`, `Health`, `Detail`, `Mode`, `Settings[]`, `Credentials[]`. The two axes stay two axes (C7). Integrations that publish none are listed as unknown with the sentence they already get, so the section shows every plug-in the binary registered rather than only the ones that answer |
| C7 | Configured and healthy are two questions | `Configured ∈ {yes, no, unknown}` is answerable with NO network call; `Health ∈ {unknown, healthy, unhealthy, degraded}` needs a round trip and is only ever `unknown` until an explicit probe runs. The operator-facing headline -- **needs configuration / configured / unhealthy** -- is DERIVED from the pair at the card, never stored as a third field. Collapsing them at the source is what would let a card claim "healthy" from a value that has never been used |
| C8 | Probing is an explicit act | `probe=true` runs the existing non-sending reachability check (Graph: acquire a client-credentials token; SMTP: connect, EHLO, STARTTLS, AUTH, QUIT) under a 10s timeout, with the provider's own words redacted of any credential before they are shown. A page that probed on render would hammer a vendor on every navigation and would make a card's freshness unknowable. The card carries a fetched-at stamp and a Refresh, the same downgrade the Cluster section's registry panels already make |
| C9 | The gate is owner-or-developer, and admin is deliberately outside it | `developer` is first-class in `component/auth/rbac.go` and denotes ENGINEERING power -- `CanAuthor` and `CanRunInline` admit owner and developer ALONE -- while `admin`'s capability set is `create`/`principal`, user administration. Wiring up what the cluster talks to is the first kind. `RoleLevel` already says these are the same rung (admin and developer both `1`); `roleRank` obscures it (developer 300, admin 200). So the requirement is a SET, not a floor, on both sides of the wire |
| C10 | Secrets are write-only, with no read-back anywhere | The UI writes a secret and can never display one. This is not a UI convention: `v1:platform:globalSecret` has **no query construct at all**, `setGlobalSecret` takes already-sealed `encryptedValue` + `fingerprint`, and the sealing needs `MEMQL_MASTER_KEY`, which exists on nodes and must never exist in a browser. So the write is a server-side builtin that seals, exactly as `providerKeySet` does, and the only feedback a caller gets is presence, source and fingerprint |
| C11 | The key NAME is not a caller parameter | `providerKeySet` maps a `vendor` argument through a closed table to the row name and refuses anything else -- so the builtin cannot be used to write an arbitrary row into the secret store. The generalized setter keys on `(integration, slot)` resolved through the config manifest (C1), which is the same property expressed over a bigger table. A setter taking a free `name` would be a general-purpose secret-store write with a role gate in front of it, which is a different and much larger thing to review |
| C12 | Unconfigured never breaks boot | An integration with no credentials registers, reports `configured=no`, and refuses at USE with its own sentence. The one place that is deliberately not true stays as it is: `MEMQL_EMAIL_ALLOW_LOG_ONLY` / `DeliveryRequired()` refuses boot on a non-local domain rather than letting mail fail upward (memql#4477). That is a decision about a specific silent-success bug, not a template |

## A. The config manifest

Today's ladder lives three times inside one integration: once in
`LazySender.resolve` (the real one, behind a `sync.Once`), once in
`status.go`'s `describer`, which REPRODUCES it rather than calling it
precisely because the real one caches, and once in the slot tables that
name the env vars. Adding a key means editing all three, and the status
report drifting from the resolver is a report that lies about the
running process.

The manifest replaces the two copies. Per integration:

| Field | Meaning |
|---|---|
| `slot` | stable id, e.g. `graph.clientSecret` |
| `envVar` | the primary env name |
| `legacyEnvVars` | names still accepted, in order (`AZURE_CLIENT_SECRET` and its four siblings) |
| `secret` | true -> `globalSecret` and never rendered; false -> `globalVariable` and rendered |
| `lane` | which mutually-exclusive configuration this slot belongs to (`graph`, `smtp`) |
| `requiredInLane` | whether the lane is incomplete without it |
| `purpose` | one sentence, shown beside the field |
| `default` | for a variable with a sensible one (`SMTP_PORT` is `587`) |

The resolver walks lanes in declared order, and within a lane resolves
every slot from ONE source (C3) before accepting it. The status report is
the same walk with the values withheld. One walk, two consumers, and the
Settings form is generated from the same declaration -- so a new key is a
manifest entry rather than a change in three files plus a React component.

**The `Editable` rule already exists and is kept:** a setting whose source
is `env` is not editable from the UI, because a row would not win (C2).
The card says so in place of the field rather than accepting a value the
ladder will ignore -- which is the shape of the "I set it and nothing
happened" complaint this whole phase exists to end.

## B. The boot-envelope boundary

`env-vars.md` already splits the world: a **bootstrap envelope** read at
process startup, where "putting any of them in a concept would be
circular", and **concept-stored config**, whose authoritative list is
`scripts/secrets/manifest.yaml`. The split is prose. The registry that
CI gates is a different file, and it has no field that says which side a
variable is on.

C4 adds one: `runtime: bootEnvelope | configurable`. Three consequences:

- **Settings renders only `configurable` keys**, and says explicitly that
  the boot envelope is set in the deployment rather than here -- rather
  than omitting those variables and leaving an operator to wonder whether
  the screen is incomplete. `MEMQL_DATABASE_DSN`, `MEMQL_MASTER_KEY`,
  `MEMQL_IDENTITY_SIGNING_KEY_B64` and the identity URLs are the obvious
  members; the manifest is where each one's membership is now recorded.
- **The gate that already exists gets a second question to ask.**
  `TestNoEnvRegistryDrift` (`cmd/envscan/scan/drift_test.go`) is
  bidirectional -- read-in-code-but-unregistered, and
  registered-but-appears-nowhere. T6 adds: every entry declares `runtime`,
  and no `bootEnvelope` entry is reachable from the setter (C11's table).
- **The embed stays in sync the way it already does.** The authored file
  is `scripts/secrets/manifest.yaml`; `component/envregistry/manifest.yaml`
  is a generated snapshot regenerated by
  `scripts/secrets/sync-embedded-manifest.sh` and gated by
  `TestEmbeddedManifestInSync`. A new key that lands in one and not the
  other is a red build, which is the correct outcome.

C5 is what makes any of this real. The loader currently ignores unknown
keys, and `secret: true` on three entries is the standing proof: it looks
authored, it parses, and it means nothing. Making the decoder strict is a
one-line change with a fan-out -- those three keys must be removed or
promoted in the same PR -- and doing it now is cheaper than discovering
later that `runtime:` was a comment.

## C. Status capabilities

`integration.email.status` is the prototype and its invariants are the
contract every other integration inherits:

- **A credential VALUE never appears in the reply.** The report carries
  `Present`, `Source`, `EnvVar`, `Purpose` and a rotation hint -- never
  the value, never the ciphertext, never a length. The whole reply is
  serialized and swept for planted credential values by a test, which is
  the form this invariant has to take: a rule about what a struct must not
  contain is one a new field breaks silently.
- **Probe output is redacted before it is shown.** A provider's error text
  can quote what was sent.
- **An integration that answers nothing is listed anyway**, as
  `configured=unknown, health=unknown` with the sentence "This integration
  publishes no configuration self-report". A section that showed only the
  integrations with a status capability would tell an operator their
  cluster has one integration.

Two honest gaps this phase closes rather than inherits:

- **The `fingerprint` that is documented and does not exist.** The
  builtin's `@description` and the `dsl/integrations/builtins.memql`
  header both promise "where the secret store already publishes one, its
  non-reversible fingerprint". No such field is on the `Credential`
  struct and nothing populates one. The fingerprint IS stored on the
  `globalSecret` row, and it is exactly what a person needs to confirm a
  rotation took -- so T6 adds the field and the doc becomes true, rather
  than the doc being trimmed to match.
- **The rotation hint names a command that does not exist.**
  `rotateCommand()` returns an invocation of a `secret-set` make target,
  passing `NAME`, `VALUE` and `SCOPE=global`. There is no such target in
  the Makefile, and `scripts/secrets` supports exactly `seed` and `health`
  -- `set` was retired. An operator following the hint gets "No rule to
  make target". The hint becomes "set it here", because after T7 that is
  true. (The repo has a gate for precisely this class of citation, and it
  reads documentation as well as code -- which is how this spec found the
  defect.)
- **The scoping gap stays a gap, and is named.** `registryRollCall` covers
  plug-ins registered through `memql.RegisteredPlugins()`. The
  integrations wired explicitly in `app/integrations_*.go` -- cognition,
  agent, stt -- are outside it and are not listed. That is a real hole in
  the section's coverage and the section says so, rather than presenting a
  partial roll call as a complete one.

## D. The gate

**Server side.** `statusAuthorized` today is an exact-equality check on
`RoleOwner` and `RoleAdmin`, its refusal test exercises no-caller, reader,
writer, admin and owner, and it never exercises `developer` at all. So a
developer is refused today, and nothing records that as a decision. C9
makes it one, in the other direction: the gate becomes the SET
`{owner, developer}` expressed through the capability model rather than
through raw constants, and the refusal test gains the case that is now the
interesting one.

The argument is about kind, not rank. An admin's power in this cluster is
over PRINCIPALS -- `create`/`principal`, which is what `AtLeastAdmin`
resolves to. A developer's is over what the cluster DOES: `CanAuthor` and
`CanRunInline` admit owner and developer and nobody else. Wiring up a mail
transport is the second. The two roles are the same rung of `RoleLevel`
(both `1`) and differ in kind, which is precisely why a floor over a
linear ladder is the wrong instrument -- `AtLeastDeveloper` admits admin
as well, and would hand a user administrator a screen full of credential
fields.

**Client side.** The OS shell has ONE role predicate and it took a `min`
floor. `{ min: "developer" }` is the nearest a floor can come and it
admits admin, so approximating it would draw a section of forms the engine
then refuses one at a time -- a worse answer than not offering the section.
`RoleRequirement` therefore gains a set form, `{ any: [...] }`, with the
ladder remaining the default for everything else: a set that is really a
contiguous top of the ladder is a `min` written the long way and silently
stops admitting whatever rung is added above it.

**The Cluster section is the structural precedent and the wrong role.**
It shows the shape to copy: a role requirement on the SECTION entry, plus
a narrower gate at the CALL for the reads that need it
(`useInfrastructureFacts(access?.clusterRole === "owner")`), and a server
refusal rendered where the panel would be, in the engine's own words --
because the floor for a section and the floor for one read inside it are
genuinely different, and flattening them either hides the section or
promises a panel that always fails. The Integrations section copies all of
that and changes only which roles are admitted. And it inherits the
standing caveat the shell states about itself: presentation gating is UX,
the engine's own gate is the authority, and a section that is drawn is not
a section whose calls will succeed.

## E. Writing a secret

The write path is `providerKeySet` generalized, and every property of that
builtin is deliberate:

1. **Seal server-side.** `secret.Encrypt` reads `MEMQL_MASTER_KEY` and
   produces `(ciphertext, fingerprint)`. `setGlobalSecret` takes the
   sealed pair; a browser could call that mutation today and could not
   produce a valid `encryptedValue`, which is why the builtin exists.
2. **The row name comes from a table, not from the caller** (C11).
3. **Refuse empty rather than storing it.** A blank credential written
   over a working one is an outage with a green result.
4. **Do not reload as a side effect.** `providerKeySet` seeds and returns;
   applying is the separate `providersReload`. The same split holds here:
   an integration resolving lazily picks the new value up on its next
   resolution, and an integration that caches behind a `sync.Once` --
   which the email sender does -- needs an explicit reload, and the card
   says which state it is in. Silently reloading a transport mid-send is
   not a thing to do on a form submit.
5. **Return presence, source and fingerprint. Never the value.**

A plain VARIABLE takes the same path minus the sealing, writing
`setGlobalVariable`. It is rendered afterwards, because it is not a
secret and hiding it would make the form unusable for exactly the values
an operator most wants to check.

**One vocabulary, not two.** `integrations/email` spells the source
`"env" | "globalVariable" | "globalSecret" | "unset"` as Go constants;
`v1:platform:providerConfigResult`'s neighbour `providerAuthStatus`
declares `authSource enum("federation", "globalSecret", "globalVariable",
"env", "unresolved")` in the DSL. Same four tiers, different name for the
empty state, and a Settings card rendering both would have to translate.
T6 adopts the DSL enum -- it is the one a client can be generated against
-- and the email constants become its Go mirror.

**Two more facts the section must respect.** `globalVariable` and
`globalSecret` declare **no `@rowAuthz` tier at all**, so nothing narrows
a read of them today; the gate on this surface is therefore the ONLY thing
between a signed-in reader and the variable table, and it lives in Go on
the builtin rather than in a filter. And the Diagnostics section
deliberately excludes the credential presence map from its copyable report
on the grounds that which slots are filled is reconnaissance -- that stays
true. The presence map appears in a role-gated configuration screen shown
to somebody who may change it, and stays out of a report designed to be
pasted into a ticket. Different audiences, deliberately different exposure.

## F. Refusals, and what the campaigns stack gets

The first consumer is the email/campaigns stack: the Graph lane
(`MEMQL_EMAIL_AZURE_TENANT_ID`, `_CLIENT_ID`, `_CLIENT_SECRET`,
`MEMQL_EMAIL_SENDER`, `MEMQL_EMAIL_FROM_NAME`), the SMTP lane, and the
campaigns keys that are not boot envelope --
`MEMQL_CAMPAIGNS_UNSUBSCRIBE_SECRET` and its `_PREVIOUS`,
`_UNSUBSCRIBE_BASE_URL`, the rate and batch tuning, `_FEEDBACK_SOURCES`,
the warmup ladder. The unsubscribe pair is the interesting case and the
form must say what the runbook says: rotating with one variable breaks
every link already sent, so the card sets both or neither and states the
rotation rule rather than presenting two independent text boxes.

Refusal handling follows the standing rules. A refusal renders in surface,
in the engine's own sentence, beside the control that produced it. An
integration reporting `configured=no` renders as **needs configuration**
with the lane's missing slots named -- not as an error, because nothing has
failed yet. A probe that comes back `unhealthy` renders the provider's own
redacted words, because "Graph rejected these credentials" and "we could
not reach Graph" are different problems and only the provider knows which.
And the Campaigns app's cue points here rather than restating the
diagnosis (SP3, A11).

## G. Testing

- **Manifest strictness:** a config manifest with an unknown key fails to
  load. The regression this covers is the three ignored `secret: true`
  entries, so the test is written against a fixture carrying exactly that
  shape.
- **`runtime:` coverage:** every registry entry declares one, and no
  `bootEnvelope` entry is reachable from the setter's table. Both live in
  the existing env-registry gate rather than in a new one.
- **Ladder equivalence:** the resolver and the status describer are driven
  from the same manifest over the same fixture and must agree slot for
  slot. This is the test that would have caught the two hand-written
  copies drifting, and it is why the manifest is worth having at all.
- **Lane wholeness:** a lane split across env and rows does not resolve and
  is not reported configured. The email suite already has this case named;
  the generalized version parameterizes it.
- **The no-value sweep:** the whole status reply is serialized and searched
  for planted credential values, for every integration that registers a
  status capability -- not only for email.
- **The gate, both sides:** the server refusal test gains `developer`
  (admitted) and `admin` (refused), stated as the assertions they are; the
  shell's role-predicate test covers the set form, including that an empty
  set admits nobody and an unrankable actor role admits only
  requirement-free surfaces.
- **Write path:** the setter refuses an unknown `(integration, slot)`,
  refuses a blank value, seals rather than storing plaintext, and returns
  no value. A round-trip test writes and then reads the STATUS -- there is
  no read-back to assert against, and that absence is the property.
- Verification is `make test` (the module-path form) plus the OS build and
  vitest for T7.

## What landed, and what C4 / C5 still describe

The epic shipped C1, C2, C3, C6, C7, C8, C9, C10, C11 and C12: the declared
manifest (`integrations/email/configmanifest.go`, generic machinery naming
email nowhere), the env-first ladder, wholesale lane resolution, the widened
status report with per-slot reasons, the two axes kept apart, the probe as an
explicit act, the owner-or-developer set on both sides of the wire, the
write-only secret, the slot-not-a-name setter (`integration.email.configure`),
and refuse-at-use rather than refuse-at-boot.

**C4 and C5 did not.** `ManifestEntry` still carries no `runtime` key and
`LoadManifestFromBytes` still calls plain `yaml.Unmarshal`, so the three
`secret: true` entries this spec found are still silently discarded. They are
written here as the design because they are the right design; they are called
out here as unbuilt because a spec that reads as a description of the code is
worse than no spec.

Neither is load-bearing for what shipped, which is why they could be left:
the Settings surface reads `editable` off the STATUS REPORT, where the engine
computes it from the resolved source, rather than re-deriving the boundary
from the registry. What C4 buys over that is a boundary a reviewer can see in
one file instead of a behaviour spread across resolvers.

They were held back rather than rushed for one reason worth recording:
turning the decoder strict is not additive. The three discarded entries become
a parse error the moment it is, and a parse error in the env registry fails
every node's boot validation -- so C5 is a change with its own blast radius
and its own cleanup, and bundling it into a 143-file epic would have made both
harder to review and one of them harder to revert.

## H. Delivery

- **PR 4 -- T6 (#4825), engine:** the config manifest and the shared
  resolver, the `runtime` key plus decoder strictness and the gate
  additions, the generalized status capability with the fingerprint field
  and the corrected rotation hint, the source-vocabulary convergence, the
  setter builtins, and the owner-or-developer gate on both.
- **PR 5 -- T7 (#4826), OS:** the `{ any }` role requirement in the shell's
  one role predicate, and the Integrations section -- a card per
  integration with its derived headline, its settings and credential slots,
  the write forms, Refresh and an explicit Probe, and the boot-envelope
  keys named as out of scope with where they are set instead.

Each PR branches from and targets `main` -- never stacked -- and each
leaves `make test` green on its own.
