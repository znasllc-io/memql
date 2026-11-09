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

Acceptance is by signing in: the sign-in page says so, and the page shows
the version in force and its effective date. **Nothing records the accepted
version today.** `v1:identity:user.legalAcceptance` and the
`recordLegalAcceptance` mutation exist, but no code path writes them at
sign-in, and the "Legal acceptance" card on `/me/settings` reads an endpoint
the service does not mount, so it shows an error. The documents are written
to match: they say a person accepts by signing in and do not promise a
re-acceptance prompt. Recording the version at sign-in is a feature to
build, not a setting to turn on.

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

Both documents promise notice in the platform and by email to each user's
account address at least 30 days before a change that materially affects
users takes effect. The engine shows no change notice and sends no such
email on its own today: the operator does both, and sets `effectiveDate`
on the new version to a date at least 30 days after the notice. A change
that only corrects an error or reduces a user's obligations may take
effect when published.

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
