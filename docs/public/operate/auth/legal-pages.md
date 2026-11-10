---
title: Legal pages -- the Terms of Service and Privacy Notice an installation shows
audience: public
status: stable
area: operate
sinceVersion: 0.31.28
owner: znas
---

# Legal pages

The sign-in page says "By continuing, you agree to the Terms of Service and
Privacy Notice", and each link opens a document the identity node serves.
This page says what those documents are, how a person reaches them, what
the service records, and how an installation replaces the engine's defaults
with its own.

## What the engine ships

The identity node embeds two documents, `component/identity/web/legal/tos.md`
and `component/identity/web/legal/privacy.md`, written so that any
installation can serve them as they are. They refer to "the operator", the
organization running the installation, whose name is the brand name shown on
the sign-in page. They name no contact address, promise nothing an
installation might not do, and choose the law and courts of Arizona for
disputes (section 14 of the terms). An installation whose operator is
elsewhere, or that wants its own name, contact and subprocessors on the
page, replaces them as described below.

Both documents say that a signed written agreement between a user's
organization and the operator controls over them in a conflict, so an
installation serving customers under contracts does not need to restate
each contract's terms here.

## How a person reaches them

- **From MemQL OS.** The OS sign-in page (`clients/os/src/auth/SignInPage.tsx`)
  opens each document inside the OS: it fetches identity's native
  representation of `GET /legal/tos` or `GET /legal/privacy`, in which `Body`
  is the document's markdown, and renders it under the document's title.
  The edge proxies that fetch on the OS origin
  (`component/edge/identity_proxy.go`), so browsers that refuse credentialed
  requests to a sibling host still get it.
- **From identity's own host.** A browser that navigates to
  `https://identity.<domain>/legal/tos` is sent to
  `https://os.<domain>/identity/legal/tos`, the same page in the OS, like
  every identity UI page (`component/identity/web/native.go`).
- Identity's own HTML template renders the markdown too, with raw HTML in
  the document escaped, for the paths that still serve identity HTML.

## What the service records

Acceptance is by signing in: the sign-in page says so, and each document
shows its version and effective date. The identity service records which
versions a person accepted, once per version.

- **When.** Every sign-in that creates a session (magic link, passkey, device
  code, upstream OIDC) appends to `v1:identity:user.legalAcceptance` one
  entry per document whose version in force differs from the newest entry
  already recorded for that document: `{documentType, version, acceptedAt,
  sourceIP, sessionId}`, the last naming the session that sign-in created. A
  second sign-in on the same version records nothing. A new version is
  recorded at the person's next sign-in. Earlier entries are never edited or
  dropped. Each recording also writes a `legal_acceptance_recorded` audit
  event naming the documents and versions.
- **Where it runs.** `createSessionRow` in `component/identity/http` is the
  one place a session row is created, so every factor is covered by one hook.
  The versions come from the web server, which loaded the documents
  (`LegalDocumentsInForce` in `component/identity/web`), so what a sign-in
  records is what the page shows, override file included. A document whose
  front matter has no `version` is not recorded. The write is the
  `recordLegalAcceptance` mutation, which is server-only: no client can write
  anybody's history, their own included.
