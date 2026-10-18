---
title: Profile -- your photo, your details, your storage and your AI budget
audience: internal
status: draft
area: operate
sinceVersion: 0.30.0
owner: znas
---

# Profile

Owner brief, 2026-10-18 (memql#6220): the profile "needs to be completely
refactored". There was no way to see your own information, change your
photo or change your own settings. It should also show the storage allocated
to you, how much you use and how much is left, and per-person storage
allocation should exist in the platform.

Routes (`2026-10-18-routes-design.md`) add a personal monthly AI budget that
Auto respects. Its home is the profile.

## Where it lives

- **The Identity app's Profile section.** It replaces the identity service's
  read-only `/me/` field list in place. The app's other sections (Passkeys and
  sessions, Access tokens, Settings) keep the identity service's own pages.
  The Identity app has no role floor, so everyone reaches their own profile.
- **Opened from the account avatar.** The dock's account menu gains
  **Profile**, between the theme modes and Sign out.
- **The layout is the Fleet machine Settings reference** (`DESIGN.md`, "Details
  and settings content"): `Details`, `DetailsSection`, `SettingRow`, `Facts`,
  open named sections, one label column, edits in place, quiet copying.

## Sections

1. **Header.** The photo, or the initial on the same circle, at 64px, then the
   display name and one muted line: the email and the role's catalog name. No
   buttons in the header.
2. **General.**
   - **Name:** first and last name, edited in place. **Save** appears only once
     the draft differs and the first name is not blank. It is absent rather
     than disabled (interface rule 12).
   - **Photo:**
     - **Upload photo** / **Change photo** opens the file picker. It takes PNG,
       JPEG or WebP of up to 5 MB.
     - The browser crops the centred square and redraws it at 160x160, then
       encodes WebP, or JPEG where WebP cannot be encoded, stepping the quality
       down until it fits the engine's 40,000-character bound.
     - The trash icon removes the photo after an in-place question ("Remove
       your photo? Your initial is shown instead.", Keep it / Remove photo).
   - **Email** (copyable) and **Role** as facts. **Phone** and **Title**
     (`primaryRole`) appear only when an admin has set them.
   - **No "Member since".** A user row's `createdAt` is its latest version's
     time, because reads collapse to the newest version, so it would change
     every time the person renamed themselves. The page does not show a date it
     cannot state truthfully.
3. **Storage.** **Allocated**, **Used** and **Left**, with one plain bar
   aligned to the values; the bar turns the warning colour from 90%.
   - Uploads in progress are named on the Used line.
   - An account with no personal limit says "No personal limit" and draws no
     bar.
   - On a cluster with no storage behind it, one line: "Storage is not set up
     on this cluster."
4. **AI budget.** Shown only when the cluster serves `myBudget`. A cluster that
   answers `function "myBudget" not found` gets no section, and no error.
   - **Monthly budget** is a dollar field: an amount and **Save** sets it, and
     the field emptied and **Remove** removes it. The empty field reads "No
     budget".
   - One caption: "Auto keeps your paid vendor spend under this each month."
   - **Spent this month** and **Resets**. The reset date is read in UTC,
     because periods are UTC months: in the local zone, Nov 1 00:00 UTC is the
     evening of Oct 31 for half the world.
5. **Sign-in and security.** How many active passkeys you have, the sign-in
   policy in words ("Passkey only" or "Passkey or email link"), and **Manage
   passkeys and sessions**, which opens the Identity app's own section.
6. **Preferences.**
   - **Computer use:** the person's own kill switch, "Let tasks use my
     machines". When it is off, one caption says what that means.
   - **Time zone:** UTC plus every IANA zone the browser lists.

Every write shows pending state, keeps the draft on a refusal with the
engine's reason beside the row, and shows no routine success receipt.

## One copy of the person

`modules/profile/myProfile.ts` reads `currentUser` once per connection and
shares the row with every place the person is drawn: the dock avatar, Ask's
"You" line and this page. A write the engine accepts patches that copy, so a
new photo reaches the dock at once. `v1:identity:user` broadcasts creates only,
so no event would ever deliver it.

## Engine

- **Photo.**
  - `v1:identity:user` gains an optional `avatar string @pii`, projected by
    `userFull`. The photo therefore reaches the person (`currentUser`) and the
    people who can already read someone's full row. It is not added to the
    public `userDisplayCard`.
  - It is written only through `setMyAvatar(avatar)` and `clearMyAvatar()`.
- **Name.** `updateMyName(firstName, lastName, displayName)` writes the two JWT
  name claims and the display name the concept documents. A whitespace-only
  name is refused.
- **Self-only, by construction.** All three mutations follow the
  `updateMyPreferences` pattern: `@actor`, `id: actor.userId`, and no target
  argument, so another person's row cannot be named.
- **The photo's contract.**
  - The value must be an inline base64 `data:` URL of `image/webp`,
    `image/jpeg` or `image/png`, at most 40,000 characters.
  - No remote URL can be stored, so a photo never becomes a request a reader's
    browser makes elsewhere.
  - SVG is excluded, because SVG is a document that can carry script.
  - The bound keeps the user row small, because every sign-in reads it.
- **Per-person storage allocation already exists, and is reused.**
  - `storagequota.Store` keeps a personal allowance per user: 10 GB for a new
    ordinary user, and no personal ceiling for owners and developers.
  - All managed Blob write paths admit against it.
  - `libraryBackupUsage` reads it, and owners and developers change it on the
    person's page in the Users app (`libraryBackupSetUserLimit`, and the
    existing Storage panel).
  - The Profile reads the caller's own allowance through `libraryBackupUsage`.
    No second quota field was added.
- **Budget.** `myBudget` / `myBudgetSave` belong to the routes work. Removal is
  the argument left out, which is that builtin's own contract.

## Testing

- **Engine, without a database**
  (`component/memql/user_profile_self_scoped_6220_test.go`):
  - each write targets the caller and ignores a supplied id;
  - with no actor, no row is selected;
  - no write declares a target argument, and none can reach role, email or
    sign-in policy;
  - the name fields that land;
  - blank and over-long names are refused;
  - the photo validator admits the three inline types up to the bound and
    refuses remote, protocol-relative, SVG, script, GIF, non-base64 and
    over-bound values.
- **Engine, with a database** (`..._db_test.go`, run against TimescaleDB with
  pgvector):
  - round trips on the caller's own row;
  - another caller's supplied id is ignored;
  - clearing keeps the name;
  - a refused write leaves the row untouched;
  - `currentUser` returns the photo.
- **OS** (`test/profile/`):
  - the photo pipeline's decisions: file refusals, the centred crop, the
    quality ladder, the JPEG fallback, refusal when nothing fits;
  - the shared copy: one read for two readers, and a write reaching both;
  - every section's states;
  - the account menu opening the Profile.
- **QA:** harness views `profile`, `profile-photo` and `profile-quiet`, captured
  in light and dark at 1400x900 and 820x760.
