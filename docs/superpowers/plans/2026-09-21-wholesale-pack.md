# The wholesale pack — implementation plan (epic memql#5533)

> **Deleted in this epic's merge.** The plan is a working document for the
> session that picks the epic up; what survives is the code, its tests, and
> `docs/public/build/consuming-the-wholesale-pack.md`.

**Goal:** an application, a decision and an entitlement any client can lay its
own process over, with Shopify's native B2B as one adapter.

**Spec:** `docs/superpowers/specs/2026-09-20-shopify-storefront-program-design.md`
— section 7 (the line and its test), D4 (plan-independent entitlement), D9
(`storeId` on every shopper-written row), D10 (the store row's tier), G7 (the
gap this closes), section 11 (testing).

**Shipped as ONE PR**, not the record's two. The owner asked for a single PR
across all seven issues; the record's PR split was a sequencing convenience,
not a design constraint, and nothing in it is load-bearing.

## Global constraints

- **DSL 1.0, edition 2026.** `memql.toml` in every `.memql` directory.
- **No emojis** in code, comments, commits or docs.
- **The three extension words.** component / integration / pack. Never "plugin".
- **The storefront-pack convention:** concepts, mutations, queries, shapes,
  tools and builtins — **no automations and no logic**. `reviewspack` keeps it;
  `deploypack` and `referencepack` do not, so it is a choice the storefront
  packs made rather than a platform rule.
- **`packs/reviewspack` is the template** for layout, registration, the
  `rowReader` seam, and the commentary register.
- **Every row carries its `storeId`** (D9), stamped server-side, refused as a
  form field.
- **`v1:shopify:store` stays `@rowAuthz(clusterOwner)`** (D10). Nothing here
  widens it.

---

## Decisions this plan records

### D-a. The plan-independent adapter is the customer TAG, not draft orders

