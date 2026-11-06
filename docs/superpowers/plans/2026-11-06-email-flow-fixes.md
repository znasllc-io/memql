# Email Flow Fixes Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Fix every blocker and should-fix problem found by testing a real organization's email flow (sign-up, confirmation, automations, Azure sending, the pages behind email links, the OS Email app) before the next release.

**Architecture:** Engine changes stay in `component/campaigns` (automations, sends, recipient pages), `integrations/email` (Azure), `dsl/campaigns` (constructs) and `clients/os` (the Email app and the desktop). No new subsystem: each task tightens an existing path and pins it with tests.

**Tech Stack:** Go (engine), memQL DSL edition 2026, React + TypeScript + vitest (`clients/os`).

**Spec:** the findings list of 2026-11-06 (ids B1-B4, S1-S13, M1, M6, M7). Its text is summarised in each task below; the review evidence (file:line) is quoted where it pins the code to change. Copy of the list: `/Users/znas/projects/memql-fylo/.material/email-test-findings.md`.

## Global Constraints

- The engine is client-agnostic: no organization names, addresses or Fylo-specific branches anywhere.
- Do not loosen the send-time recipient-link check (`component/campaigns/render_link_safety.go`); it is a security boundary.
- Existing rows keep working: no destructive migration of enrollments, receipts, recipients or senders.
- Never send twice: every change to retries keeps the receipt and `requestId` idempotency (`single_send.go`, `journey.go journeyRequestID`).
- An unsubscribed or suppressed person never receives another email through any new path.
- Copy is plain sentences in the repo's voice; no em dashes in UI copy.
- UI rules: clean and minimal, reuse kit pieces (`Notice`, `RecordRow`, `AutoRefresh`, `Wizard`), no horizontal dividers, no toasts or status labels inside list items; read `clients/os/DESIGN.md` before UI work.
- Go DB tests run only against `MEMQL_DATABASE_DSN='postgres://memql:memql_dev@127.0.0.1:55432/memql_email_a?sslmode=disable' MEMQL_REQUIRE_DB=1`, never port 5432.
- A fresh worktree needs the git-ignored `component/identity/web/static/*` build files copied from the main checkout before root-package tests pass (already done in this worktree).
- Commits: `<area>: <sentence>` subject, explicit paths, message ends with `Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>`. No push.

## Review Focus

1. **A provider refusal of the confirmation email** must never leave the person waiting 7 days with nothing in their inbox: Task 1, `TestAConfirmationStepRefusedByTheProviderIsResent`.
2. **Editing a template mid-series** must not fail anyone permanently: Task 1, `TestAJourneySendToADraftTemplateWaitsInsteadOfFailing`.
3. **An unsubscribed person** must not be mailed by any retry path: Task 2, `TestRetryEnrollmentsDoesNotSendToAnUnsubscribedPerson`.
4. **Duplicate prevention must not block a legitimate re-join** (unsubscribe, then subscribe again through preferences): Task 3, `TestRejoiningAfterUnsubscribeIsNotADuplicate`.
5. **The OS desktop must stay unscrolled** after maximizing a window and stepping through a wizard: Task 6, `desktop stays at scroll zero`.

---

### Task 1: Automation sends never vanish or fail on fixable problems (B1, B2, S2 engine, M1, M7)

**Files:**
- Modify: `component/campaigns/journey.go` (`fail()` near :314-331, the advance decision near :369-372, `advanceEnrollment` hold near :200-223)
- Modify: `component/campaigns/send_to_recipient.go` (template status refusal near :140, receipt "attempting" near :300-323)
- Modify: `component/campaigns/single_send.go` (uncertain handling near :155-170)
- Modify: `integrations/email/acs.go` (error classification near :174-175, :355-359; `X-Campaign-Id` near :117), `component/campaigns/render.go` (:559)
- Modify: `component/campaigns/journey_save.go` (state change lock)
- Test: `component/campaigns/journey_failure_test.go` (new), `integrations/email/acs_test.go`

