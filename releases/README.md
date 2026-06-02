# Release lockfiles — atomic, digest-pinned promotion (Phase 4, #702)

A **release lockfile** (`releases/<version>.yaml`) pins all **8 components**
(6 engine node-types + the `memql-bff-copresent` carrier + the `copresent` SPA)
by `@sha256:` digest. It is the **atomic unit of promotion**:

```
   per-repo CI builds + emits digest
            │  (memql x6, memql-bff-copresent, copresent)
            ▼
   assemble-lockfile.sh  ──► releases/<version>.yaml  ──► PR ──► coherence gate
            │                                                     (CI)
            ▼  validated (deep gate, Phase 3)
   promote.sh --env=prod  ──► deploy/k8s/overlays/prod (DIGEST COPY, no rebuild)
            │                                                     ──► PR
            ▼
   Argo CD reconciles prod  (Phase 2)
```

**Promotion is a digest copy — never a rebuild.** Prod runs the exact bytes
staging validated; environments differ only by config. A digest can only reach
prod if it's in a lockfile, and a lockfile only merges if the **coherence gate**
passes — so a tag can never drift from what was validated (#684 at the release
level).

## The contract

| Field | Meaning |
|---|---|
| `version` | The release version. |
| `engineVersion` | The memQL engine version the carrier + SPA were built against. |
| `validatedAt` / `gate` | Stamped when the deep staging gate (Phase 3 #701) passes. |
| `components.<name>.digest` | `sha256:…` — the only image authority. |
| `components.{memql-bff-copresent,copresent}.builtAgainstEngine` | **Must equal `engineVersion`** — cross-repo coherence. |

## Tooling (`scripts/release/`)

| Script | What |
|---|---|
| `coherence-check.sh <file>` / `--all` | The CI gate: 8 components present, all digest-pinned, carrier+SPA `builtAgainstEngine == engineVersion`. |
| `assemble-lockfile.sh --version=X --engine-version=Y` | Write a lockfile from per-component digests (env `DIGEST_<comp>` or `--<comp>=sha256:…`). Each repo's CI exports its digest; the assembly step collects them. |
| `promote.sh --version=X --env=prod` | Digest-copy a validated lockfile into the env overlay's `images:` block. Refuses an incoherent lockfile. |

## Per-repo CI digest emission (the contract each repo implements)

Each repo's image-build CI publishes the resulting `@sha256:` digest as a build
output, and the assembly step (the `release-lockfile` workflow, or an operator)
collects the 8 into a lockfile PR:

- **`znasllc-io/memql`** — the 6 engine node images (per `BUILD_TAGS`).
- **`visionarys-io/memql-bff-copresent`** — the carrier, tagged with the engine
  version it built against (→ `builtAgainstEngine`).
- **`visionarys-io/copresent`** — the SPA, likewise.

> Wiring the actual image-build-in-CI + digest export in all three repos
> (incl. ACR OIDC push) is the remaining build-out for #702; this PR establishes
> the lockfile format, the coherence gate, and the promotion tooling the CI
> feeds into. Until that lands, `assemble-lockfile.sh` is run with digests from
> `az acr build` / the live cluster.

## Relationship to `deploy/validated-versions.json`

The lockfile **supersedes** the old `deploy/validated-versions.json` ledger
(same intent — a validated version + per-engine digests — but written by the
retired imperative script). The ledger's semantics now live here + in git
history; `validated-versions.json` is kept only as a historical record.
