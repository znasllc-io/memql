# Callkey payment and adult-verification adapters

These bounded operations perform provider HTTP requests and authenticate incoming
webhooks. They do not own accounts, authorization, budgets, retry policy, credit
settlement or retention. The calls service must commit an operation and its cost
reservation before dispatch, preserve ambiguous outcomes, and reconcile an
existing operation before attempting another one.

Modes are `disabled`, `mock`, and `sandbox`. Unset means disabled. `mock` performs
no network requests; the calls service implements the local simulated lifecycle.
No production mode is accepted. Stripe requires a test secret key. Every Didit
create request includes `sandbox_scenario`, which Didit rejects for live
applications. Provider HTTP destinations are fixed, redirects are refused,
responses are bounded, and requests have timeouts. Server credentials must never
be sent to the browser or recorded in account rows.

## Stripe contract

Checkout uses USD card payments, a server-supplied amount, account reference,
operation metadata and a durable `Idempotency-Key`. Saving a payment method is
explicit opt-in after the service records consent to the funding mandate. The
adapter requests off-session setup only for that opt-in. It does not infer adult
status or a spending grant from payment.

`ReadCheckout` retrieves the authoritative session and expanded payment intent.
The service must match its stored provider ID, account, operation, amount and
currency before crediting once. A browser success redirect grants nothing.
`ChargeOffSession` creates one fixed-amount payment intent against the saved
customer/method. HTTP 402 authentication challenges retain their intent ID and
return `RequiresAction`; the caller pauses for the owner. Network failures,
HTTP 408/5xx, and unparseable write responses return `ErrOutcomeUnknown` and are
not retried here. Stripe idempotency retention is not an indefinite ledger.

The adapter flags any observed partial/full refund or dispute on the expanded latest charge.
Those flags prevent treating a reversed purchase as wholly usable funds; the service
owns exact refund/dispute amounts and accounting. `Paid` means confirmed provider
payment, not settled bank cash. It must not alone release the separate business
cash-reserve gate.

Webhook verification accepts only a valid `v1` HMAC over timestamp plus exact raw
body, with a five-minute clock window. It returns a minimal object reference;
handlers must deduplicate durable event and payment identities and re-read the
object. Charge refund and dispute events are reduced to their payment-intent
reference, which the service maps to the original checkout or automatic top-up.
Live events are refused. Pin webhook snapshot version to the adapter's
`StripeAPIVersion` (`2026-11-18.endive`).

## Didit contract

Create uses the configured workflow, stable private account ID as `vendor_data`,
operation metadata, and a browser callback URL. The callback is not a webhook.
Didit may return an unfinished session for the same account/workflow/version;
the service must reconcile that identity instead of counting it as a new check.

Read uses `include=events` because Didit only includes workflow version metadata
in that expanded KYC decision. The configured workflow/version must match. The
adapter requires Approved document, liveness and face-match features plus numeric
ID-derived age at least 18, and refuses automatic assurance from manual status
overrides. Missing evidence does not approve hosting. The published workflow must
also decline minimum-age violations for every permitted country. Only the minimal
18+ conclusion, method, references and observation time leave the adapter; it does
not retain document numbers, names, DOB, selfies or returned media URLs.

A privacy-standard age workflow may suppress the numeric document age. That
reduced decision requires a verified sandbox fixture and explicit mapping before
it is supported; the current adapter fails closed for absent age evidence. This
is intentional rather than assuming an arbitrary Approved KYC result proves 18+.
`VerifiedAt` is when the adapter observed sufficient evidence, not a claimed
provider event timestamp.

Webhook verification authenticates the entire payload with canonical Unicode V2
HMAC (or exact raw-body HMAC), supports the provider's previous-secret signature
header, checks timestamp freshness and requires a matching signed timestamp.
The deprecated envelope-only Simple signature is not accepted. The webhook
returns a reference, never identity media or a direct adult grant. Environment
must be sandbox; console test-webhook samples are rejected for state mutation.

## Testing and deployment boundary

Run `go test -race ./integrations/calls/providers` from the engine worktree.
Tests use HTTP fixtures only and cover consent, operation binding, authoritative
reads, SCA, uncertain writes, redirects, age evidence and signature tampering.
They do not constitute a successful test against either vendor account.

Before deployed sandbox testing, configure separate sandbox keys, published
workflow/version and webhook secrets; run approved, underage, review, decline,
resubmit, replay, timeout and recovery cases. Use synthetic IDs only: Didit's
sandbox does store any media uploaded even though its checks are simulated and
not billed. Production remains disabled pending that validation and separate
cash, retention, legal and provider configuration work.

Both providers allow branding. Stripe hosted Checkout supports logo, colors,
fonts and shapes; Elements allows deeper appearance control while Stripe handles
payment fields. Didit supports colors, logo, fonts, text and rounded corners per
application, enabled per workflow. Custom style normally adds $0.10 per finished
session, with an exemption for designated age-verification and certain other
workflow types. An age-restricted KYC workflow must not be assumed exempt.
Didit custom domains require Scale or Enterprise. Branding never removes
processor/biometric consent disclosures.

Sources checked 2026-11-20:

- [Stripe Checkout sessions](https://docs.stripe.com/api/checkout/sessions/create)
- [Stripe fulfillment](https://docs.stripe.com/checkout/fulfillment)
- [Stripe saving payment methods](https://docs.stripe.com/payments/save-and-reuse)
- [Stripe idempotency](https://docs.stripe.com/api/idempotent_requests)
- [Stripe webhooks](https://docs.stripe.com/webhooks)
- [Stripe sandboxes](https://docs.stripe.com/sandboxes)
- [Stripe appearance](https://docs.stripe.com/payments/checkout/customization/appearance)
- [Stripe Elements appearance](https://docs.stripe.com/elements/appearance-api)
- [Didit create session](https://docs.didit.me/sessions-api/create-session)
- [Didit retrieve decision](https://docs.didit.me/sessions-api/retrieve-session)
- [Didit result models](https://docs.didit.me/reference/data-models)
- [Didit webhooks](https://docs.didit.me/integration/webhooks)
- [Didit sandbox](https://help.didit.me/getting-started/sandbox-and-testing)
- [Didit age rules](https://help.didit.me/workflows/decision-rules)
- [Didit returned data](https://help.didit.me/workflows/returned-data)
- [Didit branding](https://help.didit.me/workflows/customize-branding-and-experience)
- [Didit billable checks](https://help.didit.me/billing-credits/what-counts-as-billable)