**Interfaces:**
- Produces: `errSendHeld` (`type sendHeld struct{ reason string }`, `func (sendHeld) Error() string`) in `component/campaigns`: a refusal caused by configuration the operator can fix (template not ready or archived, sender identity disabled or missing, organization inactive). Task 7 relies on the journey's `lastError` carrying that reason.

Decisions:
- **B1.** `send_to_recipient` returns `sendHeld` for configuration refusals. In `journey.go fail()`, a `sendHeld` does not count an attempt and never sets `failed`: the enrollment stays `active` with `nextAt = now + 15m`, and the journey's `lastError` is set to the reason.
- **B2.** Classify provider outcomes in `integrations/email`. A response the provider sent that refuses the message (429, any 5xx, any 4xx) is a *definite non-acceptance*: the single-send receipt is marked failed (not left "attempting"), so the next attempt submits again; it counts as an attempt (backoff stays 1m then 4m). Only a request whose outcome is unknown (no response, or the connection dropped after sending) is *uncertain*: then the journey does not advance; it holds the enrollment (`nextAt = now + 5m`) until the Azure operation for that `Operation-Id` resolves through the existing operation poller (`acs_operations.go`): accepted means sent and the step advances without a second POST; failed or not found means the receipt is marked failed and the next attempt submits again; unresolved after 24h marks the enrollment `failed` with a `lastError` that says the send could not be confirmed.
- **S2.** On every successful step, clear the journey's `lastError` (`recordJourneyDrain` with an empty error) when it is set.
- **M1.** Pausing and sending use one lock name, `campaign-journey:journey:<id>`, and the send re-reads the journey status inside the hold right before delivery.
- **M7.** Omit the `X-Campaign-Id` header when the campaign id is empty.

