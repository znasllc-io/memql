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

## Prerequisites

1. `/etc/hosts` entries for the test domain (the install wizard's
   `install.hostsEntries` capability does this for a real install; do it by hand
   for the spike):

   ```
   127.0.0.1  identity.memql.localhost cockpit.memql.localhost bff.memql.localhost
   ```

   **On macOS this step turned out to be unnecessary** - verified on 26.5.1 with
   no such entries present: the system resolver already maps `*.localhost` to
   `127.0.0.1`, so `curl https://identity.memql.localhost:8444/` returned 200
   with no `--resolve` and no hosts file edit, and Safari inherits that. Chrome
   maps `*.localhost` internally regardless of the resolver. Keep the entries
   documented for other platforms, but do not burn a `sudo` on them here before
   checking whether you need one.

2. An mkcert wildcard certificate and a trusted local CA. Generate it **outside
   the repository** - `*.pem` is not gitignored here, so a private key written
   into the repo root sits untracked next to your work and can be swept into a
   commit:

   ```
   mkcert -install
   mkdir -p /tmp/memql-spike-3405 && cd /tmp/memql-spike-3405
   mkcert "*.memql.localhost" memql.localhost
   ```

   Because two names are passed, mkcert **suffixes the output with `+1`**: it
   writes `_wildcard.memql.localhost+1.pem` and
   `_wildcard.memql.localhost+1-key.pem` into the current directory. Use those
   exact names below - the unsuffixed spelling does not exist.

3. **A phone on the same Bluetooth range as the desktop** for the hybrid legs.
   Hybrid transport needs BLE for proximity attestation; being on the same
   Wi-Fi is not sufficient and not a substitute.

## Run

Run from the repository root (`go run` needs the module), pointing at the
certificate directory from step 2:

```bash
CERTS=/tmp/memql-spike-3405
go run ./scripts/spikes/webauthn-rpid \
  --rp-id=memql.localhost \
  --addr=127.0.0.1:8443 \
  --cert=$CERTS/_wildcard.memql.localhost+1.pem \
  --key=$CERTS/_wildcard.memql.localhost+1-key.pem
```

Smoke-tested on 2026-08-09 (mkcert v1.4.4, macOS): the server comes up and
`https://identity.memql.localhost:8443/` returns 200 with a CA-trusted
certificate - verified with `curl --resolve` and *without* `-k`, so TLS trust is
genuinely working rather than being bypassed. Everything up to the ceremony is
confirmed good; the phones are the only untested variable left.

Open `https://identity.memql.localhost:8443/` and work down the four buttons.
Then re-run with `--rp-id=identity.memql.localhost` to measure Case A
separately - if Case B fails and Case A passes, the consequence is that a
passkey must be enrolled per subdomain rather than once per cluster, which is a
materially different (and worse, but survivable) design.

The page distinguishes the two failure modes for you:

- `SecurityError` - the **browser** rejected the RP ID. Leg 1 was wrong.
- `NotAllowedError` **after** the platform UI appeared - the **authenticator**
  refused, or you dismissed the prompt. Re-run and complete the prompt before
  concluding anything.

## Control

If a leg fails, re-run the identical sequence with
`--rp-id=memql.localhost` against `https://identity.memql.localhost:8443` (a real
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
| 6 | `memql.localhost` (control) | hybrid -> iOS | | | | |

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
