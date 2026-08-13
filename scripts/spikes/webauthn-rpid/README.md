# SPIKE memql#3405 - `.localhost` RP ID + hybrid transport viability

Time-boxed harness for the one open risk in the "signing in without email" epic
(memql#3401): the install wizard's D5 makes `*.memql.localhost` the default
local domain, and the epic promises a user can enrol a passkey on a **local**
cluster by scanning a QR code with their phone. That promise rests on a
`.localhost` RP ID being accepted, which was never verified.

**Delete this directory once the findings are recorded on memql#3405.** It is a
spike harness, not a supported tool. It deliberately shares nothing with
`component/identity` so it can run against a domain the real cluster is not
serving.

---

## What is already settled, and what still needs a human

The question splits into two legs that need completely different instruments.

### Leg 1 - the browser's RP ID validator: SETTLED ANALYTICALLY. It accepts it.

`navigator.credentials.create()` rejects with a `SecurityError` unless
`rp.id` "is a registrable domain suffix of, or is equal to" the caller's
effective domain. That phrase names the HTML algorithm
[*is a registrable domain suffix of or is equal to*][html-rds], and running that
algorithm on our two cases gives:

**Case A - RP ID equals the effective domain** (RP ID `identity.memql.localhost`
on origin `https://identity.memql.localhost`). The algorithm's step 4 is guarded
by "if hostSuffix does not equal originalHost". They are equal, so the whole
step - including every Public Suffix List consultation inside it - is skipped
and the algorithm returns true. **No PSL lookup happens at all.** An exact-match
RP ID cannot fail this check for any hostname, `.localhost` included.

**Case B - RP ID is a parent** (RP ID `memql.localhost` on origin
`https://identity.memql.localhost`, which is the shape the epic actually wants,
so one passkey covers `identity.`, `cockpit.` and `bff.`). Step 4 runs:

| Sub-step | Evaluation | Result |
|---|---|---|
| `hostSuffix` and `originalHost` are both domains | yes | continue |
| `"." + hostSuffix` matches the end of `originalHost` | `.memql.localhost` ends `identity.memql.localhost` | continue |
| `hostSuffix` equals its own public suffix? | public suffix of `memql.localhost` is `localhost`; `memql.localhost` != `localhost` | not a reject |
| `"." + hostSuffix` matches the end of `originalHost`'s public suffix? | `.memql.localhost` vs `localhost` | not a reject |

so the algorithm returns **true**.

That third row is the one that could have killed the design, and it turns on a
fact worth stating precisely: **`localhost` is not on the Public Suffix List.**
Verified against the live list on 2026-08-09:

```
$ curl -sS https://publicsuffix.org/list/public_suffix_list.dat -o /tmp/psl.dat
$ wc -l < /tmp/psl.dat
   16409
$ grep -c localhost /tmp/psl.dat
0
$ grep -cx com /tmp/psl.dat
1
```

Because no rule matches, the PSL algorithm falls through to its prevailing
default rule `*`, which makes `localhost` the public suffix of
`memql.localhost` and `memql.localhost` therefore a *registrable domain* rather
than a public suffix. Had `localhost` been listed, Case B would have failed and
only Case A would have survived.

Note the direction of this result: `.localhost` passes **because** it is
unlisted, i.e. because nobody has asserted anything about it. That is a thinner
guarantee than a spec carve-out, and it is why the analysis was not allowed to
settle the question on its own - a browser is free to special-case `.localhost`
outside the PSL, and reading the algorithm would not see it.

#### Leg 1 is now MEASURED in Chrome, and it agrees (2026-08-10)

Chrome **151.0.7922.108** (headless build, `HeadlessChrome/151.0.0.0`) on
**macOS 26.5.1**, at origin `https://identity.memql.localhost:8444` over an
mkcert-trusted certificate. Each case calls `create()` under an
`AbortController` firing at 2.5s, which reaches the RP ID validator - it runs
*before* any authenticator is consulted - without an authenticator having to
exist. That is what makes this leg automatable at all.

| RP ID | Role | Result | Error |
|---|---|---|---|
| `memql.localhost` | Case B - parent (what the epic wants) | **accepted** | `AbortError` (our own abort) |
| `identity.memql.localhost` | Case A - exact match | **accepted** | `AbortError` (our own abort) |
| `localhost` | positive control | **rejected** | `SecurityError` |
| `example.com` | negative control | **rejected** | `SecurityError` |

Read the controls before the results. The negative control proves the
discriminator fires at all - without it, "no `SecurityError`" would be evidence
of nothing. The positive control is the stronger one: `localhost` is refused as
an RP ID *for this very origin*, which happens only if `localhost` is being
treated as a public suffix. That is exactly the PSL fall-through derived above,
observed from the outside. Chrome is not being loosely permissive about
`.localhost`; it is running the algorithm and landing where the derivation said
it would.

This covers the browser's validator, headless, Chrome only. It says nothing
about what an authenticator does once the ceremony proceeds - that is leg 2.

#### Gecko was attempted and CANNOT be measured this way (2026-08-12)

Firefox **153.0** on Linux, same origin, same abort technique, driven headless
by `run.sh --probe-firefox`. The result is a **non-result**, and the controls
are how we know:

| RP ID | Role | Expected | Outcome | Error |
|---|---|---|---|---|
| `memql.localhost` | parent | accepted | inconclusive | `NotAllowedError` |
| `identity.memql.localhost` | exact match | accepted | inconclusive | `NotAllowedError` |
| `localhost` | positive control | rejected | inconclusive | `NotAllowedError` |
| `example.com` | negative control | **rejected** | inconclusive | `NotAllowedError` |

`example.com` must be refused with a `SecurityError` at this origin. It was
not - it came back with the same `NotAllowedError` as everything else, which
means **the RP ID validator is not what answered**. With no authenticator
available, Gecko rejects the ceremony before the RP ID is ever considered, so
every row in that run is a reading off an instrument that was not connected.
Enabling Gecko's software authenticator
(`security.webauth.webauthn_enable_softtoken`, which `run.sh` sets in the
throwaway profile) did not change it.

Chrome's validator runs early enough that an aborted call still reaches it;
Gecko's ordering does not give the same opening. **So the browser leg remains
Chrome-only, and the interesting divergence - WebKit - is still unmeasured.**

Two things this bought anyway, both of which outlast the spike:

- The page reports `inconclusive` rather than folding an unexpected error name
  into accepted-or-rejected, so the row is honest on its face.
- `run.sh` now **exits 5** when the negative control is not rejected. A run
  that could not measure no longer looks like a run that measured, which is the
  failure this whole harness exists to prevent one level up.

### Leg 2 - the authenticators: NOT ANSWERABLE FROM CODE. Needs a human.

Nothing above tells us what four independent implementations actually *do*:

- the desktop platform authenticator (Touch ID, Windows Hello),
- the iOS passkey provider reached over hybrid transport,
- the Android passkey provider reached over hybrid transport,
- and whether Chrome and Safari agree with each other and with the spec.

The mechanism is sound in principle - in hybrid transport the desktop browser
performs all network I/O and the phone only does cryptography, so **the phone
never resolves the cluster's hostname**; the RP ID reaches it as an opaque
string over the encrypted tunnel and is hashed into the credential. But "sound
in principle" is what this spike exists to stop us shipping on. Measuring it
requires a physical iOS device, a physical Android device, and a person to
scan the QR code and present a face or a finger. That is why memql#3405 carries
the `needs-human` label.

**Run the harness below and fill in the results table.**

---

## Run

**One command.** It issues the certificate (creating the local CA if this
machine has none), starts the TLS server, measures the browser leg by itself,
and prints what is left for you:

```bash
scripts/spikes/webauthn-rpid/run.sh
```

Then open the URL it prints. Everything below happens for you:

- the certificate lands in `/tmp/memql-spike-3405`, **outside the repository** -
  `*.pem` is not gitignored here, and memql#3518 was filed over a private key
  written next to your work;
- **no `mkcert -install`, and no `sudo`.** Issuing a certificate creates the CA
  on its own; the system trust store is only needed if you want the browser to
  stop warning, which is a separate decision;
- the server binds **both loopback stacks**. `identity.memql.localhost` resolves
  to `::1` under systemd-resolved and `127.0.0.1` from an `/etc/hosts` line, and
  a v4-only bind refuses the connection on exactly the machines where the name
  resolved fine;
- leg 1 runs on page load and is recorded before you touch anything;
- every outcome is written to `/tmp/memql-spike-3405/results.md` as it happens,
  so **nothing needs transcribing**.

Other forms:

```bash
scripts/spikes/webauthn-rpid/run.sh --probe-firefox   # headless Gecko (see above: it cannot measure)
scripts/spikes/webauthn-rpid/run.sh --control         # the local.znas.io control
scripts/spikes/webauthn-rpid/run.sh --help
```

### What still needs you

Three buttons and a phone:

1. **platform authenticator** - Touch ID / Windows Hello. No phone.
2. **hybrid -> iOS** - scan the QR with an iPhone.
3. **hybrid -> Android** - scan the QR with an Android phone.

plus button 5 (usernameless assertion) to confirm discoverable login.

The iOS and Android buttons issue an identical ceremony - the browser shows one
QR code and cannot know which phone scans it. They are two buttons so that YOU
declare which device you used, because that is the axis the finding is reported
along and nothing in the API reveals it.

**Hybrid transport needs BLUETOOTH between the phone and the desktop.** Same
Wi-Fi is not a substitute and not a fallback.

**Do Safari first.** Chrome's validator is already measured, so Chrome can no
longer surprise you on the browser leg. WebKit can, and it is also the engine
behind the iOS half of hybrid transport. If Safari's auto-run leg 1 shows
`memql.localhost` REJECTED, that is a browser-leg divergence from Chrome and the
single most consequential outcome available here - stop and record it before
touching a phone.

### Reading a failure

- `SecurityError` - the **browser** rejected the RP ID. Leg 1 was wrong.
- `NotAllowedError` **after** the platform UI appeared - the **authenticator**
  refused, or you dismissed the prompt. Re-run and complete the prompt before
  concluding anything.
- Every control row unexpected - **the run measured nothing.** See the Gecko
  section above; `run.sh` exits 5 rather than let this pass as a result.

## Prerequisites (only if you are not using run.sh)

1. `/etc/hosts` entries for the test domain:

   ```
   127.0.0.1  identity.memql.localhost cockpit.memql.localhost bff.memql.localhost
   ```

   **On macOS this is unnecessary** - verified on 26.5.1: the system resolver
   already maps `*.localhost` to `127.0.0.1`, and Chrome maps it internally
   regardless of the resolver. On Linux, systemd-resolved answers `::1`, which
   the harness handles by binding both stacks.

2. An mkcert certificate, generated **outside the repository**:

   ```
   mkdir -p /tmp/memql-spike-3405 && cd /tmp/memql-spike-3405
   mkcert "*.memql.localhost" memql.localhost
   ```

   Two names means mkcert **suffixes the output with `+1`**:
   `_wildcard.memql.localhost+1.pem` and `_wildcard.memql.localhost+1-key.pem`.

3. Then, from the repository root:

   ```bash
   go run ./scripts/spikes/webauthn-rpid \
     --rp-id=memql.localhost --addr=127.0.0.1:8443 \
     --cert=/tmp/memql-spike-3405/_wildcard.memql.localhost+1.pem \
     --key=/tmp/memql-spike-3405/_wildcard.memql.localhost+1-key.pem \
     --results=/tmp/memql-spike-3405
   ```

## Control

If a leg fails, re-run the identical sequence with
`--rp-id=local.znas.io` against `https://identity.local.znas.io:8443` (a real
domain that already resolves to `127.0.0.1`, and the shape the install wizard's
"Advanced" BYO-domain path offers). That isolates *"`.localhost` specifically is
the problem"* from *"local development origins are the problem"*, which are
different findings with different consequences for the wizard's copy.

## Results - FILL THIS IN

Record browser versions exactly; a bare "Chrome" is not a result.

**Do the Safari rows first.** Chrome's validator is already measured above, so
Chrome can no longer surprise you on the browser leg; WebKit can, and it is also
the engine behind the iOS half of hybrid transport. If Safari yields a
`SecurityError` on row 1 or 5, that is a *browser*-leg divergence from Chrome and
is the single most consequential outcome available here - stop and record it
before touching a phone.

| # | RP ID | Path | Browser + version | OS + version | Outcome | Error name (if any) |
|---|---|---|---|---|---|---|
| 1 | `memql.localhost` | platform authenticator | | | | |
| 2 | `memql.localhost` | hybrid -> iOS | | | | |
| 3 | `memql.localhost` | hybrid -> Android | | | | |
| 4 | `memql.localhost` | usernameless assertion | | | | |
| 5 | `identity.memql.localhost` | platform authenticator | | | | |
| 6 | `local.znas.io` (control) | hybrid -> iOS | | | | |

## The consequence to state

memql#3405's acceptance criteria require the finding to end in one of exactly
two sentences, because memql#3408's `/enroll` page copy and memql#3413's docs
both depend on which:

- **"The wizard MAY promise phone enrolment on the default local domain."**
- **"The wizard MAY NOT, and here is the copy it should use instead."**

Until this table is filled in, the epic ships the **conservative** branch:
memql#3408's `/enroll` page must not promise phone enrolment, and memql#3413's
documentation must not claim it. That is the standing instruction in both
issues, and it is what has been implemented.

[html-rds]: https://html.spec.whatwg.org/multipage/browsers.html#is-a-registrable-domain-suffix-of-or-is-equal-to