- **Across replicas.** Two identity replicas can finish one person's sign-ins
  at the same moment. The recorder holds a per-user Postgres advisory lock
  across its read and write, reads fresh (never from the node's result
  cache), and writes under a row-version fence stamped after the version it
  read. A row that moved in between is read again and only what is still due
  is appended (memql#5431). Tests: `component/identity/legal_acceptance_test.go`
  (the protocol) and `legal_acceptance_db_test.go` (the lock, against
  Postgres).
- **If the write fails.** The sign-in still succeeds. The terms make signing
  in the acceptance, and the record is evidence of it; the next sign-in
  writes the missing version. The failure is logged and audited as
  `legal_acceptance_record_failed`.
- **What a person sees.** Settings in MemQL OS (the identity account pages),
  which is identity's `/me/settings` page, lists the documents, versions and
  dates from the person's own row, newest first. The source IP stays on the
  record and off the page. A history that could not be read says so instead
  of showing as empty.
- **The notice after sign-in.** When a sign-in recorded a version, the
  version in force differed from what the person had accepted, or they had
  accepted nothing yet. MemQL OS says so once, right after that sign-in: the
  account overview (`GET /me/`, native) lists the entries the caller's own
  session recorded, each with the document's title, version and effective
  date and whether an earlier version had been accepted
  (`component/identity/web/legal_notice.go`), and the OS shell shows them as
  one quiet notice with a link to each document
  (`clients/os/src/auth/LegalNoticeBanner.tsx`). It never blocks sign-in:
  the notice is dismissed per browser tab, nothing records that it was
  seen, a sign-in that recorded nothing shows nothing, and a history that
  could not be read shows nothing either. Before a person signs in the page
  does not know who they are, so no notice is shown there.
- **What the record is not.** It is not a prompt to re-accept: the terms say
  that continuing to sign in once a new version is in force is acceptance,
  and the record shows exactly that.

The Privacy Notice describes the sign-in records the service keeps,
including the IP address a sign-in arrives from. An installation that
revises its notice may want to name the acceptance record itself.

## Versions, dates and the 30-day promise

Every document starts with a front-matter block, and an override file must
keep the same three keys:

```markdown
---
documentType: tos
version: 2.0.0
effectiveDate: 2026-11-09
---

These terms are an agreement between you and ...
```

- `documentType` is `tos` or `privacy`.
- `version` is `MAJOR.MINOR.PATCH`. Bump it on every published change; it
  is what a user sees change.
- `effectiveDate` is `YYYY-MM-DD`.

The identity node reads `version` and `effectiveDate` from the block with a
minimal scan (`loadLegalDoc` in `component/identity/web/server.go`), strips
the block, and shows both on the page and in the native representation. A
document with no block, or without a key, shows `unknown` and no date.

The page supplies the document's title, so the file begins with its first
paragraph or section heading, not with a `# Title` line, which would show
the title twice.

Both documents promise notice by email to each user's account address at
least 30 days before a change that materially affects users takes effect,
and a notice in the platform when the new version takes effect. The engine does the platform half, after the fact: at a
person's first sign-in once a new version is in force, MemQL OS tells them
the document changed, to which version and from which date, and that signing
in accepted it ("What the service records" above). The engine sends no email
and shows nothing BEFORE a version takes effect: it serves one document per
type, so it has no next version to announce. The operator gives the 30 days'
notice, by email, and sets `effectiveDate` on the new version to a date at
least 30 days after that notice. A change that only corrects an error or
reduces a user's obligations may take effect when published.

## Replacing the defaults in an installation

Two environment variables on the identity node name the files:

| Variable | File |
|---|---|
| `MEMQL_IDENTITY_TOS_OVERRIDE_PATH` | the Terms of Service markdown |
| `MEMQL_IDENTITY_PRIVACY_OVERRIDE_PATH` | the Privacy Notice markdown |

Each must be a path the identity process can read. The files are read ONCE,
when the identity web server starts, so a changed file needs an identity
restart to show. A path that is set but unreadable falls back to the
embedded default and logs a WARN naming the path:

```text
identity-web: legal override unreadable, serving the embedded default path=/etc/memql/legal/tos.md ...
```

Nothing on the page says a fallback happened, so after a rollout check the
version shown on the page against the override file's front matter, and
grep the identity log for that line.

In an installation repository the shape is a ConfigMap holding the two
files, mounted read-only on the identity Deployment, and the two variables
pointing into the mount. Generate the ConfigMap so its name carries a
content hash: a text change then changes the name, which rolls the identity
pods and picks up the new file. A ConfigMap updated in place under the same
name does not restart anything, and the pods keep serving the old text until
something else restarts them.

```yaml
# kustomization.yaml of the installation's instance overlay
configMapGenerator:
  - name: memql-legal
    files:
      - tos.md=legal/tos.md
      - privacy.md=legal/privacy.md
patches:
  - target: { kind: Deployment, name: identity }
    patch: |
      apiVersion: apps/v1
      kind: Deployment
      metadata:
        name: identity
      spec:
        template:
          spec:
            volumes:
              - name: legal
                configMap:
                  name: memql-legal
            containers:
              - name: identity
                volumeMounts:
                  - name: legal
                    mountPath: /etc/memql/legal
                    readOnly: true
                env:
                  - name: MEMQL_IDENTITY_TOS_OVERRIDE_PATH
                    value: /etc/memql/legal/tos.md
                  - name: MEMQL_IDENTITY_PRIVACY_OVERRIDE_PATH
                    value: /etc/memql/legal/privacy.md
```

Write the files in CommonMark: headings, paragraphs, lists and markdown
links. Raw HTML is escaped on identity's HTML page and ignored by the OS
renderer. What an installation usually changes against the engine default:
the operator's name and contact; the governing law and venue, where the
operator is not in Arizona; the hosting location and email provider the
Privacy Notice should name; and the retention periods the installation
configures, where it states them. Both documents describe AI providers at
category level only ("the AI model providers the operator uses"); keep it
that way rather than naming how the platform reaches a model. Keep the
section that gives
a signed written agreement precedence, and keep the two documents
consistent with each other: the terms point at the Privacy Notice for
personal information.

## Related

- [identity-service.md](identity-service.md) -- the identity node, its
  routes and its environment variables.
- [../env-vars.md](../env-vars.md) -- the environment variable registry the
  two override variables are declared in.