- [ ] **Step 1: Write the failing tests** (use the package's existing journey and fake-provider fixtures)
  - `TestAJourneySendToADraftTemplateWaitsInsteadOfFailing`: after five due drains with the template in draft, the enrollment is `active` with `attempts == 0` and the journey `lastError` mentions publishing; after publishing, the next drain sends once and advances.
  - `TestAJourneySendWithADisabledSenderWaits`: same shape with a disabled identity.
  - `TestAProviderRefusalIsRetriedNotSkipped`: provider answers 429 then 202; exactly two POSTs, one message sent, one advance.
  - `TestAServerErrorIsRetriedNotSkipped`: 503 then 202, same assertions.
  - `TestAConfirmationStepRefusedByTheProviderIsResent`: confirm step, provider answers 500 then 202; the enrollment reaches `waiting` only after the successful POST.
  - `TestAnUncertainSendWaitsForTheOperationResult`: the POST's response is lost; the enrollment does not advance; when the poller reports the operation accepted it advances with no second POST; in a second case the poller reports failed and exactly one more POST follows.
  - `TestJourneyLastErrorClearsAfterASuccessfulSend`.
  - `TestPausingHoldsASendThatIsDue`: pause lands between selection and delivery; no POST.
  - In `integrations/email`: `TestASendWithoutACampaignHasNoCampaignHeader`.
- [ ] **Step 2:** Run `go test ./component/campaigns/ ./integrations/email/ -run 'TestAJourney|TestAProvider|TestAServer|TestAConfirmation|TestAnUncertain|TestJourneyLastError|TestPausingHolds|TestASendWithout' -count=1`. Expected: FAIL.
- [ ] **Step 3:** Implement the decisions above.
- [ ] **Step 4:** Run the same command, then the DB lane for `./component/campaigns/` and `go test ./integrations/email/ -count=1`. Expected: PASS.
- [ ] **Step 5:** Commit: `campaigns: an automation email waits on fixable problems and is resent after a provider refusal`.

### Task 2: Enrollment recovery (S1 engine, S5)

**Files:**
- Modify: `dsl/campaigns/builtins.memql` (new builtin), `component/campaigns/capabilities.go` (registration)
- Create: `component/campaigns/journey_retry.go`
- Modify: `component/campaigns/newsletter_rejoin.go` (`expiredMembership` near :103), `component/campaigns/email_link_store.go` (`nudgeEnrollment` near :57-72)
- Test: `component/campaigns/journey_retry_test.go` (new)

**Interfaces:**
- Consumes: Task 1's held/failed semantics.
- Produces: builtin `campaignRetryEnrollments({ journeyId: string!, enrollmentIds: array })` returning `{ retried: int }`. Empty or absent `enrollmentIds` means every `failed` enrollment of the journey. Same authority as `campaignSetJourneyState`. Task 7's OS "Retry" and "Retry all" call it.

Decisions:
- Retry resets a `failed` enrollment to `active`, `attempts = 0`, `nextAt = now`, empty `lastError`. Anything not `failed` is left alone. The drain's existing exit rules still apply, so an unsubscribed or suppressed person exits instead of being mailed.
- `expiredMembership` treats a `failed` enrollment like an exited "not confirmed" one, so signing up again starts a fresh run.
- A late confirmation reopens the exited enrollment whatever the journey's state; the drain only sends while the journey is active (S5).

- [ ] **Step 1: Write the failing tests**
  - `TestRetryEnrollmentsResetsFailedOnesAndTheySend`: a failed enrollment, retry, next drain sends once and advances; `retried == 1`.
  - `TestRetryEnrollmentsDoesNotSendToAnUnsubscribedPerson`: failed enrollment, recipient unsubscribed, retry, next drain: no POST, enrollment exits.
  - `TestRetryEnrollmentsRefusesAnotherOrganizationsJourney`.
  - `TestSigningUpAgainAfterAFailedEnrollmentEnrollsAgain`: through the signup form handler.
  - `TestALateConfirmationWhilePausedIsSentAfterResume`.
- [ ] **Step 2:** Run them. Expected: FAIL.
- [ ] **Step 3:** Implement. Edition gate: the new builtin declaration follows the existing ones in `builtins.memql` (attributes, `@executor("integration.campaigns.retryEnrollments")`, `@sdk`).
- [ ] **Step 4:** Run them, the DB lane for `./component/campaigns/`, and `go test ./dsl/... -count=1`. Expected: PASS.
- [ ] **Step 5:** Commit: `campaigns: a failed automation run can be retried, and a late confirmation survives a pause`.

### Task 3: Audience membership integrity (S3 engine, S4)

**Files:**
- Modify: `dsl/campaigns/mutations.memql` (`addRecipient` near :89-120, `setRecipientSubscription` near :136), `dsl/campaigns/builtins.memql` if a builtin wrapper is the cleaner seam
- Modify: `component/campaigns/import.go` (duplicates), `component/campaigns/email_links.go` or the confirm path (`ConfirmRecipient`)
- Test: `component/campaigns/recipients_test.go` (new)

**Interfaces:**
- Produces: adding an address that is already in the audience (compared by `emailDigest`, so case and surrounding spaces do not matter) is refused with the sentence "This address is already in this audience." Imports skip such rows and count them. Task 7's Add people shows the same sentence before submitting, using `recipientInAudienceByDigest`.

Decisions:
- Choose the seam that makes the refusal unavoidable for every caller of the add path (DSL check or a builtin wrapper the OS calls); keep `addRecipient`'s argument shape for existing callers.
- Adding to an audience whose `confirmation` is `required` defaults to `pending`. An operator may still add someone as `subscribed` only by saying they already confirmed; that write records `confirmedAt` and a consent event, so automations triggered on confirmation enroll them.
- Changing a `pending` person to `subscribed` in a confirmation-required audience goes through the confirmation path: `confirmedAt`, consent, and a nudge for a waiting enrollment.

- [ ] **Step 1: Write the failing tests**
  - `TestAddingAnAddressAlreadyInTheAudienceIsRefused` (`a@b.test`, ` A@B.test `).
  - `TestImportSkipsAddressesAlreadyInTheAudience`.
  - `TestAddingToAConfirmFirstAudienceDefaultsToPending`.
  - `TestMarkingSomeoneConfirmedStartsTheirConfirmedAutomations`.
  - `TestRejoiningAfterUnsubscribeIsNotADuplicate`: unsubscribed member, "Subscribe again" from preferences succeeds.
- [ ] **Step 2:** Run them. Expected: FAIL.
- [ ] **Step 3:** Implement.
- [ ] **Step 4:** Run them, the DB lane for `./component/campaigns/`, and the edition gate checks the repo runs for `dsl/` (`go test ./dsl/... -count=1`). Expected: PASS.
- [ ] **Step 5:** Commit: `campaigns: an address is in an audience once, and confirming always goes through confirmation`.

### Task 4: Delivery ledger, bounces and merge tags (S6, S7, M6)

**Files:**
- Modify: `component/campaigns/azure_feedback.go` (:42-61, :99-106), `component/campaigns/template.go` (publish), `component/campaigns/render.go` (known tags)
- Test: `component/campaigns/azure_feedback_test.go`, `component/campaigns/template_test.go`

Decisions:
- A delivered or succeeded report (Event Grid or the operation poller) moves a `failed` delivery row to `sent` and sets `sentAt`.
- For a bounced or suppressed report whose delivery row is missing, write the suppression first, then return the retryable "not recorded yet".
- Publishing refuses any `{{...}}` that is not a known tag (`displayName`, `email`, `campaignName`, `accountName`, the four link tags, `fields.<key>`), naming the tag: "{{firstName}} is not a tag this email can fill in."

- [ ] **Step 1: Write the failing tests**: `TestADeliveredReportCorrectsAFailedRow`, `TestABounceSuppressesEvenWithoutItsDeliveryRow`, `TestPublishRefusesAnUnknownMergeTag`, `TestPublishAcceptsKnownTagsAndFieldTags`.
- [ ] **Step 2:** Run them. Expected: FAIL.
- [ ] **Step 3:** Implement.
- [ ] **Step 4:** Run them and the DB lane for `./component/campaigns/`. Expected: PASS.
- [ ] **Step 5:** Commit: `campaigns: delivery reports correct the ledger, bounces always suppress, and unknown tags cannot be published`.

### Task 5: The pages behind email links (S8, S9, S10, S11)

**Files:**
- Modify: `component/campaigns/email_links.go` (view links near :295, preferences actions near :219-281, pages near :75, :147-149, :332-336), `component/campaigns/unsubscribe.go` (`writeEmailPage` near :305-313, done page near :281-285), `component/campaigns/links.go`
- Read: the brand kit the Email app saves (`campaignSaveBrandKit`) for the organization's logo and primary colour
- Test: `component/campaigns/email_links_test.go`, `component/campaigns/unsubscribe_test.go`

Decisions:
- **S8.** The view page of an email that carries `{{confirmUrl}}` gives a pending member inside the confirmation window a working confirm link; otherwise that link points at the member's preferences page.
- **S9.** The preferences page offers "Confirm subscription" only inside the confirmation window (the confirm link's 30 days from joining). "Subscribe again" in a confirmation-required audience rejoins the person as pending, which sends a fresh confirmation through the audience's joined automation, and the page says "Check your inbox to confirm."
- **S10.** Every page shows the organization's logo from its brand kit (when there is one), uses its primary colour for the button, and names the organization in `<title>` ("Confirm your subscription to <Org>", "Unsubscribe from <Org>", "<Org> email preferences"). The link host stays the cluster setting; document in `docs/public/operate/campaign-sending.md` that it is one host for the cluster.
- **S11.** The unsubscribed page offers "Changed your mind? Manage your email preferences." Confirming after unsubscribing gets a heading and a sentence that says they unsubscribed, with the same preferences link. Expired and invalid link pages name the organization's reply-to address as the contact. The preferences unsubscribe button reads "Unsubscribe from all <Org> emails".

- [ ] **Step 1: Write the failing tests** (httptest against the handlers): `TestTheViewPageOfAPendingConfirmationHasAWorkingConfirmLink`, `TestThePreferencesPageDoesNotConfirmAnExpiredSignUp`, `TestSubscribingAgainToAConfirmFirstAudienceSendsAConfirmation`, `TestEmailPagesCarryTheOrganizationsNameAndLogo`, `TestTheUnsubscribedPageOffersAWayBack`, `TestExpiredAndInvalidLinkPagesNameAContact`, `TestThePreferencesUnsubscribeSaysItCoversEveryEmail`.
- [ ] **Step 2:** Run them. Expected: FAIL.
- [ ] **Step 3:** Implement.
- [ ] **Step 4:** Run them and the DB lane for `./component/campaigns/`. Expected: PASS.
- [ ] **Step 5:** Commit: `campaigns: the pages behind email links carry the organization's brand and always offer a next step`.

### Task 6: The OS desktop stays put, and setup says "Ready" (B3, S13)

**Files:**
- Modify: `clients/os/src/styles/index.css` (:187-191), `clients/os/src/chrome/Desktop.tsx` (:485-489), `clients/os/src/kit/Wizard.tsx` (:214), `clients/os/src/kit/reveal.ts` (:44), `clients/os/src/kit/controls.tsx` (:598, :605), `clients/os/src/kit/DialogFrame.tsx` (:100), `clients/os/src/chrome/ContextMenu.tsx` (:136-161)
- Modify: `clients/os/src/kit/ReadinessStates.tsx` (:99, :372-373), `clients/os/src/apps/email/settings/BrandPanel.tsx` (:33)
- Test: the matching `*.test.tsx` files

Decisions:
- `.os-root`, `.os-desktop` and `.os-plates` clip instead of scroll (`overflow: clip`), and `.os-desktop` resets `scrollLeft` and `scrollTop` to 0 on any scroll event.
- The wizard moves its own scroller (`scrollTop`) instead of calling `scrollIntoView`; the listed `focus()` calls pass `{ preventScroll: true }`.
- Readiness words: configured is "Ready", not configured is "Not set up"; the group heading is "Setup". Brand uses the same words.

- [ ] **Step 1: Write the failing tests**: `desktop stays at scroll zero` (a scroll event on `.os-desktop` is undone), `a wizard step change does not call scrollIntoView`, `readiness says Ready and Not set up`.
- [ ] **Step 2:** Run `npx vitest run <files>` in `clients/os`. Expected: FAIL.
- [ ] **Step 3:** Implement.
- [ ] **Step 4:** Run the same, then `npm run typecheck`. Expected: PASS.
- [ ] **Step 5:** Commit: `os: the desktop never scrolls sideways, and setup says Ready`.

### Task 7: The Email app shows and fixes what the engine now reports (S1, S2, S3, S12 UI, B1 UI)

**Files:**
- Modify: `clients/os/src/apps/email/automations/AutomationPage.tsx` (:36, :55, :60), `clients/os/src/apps/email/useEmail.ts` (:1326), `clients/os/src/apps/email/actions.ts`
- Modify: `clients/os/src/apps/email/audiences/AddPeopleWizard.tsx` (:34), `clients/os/src/apps/email/audiences/Subscribers.tsx`
- Modify: `clients/os/src/modules/connections/AzureEmailDomainRecords.tsx` (:11, :23), `clients/os/src/modules/connections/AzureEmailConnections.tsx` (:187)
- Modify: the template page that holds "Open in editor" (`apps/email/templates/`)
- Test: matching `*.test.tsx`

**Interfaces:**
- Consumes: `campaignRetryEnrollments` (Task 2), the refusal sentence of Task 3, `journey.lastError` semantics of Task 1.

Decisions:
- Automation page: a "People" section with counts by status (in a run, waiting, finished, left early, failed; zero counts shown once loaded), the failed people listed with their step and last error, a "Retry" per person and "Retry all". The journey's `lastError` shows as a `Notice` while it is set (Task 1 clears it on success). When "Turn on" is unavailable, show it disabled with the reason.
- Add people: look the address up with `recipientInAudienceByDigest` before enabling Add; show "This address is already in this audience." with a way to open that person.
- DNS records: the SPF caption says to add the value to the domain's one SPF record and end it with `-all` because Azure does not verify `~all`; the step re-checks every 30 seconds while open (`AutoRefresh`) and shows "Last checked"; after 5 minutes of "Checking" it adds the hint "Still checking: make sure there is one SPF record and it ends in -all."
- Template page: when an active automation uses the template, a `Notice` before editing says saving makes it a draft again and that automation holds those emails until it is published.

- [ ] **Step 1: Write the failing tests**: `automation page shows people by status and retries failed ones`, `add people refuses an address already in the audience`, `spf caption names -all and records re-check while open`, `template used by an active automation warns before editing`.
- [ ] **Step 2:** Run them. Expected: FAIL.
- [ ] **Step 3:** Implement.
- [ ] **Step 4:** Run them, `npm run typecheck`, and `npx vitest run src/apps/email src/modules/connections`. Expected: PASS.
- [ ] **Step 5:** Commit: `os: the Email app shows failed runs with Retry, refuses duplicate people, and explains DNS checks`.

### Task 8: One place to add senders on an Azure domain (B4)

**Files:**
- Modify: `integrations/email/azure_resources.go` (`finish()` :205-320), `integrations/email/azure_workflow.go` (the `sender` action), `integrations/email/azure_identity.go`
- Modify: `clients/os/src/modules/connections/AzureEmailConnections.tsx` (:176, :215, :231-254), `clients/os/src/apps/email/settings/AddSenderWizard.tsx` (:58, :83, :180), `clients/os/src/apps/email/settings/SendersPanel.tsx`, `clients/os/src/apps/email/automations/issues.ts`
- Test: `integrations/email/azure_resources_test.go`, matching `*.test.tsx`

Decisions:
- `finish()` takes `makeDefault`. The connection's first sender becomes the default; later senders keep the current default unless `makeDefault` is true. A "Make default" action changes only the default.
- The domain page lists every sender registered with Azure (address, name, reply-to, which is the default) with "Add sender" (registers with Azure and records the MemQL sender in one action) and "Make default".
- For an organization with an Azure domain, Settings > Senders "+" opens that domain's Add sender. The record-only wizard stays for organizations without one.
- A MemQL sender on an Azure domain whose address Azure does not have shows "Not registered with Azure", and automation issues refuse it before turning on.

- [ ] **Step 1: Write the failing tests**: Go `TestAddingASenderKeepsTheDefault`, `TestMakeDefaultMovesTheDefault`, `TestTheFirstSenderIsTheDefault`; vitest `domain page adds a sender without changing the default`, `settings plus opens the azure add sender`, `an unregistered sender is flagged and blocks turning on`.
- [ ] **Step 2:** Run them. Expected: FAIL.
- [ ] **Step 3:** Implement.
- [ ] **Step 4:** Run them, `go test ./integrations/email/ -count=1`, `npm run typecheck`, `npx vitest run src/modules/connections src/apps/email`. Expected: PASS.
- [ ] **Step 5:** Commit: `email: senders on an Azure domain are added in one place and keep the default`.

## Finish

- [ ] Whole-branch review on the most capable model.
- [ ] Full gates: `go build ./...`, `go vet ./component/campaigns/ ./integrations/email/`, `go test ./component/compose/... ./integrations/email/ ./integrations/work/ . -count=1`, the DB lane for `./component/campaigns/`, `npm run typecheck && npx vitest run` in `clients/os`.
- [ ] Merge into `main` and push (the owner's standing instruction: no PR for this program).