Issue memql#5559 requires the choice be weighed and recorded. Candidates:
a customer tag that price rules key on ("cheap, and leaks if the tag is
guessable"), or draft orders priced in MemQL ("puts every wholesale order
through a person").

**Chosen: the tag** — and the argument is not cost. Issue memql#5557 fixes what
an adapter must do: *"a provisioning failure leaves the application approved and
the entitlement pending with its error."* An adapter is called at approval time
and must report success or failure **then**. The tag adapter can. Draft orders
cannot: nothing is provisioned until an order happens, so the entitlement would
sit `pending` for ever — and `pending` already means "the push has not landed",
so every wholesale buyer would look like a failed grant. Draft orders are an
order-*placement* mechanism; they belong to the client's process, and
`v1:commerce:quote` plus `CreateDraftOrderFromQuote` already carry them.

**The leak is answered rather than accepted.** A shopper cannot assign
themselves a tag — MemQL writes it under the store's own credentials. What
leaks is a discount **code**. So the adapter states the constraint it cannot
enforce, in the code, on the row and in the guide: the discount keyed on the tag
must be an *automatic* discount conditioned on the customer, never a code.

### D-b. An application's state is DERIVED, never stored

`reviewspack`'s precedent exactly: `review` carries no state, `moderationAction`
rows are append-only. The application carries no mutable state field; the
decision carries the transition; `wholesaleApplicationState` folds the log.
"Never an overwrite" is then true by storage shape rather than by a rule
somebody must keep — and *why* an application was refused survives the next
decision.

### D-c. One entitlement per application, at a derived id

`wholesale:entitlement:<applicationId>`, as `reviewspack.SettingsRowID` derives
one settings row per store. A re-provision is a new **version** of one logical
row, and "revoked when" is that version's own `createdAt`.

### D-d. An adapter may only CALL CONSTRUCTS

The pack must not import `integrations/shopify` — that is the three-word rule,
and a pack that imported an integration would be a pack that only works on
Shopify. So `Adapter` is handed a `Caller` (one named construct, its arguments)
and nothing else. The Shopify adapters reach the connector through `@executor`
builtin **names**.

### D-e. The submit path is a BUILTIN, not a mutation

`reviewspack`'s form names a mutation because a review has no precondition. An
application has one — `applicationsOpen` — and a mutation body cannot read
another concept's row. `ShopperForm.Kind` exists for exactly this case.

---

## Tasks

### Task 1 — The application, its states and a settings row (issue memql#5555)

- Create `packs/wholesalepack/dsl/{memql.toml,namespace.pin,concepts.memql,shapes.memql,queries.memql}`.
- Four concepts: `application`, `applicationDecision`, `entitlement`,
  `wholesaleSettings`. The application's minimum is `companyName`,
  `applicantName`, `applicantEmail` + stamped `storeId`/`siteId`. **No EIN, no
  metadata blob.**
- One narrow cross-domain shape `wholesaleStorePlan` over
  `shopify.overlay.concepts.store` — `row.id`, `plan`, `isDevelopment` and
  nothing else, so the pack can never hold a credential reference.
- `packs/wholesalepack/{pack.go,reader.go,shopper.go}`.
- Test: the pack loads, its tree parses, it ships no automations/logic, it
  declares no client-specific field, it ships disabled.

### Task 2 — The transitions and their invariants (issue memql#5556)

- `packs/wholesalepack/application.go`: `ClientMayDecide`, `ValidTransition`,
  `LegalTransition`, `FoldState`, and the capabilities `submitApplication`,
  `recordDecision`, `applicationState`, `setWholesaleSettings`.
- Transition table: approve/reject legal from submitted; revoke only from
  approved; nothing out of rejected or revoked.
- Test: the fold, the table, the Provider refusal, the unreadable-application
  refusal, `storeId` copied off the subject, the per-store gate, the minimum.

### Task 3 — The entitlement seam (issue memql#5557)

- `packs/wholesalepack/entitlement.go`: `Grant`, `Outcome`, `Caller`, `Adapter`,
  the registry, `AdapterRefusal`, `engineCaller`, and the provision/revoke flow.
- Pending vs failed: a transient error is `pending`; an `AdapterRefusal` is
  `failed`. The row is written either way and the error is **not** returned.
- Revoke goes through the adapter recorded on the ROW, not the one settings name.

### Task 4 — The native-B2B adapter (issue memql#5558)

- `integrations/shopify/{wholesale.go,wholesale_calls.go,wholesale_capabilities.go}`:
  company + location + contact + catalog + price list, idempotent by
  `externalId`, ceiling read **before** anything is created, and **nothing ever
  deleted** — revoke sets the catalog to `DRAFT`.
- `dsl/shopify/overlay/builtins.memql`: five new builtins.
- `packs/wholesalepack/adapter_shopify_b2b.go`: `Available` refuses an unknown
  plan; **no tier gate** — the ceiling is a catalog count, not a plan.

### Task 5 — The plan-independent adapter (issue memql#5559)

- `packs/wholesalepack/adapter_customer_tag.go`, carrying D-a's argument in full.
- `Available` returns nil unconditionally — that function **is**
  plan-independence.
- The revoke removes the tag read off the reference, not the configured one.

### Task 6 — The design's test (issue memql#5560)

- `packs/wholesalepack/testdata/clients/{northwind,contoso}/` — two real fixture
  domains with genuinely different processes; one collects an EIN as its own
  concept with an `@relationship`, the other collects none and routes through
  `v1:commerce:approvalChain` naming a `v1:commerce:salesRep`.
- `twoclients_test.go`: both parse, both load beside the pack, the pack's tree
  is **fingerprinted before and after** and must be unchanged, the two processes
  must actually differ, and both fixtures must reach only declared surface.
- **Prove the gate can refuse** before trusting it.

### Task 7 — The adoption guide (issue memql#5561)

- `docs/public/build/consuming-the-wholesale-pack.md`.
- Written against the two fixtures, because memql-fylo#33 and #34 are still
  open — so the worked examples are ones CI executes rather than prose nobody
  has run. Say so in the page.

### Task 8 — Into the default build

- `app/anchor_storefront_packs.go`: register beside `reviewspack`, no build tag.
- `make concept-snapshot`.
- `embed_inventory_test.go`: the pack's measured embed count.
