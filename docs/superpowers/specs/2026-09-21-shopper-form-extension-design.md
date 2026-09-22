# The shopper-form extension: how a client's own fields reach a client's own concept

- **Date:** 2026-09-21
- **Status:** brainstormed with the owner on 2026-09-21 and **approved**. The
  one question that was the owner's rather than the engine's -- what happens
  when the pack's row is written and the client's is not -- was put to him and
  answered; it is D5 below.
- **What it is:** one new declaration, `@shopperFormExtension`, by which a
  client's DSL domain adds its own fields to a shopper form a pack already
  declares, and names the mutation that stores them.
- **Why it is needed:** the storefront program's design record states, as
  settled law, that "client-specific fields are a related concept, not a blob"
  ([2026-09-20](2026-09-20-shopify-storefront-program-design.md), section 7).
  That sentence is currently unimplementable. There is no path by which a field
  a shopper types reaches a concept the pack does not own, and the fixture that
  is supposed to prove the law passes only because neither fixture client tries.
- **What this record rests on:** every path and claim below was read at
  `origin/main` `1dd03c9fb` on 2026-09-21, not recalled.

## 1. Problem

`memql-fylo`'s wholesale application form collects seventeen fields. The
wholesale pack declares three -- `companyName`, `applicantName`,
`applicantEmail`, "the minimum an application needs to be one"
(`packs/wholesalepack/shopper.go`). The other fourteen include an EIN, which is
the specific field the storefront program's section 7 uses to argue that a pack
must not grow a field per client.

Those fourteen currently go nowhere, and they go nowhere **silently**:

> AN UNDECLARED INPUT IS DROPPED, NOT REFUSED, and the asymmetry is
> deliberate: a browser posts what the page contains, so a hidden CSRF
> token, an honeypot field or a stray input the merchant added would turn
> every submission into a server error if an unknown name were fatal.
>
> -- `component/server/shopper_handler.go`, `shopperArgs`

That reasoning is right and is not what is being changed. The gap is that there
is no way to declare the fields in the first place. `RegisterShopperForm` is a Go
function called from a pack's `Register` (`component/memql/shopper_surface.go`),
and a product repository ships no Go: `memql-fylo`'s own guide opens with "a
single-repo memQL product = a DSL bundle plus one or more client surfaces (no
product Go in the common case)".

