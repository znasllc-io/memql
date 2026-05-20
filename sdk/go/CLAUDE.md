# memQL Go SDK

**Purpose:** The canonical client surface for memQL. Every consumer
(memql-cockpit, future thick clients, future thin clients) goes
through this package. No bespoke wire wrappers in the consumer.

**Language:** Go
**Mirrors:** `sdk/ts/` (TS implementation tracked in #116; spec at
`sdk/ts/README.md`)

---

## The rules

### 1. Named primitives only -- no raw DSL strings

Consumers call typed generated methods on `QueryClient` -- never
inline a MemQL string. The generated tree (`generated_queries.go`,
`generated_mutations.go`, `generated_logics.go`) is the contract.

```go
// WRONG -- raw DSL inlined in client code:
res, err := qc.Execute(ctx, `queryActiveSpaces({})`)

// WRONG -- a different kind of raw:
res, err := qc.Execute(ctx, `concept==v1:cognition:space; payload.status=="active"`)

// RIGHT -- typed primitive generated from the DSL:
res, err := qc.QueryActiveSpaces(ctx, client.QueryActiveSpacesArgs{})
```

The engine reserves the right to evolve internal projection / bundle
shapes without breaking clients. That only works if clients stay on
the named-primitive surface. Raw concept queries are how
memql-cockpit#49 happened -- a bundle shape change silently nulled
out the cockpit's space list. The fix wasn't a one-off patch; it was
the architectural commitment that this file documents.

The lone exception: the concept-browser surface in admin / debug
tools (the cockpit Concepts tab specifically) reaches `BrowseConcept`
/ `GetRowByConceptAndId`. Those are SDK-owned methods, not raw-string
escape hatches. They exist precisely because a concept-agnostic
browser has no compile-time named primitive to call -- and they sit
on the SDK side of the wire so the consumer code path stays uniform.

### 2. Opaque types -- no `memqlv1` imports in consumers

The SDK exposes its own types (`Row`, `Result`, `Event`, etc.). Raw
`memqlv1.*` protobuf types do not appear in the public surface.
Internal SDK code does the proto<->SDK translation in one place.

If you find yourself importing
`github.com/znasllc-io/memql/component/grpc/gen` from a consumer,
that's the SDK leaking and the right fix is to add a wrapper type to
the SDK -- not to import the proto package downstream.

Concrete gaps tracked in #115 (proto-leak sweep).

### 3. Generated code is read-only

`generated_queries.go`, `generated_mutations.go`,
`generated_logics.go` are produced by `scripts/sdk-gen` from the DSL
tree. Don't hand-edit. CI gate: `make sdk-gen-check` regenerates and
fails on drift. After any DSL change to a construct's args /
signature, run `make sdk-gen` locally and commit the result.

### 4. The TS SDK mirrors the Go SDK

`sdk/ts/` ships from the same generator (the generator's TS output
target is tracked in #116). One logical surface, two language
implementations. Every Go method has a TS equivalent; idioms diverge
only where the host language demands it (`context.Context` ->
`AbortSignal`, `<-chan` -> `AsyncIterable`, etc.).

---

## Layout

```
sdk/go/
  client/                  Connection, Dispatcher, QueryClient, SubscriptionManager
    queries.go            ListConcepts, GetMyAccess + the unexported executeRaw
    generated_queries.go  Generated typed query methods (DO NOT EDIT)
    generated_mutations.go Generated typed mutation methods (DO NOT EDIT)
    generated_logics.go   Generated typed logic methods (DO NOT EDIT)
    concept_browser.go    Admin-surface BrowseConcept / GetRowByConceptAndId
    support.go            Result type + executeNamed + renderMemQLValue
  voice/                  Push-to-talk transcription
  sense/                  Tokenize / Diagnose / Complete / Hover / SignatureHelp
  worker/                 (Pending #117) WorkerService client
```

---

## Adding a new method

Don't. Add it to the DSL tree (`dsl/<domain>/queries.memql`,
`dsl/<domain>/mutations.memql`, etc.), then run `make sdk-gen`. The
typed method appears in `generated_<kind>s.go`.

If the operation has no natural place in the DSL (admin tool,
debugging affordance, etc.), write it as a hand-rolled SDK method
parallel to `concept_browser.go`. Document the carve-out at the
top of the file -- silence rots into "raw queries are OK now" the
moment someone copies the pattern without context.
