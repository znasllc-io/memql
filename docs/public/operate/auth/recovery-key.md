---
title: Owner Recovery Key (Operator Guide)
audience: public
status: stable
area: operate
sinceVersion: 0.9.0
owner: znas
---

# Owner Recovery Key

**Audience:** whoever is accountable for a MemQL cluster staying reachable.

The recovery key is the answer to one question: *the owner cannot sign in, and
nobody else can promote them — now what?*

Every other credential MemQL issues assumes a working route in. A magic link
needs a mailbox. An enrolment link needs a browser session someone can hand it
to. A passkey needs the device it was created on. This one assumes none of
them, which is why it exists — and why it is refused on every day it is not
needed.

> **`MEMQL_MASTER_KEY` decrypts and never authenticates.** It is not a way in
> and never was after memql#3519. The operator bearer is `MEMQL_OPERATOR_KEY`;
> the break-glass credential is this key. Three different secrets, three
> different jobs. See [operator-credential.md](operator-credential.md).

---

## What it is

```
mql_rec_<43 base64url characters>
```

32 CSPRNG bytes. Stored as a SHA-256 hex digest and in no other form, so the
plaintext cannot be recovered from the cluster — not by an operator, not by
anyone who takes the database.

It is a `recovery_key` variant on `v1:identity:identity`, bound to exactly one
owner. One active key per owner, at all times.

**It authorizes exactly one action:** register a passkey as the bound owner.
Not a session, not a token, not a role change. That narrowness is what makes it
safe to write down.

---

## The four things that govern it

### 1. It is minted automatically, and its value is never logged

An invariant on the identity node holds the rule *"if a cluster owner exists
and there is no active unredeemed recovery key, mint one"*, evaluated on every
start. The mint **discards the plaintext**: a break-glass credential printed at
boot would land in the pod log, and from there in whatever ships those logs off
the cluster.

So the key exists before anybody has seen it. Claiming is how a human gets the
value.

A fresh cluster has no owner until its first sign-in, so the first boot mints
nothing and the key appears the moment the cluster is claimed.

### 2. It is refused while the owner can still sign in

Redemption checks whether the bound owner still holds an active magic-link or
passkey identity. If they do, the key is **refused and not spent** — the page
says so and tells them to sign in normally.

This is the whole difference between a break-glass credential and a second
password. A key that worked at any time would be a permanent owner-equivalent
bearer sitting in a password manager.

If the check cannot be answered — a database read fails — the redemption is
**refused**, not allowed. An unknown answer is exactly the state an attacker
who can disrupt a read would want.

### 3. It works exactly once, and mints its own replacement

Redeeming stamps `redeemedAt` and deactivates the row **in one write**, so
there is no window in which a spent key is still live. A fresh unclaimed
successor is then minted.

The consequences worth knowing:

- A leaked key is worth **one** passkey registration, not unlimited access.
- The cluster is never left without a break-glass route.
- After a recovery, the key you hold is dead. Claim the successor.

### 4. Every outcome is audited, with the source address

Mint, claim, rotate, redeem, and each of the four rejection states write a
`v1:identity:auditEvent` carrying `SourceIP`. A burst of `already-redeemed` is
a replay attempt; a burst of `invalid` is somebody spraying guesses at a
break-glass endpoint. Both are visible; neither is silent.

Both ceremony halves are per-IP rate limited, sharing the enrolment limiter —
a script that exhausts one does not get a fresh allowance by switching paths.

---

## Runbook

### Claim it (do this at install time)

From inside the identity pod:

```bash
kubectl exec -n memql deploy/identity -- /app/memql recovery-key claim
```

The key is the **only** thing on stdout, so a capture holds the key and
nothing else:

```bash
RECOVERY_KEY="$(kubectl exec -n memql deploy/identity -- /app/memql recovery-key claim)"
```

The local installer runs this as its last step
(`scripts/install/recovery-key.sh`, capability `install.recoveryKey`).