**The fixture hides this.** `packs/wholesalepack/testdata/clients/northwind/`
declares `northwindTaxDetail` carrying an `ein`, with an `@relationship` to the
pack's application, exactly as section 7 prescribes. Nothing writes a row of it.
The two-client test (memql#5560) asks whether two clients can express different
**approval processes** without editing the pack, and they can. It does not ask
whether either can collect a field, and neither can.

## 2. What the tree already has

### 2.1 The registry and its refusals

`component/memql/shopper_surface.go` is "THE WHOLE OF WHAT A SHOPPER CAN REACH".
A pack declares forms and reads; everything else on that pack is as unreachable
as it was. Registration is init-time and every refusal is a panic, "because
every caller is a pack's `Register` running before the node serves anything".

It refuses, by name, a declared field called `id`, `storeId`, `siteId` or
`ownerUserId` -- the server-stamped names -- so a shopper who could choose a row
id could not overwrite another shopper's row.

### 2.2 The handler, and the order it does things in

`component/server/shopper_handler.go` resolves the declaration first, "because
everything after it can be answered with a redirect to a page the declaration
names, and a refusal before it cannot". Then: the stamp, the site, the body cap,
`ParseForm`, `shopperArgs` against the declared fields, and then

```go
args["storeId"] = stamp.storeID
args["siteId"] = site.ID
```

"STAMPED LAST so nothing a caller sent can reach these keys, whatever the
declaration said."

### 2.3 A receipt is not a row, and this is the obstacle

`wholesaleSubmitApplication` writes through `createApplicationRow` **without
naming an id** and answers with `confirm(...)`, which is documented at length as

> A RECEIPT, NOT THE ROW. [...] These two fields exist to identify the
> ANSWER, not a row. They deliberately do not take the "v1:" form a stored id
> carries, because a reader who saw one would reasonably go looking for the row
> it names.
>
> -- `packs/wholesalepack/reader.go`

So the application's row id is derived by the engine and is known to nobody. An
extension row has nothing to point at. Section 4 resolves this without
weakening the sentence above.

### 2.4 The four controls that already stand in front of a form

Declared in `shopper_surface.go` and deliberately not this registry's job: the
site's own `shopperForms` switch (off by default), the per-address per-site rate
limit, the body size cap, and the `servedButNotExternallyRouted` classification
that makes the edge the only way in.

## 3. Decisions

### D1 -- An extension attaches to an existing route; it never opens one

The obvious alternative was to let a DSL domain declare a shopper **form** of
its own, so a client could name any route and any construct. It was rejected on
the registry's own stated grounds:

> Declaring a form puts an endpoint on a hosted site's own origin that
> anybody on the internet may post to.

Today the population that can do that is "Go compiled into the engine". Route
declaration would widen it to "anyone who can author DSL", which is every
product developer on every cluster. An extension widens nothing: it adds fields
to a route a pack already opened, and inherits that pack's gate, rate limit,
size cap and redirect pages rather than re-implementing them.

The difference is load-bearing for the wholesale case in particular. The pack's
form is a builtin rather than a mutation precisely because an application has a
precondition -- `wholesaleSettings.applicationsOpen` -- and "a plain POST to the
endpoint would sail past it, which is not a closed door". A client-declared
route would make that gate something a client's logic must remember to call. An
extension cannot skip it: the pack's construct runs first, and a refusal there
ends the request.

### D2 -- The extension target is a mutation, never a logic

A `logic` may call builtins. Pointing a public form at one would put arbitrary
builtin calls within reach of the internet under the site owner's borrowed
authority. A mutation writes one concept, and that is the whole blast radius.

A client needing two rows from one submission does not get it here. That is
YAGNI held deliberately: the case does not exist yet, and the narrower rule can
be widened later without breaking anything, where the reverse is not true.

### D3 -- At most one extension per (pack, form)

A second extension on one route is a load refusal naming both domains, in the
manner of the policy and rule registries, which "REFUSE a duplicate name across
the whole corpus rather than last-wins". Ordering between two extensions would
be resolved by nothing, and two clients extending one route is not a case the
design has: a route belongs to a store, a store belongs to one merchant.

### D4 -- The bff mints one submission id per POST and stamps it into both

The id joins `storeId` and `siteId` at the same point in the handler and for the
same reason. The pack's construct uses it as the row id it creates; the
extension references it.

This is why 2.3 is not weakened: `confirm` still returns a receipt and still
carries no row id. The id does not travel **back** from the pack's construct; it
travels **down** into both from the handler, which is the one place that can
know it applies to one submission.

**It changes the pack directories, and it is not a client field.** Both packs'
shopper constructs take a stamped id instead of letting the engine derive one.
A reviewer checking memql-fylo#33's acceptance ("the pack's directory in the
engine is untouched by this client's fields") should read this paragraph: no
client field enters a pack, and the id is the engine's, not Fylo's.

A second property falls out for free: a review or an application now has an id
that is a fact about the submission rather than an engine-derived surprise,
which is what makes the extension's `@relationship` expressible at all.

### D5 -- A failed extension write leaves the application standing (the owner's answer)

Both field sets are validated **before anything is written**, so a declared
field that fails its own rule refuses the submission with no row of either kind.
The only reachable failure after that is transient.

When it happens: the pack's row stands, the shopper is sent to the pack's
success page, and the engine records an Error log carrying the submission id
plus a `memql_shopper_extension_write_failed_total{pack,form}` counter.

The owner chose this over refusing the submission and over compensating. The
reasoning he was given and accepted: the trade desk gets an application it can
act on with its client fields visibly blank, and a person emails the applicant
for their EIN. The alternative sends someone who has typed seventeen fields to
an error page, from which they resubmit -- and the pack has no dedupe on
applications, so one business becomes two rows, one of them with fields and one
without. That is a worse outcome from the same fault.

It also matches the pack's existing stance one level up: when entitlement
provisioning fails, "the application STAYS APPROVED -- an approval is a decision
somebody made, and a push that failed is not a reason to undo it".

### D6 -- An extension may declare required fields

A field marked `@required` in the extension refuses a submission the pack alone
would have accepted, before any write. The form on the page is the client's, and
the client decides what is mandatory; without this, a direct POST could create
an application carrying none of the data the client's own process needs.

### D7 -- The field list is the args block minus the stamped names

A Go pack declares its mutation's args and its `ShopperForm.Fields` separately
and they must agree -- `submitReview` declares `storeId` and `siteId` in args
while its `Fields` omit them. For a DSL extension the two are derived from one
declaration instead of restated, which removes the class of bug where they
drift. The existing refusals apply unchanged to what remains.

## 4. The design

### 4.1 The declaration

```memql
use wholesale.concepts.{ application }

@shopperFormExtension(pack="wholesale", form="application")
mutation applicationDetail recordFyloApplicationDetail {
  args {
    submissionId  string!                          // stamped; not a form field
    storeId       string!                          // stamped
    siteId        string                           // stamped
    businessType  string!  @enum("Dental Practice", "Retail Store")
    address       string!  @maxLength(200)
    ein           string   @maxLength(20)
  }
  insert {
    accept { businessType, address, ein }
    stamp {
      id:            args.submissionId
      applicationId: args.submissionId
      storeId:       args.storeId
      siteId:        args.siteId
      ownerUserId:   actor.userId
    }
  }
}
```

`@required` / `@maxLength` / `@enum` / `@minimum` / `@maximum` already mean what
`ShopperField` needs, so the extension introduces no second vocabulary. An `int`
arg maps to `ShopperField.Numeric`, which is what makes a form's `"5"` reach a
`@minimum` as a number.

### 4.2 Registration

A pack's declaration is init-time and panics. An extension's is **load-time**,
because a DSL domain -- embedded or mounted at `MEMQL_DSL_PATH` -- loads long
after `init`. So it registers during `MemQLEngine.Init`, lands on the
`LoadReport`, and is refused by strict boot, which is where every other contract
gate already lives and is the tier that reaches a product bundle no Go test in
this repository walks.

The extension registry is rebuilt every time the DSL tree is loaded; the pack
registry is written once at `init` and never again. That asymmetry is why they
are two registries rather than one entry type in the existing one.

Load refusals, each naming the domain and the construct:

| | Refusal |
|---|---|
| 1 | the named `(pack, form)` is not a declared shopper form |
| 2 | a second extension on that route (D3), naming both domains |
| 3 | the target is not a mutation (D2) |
| 4 | a field carries a server-stamped name (`id`, `ownerUserId`) |
| 5 | the declaring domain IS the pack being extended -- a pack adding a field to its own form declares it, rather than extending itself |

Refusal 1 has a consequence worth stating: a pack that is **disabled** declares
no forms, so an extension naming its route refuses the load. That is correct
rather than awkward -- an extension of a route nobody serves is a declaration
whose author believes something false -- and the remedy is the `packState` row
the operator already controls.

### 4.3 The handler

```
POST /_memql/forms/{pack}/{form}
  1. resolve the pack's form declaration        -- 404 if absent (unchanged)
  2. resolve the extension for (pack, form)     -- may be nil
  3. stamp, site, body cap, ParseForm           -- unchanged
  4. validate the pack's fields                 -- refuse: invalid, nothing written
  5. validate the extension's fields            -- refuse: invalid, nothing written
  6. mint submissionId
  7. stamp {storeId, siteId, submissionId} -> run the pack's construct
        refused -> redirect error, nothing further runs
  8. stamp {storeId, siteId, submissionId} -> run the extension's mutation
        refused -> log + counter, and CONTINUE (D5)
  9. redirect OK
```

Step 5 before step 7 is the whole of D5's "nothing is written if either fails".

Both constructs run under the same borrowed authority the pack's already does --
`auth.ContextWithUserActor(ctx, stamp.owner)`, never internal origin, so every
`@serverOnly` construct stays out of reach of a public form.

### 4.4 What does not change

- No new path, prefix or route. `/_memql/forms/{pack}/{name}` is the whole
  surface and an extension adds no member to it.
- The four controls of 2.4, untouched.
- The read path. An extension is write-only; there is no extension of a read,
  because a public read of a client's own fields is a decision nobody has made
  and `wholesalepack/shopper.go` already argues at length against one.
- `shopperArgs`' drop-the-undeclared rule, for exactly its stated reason.
- A route with no extension behaves byte-identically to today, which is a test.

## 5. Testing

The gate is `packs/wholesalepack/twoclients_test.go`, extended to ask the
question it does not currently ask. `northwind` gains an extension carrying its
EIN; `contoso` declares none and keeps its two-automation chain; the test fails
if either needs a change under `packs/wholesalepack/dsl/`.

Beside it:

- **A bad extension field writes nothing at all** -- no application row. This is
  the test that D5 rests on, and the one whose absence would make "validated up
  front" a comment rather than a property.
- An extension write failure leaves the application and still redirects OK, and
  raises the counter.
- A form-supplied `submissionId`, `storeId` or `siteId` cannot win.
- Each of the five load refusals, by code, including the disabled-pack case.
- With no extension declared, the handler's behaviour is unchanged.
- `memql-fylo`'s domain is green under `scripts/dev/check-package.go` and
  `scripts/dev/edition-gate.sh`.

## 6. What this unblocks

`memql-fylo#33` ("the client's own fields in the product's own concept") and,
through it, `memql-fylo#34`. It also makes the storefront program's section 7
true rather than aspirational: after this, the two-client fixture proves both
halves of its own claim -- a different process, and a different field.
