# The guided cockpit install -- Design

- **Date:** 2026-09-08
- **Status:** approved by the owner on 2026-09-08 ("guide the user and install the
  cockpit and check, make sure that it's connected, make sure that everything looks good,
  and it's gonna stay connected"; the person must be able to cancel while it waits; the
  wizard shows until everything is done and accounts for manual steps; same look and
  feel as the Deployables compose flow and the first-run wizard; minimal, reusing kit
  pieces). D1-D9 are rulings recorded for the owner to overturn.
- **Owner areas:** `clients/os` (the Fleet app's Machines section, the kit rail already
  promoted for the first-run wizard); the front door (`deploy/k8s`, `cmd/frontdoorhosts`);
  the worker wire and `component/worker` (the cluster ping); `memql-cockpit` (the
  uninstallers, the ping's answer, a release).
- **Amended 2026-09-08, same day, from the owner's first run.** The owner paired the
  machine they were sitting at: the install printed SUCCESS and the machine never appeared,
  the `--inference` step failed with `flag provided but not defined`, and the panel's two
  copy buttons read as furniture. D10 to D15 below are what that run taught; D9's "nothing
  on the engine or the wire" is withdrawn.
- **Depends on:** the Fleet app (epic memql#4729), the promoted rail and interface rules
  11 and 12 (epics memql#4937, memql#5106), the `--inference` install flag (epic
  memql#5103), the registration's hardware and permissions fields (epic memql#5146).

---

## 1. Problem

To use the fleet -- tools on a machine you own, local models, delegated apps -- a person
has to install MemQL Cockpit on that machine, and today the OS hands them a panel: type a
name, tick two boxes, mint a token, copy a 200-character line, and watch one sentence that
says "waiting for the machine to connect" until the machine count grows by one. Nothing
tells them what will happen when they paste the line (a password prompt, on macOS two
permission dialogs, on an inference machine a download of several gigabytes), nothing
says whether the machine that appeared is the one they meant, nothing checks that it will
still be there after a restart, and there is no way to cancel except closing the panel
behind an acknowledgment box. The panel also sits ABOVE the machine list in the same
scroll column, which interface rule 11 names as the tell that a page is not a page.

The owner wants MemQL OS to guide the whole thing: mint, install, connect, check, done --
with the manual steps written down in order, the cluster listening while the person works
in a terminal, a cancel that is honest about the credential it leaves behind, and the same
rail the compose flow and the first-run wizard already read.

## 2. What the tree already has

- **The panel.** `apps/fleet/addMachine/AddMachine.tsx` mints over `CreateWorkerTokenMsg`
  on the connection's own credential, shows the plain `mql_wkr_...` once, composes the
  runbook's ONE-physical-line command (`install.ts`, memql#4875), and reports success when
  `useMachines().count` grows past the value captured at mint. The token is never written
  to storage or a URL, and the panel says so. Both facts are asserted by
  `test/fleet/addMachine.test.tsx` and stay.
- **The rail.** `kit/Rail.tsx` draws a column of marks joined by a line with one closed
  state set: `done`/`complete` lit, `current` pulsing (the one thing moving), `open` a held
  ring (waiting on the person), `waiting` reachable, `ahead`/`pending`/`unknown` dimmed,
  `skipped` dashed with its reason. Collapsed-by-default, one open, `nextOpen` picks the
  first unsettled stop. No Next, no Back, no step number.
- **The action bar.** `kit/ActionBar.tsx` encodes rule 12: the state in words, the acts
  legal from that state, at most three, primary last, an illegal act ABSENT. `children`
  carries a confirmation in place of the acts.
- **The page shape.** The Deployables `ComposePage` replaces the list with a `Panel` whose
  `Head` carries a quiet `<- Deployables` button, a rail whose bodies are the form, and
  the bar on the window's bottom edge (`.os-deploy-pane` / `.os-deploy-scroll`).
- **The registration row** (`dsl/worker/concepts.memql`) carries everything a check
  needs and the cockpit refreshes it: `identityId` (the worker-token identity the machine
  authenticated with), `capabilities`, `buildTag`, `version`, `platformInfo`,
  `permissions` (`accessibility`, `screen_recording`, `x11_display`, `detail`, a snapshot
  at register time, re-registered on reconnect), `hardware.runtimes`, the `model:` labels
  (bound at Register, re-advertised by a reconnect when they change), `lastSeenAt` (every
  15 s), `revokedAt`. The feed is live: `v1:worker:registration` carries a broadcast
  routing rule.
- **The mint reply** (`CreateWorkerTokenResult`) carries `identity_id` beside the plain
  token, and `RevokeWorkerTokenMsg` takes that id. The SDK exposes both
  (`identity/workerToken.ts`).
- **The installers** (`memql-cockpit/scripts/install/install-{mac,linux}.sh`) preflight
  the release asset, prompt for sudo unless `--user-local`, upsert a home in `workers.yaml` (legacy `worker.yaml` mirror), install a
  LaunchAgent / user systemd unit that reconnects on boot, and with `--inference` run
  `memql worker setup --inference --non-interactive`, which exits 3 when a runtime install
  needs a person and prints the command to run by hand. The computer-use build asks macOS
  for Accessibility and Screen Recording on first run; on Linux a Wayland session
  registers HEADLESS only.
- **Three surfaces open the panel by intent** with `{ addMachine: { inference?: true } }`:
  the first-run wizard's fleet door, Settings -> Doors, and Cluster -> Readiness.

## 3. Decisions

### D1 -- The wizard is a page in Machines that replaces the list

Chosen over the panel above the list (rule 11: a list and its detail never share a scroll
column), a new Fleet section (a section is a standing destination and this is an act), a
desk widget (the first-run card is a set of doors; this is one sequence) and a modal
(forbidden by the Accounts covenant). The Machines `Head` keeps its one primary act, "Add
machine"; pressing it replaces the list with a `Panel` headed "Add a machine" carrying a
quiet `<- Machines`, exactly the `ComposePage` shape. The intent contract is unchanged:
`{ addMachine: {} }` opens the page, `{ addMachine: { inference: true } }` opens it with
the local-models choice pre-selected, consumed by id, an object and never a boolean.

### D2 -- The rail is the form, and here the order IS a law

Four stops: **This machine**, **Install**, **Connect**, **Checks**. Unlike the first-run
rail, every stop depends on the one before -- there is no command without a token, no
connection without the command having run, no check without a registration -- so the
unreached stops draw `ahead` and the rail moves by what HAPPENS (a mint, a registration, a
heartbeat), never by a click. There is still no Next: the mint is the bar's forward act,
and everything after it is answered by the cluster.

### D3 -- Two stops are answered by the machine, and the picture says so

While the person is in a terminal, **Install** is `open` (the held ring: waiting on you)
and **Connect** is `current` (the pulse: the cluster is listening) at the same time. That
is the one moment the rail draws two lit marks, and it is the honest picture: you do this;
we are listening for that.

Success is the registration whose `identityId` is the identity the mint returned --
MATCHED, never counted. Counting the population lied in both directions: two people adding
machines at once, or any other machine of yours reconnecting, grew the count with the
wrong machine; and a machine of yours revoked elsewhere shrank it and hid a real arrival.
Ids are compared tolerant of the bare/canonical seam, the way the presence map already
does, because the mint reply and a bare-ified row are two egress seams.

### D4 -- "Stays connected" is evidence, not a claim

The **Checks** stop's first line is the connection itself, and it settles only after the
machine has been heard from twice more since it registered -- the online window, 30 s --
drawing a live "heartbeat 3, 12 s ago" line meanwhile. That the one-liner installed a
LaunchAgent or a user systemd unit that reconnects on boot is stated on the Install stop
as a fact about the command, never as a check the OS cannot make.

### D5 -- Manual steps are the stops' bodies, in order, on that machine

The Install stop lists what happens when the line is pasted, in the order it happens:
open a terminal ON THE MACHINE BEING ADDED; paste; the installer asks for the account
password (it installs under `/usr/local/bin`); with the computer-use build, macOS asks for
Accessibility and Screen Recording; with local models, a download of several gigabytes
in the same terminal. Each check that can need a hand says the exact repair and waits on
the feed rather than on a button: Accessibility or Screen Recording not granted -- grant
them in System Settings -> Privacy & Security, then `memql worker setup` (the worker
re-registers and the check settles); no runtime for local models -- `memql worker setup
--inference` in that terminal (the installer said so when it could not approve the
install); models not yet advertised -- the pull is still running. A Linux machine on
Wayland is `skipped` with the reason, never failed: registering headless there is what
the installer promises.

### D6 -- Cancel is always reachable, and after a mint it asks which of two things

Before a mint, Cancel leaves and nothing was created. After a mint, while the wizard is
waiting, Cancel (and the Head's `<- Machines`) puts one question in the bar: keep the
token, or revoke it. **Keep** leaves with the credential live -- an install already running
in a terminal will still finish, and the machine appears in Machines by itself. **Revoke**
calls `RevokeWorkerTokenMsg` on the minted identity first, because a minted credential
nobody will ever use should not stay live. The person who pressed Cancel by mistake has
"Keep waiting". Once the machine has connected there is nothing to cancel, so the word
disappears: the acts are **Open <machine>** and **Done**.

### D7 -- The flow survives the window's own navigation and nothing else

The draft, the minted token and the phase live in React state held by the Fleet app, so a
person who clicks Routing while a download runs and comes back finds the wizard where they
left it. The token is never written to `localStorage`, `sessionStorage` or a URL (the
existing test stays), so closing the window IS cancel-keep-token, and the Install stop
says so in one sentence.

### D8 -- The name typed first becomes the machine's name on arrival

One `renameWorker` write, once, when the matched registration appears: `displayName` takes
the name the person typed; `name` stays the hostname the cockpit re-stamps on every
reconnect. The old panel asked for a name, called it "yours, for the credential", and then
showed `host.local` with an invitation to rename -- which handed the last step back.

### D9 -- The wizard itself asks nothing new of the engine

`CreateWorkerTokenMsg`, `RevokeWorkerTokenMsg`, `renameWorker`, and the registration's
existing fields are the wizard's whole surface. The client projection
(`apps/fleet/rows.ts`) gains `permissions` (`accessibility`, `screenRecording`,
`x11Display`, `detail`, `present`), read the way `hardware` is: presence decided by
content, so a cockpit that predates the field says "not reported" rather than "denied".
What the engine DOES gain is below, and none of it is for the wizard's benefit alone.

### D10 -- The front door routes the worker stream to the agent (the bug)

`WorkerService.Stream` is served by the AGENT node and by nothing else
(`app/transport_agent.go`); `api.<domain>` routes every gRPC call to the bff's h2c
catch-all. So a cockpit dialling the documented `--cluster https://api.<domain>` was answered
`Unimplemented: unknown service znasllc.memql.worker.v1.WorkerService`, forever, in the
local cluster and in the cloud alike -- the machine never registered, and the OS reported
"waiting". Both overlays now carry one more rule on the api host, ABOVE the catch-all:
the service's own path prefix, `/znasllc.memql.worker.v1.WorkerService/`, to
`svc/agent:50051`. It is the shape of the system, so it lives in the hand-authored local
front door and in `cmd/frontdoorhosts`' gRPC ingress, and render tests in both places
refuse an overlay without it. The prefix is named once, in `component/frontdoor`, and a
test holds it equal to the generated service descriptor's name.

### D11 -- The cluster pings every machine, and the answer is on the row

Heartbeats are the machine's word that it is there. The cluster's own evidence that the
return path works -- and how fast -- is a `Ping` it sends on the stream every minute (the
first one a few seconds after RegisterAck), answered by a `Pong` carrying the ping's
timestamp back. The round trip lands on the registration as `rttMs` and `rttAt` on the
next heartbeat flush, so the OS shows "round trip 12 ms, checked 40 s ago" beside the
heartbeat. A cockpit that predates the message ignores it (its dispatcher has no default
arm) and the row simply carries no figure, which the OS reads as "not measured" rather
than "slow". Both messages ride `WorkerService.Stream` as new oneof arms; the cockpit's
answer is its own PR and ships in the release D15 cuts.

### D12 -- Uninstall is one line too

`scripts/install/uninstall-{mac,linux}.sh` in the cockpit repository stop and remove the
service, remove the binary and its symlink, and remove `workers.yaml` (the token registry) plus the legacy `worker.yaml` mirror;
`--purge` removes the state directory, `policy.yaml` and the logs as well; `--user-local`
removes from `~/.memql/bin` instead of `/usr/local/bin`. The OS composes the one-liner
in three places: the machine page's **Remove this machine** (revoke, then the line), the
wizard's revoke question (a person who already ran the install has a worker retrying with
a dead token), and the runbook. Paste-safe curl one-liners live in the workers runbook Uninstall section.

**Amended 2026-09-27.** The line carries `--cluster=<url>` (the install line's own
`https://api.<domain>`) on both platforms. The uninstallers became multi-home aware after D12 was
written -- one enrollment per cluster in `workers.yaml`, removed with `memql worker unpair` -- and
`uninstall-mac.sh` refused a line that named no enrollment ("choose --cluster=URL or --all-homes",
exit 2). That refusal was copied verbatim from the machine page, which is the failure D12 exists
to prevent, so the OS states the cluster it knows. The machine page also stops asking where
Cockpit was installed: the registration does not report it and the person removing a machine
cannot be expected to know it, so the uninstallers detect the installed shape (account-only,
system-wide, or both) and remove what is there; the wizard's cancel path still passes
`--user-local` because it printed that install line moments before. Given no flags at all the
uninstallers act on a single enrollment, remove the runtime when there is none, and list several
rather than guess.

### D13 -- Local models from the OS: the second command, then the pull

`worker setup --inference` cannot install a runtime unattended (it needs a person for the
install commands), and the one-liner runs without a terminal to ask on, so on a fresh
machine local models are ALWAYS a second command: `memql worker setup --inference`, in the
same terminal, after SUCCESS. The Install stop states it up front when local models were
asked for, rather than leaving it to the Checks stop's repair. Once a runtime is reported
the Checks stop offers **Pull the recommended models** (the existing
`fleetPullRecommended` act, refusals shown in surface) and draws the pulls live from the
`modelPullsForWorker` feed until a model is advertised. The engine's half of that act
has shipped since epic memql#5103; the COCKPIT's half -- the `ModelPullStart` /
`Progress` / `End` arm on its stream loop, over its own `inference.Pull` -- had not,
because its pin predates the wire (its loop says so in prose). It ships in the cockpit
PR beside the Pong, so until that release a pull from the OS is refused with "does not
support model pulls", in surface, and the second command still works.

### D14 -- The round trip: ask it something

A machine serving a model is proved by using it. The Checks stop and the machine page's
Models group carry **Ask it something**: one non-streaming chat, pinned by
`ExplicitProvider` to `fleet:<modelId>` for a model this machine advertises, showing the
answer, the time it took, and the door that served it. It is the whole path the owner
asked to see -- OS to cluster to cockpit to runtime and back -- in one act, and a refusal
is shown in the router's own words.

### D15 -- The copy control is an icon at the end of the field, and the cockpit is released

The token, the install line, the uninstall line and every repair command render through
one kit piece, `CopyField`: a read-only field whose only control is the copy icon at its
end (the `CopyValue` treatment, on a field). And the cockpit's latest release (v0.10.0,
2026-08-25) predates `worker setup --inference`, so the installer on `main` drives a
binary that does not know the flag; the cockpit half of this epic ends in a release cut
from `main` so `releases/latest` carries the flag, the Pong and the uninstallers'
counterpart.

## 4. The change

- `clients/os/src/apps/fleet/addMachine/flow.ts` -- the PURE reading: `Draft`, `Phase`
  (`describe` / `minting` / `waiting` / `connected` / `steady`), `matchRegistration`,
  `checksFor`, `stopsFor`, `barFor`, `sameId`, the heartbeat count; testable without a
  DOM.
- `clients/os/src/apps/fleet/addMachine/useAddMachineFlow.ts` -- the reducer hook the
  Fleet app holds (D7): mint, cancel, revoke, the one rename (D8).
- `clients/os/src/apps/fleet/addMachine/AddMachinePage.tsx` and `stops/` (`Machine`,
  `Install`, `Connect`, `Checks`) -- the page (D1, D2, D3, D5, D6). `AddMachine.tsx` is
  deleted; `install.ts` stays as it is.
- `clients/os/src/apps/fleet/machines/MachinesSection.tsx`, `FleetApp.tsx` -- the page
  replaces the list while a flow is live; the intent still opens it.
- `clients/os/src/apps/fleet/rows.ts` -- `permissions` (D9).
- `clients/os/src/styles/index.css` -- the checks list and the heartbeat line, in the
  rail's own vocabulary; no new colour, no new type.
- Tests: `test/fleet/addMachine.test.tsx` rewritten around the page (everything the panel
  asserted plus D3-D8); `test/fleet/addMachineFlow.test.ts` on fixtures;
  `addMachineIntent.test.tsx` kept green.
- Docs: `workers-runbook.md` 5.5, `memql-os.md`, `first-run.md`, `local-models.md`,
  `ai-settings.md`, `clients/os/README.md`; this record.

## 5. Failure modes

- No live connection: the page says a token can only be minted over one; no act offered.
- The mint refused: the refusal in surface under the This machine stop, in the server's
  words; nothing was created; the bar offers Mint again.
- No domain published: the placeholder `<your cluster URL>` and the caption, as today.
- The registration never comes: the wizard waits indefinitely; Cancel stays reachable;
  after ten minutes the Connect stop adds the three things that usually went wrong (the
  line was pasted somewhere else, the machine cannot reach `api.<domain>`, the installer
  stopped at a prompt) and how to read the worker log.
- The machine registered and went silent: the connection check draws `stopped` with the
  last heartbeat's age and the log path; the wizard stays until Done.
- The rename refused: the machine keeps its hostname; the refusal is a caption on the
  Connect stop; nothing else changes.
- The revoke refused: the wizard stays, the refusal is in the bar's confirmation, and
  Keep is still offered.
- The window closed mid-flow: the token is gone with it (never stored); the machine can
  still connect and appears in the list; the Install stop said so.

## 6. Testing

- `flow.ts` on fixtures: every phase's stop states and bar acts; the two-lit-marks
  moment; `matchRegistration` by identity across bare and canonical forms and NOT by
  count; steadiness after two heartbeats past registration and not before; each check's
  settle, wait, skip and stop conditions (permissions, Wayland, runtime, models, silence).
- The page through the fake connection: mints once; never stores the token; composes the
  one-liner; the registration matched by identity settles Install and Connect; a
  different machine arriving settles nothing; Cancel before a mint leaves; Cancel after a
  mint asks, Keep leaves with no revoke, Revoke calls `revokeWorkerToken` with the minted
  identity; the rename fires once with the typed name; Done returns to the list with the
  machine open; the flow survives a section change within the app.
- Intent: the three existing cases plus the pre-selected local-models choice on the page.
- Screenshots, both modes: the empty form, the token and command with the manual steps,
  waiting with two lit marks, connected with checks settling, a permissions repair, steady
  with Done.

## 7. Delivery

ONE pull request in `memql` closing every task issue and the epic (owner's instruction),
on the epic branch `epic/cockpit-install-wizard`, commits per issue. Then, in
`memql-cockpit`: the uninstallers (no pin dependency), and after the engine PR merges, a
pin bump to that merge commit with the Pong; then a release cut from `main`.

## 8. Out of scope

- `memql worker pair` and identity's pairing-code flow (the OS cannot start it: its auth
  source keeps the bearer behind an interface no component may reach past).
- Windows, a mobile or QR flow, uninstalling or upgrading a machine, and any change to
  the installers themselves.
- Re-running checks on an existing machine's page (a later epic may lift the Checks stop
  into `MachineDetail`; the reading in `flow.ts` is written to be lifted).

## 9. Facts re-verified before starting

The mint reply carries `identity_id` and the SDK returns it; `revokeWorkerToken` exists in
the SDK; the registration row carries `identityId`, `permissions`, `buildTag`, `version`,
`hardware.runtimes`, `model:` labels and `lastSeenAt`, and `v1:worker:registration`
broadcasts; the installers' sudo prompt, `--user-local`, the LaunchAgent / systemd unit,
`setup_inference`'s exit-3 message; the Wayland gate; the kit rail's state set and
`nextOpen`; the ActionBar's `children` confirmation; the three intent producers. All read
on 2026-09-08.

## 10. Pop!_OS walkthrough corrections (2026-09-09)

The first-machine walkthrough retains the four-stop flow and shared brand/kit.
Linux copy names the X11 requirement for mouse and keyboard control and the
second interactive runtime setup command. Installation location is an explicit
choice: the default protected system command or account-only `--user-local`.
Setup uses the selected absolute binary path, including quoted `$HOME` for
account-only installs, and cancellation/removal uses the corresponding flag.
Form content stays within a readable 78ch measure at wide window sizes.

Download progress remains visible while any requested model is pulling. The
latest attempt for each model determines failure, so another model's success
cannot hide it; a repaired model's advertisement clears an old terminal failure.
Feed failures and partial-start errors are visible, and retry remains available.
Back after registration finishes the wizard and returns to Machines.

The chat proof selects a non-embedding model and carries an optional
`AiChatMsg.fleet_registration_id` alongside a concrete `fleet:<modelId>` provider.
The receiving engine resolves the bare ID, checks ownership, and restricts every
attempt to that registration, including a worker connected on another replica.
It refuses an unavailable target rather than letting a sibling answer. The
machine detail's chat proof uses the same owner-only contract. No worker wire
change is needed: the existing forwarded call already carries the selected
registration and authenticated caller.
