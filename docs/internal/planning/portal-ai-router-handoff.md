---
title: AI Router — Portal Product Specification
audience: internal
status: draft
area: internal
sinceVersion: 0.9.0
owner: znas
---

# AI Router — Portal Product Specification

> **STATUS (2026-05-18): QUESTIONABLE — under product review.**
>
> Spec is intact and forward-looking; no execution work has started in any
> of the three repos. Confirm scope and priority before handing this off
> to a Portal-team implementer.

---

**Audience:** the engineer building the CoPresent Portal.
**Purpose:** give you a complete understanding of the AI Router as a product — what it is, who it's for, what features it offers, and what requirements each feature has — so you can design and build the admin surfaces in the Portal from scratch.

This document is deliberately free of references to existing code, commit history, data-type schemas, or file paths. It's a product spec.

---

## 1. What the AI Router is

Every CoPresent agent reply, suggestion, transcription, and synthesis call is a request to an external AI vendor — OpenAI, Anthropic, Google, and so on. Each of those calls costs money (per-token pricing), takes time, can fail, and uses someone's API key.

The AI Router is the layer that makes all of that visible and controllable. It sits between CoPresent and the vendors. For every AI call it:

- **Picks** the right model — either from a named routing strategy ("use the fast cheap one", "use the strong-reasoning one") or a specific model an operator pinned.
- **Executes** the call against the vendor, with automatic fallback to a backup model if the primary vendor returns an error before the stream starts.
- **Records** the result — which model, how many tokens, how much it cost, how fast the first token arrived, how long the whole call took, whether it succeeded — as a row in a time-series ledger.

Over time that ledger becomes the operator's single source of truth for "what did my AI spend look like this month, and why?"

The Portal is where an operator **provisions** and **administers** the AI Router for their CoPresent instances. The individual CoPresent instance consumes the router at runtime but does not configure it.

---

## 2. Who uses it, and what they're trying to do

**Primary persona:** **the Operator.** Someone responsible for one or more CoPresent instances — often the owner of a workspace, the admin who bought the plan, or the IT lead at the organization using CoPresent.

**What the Operator needs to do, in plain English:**

1. **Know what's happening.** "Are my agents working? How much did they cost me last week?"
2. **See what's available.** "What models does CoPresent support? What do they cost?"
3. **Understand how the system decides.** "When my agent replies, which model actually runs? Who decides that?"
4. **Bring their own vendor account.** "I want the AI bills to come to my OpenAI account, not yours."
5. **Cap the spend.** "Don't let us spend more than $500/month on this."
6. **Get alerted before it matters.** "Tell me when we cross 80% of our cap so I can act before we hit it."

The AI Router admin surface in the Portal is the set of screens that lets the Operator do all six.

**Secondary persona:** **the CoPresent end-user.** Not a target of the Portal. They interact with the router indirectly: when they create an agent in CoPresent, they can pick a routing policy (or pin a specific model) for that agent. The policies and models they can choose from are the ones the Operator set up in the Portal. The end-user never configures the router itself.

---

## 3. Feature 1 — Usage & Spend Observability

**What it is:** a live dashboard showing what the AI Router is doing, right now and over time.

**User stories:**
- As an Operator I want to see total spend for the last 24 hours, 7 days, and 30 days so I can trend it.
- As an Operator I want to see error rate so I know if something is broken.
- As an Operator I want to see a chart of daily spend so I can spot the shape of usage.
- As an Operator I want to see the most recent individual AI calls so I can investigate a specific incident.
- As an Operator I want to filter by vendor or by outcome so I can zoom into a specific problem (e.g. "show me only the errors", "show me only the Anthropic calls").

**Requirements:**

1. **Four headline numbers at the top of the screen**, each combining a dollar amount with a call-count hint:
   - Spend, last 24 hours
   - Spend, last 7 days
   - Spend, last 30 days
   - Error rate, last 24 hours (percentage). This card must visually emphasize (amber / warning tint) when the error rate exceeds 5%.
2. **A sparkline chart of daily spend over the last 7 days.** Each day is a labeled point; hovering shows the exact amount. The chart must render even when some days have zero spend (the line flatlines at zero for those days).
3. **A filter bar:**
   - Vendor dropdown. Options: "All vendors" plus every vendor that appears in the returned data (dynamically).
   - Outcome filter, shown as a chip group: "All", "OK", "Errors", "Cancelled". Single-select.
   - A count indicator showing "Showing N of M recent calls."
