# Developers see every pipeline run (view only)

Date: 2026-11-24. Requested by José (cluster owner) in a call with Jesús; decisions by Jesús.

## Requirement

José: "developers can have access to the pipelines, and even though they are not like who built them, they should be
able to see them, to have visibility, to see errors and stuff so that they can fix them."

Jesús's decisions:

- Every person at **developer rank or above** sees the runs of **every repository** on the cluster: memQL's own and a
  client's (a client's website pipeline failing is a support call the developer must be able to read).
- **View only.** A developer who does not own the pipeline cannot **cancel** a run ("that makes me a liability") and
  cannot **re-run** one: a re-run spends the owner's compute under the owner's grant and secrets, and can repeat a
  deploy or a notification. Pushing a fix opens a new run by itself, so support needs no re-run. Re-run can become an
  owner-granted permission later if it proves necessary.
- Below developer rank nothing changes. Pipeline settings and channels stay owner-only.

## Why it was invisible

- `v1:pipelines:run` is `@rowAuthz(owner="ownerUserId", clusterOwner, account="accountId")`; memQL's own pipeline is
  tied to no account, so nobody but the owner and the cluster owner passes the tier.
- Every pipelines query filters `row.ownerUserId == actor.userId` (`dsl/pipelines/queries.memql`), so even an
  account member sees only their own runs.
- A run's steps are `v1:work:step` rows and its files Library artifacts, both owner-only; the log tail is read from
  `GET /artifacts/{id}/content`, which serves only the caller's own file (or one shared with them).

## Design

1. **Read floor on the owner tier.** `rankFloor=` already exists on the `clusterOwner` tier
   (`component/memql/rowauthz_read_floor.go`): it ORs "caller's rank is at least the floor" into READS only. It is
   extended to be a legal argument of the owner tier, still read-only (it never widens a write), and
   `v1:pipelines:run` declares `rankFloor="developer"`. Organization-scoped roles never pass a floor (existing rule).
2. **Two developer reads.** New queries `pipelineRunsBrowseAll` and `pipelineRunView(runId)`, both
   `@requiresRank("developer")`, are the owner-filtered queries without the owner filter; the tier decides the read.
   `pipelineRunForOwner` stays strictly owner-only: re-run, cancel, artifact expiry and retention, SARIF and release
   carry all depend on it, which is why a non-owner's re-run and cancel are already refused server-side.
3. **Steps and files through one narrow builtin.** `pipelinesRunDetail(runId)` resolves the run as the caller (owner
   read, else the developer read), then reads that run's work steps and its `source == "pipeline"` artifacts under
   the run owner's borrowed authority, and returns a projection (not the rows), so no work or Library tier is widened.
4. **Logs through the existing download.** `GET /artifacts/{id}/content?pipelineRun=<runId>` serves an artifact to a
   caller at developer rank or above only when: the caller can read that run; the artifact's `source` is
   `pipeline`, its `producedByRunId` is the run's work run, and its owner is the run's owner; it is a live file. It is
   then read and served under the owner, as the shared-file fallback already does. No new route.
5. **Client.** A developer's Runs list and run page use the developer reads; on a run they do not own, Re-run,
   Cancel, "Open in Files" and the follow-up goal are not offered. No new control or screen.

Left out on purpose (YAGNI): developer access to pipeline rows and the repository directory (the run list's search
already matches the repository), expired-file handling for non-owners, an extra Go ownership check in re-run and
cancel (the owner-only read already refuses; a test pins it).

## For José

The Library artifact content route in CLAUDE.md's HTTP exceptions table gains one allowed case: a pipeline log served
to a developer through the `pipelineRun` pointer, under the checks in point 4. No new HTTP endpoint is added.

## Tests

- Real database: a developer reads an untied run owned by someone else and cannot write it; an admin and an
  organization-scoped role do not; the subscription admits the developer.
- Re-run and cancel by a non-owner developer are refused.
- Content route: a developer gets the run's pipeline log; another run's file, a non-pipeline artifact and a caller
  below developer get 404.
- Client: a developer's list uses `pipelineRunsBrowseAll`; a non-owner's run bar has no Re-run or Cancel; a
  non-owner's run page loads steps through `pipelinesRunDetail`.
