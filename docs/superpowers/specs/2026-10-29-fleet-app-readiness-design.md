---
title: Fleet app readiness, failover and sign-in
audience: internal
status: draft
area: operate
sinceVersion: 0.31.10
owner: znas
---

# Fleet app readiness: checked, routed around, and signed in from Fleet

Status: design for three PRs, each usable alone. Owner's brief, 2026-10-29:
availability is checked "in intervals every once in a while", usage limits are
"checked before exposing the app's route" with "a simple ping that doesn't waste
a lot of tokens", and an unauthenticated app gets "a sign-in button", not "retry
availability".

## What went wrong on production (engine 0.31.9)

Ask with routing set to apps failed twice on one machine. Codex was out of
credits and the call did not move on. Claude Code on the same machine had failed
since 2026-10-24 with `Failed to authenticate: OAuth session expired and could
not be refreshed`, while Fleet showed it signed in.

## What was measured (2026-10-29, this Mac)

Claude Code 2.1.304 and codex-cli 0.162.1. Expired logins were fabricated in
scratch config directories; the owner's own login was read, never changed.

| Question | Claude Code | Codex |
|---|---|---|
| Does the status command see an expired login? | No: `claude auth status` printed `loggedIn: true`, exit 0 | No: `codex login status` printed `Logged in using ChatGPT` |
| Signed out, a real turn | `Not logged in · Please run /login`, 0 tokens, under 1 s | `401 Unauthorized`, after about 18 s of retries |
| Expired, a real turn | `Failed to authenticate: OAuth session expired...`, 0 tokens, 45 ms | `workspace routing discovery unauthorized (401)`, after about 21 s |
| Out of usage, a real turn | not reachable here | `You’ve hit your usage limit... try again at Nov 3rd, 2026 11:52 PM`, `usageLimitExceeded`, about 4.6 s, no usage reported |
| Usage read | no zero-token read (`/usage` in print mode is local session totals only) | `account/rateLimits/read`: `ordinaryUsageAllowed`, `rateLimitReachedType`, `primary.resetsAt`, 0 tokens |
| Ping | 2 input + 5 output + 590 cache-creation tokens, about 2.7 s, `apiKeySource: "none"`, `rate_limit_event` with `status`, `resetsAt`, `isUsingOverage`, `unifiedWindows` | not needed |
| Sign-in | `claude auth login` prints a URL to a page that shows a code, and takes the code on stdin through a plain pipe. `BROWSER` is honoured, so no browser opens on the machine | `account/login/start` `{type: chatgptDeviceCode}` returns `verificationUrl` and `userCode` and finishes with `account/login/completed`; `codex login --device-auth` prints the same |

Two more facts that shaped this. With `USER` unset, Claude Code reports `Not
logged in` on macOS even when signed in, so a probe must run in the worker's own
environment. And both CLIs print the file-level answer quickly, so file presence
is cheap but proves nothing about whether a login still works.

## Piece 1: failover and classification (engine only)

A refusal may move to another app only when it is proven pre-work: no served
model, no result, no recorded or dropped action, no tokens, and no prose of the
model's own. `usage.known` leaves the proof, because a refused Codex turn states
no usage. An expired or missing login becomes its own refusal beside the quota
one, stamped on the session row, and blocks that machine's app. A usage limit
rules the app out on every machine for the rest of the call; a login rules out
one machine's app, so the same app on another machine can still answer. A pin
stays its app either way.

Codex's unzoned reset is read as the earliest instant it can mean, so a limit
that lasts days blocks for days. A reset that has already passed takes the
15-minute recheck rather than no block at all.

## Piece 2: readiness probes (Cockpit, row, selection)

`AppInfo` gains `readiness` (`ready`, `signed_out`, `out_of_usage`, `unknown`),
when it was measured, when the limit resets and why it is not ready. **Selection
admits only `ready`** (owner, 2026-10-30: "I shouldn't be able to reach an app that is
not online, that meaning like does not, is not ready, is not connected"). **A Cockpit
that predates the checks sends no word and stays selectable**, as the one exception,
or every current machine would leave routing. A Cockpit that measures always sends a
word, `unknown` before its first check. `signed_out` also clears `signed_in`, so an
older engine gets the right answer too. `Runnable` carries the rule, so labels, doors,
the session gate and a replica without the stream all read one answer from the row. The
machine owns the clock: it re-checks at the reset and reports the change.

**An exhausted app is durable state, not a retry through somebody's request** (owner,
2026-10-30: "we should keep a state and an interval checking for it until it becomes
available, then it can be used again"). On a machine that measures, a refusal holds
until a `ready` measured after it. Neither a reset passing nor a timer ends it. A
Cockpit that predates the checks has only piece 1's clock.

Cadence, on one machine, one check at a time:

- Codex: every 5 minutes, at no token cost.
- Claude Code: hourly when idle and working, about 600 tokens, so at most about
  14,400 tokens a day per machine. Every 15 minutes when not working, which is free. A hard ceiling of 30 pings in a rolling day stops anything
  that would exceed it.
- No check while that app has a session running. A real turn's own report counts
  as a check and defers the next one, so a busy machine never pings.
- An app no cluster allows is never checked.
- A signed-out app is re-checked when its credential files change, because a
  refused ping costs nothing. A working one is not, because every interactive Claude
  Code rewrites `~/.claude.json`.

Claude Code is pinged only when nothing can bill API credit: no `ANTHROPIC_API_KEY`,
`ANTHROPIC_AUTH_TOKEN`, `ANTHROPIC_BASE_URL` or cloud-provider switch in the
worker's environment, and `claude auth status` naming `claude.ai`. A ping whose
init event reports `apiKeySource` other than `none` is stopped. While a plan is
on metered overage, pings wait for the reset.

A check that fails for any other reason (no network, a timeout, a flag an older
CLI does not know) reports `unknown`, which selection does not route to. One such
check keeps the last answer and looks again within a minute, so a timeout on a busy
machine does not take a working app away.

## Piece 3: Sign in from Fleet

Only the machine's owner may start one, and the app must already be allowed for this
cluster. The act writes a `v1:worker:appSignIn` row, whose tier has no cluster-owner bypass,
and forwards the start to the replica holding the stream. A start is forwarded rather than
claimed from an event as a model pull is, because a pasted code must reach that same replica
without being written anywhere. The row carries the URL and device code and never a token.
Claude Code's code is useless without the verifier held by the process on the machine (PKCE),
and it is written to no row and no log.

Fleet shows each app's state, with the reset time when out of usage, and replaces "Retry
availability" with Sign in. A `ready` measured after a refusal ends that refusal's block, which
is what makes the retry unnecessary. No browser opens on the machine, so it works from anywhere.

## Linux service hardening

Measured under the unit the installer wrote (systemd 255, Claude Code 2.1.314,
codex-cli 0.166.0, in a VM with user namespaces allowed, as on the fleet):
`codex app-server` exits with `Read-only file system`, `~/.claude`, `~/.claude.json`
and `~/.codex` are read-only, Node.js dies under `MemoryDenyWriteExecute`, and bubblewrap
cannot create a namespace under `RestrictNamespaces`. The new unit leaves the worker's
own home writable and drops those two directives. Other users' homes and the system tree
stay read-only and privilege gain stays off. Under it the real checks answered correctly
against fabricated expired logins.