**Installing through the editor.** The VS Code extension runs the same step and
shows the claimed key once, on the install's done screen, with a Copy button
(memql#4079). The value lives only on that screen: the run log and the install
receipt deliberately withhold it, and closing the screen is goodbye. On a
repair or upgrade the screen reports the state instead -- claimed earlier, or
awaiting the first sign-in -- since an already-claimed key cannot be re-shown.
The CLI path above remains for headless installs.

**Store it somewhere the cluster is not.** A password manager, a safe, a sealed
envelope in a drawer. Storing it in the cluster it recovers is the one place
that cannot work.

> **What `claim` actually does.** The plaintext of an already-minted key is not
> recoverable from its hash, so claiming *rotates*: it retires the row nobody
> holds and mints one whose value it prints. Safe precisely because the
> predecessor was never claimed — nobody is stranded. The command says so.

With several owners, name one: `--user-id v1:identity:user:<id>`.

### Redeem it (the day you need it)

Open, on any device with a browser:

```
https://identity.<your-domain>/recover?code=mql_rec_...
```

The page runs the same passkey-registration ceremony `/enroll` runs. Touch ID,
Windows Hello, or whatever unlock that device uses. When it completes you have
a passkey for the owner account and can sign in.

Then **claim the successor** — the key you just used is dead.

If the page says *"You can still sign in normally"*, that is the break-glass
gate. Nothing is broken and nothing has been spent: sign in as usual and add a
passkey from `/me/devices`.

### Rotate it

**From the console**, as a signed-in owner: Users → the owner → **Recovery key**
→ *Rotate the key*. Shown once.

**From the pod**, when nobody can sign in:

```bash
kubectl exec -n memql deploy/identity -- /app/memql recovery-key claim --reclaim
```

Rotate when the value was lost, when it was never claimed, when somebody who
had it leaves, or on any suspicion of a leak. It costs nothing.

---

## Failure states, and what each means

| The page says | What happened | Do this |
|---|---|---|
| This recovery key is not valid | No row matched — mistyped, truncated, or from another cluster | Check you copied the whole key; claim the current one from the pod |
| This recovery key has already been used | It was spent; a replacement was minted at the same moment | If that was you, sign in with the passkey you made. **If it was not, treat it as a compromise**: sign in, rotate, review the account's passkeys |
| This recovery key was replaced | Somebody rotated the key, retiring this one | Use the key from the most recent rotation |
| You can still sign in normally | The break-glass gate. The account has a working route in | Sign in as usual, then add a passkey. Keep this key |

### When `claim` itself fails

**These two surfaces report the same underlying outcomes differently, and the
difference matters if you are scripting against either one.**

The raw CLI, `memql recovery-key claim` (`subcommand_recovery_key.go`), is a
plain Unix command: every refusal -- no owner yet, no active key, several
owners with no `--user-id` to disambiguate, an already-claimed key without
`--reclaim` -- is reported as a plain-text message on stderr and an **exit 1**.
Only success (a key printed to stdout) exits 0.

| Output | What happened | Do this |
|---|---|---|
| A key on stdout, exit 0 | Worked | Store it somewhere the cluster is not |
| `this cluster has no owner yet, so there is no recovery key to claim`, exit 1 | No owner exists yet, so there is nothing to bind a key to. Expected on a cluster nobody has signed into | Nothing yet. Claim it after the first sign-in |
| `the key for <owner> was already claimed at <time>`, exit 1 | The key was already revealed on an earlier run. Only its SHA-256 hash is stored, so the original value cannot be shown again | If you still hold it, keep it. If you do not, rotate deliberately with `--reclaim` |
| `function "activeRecoveryKeys" is server-only and cannot be called by a client`, exit 1 | The store reached the engine on a context that did not say the call was server-initiated | Not an operator problem — it is an engine defect. See below |

`scripts/install/recovery-key.sh` -- the capability script the local installer
runs, and the one the VS Code extension's install/repair flow drives -- wraps
that same CLI and translates its exit-1 refusals into a structured JSON result
on stdout with **exit 0**, because a second run of the install graph against
an already-claimed cluster (repair, upgrade) is expected, not a failure. It
matches the CLI's stderr text for exactly the two non-failure cases above
(`CLAIM_NO_OWNER_MSG` / `CLAIM_ALREADY_CLAIMED_MSG` in the script) and reports:

| `recoveryKeyState` | Meaning | `changed` |
|---|---|---|
| `awaitingOwner` | No owner yet -- mirrors the CLI's "no owner" refusal | `false` |
| `alreadyClaimed` | The key was claimed on an earlier run; not re-revealed | `false` |
| `claimed` | A key was revealed in this run | `true` |

Any other CLI failure (the engine defect above, an unreachable pod, and so on)
still fails the capability script with a non-zero exit and `cap_fail`.

That last one is worth naming precisely, because it presented as a single
failing install step and was not one. The whole break-glass surface is
`@serverOnly` — both reads and all four writes — and the engine refuses a
`@serverOnly` construct unless the call is stamped server-initiated. When
`component/identity/recoverykey` stamped none of it, the credential was not
degraded, it was **inert**: the boot invariant could not take its read, so no
cluster minted a key at all; claim exited 1; rotation failed; and redeem could
not resolve a presented key. The only symptom on a running cluster was one WARN
per boot —

```
identity: recovery-key invariant did not complete; this cluster may have no
break-glass route for its owner
```

— which is why the useful check is not "did the install step pass" but "does
this cluster have a live key". To ask directly:

```bash
kubectl logs -n memql deploy/identity | grep -c 'recovery-key invariant did not complete'
```

Anything other than `0` means this cluster has no break-glass route, whatever
its install record says. Both halves are now pinned by tests — the stamp itself
(`component/identity/recoverykey/store_internal_origin_test.go`), the engine's
refusal and admission
(`component/memql`'s `TestRecoveryKeyConstructsAreServerOnlyAndInternalOriginPasses`),
and the class
(`test/dslconformance`'s `TestEveryGoCallerOfAServerOnlyConstructStampsInternalOrigin`,
which fails the build for any Go caller of a `@serverOnly` construct that does
not stamp).

---

## What this is not

- **Not a password.** It cannot open a session. It registers a passkey and
  nothing else.
- **Not a decryption key.** `MEMQL_MASTER_KEY` decrypts secrets at rest; this
  authenticates. Separate secrets, separate jobs.
- **Not the operator credential.** `MEMQL_OPERATOR_KEY` admits tooling as a
  synthetic cluster owner over gRPC. That is a different bearer with a
  different blast radius.
- **Not a replacement for a second owner.** The cheapest protection against
  losing a cluster is a second person with the owner role. This is what you
  reach for when that was not done.

---

## Where it came from

MemQL used to deliver config two ways: the `memql-secrets` Secret every node
`envFrom`s, and a sealed **genesis envelope** that autoloaded into the process
environment at boot. The envelope's own tooling wrote
`export MEMQL_MASTER_KEY=` into a world-readable `~/.bashrc` — the defect
memql#3519 named while that key still doubled as an authenticator.

Epic memql#3958 deleted the envelope so config has one delivery path. This key
is what replaced its one irreplaceable job: getting an owner back into a
cluster they are locked out of — without a second bearer that also decrypts
everything.

---

## See also

- [operator-credential.md](operator-credential.md) — `MEMQL_OPERATOR_KEY`, and why it is not the master key
- [identity-service.md](identity-service.md) — the service that mints and redeems this
- [sign-in-paths.md](sign-in-paths.md) — the routes this one exists to backstop
- [access-model.md](access-model.md) — roles, and why an owner is hard to replace
