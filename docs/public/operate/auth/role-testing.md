---
title: Test roles in MemQL OS
audience: public
status: stable
area: operate
sinceVersion: 0.29.0
owner: znas
---

# Test roles in MemQL OS

Open **Users → Role testing** from your own account, then choose a role.
MemQL opens a separate browser tab with a real test identity and the ordinary
permissions for that role. The session lasts 30 minutes. Your original tab
stays signed in.

| Your role | Roles you may test |
| --- | --- |
| Owner | Viewer, Member, Developer, Admin |
| Developer | Viewer, Member, Developer |
| Admin, Member, Viewer | None |

There is no Owner test identity. A restriction on a Developer account can
prevent role testing; a test must not bypass a denial on its operator.

## Identities and data

The first test creates a separate identity for that operator and role.
Later tests reuse that identity, including its files and conversations.
No password, passkey, invitation, or shared login is created. Test identities
cannot sign in normally and do not inherit the operator's groups or grants.

Tests use the current installation and save real changes. A Viewer can upload,
open and edit their own Markdown files and create Ask conversations. Shared
business writes still follow the role's permissions. Higher roles can affect
the data their ordinary permissions allow; this is not a sandbox or a preview.
Use clearly named test items and remove them through the relevant app when
finished. Ending a session does not delete its data.

## Browser editor

Open a Markdown file from Files and choose the browser editor. The test
credential passes directly to the new editor tab, stays in memory and does not
appear in its URL or browser storage. The editor uses a separate temporary
profile, so it cannot reconnect using your original editor credentials.
Native editors and account-security setup are unavailable in a test session.
Test sessions also cannot issue credentials or deploy the cluster.

## Ending and restarting

Use **End test** in the test tab, or end its session from Role testing in the
original tab. Starting another test of the same role ends the preceding run,
including its editor connections. Closing or reloading a tab loses its local
credential; use the original tab to end the remaining server session or let
it expire. A reloaded test tab never falls back to your original login.

All replicas verify the shared session. Revocation rejects new work and closes
idle test streams within approximately five seconds. The deadline bounds
in-flight work, and queued work retains the session boundary when it resumes.
Disabling the operator, revoking their sessions, changing their authority so
the test is no longer allowed, or modifying the persona's role or grants also
invalidates the test.

The identity audit records who started and ended each test, its role and the
session identifier. Credentials are returned once and only their hashes are
stored. Use these records to associate a test identity with its operator.
