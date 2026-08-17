---
title: Owner Recovery Key (Operator Guide)
audience: public
status: stable
area: operate
sinceVersion: 0.9.0
owner: znas
---

# Owner Recovery Key

**Audience:** whoever is accountable for a memQL cluster staying reachable.

The recovery key is the answer to one question: *the owner cannot sign in, and
nobody else can promote them — now what?*

Every other credential memQL issues assumes a working route in. A magic link
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

**From the portal**, as a signed-in owner: People → the owner → **Recovery key**
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

memQL used to deliver config two ways: the `memql-secrets` Secret every node
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