4. **A table of recent AI calls**, reverse-chronological. Each row shows, at minimum:
   - Timestamp (local formatting).
   - The vendor and the specific model used.
   - Which agent triggered the call (or a dash if the call wasn't agent-driven — e.g. a suggestion or a transcription).
   - Input token count, output token count. Token counts may be marked as estimated today; the UI must display an "est." indicator when the flag is set.
   - The dollar cost of that call. Empty dash when the model has no pricing configured.
   - Time-to-first-token (for streaming calls) and total duration.
   - Outcome badge: green "OK", red "Error" with a short category (timeout, rate-limit, auth, upstream, etc.), or grey "Cancelled".
5. **Refresh**: the dashboard must support an explicit refresh action, and should be snappy (the backend query returns a few hundred rows at most; no pagination required for v1).
6. **Empty state**: when no AI calls have happened yet, the table shows a friendly message pointing the Operator at "have an agent reply to something — rows appear here the moment the router finishes the call."

**Non-requirements for v1:**
- No deep drill-down into a single call (modal, detail page). The table row is the detail view.
- No custom date-range picker. 24h / 7d / 30d buckets are enough.
- No export to CSV (nice-to-have, deferrable).

---

## 4. Feature 2 — Model Catalog

**What it is:** a read-only browsable list of every AI model the router can use, with pricing and availability.

**User stories:**
- As an Operator I want to see every model my CoPresent instance can call so I know what's on the menu.
- As an Operator I want to see per-million-token pricing for input, output, and cached input so I can compare model costs.
- As an Operator I want to see which models are currently unavailable (missing vendor API key) so I know what I need to configure.
- As an Operator I want to filter by vendor so I can focus on one ecosystem.

**Requirements:**

1. **A table listing every model**, grouped or sorted by vendor. Each row shows:
   - Model name and a short description.
   - Vendor (as a pill / chip).
   - Model's vendor-side identifier (the string that goes to the API).
   - Modality: text, text-to-speech, speech-to-text, embedding, etc.
   - Context window size (number of tokens).
   - Per-million-token prices: input, output, cached input. Models without pricing configuration show a dash for each.
   - Status: **Available** (green checkmark) or **Unavailable** (grey, with a tooltip explaining why — usually "vendor API key not set"). A "Default" indicator when a model is marked as the system default.
2. **A vendor filter dropdown** listing "All vendors" + every vendor present.
3. **A "Show unavailable" toggle** — off by default. Operators usually only want to see what they can actually use, but need a way to discover what they could enable.
4. **No editing in this view.** This is informational. Adding a new vendor is a backend / deployment-time operation, not a UI action (see section 10).

**Non-requirements:**
- No per-model usage stats overlay on the catalog. That's the dashboard's job.
- No "try this model" sandbox action.

---

## 5. Feature 3 — Routing Policies

**What it is:** a read-only viewer for the named routing strategies the router can apply. A policy says "for this kind of work, primary model is X, fall back to Y, fall back to Z, and here are latency bounds."

**Why operators care:** when an agent is created without pinning a specific model, the router uses a policy to decide. Operators want to understand those policies so they can make sense of why a particular call went to a particular model.

**User stories:**
- As an Operator I want to see every policy my instance has so I know what options my end-users have when creating agents.
- As an Operator I want to see each policy's chain (primary + fallbacks) so I understand the retry behavior.
- As an Operator I want to see which agent roles default to which policy so I understand the out-of-the-box routing behavior.

**Requirements:**

1. **A card grid of policies**, one card per policy. Each card shows:
   - **Policy name** — typically a short descriptive slug like "balanced chat", "strong reasoning", "fast coding", "low-latency voice", "cheapest capable".
   - **Short description** explaining what the policy is good for.
   - **The full chain**: primary provider followed by each fallback, rendered as chips connected by arrows. The primary chip visually distinct (accent color, ring outline).
   - **Constraints** (when set): maximum latency in milliseconds, maximum time-to-first-token in milliseconds. Render only when a value is set; omit when not.
   - **Preferred agent roles**: which agent roles default to this policy. A policy may be preferred by zero, one, or many roles.
2. **No editing in this view.** Policy authoring in v1 is a deployment-time operation — policies ship as part of the backend configuration, not as user-managed records. The UI surfaces them; it doesn't create them.
3. **Refresh action** to reload the list.

**Non-requirements for v1:**
- No policy-creation UI. (Possible future: see section 10.)
- No "test this policy" simulator.
- No per-role default overrides from the UI.

---

## 6. Feature 4 — Bring Your Own Key (BYOK)

**What it is:** the ability for the Operator to provide their own API keys for each supported AI vendor, so their AI usage is billed to their own vendor account instead of the platform's.

**User stories:**
- As an Operator I want to add my OpenAI API key so my spend goes to my OpenAI account.
- As an Operator I want to label my keys so I can tell "Production" from "Staging" or "Shared team key" apart.
- As an Operator I want to rotate a key quickly when it might be compromised.
- As an Operator I want to see when a stored key was last used so I know if it's still needed.
- As an Operator I want to be confident my key is stored securely and never surfaces in the UI after I enter it.

**Requirements:**

1. **Add-key form** (collapsible):
   - Vendor dropdown: the set of supported vendors (OpenAI, Anthropic, Google Gemini, Groq, xAI, Mistral, and any others the router supports at that moment).
   - Optional label input, free-form.
   - Plaintext key input, rendered as a password input. Validation: disable the save button until the key is at least ~10 characters (obvious-typo guard, not a strict check).
   - Helper text explaining the security contract: "Encrypted server-side. The cleartext never leaves the browser except over TLS to the platform."
   - Save action. On save the UI must clear the plaintext from memory immediately; the form should reset.
2. **Keys table** listing active keys. Each row:
   - Vendor (emphasized).
   - Label (or dash when empty).
   - **Fingerprint only** — e.g. `...fG3q`. The plaintext NEVER returns from the server after save. The fingerprint is the last four characters and lets the Operator tell rotated keys apart.
   - When it was added.
   - A delete action (soft-deletes; the record stays for audit, it just becomes inactive).
3. **Rotation is additive.** Saving a new key for a vendor that already has one replaces the active key; history is retained.
4. **Server-side encryption is mandatory.** The Operator submits plaintext over TLS; the server encrypts it with a symmetric key (a platform-operator-managed secret) before persisting. If the server's encryption key is unavailable, the save must fail with a clear, actionable error message ("Server is not configured for BYOK yet — contact your platform operator") rather than silently storing the plaintext or succeeding with bad data.
5. **Partition scoping.** Every key belongs to exactly one partition (tenant scope). The authenticated Operator only sees and manages keys for their own partition.

**Operational note:** the backend BYOK encryption depends on a platform-level secret being configured. The Portal's BYOK surface must surface a clear error when it isn't set, and the feature rollout should be coordinated with whoever runs the backend so the secret is in place before the UI goes live.

**Non-requirements for v1:**
- No per-vendor "primary + backup" model of two keys. One active key per vendor per partition.
- No key testing action ("send a ping to verify this works"). Nice future add.
- No key expiry dates from the UI.

---

## 7. Feature 5 — Budget Caps

**What it is:** the ability to cap how much the router is allowed to spend in a given time period, partition-wide or per-agent, with an alert threshold.

**User stories:**
- As an Operator I want to cap my organization's monthly AI spend at $500 so I don't get surprised.
- As an Operator I want a warning at 80% of that cap so I can react before it's hit.
- As an Operator I want to cap a specific agent's spend separately so one chatty agent can't consume the whole budget.
- As an Operator I want to temporarily disable a budget (e.g. during incident response) without losing its history.

**Requirements:**

1. **Add-budget form** (collapsible):
   - Scope dropdown: "Partition (all agents)" or "Single agent".
   - When scope = agent: an agent picker (searchable list of the Operator's agents, ideally; at minimum a text input that accepts an agent identifier).
   - Period dropdown: **Daily**, **Weekly**, **Monthly**. (Periods roll over at UTC day / week-start-Monday / month boundaries.)
   - Limit in USD, numeric input.
   - Alert threshold percentage, numeric input (e.g. 80 = "alert at 80% spent"). Setting 0 disables early alerts.
   - Save action.
2. **Budgets list**. Each row shows:
   - Scope label: "Partition-wide" or "Agent: {name or id}".
   - Period (daily / weekly / monthly).
   - Alert threshold percent.
   - Large, prominent limit amount on the right.
   - Next reset moment.
   - A way to deactivate the budget (soft-disable; history retained).
3. **Utilization (future enhancement)**: display current-period spend against the cap as a progress bar. For v1, the Portal can compute this client-side by cross-referencing the AI-call ledger against the budget period. Ship the bar when the volume supports it; the bar is a polish item, not a blocker.
4. **Partition scoping and authorization.** Budgets live per partition. Only admins of the partition can create, edit, or deactivate budgets.

**Operational note:** the Portal can build this feature and ship the UI right away; runtime enforcement (the router refusing an over-limit call before sending it to the vendor) is a follow-up that lands on the backend without any further Portal work.

**Non-requirements for v1:**
- No multi-level alerts (e.g. alert at 50%, 80%, 100%). Single threshold.
- No per-vendor or per-model budgets. Scope is partition or single agent.
- No budget transfers or rollovers.

---

## 8. Cross-cutting requirements

Requirements that apply across every feature above.

### 8.1 Authentication and authorization

- Every surface requires authentication. No anonymous access.
- Every view is scoped to the authenticated Operator's partition (tenant). They see and manage only their own data.
- **BYOK and Budget Caps must be admin-gated.** Whatever admin-role concept the Portal has, those two surfaces are restricted to admins. Dashboard, Catalog, and Policies can reasonably be visible to any authenticated Operator of the partition; admin-gating them is a product decision for the Portal team.

### 8.2 Error handling

- Every surface needs a graceful loading state, error state, and empty state.
- Server-side errors must be surfaced to the user with actionable text — "Server is not configured for BYOK yet — contact your platform operator" is better than "Failed to save (500)".
- Network failure or disconnection mid-save should not leave the UI in an inconsistent state. A failed save must be re-attemptable.

### 8.3 Performance expectations

- Dashboard, Catalog, Policies, Settings lists all return small result sets (hundreds of rows max at current scale). No pagination is required for v1.
- The dashboard's recent-calls table is the largest consumer and returns up to a few hundred rows. Client-side filtering (vendor / outcome dropdowns) should be instant; no re-query on filter toggle.
- Writes (save key, save budget, delete key) are low-frequency. Optimistic UI updates are nice-to-have; a simple request → wait → refresh list cycle is acceptable for v1.

### 8.4 Security

- **BYOK plaintext.** The key is entered in the browser, transmitted to the server over the secured channel, encrypted server-side, and never returned in plaintext. The UI must not store the plaintext in any persistent state (no localStorage, no URL params, no analytics). The input field is cleared as soon as the save action fires.
- **Fingerprint only.** After save, the only representation of the key in the UI is the fingerprint (last 4 chars of plaintext). This is intentional — it lets Operators disambiguate rotated keys without exposing the secret.
- **Never log the plaintext.** Server-side or client-side logging must not include the plaintext key.

### 8.5 Time and currency formatting

- All timestamps render in the user's locale, in their local timezone.
- All currency renders in USD with appropriate decimal precision (enough to show $0.0025 accurately on small per-call costs; fewer decimals are fine for aggregate numbers).
- Token counts render with thousands separators.

---

## 9. What is and isn't the Portal's job

**The Portal owns:**
- The five admin surfaces above: Dashboard, Catalog, Policies, Settings (BYOK + Budgets).
- Navigation, layout, auth gating, and product polish for those surfaces.
- Any future admin feature that manages the router itself.

**The Portal does NOT own:**
- The underlying AI Router engine. The Portal invokes backend operations the platform exposes; it does not re-implement the router.
- End-user-facing pieces in individual CoPresent instances: when a CoPresent user creates an agent, they pick a routing policy (dropdown of the policies you'd see in the Portal) and optionally pin a specific model. That picker lives in the CoPresent app, not the Portal. The Portal makes the policies and models *exist*; the CoPresent end-user *picks among them*.
- Provisioning vendors or adding new models. Today, supporting a new AI vendor is a deployment-time operation. The Portal surfaces the existing catalog; it doesn't create new rows in it.
- Policy authoring. Today, policies are configured at deployment time. The Portal surfaces them read-only. A future enhancement could let operators author policies in UI — flagged in section 10 below.

---

## 10. Known follow-ups and product gaps

Features that are functional or partial today and have a known completion path. The Portal can ship its UI now; these unlock when the backend catches up, without any Portal change.

1. **Accurate token counts on the dashboard.** Today the ledger marks token counts as "estimated" (a char-count heuristic). Surfacing an "est." badge on the UI is required so operators don't trust the numbers to the cent. When the backend swaps to vendor-reported usage, the flag flips and the badge stops showing automatically.

2. **BYOK runtime activation.** The Portal can collect BYOK keys today and they're stored securely, but the runtime AI-call path still uses the platform's own vendor keys. A separate backend change will flip the runtime to use the Operator's BYOK key when present. The Portal UX doesn't change.

3. **Budget enforcement at runtime.** Similarly, budgets are captured but not yet enforced pre-flight. Portal ships the UI; enforcement lands separately.

4. **Alert emission on budget threshold.** The threshold percentage is stored. Actually firing a notification when spend crosses it is a separate feature — needs a notification channel (email, webhook, in-app toast) and an emission path. The Portal can design the notification surface once; any of those channels will feed the same UI.

5. **UI-based policy authoring.** Today, adding a new routing policy is a deployment-time operation. A future phase could let operators compose policies in UI (pick a primary, add fallbacks, set latency bounds). Out of scope for v1 because the composition is nuanced (provider compatibility, cost / latency trade-offs) and deserves its own design pass.

6. **Rich agent picker in the per-agent budget form.** A searchable, name-first picker rather than raw identifier input.

7. **Usage attribution rollups on the dashboard.** Grouping by agent, by role, by user, or by model. The data is in the ledger; it's a UI add.

---

## 11. Out of scope for v1

Explicit non-goals so the Portal team doesn't accidentally over-build:

- Multi-tenant operator views (an Operator managing multiple partitions from one account). The router is scoped to one partition at a time.
- Cross-partition analytics or aggregation.
- Alerting channels (email, Slack, PagerDuty integrations). The threshold is captured; where the alert goes is a separate product decision.
- Public-facing pricing pages. The catalog is the Operator's internal reference, not a published price list.
- AI Router access-control granularity finer than "admin of the partition". No per-feature role-based policies within the router itself.

---

## 12. Success criteria

You know you've built this correctly when:

1. An Operator logs in, opens the Dashboard, and can tell at a glance: "we spent $X this week, it's up / down from last week, our error rate is healthy, and the most recent AI calls all succeeded."
2. An Operator can browse the Catalog and see which models are available, which cost what, and flag any that are unavailable.
3. An Operator can read the Policies page and explain to a colleague "our default policy starts with model X and falls back to Y, and our tours use policy Z which optimizes for latency."
4. An Operator can add their own OpenAI key, see only the fingerprint afterward, and later rotate it in seconds.
5. An Operator can set a $500/month cap with an 80% alert threshold and have confidence the system is tracking it.
6. An Operator sees a clear, actionable error message when their instance's server isn't configured for BYOK yet — not a blank screen or a generic failure.

---

## 13. Glossary

- **Operator** — an admin / owner of a CoPresent instance. The primary user of the Portal.
- **Partition** — a tenant scope. Every Operator's data is scoped to a partition; the router admin surface only shows that partition's data.
- **Vendor** — an upstream AI provider (OpenAI, Anthropic, Google, etc.).
- **Model** — a specific AI model offered by a vendor (e.g. Anthropic's Claude Sonnet, OpenAI's GPT-5).
- **Provider** — a specific combination of vendor + model + configuration. The catalog's rows are providers.
- **Policy** — a named routing strategy: primary model + ordered fallback chain + optional latency constraints + preferred agent roles.
- **Ledger** — the time-series record of every AI call the router has made. The Dashboard is a view over the ledger.
- **BYOK** — Bring Your Own Key. The Operator supplies their own vendor API key and the router uses it in place of the platform's key.
- **Budget** — a spend cap with a period (daily / weekly / monthly), a scope (partition or agent), a limit, and an alert threshold.

---

**Questions about product behavior go to whoever owns the CoPresent product roadmap.**
**Questions about the backend contract the Portal calls go to whoever owns the AI Router backend.**
