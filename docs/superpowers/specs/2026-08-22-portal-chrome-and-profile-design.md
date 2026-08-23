# Portal chrome pass 2 -- the brand in the header, a handle on the edge, a profile row, and `/me`

**Date:** 2026-08-22
**Status:** approved in the 2026-08-22 backlog brainstorm (sub-project C of nine); mockups reviewed in the visual companion
**Owner:** `clients/portal` (plus one field on `MyAccessResult`)

Sub-project C of the 2026-08-22 backlog brief. A second pass over the shell
that memql#4240 / #4241 produced yesterday: the pieces exist, they are in the
wrong places. The Sessions and Security facets of the profile page consume
what sub-project A (epic #4300) supplies.

---

## 1. What is wrong with the shell today

All in `clients/portal/src/app/AppShell.tsx` and
`clients/portal/src/components/` at `e873a122`:

1. **The header says "Cluster" and the hostname** (`ClusterBadge.tsx:19-34`,
   value = `window.location.host` via `cluster/endpoint.ts:94`). The host is
   the page's own origin; the label tells the reader nothing they do not
   already see in the address bar, and it is the only thing on the left of the
   header.
2. **The brand lives in the rail** (`AppShell.tsx:268-280`: `RailMark` + the
   `MemQL Portal` wordmark) while the header has nothing to anchor it.
3. **The collapse control is inside the brand row** (`:281-296`), `ml-auto`
   when expanded and centred under the mark when collapsed -- a chevron
   floating in the rail with nothing to belong to, which is what the owner
   called terrible.
4. **The footer has no divider** (`SidebarProfile` is the last child of the
   `<nav>`, `:312`; only the parent's `gap-4` separates it) and carries six
   things: connection dot, node id, version, email, role chip, Sign out
   (`SidebarProfile.tsx:68-129`).
5. **There is no profile page.** `routes.tsx:98-132` has no `me` / `profile`
   route; Sign out is a button in the footer.

What is right and stays: the rail's flat `NavGroup` structure and the ruling
against nesting (`:114-116`); `useMyAccess()` as the identity source (no JWT
decoding, `useMyAccess.ts:6-18`); the documented split that identity's
`/me/*` self-service pages do not move into the portal
(`docs/public/operate/portal.md:479-503`); the single `role="status"` for the
connection; the reduced-motion and a11y rules; the repo-root guards.

---

## 2. Decisions

### D1 -- The brand goes to the header; "Cluster / hostname" goes away

Header left = `RailMark` + wordmark, exactly the two components the brand row
renders today. `ClusterBadge` is retired. The header's accessible name becomes
"Portal header". The theme toggle stays on the right. The cluster host
remains derivable from the origin; `portal.md` "What the UI tells you"
(`:311-325`) is updated to say the host is the address bar, the node id and
version are in the rail footer, and who-you-are is the profile row.

### D2 -- The collapse handle is a round tab on the rail's edge, at the top

Chosen over a mid-height grip pill (quiet, but hides that the rail collapses
at all) and a tab above the footer (drifts into the nav as it grows). An 18px
circle centred on the rail's right border just below the header; `«` / `»`
(`ChevronsLeft` / `ChevronsRight`); same spot when collapsed. The top corner
is the one place on the edge nothing will ever grow into, and it is the
convention people already know.

### D3 -- The footer is a divided status line: dot, node id, version

`border-t`, then the connection dot with the node id as its label
(`ServerHello.node_id`, "memqlGRPCServer" today -- the label the owner asked
to keep) and the engine version (`ServerHello.engine_version`, `"dev"` when
empty). Email, role and Sign out leave the footer. Retry-on-error stays as the
dot's action.

### D4 -- The profile element is a nav row, not a card

Chosen over a bordered card (more presence, but a box at the top of a rail
that has no other boxes) and a stacked header (90px, unlike anything else in
the portal). The profile row styles like the other rows -- hover wash, accent
bar when `/me` is active -- with an initials avatar, display name over email,
and the role as a `Badge`. Collapsed: avatar only, details in the tooltip. It
is a `NavLink` to `/me` placed before the first `NavGroup`; it is not a group
and does not nest.

### D5 -- The profile page is tabs per facet, routed

Chosen over one column of bands (standard, but sprawls as facets arrive) and
a summary-card two-column layout (would be the portal's first two-column
page). `PageHeader` with name, email, role chip and **Sign out** as its single
primary action, then `Tabs`: Account (`/me`), Sessions (`/me/sessions`),
Security (`/me/security`) -- each a routed sub-address, the way `/admin/*`
works (`admin/AdminLayout.tsx:67-82`).

### D6 -- Self-service stays on identity; the portal renders and links

Passkey management, personal tokens, data export and deletion stay on
identity's `/me/*` pages and are linked from the Security and Account tabs.
The two controls the portal does render -- the sessions list with revoke and
the passkey-only switch -- call the same reads and mutations identity's own
pages call (sub-project A, §7.2 and §6.2): one implementation, two renderers.

### D7 -- One new wire field: `display_name` on `MyAccessResult`

The profile row needs the person's name on every shell render; the portal
does not decode JWTs; `MyAccessResult` is where `userId`, `primaryEmail` and
`clusterRole` already come from (`memql.proto:2044-2055`,
`my_access_handler.go`). Sub-project A adds `session_id` to the same message;
whichever lands first, the other is a one-field merge.

---

## 3. The shell, after

```
+------------------------------------------------------------------------------+
| <header aria-label="Portal header">  [mark] MemQL Portal          [light|dark|sys] |
+-----------------------+------------------------------------------------------+
| <nav "Portal sections">(«)                                                   |
|  (JS) Jose Sanz       |   <main>                                             |
|       znas@znas.io  owner  <- profile row, NavLink to /me                    |
|  > Console            |                                                      |
|  VIEWS                |                                                      |
|   People  Agents ...  |                                                      |
|  CUSTOM / BUILD / CLUSTER (unchanged)                                        |
|-----------------------|  <- border-t divider                                 |
|  * memqlGRPCServer    |                                                      |
|    v0.19.6            |                                                      |
+-----------------------+------------------------------------------------------+
   («) = the handle: 18px circle on the rail's right border, top: 8px,
         a child of <nav>, absolutely positioned; same spot when collapsed.
```

Collapsed: rail `w-14`; profile row shows the avatar only; nav items icon-only
with `title` + `aria-label` (unchanged); footer shows dot over version, node
id in the tooltip; the handle shows `»`.

---

## 4. Components

| Component | Change |
|---|---|
| `AppShell.tsx` header | `ClusterBadge` removed; brand row's `RailMark` + wordmark moved here; `aria-label="Portal header"` |
| `AppShell.tsx` nav | brand row removed; `RailProfileLink` first; `RailHandle` absolutely positioned on the edge; `RailStatus` last, after a `border-t` |
| `components/ClusterBadge.tsx` | deleted |
| `components/SidebarProfile.tsx` | deleted; replaced by `components/RailStatus.tsx` (dot + node id + version + retry; the single `role="status"` and `data-connection-tone`) and `components/RailProfileLink.tsx` (avatar, name, email, role; `NavLink` to `/me`) |
| `components/RailHandle.tsx` | the tab: `toggleRail`, `aria-expanded`, `aria-label` "Collapse/Expand the navigation rail", borderless, `memql-portal-rail` storage unchanged (`AppShell.tsx:137-156`) |
| `components/Avatar.tsx` (new `src/ui/` primitive) | initials from `displayName` (fallback: first letter of the email), `accent-subtle` ground, sizes sm/md/lg; `aria-hidden` with the name carried by the link |
| `src/me/` (new feature directory) | `MeRoutes.tsx` (splat under `me/*`), `MeLayout.tsx` (PageHeader + Sign out + Tabs), `AccountTab.tsx`, `SessionsTab.tsx`, `SecurityTab.tsx`, `urls.ts`, `useMe.ts`, `useMySessions.ts` |
| `app/routes.tsx` | `<Route path="me/*" element={<MeRoutes />} />` |
| `src/ui/icons.ts` | `User`, `ShieldCheck`, `Monitor`, `KeyRound` (or the nearest lucide names) added to the allowlist |
| `cluster/useMyAccess.ts`, `sdk/ts` types | `displayName` on `AccessSummary` |
| `component/grpc/memql.proto`, `my_access_handler.go` | `display_name` on `MyAccessResult`, filled from the user row the handler already resolves |

### 4.1 The profile page in detail

- **PageHeader**: `displayName`; subtitle `primaryEmail · <role Badge> · member since <user.createdAt>`; primary action **Sign out** (`Button tone="danger"` -- the one destructive verb on the page, confirmed by `ConfirmDialog`), calling the existing `signOut` (`AuthProvider.tsx:359-366`); after sign-out `RequireAuth` renders the sign-in page in place as it does today.
- **Account** (`/me`): `DataText` rows for display name, email, role, member since, last seen; the shared-mailbox note (`user.sharedMailbox`, A) when set; "Edit on identity" -> `identity.<domain>/me/settings`. The user row is read through the self-scoped path the PII narrowing allows (`rowauthz_pii_unbound.go`).
- **Sessions** (`/me/sessions`): view-kit `TABLE_ELEMENT` over `authSessionsForSelf` (A, #4306) -- device label (`clientLabel`), source, signed in (`firstAuthenticatedAt`), last active (`lastActivityAt`), "this device" where `row.id == session_id`; per-row Revoke and "Revoke all others" over the existing gRPC handlers; every revoke confirmed. Until #4306 lands the tab renders an `EmptyState` that says what is coming rather than a broken table.
- **Security** (`/me/security`): passkeys enrolled (count + labels from the self-scoped identities read, memql#3178, filtered to the `passkey` variant) with "Manage passkeys on identity" -> `/me/devices`; the passkey-only switch (`user.signInPolicy`, A #4304) calling the same mutation identity's page calls, disabled with an explanation when no passkey is enrolled; links for personal tokens (`/me/tokens`), data export and deletion. Until #4304 lands the switch is absent and the tab is passkeys + links.

Every destination is a URL (`routes.tsx:29-33`); every page body goes in
`Container` (`portal_page_frame_test.go`); concept ids appear only inside
`src/me/` (`portal_render_path_test.go`); no `--memql-*` token is defined in
the portal (`brand_shared_source_test.go`); icons go through `src/ui/icons.ts`.

---

## 5. Testing

`clients/portal/test/app.test.tsx` and `predefinedViews.test.tsx` are updated
to the new structure, keeping every invariant that still applies and adding:

1. The header (`banner`, "Portal header") contains the wordmark and no version,
   node id, email, role or Sign out.
2. The rail's single `role="status"` is in the footer; `[data-connection-tone]`
   resolves to one element; connecting is `"danger"`; error shows Retry.
3. The handle is the only button matching `/navigation rail/`, is borderless,
   sits inside the `<nav>`, toggles `memql-portal-rail`, and flips its icon and
   `aria-expanded`.
4. The profile row links to `/me`, shows name + email + role expanded, avatar
   only collapsed with the three facts in `title`/`aria-label`, and carries
   the active style on `/me/*`.
5. `/me` renders PageHeader + three tabs; Sign out confirms, then calls the
   identity logout and lands on the sign-in page (`authFlow.test.tsx` keeps
   its document-wide "Sign out" click).
6. Sessions tab: "this device" matches `session_id`; Revoke and Revoke all
   others call the SDK wrappers; the empty state renders without #4306.
7. Security tab: the switch is disabled with zero passkeys; links point at the
   identity origin from config, never a hardcoded host.
8. The repo-root Go guards pass on the changed tree.

---

## 6. Delivery

| PR | Contains | Depends on |
|---|---|---|
| 1 -- the shell and the page | header brand, handle, footer, profile row, `display_name`, `/me` with Account and Security-as-links, Sign out, docs | nothing |
| 2 -- the live facets | Sessions tab; passkey-only switch; shared-mailbox note | A's #4304 and #4306 on `main` |

One `Closes #N` line per issue.

---

## 7. Out of scope

- Moving identity's `/me/*` pages into the portal (the documented split
  stands; D6).
- Avatar images (no field on the user; initials only).
- Editing the display name or email in the portal (identity's settings page).
- Preferences beyond theme (theme stays in the header).
- Multi-cluster switching in the header (rejected at length in
  `cluster/endpoint.ts:17-62`).

---

## 8. References

- Code: `clients/portal/src/app/AppShell.tsx`, `src/components/{ClusterBadge,SidebarProfile,RailMark,ThemeToggle}.tsx`,
  `src/cluster/{ClusterProvider,useMyAccess,endpoint}.ts(x)`,
  `src/auth/AuthProvider.tsx`, `src/admin/AdminLayout.tsx`, `src/ui/README.md`,
  `component/grpc/memql.proto`, `component/grpc/my_access_handler.go`.
- Docs: `docs/public/operate/portal.md` (:311-325, :479-503).
- Related: memql#4240 / #4241 (yesterday's chrome), epic #4300 (sub-project A:
  sessions read, `session_id`, `sharedMailbox`, `signInPolicy`), epic #4288
  (the Artifacts page; the feature-directory recipe this page copies).
